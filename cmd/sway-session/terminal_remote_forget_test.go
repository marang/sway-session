package main

import (
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func TestTerminalForgetRequiresExplicitConfirmationAndUUID(t *testing.T) {
	for _, arguments := range [][]string{{}, {string(testContextID)}, {"--yes", "remote label"}} {
		deps := testDependencies(t)
		deps.newSwayClient = func(string) swayRequester { t.Fatal("invalid forget contacted Sway"); return nil }
		if _, problem := executeTerminalForget(t.Context(), arguments, deps); problem == nil {
			t.Fatalf("unconfirmed or non-UUID forget accepted: %v", arguments)
		}
	}
}

func TestTerminalForgetCLIUsesOnlyLocalRegistrationAndObservation(t *testing.T) {
	deps := testDependencies(t)
	item := registerTestContext(t, deps)
	item.State = sessionstate.ContextArchived
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	item.ArchivedAt = &at
	item.Launcher.Terminal.Remote = &sessionstate.TerminalRemoteBinding{Target: "fixture", Session: "remote-work"}
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := sessionstate.RegistryStoreFor(root).Save(sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{item}}); err != nil {
		t.Fatal(err)
	}
	client := &fakeSwayClient{trees: []*swayipc.TreeNode{treeWithContexts(), treeWithContexts()}}
	deps.newSwayClient = func(socket string) swayRequester {
		if socket != "/tmp/fixture-sway.sock" {
			t.Fatalf("wrong compositor: %s", socket)
		}
		return client
	}
	deps.resolveProgram = func(string) (string, error) {
		t.Fatal("forget resolved a terminal or Herdr executable")
		return "", nil
	}
	deps.herdrPaths = func() (sessionstate.HerdrPaths, error) {
		t.Fatal("forget inspected Herdr configuration")
		return sessionstate.HerdrPaths{}, nil
	}
	result, problem := executeTerminalForget(t.Context(), []string{"--yes", "--socket", "/tmp/fixture-sway.sock", string(item.ID)}, deps)
	if problem != nil || result.Command != "terminal forget" || len(result.Contexts) != 1 || result.Contexts[0].ID != item.ID {
		t.Fatalf("forget result %+v, failure %+v", result, problem)
	}
	if len(deps.herdrRunner.(*recordingHerdrRunner).calls) != 0 || len(deps.processStarter.(*recordingStarter).calls) != 0 {
		t.Fatal("forget ran a Herdr or terminal process")
	}
	var registry sessionstate.Registry
	if err := sessionstate.RegistryStoreFor(root).LoadInto(&registry); err != nil || len(registry.Contexts) != 0 {
		t.Fatalf("registration retained: %+v %v", registry, err)
	}
}
