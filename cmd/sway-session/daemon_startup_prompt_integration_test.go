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

// The prompt maps on the private compositor's initial workspace without any
// binding or workspace command while startup waits for the saved terminals.
func TestSessionRuntimeStartupPromptHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway/Alacritty integration")
	}
	for _, name := range []string{"sway", "alacritty", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("headless integration requires %s: %v", name, err)
		}
	}
	h := newRestoreCleanupHeadless(t)
	configPath := filepath.Join(h.root, "config", "sway.conf")
	configBytes, configErr := os.ReadFile(configPath)
	if configErr != nil {
		t.Fatal(configErr)
	}
	// Keep the unrelated dialog focused while saved test windows map. The
	// daemon must reconstruct them without waiting for a focus change.
	configBytes = append(configBytes, []byte("\nno_focus [app_id=\"^"+strings.ReplaceAll(sessionstate.AppIDPrefix, ".", "[.]")+"\"]\n")...)
	if err := os.WriteFile(configPath, configBytes, 0600); err != nil {
		t.Fatal(err)
	}
	h.command("reload")
	ids := []sessionstate.ContextID{testManagedContextID, restoreReportSecondID}
	registry := sessionRegistryIDs(ids...)
	for index := range registry.Contexts {
		registry.Contexts[index].Launcher.Cwd = h.root
	}
	if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
		t.Fatal(err)
	}
	desired := exactDaemonSnapshot("99", ids...)
	desired.Workspaces[0].Tiling.Layout = sessionstate.LayoutTabbed
	if err := sessionstate.LayoutStoreFor(h.state).Save(desired); err != nil {
		t.Fatal(err)
	}
	// This preexisting view is observed at startup. Closing the prompt later
	// returns focus here automatically, without a user focus command.
	_, baselineID := startupPromptWindow(t, h, "diagnostic-existing-view")
	requester := &restoreCleanupRealRequester{Client: h.client}
	streamState := &swayipc.EventStreamState{}
	now := time.Now()
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Context: h.ctx, Root: h.state, EventStreamState: streamState,
		CompositorID: strings.Repeat("a", 64), StartedAt: now, Now: func() time.Time { return now },
		ApplicationRestore: sessionstate.ApplicationRestoreOptions{
			AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: time.Second, MaxConcurrent: 1,
		},
		IndicatorCatalog: func() (sessionstate.DesktopCatalog, error) { return sessionstate.DesktopCatalog{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.subscribe(runtime, streamState)
	reconcile := func() {
		// Mirror the daemon's bounded per-event fresh observations, using the
		// same injected clock for both events and timer reconciliation.
		for range 4 {
			refresh, err := runtime.Reconcile(h.tree(), now)
			if err != nil {
				t.Fatal(err)
			}
			if !refresh {
				return
			}
		}
	}
	reconcile()
	if runtime.startupComplete || runtime.restoreProgress != nil {
		t.Fatal("cold fixture must await registered terminals before reconstruction")
	}

	prompt, promptID := startupPromptWindow(t, h, "diagnostic-secrets-prompt")
	newEvents, focusEvents, closeEvents, refocusEvents := 0, 0, 0, 0
	handle := func(event swayipc.Event) {
		if event.Type == swayipc.EventBinding {
			t.Fatalf("prompt mapping unexpectedly produced a user binding: %+v", event)
		}
		if event.Type == swayipc.EventWindow && event.Container != nil && event.Container.ID == promptID {
			switch event.Change {
			case "new":
				newEvents++
			case "focus":
				focusEvents++
			case "close":
				closeEvents++
			}
		}
		if event.Type == swayipc.EventWindow && event.Change == "focus" && event.Container != nil && event.Container.ID == baselineID {
			refocusEvents++
		}
		runtime.HandleEvent(event, now)
		if event.AffectsSessionLayout() {
			reconcile()
		}
	}
	drainRestoreFocusDaemonEvents(t, h, handle)
	if newEvents != 1 || focusEvents == 0 {
		t.Fatalf("real prompt map/focus events missing: new=%d focus=%d", newEvents, focusEvents)
	}
	if workspace := restoreCleanupWorkspace(h.tree(), promptID); workspace != "98" {
		t.Fatalf("automatically mapped prompt workspace = %q, want 98", workspace)
	}
	for _, id := range ids {
		outcome := restoreReportOutcome(t, runtime, "automatic", id)
		if outcome.Reason == "user_cancelled" || outcome.Status == "interrupted" || runtime.restoreCancelled || runtime.startupComplete {
			t.Fatalf("automatic unregistered prompt map/focus permanently cancelled pending startup restore: new=%d focus=%d cancelled=%t startupComplete=%t outcome=%+v; commands=%q",
				newEvents, focusEvents, runtime.restoreCancelled, runtime.startupComplete, outcome, requester.commands)
		}
	}
	// Missing saved windows retain their intent through normal startup capture.
	// The unrelated dialog adds no pause or deadline to the runtime.
	now = now.Add(8 * time.Second)
	if _, err := runtime.Reconcile(h.tree(), now); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Flush(now.Add(sessionSnapshotDebounce)); err != nil {
		t.Fatal(err)
	}
	var waitingSnapshot sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(h.state).LoadInto(&waitingSnapshot); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(waitingSnapshot, desired) || focusedContainerID(h.tree()) != promptID || len(requester.commands) != 0 {
		t.Fatalf("missing windows lost saved intent before startup settling: saved=%+v focus=%d commands=%q", waitingSnapshot, focusedContainerID(h.tree()), requester.commands)
	}
	owned := make([]int64, len(ids))
	for index, id := range ids {
		owned[index] = h.terminal(id)
	}
	settled := 0
	for pass := range 128 {
		drainRestoreFocusDaemonEvents(t, h, handle)
		now = now.Add(100 * time.Millisecond)
		if _, err := runtime.Reconcile(h.tree(), now); err != nil {
			t.Fatalf("post-prompt settle pass %d: %v", pass, err)
		}
		if runtime.startupComplete && runtime.restoreProgress == nil && !runtime.lateRestorePending && !runtime.restoreCleanupPending {
			settled++
			if settled == 4 {
				break
			}
		} else {
			settled = 0
		}
	}
	if settled != 4 {
		t.Fatalf("saved windows did not restore while the unrelated dialog remained open: complete=%t progress=%+v late=%t cleanup=%t deadline=%s now=%s commands=%q", runtime.startupComplete, runtime.restoreProgress, runtime.lateRestorePending, runtime.restoreCleanupPending, runtime.startupDeadline, now, requester.commands)
	}
	tree := h.tree()
	node := restoreCleanupNamedWorkspace(tree, "99")
	for node != nil && len(node.Nodes) == 1 && len(node.FloatingNodes) == 0 {
		node = node.Nodes[0]
	}
	if node == nil || node.Layout != "tabbed" || len(node.Nodes) != len(owned) || len(node.FloatingNodes) != 0 {
		t.Fatalf("post-prompt restore lost saved tabbed layout: %+v; commands=%q", node, requester.commands)
	}
	for index, child := range node.Nodes {
		mark, err := ids[index].Mark()
		if err != nil || child.ID != owned[index] || !slices.Contains(child.Marks, mark) {
			t.Fatalf("post-prompt restored child %d lost saved order or identity: %+v; %v", index, child, err)
		}
		outcome := restoreReportOutcome(t, runtime, "automatic", ids[index])
		if outcome.Status != "completed" || !outcome.WindowMapped || !outcome.PlacementApplied || !outcome.LayoutApplied {
			t.Fatalf("post-prompt restore did not prove durable completion: %+v", outcome)
		}
	}
	if restoreCleanupWorkspace(tree, baselineID) != "98" {
		t.Fatal("startup restore moved the unrelated preexisting view")
	}
	assertRestoreFocusNoTemporaryState(t, tree)
	if err := syscall.Kill(-prompt.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	h.until("private prompt close", func() bool { return findContainerByID(h.tree(), promptID) == nil })
	drainRestoreFocusDaemonEvents(t, h, handle)
	if closeEvents != 1 || refocusEvents == 0 || focusedContainerID(h.tree()) != baselineID {
		t.Fatalf("real prompt close/refocus events missing: close=%d refocus=%d focus=%d want=%d", closeEvents, refocusEvents, focusedContainerID(h.tree()), baselineID)
	}
	for _, id := range ids {
		outcome := restoreReportOutcome(t, runtime, "automatic", id)
		if outcome.Reason == "user_cancelled" || outcome.Status == "interrupted" || runtime.restoreCancelled {
			t.Fatalf("automatic prompt close/refocus permanently cancelled pending startup restore: close=%d refocus=%d cancelled=%t outcome=%+v; commands=%q", closeEvents, refocusEvents, runtime.restoreCancelled, outcome, requester.commands)
		}
	}
	if err := runtime.Flush(now.Add(sessionSnapshotDebounce)); err != nil {
		t.Fatal(err)
	}
	var restoredSnapshot sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(h.state).LoadInto(&restoredSnapshot); err != nil {
		t.Fatal(err)
	}
	workspace, exists := workspaceByName(restoredSnapshot, "99")
	if !exists || workspace.Tiling == nil || workspace.Tiling.Layout != sessionstate.LayoutTabbed || len(workspace.Tiling.Children) != len(ids) {
		t.Fatalf("post-prompt Flush lost saved tabbed layout: %+v", restoredSnapshot)
	}
	for index, child := range workspace.Tiling.Children {
		if child.ContextID == nil || *child.ContextID != ids[index] || len(child.Children) != 0 {
			t.Fatalf("post-prompt saved child %d lost saved identity/order: %+v", index, child)
		}
	}
	t.Logf("restore completed before prompt closure; real lifecycle events: new=%d focus=%d close=%d refocus=%d; saved terminals restored tabbed on99", newEvents, focusEvents, closeEvents, refocusEvents)
}

func TestSessionRuntimeForeignMoveHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("requires private headless compositor")
	}
	h := newRestoreCleanupHeadless(t)
	ids := []sessionstate.ContextID{testManagedContextID, restoreReportSecondID}
	registry := sessionRegistryIDs(ids...)
	for index := range registry.Contexts {
		registry.Contexts[index].Launcher.Cwd = h.root
	}
	if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
		t.Fatal(err)
	}
	desired := exactDaemonSnapshot("98", ids...)
	desired.Workspaces[0].Tiling.Layout = sessionstate.LayoutTabbed
	if err := sessionstate.LayoutStoreFor(h.state).Save(desired); err != nil {
		t.Fatal(err)
	}
	savedID := h.terminal(ids[0])
	requester := &restoreCleanupRealRequester{Client: h.client}
	state := &swayipc.EventStreamState{}
	now := time.Now()
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Context: h.ctx, Root: h.state, EventStreamState: state,
		StartedAt: now, Now: func() time.Time { return now }, CompositorID: strings.Repeat("a", 64),
		ApplicationRestore: sessionstate.ApplicationRestoreOptions{
			AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: time.Second, MaxConcurrent: 1,
		},
		IndicatorCatalog: func() (sessionstate.DesktopCatalog, error) { return sessionstate.DesktopCatalog{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.subscribe(runtime, state)
	reconcile := func() {
		for range 4 {
			refresh, err := runtime.Reconcile(h.tree(), now)
			if err != nil {
				t.Fatal(err)
			}
			if !refresh {
				return
			}
		}
	}
	var events []string
	handle := func(event swayipc.Event) {
		if event.Type == swayipc.EventWindow && event.Container != nil {
			events = append(events, fmt.Sprintf("%s(%d)", event.Change, event.Container.ID))
		}
		runtime.HandleEvent(event, now)
		if event.AffectsSessionLayout() {
			reconcile()
		}
	}
	reconcile()
	drainRestoreFocusDaemonEvents(t, h, handle)
	_, promptID := startupPromptWindow(t, h, "diagnostic-secrets-prompt")
	drainRestoreFocusDaemonEvents(t, h, handle)
	if runtime.restoreCancelled || focusedContainerID(h.tree()) != promptID {
		t.Fatalf("fixture failed before foreign move: cancelled=%t focus=%d events=%v", runtime.restoreCancelled, focusedContainerID(h.tree()), events)
	}
	events = nil
	h.command(fmt.Sprintf("[con_id=%d] move container to workspace 99", promptID))
	drainRestoreFocusDaemonEvents(t, h, handle)
	if focusedContainerID(h.tree()) != savedID || runtime.restoreCancelled {
		t.Fatalf("foreign move's automatic source refocus cancelled saved reconstruction: cancelled=%t focus=%d want=%d events=%v", runtime.restoreCancelled, focusedContainerID(h.tree()), savedID, events)
	}
	// Window focus remains irrelevant after the automatic return. A genuine
	// move of the saved window must still cancel the pending tab reconstruction.
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: findContainerByID(h.tree(), savedID)}, now)
	if runtime.restoreCancelled {
		t.Fatal("a subsequent saved-window focus cancelled restoration")
	}
	h.command(fmt.Sprintf("[con_id=%d] move container to workspace 100", savedID))
	drainRestoreFocusDaemonEvents(t, h, handle)
	if !runtime.restoreCancelled {
		t.Fatal("a subsequent saved-window move did not cancel pending reconstruction")
	}
}
