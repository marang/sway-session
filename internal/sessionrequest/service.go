package sessionrequest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
	"github.com/marang/sway-title-animator/internal/statefile"
	"github.com/marang/sway-title-animator/internal/swayipc"
)

type SwayRequester interface {
	Request(swayipc.MessageType, []byte) (swayipc.Message, error)
	Close()
}

type RestoreRunner interface {
	Restore(context.Context, sessionstate.ContextID) error
}

type ExecRestoreRunner struct {
	Executable string
	SwaySocket string
}

func (runner ExecRestoreRunner) Restore(ctx context.Context, id sessionstate.ContextID) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if runner.Executable == "" || runner.Executable[0] != '/' {
		return errors.New("sway-session executable must be absolute")
	}
	if runner.SwaySocket == "" || runner.SwaySocket[0] != '/' {
		return errors.New("sway socket must be absolute")
	}
	command := exec.CommandContext(ctx, runner.Executable, "--json", "restore", "--require-active", "--socket", runner.SwaySocket, string(id))
	command.Env = systemExecutableEnvironment()
	stderr := boundedBuffer{limit: 4096}
	command.Stdout = io.Discard
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("trusted restore failed: %w: %s", err, detail)
		}
		return fmt.Errorf("trusted restore failed: %w", err)
	}
	return nil
}

func systemExecutableEnvironment() []string {
	environment := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		name, _, found := strings.Cut(value, "=")
		if !found || name == "PATH" || strings.HasPrefix(name, "LD_") {
			continue
		}
		environment = append(environment, value)
	}
	return append(environment, "PATH=/usr/local/sbin:/usr/local/bin:/usr/bin")
}

type boundedBuffer struct {
	data  []byte
	limit int
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	written := len(value)
	remaining := buffer.limit - len(buffer.data)
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		buffer.data = append(buffer.data, value...)
	}
	return written, nil
}

func (buffer *boundedBuffer) String() string { return string(buffer.data) }

type Service struct {
	StateRoot    string
	NewContextID func() (sessionstate.ContextID, error)
	NewSway      func() SwayRequester
	Restore      RestoreRunner

	mu sync.Mutex
}

func (service *Service) Handle(ctx context.Context, request Request) (Response, error) {
	if service == nil {
		return Response{}, errors.New("session start service is nil")
	}
	if err := request.Validate(); err != nil {
		return Response{}, err
	}
	if service.StateRoot == "" || service.StateRoot[0] != '/' {
		return Response{}, errors.New("session state root must be absolute")
	}
	if service.NewContextID == nil || service.NewSway == nil || service.Restore == nil {
		return Response{}, errors.New("session start service dependencies are incomplete")
	}
	info, err := os.Stat(request.Cwd)
	if err != nil {
		return Response{}, fmt.Errorf("inspect requested working directory: %w", err)
	}
	if !info.IsDir() {
		return Response{}, errors.New("requested working path is not a directory")
	}

	service.mu.Lock()
	defer service.mu.Unlock()
	client := service.NewSway()
	if client == nil {
		return Response{}, errors.New("sway client is nil")
	}
	defer client.Close()
	registry, err := loadRegistry(service.StateRoot)
	if err != nil {
		return Response{}, err
	}
	tree, err := requestTree(client)
	if err != nil {
		return Response{}, err
	}
	if contextValue, found, err := matchingContext(registry, request); err != nil {
		return Response{}, err
	} else if found {
		if window, mapped, err := observeRequestedContext(tree, registry, contextValue.ID); err != nil {
			return Response{}, err
		} else if mapped {
			return service.focusMappedActiveContext(request, client, window.Workspace)
		}
		if err := requireCompatibleSavedWorkspace(service.StateRoot, contextValue.ID, request.Workspace); err != nil {
			return Response{}, err
		}
	}
	if err := requireWorkspaceEmpty(tree, request.Workspace); err != nil {
		return Response{}, err
	}
	contextValue, registry, created, err := service.ensureContext(request)
	if err != nil {
		return Response{}, err
	}
	if !created {
		if err := requireCompatibleSavedWorkspace(service.StateRoot, contextValue.ID, request.Workspace); err != nil {
			return Response{}, err
		}
	}
	tree, err = requestTree(client)
	if err != nil {
		return rollbackCreatedRegistration(service.StateRoot, request, contextValue, created, err)
	}
	if err := requireWorkspaceEmpty(tree, request.Workspace); err != nil {
		return rollbackCreatedRegistration(service.StateRoot, request, contextValue, created, err)
	}
	if err := focusWorkspace(client, request.Workspace); err != nil {
		return rollbackCreatedRegistration(service.StateRoot, request, contextValue, created, err)
	}
	tree, err = requestTree(client)
	if err != nil {
		return rollbackCreatedRegistration(service.StateRoot, request, contextValue, created, err)
	}
	if err := requireWorkspaceEmpty(tree, request.Workspace); err != nil {
		return rollbackCreatedRegistration(service.StateRoot, request, contextValue, created, err)
	}
	if err := service.Restore.Restore(ctx, contextValue.ID); err != nil {
		return Response{}, err
	}
	tree, err = requestTree(client)
	if err != nil {
		return Response{}, err
	}
	window, found, err := observeRequestedContext(tree, registry, contextValue.ID)
	if err != nil {
		return Response{}, err
	}
	if !found {
		return Response{}, errors.New("restored context did not map before the broker deadline")
	}
	if !workspaceNameHasNumber(window.Workspace, request.Workspace) {
		return Response{}, fmt.Errorf("restored context mapped on workspace %q instead of requested workspace %d", window.Workspace, request.Workspace)
	}
	return acceptedResponse(contextValue, request.Workspace, created), nil
}

func (service *Service) focusMappedActiveContext(request Request, client SwayRequester, observedWorkspace string) (Response, error) {
	if !workspaceNameHasNumber(observedWorkspace, request.Workspace) {
		return Response{}, fmt.Errorf("context is already mapped on workspace %q", observedWorkspace)
	}
	var response Response
	err := sessionstate.InspectRegistryLocked(service.StateRoot, func(registry sessionstate.Registry) error {
		contextValue, found, err := matchingContext(registry, request)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("matching context disappeared before focus")
		}
		tree, err := requestTree(client)
		if err != nil {
			return err
		}
		window, mapped, err := observeRequestedContext(tree, registry, contextValue.ID)
		if err != nil {
			return err
		}
		if !mapped {
			return errors.New("matching context disappeared before focus")
		}
		if !workspaceNameHasNumber(window.Workspace, request.Workspace) {
			return fmt.Errorf("context is already mapped on workspace %q", window.Workspace)
		}
		if err := focusWorkspace(client, request.Workspace); err != nil {
			return err
		}
		response = acceptedResponse(contextValue, request.Workspace, false)
		return nil
	})
	return response, err
}

func requireCompatibleSavedWorkspace(root string, id sessionstate.ContextID, requested int) error {
	snapshot := sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}}
	if err := sessionstate.LayoutFile(root).LoadInto(&snapshot); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("load saved workspace placement: %w", err)
	}
	for _, workspace := range snapshot.Workspaces {
		if !workspaceContainsContext(workspace, id) {
			continue
		}
		if !workspaceNameHasNumber(workspace.Name, requested) {
			return fmt.Errorf("context %q has saved placement on workspace %q, conflicting with requested workspace %d", id, workspace.Name, requested)
		}
		return nil
	}
	return nil
}

func workspaceContainsContext(workspace sessionstate.WorkspaceLayout, id sessionstate.ContextID) bool {
	for _, current := range workspace.PlacementContexts {
		if current == id {
			return true
		}
	}
	if workspace.Tiling != nil && layoutContainsContext(*workspace.Tiling, id) {
		return true
	}
	for _, floating := range workspace.Floating {
		if layoutContainsContext(floating, id) {
			return true
		}
	}
	return false
}

func layoutContainsContext(node sessionstate.LayoutNode, id sessionstate.ContextID) bool {
	if node.ContextID != nil && *node.ContextID == id {
		return true
	}
	for _, child := range node.Children {
		if layoutContainsContext(child, id) {
			return true
		}
	}
	return false
}

func loadRegistry(root string) (sessionstate.Registry, error) {
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{}}
	if err := sessionstate.RegistryFile(root).LoadInto(&registry); err != nil && !errors.Is(err, os.ErrNotExist) {
		return sessionstate.Registry{}, fmt.Errorf("load context registry: %w", err)
	}
	return registry, nil
}

func matchingContext(registry sessionstate.Registry, request Request) (sessionstate.Context, bool, error) {
	for _, current := range registry.Contexts {
		if current.Launcher.Session == request.Session {
			if !requestMatchesContext(request, current) {
				return sessionstate.Context{}, false, fmt.Errorf("herdr session %q is registered with conflicting context metadata", request.Session)
			}
			if current.State != sessionstate.ContextActive {
				return sessionstate.Context{}, false, fmt.Errorf("matching context %q is archived", current.ID)
			}
			return current, true, nil
		}
		if request.Label != "" && current.Label == request.Label {
			return sessionstate.Context{}, false, fmt.Errorf("label %q is already used by context %q", request.Label, current.ID)
		}
	}
	return sessionstate.Context{}, false, nil
}

func (service *Service) ensureContext(request Request) (sessionstate.Context, sessionstate.Registry, bool, error) {
	var selected sessionstate.Context
	created := false
	registry, err := sessionstate.UpdateRegistry(service.StateRoot, func(registry *sessionstate.Registry) error {
		current, found, err := matchingContext(*registry, request)
		if err != nil {
			return err
		}
		if found {
			selected = current
			return nil
		}
		id, err := service.NewContextID()
		if err != nil {
			return fmt.Errorf("generate context ID: %w", err)
		}
		selected = sessionstate.Context{
			ID: id, Label: request.Label, Provider: request.Provider, State: sessionstate.ContextActive,
			Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: request.Session, Cwd: request.Cwd},
		}
		if err := sessionstate.AddContext(registry, selected); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err == nil {
		return selected, registry, created, nil
	}
	var unknown *statefile.CommitOutcomeUnknownError
	if !errors.As(err, &unknown) || selected.ID == "" {
		return sessionstate.Context{}, sessionstate.Registry{}, false, fmt.Errorf("ensure context registration: %w", err)
	}
	visible, loadErr := loadRegistry(service.StateRoot)
	if loadErr != nil {
		return sessionstate.Context{}, sessionstate.Registry{}, false, errors.Join(err, fmt.Errorf("reload context registration: %w", loadErr))
	}
	for _, current := range visible.Contexts {
		if current.ID == selected.ID && requestMatchesContext(request, current) && current.State == sessionstate.ContextActive {
			return current, visible, created, nil
		}
	}
	return sessionstate.Context{}, sessionstate.Registry{}, false, err
}

func rollbackCreatedRegistration(root string, request Request, contextValue sessionstate.Context, created bool, cause error) (Response, error) {
	if !created {
		return Response{}, cause
	}
	_, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
		for _, current := range registry.Contexts {
			if current.ID != contextValue.ID {
				continue
			}
			if current.State != sessionstate.ContextActive || !requestMatchesContext(request, current) {
				return errors.New("created context changed before registration rollback")
			}
			_, removeErr := sessionstate.RemoveContext(registry, string(contextValue.ID))
			return removeErr
		}
		return nil
	})
	if err == nil {
		return Response{}, cause
	}
	var unknown *statefile.CommitOutcomeUnknownError
	if errors.As(err, &unknown) {
		visible, loadErr := loadRegistry(root)
		if loadErr == nil {
			removed := true
			for _, current := range visible.Contexts {
				if current.ID == contextValue.ID {
					removed = false
					break
				}
			}
			if removed {
				return Response{}, cause
			}
		} else {
			err = errors.Join(err, fmt.Errorf("reload registration rollback: %w", loadErr))
		}
	}
	return Response{}, errors.Join(cause, fmt.Errorf("roll back created context registration: %w", err))
}

func requestMatchesContext(request Request, contextValue sessionstate.Context) bool {
	return contextValue.Label == request.Label && contextValue.Provider == request.Provider && contextValue.Launcher.Kind == sessionstate.LauncherHerdr && contextValue.Launcher.Session == request.Session && contextValue.Launcher.Cwd == request.Cwd
}

func acceptedResponse(contextValue sessionstate.Context, workspace int, created bool) Response {
	return Response{Context: &contextValue, Workspace: workspace, Created: created}
}

func requestTree(client SwayRequester) (*swayipc.TreeNode, error) {
	message, err := client.Request(swayipc.GetTree, nil)
	if err != nil {
		return nil, fmt.Errorf("request Sway tree: %w", err)
	}
	if message.Type != swayipc.GetTree {
		return nil, fmt.Errorf("unexpected Sway tree response type %d", message.Type)
	}
	var root swayipc.TreeNode
	if err := json.Unmarshal(message.Payload, &root); err != nil {
		return nil, fmt.Errorf("decode Sway tree: %w", err)
	}
	return &root, nil
}

func observeRequestedContext(root *swayipc.TreeNode, registry sessionstate.Registry, id sessionstate.ContextID) (sessionstate.ManagedWindow, bool, error) {
	windows, issues, err := sessionstate.ObserveManagedWindowsIsolated(root, registry)
	if err != nil {
		return sessionstate.ManagedWindow{}, false, fmt.Errorf("observe managed windows: %w", err)
	}
	for _, issue := range issues {
		if issue.ContextID == id {
			return sessionstate.ManagedWindow{}, false, issue
		}
	}
	window, found := windows[id]
	return window, found, nil
}

func requireWorkspaceEmpty(root *swayipc.TreeNode, number int) error {
	var matches []*swayipc.TreeNode
	var walk func(*swayipc.TreeNode)
	walk = func(node *swayipc.TreeNode) {
		if node == nil {
			return
		}
		if node.Type == "workspace" && workspaceNameHasNumber(node.Name, number) {
			matches = append(matches, node)
		}
		for _, child := range node.Nodes {
			walk(child)
		}
		for _, child := range node.FloatingNodes {
			walk(child)
		}
	}
	walk(root)
	if len(matches) > 1 {
		return fmt.Errorf("workspace number %d is ambiguous", number)
	}
	if len(matches) == 1 && (len(matches[0].Nodes) != 0 || len(matches[0].FloatingNodes) != 0) {
		return fmt.Errorf("workspace %q is not empty", matches[0].Name)
	}
	return nil
}

func workspaceNameHasNumber(name string, number int) bool {
	prefix := name
	if before, _, found := strings.Cut(name, ":"); found {
		prefix = before
	}
	value, err := strconv.Atoi(strings.TrimSpace(prefix))
	return err == nil && value == number
}

func focusWorkspace(client SwayRequester, number int) error {
	message, err := client.Request(swayipc.RunCommand, []byte(fmt.Sprintf("workspace number %d", number)))
	if err != nil {
		return fmt.Errorf("focus requested workspace: %w", err)
	}
	if err := swayipc.CheckRunCommandResponse(message); err != nil {
		return fmt.Errorf("focus requested workspace: %w", err)
	}
	return nil
}
