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
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/statefile"
	"github.com/marang/sway-session/internal/swayipc"
)

const (
	registrationRollbackTimeout = time.Second
	// Bound output-pipe draining; descendant lifetimes remain with their owners.
	restoreCommandWaitDelay = 250 * time.Millisecond
)

type SwayRequester interface {
	RequestContext(context.Context, swayipc.MessageType, []byte) (swayipc.Message, error)
	Close()
}

type RestoreRunner interface {
	Restore(context.Context, sessionstate.ContextID) error
}

type SessionInitializer interface {
	Initialize(context.Context, sessionstate.Context) error
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
	command.WaitDelay = restoreCommandWaitDelay
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
	Initializer  SessionInitializer
	Now          func() time.Time

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
	access, err := sessionstate.AcquireStateAccess(ctx, service.StateRoot, true)
	if err != nil {
		return Response{}, err
	}
	defer access.Close()
	client := service.NewSway()
	if client == nil {
		return Response{}, errors.New("sway client is nil")
	}
	defer client.Close()
	var contextValue sessionstate.Context
	var created bool
	var focusedResponse *Response
	err = sessionstate.WithTerminalLifecycleLockContext(ctx, service.StateRoot, func() error {
		registry, prepareErr := loadRegistryContext(ctx, service.StateRoot)
		if prepareErr != nil {
			return prepareErr
		}
		tree, prepareErr := requestTree(ctx, client)
		if prepareErr != nil {
			return prepareErr
		}
		if current, found, matchErr := matchingContext(registry, request); matchErr != nil {
			return matchErr
		} else if found {
			contextValue = current
			if window, mapped, observeErr := observeRequestedContext(tree, registry, current.ID, request.Workspace); observeErr != nil {
				return observeErr
			} else if mapped {
				response, focusErr := service.focusMappedActiveContext(ctx, request, client, current.ID, window)
				if focusErr != nil {
					return focusErr
				}
				focusedResponse = &response
				return nil
			}
			if prepareErr := requireCompatibleSavedWorkspace(ctx, service.StateRoot, current.ID, request.Workspace); prepareErr != nil {
				return prepareErr
			}
		}
		if prepareErr := requireWorkspaceUnambiguous(tree, request.Workspace); prepareErr != nil {
			return withDiagnosticContext(prepareErr, contextValue.ID)
		}
		contextValue, _, created, prepareErr = service.ensureContext(ctx, request)
		if prepareErr != nil {
			return prepareErr
		}
		if !created {
			if prepareErr := requireCompatibleSavedWorkspace(ctx, service.StateRoot, contextValue.ID, request.Workspace); prepareErr != nil {
				return prepareErr
			}
		}
		rollback := func(cause error) error {
			if !created {
				cause = withDiagnosticContext(cause, contextValue.ID)
			}
			_, rollbackErr := rollbackCreatedRegistration(service.StateRoot, request, contextValue, created, cause)
			return rollbackErr
		}
		tree, prepareErr = requestTree(ctx, client)
		if prepareErr != nil {
			return rollback(prepareErr)
		}
		if prepareErr := requireWorkspaceUnambiguous(tree, request.Workspace); prepareErr != nil {
			return rollback(prepareErr)
		}
		if prepareErr := focusWorkspace(ctx, client, request.Workspace); prepareErr != nil {
			return rollback(prepareErr)
		}
		tree, prepareErr = requestTree(ctx, client)
		if prepareErr != nil {
			return rollback(prepareErr)
		}
		if prepareErr := requireWorkspaceUnambiguous(tree, request.Workspace); prepareErr != nil {
			return rollback(prepareErr)
		}
		return nil
	})
	if err != nil {
		return Response{}, err
	}
	if focusedResponse != nil {
		return service.initializeResponse(ctx, *focusedResponse, nil)
	}
	if err := service.Restore.Restore(ctx, contextValue.ID); err != nil {
		return Response{}, &RequestDiagnostic{Code: DiagnosticRestoreFailed, ContextID: contextValue.ID, Workspace: request.Workspace, Cause: err}
	}
	response, finalizeErr := service.finalizeRestoredContext(ctx, request, client, contextValue.ID, created)
	return service.initializeResponse(ctx, response, finalizeErr)
}

func (service *Service) initializeResponse(ctx context.Context, response Response, prior error) (Response, error) {
	if prior != nil || service.Initializer == nil || response.Context == nil {
		return response, prior
	}
	if err := service.Initializer.Initialize(ctx, *response.Context); err != nil {
		cause := protocolMismatchAfterRegistration(response.Context.ID, err)
		var mismatch *ProtocolMismatchDiagnostic
		if errors.As(cause, &mismatch) {
			return response, fmt.Errorf("initialize requested terminal session: %w", cause)
		}
		return response, &RequestDiagnostic{Code: DiagnosticInitializationFailed, ContextID: response.Context.ID, Workspace: response.Workspace, Cause: cause}
	}
	return response, nil
}

func (service *Service) focusMappedActiveContext(ctx context.Context, request Request, client SwayRequester, expectedID sessionstate.ContextID, observed sessionstate.ManagedWindow) (Response, error) {
	if !workspaceNameHasNumber(observed.Workspace, request.Workspace) {
		return Response{}, &RequestDiagnostic{Code: DiagnosticWorkspaceConflict, ContextID: expectedID, Workspace: request.Workspace,
			Cause: fmt.Errorf("context is already mapped on workspace %q", observed.Workspace)}
	}
	var response Response
	err := sessionstate.InspectRegistryLockedContext(ctx, service.StateRoot, func(registry sessionstate.Registry) error {
		contextValue, found, err := matchingContext(registry, request)
		if err != nil {
			return &RequestDiagnostic{Code: DiagnosticContextChanged, ContextID: expectedID, Workspace: request.Workspace, Cause: err}
		}
		if !found || contextValue.ID != expectedID {
			return &RequestDiagnostic{Code: DiagnosticContextChanged, ContextID: expectedID, Workspace: request.Workspace,
				Cause: errors.New("matching context disappeared before focus")}
		}
		tree, err := requestTree(ctx, client)
		if err != nil {
			return err
		}
		window, mapped, err := observeRequestedContext(tree, registry, contextValue.ID, request.Workspace)
		if err != nil {
			return err
		}
		if !mapped {
			return &RequestDiagnostic{Code: DiagnosticContextChanged, ContextID: expectedID, Workspace: request.Workspace,
				Cause: errors.New("matching context disappeared before focus")}
		}
		if window.ContainerID != observed.ContainerID {
			return &RequestDiagnostic{Code: DiagnosticContextChanged, ContextID: expectedID, Workspace: request.Workspace,
				Cause: errors.New("matching context window changed before focus")}
		}
		if !workspaceNameHasNumber(window.Workspace, request.Workspace) {
			return &RequestDiagnostic{Code: DiagnosticWorkspaceConflict, ContextID: expectedID, Workspace: request.Workspace,
				Cause: fmt.Errorf("context is already mapped on workspace %q", window.Workspace)}
		}
		if err := requireWorkspaceContainsWindow(tree, request.Workspace, window.ContainerID); err != nil {
			return withDiagnosticContext(err, expectedID)
		}
		if err := focusAndVerifyWindow(ctx, client, registry, contextValue.ID, request.Workspace, window); err != nil {
			return err
		}
		response = acceptedResponse(contextValue, request.Workspace, false)
		return nil
	})
	return response, err
}

func (service *Service) finalizeRestoredContext(ctx context.Context, request Request, client SwayRequester, expectedID sessionstate.ContextID, created bool) (Response, error) {
	var response Response
	err := sessionstate.InspectRegistryLockedContext(ctx, service.StateRoot, func(registry sessionstate.Registry) error {
		contextValue, found, err := matchingContext(registry, request)
		if err != nil {
			return &RequestDiagnostic{Code: DiagnosticContextChanged, ContextID: expectedID, Workspace: request.Workspace, Cause: err}
		}
		if !found || contextValue.ID != expectedID {
			return &RequestDiagnostic{Code: DiagnosticContextChanged, ContextID: expectedID, Workspace: request.Workspace,
				Cause: errors.New("matching context disappeared after restore")}
		}
		tree, err := requestTree(ctx, client)
		if err != nil {
			return err
		}
		window, mapped, err := observeRequestedContext(tree, registry, contextValue.ID, request.Workspace)
		if err != nil {
			return err
		}
		if !mapped {
			return &RequestDiagnostic{Code: DiagnosticMappingPending, ContextID: expectedID, Workspace: request.Workspace,
				Cause: errors.New("restored context did not map before the broker deadline")}
		}
		if !workspaceNameHasNumber(window.Workspace, request.Workspace) {
			return &RequestDiagnostic{Code: DiagnosticWorkspaceConflict, ContextID: expectedID, Workspace: request.Workspace,
				Cause: fmt.Errorf("restored context mapped on workspace %q instead of requested workspace %d", window.Workspace, request.Workspace)}
		}
		if err := requireWorkspaceContainsWindow(tree, request.Workspace, window.ContainerID); err != nil {
			return withDiagnosticContext(err, expectedID)
		}
		if err := focusAndVerifyWindow(ctx, client, registry, contextValue.ID, request.Workspace, window); err != nil {
			return err
		}
		response = acceptedResponse(contextValue, request.Workspace, created)
		return nil
	})
	return response, err
}

func requireCompatibleSavedWorkspace(ctx context.Context, root string, id sessionstate.ContextID, requested int) error {
	snapshot := sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}}
	if err := sessionstate.LayoutStoreFor(root).LoadIntoContext(ctx, &snapshot); err != nil {
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
			return &RequestDiagnostic{Code: DiagnosticWorkspaceConflict, ContextID: id, Workspace: requested,
				Cause: fmt.Errorf("context %q has saved placement on workspace %q, conflicting with requested workspace %d", id, workspace.Name, requested)}
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
	return loadRegistryContext(context.Background(), root)
}

func loadRegistryContext(ctx context.Context, root string) (sessionstate.Registry, error) {
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{}}
	err := sessionstate.InspectRegistryLockedContext(ctx, root, func(current sessionstate.Registry) error {
		registry = current
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
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

func (service *Service) ensureContext(ctx context.Context, request Request) (sessionstate.Context, sessionstate.Registry, bool, error) {
	var selected sessionstate.Context
	created := false
	createdAt := time.Now()
	if service.Now != nil {
		createdAt = service.Now()
	}
	registry, err := sessionstate.UpdateRegistryWithTerminalCreationContext(ctx, service.StateRoot, func(registry *sessionstate.Registry) error {
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
			Launcher: sessionstate.Launcher{
				Kind: sessionstate.LauncherHerdr, Session: request.Session, Cwd: request.Cwd,
				Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty},
			},
		}
		if err := sessionstate.AddContext(registry, selected); err != nil {
			return err
		}
		created = true
		return nil
	}, func() (sessionstate.ContextID, time.Time, bool) {
		return selected.ID, createdAt, created
	})
	if err == nil {
		return selected, registry, created, nil
	}
	var unknown *statefile.CommitOutcomeUnknownError
	if !errors.As(err, &unknown) || selected.ID == "" {
		return sessionstate.Context{}, sessionstate.Registry{}, false, fmt.Errorf("ensure context registration: %w", err)
	}
	visible, loadErr := loadRegistryContext(ctx, service.StateRoot)
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
	cleanupContext, cancel := context.WithTimeout(context.Background(), registrationRollbackTimeout)
	defer cancel()
	_, err := sessionstate.UpdateRegistryContext(cleanupContext, root, func(registry *sessionstate.Registry) error {
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
	removed := err == nil
	var unknown *statefile.CommitOutcomeUnknownError
	if !removed && errors.As(err, &unknown) {
		visible, loadErr := loadRegistryContext(cleanupContext, root)
		if loadErr == nil {
			removed = true
			for _, current := range visible.Contexts {
				if current.ID == contextValue.ID {
					removed = false
					break
				}
			}
		} else {
			err = errors.Join(err, fmt.Errorf("reload registration rollback: %w", loadErr))
		}
	}
	if removed {
		return Response{}, cause
	}
	return Response{}, errors.Join(cause, fmt.Errorf("roll back created context registration: %w", err))
}

func requestMatchesContext(request Request, contextValue sessionstate.Context) bool {
	return contextValue.Label == request.Label && contextValue.Provider == request.Provider &&
		!sessionstate.IsTerminalInstanceContext(contextValue) &&
		contextValue.Launcher.Kind == sessionstate.LauncherHerdr && contextValue.Launcher.Session == request.Session &&
		contextValue.Launcher.Cwd == request.Cwd && contextValue.Launcher.Terminal != nil &&
		contextValue.Launcher.Terminal.Adapter == sessionstate.TerminalAdapterAlacritty &&
		contextValue.Launcher.Terminal.Identity == nil
}

func acceptedResponse(contextValue sessionstate.Context, workspace int, created bool) Response {
	return Response{Context: &contextValue, Workspace: workspace, Created: created}
}

func requestTree(ctx context.Context, client SwayRequester) (*swayipc.TreeNode, error) {
	message, err := client.RequestContext(ctx, swayipc.GetTree, nil)
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

func observeRequestedContext(root *swayipc.TreeNode, registry sessionstate.Registry, id sessionstate.ContextID, workspace int) (sessionstate.ManagedWindow, bool, error) {
	windows, issues, err := sessionstate.ObserveManagedWindowsIsolated(root, registry)
	if err != nil {
		return sessionstate.ManagedWindow{}, false, fmt.Errorf("observe managed windows: %w", err)
	}
	for _, issue := range issues {
		if issue.ContextID == id {
			return sessionstate.ManagedWindow{}, false, &RequestDiagnostic{Code: DiagnosticWindowAmbiguous, ContextID: id, Workspace: workspace, Cause: issue}
		}
	}
	window, found := windows[id]
	return window, found, nil
}

// Occupancy is not an identity boundary. Only ambiguous numbered destinations
// prevent a request from choosing a workspace safely.
func requestedWorkspace(root *swayipc.TreeNode, number int) (*swayipc.TreeNode, error) {
	var selected *swayipc.TreeNode
	var walk func(*swayipc.TreeNode) error
	walk = func(node *swayipc.TreeNode) error {
		if node == nil {
			return errors.New("sway tree contains an invalid nil node")
		}
		if node.Type == "workspace" && workspaceNameHasNumber(node.Name, number) {
			if selected != nil {
				return &RequestDiagnostic{Code: DiagnosticWorkspaceAmbiguous, Workspace: number,
					Cause: fmt.Errorf("workspace number %d is ambiguous", number)}
			}
			selected = node
		}
		for _, child := range node.Nodes {
			if err := walk(child); err != nil {
				return err
			}
		}
		for _, child := range node.FloatingNodes {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return selected, nil
}

func requireWorkspaceUnambiguous(root *swayipc.TreeNode, number int) error {
	_, err := requestedWorkspace(root, number)
	return err
}

func requireWorkspaceContainsWindow(root *swayipc.TreeNode, number int, containerID int64) error {
	if containerID <= 0 {
		return errors.New("requested context has an invalid Sway container ID")
	}
	workspace, err := requestedWorkspace(root, number)
	if err != nil {
		return err
	}
	if workspace == nil {
		return fmt.Errorf("workspace number %d disappeared after restore", number)
	}

	matches := 0
	var collectLeaves func(*swayipc.TreeNode) error
	collectLeaves = func(node *swayipc.TreeNode) error {
		if node == nil {
			return errors.New("workspace contains an invalid nil node")
		}
		if len(node.Nodes) == 0 && len(node.FloatingNodes) == 0 {
			if node.ID == containerID {
				matches++
			}
			return nil
		}
		for _, child := range node.Nodes {
			if err := collectLeaves(child); err != nil {
				return err
			}
		}
		for _, child := range node.FloatingNodes {
			if err := collectLeaves(child); err != nil {
				return err
			}
		}
		return nil
	}
	for _, child := range workspace.Nodes {
		if err := collectLeaves(child); err != nil {
			return err
		}
	}
	for _, child := range workspace.FloatingNodes {
		if err := collectLeaves(child); err != nil {
			return err
		}
	}
	if matches != 1 {
		return errors.New("requested context window is missing or ambiguous on the requested workspace")
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

func focusWorkspace(ctx context.Context, client SwayRequester, number int) error {
	message, err := client.RequestContext(ctx, swayipc.RunCommand, []byte(fmt.Sprintf("workspace number %d", number)))
	if err != nil {
		return fmt.Errorf("focus requested workspace: %w", err)
	}
	if err := swayipc.CheckRunCommandResponse(message); err != nil {
		return fmt.Errorf("focus requested workspace: %w", err)
	}
	return nil
}

func focusWindow(ctx context.Context, client SwayRequester, containerID int64) error {
	if containerID <= 0 {
		return errors.New("requested context has an invalid Sway container ID")
	}
	message, err := client.RequestContext(ctx, swayipc.RunCommand, []byte(fmt.Sprintf("[con_id=%d] focus", containerID)))
	if err != nil {
		return fmt.Errorf("focus requested context window: %w", err)
	}
	if err := swayipc.CheckRunCommandResponse(message); err != nil {
		return fmt.Errorf("focus requested context window: %w", err)
	}
	return nil
}

func focusAndVerifyWindow(ctx context.Context, client SwayRequester, registry sessionstate.Registry, id sessionstate.ContextID, workspace int, expected sessionstate.ManagedWindow) error {
	if err := focusWindow(ctx, client, expected.ContainerID); err != nil {
		return err
	}
	// A successful criteria command can match no window. Reobserve after the
	// effect so close, replacement, duplicate identity and user-move races do
	// not turn a stale container ID into a successful broker response.
	tree, err := requestTree(ctx, client)
	if err != nil {
		return err
	}
	window, mapped, err := observeRequestedContext(tree, registry, id, workspace)
	if err != nil {
		return err
	}
	if !mapped || window.ContainerID != expected.ContainerID {
		return &RequestDiagnostic{Code: DiagnosticContextChanged, ContextID: id, Workspace: workspace,
			Cause: errors.New("requested context window changed during focus")}
	}
	if !workspaceNameHasNumber(window.Workspace, workspace) {
		return &RequestDiagnostic{Code: DiagnosticWorkspaceConflict, ContextID: id, Workspace: workspace,
			Cause: errors.New("requested context window left the requested workspace during focus")}
	}
	if err := requireWorkspaceContainsWindow(tree, workspace, window.ContainerID); err != nil {
		return withDiagnosticContext(err, id)
	}
	focused := false
	var walk func(*swayipc.TreeNode)
	walk = func(node *swayipc.TreeNode) {
		if node.ID == window.ContainerID {
			focused = node.Focused
		}
		for _, child := range node.Nodes {
			walk(child)
		}
		for _, child := range node.FloatingNodes {
			walk(child)
		}
	}
	walk(tree)
	if !focused {
		return &RequestDiagnostic{Code: DiagnosticContextChanged, ContextID: id, Workspace: workspace,
			Cause: errors.New("requested context window did not retain focus")}
	}
	return nil
}

// Workspace resolution can fail before any context exists. At a caller that
// retains a registration, include its known UUID without changing the reason
// or losing the internal cause. Do not select one branch of a joined failure.
func withDiagnosticContext(err error, id sessionstate.ContextID) error {
	diagnostic, ok := err.(*RequestDiagnostic)
	if !ok || diagnostic.ContextID != "" || id.Validate() != nil {
		return err
	}
	copy := *diagnostic
	copy.ContextID = id
	return &copy
}
