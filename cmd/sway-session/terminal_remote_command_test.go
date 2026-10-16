package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func TestTerminalRemoteCommandPersistsAndReopensExactRoute(t *testing.T) {
	deps := testDependencies(t)
	client := &terminalCommandClient{id: testContextID}
	starter := &terminalCommandStarter{onStart: func() { client.setMapped(true) }}
	deps.newSwayClient = func(string) swayRequester { return client }
	deps.processStarter = starter
	first := runTerminalJSON(t, deps, "--json", "terminal", "--new", "--remote", "workbox", "--remote-session", "default", "--socket", "/tmp/sway.sock")
	if first.Terminal == nil || first.Terminal.Remote == nil || first.Terminal.Remote.Target != "workbox" || first.Terminal.Remote.Session != "default" {
		t.Fatalf("route missing: %+v", first)
	}
	reopened := runTerminalJSON(t, deps, "--json", "terminal", "--context", string(first.Terminal.ContextID), "--socket", "/tmp/sway.sock")
	if reopened.Terminal.Remote == nil || *reopened.Terminal.Remote != *first.Terminal.Remote || len(starter.specs) != 1 {
		t.Fatalf("reopen changed route or duplicated window: %+v", reopened)
	}
}

func TestTerminalRemoteOptionsCannotInitializeOrRedirectExactContexts(t *testing.T) {
	for _, args := range [][]string{
		{"--remote", "workbox", "--remote-session", "work"},
		{"--new", "--remote", "workbox"},
		{"--new", "--machine", "id", "--remote", "workbox"},
		{"--new", "--remote", "workbox", "--remote-session", "work", "--role", "codex", "--role", "shell"},
		{"--context", string(testContextID), "--remote", "workbox", "--remote-session", "work"},
	} {
		var out, errors bytes.Buffer
		code := runWith(append([]string{"terminal"}, args...), strings.NewReader(""), &out, &errors, dependencies{})
		if code != exitUsage {
			t.Fatalf("invalid remote options code=%d args=%v err=%s", code, args, errors.String())
		}
	}
}

func TestRemoteClosePreservesIntentAfterFreshObservedAbsence(t *testing.T) {
	guard := &testTerminalCloseGuard{generation: 7, safe: true}
	runtime, _, root, now, leaf := armedTerminalClose(t, guard)
	defer runtime.Shutdown()
	registry := sessionRegistry(testManagedContextID)
	registry.Contexts[0].Launcher.Terminal.Remote = &sessionstate.TerminalRemoteBinding{Target: "workbox", Session: "work"}
	if err := sessionstate.RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Reconcile(daemonTree("98", leaf), now); err != nil {
		t.Fatal(err)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: leaf}, now)
	if err := runtime.Flush(now.Add(time.Second + terminalCloseGrace)); err != nil {
		t.Fatal(err)
	}
	assertTerminalCloseState(t, root, sessionstate.ContextActive)
}

func TestRemoteManagerSmallViewShowsRouteAndRefusesPurge(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	item := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Remote work", sessionstate.ContextActive)
	item.Remote = &sessionstate.TerminalRemoteBinding{Target: "workbox", Session: "agents"}
	ops := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{item}}}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 48, Height: 16})
	view := model.View().Content
	if !strings.Contains(view, "workbox / agents") {
		t.Fatalf("remote route hidden in small viewport:\n%s", view)
	}
	t.Logf("48x16 remote view:\n%s", view)
	model = terminalManageUpdate(t, model, terminalManageKey("d"))
	if model.mode == terminalManagePurgeMode || model.err == nil || !strings.Contains(model.err.Error(), "terminal forget --yes") {
		t.Fatalf("remote purge showed destructive confirmation: %+v", model)
	}
}
