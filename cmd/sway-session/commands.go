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
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/marang/sway-session/internal/diagnostic"
	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/sessionrequest"
	"github.com/marang/sway-session/internal/statefile"
	"github.com/marang/sway-session/internal/swayipc"
)

// maxConcurrentTerminalRestores bounds only one automatic restore wave. The
// durable registry remains unbounded, and later waves re-read registry and
// compositor state after releasing the lifecycle lock.
const maxConcurrentTerminalRestores = 2

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

func executeRegister(ctx context.Context, arguments []string, deps dependencies) (commandResult, *commandFailure) {
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
		Launcher: sessionstate.Launcher{
			Kind: sessionstate.LauncherHerdr, Session: *sessionName, Cwd: directory,
			Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty},
		},
	}
	if err := created.Validate(); err != nil {
		return commandResult{}, failure("invalid_context", "invalid context registration", err.Error())
	}
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	err = sessionstate.WithTerminalLifecycleLockContext(ctx, root, func() error {
		createdAt := deps.now()
		_, registerErr := sessionstate.UpdateRegistryWithTerminalCreationContext(ctx, root, func(registry *sessionstate.Registry) error {
			return sessionstate.AddContext(registry, created)
		}, func() (sessionstate.ContextID, time.Time, bool) {
			return created.ID, createdAt, true
		})
		if registerErr != nil && !committedContextContext(ctx, root, created.ID, func(context sessionstate.Context) bool {
			return contextsEqual(context, created)
		}, registerErr) {
			return registerErr
		}
		return nil
	})
	if err != nil {
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

func executeList(ctx context.Context, arguments []string, deps dependencies) (commandResult, *commandFailure) {
	if len(arguments) != 0 {
		return commandResult{}, usageFailure("list", "list accepts no arguments")
	}
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{}}
	if err := sessionstate.RegistryStoreFor(root).LoadIntoContext(ctx, &registry); err != nil && !errors.Is(err, os.ErrNotExist) {
		return commandResult{}, classifyStateError("load context registry", err)
	}
	sort.Slice(registry.Contexts, func(left int, right int) bool {
		return registry.Contexts[left].ID < registry.Contexts[right].ID
	})
	return commandResult{Command: "list", Contexts: registry.Contexts}, nil
}

func executeStateChange(ctx context.Context, name string, arguments []string, state sessionstate.ContextState, deps dependencies) (commandResult, *commandFailure) {
	if len(arguments) != 1 {
		return commandResult{}, usageFailure(name, fmt.Sprintf("%s requires exactly one context", name))
	}
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	var changed sessionstate.Context
	action := "no_change"
	_, err := sessionstate.UpdateRegistryContext(ctx, root, func(registry *sessionstate.Registry) error {
		index, resolveErr := sessionstate.ResolveContext(*registry, arguments[0])
		if resolveErr != nil {
			return resolveErr
		}
		previous := registry.Contexts[index]
		var mutationErr error
		changed, mutationErr = sessionstate.SetContextStateAt(registry, arguments[0], state, deps.now())
		if mutationErr == nil && !reflect.DeepEqual(previous, changed) {
			action = name + "d"
			if name == "activate" {
				action = "activated"
			}
		}
		return mutationErr
	})
	if err != nil {
		if changed.ID != "" && committedContextContext(ctx, root, changed.ID, func(context sessionstate.Context) bool { return contextsEqual(context, changed) }, err) {
			return commandResult{Command: name, Contexts: []sessionstate.Context{changed}, Actions: []string{action}}, nil
		}
		return commandResult{}, classifyStateError(name+" context", err)
	}
	return commandResult{Command: name, Contexts: []sessionstate.Context{changed}, Actions: []string{action}}, nil
}

func executePurge(ctx context.Context, arguments []string, stdin io.Reader, stderr io.Writer, structured bool, deps dependencies) (commandResult, *commandFailure) {
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
	if err := sessionstate.RegistryStoreFor(root).LoadIntoContext(ctx, &registry); err != nil {
		return commandResult{}, classifyStateError("load context registry", err)
	}
	index, err := sessionstate.ResolveContext(registry, set.Arg(0))
	if err != nil {
		return commandResult{}, classifyStateError("select context to purge", err)
	}
	target := registry.Contexts[index]
	if target.Launcher.Kind != sessionstate.LauncherHerdr {
		return commandResult{}, failure(
			"context_kind",
			"purge accepts only Herdr terminal contexts",
			"Use sway-session app forget --yes "+string(target.ID)+" so live marks and launcher approval are removed transactionally.",
		)
	}
	if *yes && set.Arg(0) != string(target.ID) {
		return commandResult{
			Command:  "purge",
			Contexts: []sessionstate.Context{target},
			Preview:  true,
			Actions:  []string{"preview"},
			Message:  "Preview only; re-run with --yes and the exact context UUID returned here.",
		}, failure("confirmation_required", "purge --yes requires the exact context UUID", "Use the context ID from this preview; labels are not accepted for noninteractive deletion.")
	}
	if !*yes {
		if structured || !deps.stdinTerminal() {
			return commandResult{
				Command:  "purge",
				Contexts: []sessionstate.Context{target},
				Preview:  true,
				Actions:  []string{"preview"},
				Message:  "Preview only; re-run with --yes after verifying the exact context UUID.",
			}, failure("confirmation_required", "purge requires an interactive terminal or --yes", "Re-run with --yes only after verifying the selected context.")
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
		if !reflect.DeepEqual(current.Launcher, target.Launcher) {
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
	err = sessionstate.WithTerminalLifecycleLockContext(ctx, root, func() error {
		var purgeErr error
		for attempt := 0; attempt < 2; attempt++ {
			externalReconciled = false
			_, purgeErr = sessionstate.UpdateRegistryContext(ctx, root, mutate)
			if purgeErr == nil {
				break
			}
			if registryMissingContext(ctx, root, target.ID, purgeErr) {
				purgeErr = nil
				break
			}
			if !externalReconciled {
				break
			}
		}
		if purgeErr != nil {
			return purgeErr
		}
		return nil
	})
	if err != nil {
		if purgeCode != "" {
			return commandResult{}, failure(purgeCode, "purge context", err.Error())
		}
		return commandResult{}, classifyStateError("purge context", err)
	}
	return commandResult{Command: "purge", Contexts: []sessionstate.Context{target}, Actions: []string{"purged"}}, nil
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
	if selector != "" {
		queued, handled, err := queueDesktopRestore(ctx, root, selector)
		if err != nil {
			return commandResult{}, classifyStateError("queue desktop application restore", err)
		}
		if handled {
			result.Contexts = append(result.Contexts, queued)
			result.Message = "Desktop application restore queued for sway-session daemon."
			return result, nil
		}
	}
	var operationDiagnostics []diagnostic.Diagnostic
	initialTargets := map[sessionstate.ContextID]struct{}(nil)
	processed := make(map[sessionstate.ContextID]struct{})
	for {
		var wave commandResult
		var waveDiagnostics []diagnostic.Diagnostic
		more := false
		err := sessionstate.WithTerminalLifecycleLockContext(ctx, root, func() error {
			return sessionstate.InspectRegistryLockedContext(ctx, root, func(registry sessionstate.Registry) error {
				targets, err := restoreTargets(registry, selector, *requireActive)
				if err != nil {
					return err
				}
				if initialTargets == nil {
					initialTargets = make(map[sessionstate.ContextID]struct{}, len(targets))
					for _, target := range targets {
						initialTargets[target.ID] = struct{}{}
					}
					if len(initialTargets) == 0 {
						result.Message = "No active contexts to restore."
						return nil
					}
				}
				current := make([]sessionstate.Context, 0, len(targets))
				for _, target := range targets {
					if _, selected := initialTargets[target.ID]; !selected {
						continue
					}
					if _, done := processed[target.ID]; done {
						continue
					}
					current = append(current, target)
				}
				if len(current) == 0 {
					return nil
				}
				var handled []sessionstate.ContextID
				wave, waveDiagnostics, handled, more, err = restoreTerminalWave(ctx, socket, registry, current, deps)
				for _, id := range handled {
					processed[id] = struct{}{}
				}
				return err
			})
		})
		result.Contexts = append(result.Contexts, wave.Contexts...)
		operationDiagnostics = append(operationDiagnostics, waveDiagnostics...)
		if err != nil {
			classified := classifyStateError("restore contexts", err)
			operationDiagnostics = append(operationDiagnostics, classified.diagnostics...)
			sort.Slice(result.Contexts, func(left int, right int) bool { return result.Contexts[left].ID < result.Contexts[right].ID })
			return result, failures(operationDiagnostics)
		}
		if !more || selector != "" {
			break
		}
	}
	sort.Slice(result.Contexts, func(left int, right int) bool { return result.Contexts[left].ID < result.Contexts[right].ID })
	if len(operationDiagnostics) != 0 {
		return result, failures(operationDiagnostics)
	}
	return result, nil
}

func restoreTerminalWave(
	ctx context.Context,
	socket string,
	registry sessionstate.Registry,
	targets []sessionstate.Context,
	deps dependencies,
) (commandResult, []diagnostic.Diagnostic, []sessionstate.ContextID, bool, error) {
	result := commandResult{Command: "restore", Contexts: []sessionstate.Context{}}
	var operationDiagnostics []diagnostic.Diagnostic
	processed := make([]sessionstate.ContextID, 0, len(targets))
	client := deps.newSwayClient(socket)
	if client == nil {
		return result, nil, nil, false, errors.New("sway client is nil")
	}
	defer client.Close()
	tree, err := requestTree(ctx, client)
	if err != nil {
		return result, nil, nil, false, err
	}
	observed, observationIssues, err := sessionstate.ObserveManagedWindowsIsolated(tree, registry)
	if err != nil {
		return result, nil, nil, false, fmt.Errorf("observe managed windows: %w", err)
	}
	issueByID := make(map[sessionstate.ContextID]error, len(observationIssues))
	for _, issue := range observationIssues {
		issueByID[issue.ContextID] = issue.Cause
	}
	waiting := make(map[sessionstate.ContextID]sessionstate.Context)
	accepted := make(map[sessionstate.ContextID]int64)
	for _, target := range targets {
		if issue := issueByID[target.ID]; issue != nil {
			operationDiagnostics = append(operationDiagnostics, diagnosticForContext("duplicate_window", target, issue, "Resolve this context's duplicate or structurally invalid managed window before retrying."))
			processed = append(processed, target.ID)
			continue
		}
		if window, exists := observed[target.ID]; exists {
			result.Contexts = append(result.Contexts, target)
			accepted[target.ID] = window.ContainerID
			processed = append(processed, target.ID)
			continue
		}
		waiting[target.ID] = target
	}

	batch := make(map[sessionstate.ContextID]sessionstate.Context, maxConcurrentTerminalRestores)
	for _, id := range sortedWaitingIDs(waiting) {
		if len(batch) == maxConcurrentTerminalRestores {
			break
		}
		target := waiting[id]
		delete(waiting, id)
		processed = append(processed, id)
		if target.Launcher.Terminal == nil {
			operationDiagnostics = append(operationDiagnostics, diagnosticForContext("terminal_adapter", target, errors.New("herdr context has no terminal adapter"), "Reset the invalid session state and recreate the context."))
			continue
		}
		programName, err := sessionstate.TerminalAdapterExecutableName(target.Launcher.Terminal.Adapter)
		if err != nil {
			operationDiagnostics = append(operationDiagnostics, diagnosticForContext("terminal_adapter", target, err, "Use a supported typed terminal adapter."))
			continue
		}
		terminalExecutable, err := deps.resolveProgram(programName)
		if err != nil {
			operationDiagnostics = append(operationDiagnostics, diagnosticForContext("missing_executable", target, err, fmt.Sprintf("Install the %s terminal adapter and ensure it is on PATH.", target.Launcher.Terminal.Adapter)))
			continue
		}
		sessionManager, err := terminalSessionManagerForContext(target, deps)
		if err != nil {
			operationDiagnostics = append(operationDiagnostics, diagnosticForContext("session_manager", target, err, "Use a supported typed terminal session manager."))
			continue
		}
		spec, err := sessionManager.BuildProcessSpec(target, terminalExecutable)
		if err != nil {
			code := "session_manager"
			hint := "Verify the selected terminal session manager and its private state configuration."
			switch {
			case errors.Is(err, errHerdrSessionPath):
				code = "herdr_path"
				hint = "Shorten XDG_CONFIG_HOME, or purge this context and create a fresh terminal with sway-session terminal --new."
			case errors.Is(err, errHerdrPaneHistory):
				code = "pane_history"
				hint = "Set [experimental] pane_history = true and keep Herdr state owner-only."
			case errors.Is(err, errHerdrExecutable):
				code = "missing_executable"
				hint = "Install Herdr and ensure it is on PATH."
			}
			operationDiagnostics = append(operationDiagnostics, diagnosticForContext(code, target, err, hint))
			continue
		}
		pending, err := deps.findPendingProcess("/proc", spec)
		if err != nil {
			operationDiagnostics = append(operationDiagnostics, diagnosticForContext("process_observation", target, err, ""))
			continue
		}
		if len(pending) > 1 {
			operationDiagnostics = append(operationDiagnostics, diagnosticForContext("duplicate_launch", target, fmt.Errorf("multiple pending %s terminal processes %v", target.Launcher.Terminal.Adapter, pending), "Stop the duplicate processes before retrying."))
			continue
		}
		if len(pending) == 0 {
			if err := sessionstate.ValidateTerminalCwd(target.Launcher.Cwd); err != nil {
				operationDiagnostics = append(operationDiagnostics, diagnosticForContext("project_path", target, err, "Restore or update the persisted terminal working directory before retrying."))
				continue
			}
			if err := deps.processStarter.Start(spec); err != nil {
				operationDiagnostics = append(operationDiagnostics, diagnosticForContext("launch", target, err, ""))
				continue
			}
		}
		batch[id] = target
	}
	more := len(waiting) != 0
	if len(batch) == 0 {
		confirmRestoreMappings(ctx, client, registry, deps, accepted, &result, &operationDiagnostics)
		return result, operationDiagnostics, processed, more, nil
	}

	deadline := deps.now().Add(deps.settleTimeout)
	for len(batch) != 0 {
		tree, err = requestTree(ctx, client)
		if err != nil {
			appendAllRestoreFailures(&operationDiagnostics, batch, "sway_tree", err, "The launched process remains detectable; retry after Sway IPC recovers.")
			appendAllRestoreFailures(&operationDiagnostics, waiting, "sway_tree", err, "Retry after Sway IPC recovers.")
			for id := range waiting {
				processed = append(processed, id)
			}
			more = false
			break
		}
		observed, observationIssues, err = sessionstate.ObserveManagedWindowsIsolated(tree, registry)
		if err != nil {
			appendAllRestoreFailures(&operationDiagnostics, batch, "sway_tree", err, "Resolve malformed managed identities before retrying.")
			appendAllRestoreFailures(&operationDiagnostics, waiting, "sway_tree", err, "Resolve malformed managed identities before retrying.")
			for id := range waiting {
				processed = append(processed, id)
			}
			more = false
			break
		}
		for _, issue := range observationIssues {
			target, exists := batch[issue.ContextID]
			if !exists {
				continue
			}
			operationDiagnostics = append(operationDiagnostics, diagnosticForContext("duplicate_window", target, issue.Cause, "Resolve this context's duplicate or structurally invalid managed window before retrying."))
			delete(batch, issue.ContextID)
		}
		for id, target := range batch {
			if window, exists := observed[id]; exists {
				result.Contexts = append(result.Contexts, target)
				accepted[id] = window.ContainerID
				delete(batch, id)
			}
		}
		if len(batch) == 0 {
			break
		}
		if !deps.now().Before(deadline) {
			appendAllRestoreFailures(&operationDiagnostics, batch, "mapping_timeout", errors.New("window did not appear before the restore deadline"), "A matching pending process prevents duplicate launches; retry after it maps or exits.")
			if len(waiting) != 0 {
				operationDiagnostics = append(operationDiagnostics, diagnostic.Diagnostic{
					Level:   diagnostic.LevelError,
					Code:    "restore_deferred",
					Message: "automatic terminal restore paused after a mapping timeout",
					Hint:    fmt.Sprintf("%d additional active terminal contexts were not started; resolve the timed-out adapters and run sway-session restore again.", len(waiting)),
				})
			}
			more = false
			break
		}
		deps.sleep(100 * time.Millisecond)
	}
	confirmRestoreMappings(ctx, client, registry, deps, accepted, &result, &operationDiagnostics)
	return result, operationDiagnostics, processed, more, nil
}

func confirmRestoreMappings(
	ctx context.Context,
	client swayRequester,
	registry sessionstate.Registry,
	deps dependencies,
	accepted map[sessionstate.ContextID]int64,
	result *commandResult,
	diagnostics *[]diagnostic.Diagnostic,
) {
	if len(accepted) == 0 {
		return
	}
	deps.sleep(deps.stabilityDelay)
	tree, err := requestTree(ctx, client)
	if err != nil {
		removeUnstableRestoreResults(accepted, result, diagnostics, nil, nil, err)
		return
	}
	observed, issues, err := sessionstate.ObserveManagedWindowsIsolated(tree, registry)
	if err != nil {
		removeUnstableRestoreResults(accepted, result, diagnostics, nil, nil, err)
		return
	}
	issueByID := make(map[sessionstate.ContextID]error, len(issues))
	for _, issue := range issues {
		issueByID[issue.ContextID] = issue.Cause
	}
	removeUnstableRestoreResults(accepted, result, diagnostics, observed, issueByID, nil)
}

func removeUnstableRestoreResults(
	accepted map[sessionstate.ContextID]int64,
	result *commandResult,
	diagnostics *[]diagnostic.Diagnostic,
	observed map[sessionstate.ContextID]sessionstate.ManagedWindow,
	issues map[sessionstate.ContextID]error,
	observationErr error,
) {
	stable := result.Contexts[:0]
	for _, target := range result.Contexts {
		expected, mustConfirm := accepted[target.ID]
		if !mustConfirm {
			stable = append(stable, target)
			continue
		}
		cause := observationErr
		if cause == nil {
			cause = issues[target.ID]
		}
		window, exists := observed[target.ID]
		if cause == nil && (!exists || window.ContainerID != expected) {
			cause = errors.New("terminal window disappeared after it was mapped")
		}
		if cause == nil {
			stable = append(stable, target)
			continue
		}
		*diagnostics = append(*diagnostics, diagnosticForContext(
			"mapping_unstable", target, cause,
			"The registry remains active; retry sway-session terminal or restore to attach the existing session again.",
		))
	}
	result.Contexts = stable
}

var errNotDesktopApplication = errors.New("selected context is not a desktop application")

func queueDesktopRestore(ctx context.Context, root string, selector string) (sessionstate.Context, bool, error) {
	var queued sessionstate.Context
	_, err := sessionstate.UpdateRegistryContext(ctx, root, func(registry *sessionstate.Registry) error {
		index, err := sessionstate.ResolveContext(*registry, selector)
		if err != nil {
			return err
		}
		if registry.Contexts[index].App == nil {
			return errNotDesktopApplication
		}
		if registry.Contexts[index].State != sessionstate.ContextActive {
			return fmt.Errorf("desktop application context %q is archived; activate it before restore", registry.Contexts[index].ID)
		}
		registry.Contexts[index].App.DesiredOpen = true
		queued = registry.Contexts[index]
		return registry.Validate()
	})
	if errors.Is(err, errNotDesktopApplication) {
		return sessionstate.Context{}, false, nil
	}
	if err != nil {
		if queued.ID != "" && committedContext(root, queued.ID, func(context sessionstate.Context) bool {
			return contextsEqual(context, queued)
		}, err) {
			return queued, true, nil
		}
		return sessionstate.Context{}, true, err
	}
	return queued, true, nil
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
			return nil, fmt.Errorf("desktop application context %q must be restored through the session daemon", selected.ID)
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

func requestTree(ctx context.Context, client swayRequester) (*swayipc.TreeNode, error) {
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
	return committedContextContext(context.Background(), root, id, predicate, updateErr)
}

func committedContextContext(ctx context.Context, root string, id sessionstate.ContextID, predicate func(sessionstate.Context) bool, updateErr error) bool {
	var unknown *statefile.CommitOutcomeUnknownError
	if !errors.As(updateErr, &unknown) {
		return false
	}
	reconcileCtx, cancel := commandReconciliationContext(ctx)
	defer cancel()
	registry := sessionstate.Registry{}
	if err := sessionstate.RegistryStoreFor(root).LoadIntoContext(reconcileCtx, &registry); err != nil {
		return false
	}
	for _, context := range registry.Contexts {
		if context.ID == id {
			return predicate(context)
		}
	}
	return false
}

func registryMissingContext(ctx context.Context, root string, id sessionstate.ContextID, updateErr error) bool {
	var unknown *statefile.CommitOutcomeUnknownError
	if !errors.As(updateErr, &unknown) {
		return false
	}
	reconcileCtx, cancel := commandReconciliationContext(ctx)
	defer cancel()
	registry := sessionstate.Registry{}
	if err := sessionstate.RegistryStoreFor(root).LoadIntoContext(reconcileCtx, &registry); err != nil {
		return false
	}
	for _, context := range registry.Contexts {
		if context.ID == id {
			return false
		}
	}
	return true
}

func commandReconciliationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func contextsEqual(left sessionstate.Context, right sessionstate.Context) bool {
	return reflect.DeepEqual(left, right)
}
