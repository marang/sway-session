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
	"syscall"
	"time"

	"github.com/marang/sway-title-animator/internal/herdrinit"
	sessionstate "github.com/marang/sway-title-animator/internal/session"
)

const operationTimeout = 90 * time.Second

type roleFlags []string

func (values *roleFlags) String() string { return fmt.Sprint([]string(*values)) }
func (values *roleFlags) Set(value string) error {
	*values = append(*values, value)
	return nil
}

type dependencies struct {
	userPaths          func() (herdrinit.UserPaths, error)
	acquireContextLock func(string, sessionstate.ContextID) (io.Closer, error)
	resolveExecutable  func(string) (string, error)
	inspectContext     func(string, sessionstate.ContextID, func(sessionstate.Context) error) error
	initialize         func(context.Context, sessionstate.Context, []string, herdrinit.Runner) (herdrinit.Result, error)
}

func defaultDependencies() dependencies {
	return dependencies{
		userPaths: herdrinit.CurrentUserPaths,
		acquireContextLock: func(root string, id sessionstate.ContextID) (io.Closer, error) {
			return herdrinit.AcquireContextLock(root, id)
		},
		resolveExecutable: herdrinit.ResolveSystemExecutable,
		inspectContext:    herdrinit.InspectActiveContext,
		initialize:        herdrinit.Initialize,
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(runWithContext(ctx, os.Args[1:], os.Stdout, os.Stderr, defaultDependencies()))
}

func run(arguments []string, stdout io.Writer, stderr io.Writer, deps dependencies) int {
	return runWithContext(context.Background(), arguments, stdout, stderr, deps)
}

func runWithContext(ctx context.Context, arguments []string, stdout io.Writer, stderr io.Writer, deps dependencies) int {
	flags := flag.NewFlagSet("sway-herdr-init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	structured := flags.Bool("json", false, "write one JSON result")
	contextID := flags.String("context", "", "registered sway-session context UUID")
	var roles roleFlags
	flags.Var(&roles, "role", "pane role; repeat exactly twice (for example codex and shell)")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: sway-herdr-init [--json] --context <uuid> --role <left> --role <right>")
		fmt.Fprintln(flags.Output(), "Initializes only a genuinely empty registered Herdr session; existing sessions are unchanged.")
	}
	if err := flags.Parse(arguments); errors.Is(err, flag.ErrHelp) {
		return 0
	} else if err != nil {
		return 2
	}
	if flags.NArg() != 0 || *contextID == "" || len(roles) != herdrinit.RequiredRoles {
		flags.Usage()
		writeError(stderr, *structured, errors.New("context and exactly two roles are required"))
		return 2
	}
	id := sessionstate.ContextID(*contextID)
	if err := id.Validate(); err != nil {
		writeError(stderr, *structured, fmt.Errorf("invalid context ID: %w", err))
		return 2
	}
	paths, err := deps.userPaths()
	if err != nil {
		writeError(stderr, *structured, err)
		return 3
	}
	lock, err := deps.acquireContextLock(paths.RuntimeDir, id)
	if err != nil {
		writeError(stderr, *structured, err)
		return 3
	}
	defer lock.Close()
	executable, err := deps.resolveExecutable("herdr")
	if err != nil {
		writeError(stderr, *structured, err)
		return 3
	}
	operationContext, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	var result herdrinit.Result
	err = deps.inspectContext(paths.SessionStateRoot(), id, func(contextValue sessionstate.Context) error {
		var initializeErr error
		result, initializeErr = deps.initialize(operationContext, contextValue, []string(roles), herdrinit.ExecRunner{Executable: executable, User: paths})
		return initializeErr
	})
	if err != nil {
		writeError(stderr, *structured, err)
		return 3
	}
	if *structured {
		if err := json.NewEncoder(stdout).Encode(struct {
			OK bool `json:"ok"`
			herdrinit.Result
		}{OK: true, Result: result}); err != nil {
			fmt.Fprintf(stderr, "write result: %v\n", err)
			return 3
		}
		return 0
	}
	if result.Initialized {
		fmt.Fprintf(stdout, "initialized Herdr session %q as %s + %s\n", result.Session, result.Roles[0], result.Roles[1])
	} else {
		fmt.Fprintf(stdout, "Herdr session %q unchanged: %s\n", result.Session, result.Reason)
	}
	return 0
}

func writeError(stderr io.Writer, structured bool, err error) {
	if structured {
		_ = json.NewEncoder(stderr).Encode(struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}{Error: err.Error()})
		return
	}
	fmt.Fprintf(stderr, "sway-herdr-init: %v\n", err)
}
