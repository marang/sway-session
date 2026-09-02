package main

import (
	"context"
	"testing"
	"time"

	"github.com/marang/sway-title-animator/internal/herdrinit"
	sessionstate "github.com/marang/sway-title-animator/internal/session"
)

func TestHerdrTerminalSessionInitializationHasOverallDeadline(t *testing.T) {
	contextValue := sessionstate.Context{
		ID:    sessionstate.ContextID("8f33d6d0-7c54-4da1-9e38-2bd290ef85ca"),
		State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{
			Kind: sessionstate.LauncherHerdr, Session: "lab-110", Cwd: t.TempDir(),
			Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty},
		},
	}
	manager := herdrTerminalSessionManager{
		paths: func() (sessionstate.HerdrPaths, error) {
			return sessionstate.HerdrPaths{Root: "/tmp/herdr", ConfigFile: "/tmp/herdr/config.toml"}, nil
		},
		resolveProgram: func(string) (string, error) { return "/usr/bin/herdr", nil },
		initialize: func(ctx context.Context, _ sessionstate.Context, _ []string, _ herdrinit.Runner) (herdrinit.Result, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("Herdr initialization context has no deadline")
			}
			remaining := time.Until(deadline)
			if remaining <= 0 || remaining > herdrInitializationTimeout {
				t.Fatalf("unexpected initialization deadline: %s", remaining)
			}
			return herdrinit.Result{Initialized: true}, nil
		},
	}
	result, err := manager.Initialize(t.Context(), contextValue, []string{"codex", "shell"})
	if err != nil || !result.Initialized {
		t.Fatalf("bounded initialization result=%+v err=%v", result, err)
	}
}
