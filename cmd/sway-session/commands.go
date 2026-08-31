package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/marang/sway-title-animator/internal/diagnostic"
	sessionstate "github.com/marang/sway-title-animator/internal/session"
	"github.com/marang/sway-title-animator/internal/sessionrequest"
	"github.com/marang/sway-title-animator/internal/statefile"
	"github.com/marang/sway-title-animator/internal/swayipc"
)

func executeRequestStart(ctx context.Context, arguments []string, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("request-start")
	sessionName := set.String("session", "", "Herdr session name")
	cwd := set.String("cwd", "", "project directory")
	label := set.String("label", "", "presentation label")
	provider := set.String("provider", "", "provider metadata")
	workspace := set.Int("workspace", 0, "numbered Sway workspace")
	if err := set.Parse(arguments); err != nil || set.NArg() != 0 || *sessionName == "" || *workspace == 0 {
		return commandResult{}, usageFailure("request-start", "request-start requires --session, --workspace, and no positional arguments")
	}
	directory := *cwd
	var err error
	if directory == "" {
		directory, err = deps.workingDir()
	} else {
		directory, err = filepath.Abs(directory)
	}
	if err != nil {
		return commandResult{}, failure("project_directory", "resolve project directory", err.Error())
	}
	directory = filepath.Clean(directory)
	request := sessionrequest.Request{
		Version: sessionrequest.ProtocolVersion, Session: *sessionName, Cwd: directory,
		Label: *label, Provider: *provider, Workspace: *workspace,
	}
	if err := request.Validate(); err != nil {
		return commandResult{}, failure("invalid_request", "invalid session start request", err.Error())
	}
	if deps.requestStart == nil {
		return commandResult{}, failure("session_request", "request session start", "session request dependency is unavailable")
	}
	response, err := deps.requestStart(ctx, request)
	if err != nil {
		return commandResult{}, failure("session_request", "request session start", err.Error())
	}
	return commandResult{Command: "request-start", Contexts: []sessionstate.Context{*response.Context}, Workspace: response.Workspace, Created: response.Created}, nil
}

func executeRegister(arguments []string, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("register")
	sessionName := set.String("session", "", "Herdr session name")
	cwd := set.String("cwd", "", "project directory")
	label := set.String("label", "", "presentation label")
	provider := set.String("provider", "", "provider metadata")
	idValue := set.String("id", "", "context UUID")
	if err := set.Parse(arguments); err != nil || set.NArg() != 0 || *sessionName == "" {
		return commandResult{}, usageFailure("register", "register requires --session and no positional arguments")
	}

	id, err := registrationID(*idValue, deps)
	if err != nil {
		return commandResult{}, failure("invalid_context", "invalid context ID", err.Error())
	}
	directory := *cwd
	if directory == "" {
		directory, err = deps.workingDir()
	} else {
		directory, err = filepath.Abs(directory)
	}
	if err != nil {
		return commandResult{}, failure("project_directory", "resolve project directory", err.Error())
	}
	directory = filepath.Clean(directory)
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("path is not a directory")
		}
		return commandResult{}, failure("project_directory", fmt.Sprintf("invalid project directory %s", directory), err.Error())
	}
	created := sessionstate.Context{
		ID: id, Label: *label, Provider: *provider, State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: *sessionName, Cwd: directory},
	}
	if err := created.Validate(); err != nil {
		return commandResult{}, failure("invalid_context", "invalid context registration", err.Error())
	}
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	_, err = sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
		return sessionstate.AddContext(registry, created)
	})
	if err != nil {
		if committedContext(root, created.ID, func(context sessionstate.Context) bool { return contextsEqual(context, created) }, err) {
			return commandResult{Command: "register", Contexts: []sessionstate.Context{created}}, nil
		}
		return commandResult{}, classifyStateError("register context", err)
	}
	return commandResult{Command: "register", Contexts: []sessionstate.Context{created}}, nil
}

func registrationID(value string, deps dependencies) (sessionstate.ContextID, error) {
	if value != "" {
		return sessionstate.ParseContextID(value)
	}
	return deps.newContextID()
}

func executeList(arguments []string, deps dependencies) (commandResult, *commandFailure) {
	if len(arguments) != 0 {
		return commandResult{}, usageFailure("list", "list accepts no arguments")
	}
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{}}
	if err := sessionstate.RegistryFile(root).LoadInto(&registry); err != nil && !errors.Is(err, os.ErrNotExist) {
		return commandResult{}, classifyStateError("load context registry", err)
	}
	sort.Slice(registry.Contexts, func(left int, right int) bool {
		return registry.Contexts[left].ID < registry.Contexts[right].ID
	})
	return commandResult{Command: "list", Contexts: registry.Contexts}, nil
}

func executeStateChange(name string, arguments []string, state sessionstate.ContextState, deps dependencies) (commandResult, *commandFailure) {
	if len(arguments) != 1 {
		return commandResult{}, usageFailure(name, fmt.Sprintf("%s requires exactly one context", name))
	}
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	var changed sessionstate.Context
	_, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
		var mutationErr error
		changed, mutationErr = sessionstate.SetContextState(registry, arguments[0], state)
		return mutationErr
	})
	if err != nil {
		if changed.ID != "" && committedContext(root, changed.ID, func(context sessionstate.Context) bool { return context.State == state }, err) {
			return commandResult{Command: name, Contexts: []sessionstate.Context{changed}}, nil
		}
		return commandResult{}, classifyStateError(name+" context", err)
	}
	return commandResult{Command: name, Contexts: []sessionstate.Context{changed}}, nil
}

func executePurge(ctx context.Context, arguments []string, stdin io.Reader, stderr io.Writer, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("purge")
	yes := set.Bool("yes", false, "confirm non-interactively")
	if err := set.Parse(arguments); err != nil || set.NArg() != 1 {
		return commandResult{}, usageFailure("purge", "purge requires exactly one context")
	}
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	registry := sessionstate.Registry{}
	if err := sessionstate.RegistryFile(root).LoadInto(&registry); err != nil {
		return commandResult{}, classifyStateError("load context registry", err)
	}
	index, err := sessionstate.ResolveContext(registry, set.Arg(0))
	if err != nil {
		return commandResult{}, classifyStateError("select context to purge", err)
	}
	target := registry.Contexts[index]
	if !*yes {
		if !deps.stdinTerminal() {
			return commandResult{}, failure("confirmation_required", "purge requires an interactive terminal or --yes", "Re-run with --yes only after verifying the selected context.")
		}
		_, _ = fmt.Fprintf(stderr, "Type the full context ID %s to permanently delete its Herdr state: ", target.ID)
		reader := bufio.NewReader(io.LimitReader(stdin, 1025))
		confirmation, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return commandResult{}, failure("confirmation", "read purge confirmation", readErr.Error())
		}
		if strings.TrimSpace(confirmation) != string(target.ID) {
			return commandResult{}, failure("confirmation", "purge confirmation did not match the full context ID", "No state was deleted.")
		}
	}
	paths, err := deps.herdrPaths()
	if err != nil {
		return commandResult{}, failure("herdr_path", "resolve Herdr paths", err.Error())
	}
	selector := string(target.ID)
	externalReconciled := false
	purgeCode := ""
	mutate := func(registry *sessionstate.Registry) error {
		purgeCode = ""
		index, err := sessionstate.ResolveContext(*registry, selector)
		if err != nil {
			return err
		}
		current := registry.Contexts[index]
		if current.Launcher != target.Launcher {
			return errors.New("context launcher changed while purge confirmation was pending")
		}
		rootExists, err := sessionstate.HerdrStateRootExists(paths.Root)
		if err != nil {
			purgeCode = "herdr_permissions"
			return fmt.Errorf("refuse purge from an unsafe Herdr state root: %w", err)
		}
		if rootExists {
			sessionExists, err := sessionstate.HerdrNamedSessionExists(paths.Root, current.Launcher.Session)
			if err != nil {
				purgeCode = "herdr_state"
				return err
			}
			if sessionExists {
				herdrExecutable, err := deps.resolveProgram("herdr")
				if err != nil {
					purgeCode = "missing_executable"
					return fmt.Errorf("find Herdr executable for purge: %w", err)
				}
				manager := sessionstate.HerdrManager{
					Executable: herdrExecutable, Root: paths.Root, Runner: deps.herdrRunner,
				}
				if err := manager.DeleteSession(ctx, current.Launcher.Session); err != nil {
					purgeCode = "herdr"
					return err
				}
			}
		}
		externalReconciled = true
		_, err = sessionstate.RemoveContext(registry, selector)
		if err != nil {
			return err
		}
		return nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		externalReconciled = false
		_, err = sessionstate.UpdateRegistry(root, mutate)
		if err == nil {
			break
		}
		if registryMissingContext(root, target.ID, err) {
			err = nil
			break
		}
		if !externalReconciled {
			break
		}
	}
	if err != nil {
		if purgeCode != "" {
			return commandResult{}, failure(purgeCode, "purge context", err.Error())
		}
		return commandResult{}, classifyStateError("purge context", err)
	}
	return commandResult{Command: "purge", Contexts: []sessionstate.Context{target}}, nil
}

func executeRestore(ctx context.Context, arguments []string, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("restore")
	socketFlag := set.String("socket", "", "Sway IPC socket")
	requireActive := set.Bool("require-active", false, "reject an explicitly selected archived context")
	if err := set.Parse(arguments); err != nil || set.NArg() > 1 {
		return commandResult{}, usageFailure("restore", "restore accepts at most one context")
	}
	socket := *socketFlag
	if socket == "" {
		socket = os.Getenv("SWAYSOCK")
	}
	if socket == "" || !filepath.IsAbs(socket) {
		return commandResult{}, failure("sway_socket", "a valid absolute Sway IPC socket is required", "Run inside Sway or pass --socket PATH.")
	}
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	selector := ""
	if set.NArg() == 1 {
		selector = set.Arg(0)
	}
	result := commandResult{Command: "restore", Contexts: []sessionstate.Context{}}
	var operationDiagnostics []diagnostic.Diagnostic
	err := sessionstate.InspectRegistryLocked(root, func(registry sessionstate.Registry) error {
		targets, err := restoreTargets(registry, selector, *requireActive)
		if err != nil {
			return err
		}
		if len(targets) == 0 {
			result.Message = "No active contexts to restore."
			return nil
		}
		client := deps.newSwayClient(socket)
		if client == nil {
			return errors.New("sway client is nil")
		}
		defer client.Close()
		tree, err := requestTree(client)
		if err != nil {
			return err
		}
		observed, observationIssues, err := sessionstate.ObserveManagedWindowsIsolated(tree, registry)
		if err != nil {
			return fmt.Errorf("observe managed windows: %w", err)
		}
		issueByID := make(map[sessionstate.ContextID]error, len(observationIssues))
		for _, issue := range observationIssues {
			issueByID[issue.ContextID] = issue.Cause
		}
		waiting := make(map[sessionstate.ContextID]sessionstate.Context)
		for _, target := range targets {
			if issue := issueByID[target.ID]; issue != nil {
				operationDiagnostics = append(operationDiagnostics, diagnosticForContext("duplicate_window", target, issue, "Resolve this context's duplicate or structurally invalid managed window before retrying."))
				continue
			}
			if _, exists := observed[target.ID]; exists {
				result.Contexts = append(result.Contexts, target)
				continue
			}
			waiting[target.ID] = target
		}
		if len(waiting) == 0 {
			return nil
		}
		paths, err := deps.herdrPaths()
		if err != nil {
			appendAllRestoreFailures(&operationDiagnostics, waiting, "herdr_path", err, "")
			return nil
		}
		if err := deps.validateHistory(paths); err != nil {
			appendAllRestoreFailures(&operationDiagnostics, waiting, "pane_history", err, "Set [experimental] pane_history = true and keep Herdr state owner-only.")
			return nil
		}
		alacritty, err := deps.resolveProgram("alacritty")
		if err != nil {
			appendAllRestoreFailures(&operationDiagnostics, waiting, "missing_executable", err, "Install Alacritty and ensure it is on PATH.")
			return nil
		}
		herdr, err := deps.resolveProgram("herdr")
		if err != nil {
			appendAllRestoreFailures(&operationDiagnostics, waiting, "missing_executable", err, "Install Herdr and ensure it is on PATH.")
			return nil
		}
		launcher := sessionstate.AlacrittyHerdrLauncher{Alacritty: alacritty, Herdr: herdr, Starter: deps.processStarter}
		for _, id := range sortedWaitingIDs(waiting) {
			target := waiting[id]
			pending, err := deps.findPending("/proc", target, alacritty, herdr)
			if err != nil {
				operationDiagnostics = append(operationDiagnostics, diagnosticForContext("process_observation", target, err, ""))
				delete(waiting, id)
				continue
			}
			if len(pending) > 1 {
				operationDiagnostics = append(operationDiagnostics, diagnosticForContext("duplicate_launch", target, fmt.Errorf("multiple pending Alacritty processes %v", pending), "Stop the duplicate processes before retrying."))
				delete(waiting, id)
				continue
			}
			if len(pending) == 1 {
				continue
			}
			if err := launcher.Launch(target); err != nil {
				operationDiagnostics = append(operationDiagnostics, diagnosticForContext("launch", target, err, ""))
				delete(waiting, id)
			}
		}
		if len(waiting) == 0 {
			return nil
		}
		deadline := deps.now().Add(deps.settleTimeout)
		for {
			tree, err = requestTree(client)
			if err != nil {
				appendAllRestoreFailures(&operationDiagnostics, waiting, "sway_tree", err, "The launched process remains detectable; retry after Sway IPC recovers.")
				return nil
			}
			observed, observationIssues, err = sessionstate.ObserveManagedWindowsIsolated(tree, registry)
			if err != nil {
				appendAllRestoreFailures(&operationDiagnostics, waiting, "sway_tree", err, "Resolve malformed managed identities before retrying.")
				return nil
			}
			for _, issue := range observationIssues {
				target, exists := waiting[issue.ContextID]
				if !exists {
					continue
				}
				operationDiagnostics = append(operationDiagnostics, diagnosticForContext("duplicate_window", target, issue.Cause, "Resolve this context's duplicate or structurally invalid managed window before retrying."))
				delete(waiting, issue.ContextID)
			}
			for id, target := range waiting {
				if _, exists := observed[id]; exists {
					result.Contexts = append(result.Contexts, target)
					delete(waiting, id)
				}
			}
			if len(waiting) == 0 {
				return nil
			}
			if !deps.now().Before(deadline) {
				appendAllRestoreFailures(&operationDiagnostics, waiting, "mapping_timeout", errors.New("window did not appear before the restore deadline"), "A matching pending process prevents duplicate launches; retry after it maps or exits.")
				return nil
			}
			deps.sleep(100 * time.Millisecond)
		}
	})
	if err != nil {
		return commandResult{}, classifyStateError("restore contexts", err)
	}
	sort.Slice(result.Contexts, func(left int, right int) bool { return result.Contexts[left].ID < result.Contexts[right].ID })
	if len(operationDiagnostics) != 0 {
		return result, failures(operationDiagnostics)
	}
	return result, nil
}

func restoreTargets(registry sessionstate.Registry, selector string, requireActive bool) ([]sessionstate.Context, error) {
	if selector != "" {
		index, err := sessionstate.ResolveContext(registry, selector)
		if err != nil {
			return nil, err
		}
		selected := registry.Contexts[index]
		if requireActive && selected.State != sessionstate.ContextActive {
			return nil, fmt.Errorf("context %q is archived", selected.ID)
		}
		if selected.Launcher.Kind != sessionstate.LauncherHerdr {
			return nil, fmt.Errorf("desktop application restore is not available until LAB-98; registration and policy state were preserved for context %q", selected.ID)
		}
		return []sessionstate.Context{selected}, nil
	}
	targets := make([]sessionstate.Context, 0, len(registry.Contexts))
	for _, context := range registry.Contexts {
		if context.State == sessionstate.ContextActive && context.Launcher.Kind == sessionstate.LauncherHerdr {
			targets = append(targets, context)
		}
	}
	return targets, nil
}

func requestTree(client swayRequester) (*swayipc.TreeNode, error) {
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

func appendAllRestoreFailures(items *[]diagnostic.Diagnostic, waiting map[sessionstate.ContextID]sessionstate.Context, code string, err error, hint string) {
	for _, id := range sortedWaitingIDs(waiting) {
		*items = append(*items, diagnosticForContext(code, waiting[id], err, hint))
	}
}

func sortedWaitingIDs(waiting map[sessionstate.ContextID]sessionstate.Context) []sessionstate.ContextID {
	ids := make([]sessionstate.ContextID, 0, len(waiting))
	for id := range waiting {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left int, right int) bool { return ids[left] < ids[right] })
	return ids
}

func committedContext(root string, id sessionstate.ContextID, predicate func(sessionstate.Context) bool, updateErr error) bool {
	var unknown *statefile.CommitOutcomeUnknownError
	if !errors.As(updateErr, &unknown) {
		return false
	}
	registry := sessionstate.Registry{}
	if err := sessionstate.RegistryFile(root).LoadInto(&registry); err != nil {
		return false
	}
	for _, context := range registry.Contexts {
		if context.ID == id {
			return predicate(context)
		}
	}
	return false
}

func registryMissingContext(root string, id sessionstate.ContextID, updateErr error) bool {
	var unknown *statefile.CommitOutcomeUnknownError
	if !errors.As(updateErr, &unknown) {
		return false
	}
	registry := sessionstate.Registry{}
	if err := sessionstate.RegistryFile(root).LoadInto(&registry); err != nil {
		return false
	}
	for _, context := range registry.Contexts {
		if context.ID == id {
			return false
		}
	}
	return true
}

func contextsEqual(left sessionstate.Context, right sessionstate.Context) bool {
	return left == right
}
