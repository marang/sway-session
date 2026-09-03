package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/marang/sway-title-animator/internal/codexreport"
	"github.com/marang/sway-title-animator/internal/diagnostic"
	"github.com/marang/sway-title-animator/internal/herdrinit"
	sessionstate "github.com/marang/sway-title-animator/internal/session"
	"github.com/marang/sway-title-animator/internal/sessionrequest"
	"github.com/marang/sway-title-animator/internal/swayipc"
	"golang.org/x/term"
)

const (
	exitSuccess   = 0
	exitUsage     = 2
	exitOperation = 3

	commandResultVersion = 1
)

type commandSpec struct {
	usage   string
	summary string
}

var commandSpecs = map[string]commandSpec{
	"register":             {usage: "register --session <name> [options]", summary: "Register a persistent work context"},
	"restore":              {usage: "restore [--socket <path>] [context]", summary: "Restore active or selected contexts"},
	"list":                 {usage: "list", summary: "List registered contexts"},
	"archive":              {usage: "archive <context>", summary: "Exclude a context from automatic restore"},
	"activate":             {usage: "activate <context>", summary: "Return an archived context to automatic restore"},
	"purge":                {usage: "purge [--yes] <context>", summary: "Permanently remove a Herdr terminal context and its saved session"},
	"broker":               {usage: "broker [--socket <path>]", summary: "Serve typed work-session start requests"},
	"daemon":               {usage: "daemon [--socket <path>]", summary: "Observe and restore persistent Sway session state"},
	"request-start":        {usage: "request-start --session <name> --workspace <number> [options]", summary: "Request a typed ensure-and-start operation"},
	"report-codex-session": {usage: "report-codex-session", summary: "Report a managed Codex SessionStart event to the narrow broker"},
	"app":                  {usage: "app <subcommand> [options]", summary: "Manage explicitly registered desktop applications"},
	"terminal":             {usage: "terminal [--new | --context <uuid> | --project <name> | --ephemeral] [options]", summary: "Open a typed terminal"},
	"completion":           {usage: "completion contexts <command>", summary: "Emit read-only shell completion candidates"},
}

var commandOrder = []string{"terminal", "register", "restore", "list", "archive", "activate", "purge", "app", "daemon", "broker", "request-start", "report-codex-session", "completion"}

type swayRequester interface {
	RequestContext(context.Context, swayipc.MessageType, []byte) (swayipc.Message, error)
	Close()
}

type dependencies struct {
	stateRoot          func() (string, error)
	workingDir         func() (string, error)
	homeDir            func() (string, error)
	newContextID       func() (sessionstate.ContextID, error)
	loadSessionConfig  func(string) (sessionstate.SessionConfig, string, error)
	herdrPaths         func() (sessionstate.HerdrPaths, error)
	validateHistory    func(sessionstate.HerdrPaths) error
	resolveProgram     func(string) (string, error)
	resolveSystem      func(string) (string, error)
	desktopCatalog     func() (sessionstate.DesktopCatalog, error)
	operationStore     func() (sessionstate.ApplicationOperationStore, error)
	presentApproval    func(string, []sessionstate.ApprovalChoice) error
	verifyFlatpak      func(sessionstate.Launcher) error
	newSwayClient      func(string) swayRequester
	processStarter     sessionstate.ProcessStarter
	herdrRunner        sessionstate.HerdrCommandRunner
	initializeHerdr    func(context.Context, sessionstate.Context, []string, herdrinit.Runner) (herdrinit.Result, error)
	findPendingProcess func(string, sessionstate.ProcessSpec) ([]int, error)
	now                func() time.Time
	sleep              func(time.Duration)
	settleTimeout      time.Duration
	stabilityDelay     time.Duration
	stdinTerminal      func() bool
	reportCodexHook    func(context.Context, io.Reader, func(string) string) error
	requestStart       func(context.Context, sessionrequest.Request) (sessionrequest.Response, error)
	runBroker          func(context.Context, string, func(error)) error
	runDaemon          func(context.Context, string, func(error)) error
	runTerminalManage  terminalManageRunner
}

func defaultDependencies(stdin io.Reader) dependencies {
	deps := dependencies{
		stateRoot:         sessionstate.DefaultStateRoot,
		workingDir:        os.Getwd,
		homeDir:           os.UserHomeDir,
		newContextID:      sessionstate.NewContextID,
		loadSessionConfig: sessionstate.LoadSessionConfig,
		herdrPaths:        sessionstate.DefaultHerdrPaths,
		validateHistory:   sessionstate.ValidateHerdrPaneHistory,
		resolveProgram:    sessionstate.ResolveTrustedExecutable,
		resolveSystem:     sessionstate.ResolveRootOwnedSystemExecutable,
		desktopCatalog: func() (sessionstate.DesktopCatalog, error) {
			search, err := sessionstate.DefaultDesktopSearchPath()
			if err != nil {
				return sessionstate.DesktopCatalog{}, err
			}
			return sessionstate.LoadDesktopCatalog(search)
		},
		operationStore: sessionstate.DefaultApplicationOperationStore,
		newSwayClient: func(socket string) swayRequester {
			return swayipc.NewClient(socket)
		},
		processStarter:     sessionstate.ExecProcessStarter{},
		herdrRunner:        sessionstate.ExecCommandRunner{},
		initializeHerdr:    herdrinit.Initialize,
		findPendingProcess: sessionstate.FindPendingProcessLaunches,
		now:                time.Now,
		sleep:              time.Sleep,
		settleTimeout:      10 * time.Second,
		stabilityDelay:     time.Second,
		stdinTerminal: func() bool {
			file, ok := stdin.(*os.File)
			return ok && term.IsTerminal(int(file.Fd()))
		},
		reportCodexHook: codexreport.ReportCodexHook,
		requestStart: func(ctx context.Context, request sessionrequest.Request) (sessionrequest.Response, error) {
			socketPath, err := sessionrequest.DefaultSocketPath()
			if err != nil {
				return sessionrequest.Response{}, err
			}
			return sessionrequest.Send(ctx, socketPath, request)
		},
		runBroker:         runSessionRequestBroker,
		runDaemon:         runSessionDaemon,
		runTerminalManage: runTerminalManager,
	}
	deps.presentApproval = func(message string, choices []sessionstate.ApprovalChoice) error {
		swaynag, err := deps.resolveSystem("swaynag")
		if err != nil {
			return err
		}
		swaySession, err := deps.resolveSystem("sway-session")
		if err != nil {
			return err
		}
		return (sessionstate.SwaynagApprovalPresenter{
			Swaynag: swaynag, SwaySession: swaySession, Starter: deps.processStarter,
		}).Present(message, choices)
	}
	deps.verifyFlatpak = func(launcher sessionstate.Launcher) error {
		flatpak, err := deps.resolveSystem("flatpak")
		if err != nil {
			return err
		}
		return sessionstate.VerifyFlatpakInstallation(flatpak, launcher, sessionstate.ExecCommandRunner{})
	}
	return deps
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(runWithContext(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, defaultDependencies(os.Stdin)))
}

func run(arguments []string, stdout io.Writer, stderr io.Writer) int {
	return runWith(arguments, os.Stdin, stdout, stderr, defaultDependencies(os.Stdin))
}

func runWith(arguments []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, deps dependencies) int {
	return runWithContext(context.Background(), arguments, stdin, stdout, stderr, deps)
}

func runWithContext(ctx context.Context, arguments []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, deps dependencies) int {
	arguments, structured, help, configPath, optionFailure := globalOptions(arguments)
	if optionFailure != nil {
		writeFailure(stderr, structured, optionFailure)
		return exitUsage
	}
	if len(arguments) == 0 {
		if help {
			writeUsage(stdout)
			return exitSuccess
		}
		writeUsage(stderr)
		writeFailure(stderr, structured, failure("usage", "a command is required", "Run sway-session --help to see available commands."))
		return exitUsage
	}

	if arguments[0] == "help" {
		if len(arguments) == 1 {
			writeUsage(stdout)
			return exitSuccess
		}
		if len(arguments) == 2 {
			return writeCommandHelp(arguments[1], stdout, stderr, structured)
		}
		writeFailure(stderr, structured, failure("usage", "help accepts at most one command", "Run sway-session --help to see available commands."))
		return exitUsage
	}

	name := arguments[0]
	spec, exists := commandSpecs[name]
	if !exists {
		writeFailure(stderr, structured, failure("unknown_command", fmt.Sprintf("unknown command %q", name), "Run sway-session --help to see available commands."))
		return exitUsage
	}
	if help {
		writeCommandUsageForArguments(stdout, name, spec, arguments[1:])
		return exitSuccess
	}

	result, commandFailure := executeCommand(ctx, name, arguments[1:], stdin, stdout, stderr, structured, configPath, deps)
	if commandFailure != nil {
		if len(result.Contexts) != 0 || result.Message != "" {
			if err := writeResult(stdout, structured, result); err != nil {
				commandFailure.diagnostics = append(commandFailure.diagnostics, diagnostic.Diagnostic{
					Level: diagnostic.LevelError, Code: "output", Message: "write partial command result", Hint: err.Error(),
				})
			}
		}
		writeFailure(stderr, structured, commandFailure)
		if commandFailure.usage {
			return exitUsage
		}
		return exitOperation
	}
	if err := writeResult(stdout, structured, result); err != nil {
		writeFailure(stderr, structured, failure("output", "write command result", err.Error()))
		return exitOperation
	}
	return exitSuccess
}

type commandResult struct {
	Version              int                        `json:"version"`
	Command              string                     `json:"command"`
	Contexts             []sessionstate.Context     `json:"contexts"`
	CompletionCandidates []completionCandidate      `json:"completion_candidates,omitempty"`
	Message              string                     `json:"message,omitempty"`
	Workspace            int                        `json:"workspace,omitempty"`
	Created              bool                       `json:"created,omitempty"`
	Actions              []string                   `json:"actions,omitempty"`
	Terminal             *terminalCommandResult     `json:"terminal,omitempty"`
	Terminals            *[]terminalInventoryResult `json:"terminals,omitempty"`
	Preview              bool                       `json:"preview,omitempty"`
}

type terminalCommandResult struct {
	ContextID      sessionstate.ContextID                      `json:"context_id,omitempty"`
	Identity       *terminalIdentityResult                     `json:"identity,omitempty"`
	Adapter        sessionstate.TerminalAdapter                `json:"adapter"`
	Session        string                                      `json:"session,omitempty"`
	Actions        []sessionstate.TerminalOpenAction           `json:"actions"`
	Manager        sessionstate.TerminalSessionManagerKind     `json:"session_manager,omitempty"`
	Initialization *sessionstate.TerminalSessionInitialization `json:"initialization,omitempty"`
	Ephemeral      bool                                        `json:"ephemeral,omitempty"`
}

type terminalIdentityResult struct {
	Kind      sessionstate.TerminalIdentityKind `json:"kind"`
	Project   string                            `json:"project,omitempty"`
	ContextID sessionstate.ContextID            `json:"context_id,omitempty"`
}

type terminalInventoryResult struct {
	ContextID     sessionstate.ContextID                  `json:"context_id"`
	Label         string                                  `json:"label,omitempty"`
	Identity      terminalIdentityResult                  `json:"identity"`
	Adapter       sessionstate.TerminalAdapter            `json:"adapter"`
	Manager       sessionstate.TerminalSessionManagerKind `json:"session_manager"`
	State         sessionstate.ContextState               `json:"state"`
	Session       string                                  `json:"session"`
	Cwd           string                                  `json:"cwd"`
	CreatedAt     *time.Time                              `json:"created_at,omitempty"`
	LastFocusedAt *time.Time                              `json:"last_focused_at,omitempty"`
	ArchivedAt    *time.Time                              `json:"archived_at,omitempty"`
}

type commandFailure struct {
	diagnostics []diagnostic.Diagnostic
	usage       bool
}

func failure(code string, message string, hint string) *commandFailure {
	return &commandFailure{diagnostics: []diagnostic.Diagnostic{{
		Level: diagnostic.LevelError, Code: code, Message: message, Hint: hint,
	}}}
}

func failures(items []diagnostic.Diagnostic) *commandFailure {
	return &commandFailure{diagnostics: items}
}

func usageFailure(name string, detail string) *commandFailure {
	return &commandFailure{
		usage: true,
		diagnostics: []diagnostic.Diagnostic{{
			Level: diagnostic.LevelError,
			Code:  "usage", Message: detail, Hint: "Usage: sway-session " + commandSpecs[name].usage,
		}},
	}
}

func writeFailure(writer io.Writer, structured bool, item *commandFailure) {
	_ = diagnostic.WriteAll(writer, "sway-session", item.diagnostics, structured)
}

func writeResult(writer io.Writer, structured bool, result commandResult) error {
	if result.Version == 0 {
		result.Version = commandResultVersion
	}
	if structured {
		if result.Contexts == nil {
			result.Contexts = []sessionstate.Context{}
		}
		return json.NewEncoder(writer).Encode(result)
	}
	if len(result.CompletionCandidates) != 0 {
		for _, candidate := range result.CompletionCandidates {
			if _, err := fmt.Fprintf(writer, "%s\t%s\n", candidate.Value, candidate.Description); err != nil {
				return err
			}
		}
		return nil
	}
	if result.Terminals != nil {
		for _, terminal := range *result.Terminals {
			identity := string(terminal.Identity.Kind)
			if terminal.Identity.Project != "" {
				identity += ":" + terminal.Identity.Project
			}
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", terminal.ContextID, identity, terminal.Adapter, terminal.State, terminal.Cwd); err != nil {
				return err
			}
		}
		if result.Message != "" {
			_, err := fmt.Fprintln(writer, result.Message)
			return err
		}
		return nil
	}
	if result.Message != "" {
		_, err := fmt.Fprintln(writer, result.Message)
		return err
	}
	for _, context := range result.Contexts {
		label := context.Label
		if label == "" {
			label = context.Launcher.Session
		}
		provider := context.Provider
		if provider == "" {
			provider = "-"
		}
		launcherName, launcherDetail := launcherOutput(context.Launcher)
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", label, context.ID, context.State, provider, launcherName, launcherDetail); err != nil {
			return err
		}
	}
	return nil
}

func launcherOutput(launcher sessionstate.Launcher) (string, string) {
	switch launcher.Kind {
	case sessionstate.LauncherHerdr:
		return launcher.Session, launcher.Cwd
	case sessionstate.LauncherDesktop:
		return launcher.DesktopID, launcher.DesktopPath
	case sessionstate.LauncherFlatpak:
		return launcher.FlatpakID, string(launcher.FlatpakInstallation)
	default:
		return string(launcher.Kind), ""
	}
}

func globalOptions(arguments []string) ([]string, bool, bool, string, *commandFailure) {
	filtered := make([]string, 0, len(arguments))
	structured := false
	help := false
	configPath := ""
	optionsEnded := false
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if optionsEnded {
			filtered = append(filtered, argument)
			continue
		}
		if argument == "--" {
			optionsEnded = true
			filtered = append(filtered, argument)
			continue
		}
		switch argument {
		case "--json":
			structured = true
		case "--help", "-h":
			help = true
		case "--config":
			if configPath != "" {
				return filtered, structured, help, configPath, usageFailure("terminal", "--config may be provided only once")
			}
			index++
			if index >= len(arguments) {
				return filtered, structured, help, configPath, usageFailure("terminal", "--config requires an absolute path")
			}
			configPath = arguments[index]
		default:
			if strings.HasPrefix(argument, "--config=") {
				if configPath != "" {
					return filtered, structured, help, configPath, usageFailure("terminal", "--config may be provided only once")
				}
				configPath = strings.TrimPrefix(argument, "--config=")
				if configPath == "" {
					return filtered, structured, help, configPath, usageFailure("terminal", "--config requires an absolute path")
				}
			} else {
				filtered = append(filtered, argument)
			}
		}
	}
	return filtered, structured, help, configPath, nil
}

func newFlagSet(name string) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	return set
}

func writeUsage(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "Usage: sway-session [--json] <command> [options]")
	_, _ = fmt.Fprintln(writer)
	_, _ = fmt.Fprintln(writer, "Persist explicitly registered Sway work contexts.")
	_, _ = fmt.Fprintln(writer)
	_, _ = fmt.Fprintln(writer, "Commands:")
	for _, name := range commandOrder {
		spec := commandSpecs[name]
		_, _ = fmt.Fprintf(writer, "  %-42s %s\n", spec.usage, spec.summary)
	}
	_, _ = fmt.Fprintln(writer)
	_, _ = fmt.Fprintln(writer, "Options:")
	_, _ = fmt.Fprintln(writer, "  --json               Emit machine-readable results and diagnostics")
	_, _ = fmt.Fprintln(writer, "  --config PATH        Use an explicit typed sway-session configuration")
	_, _ = fmt.Fprintln(writer, "  -h, --help           Show help")
	_, _ = fmt.Fprintln(writer)
	_, _ = fmt.Fprintln(writer, "Exit status:")
	_, _ = fmt.Fprintln(writer, "  0  Successful operation")
	_, _ = fmt.Fprintln(writer, "  2  Invalid command or arguments")
	_, _ = fmt.Fprintln(writer, "  3  Operational failure")
}

func writeCommandHelp(name string, stdout io.Writer, stderr io.Writer, structured bool) int {
	spec, exists := commandSpecs[name]
	if !exists {
		writeFailure(stderr, structured, failure("unknown_command", fmt.Sprintf("unknown command %q", name), "Run sway-session --help to see available commands."))
		return exitUsage
	}
	writeCommandUsage(stdout, name, spec)
	return exitSuccess
}

func writeCommandUsage(writer io.Writer, name string, spec commandSpec) {
	_, _ = fmt.Fprintf(writer, "Usage: sway-session [--json] %s\n\n%s.\n", spec.usage, spec.summary)
	if slices.Contains([]string{"archive", "activate", "purge", "restore"}, name) {
		_, _ = fmt.Fprintln(writer, "A context is an unambiguous exact UUID or label.")
	}
	if name == "purge" {
		_, _ = fmt.Fprintln(writer, "Desktop and Flatpak registrations must use sway-session app forget --yes <context> so live marks and launcher approval are removed transactionally.")
	}
	if name == "register" {
		_, _ = fmt.Fprintln(writer, "Options: --session NAME [--cwd PATH] [--label LABEL] [--provider NAME] [--id UUID]")
	}
	if name == "terminal" {
		_, _ = fmt.Fprintln(writer, "Options: [--new | --context UUID | --project NAME | --ephemeral] [--cwd PATH] [--label LABEL] [--socket PATH] [--role LEFT --role RIGHT]")
		_, _ = fmt.Fprintln(writer, "Subcommands: manage [--socket PATH], list, status, cleanup, rename --label NAME <context>, reconfigure [--project NAME] [--socket PATH]")
	}
	if name == "request-start" {
		_, _ = fmt.Fprintln(writer, "Options: --session NAME --workspace NUMBER [--cwd PATH] [--label LABEL] [--provider NAME]")
	}
	if name == "app" {
		_, _ = fmt.Fprintln(writer, "Subcommands: register-focused, register-workspace, confirm, status, list, rebind-focused, reapprove, pin, unpin, archive, activate, forget")
		_, _ = fmt.Fprintln(writer, "Machine-readable inventory: sway-session --json app list")
		_, _ = fmt.Fprintln(writer, "Indicators after first registration: ○ unregistered, ◔ pending, ● registered/follow, ▲ pinned/autostart")
	}
	if name == "completion" {
		_, _ = fmt.Fprintln(writer, "Commands: archive, activate, restore, restore-active, purge, terminal-status, app-forget")
		_, _ = fmt.Fprintln(writer, "Output: one canonical UUID and presentation-only description per tab-separated line.")
	}
}

func writeCommandUsageForArguments(writer io.Writer, name string, spec commandSpec, arguments []string) {
	if name != "terminal" || len(arguments) == 0 {
		writeCommandUsage(writer, name, spec)
		return
	}
	var usage string
	var summary string
	switch arguments[0] {
	case "list":
		usage = "terminal list"
		summary = "List typed terminal contexts without changing state"
	case "status":
		usage = "terminal status [context] [--project NAME]"
		summary = "Show one typed terminal context without changing state"
	case "cleanup":
		usage = "terminal cleanup [--archived-before YYYY-MM-DD]"
		summary = "Preview archived typed terminal cleanup candidates"
	case "manage":
		usage = "terminal manage [--socket PATH]"
		summary = "Interactively open, rename, archive, activate, or purge persistent terminals"
	case "reconfigure":
		usage = "[--config PATH] terminal reconfigure [--project NAME] [--socket PATH]"
		summary = "Change the closed adapter of one archived and stopped terminal identity"
	case "rename":
		usage = "terminal rename --label NAME <context>"
		summary = "Change a terminal's presentation title without changing its identity"
	default:
		writeCommandUsage(writer, name, spec)
		return
	}
	globalOptions := "[--json] "
	if arguments[0] == "manage" {
		globalOptions = ""
	}
	_, _ = fmt.Fprintf(writer, "Usage: sway-session %s%s\n\n%s.\n", globalOptions, usage, summary)
}

func executeCommand(ctx context.Context, name string, arguments []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, structured bool, configPath string, deps dependencies) (commandResult, *commandFailure) {
	if configPath != "" && name != "terminal" {
		return commandResult{}, usageFailure(name, "--config is only valid with the terminal command")
	}
	switch name {
	case "terminal":
		return executeTerminal(ctx, arguments, stdin, stdout, structured, configPath, deps)
	case "register":
		return executeRegister(ctx, arguments, deps)
	case "list":
		return executeList(ctx, arguments, deps)
	case "archive":
		return executeStateChange(ctx, name, arguments, sessionstate.ContextArchived, deps)
	case "activate":
		return executeStateChange(ctx, name, arguments, sessionstate.ContextActive, deps)
	case "purge":
		return executePurge(ctx, arguments, stdin, stderr, structured, deps)
	case "restore":
		return executeRestore(ctx, arguments, deps)
	case "app":
		return executeApp(ctx, arguments, deps)
	case "broker":
		return executeBroker(ctx, arguments, stderr, structured, deps)
	case "daemon":
		return executeDaemon(ctx, arguments, stderr, structured, deps)
	case "request-start":
		return executeRequestStart(ctx, arguments, deps)
	case "report-codex-session":
		if len(arguments) != 0 {
			return commandResult{}, usageFailure(name, "report-codex-session accepts no arguments")
		}
		if deps.reportCodexHook == nil {
			return commandResult{}, failure("codex_report", "report Codex session", "Codex report dependency is unavailable")
		}
		err := deps.reportCodexHook(ctx, stdin, os.Getenv)
		if errors.Is(err, codexreport.ErrNotManagedSession) {
			return commandResult{Command: name, Contexts: []sessionstate.Context{}}, nil
		}
		if err != nil {
			return commandResult{}, failure("codex_report", "report Codex session", err.Error())
		}
		return commandResult{Command: name, Contexts: []sessionstate.Context{}}, nil
	case "completion":
		return executeCompletion(ctx, arguments, deps)
	default:
		return commandResult{}, failure("unknown_command", fmt.Sprintf("unknown command %q", name), "")
	}
}

func stateRoot(deps dependencies) (string, *commandFailure) {
	root, err := deps.stateRoot()
	if err != nil {
		return "", failure("state_path", "resolve state directory", err.Error())
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", failure("state_path", "state directory is not a clean absolute path", root)
	}
	return root, nil
}

func classifyStateError(action string, err error) *commandFailure {
	code := "state"
	var unsupported *sessionstate.UnsupportedVersionError
	if errors.As(err, &unsupported) {
		code = "unsupported_version"
	} else if errors.Is(err, sessionstate.ErrContextNotFound) {
		code = "context_not_found"
	} else if errors.Is(err, sessionstate.ErrContextAmbiguous) {
		code = "context_ambiguous"
	}
	return failure(code, action, err.Error())
}

func diagnosticForContext(code string, context sessionstate.Context, err error, hint string) diagnostic.Diagnostic {
	name := context.Label
	if name == "" {
		name = string(context.ID)
	}
	return diagnostic.Diagnostic{
		Level: diagnostic.LevelError,
		Code:  code, Message: fmt.Sprintf("context %s: %v", name, err), Hint: strings.TrimSpace(hint),
	}
}
