package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// The two desktop windows are Alacritty surrogates with distinct ordinary app
// identities. Launch requests are recorded; no browser, messaging application,
// desktop launcher, login compositor or secrets service participates.
func TestSessionRuntimeStartupPromptApplicationHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway/Alacritty integration")
	}
	for _, name := range []string{"sway", "alacritty", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("headless integration requires %s: %v", name, err)
		}
	}
	h := newRestoreCleanupHeadless(t)
	terminalIDs := []sessionstate.ContextID{
		testManagedContextID,
		"6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"6ba7b811-9dad-11d1-80b4-00c04fd430c8",
		"6ba7b812-9dad-11d1-80b4-00c04fd430c8",
	}
	appIDs := []sessionstate.ContextID{"33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"}
	appIdentities := []string{"diagnostic.Chrome", "diagnostic.Slack"}
	registry := sessionRegistryIDs(terminalIDs...)
	for index := range registry.Contexts {
		registry.Contexts[index].Launcher.Cwd = h.root
	}
	for index, id := range appIDs {
		desktopID := []string{"DiagnosticChrome.desktop", "DiagnosticSlack.desktop"}[index]
		desktopPath := filepath.Join(h.root, "data", desktopID)
		if err := os.WriteFile(desktopPath, []byte(fmt.Sprintf("[Desktop Entry]\nType=Application\nName=LAB-275 surrogate %d\nExec=/usr/bin/alacritty\nStartupWMClass=%s\n", index, appIdentities[index])), 0600); err != nil {
			t.Fatal(err)
		}
		registry.Contexts = append(registry.Contexts, sessionstate.Context{
			ID: id, Label: "LAB-275 " + appIdentities[index], Provider: "desktop", State: sessionstate.ContextActive,
			Launcher: sessionstate.Launcher{
				Kind: sessionstate.LauncherDesktop, DesktopID: desktopID,
				DesktopOrigin: sessionstate.DesktopEntrySystem, DesktopPath: desktopPath,
			},
			App: &sessionstate.Application{
				Identity:    sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: appIdentities[index]},
				DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow,
			},
		})
	}
	if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
		t.Fatal(err)
	}
	desired := exactDaemonSnapshot("100", terminalIDs[:2]...)
	desired.Workspaces[0].Tiling.Layout = sessionstate.LayoutTabbed
	secondGroup := exactDaemonSnapshot("101", terminalIDs[2:]...).Workspaces[0]
	secondGroup.Tiling.Layout = sessionstate.LayoutTabbed
	desired.Workspaces = append([]sessionstate.WorkspaceLayout{{Name: "99", RestoreMode: sessionstate.WorkspaceRestorePlacementOnly, PlacementContexts: slices.Clone(appIDs)}}, desired.Workspaces[0], secondGroup)
	if err := sessionstate.LayoutStoreFor(h.state).Save(desired); err != nil {
		t.Fatal(err)
	}
	// A preexisting unmanaged view receives automatic focus when the prompt
	// closes. It is fixture setup before the runtime subscribes to Sway.
	_, baselineID := startupPromptWindow(t, h, "diagnostic-existing-view")
	requester := &restoreCleanupRealRequester{Client: h.client}
	launcher := &recordingApplicationLauncher{}
	stream := &swayipc.EventStreamState{}
	start := time.Now()
	now := start
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Context: h.ctx, Root: h.state, StartedAt: start, EventStreamState: stream,
		Now: func() time.Time { return now }, CompositorID: strings.Repeat("a", 64), ApplicationLauncher: launcher,
		ApplicationRestore: sessionstate.ApplicationRestoreOptions{
			AdoptionGrace: time.Second, CloseGrace: 2 * time.Second, LaunchTimeout: 30 * time.Second, MaxConcurrent: 2,
		},
		IndicatorCatalog: func() (sessionstate.DesktopCatalog, error) { return sessionstate.DesktopCatalog{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.subscribe(runtime, stream)
	reconcile := func() {
		for range 4 {
			tree := h.tree()
			refresh, err := runtime.Reconcile(tree, now)
			if err != nil {
				t.Fatalf("reconcile at %s: %v; commands=%q", now.Sub(start), err, requester.commands)
			}
			indicatorRefresh, err := runtime.ReconcileIndicators(tree)
			if err != nil {
				t.Fatal(err)
			}
			if !refresh && !indicatorRefresh {
				return
			}
		}
	}
	newEvents, focusEvents, closeEvents := 0, 0, 0
	handle := func(event swayipc.Event) {
		if event.Type == swayipc.EventBinding {
			t.Fatalf("unexpected user binding during automatic startup: %+v", event)
		}
		if event.Type == swayipc.EventWindow {
			switch event.Change {
			case "new":
				newEvents++
			case "focus":
				focusEvents++
			case "close":
				closeEvents++
			}
		}
		runtime.HandleEvent(event, now)
		if runtime.restoreCancelled {
			t.Fatalf("automatic real event cancelled restoration: %+v; commands=%q", event, requester.commands)
		}
		if event.AffectsSessionLayout() {
			reconcile()
		}
	}
	drain := func() { drainRestoreFocusDaemonEvents(t, h, handle) }
	reconcile()
	now = start.Add(2 * time.Second)
	reconcile()
	drain()
	if launcher.starts != 2 || len(launcher.contexts) != 2 {
		t.Fatalf("surrogate desktop requests were not launched exactly once each: %+v", launcher)
	}
	for _, id := range appIDs {
		if !slices.ContainsFunc(launcher.contexts, func(item sessionstate.Context) bool { return item.ID == id }) {
			t.Fatalf("desktop launch missing for %s: %+v", id, launcher.contexts)
		}
	}
	prompt, promptID := startupPromptWindow(t, h, "diagnostic-secrets-prompt")
	drain()
	if focusedContainerID(h.tree()) != promptID || runtime.startupComplete {
		t.Fatal("prompt must hold focus while saved applications and terminals are missing")
	}
	now = now.Add(61 * time.Second)
	reconcile()
	drain()
	if err := runtime.Flush(now); err != nil {
		t.Fatal(err)
	}
	var paused sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(h.state).LoadInto(&paused); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paused, desired) || focusedContainerID(h.tree()) != promptID || len(requester.commands) != 0 || launcher.starts != 2 {
		t.Fatalf("prolonged prompt pause lost saved intent/focus or repeated launch: saved=%+v focus=%d commands=%q launches=%d", paused, focusedContainerID(h.tree()), requester.commands, launcher.starts)
	}
	for _, item := range registry.Contexts {
		outcome := restoreReportOutcome(t, runtime, "automatic", item.ID)
		if outcome.Status != "pending" || outcome.Reason == "user_cancelled" {
			t.Fatalf("prolonged prompt pause consumed pending report: %+v", outcome)
		}
	}
	if err := syscall.Kill(-prompt.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	h.until("surrogate prompt close", func() bool { return findContainerByID(h.tree(), promptID) == nil })
	drain()
	if focusedContainerID(h.tree()) != baselineID || closeEvents != 1 {
		t.Fatalf("prompt did not automatically return focus: focus=%d want=%d close=%d", focusedContainerID(h.tree()), baselineID, closeEvents)
	}
	applicationWindows := make([]int64, len(appIDs))
	for index, identity := range appIdentities {
		_, applicationWindows[index] = startupPromptWindow(t, h, identity)
		if workspace := restoreCleanupWorkspace(h.tree(), applicationWindows[index]); workspace != "98" {
			t.Fatalf("late desktop surrogate initially mapped on %q, want wrong workspace98", workspace)
		}
		drain()
	}
	terminalWindows := make([]int64, len(terminalIDs))
	for index, id := range terminalIDs {
		terminalWindows[index] = h.terminal(id)
		drain()
	}
	settled := 0
	for range 128 {
		now = now.Add(100 * time.Millisecond)
		reconcile()
		drain()
		if runtime.startupComplete && runtime.restoreProgress == nil && !runtime.lateRestorePending && !runtime.restoreCleanupPending {
			settled++
			if settled == 4 {
				break
			}
		} else {
			settled = 0
		}
	}
	tree := h.tree()
	if settled != 4 {
		t.Fatalf("application/terminal restore did not settle after prolonged prompt: complete=%t progress=%+v late=%t commands=%q", runtime.startupComplete, runtime.restoreProgress, runtime.lateRestorePending, requester.commands)
	}
	for index, id := range applicationWindows {
		if workspace := restoreCleanupWorkspace(tree, id); workspace != "99" {
			t.Fatalf("late %s remained on workspace%q, want99; commands=%q", appIdentities[index], workspace, requester.commands)
		}
	}
	for group, workspace := range []string{"100", "101"} {
		node := restoreCleanupNamedWorkspace(tree, workspace)
		for node != nil && len(node.Nodes) == 1 && len(node.FloatingNodes) == 0 {
			node = node.Nodes[0]
		}
		if node == nil || node.Layout != "tabbed" || len(node.Nodes) != 2 || len(node.FloatingNodes) != 0 {
			t.Fatalf("saved workspace%s tab group did not converge: %+v; commands=%q", workspace, node, requester.commands)
		}
		for index, child := range node.Nodes {
			contextIndex := group*2 + index
			mark, err := terminalIDs[contextIndex].Mark()
			if err != nil || child.ID != terminalWindows[contextIndex] || !slices.Contains(child.Marks, mark) {
				t.Fatalf("workspace%s child%d lost identity/order: %+v; %v", workspace, index, child, err)
			}
		}
	}
	for _, item := range registry.Contexts {
		outcome := restoreReportOutcome(t, runtime, "automatic", item.ID)
		if outcome.Status != "completed" || !outcome.WindowMapped || !outcome.PlacementApplied || outcome.Requested.Layout && !outcome.LayoutApplied {
			t.Fatalf("post-prompt application/terminal report did not complete: %+v", outcome)
		}
	}
	if launcher.starts != 2 || restoreCleanupWorkspace(tree, baselineID) != "98" {
		t.Fatalf("restore repeated app launch or moved baseline: launches=%d baselineworkspace=%s", launcher.starts, restoreCleanupWorkspace(tree, baselineID))
	}
	assertRestoreFocusNoTemporaryState(t, tree)
	if err := runtime.Flush(now.Add(sessionSnapshotDebounce)); err != nil {
		t.Fatal(err)
	}
	var saved sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(h.state).LoadInto(&saved); err != nil {
		t.Fatal(err)
	}
	for group, name := range []string{"100", "101"} {
		workspace, exists := workspaceByName(saved, name)
		if !exists || workspace.Tiling == nil || workspace.Tiling.Layout != sessionstate.LayoutTabbed || len(workspace.Tiling.Children) != 2 {
			t.Fatalf("Flush lost workspace%s tab group: %+v", name, saved)
		}
		for index, child := range workspace.Tiling.Children {
			if child.ContextID == nil || *child.ContextID != terminalIDs[group*2+index] || len(child.Children) != 0 {
				t.Fatalf("Flush lost workspace%s child%d: %+v", name, index, child)
			}
		}
	}
	t.Logf("prompt paused 61s; real new=%d focus=%d close=%d; two desktop surrogates restored99, terminal tabs100/101", newEvents, focusEvents, closeEvents)
}

func startupPromptWindow(t *testing.T, h *restoreCleanupHeadless, identity string) (*exec.Cmd, int64) {
	t.Helper()
	config := filepath.Join(h.root, "config", "surrogate-application.toml")
	if err := os.WriteFile(config, []byte("[window]\ndynamic_title = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command := h.start("alacritty", "--config-file", config, "--class", identity, "--title", "LAB-275 private surrogate", "-e", "/usr/bin/sleep", "120")
	var id int64
	h.until("ordinary surrogate application", func() bool {
		var visit func(*Node)
		visit = func(node *Node) {
			if node.AppID != nil && *node.AppID == identity && node.PID == command.Process.Pid {
				id = node.ID
			}
			for _, child := range append(slices.Clone(node.Nodes), node.FloatingNodes...) {
				visit(child)
			}
		}
		visit(h.tree())
		return id != 0
	})
	return command, id
}
