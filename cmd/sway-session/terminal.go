package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
)

type terminalRoleFlags []string

func (roles *terminalRoleFlags) String() string { return fmt.Sprint([]string(*roles)) }
func (roles *terminalRoleFlags) Set(value string) error {
	*roles = append(*roles, value)
	return nil
}

func executeTerminal(ctx context.Context, arguments []string, stdin io.Reader, stdout io.Writer, structured bool, configPath string, deps dependencies) (commandResult, *commandFailure) {
	if len(arguments) > 0 {
		switch arguments[0] {
		case "manage":
			return executeTerminalManage(ctx, arguments[1:], stdin, stdout, structured, configPath, deps)
		case "list":
			return executeTerminalList(ctx, arguments[1:], deps)
		case "status":
			return executeTerminalStatus(ctx, arguments[1:], deps)
		case "cleanup":
			return executeTerminalCleanup(ctx, arguments[1:], deps)
		case "reconfigure":
			return executeTerminalReconfigure(ctx, arguments[1:], configPath, deps)
		case "rename":
			return executeTerminalRename(ctx, arguments[1:], deps)
		}
	}
	set := newFlagSet("terminal")
	project := set.String("project", "", "stable project identity")
	contextFlag := set.String("context", "", "exact registered terminal context UUID")
	cwdFlag := set.String("cwd", "", "initial working directory")
	label := set.String("label", "", "presentation label")
	socketFlag := set.String("socket", "", "Sway IPC socket")
	newTerminal := set.Bool("new", false, "create a fresh persistent terminal context")
	ephemeral := set.Bool("ephemeral", false, "open an ordinary terminal without persistence")
	var roles terminalRoleFlags
	set.Var(&roles, "role", "session-manager role; repeat exactly twice (for example codex and shell)")
	if err := set.Parse(arguments); err != nil || set.NArg() != 0 {
		return commandResult{}, usageFailure("terminal", "terminal accepts only typed terminal options and no positional arguments")
	}
	provided := make(map[string]bool)
	set.Visit(func(option *flag.Flag) {
		provided[option.Name] = true
	})
	if *newTerminal && (provided["project"] || provided["context"] || provided["ephemeral"]) {
		return commandResult{}, usageFailure("terminal", "--new cannot be combined with --project, --context, or --ephemeral")
	}
	if provided["context"] && (*contextFlag == "" || provided["project"] || provided["cwd"] || provided["label"] || provided["ephemeral"]) {
		return commandResult{}, usageFailure("terminal", "--context requires an exact UUID and cannot be combined with --project, --cwd, --label, or --ephemeral")
	}
	if provided["project"] && *project == "" {
		return commandResult{}, usageFailure("terminal", "--project requires a non-empty name")
	}
	if provided["cwd"] && *cwdFlag == "" {
		return commandResult{}, failure("terminal_cwd", "validate terminal working directory", "--cwd requires a non-empty path")
	}
	if *ephemeral && (provided["new"] || provided["project"] || provided["context"] || provided["label"] || provided["socket"] || provided["role"]) {
		return commandResult{}, usageFailure("terminal", "--ephemeral accepts only --cwd")
	}
	if len(roles) != 0 && len(roles) != 2 {
		return commandResult{}, usageFailure("terminal", "--role must be omitted or repeated exactly twice")
	}
	if err := sessionstate.ValidateContextLabel(*label); err != nil {
		return commandResult{}, failure("terminal_label", "validate terminal label", err.Error())
	}
	var identity sessionstate.TerminalIdentity
	if !*newTerminal && *contextFlag == "" {
		identity = sessionstate.TerminalIdentity{Kind: sessionstate.TerminalIdentityDefault}
	}
	if *project != "" {
		var err error
		identity, err = sessionstate.ParseTerminalIdentity(*project)
		if err != nil {
			return commandResult{}, failure("terminal_identity", "validate terminal project identity", err.Error())
		}
	}
	cwdExplicit := provided["cwd"]
	cwd := "/"
	var err error
	if *contextFlag == "" {
		cwd, err = terminalWorkingDirectory(*cwdFlag, *project, deps)
		if err != nil {
			return commandResult{}, failure("terminal_cwd", "resolve terminal working directory", err.Error())
		}
		if err := sessionstate.ValidateTerminalCwd(cwd); err != nil {
			return commandResult{}, failure("terminal_cwd", "validate terminal working directory", err.Error())
		}
	}
	contextID := sessionstate.ContextID(*contextFlag)
	if contextID != "" {
		if err := contextID.Validate(); err != nil {
			return commandResult{}, failure("terminal_context", "validate terminal context UUID", err.Error())
		}
	}
	socket := *socketFlag
	if !*ephemeral && provided["socket"] && (socket == "" || !filepath.IsAbs(socket) || filepath.Clean(socket) != socket) {
		return commandResult{}, failure("sway_socket", "a valid absolute Sway IPC socket is required", "Run inside Sway or pass --socket PATH.")
	}
	if deps.loadSessionConfig == nil {
		return commandResult{}, failure("terminal_config", "load terminal configuration", "session config dependency is unavailable")
	}
	config, _, err := deps.loadSessionConfig(configPath)
	if err != nil {
		return commandResult{}, failure("terminal_config", "load terminal configuration", err.Error())
	}

	if *ephemeral {
		program, err := sessionstate.TerminalAdapterExecutableName(config.Terminal.Adapter)
		if err != nil {
			return commandResult{}, failure("terminal_adapter", "select terminal adapter", err.Error())
		}
		executable, err := deps.resolveProgram(program)
		if err != nil {
			return commandResult{}, failure("missing_executable", "resolve terminal executable", err.Error())
		}
		spec, err := sessionstate.BuildEphemeralTerminalProcessSpec(config.Terminal.Adapter, cwd, executable)
		if err != nil {
			return commandResult{}, failure("terminal_adapter", "build terminal process", err.Error())
		}
		if deps.processStarter == nil {
			return commandResult{}, failure("terminal_launch", "start ephemeral terminal", "process starter is unavailable")
		}
		if err := deps.processStarter.Start(spec); err != nil {
			return commandResult{}, failure("terminal_launch", "start ephemeral terminal", err.Error())
		}
		return commandResult{
			Command: "terminal",
			Message: "Ephemeral terminal opened.",
			Terminal: &terminalCommandResult{
				Adapter: config.Terminal.Adapter, Actions: []sessionstate.TerminalOpenAction{sessionstate.TerminalActionOpened}, Ephemeral: true,
			},
		}, nil
	}
	sessionManager, err := terminalSessionManager(config.Terminal.SessionManager, deps)
	if err != nil {
		return commandResult{}, failure("terminal_session_manager", "select terminal session manager", err.Error())
	}
	if len(roles) != 0 {
		if err := sessionManager.ValidateRoles(roles); err != nil {
			return commandResult{}, failure("terminal_roles", "validate terminal session roles", err.Error())
		}
	}
	if socket == "" {
		socket = os.Getenv("SWAYSOCK")
	}
	if socket == "" || !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return commandResult{}, failure("sway_socket", "a valid absolute Sway IPC socket is required", "Run inside Sway or pass --socket PATH.")
	}

	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	client := deps.newSwayClient(socket)
	if client == nil {
		return commandResult{}, failure("sway_socket", "open Sway IPC client", "Sway client dependency is unavailable")
	}
	defer client.Close()
	contextClient, ok := client.(sessionstate.ContextSwayRequestClient)
	if !ok {
		return commandResult{}, failure("sway_socket", "open Sway IPC client", "Sway client does not support cancelable requests")
	}
	manager := sessionstate.TerminalManager{
		StateRoot:      root,
		ProcRoot:       "/proc",
		Client:         contextClient,
		NewContextID:   deps.newContextID,
		ResolveProgram: deps.resolveProgram,
		SessionManager: sessionManager,
		FindPending:    deps.findPendingProcess,
		Starter:        deps.processStarter,
		Now:            deps.now,
		Sleep:          deps.sleep,
		SettleTimeout:  deps.settleTimeout,
		StabilityDelay: deps.stabilityDelay,
	}
	opened, err := manager.Open(ctx, sessionstate.TerminalOpenRequest{
		New: *newTerminal, ContextID: contextID, Identity: identity, Adapter: config.Terminal.Adapter, Cwd: cwd, CwdExplicit: cwdExplicit, Label: *label, Focus: true, Roles: roles,
	})
	result := terminalOpenCommandResult(opened)
	if err != nil {
		code := "terminal_open"
		switch {
		case errors.Is(err, sessionstate.ErrTerminalIdentityConflict):
			code = "terminal_identity_conflict"
		case errors.Is(err, sessionstate.ErrTerminalIdentityArchived):
			code = "terminal_identity_archived"
		case errors.Is(err, sessionstate.ErrTerminalAdapterConflict):
			code = "terminal_adapter_conflict"
		case errors.Is(err, sessionstate.ErrTerminalSessionCollision):
			code = "terminal_session_collision"
		}
		return result, failure(code, "open persistent terminal", err.Error())
	}
	actions := make([]string, len(opened.Actions))
	for index, action := range opened.Actions {
		actions[index] = string(action)
	}
	result.Message = fmt.Sprintf("Terminal %s: %s.", opened.Context.ID, strings.Join(actions, ", "))
	if opened.Initialization != nil && !opened.Initialization.Initialized && opened.Initialization.Reason != "" {
		result.Message += " Session layout unchanged: " + opened.Initialization.Reason + "."
	}
	return result, nil
}

func executeTerminalRename(ctx context.Context, arguments []string, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("terminal rename")
	label := set.String("label", "", "human-readable presentation title")
	if err := set.Parse(arguments); err != nil || set.NArg() != 1 || *label == "" {
		return commandResult{}, usageFailure("terminal", "terminal rename requires --label NAME and exactly one context")
	}
	if err := sessionstate.ValidateContextLabel(*label); err != nil {
		return commandResult{}, usageFailure("terminal", "terminal rename label is invalid: "+err.Error())
	}
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	var changed sessionstate.Context
	action := "no_change"
	_, err := sessionstate.UpdateRegistryContext(ctx, root, func(registry *sessionstate.Registry) error {
		var renamed bool
		var mutationErr error
		changed, renamed, mutationErr = sessionstate.RenameTerminalContext(registry, set.Arg(0), *label)
		if renamed {
			action = "renamed"
		}
		return mutationErr
	})
	if err != nil {
		if changed.ID != "" && committedContextContext(ctx, root, changed.ID, func(context sessionstate.Context) bool {
			return contextsEqual(context, changed)
		}, err) {
			return terminalRenameResult(changed, action), nil
		}
		return commandResult{}, classifyStateError("rename terminal", err)
	}
	return terminalRenameResult(changed, action), nil
}

func terminalRenameResult(changed sessionstate.Context, action string) commandResult {
	return commandResult{
		Command: "terminal rename", Contexts: []sessionstate.Context{changed}, Actions: []string{action},
		Message: fmt.Sprintf("Terminal %s renamed to %s.", changed.ID, changed.Label),
	}
}

func executeTerminalReconfigure(ctx context.Context, arguments []string, configPath string, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("terminal reconfigure")
	project := set.String("project", "", "stable project identity")
	socketFlag := set.String("socket", "", "Sway IPC socket")
	if err := set.Parse(arguments); err != nil || set.NArg() != 0 {
		return commandResult{}, usageFailure("terminal", "terminal reconfigure accepts only --project NAME and --socket PATH")
	}
	if deps.loadSessionConfig == nil {
		return commandResult{}, failure("terminal_config", "load terminal configuration", "session config dependency is unavailable")
	}
	config, _, err := deps.loadSessionConfig(configPath)
	if err != nil {
		return commandResult{}, failure("terminal_config", "load terminal configuration", err.Error())
	}
	identity := sessionstate.TerminalIdentity{Kind: sessionstate.TerminalIdentityDefault}
	if *project != "" {
		identity, err = sessionstate.ParseTerminalIdentity(*project)
		if err != nil {
			return commandResult{}, failure("terminal_identity", "validate terminal project identity", err.Error())
		}
	}
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	socket := *socketFlag
	if socket == "" {
		socket = os.Getenv("SWAYSOCK")
	}
	if socket == "" || !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return commandResult{}, failure("sway_socket", "a valid absolute Sway IPC socket is required", "Run inside Sway or pass --socket PATH.")
	}
	client := deps.newSwayClient(socket)
	if client == nil {
		return commandResult{}, failure("sway_socket", "open Sway IPC client", "Sway client dependency is unavailable")
	}
	defer client.Close()
	contextClient, ok := client.(sessionstate.ContextSwayRequestClient)
	if !ok {
		return commandResult{}, failure("sway_socket", "open Sway IPC client", "Sway client does not support cancelable requests")
	}
	reconfigurer := sessionstate.TerminalAdapterReconfigurer{
		StateRoot: root, ProcRoot: "/proc", Client: contextClient,
		FindProcesses: sessionstate.FindTerminalAdapterProcesses,
	}
	changed, reconfigured, err := reconfigurer.Reconfigure(ctx, identity, config.Terminal.Adapter)
	if err != nil {
		if changed.ID != "" && committedContextContext(ctx, root, changed.ID, func(context sessionstate.Context) bool {
			return contextsEqual(context, changed)
		}, err) {
			return terminalReconfigureResult(changed, reconfigured), nil
		}
		switch {
		case errors.Is(err, sessionstate.ErrTerminalAdapterActive):
			return commandResult{}, failure("terminal_adapter_active", "reconfigure terminal adapter", err.Error())
		case errors.Is(err, sessionstate.ErrTerminalAdapterInUse):
			return commandResult{}, failure("terminal_adapter_in_use", "reconfigure terminal adapter", err.Error())
		case errors.Is(err, sessionstate.ErrContextNotFound):
			return commandResult{}, failure("context_not_found", "select terminal identity", err.Error())
		default:
			return commandResult{}, classifyStateError("reconfigure terminal adapter", err)
		}
	}
	return terminalReconfigureResult(changed, reconfigured), nil
}

func terminalReconfigureResult(context sessionstate.Context, changed bool) commandResult {
	action := "no_change"
	message := fmt.Sprintf("Terminal %s already uses adapter %s.", context.ID, context.Launcher.Terminal.Adapter)
	if changed {
		action = "reconfigured"
		message = fmt.Sprintf("Terminal %s will use adapter %s after activation.", context.ID, context.Launcher.Terminal.Adapter)
	}
	items := terminalInventory([]sessionstate.Context{context})
	return commandResult{
		Command: "terminal reconfigure", Contexts: []sessionstate.Context{context}, Terminals: &items,
		Actions: []string{action}, Message: message,
	}
}

func terminalOpenCommandResult(opened sessionstate.TerminalOpenResult) commandResult {
	result := commandResult{Command: "terminal"}
	if opened.Context.ID == "" || opened.Context.Launcher.Terminal == nil {
		return result
	}
	result.Contexts = []sessionstate.Context{opened.Context}
	identity := terminalResultIdentity(opened.Context)
	result.Terminal = &terminalCommandResult{
		ContextID:      opened.Context.ID,
		Identity:       identity,
		Adapter:        opened.Context.Launcher.Terminal.Adapter,
		Session:        opened.Context.Launcher.Session,
		Actions:        opened.Actions,
		Manager:        opened.Manager,
		Initialization: opened.Initialization,
	}
	return result
}

func terminalResultIdentity(context sessionstate.Context) *terminalIdentityResult {
	if context.Launcher.Terminal == nil {
		return nil
	}
	if context.Launcher.Terminal.Identity != nil {
		return &terminalIdentityResult{
			Kind:    context.Launcher.Terminal.Identity.Kind,
			Project: context.Launcher.Terminal.Identity.Project,
		}
	}
	if sessionstate.IsTerminalInstanceContext(context) {
		return &terminalIdentityResult{
			Kind:      sessionstate.TerminalIdentityKind("instance"),
			ContextID: context.ID,
		}
	}
	return nil
}

func terminalWorkingDirectory(value string, project string, deps dependencies) (string, error) {
	if value != "" {
		return filepath.Abs(value)
	}
	if project != "" {
		if deps.workingDir == nil {
			return "", errors.New("working directory dependency is unavailable")
		}
		return deps.workingDir()
	}
	if deps.homeDir == nil {
		return "", errors.New("home directory dependency is unavailable")
	}
	return deps.homeDir()
}

func executeTerminalList(ctx context.Context, arguments []string, deps dependencies) (commandResult, *commandFailure) {
	if len(arguments) != 0 {
		return commandResult{}, usageFailure("terminal", "terminal list accepts no arguments")
	}
	snapshot, commandFailure := loadTerminalInventory(ctx, deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	items := terminalInventory(snapshot.Registry.Contexts, snapshot.Activity)
	return commandResult{Command: "terminal list", Terminals: &items}, nil
}

func executeTerminalStatus(ctx context.Context, arguments []string, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("terminal status")
	project := set.String("project", "", "stable project identity")
	if err := set.Parse(arguments); err != nil || set.NArg() > 1 || (*project != "" && set.NArg() != 0) {
		return commandResult{}, usageFailure("terminal", "terminal status accepts one context UUID or exact label, or --project NAME")
	}
	snapshot, commandFailure := loadTerminalInventory(ctx, deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	registry := snapshot.Registry
	var selected sessionstate.Context
	if *project != "" {
		identity, err := sessionstate.ParseTerminalIdentity(*project)
		if err != nil {
			return commandResult{}, failure("terminal_identity", "validate terminal project identity", err.Error())
		}
		found := false
		for _, context := range registry.Contexts {
			if context.Launcher.Terminal == nil || context.Launcher.Terminal.Identity == nil || *context.Launcher.Terminal.Identity != identity {
				continue
			}
			selected = context
			found = true
			break
		}
		if !found {
			return commandResult{}, failure("context_not_found", "select terminal project identity", "No terminal context uses project identity "+*project+".")
		}
	} else if set.NArg() == 1 {
		index, err := sessionstate.ResolveContext(registry, set.Arg(0))
		if err != nil {
			return commandResult{}, classifyStateError("select terminal context", err)
		}
		selected = registry.Contexts[index]
	} else {
		found := false
		for _, context := range registry.Contexts {
			if context.Launcher.Terminal == nil || context.Launcher.Terminal.Identity == nil ||
				context.Launcher.Terminal.Identity.Kind != sessionstate.TerminalIdentityDefault {
				continue
			}
			selected = context
			found = true
			break
		}
		if !found {
			return commandResult{}, failure("context_not_found", "select default terminal context", "Run sway-session terminal to create it.")
		}
	}
	items := terminalInventory([]sessionstate.Context{selected}, snapshot.Activity)
	if len(items) != 1 {
		return commandResult{}, failure("terminal_context", "selected context is not a Herdr terminal", string(selected.ID))
	}
	return commandResult{Command: "terminal status", Terminals: &items}, nil
}

func executeTerminalCleanup(ctx context.Context, arguments []string, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("terminal cleanup")
	beforeValue := set.String("archived-before", "", "UTC date YYYY-MM-DD")
	if err := set.Parse(arguments); err != nil || set.NArg() != 0 {
		return commandResult{}, usageFailure("terminal", "terminal cleanup accepts only --archived-before YYYY-MM-DD")
	}
	var cutoff time.Time
	if *beforeValue != "" {
		var err error
		cutoff, err = time.Parse("2006-01-02", *beforeValue)
		if err != nil {
			return commandResult{}, usageFailure("terminal", "--archived-before must be an exact UTC date in YYYY-MM-DD form")
		}
	}
	snapshot, commandFailure := loadTerminalInventory(ctx, deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	registry := snapshot.Registry
	candidates := make([]sessionstate.Context, 0)
	for _, context := range registry.Contexts {
		if context.State != sessionstate.ContextArchived || context.Launcher.Kind != sessionstate.LauncherHerdr || context.Launcher.Terminal == nil {
			continue
		}
		if *beforeValue != "" && (context.ArchivedAt == nil || !context.ArchivedAt.Before(cutoff)) {
			continue
		}
		candidates = append(candidates, context)
	}
	items := terminalInventory(candidates, snapshot.Activity)
	return commandResult{
		Command: "terminal cleanup", Terminals: &items, Preview: true, Actions: []string{"preview"},
		Message: "Preview only; use sway-session purge --yes <context-uuid> after reviewing each candidate.",
	}, nil
}

func loadTerminalInventory(ctx context.Context, deps dependencies) (sessionstate.TerminalInventorySnapshot, *commandFailure) {
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return sessionstate.TerminalInventorySnapshot{}, commandFailure
	}
	snapshot, err := sessionstate.ReadTerminalInventorySnapshotContext(ctx, root)
	if err != nil {
		return sessionstate.TerminalInventorySnapshot{}, classifyStateError("load terminal inventory", err)
	}
	return snapshot, nil
}

func terminalInventory(contexts []sessionstate.Context, activityState ...sessionstate.TerminalActivityState) []terminalInventoryResult {
	items := make([]terminalInventoryResult, 0, len(contexts))
	activityByContext := make(map[sessionstate.ContextID]sessionstate.TerminalActivity)
	if len(activityState) != 0 {
		activityByContext = make(map[sessionstate.ContextID]sessionstate.TerminalActivity, len(activityState[0].Terminals))
		for _, activity := range activityState[0].Terminals {
			activityByContext[activity.ContextID] = activity
		}
	}
	for _, context := range contexts {
		if context.Launcher.Kind != sessionstate.LauncherHerdr || context.Launcher.Terminal == nil {
			continue
		}
		identity := terminalIdentityResult{Kind: sessionstate.TerminalIdentityKind("manual")}
		if context.Launcher.Terminal.Identity != nil {
			identity = terminalIdentityResult{
				Kind: context.Launcher.Terminal.Identity.Kind, Project: context.Launcher.Terminal.Identity.Project,
			}
		} else if sessionstate.IsTerminalInstanceContext(context) {
			identity = terminalIdentityResult{Kind: sessionstate.TerminalIdentityKind("instance"), ContextID: context.ID}
		}
		activity := activityByContext[context.ID]
		items = append(items, terminalInventoryResult{
			ContextID:     context.ID,
			Label:         context.Label,
			Identity:      identity,
			Adapter:       context.Launcher.Terminal.Adapter,
			Manager:       sessionstate.TerminalSessionManagerHerdr,
			State:         context.State,
			Session:       context.Launcher.Session,
			Cwd:           context.Launcher.Cwd,
			CreatedAt:     activity.CreatedAt,
			LastFocusedAt: activity.LastFocusedAt,
			ArchivedAt:    context.ArchivedAt,
		})
	}
	sort.Slice(items, func(left int, right int) bool { return items[left].ContextID < items[right].ContextID })
	return items
}
