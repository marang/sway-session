package main

import (
	"context"
	"strings"
	"testing"

	"github.com/marang/sway-session/internal/herdrinit"
	sessionstate "github.com/marang/sway-session/internal/session"
)

func remoteManagerTestContext(t *testing.T, machineID string) sessionstate.Context {
	t.Helper()
	return sessionstate.Context{
		ID: "8f33d6d0-7c54-4da1-9e38-2bd290ef85ca", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: "local-name", Cwd: t.TempDir(), Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty, Remote: &sessionstate.TerminalRemoteBinding{Target: "fixture", Session: "remote-work", MachineID: machineID}}},
	}
}

func TestRemoteManagerSkipsLocalSessionStateAndInitialization(t *testing.T) {
	contextValue := remoteManagerTestContext(t, "")
	manager := herdrTerminalSessionManager{
		paths: func() (sessionstate.HerdrPaths, error) {
			return sessionstate.HerdrPaths{Root: "/extremely/long/" + strings.Repeat("x", 120) + "/herdr", ConfigFile: "/tmp/herdr/config.toml"}, nil
		},
		resolveProgram: func(string) (string, error) { return "/usr/bin/herdr", nil },
		validateHistory: func(sessionstate.HerdrPaths) error {
			t.Fatal("remote launch inspected local history config")
			return nil
		},
		initialize: func(context.Context, sessionstate.Context, []string, herdrinit.Runner) (herdrinit.Result, error) {
			t.Fatal("remote context initialized local panes")
			return herdrinit.Result{}, nil
		},
	}
	if err := manager.ValidateContext(contextValue); err != nil {
		t.Fatal(err)
	}
	if err := manager.PrepareContext(t.Context(), contextValue); err != nil {
		t.Fatal(err)
	}
	spec, err := manager.BuildProcessSpec(contextValue, "/usr/bin/alacritty")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(spec.Arguments, " "), "--remote fixture --session remote-work") {
		t.Fatalf("remote route not emitted: %v", spec.Arguments)
	}
	result, err := manager.Initialize(t.Context(), contextValue, []string{"codex", "shell"})
	if err == nil || result.Initialized || result.Reason != "remote_initialization_unsupported" {
		t.Fatalf("unexpected remote init result %+v, %v", result, err)
	}
}

func TestRemoteManagerPreparationRevalidatesSavedPinOutsidePureValidation(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	contextValue := remoteManagerTestContext(t, id)
	runner := &recordingHerdrRunner{responses: []string{`[{"id":"` + id + `","label":"Fixture","target":"changed-host","session":"remote-work","enabled":true,"selected":false}]`}}
	manager := herdrTerminalSessionManager{runner: runner, resolveProgram: func(string) (string, error) { return "/usr/bin/herdr", nil }}
	// Remote validation needs neither paths nor a subprocess. Preparation alone
	// checks the external catalog immediately before a missing-window launch.
	if err := manager.ValidateContext(contextValue); err != nil || len(runner.calls) != 0 {
		t.Fatalf("validation had side effects: %v %v", err, runner.calls)
	}
	if err := manager.PrepareContext(t.Context(), contextValue); err == nil || len(runner.calls) != 1 {
		t.Fatalf("retargeted profile accepted: %v %v", err, runner.calls)
	}
}
