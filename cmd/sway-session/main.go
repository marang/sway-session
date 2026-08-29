package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/marang/sway-title-animator/internal/codexreport"
	"github.com/marang/sway-title-animator/internal/diagnostic"
	sessionstate "github.com/marang/sway-title-animator/internal/session"
	"github.com/marang/sway-title-animator/internal/swayipc"
	"golang.org/x/term"
)

const (
	exitSuccess   = 0
	exitUsage     = 2
	exitOperation = 3
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
	"purge":                {usage: "purge [--yes] <context>", summary: "Permanently remove a context and its saved Herdr state"},
	"report-codex-session": {usage: "report-codex-session", summary: "Report a managed Codex SessionStart event to the narrow broker"},
}

var commandOrder = []string{"register", "restore", "list", "archive", "activate", "purge", "report-codex-session"}

type swayRequester interface {
	Request(swayipc.MessageType, []byte) (swayipc.Message, error)
	Close()
}

type dependencies struct {
	stateRoot       func() (string, error)
	workingDir      func() (string, error)
	newContextID    func() (sessionstate.ContextID, error)
	herdrPaths      func() (sessionstate.HerdrPaths, error)
	validateHistory func(sessionstate.HerdrPaths) error
	resolveProgram  func(string) (string, error)
	newSwayClient   func(string) swayRequester
	processStarter  sessionstate.ProcessStarter
	herdrRunner     sessionstate.HerdrCommandRunner
	findPending     func(string, sessionstate.Context, string, string) ([]int, error)
	now             func() time.Time
	sleep           func(time.Duration)
	settleTimeout   time.Duration
	stdinTerminal   func() bool
	reportCodexHook func(context.Context, io.Reader, func(string) string) error
}

func defaultDependencies(stdin io.Reader) dependencies {
	return dependencies{
		stateRoot:       sessionstate.DefaultStateRoot,
		workingDir:      os.Getwd,
		newContextID:    sessionstate.NewContextID,
		herdrPaths:      sessionstate.DefaultHerdrPaths,
		validateHistory: sessionstate.ValidateHerdrPaneHistory,
		resolveProgram:  sessionstate.ResolveTrustedExecutable,
		newSwayClient: func(socket string) swayRequester {
			return swayipc.NewClient(socket)
		},
		processStarter: sessionstate.ExecProcessStarter{},
		herdrRunner:    sessionstate.ExecCommandRunner{},
		findPending:    sessionstate.FindPendingAlacrittyLaunches,
		now:            time.Now,
		sleep:          time.Sleep,
		settleTimeout:  10 * time.Second,
		stdinTerminal: func() bool {
			file, ok := stdin.(*os.File)
			return ok && term.IsTerminal(int(file.Fd()))
		},
		reportCodexHook: codexreport.ReportCodexHook,
	}
}

func main() {
	os.Exit(runWith(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, defaultDependencies(os.Stdin)))
}

func run(arguments []string, stdout io.Writer, stderr io.Writer) int {
	return runWith(arguments, os.Stdin, stdout, stderr, defaultDependencies(os.Stdin))
}

func runWith(arguments []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, deps dependencies) int {
	arguments, structured, help := globalOptions(arguments)
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
		writeCommandUsage(stdout, name, spec)
		return exitSuccess
	}

	result, commandFailure := executeCommand(context.Background(), name, arguments[1:], stdin, stderr, deps)
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
	Command  string                 `json:"command"`
	Contexts []sessionstate.Context `json:"contexts"`
	Message  string                 `json:"message,omitempty"`
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
	if structured {
		return json.NewEncoder(writer).Encode(result)
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
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", label, context.ID, context.State, provider, context.Launcher.Session, context.Launcher.Cwd); err != nil {
			return err
		}
	}
	return nil
}

func globalOptions(arguments []string) ([]string, bool, bool) {
	filtered := make([]string, 0, len(arguments))
	structured := false
	help := false
	optionsEnded := false
	for _, argument := range arguments {
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
		default:
			filtered = append(filtered, argument)
		}
	}
	return filtered, structured, help
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
	if name == "register" {
		_, _ = fmt.Fprintln(writer, "Options: --session NAME [--cwd PATH] [--label LABEL] [--provider NAME] [--id UUID]")
	}
}

func executeCommand(ctx context.Context, name string, arguments []string, stdin io.Reader, stderr io.Writer, deps dependencies) (commandResult, *commandFailure) {
	switch name {
	case "register":
		return executeRegister(arguments, deps)
	case "list":
		return executeList(arguments, deps)
	case "archive":
		return executeStateChange(name, arguments, sessionstate.ContextArchived, deps)
	case "activate":
		return executeStateChange(name, arguments, sessionstate.ContextActive, deps)
	case "purge":
		return executePurge(ctx, arguments, stdin, stderr, deps)
	case "restore":
		return executeRestore(ctx, arguments, deps)
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
	if errors.Is(err, sessionstate.ErrContextNotFound) {
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
