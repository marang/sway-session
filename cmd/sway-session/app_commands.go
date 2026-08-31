package main

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
	"github.com/marang/sway-title-animator/internal/swayipc"
)

func executeApp(arguments []string, deps dependencies) (commandResult, *commandFailure) {
	if len(arguments) == 0 {
		return commandResult{}, usageFailure("app", "app requires a subcommand")
	}
	subcommand := arguments[0]
	arguments = arguments[1:]
	switch subcommand {
	case "register-focused":
		return executeAppRegisterFocused(arguments, deps)
	case "register-workspace":
		return executeAppRegisterWorkspace(arguments, deps)
	case "confirm":
		return executeAppConfirm(arguments, deps)
	case "status":
		return executeAppStatus(arguments, deps)
	case "list":
		return executeAppList(arguments, deps)
	case "rebind-focused":
		return executeAppRebindFocused(arguments, deps)
	case "reapprove":
		return executeAppReapprove(arguments, deps)
	case "pin", "unpin":
		return executeAppPolicy(subcommand, arguments, deps)
	case "archive", "activate":
		state := sessionstate.ContextArchived
		if subcommand == "activate" {
			state = sessionstate.ContextActive
		}
		return executeAppStateChange(subcommand, arguments, state, deps)
	case "forget":
		return executeAppForget(arguments, deps)
	default:
		return commandResult{}, usageFailure("app", fmt.Sprintf("unknown app subcommand %q", subcommand))
	}
}

func executeAppRegisterFocused(arguments []string, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("app register-focused")
	socketFlag := set.String("socket", "", "Sway IPC socket")
	desktopID := set.String("desktop-id", "", "exact desktop file ID for an ambiguous match")
	yes := set.Bool("yes", false, "approve without swaynag")
	if err := set.Parse(arguments); err != nil || set.NArg() != 0 {
		return commandResult{}, usageFailure("app", "app register-focused accepts only --socket, --desktop-id, and --yes")
	}
	environment, commandFailure := loadApplicationEnvironment(*socketFlag, deps, true, false)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	defer environment.close()
	resolution, err := sessionstate.ResolveFocusedApplication(environment.tree, environment.catalog, environment.registry)
	if err != nil {
		return commandResult{}, failure("app_resolution", "resolve focused application", err.Error())
	}
	if resolution.Registered != nil {
		if err := sessionstate.RepairApplicationMark(environment.client, resolution.Window.ContainerID, resolution.Registered.ID); err != nil {
			return commandResult{}, failure("sway_mark", "repair registered application mark", err.Error())
		}
		return commandResult{Command: "app status", Contexts: []sessionstate.Context{*resolution.Registered}, Message: "Focused application is already registered; its mark is healthy."}, nil
	}
	catalog, err := deps.desktopCatalog()
	if err != nil {
		return commandResult{}, failure("desktop_catalog", "load desktop application catalog", err.Error())
	}
	resolution.Candidates = sessionstate.DesktopCandidatesForWindow(resolution.Window, catalog)
	candidates, err := selectDesktopCandidates(resolution.Candidates, *desktopID)
	if err != nil {
		return commandResult{}, failure("app_ambiguous", "select focused application launcher", err.Error())
	}
	if *yes {
		if len(candidates) != 1 {
			return commandResult{}, failure("app_ambiguous", "select focused application launcher", "--yes requires one exact match or --desktop-id")
		}
		operation, err := newRegisterOperation([]sessionstate.WindowApplication{resolution.Window}, candidates, deps)
		if err != nil {
			return commandResult{}, failure("app_operation", "prepare focused registration", err.Error())
		}
		return applyApplicationOperation(operation, deps, *socketFlag)
	}
	choices := make([]sessionstate.ApprovalChoice, 0, len(candidates))
	for _, candidate := range candidates {
		operation, err := newRegisterOperation([]sessionstate.WindowApplication{resolution.Window}, []sessionstate.DesktopEntry{candidate}, deps)
		if err != nil {
			return commandResult{}, failure("app_operation", "prepare focused registration", err.Error())
		}
		choice, err := storeApplicationOperation(operation, approvalChoiceLabel(candidate), deps)
		if err != nil {
			return commandResult{}, failure("app_operation", "store focused registration approval", err.Error())
		}
		choices = append(choices, choice)
	}
	message := fmt.Sprintf("Register the focused application on workspace %s for session restore?", resolution.Window.Workspace)
	if len(candidates) == 1 {
		summary, summaryErr := sessionstate.DesktopApprovalSummary(candidates[0])
		if summaryErr != nil {
			return commandResult{}, failure("launcher_trust", "preview desktop launcher", summaryErr.Error())
		}
		message = fmt.Sprintf("Register %s on workspace %s for session restore?", summary, resolution.Window.Workspace)
	}
	if err := deps.presentApproval(message, choices); err != nil {
		return commandResult{}, failure("approval_ui", "open application registration confirmation", err.Error())
	}
	return commandResult{Command: "app register-focused", Message: "Registration confirmation opened."}, nil
}

func executeAppRegisterWorkspace(arguments []string, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("app register-workspace")
	socketFlag := set.String("socket", "", "Sway IPC socket")
	yes := set.Bool("yes", false, "approve without swaynag")
	if err := set.Parse(arguments); err != nil || set.NArg() != 0 {
		return commandResult{}, usageFailure("app", "app register-workspace accepts only --socket and --yes")
	}
	environment, commandFailure := loadApplicationEnvironment(*socketFlag, deps, true, true)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	defer environment.close()
	windows, err := sessionstate.FocusedWorkspaceApplications(environment.tree)
	if err != nil {
		return commandResult{}, failure("app_resolution", "resolve focused workspace applications", err.Error())
	}
	pendingWindows := make([]sessionstate.WindowApplication, 0, len(windows))
	candidates := make([]sessionstate.DesktopEntry, 0, len(windows))
	for _, window := range windows {
		resolution, err := sessionstate.ResolveApplication(window, environment.catalog, environment.registry)
		if err != nil {
			return commandResult{}, failure("app_resolution", "resolve workspace application", err.Error())
		}
		if resolution.Registered != nil {
			continue
		}
		if len(resolution.Candidates) != 1 {
			return commandResult{}, failure("app_ambiguous", "preview workspace registration", fmt.Sprintf("container %d has %d exact desktop-entry matches; register it individually", window.ContainerID, len(resolution.Candidates)))
		}
		duplicate := false
		for index := range pendingWindows {
			if candidates[index].ID == resolution.Candidates[0].ID || sessionstate.ApplicationIdentitiesOverlap(pendingWindows[index].Identity, window.Identity) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		pendingWindows = append(pendingWindows, window)
		candidates = append(candidates, resolution.Candidates[0])
	}
	if len(pendingWindows) == 0 {
		return commandResult{Command: "app register-workspace", Message: "No unregistered eligible applications on the focused workspace."}, nil
	}
	operation, err := newRegisterOperation(pendingWindows, candidates, deps)
	if err != nil {
		return commandResult{}, failure("app_operation", "prepare workspace registration", err.Error())
	}
	if *yes {
		return applyApplicationOperation(operation, deps, *socketFlag)
	}
	choice, err := storeApplicationOperation(operation, fmt.Sprintf("Register %d applications", len(operation.Items)), deps)
	if err != nil {
		return commandResult{}, failure("app_operation", "store workspace registration approval", err.Error())
	}
	names := make([]string, len(candidates))
	for index := range candidates {
		names[index] = approvalChoiceLabel(candidates[index])
	}
	message := boundedDisplay("Register these applications from workspace "+pendingWindows[0].Workspace+": "+strings.Join(names, ", ")+"?", 4000)
	if err := deps.presentApproval(message, []sessionstate.ApprovalChoice{choice}); err != nil {
		return commandResult{}, failure("approval_ui", "open workspace registration confirmation", err.Error())
	}
	return commandResult{Command: "app register-workspace", Message: "Workspace registration confirmation opened."}, nil
}

func executeAppConfirm(arguments []string, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("app confirm")
	if err := set.Parse(arguments); err != nil || set.NArg() != 1 {
		return commandResult{}, usageFailure("app", "app confirm requires exactly one operation token")
	}
	store, err := deps.operationStore()
	if err != nil {
		return commandResult{}, failure("app_operation", "resolve application operation store", err.Error())
	}
	operation, err := store.Consume(set.Arg(0))
	if err != nil {
		return commandResult{}, failure("app_operation", "consume application approval", err.Error())
	}
	return applyApplicationOperation(operation, deps, "")
}

func executeAppStatus(arguments []string, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("app status")
	socketFlag := set.String("socket", "", "Sway IPC socket")
	if err := set.Parse(arguments); err != nil || set.NArg() != 0 {
		return commandResult{}, usageFailure("app", "app status accepts only --socket")
	}
	environment, commandFailure := loadApplicationEnvironment(*socketFlag, deps, true, false)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	defer environment.close()
	resolution, err := sessionstate.ResolveFocusedApplication(environment.tree, environment.catalog, environment.registry)
	if err != nil {
		return commandResult{}, failure("app_resolution", "resolve focused application", err.Error())
	}
	if resolution.Registered == nil {
		return commandResult{Command: "app status", Message: "Focused application is not registered."}, nil
	}
	return commandResult{Command: "app status", Contexts: []sessionstate.Context{*resolution.Registered}}, nil
}

func executeAppList(arguments []string, deps dependencies) (commandResult, *commandFailure) {
	if len(arguments) != 0 {
		return commandResult{}, usageFailure("app", "app list accepts no arguments")
	}
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	registry, err := loadRegistry(root)
	if err != nil {
		return commandResult{}, classifyStateError("load application registry", err)
	}
	contexts := make([]sessionstate.Context, 0)
	for _, context := range registry.Contexts {
		if context.App != nil {
			contexts = append(contexts, context)
		}
	}
	sort.Slice(contexts, func(left, right int) bool { return contexts[left].ID < contexts[right].ID })
	return commandResult{Command: "app list", Contexts: contexts}, nil
}

func executeAppRebindFocused(arguments []string, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("app rebind-focused")
	socketFlag := set.String("socket", "", "Sway IPC socket")
	desktopID := set.String("desktop-id", "", "exact desktop file ID")
	yes := set.Bool("yes", false, "approve without swaynag")
	if err := set.Parse(arguments); err != nil || set.NArg() != 1 {
		return commandResult{}, usageFailure("app", "app rebind-focused requires one context and optional --socket, --desktop-id, or --yes")
	}
	environment, commandFailure := loadApplicationEnvironment(*socketFlag, deps, true, true)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	defer environment.close()
	index, err := sessionstate.ResolveContext(environment.registry, set.Arg(0))
	if err != nil || environment.registry.Contexts[index].App == nil {
		if err == nil {
			err = errors.New("selected context is not a desktop application")
		}
		return commandResult{}, classifyStateError("select application to rebind", err)
	}
	window, err := sessionstate.FocusedApplicationWindow(environment.tree)
	if err != nil {
		return commandResult{}, failure("app_resolution", "resolve focused application", err.Error())
	}
	if len(window.ContextMarks) == 1 && window.ContextMarks[0] != environment.registry.Contexts[index].ID {
		return commandResult{}, failure("app_resolution", "resolve focused application", "focused window belongs to another persistent context")
	}
	candidates, err := selectDesktopCandidates(sessionstate.DesktopCandidatesForWindow(window, environment.catalog), *desktopID)
	if err != nil || len(candidates) != 1 {
		if err == nil {
			err = errors.New("rebind requires one exact desktop-entry match or --desktop-id")
		}
		return commandResult{}, failure("app_ambiguous", "select rebind launcher", err.Error())
	}
	if err := validatePreviewCandidate(candidates[0]); err != nil {
		return commandResult{}, failure("launcher_trust", "preview rebound desktop launcher", err.Error())
	}
	item := sessionstate.ApplicationOperationItem{ContextID: environment.registry.Contexts[index].ID, Window: &window, DesktopID: candidates[0].ID}
	operation := applicationOperation(sessionstate.OperationRebind, []sessionstate.ApplicationOperationItem{item}, deps)
	if *yes {
		return applyApplicationOperation(operation, deps, *socketFlag)
	}
	choice, err := storeApplicationOperation(operation, "Rebind to "+candidates[0].Name, deps)
	if err != nil {
		return commandResult{}, failure("app_operation", "store application rebind approval", err.Error())
	}
	old := environment.registry.Contexts[index]
	summary, err := sessionstate.DesktopApprovalSummary(candidates[0])
	if err != nil {
		return commandResult{}, failure("launcher_trust", "preview rebound desktop launcher", err.Error())
	}
	message := fmt.Sprintf("Rebind %s (%s) to %s?", contextDisplayName(old), launcherDisplay(old.Launcher), summary)
	if err := deps.presentApproval(message, []sessionstate.ApprovalChoice{choice}); err != nil {
		return commandResult{}, failure("approval_ui", "open application rebind confirmation", err.Error())
	}
	return commandResult{Command: "app rebind-focused", Message: "Application rebind confirmation opened."}, nil
}

func executeAppReapprove(arguments []string, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("app reapprove")
	yes := set.Bool("yes", false, "approve without swaynag")
	if err := set.Parse(arguments); err != nil || set.NArg() != 1 {
		return commandResult{}, usageFailure("app", "app reapprove requires one context and optional --yes")
	}
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	registry, err := loadRegistry(root)
	if err != nil {
		return commandResult{}, classifyStateError("load application registry", err)
	}
	index, err := sessionstate.ResolveContext(registry, set.Arg(0))
	if err != nil {
		return commandResult{}, classifyStateError("select application to reapprove", err)
	}
	context := registry.Contexts[index]
	if context.Launcher.Kind != sessionstate.LauncherDesktop || context.Launcher.DesktopOrigin != sessionstate.DesktopEntryUser {
		return commandResult{}, failure("app_reapproval", "reapprove application launcher", "only approved user-local desktop entries require reapproval")
	}
	item := sessionstate.ApplicationOperationItem{ContextID: context.ID, DesktopID: context.Launcher.DesktopID}
	operation := applicationOperation(sessionstate.OperationReapprove, []sessionstate.ApplicationOperationItem{item}, deps)
	if *yes {
		return applyApplicationOperation(operation, deps, "")
	}
	choice, err := storeApplicationOperation(operation, "Reapprove "+contextDisplayName(context), deps)
	if err != nil {
		return commandResult{}, failure("app_operation", "store launcher reapproval", err.Error())
	}
	if err := deps.presentApproval("Approve the current user-local desktop entry and executable checksums for "+contextDisplayName(context)+"?", []sessionstate.ApprovalChoice{choice}); err != nil {
		return commandResult{}, failure("approval_ui", "open launcher reapproval confirmation", err.Error())
	}
	return commandResult{Command: "app reapprove", Message: "Launcher reapproval confirmation opened."}, nil
}

func executeAppPolicy(name string, arguments []string, deps dependencies) (commandResult, *commandFailure) {
	if len(arguments) != 1 {
		return commandResult{}, usageFailure("app", "app "+name+" requires exactly one context")
	}
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	policy := sessionstate.ApplicationRestoreFollow
	if name == "pin" {
		policy = sessionstate.ApplicationRestorePinned
	}
	var changed sessionstate.Context
	_, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
		var mutateErr error
		changed, mutateErr = sessionstate.SetApplicationRestorePolicy(registry, arguments[0], policy)
		return mutateErr
	})
	if err != nil && (changed.ID == "" || !registryContainsExactApplicationContext(root, changed)) {
		return commandResult{}, classifyStateError(name+" application", err)
	}
	return commandResult{Command: "app " + name, Contexts: []sessionstate.Context{changed}}, nil
}

func executeAppStateChange(name string, arguments []string, state sessionstate.ContextState, deps dependencies) (commandResult, *commandFailure) {
	if len(arguments) != 1 {
		return commandResult{}, usageFailure("app", "app "+name+" requires exactly one context")
	}
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	var changed sessionstate.Context
	_, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
		index, err := sessionstate.ResolveContext(*registry, arguments[0])
		if err != nil {
			return err
		}
		if registry.Contexts[index].App == nil {
			return errors.New("selected context is not a desktop application")
		}
		changed, err = sessionstate.SetContextState(registry, arguments[0], state)
		return err
	})
	if err != nil && (changed.ID == "" || !registryContainsExactApplicationContext(root, changed)) {
		return commandResult{}, classifyStateError(name+" application", err)
	}
	return commandResult{Command: "app " + name, Contexts: []sessionstate.Context{changed}}, nil
}

func executeAppForget(arguments []string, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("app forget")
	socketFlag := set.String("socket", "", "Sway IPC socket")
	yes := set.Bool("yes", false, "confirm permanent registry removal")
	if err := set.Parse(arguments); err != nil || set.NArg() != 1 || !*yes {
		return commandResult{}, usageFailure("app", "app forget requires --yes and exactly one context")
	}
	socket, commandFailure := applicationSocket(*socketFlag)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	client := deps.newSwayClient(socket)
	if client == nil {
		return commandResult{}, failure("sway", "connect to Sway", "Sway client is unavailable")
	}
	defer client.Close()
	removed, err := sessionstate.ForgetApplicationContext(root, client, set.Arg(0))
	if err != nil {
		return commandResult{}, classifyStateError("forget application", err)
	}
	if cleanupErr := sessionstate.RemoveDesktopApprovalSnapshot(root, removed.Launcher); cleanupErr != nil {
		return commandResult{Command: "app forget", Contexts: []sessionstate.Context{removed}}, failure("approval_cleanup", "application was forgotten but its protected launcher snapshot could not be removed", cleanupErr.Error())
	}
	return commandResult{Command: "app forget", Contexts: []sessionstate.Context{removed}}, nil
}

type applicationEnvironment struct {
	root     string
	registry sessionstate.Registry
	catalog  sessionstate.DesktopCatalog
	tree     *swayipc.TreeNode
	client   swayRequester
}

func (environment applicationEnvironment) close() {
	if environment.client != nil {
		environment.client.Close()
	}
}

func loadApplicationEnvironment(socketFlag string, deps dependencies, needTree bool, needCatalog bool) (applicationEnvironment, *commandFailure) {
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return applicationEnvironment{}, commandFailure
	}
	registry, err := loadRegistry(root)
	if err != nil {
		return applicationEnvironment{}, classifyStateError("load application registry", err)
	}
	catalog := sessionstate.DesktopCatalog{}
	if needCatalog {
		catalog, err = deps.desktopCatalog()
		if err != nil {
			return applicationEnvironment{}, failure("desktop_catalog", "load desktop application catalog", err.Error())
		}
	}
	environment := applicationEnvironment{root: root, registry: registry, catalog: catalog}
	if !needTree {
		return environment, nil
	}
	socket, commandFailure := applicationSocket(socketFlag)
	if commandFailure != nil {
		return applicationEnvironment{}, commandFailure
	}
	environment.client = deps.newSwayClient(socket)
	if environment.client == nil {
		return applicationEnvironment{}, failure("sway", "connect to Sway", "Sway client is unavailable")
	}
	tree, err := requestTree(environment.client)
	if err != nil {
		environment.close()
		return applicationEnvironment{}, failure("sway_tree", "observe Sway applications", err.Error())
	}
	environment.tree = tree
	return environment, nil
}

func applicationSocket(flagValue string) (string, *commandFailure) {
	socket := flagValue
	if socket == "" {
		socket = os.Getenv("SWAYSOCK")
	}
	if socket == "" || !strings.HasPrefix(socket, "/") {
		return "", failure("sway_socket", "a valid absolute Sway IPC socket is required", "Run inside Sway or pass --socket PATH.")
	}
	return socket, nil
}

func loadRegistry(root string) (sessionstate.Registry, error) {
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{}}
	err := sessionstate.RegistryFile(root).LoadInto(&registry)
	if errors.Is(err, os.ErrNotExist) {
		return registry, nil
	}
	return registry, err
}

func selectDesktopCandidates(candidates []sessionstate.DesktopEntry, exactID string) ([]sessionstate.DesktopEntry, error) {
	if exactID != "" {
		for _, candidate := range candidates {
			if candidate.ID == exactID {
				return []sessionstate.DesktopEntry{candidate}, nil
			}
		}
		return nil, fmt.Errorf("desktop entry %q is not an exact candidate for this window", exactID)
	}
	if len(candidates) == 0 {
		return nil, errors.New("no valid desktop entry matches this window")
	}
	if len(candidates) > 32 {
		return nil, fmt.Errorf("%d desktop entries match this window; maximum approval choices is 32", len(candidates))
	}
	return candidates, nil
}

func newRegisterOperation(windows []sessionstate.WindowApplication, candidates []sessionstate.DesktopEntry, deps dependencies) (sessionstate.ApplicationOperation, error) {
	if len(windows) == 0 || len(windows) != len(candidates) {
		return sessionstate.ApplicationOperation{}, errors.New("registration preview is empty or misaligned")
	}
	items := make([]sessionstate.ApplicationOperationItem, len(windows))
	for index := range windows {
		if err := validatePreviewCandidate(candidates[index]); err != nil {
			return sessionstate.ApplicationOperation{}, fmt.Errorf("%s: %w", candidates[index].ID, err)
		}
		id, err := deps.newContextID()
		if err != nil {
			return sessionstate.ApplicationOperation{}, err
		}
		window := windows[index]
		items[index] = sessionstate.ApplicationOperationItem{ContextID: id, Window: &window, DesktopID: candidates[index].ID}
	}
	return applicationOperation(sessionstate.OperationRegister, items, deps), nil
}

func applicationOperation(kind sessionstate.ApplicationOperationKind, items []sessionstate.ApplicationOperationItem, deps dependencies) sessionstate.ApplicationOperation {
	return sessionstate.ApplicationOperation{Version: sessionstate.ApplicationOperationVersion, Kind: kind, ExpiresAt: deps.now().UTC().Add(2 * time.Minute), Items: items}
}

func validatePreviewCandidate(entry sessionstate.DesktopEntry) error {
	return sessionstate.ValidateDesktopEntryForApproval(entry)
}

func approvalChoiceLabel(entry sessionstate.DesktopEntry) string {
	return boundedDisplay(entry.Name+" ("+entry.ID+")", 240)
}

func boundedDisplay(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	var result strings.Builder
	for _, character := range value {
		if result.Len()+len(string(character))+3 > maxBytes {
			break
		}
		result.WriteRune(character)
	}
	result.WriteString("...")
	return result.String()
}

func storeApplicationOperation(operation sessionstate.ApplicationOperation, label string, deps dependencies) (sessionstate.ApprovalChoice, error) {
	store, err := deps.operationStore()
	if err != nil {
		return sessionstate.ApprovalChoice{}, err
	}
	token, err := store.Create(operation)
	if err != nil {
		return sessionstate.ApprovalChoice{}, err
	}
	return sessionstate.ApprovalChoice{Label: label, Token: token}, nil
}

func applyApplicationOperation(operation sessionstate.ApplicationOperation, deps dependencies, socketOverride string) (commandResult, *commandFailure) {
	if err := operation.Validate(deps.now().UTC()); err != nil {
		return commandResult{}, failure("app_operation", "validate application approval", err.Error())
	}
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	catalog, err := deps.desktopCatalog()
	if err != nil {
		return commandResult{}, failure("desktop_catalog", "reload desktop application catalog", err.Error())
	}
	switch operation.Kind {
	case sessionstate.OperationRegister:
		return applyRegisterOperation(root, catalog, operation, deps, socketOverride)
	case sessionstate.OperationRebind:
		return applyRebindOperation(root, catalog, operation, deps, socketOverride)
	case sessionstate.OperationReapprove:
		return applyReapproveOperation(root, catalog, operation, deps)
	default:
		return commandResult{}, failure("app_operation", "apply application approval", "unsupported operation")
	}
}

func applyRegisterOperation(root string, catalog sessionstate.DesktopCatalog, operation sessionstate.ApplicationOperation, deps dependencies, socketOverride string) (commandResult, *commandFailure) {
	client, tree, commandFailure := operationSwayClient(deps, socketOverride)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	defer client.Close()
	contexts := make([]sessionstate.Context, 0, len(operation.Items))
	containers := make([]int64, 0, len(operation.Items))
	approvals := make([]sessionstate.DesktopApproval, 0, len(operation.Items))
	cleanup := func() {
		for _, approval := range approvals {
			_ = sessionstate.DiscardDesktopApproval(root, approval)
		}
	}
	for _, item := range operation.Items {
		window, err := sessionstate.ApplicationWindowByContainer(tree, item.Window.ContainerID)
		if err != nil || !reflect.DeepEqual(window, *item.Window) {
			cleanup()
			if err == nil {
				err = errors.New("application window identity or workspace changed while approval was pending")
			}
			return commandResult{}, failure("app_stale", "revalidate application approval", err.Error())
		}
		entry, ok := catalog.ByID(item.DesktopID)
		if !ok || !candidateContains(sessionstate.ApplicationResolution{Candidates: desktopCandidatesForWindow(window, catalog)}, item.DesktopID) {
			cleanup()
			return commandResult{}, failure("app_stale", "revalidate desktop launcher", "approved desktop entry no longer matches the window")
		}
		approval, err := sessionstate.PrepareDesktopApproval(root, item.ContextID, entry)
		if err != nil {
			cleanup()
			return commandResult{}, failure("launcher_trust", "approve desktop launcher", err.Error())
		}
		approvals = append(approvals, approval)
		if approval.Launcher.Kind == sessionstate.LauncherFlatpak {
			if err := deps.verifyFlatpak(approval.Launcher); err != nil {
				cleanup()
				return commandResult{}, failure("flatpak_installation", "verify Flatpak installation", err.Error())
			}
		}
		context, err := sessionstate.NewApplicationContext(item.ContextID, entry, window, approval.Launcher)
		if err != nil {
			cleanup()
			return commandResult{}, failure("invalid_context", "create application context", err.Error())
		}
		contexts = append(contexts, context)
		containers = append(containers, window.ContainerID)
	}
	if err := sessionstate.RegisterApplicationContexts(root, client, contexts, containers); err != nil {
		cleanup()
		return commandResult{}, classifyStateError("register desktop application", err)
	}
	return commandResult{Command: "app register", Contexts: contexts}, nil
}

func applyRebindOperation(root string, catalog sessionstate.DesktopCatalog, operation sessionstate.ApplicationOperation, deps dependencies, socketOverride string) (commandResult, *commandFailure) {
	item := operation.Items[0]
	client, tree, commandFailure := operationSwayClient(deps, socketOverride)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	defer client.Close()
	window, err := sessionstate.ApplicationWindowByContainer(tree, item.Window.ContainerID)
	if err != nil || !reflect.DeepEqual(window, *item.Window) {
		if err == nil {
			err = errors.New("focused application changed while rebind approval was pending")
		}
		return commandResult{}, failure("app_stale", "revalidate application rebind", err.Error())
	}
	entry, ok := catalog.ByID(item.DesktopID)
	if !ok || !candidateContains(sessionstate.ApplicationResolution{Candidates: desktopCandidatesForWindow(window, catalog)}, item.DesktopID) {
		return commandResult{}, failure("app_stale", "revalidate rebind launcher", "approved desktop entry no longer matches the window")
	}
	registry, err := loadRegistry(root)
	if err != nil {
		return commandResult{}, classifyStateError("load application registry", err)
	}
	index, err := sessionstate.ResolveContext(registry, string(item.ContextID))
	if err != nil || registry.Contexts[index].App == nil {
		if err == nil {
			err = errors.New("context is no longer a desktop application")
		}
		return commandResult{}, classifyStateError("select application to rebind", err)
	}
	previous := registry.Contexts[index]
	approval, err := sessionstate.PrepareDesktopApproval(root, item.ContextID, entry)
	if err != nil {
		return commandResult{}, failure("launcher_trust", "approve rebound desktop launcher", err.Error())
	}
	if approval.Launcher.Kind == sessionstate.LauncherFlatpak {
		if err := deps.verifyFlatpak(approval.Launcher); err != nil {
			_ = sessionstate.DiscardDesktopApproval(root, approval)
			return commandResult{}, failure("flatpak_installation", "verify rebound Flatpak installation", err.Error())
		}
	}
	replacement, err := sessionstate.NewApplicationContext(item.ContextID, entry, window, approval.Launcher)
	if err != nil {
		_ = sessionstate.DiscardDesktopApproval(root, approval)
		return commandResult{}, failure("invalid_context", "create rebound application context", err.Error())
	}
	replacement.State = previous.State
	replacement.App.DesiredOpen = previous.App.DesiredOpen
	replacement.App.RestorePolicy = previous.App.RestorePolicy
	old, err := sessionstate.RebindApplicationContext(root, client, replacement, window.ContainerID)
	if err != nil {
		_ = sessionstate.DiscardDesktopApproval(root, approval)
		return commandResult{}, classifyStateError("rebind desktop application", err)
	}
	if old.Launcher.ApprovedDesktopPath != replacement.Launcher.ApprovedDesktopPath {
		_ = sessionstate.RemoveDesktopApprovalSnapshot(root, old.Launcher)
	}
	return commandResult{Command: "app rebind-focused", Contexts: []sessionstate.Context{replacement}}, nil
}

func applyReapproveOperation(root string, catalog sessionstate.DesktopCatalog, operation sessionstate.ApplicationOperation, _ dependencies) (commandResult, *commandFailure) {
	item := operation.Items[0]
	registry, err := loadRegistry(root)
	if err != nil {
		return commandResult{}, classifyStateError("load application registry", err)
	}
	index, err := sessionstate.ResolveContext(registry, string(item.ContextID))
	if err != nil {
		return commandResult{}, classifyStateError("select application to reapprove", err)
	}
	previous := registry.Contexts[index]
	if previous.Launcher.Kind != sessionstate.LauncherDesktop || previous.Launcher.DesktopOrigin != sessionstate.DesktopEntryUser || previous.Launcher.DesktopID != item.DesktopID {
		return commandResult{}, failure("app_stale", "revalidate launcher reapproval", "application launcher changed while approval was pending")
	}
	entry, ok := catalog.ByID(item.DesktopID)
	if !ok {
		return commandResult{}, failure("app_stale", "revalidate launcher reapproval", "desktop entry no longer exists")
	}
	approval, err := sessionstate.PrepareDesktopApproval(root, item.ContextID, entry)
	if err != nil {
		return commandResult{}, failure("launcher_trust", "reapprove desktop launcher", err.Error())
	}
	replacement := previous
	replacement.Launcher = approval.Launcher
	_, err = sessionstate.UpdateRegistry(root, func(current *sessionstate.Registry) error {
		currentIndex, err := sessionstate.ResolveContext(*current, string(item.ContextID))
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current.Contexts[currentIndex], previous) {
			return errors.New("application context changed while launcher reapproval was pending")
		}
		current.Contexts[currentIndex] = replacement
		return current.Validate()
	})
	if err != nil && !registryContainsExactApplicationContext(root, replacement) {
		_ = sessionstate.DiscardDesktopApproval(root, approval)
		return commandResult{}, classifyStateError("reapprove desktop launcher", err)
	}
	if previous.Launcher.ApprovedDesktopPath != replacement.Launcher.ApprovedDesktopPath {
		_ = sessionstate.RemoveDesktopApprovalSnapshot(root, previous.Launcher)
	}
	return commandResult{Command: "app reapprove", Contexts: []sessionstate.Context{replacement}}, nil
}

func operationSwayClient(deps dependencies, socketOverride string) (swayRequester, *swayipc.TreeNode, *commandFailure) {
	socket, commandFailure := applicationSocket(socketOverride)
	if commandFailure != nil {
		return nil, nil, commandFailure
	}
	client := deps.newSwayClient(socket)
	if client == nil {
		return nil, nil, failure("sway", "connect to Sway", "Sway client is unavailable")
	}
	tree, err := requestTree(client)
	if err != nil {
		client.Close()
		return nil, nil, failure("sway_tree", "observe Sway applications", err.Error())
	}
	return client, tree, nil
}

func desktopCandidatesForWindow(window sessionstate.WindowApplication, catalog sessionstate.DesktopCatalog) []sessionstate.DesktopEntry {
	return sessionstate.DesktopCandidatesForWindow(window, catalog)
}

func registryContainsExactApplicationContext(root string, expected sessionstate.Context) bool {
	registry, err := loadRegistry(root)
	if err != nil {
		return false
	}
	index, err := sessionstate.ResolveContext(registry, string(expected.ID))
	return err == nil && reflect.DeepEqual(registry.Contexts[index], expected)
}

func candidateContains(resolution sessionstate.ApplicationResolution, id string) bool {
	for _, candidate := range resolution.Candidates {
		if candidate.ID == id {
			return true
		}
	}
	return false
}

func contextDisplayName(context sessionstate.Context) string {
	if context.Label != "" {
		return context.Label
	}
	return string(context.ID)
}

func launcherDisplay(launcher sessionstate.Launcher) string {
	if launcher.Kind == sessionstate.LauncherFlatpak {
		return string(launcher.Kind) + ":" + launcher.FlatpakID
	}
	return string(launcher.Kind) + ":" + launcher.DesktopID
}
