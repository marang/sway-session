package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/marang/sway-title-animator/internal/herdrinit"
	sessionstate "github.com/marang/sway-title-animator/internal/session"
)

func testDependencies(t *testing.T) (dependencies, *int) {
	t.Helper()
	initialized := 0
	return dependencies{
		userPaths: func() (herdrinit.UserPaths, error) {
			return herdrinit.UserPaths{Home: "/home/test", Name: "test", UID: 1000, StateHome: "/home/test/.local/state", RuntimeDir: "/run/user/1000"}, nil
		},
		acquireContextLock: func(root string, id sessionstate.ContextID) (io.Closer, error) {
			if root != "/run/user/1000" || id != sessionstate.ContextID("8f33d6d0-7c54-4da1-9e38-2bd290ef85ca") {
				t.Fatalf("unexpected context lock: root=%s id=%s", root, id)
			}
			return io.NopCloser(strings.NewReader("")), nil
		},
		resolveExecutable: func(name string) (string, error) {
			if name != "herdr" {
				t.Fatalf("unexpected executable: %s", name)
			}
			return "/usr/bin/herdr", nil
		},
		inspectContext: func(root string, id sessionstate.ContextID, inspect func(sessionstate.Context) error) error {
			if root != "/home/test/.local/state/sway-session" {
				t.Fatalf("unexpected state root: %s", root)
			}
			return inspect(sessionstate.Context{
				ID: id, State: sessionstate.ContextActive,
				Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: "lab-88", Cwd: "/repo", Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}},
			})
		},
		initialize: func(_ context.Context, contextValue sessionstate.Context, roles []string, runner herdrinit.Runner) (herdrinit.Result, error) {
			initialized++
			if contextValue.Launcher.Session != "lab-88" || strings.Join(roles, ",") != "codex,shell" {
				t.Fatalf("unexpected initialization: context=%+v roles=%v", contextValue, roles)
			}
			if runner == nil {
				t.Fatal("runner is nil")
			}
			return herdrinit.Result{ContextID: contextValue.ID, Session: "lab-88", Roles: roles, Initialized: true}, nil
		},
	}, &initialized
}

func TestRunInitializesWithTwoExplicitRoles(t *testing.T) {
	deps, initialized := testDependencies(t)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exit := run([]string{
		"--json",
		"--context", "8f33d6d0-7c54-4da1-9e38-2bd290ef85ca",
		"--role", "codex",
		"--role", "shell",
	}, &stdout, &stderr, deps)
	if exit != 0 {
		t.Fatalf("unexpected exit %d: %s", exit, stderr.String())
	}
	if *initialized != 1 {
		t.Fatalf("initialize called %d times", *initialized)
	}
	if !strings.Contains(stdout.String(), `"ok":true`) || !strings.Contains(stdout.String(), `"initialized":true`) {
		t.Fatalf("unexpected JSON output: %s", stdout.String())
	}
}

func TestRunRequiresRolesWithoutChangingState(t *testing.T) {
	deps, initialized := testDependencies(t)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exit := run([]string{"--context", "8f33d6d0-7c54-4da1-9e38-2bd290ef85ca"}, &stdout, &stderr, deps)
	if exit != 2 {
		t.Fatalf("unexpected exit %d", exit)
	}
	if *initialized != 0 {
		t.Fatalf("initialize called %d times", *initialized)
	}
	if !strings.Contains(stderr.String(), "exactly two roles") {
		t.Fatalf("missing role guidance: %s", stderr.String())
	}
}

func TestRunDoesNotInitializeUnknownContext(t *testing.T) {
	deps, initialized := testDependencies(t)
	deps.inspectContext = func(string, sessionstate.ContextID, func(sessionstate.Context) error) error {
		return errors.New("context is not registered")
	}
	var stderr bytes.Buffer
	exit := run([]string{
		"--context", "8f33d6d0-7c54-4da1-9e38-2bd290ef85ca",
		"--role", "codex", "--role", "shell",
	}, &bytes.Buffer{}, &stderr, deps)
	if exit != 3 || *initialized != 0 || !strings.Contains(stderr.String(), "not registered") {
		t.Fatalf("unexpected result: exit=%d initialized=%d stderr=%s", exit, *initialized, stderr.String())
	}
}

func TestRunStopsBeforeRegistryReadWhenContextLockIsBusy(t *testing.T) {
	deps, initialized := testDependencies(t)
	loaded := false
	deps.acquireContextLock = func(string, sessionstate.ContextID) (io.Closer, error) {
		return nil, herdrinit.ErrInitializationRunning
	}
	deps.inspectContext = func(string, sessionstate.ContextID, func(sessionstate.Context) error) error {
		loaded = true
		return nil
	}
	var stderr bytes.Buffer
	exit := run([]string{
		"--context", "8f33d6d0-7c54-4da1-9e38-2bd290ef85ca",
		"--role", "codex", "--role", "shell",
	}, &bytes.Buffer{}, &stderr, deps)
	if exit != 3 || loaded || *initialized != 0 || !strings.Contains(stderr.String(), "already running") {
		t.Fatalf("unexpected result: exit=%d loaded=%t initialized=%d stderr=%s", exit, loaded, *initialized, stderr.String())
	}
}

func TestRunPropagatesCancellationToInitialization(t *testing.T) {
	deps, initialized := testDependencies(t)
	deps.initialize = func(ctx context.Context, _ sessionstate.Context, _ []string, _ herdrinit.Runner) (herdrinit.Result, error) {
		*initialized++
		return herdrinit.Result{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stderr bytes.Buffer

	exit := runWithContext(ctx, []string{
		"--context", "8f33d6d0-7c54-4da1-9e38-2bd290ef85ca",
		"--role", "codex", "--role", "shell",
	}, &bytes.Buffer{}, &stderr, deps)

	if exit != 3 || *initialized != 1 || !strings.Contains(stderr.String(), context.Canceled.Error()) {
		t.Fatalf("cancellation was not propagated: exit=%d initialized=%d stderr=%s", exit, *initialized, stderr.String())
	}
}

func TestRunHelpIsSuccessfulAndDoesNotInitialize(t *testing.T) {
	deps, initialized := testDependencies(t)
	var stderr bytes.Buffer
	if exit := run([]string{"--help"}, &bytes.Buffer{}, &stderr, deps); exit != 0 {
		t.Fatalf("help exited %d: %s", exit, stderr.String())
	}
	if *initialized != 0 || !strings.Contains(stderr.String(), "Usage: sway-herdr-init") {
		t.Fatalf("unexpected help behavior: initialized=%d output=%s", *initialized, stderr.String())
	}
}
