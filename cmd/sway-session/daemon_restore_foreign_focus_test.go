package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func TestSessionRuntimeForeignStreamLossReportsObservationFailure(t *testing.T) {
	for _, seeded := range []bool{false, true} {
		name := "before_report_seed"
		if seeded {
			name = "after_report_seed"
		}
		t.Run(name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			if err := sessionstate.RegistryStoreFor(state).Save(sessionRegistry(testManagedContextID)); err != nil {
				t.Fatal(err)
			}
			if err := sessionstate.LayoutStoreFor(state).Save(exactDaemonSnapshot("98", testManagedContextID)); err != nil {
				t.Fatal(err)
			}
			runtime, err := newSessionRuntimeWithOptions(&recordingRequester{}, sessionRuntimeOptions{Root: state})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 1}, now)
			if seeded {
				if _, err := runtime.Reconcile(daemonTree("98"), now); err != nil {
					t.Fatal(err)
				}
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "disconnected", StreamEpoch: 1}, now)
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 2}, now)
			if _, err := runtime.Reconcile(daemonTree("98"), now); err != nil {
				t.Fatal(err)
			}
			outcome := restoreReportOutcome(t, runtime, "automatic", testManagedContextID)
			if outcome.Status != "interrupted" || outcome.Reason != "observation_unavailable" {
				t.Fatalf("lost observation was attributed to user cancellation: %+v", outcome)
			}
		})
	}
}

func TestSessionRuntimeForeignViewBecomingManagedRetainsCloseIntent(t *testing.T) {
	runtime, compositor, first, now := newMappingFocusScenario(t)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: first}, now)
	if _, err := runtime.Reconcile(compositor.root, now); err != nil {
		t.Fatal(err)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: first}, now)
	compositor.drain(runtime, now)
	identity := "org.example.Unregistered"
	view := &Node{ID: 43, Type: "con", AppID: &identity, Focused: true}
	workspace := compositor.root.Nodes[0].Nodes[0]
	workspace.Nodes = append(workspace.Nodes, view)
	workspace.Focus = []int64{view.ID, first.ID}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: view}, now)
	if _, err := runtime.Reconcile(compositor.root, now); err != nil {
		t.Fatal(err)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: view}, now)
	compositor.drain(runtime, now)
	// A fresh validated identity changes which lifecycle owns this window.
	// Its later close must no longer be exempt as an unrelated dialog.
	id := sessionstate.ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	identity, _ = id.AppID()
	if _, err := runtime.Reconcile(compositor.root, now); err != nil {
		t.Fatal(err)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: view}, now)
	if !runtime.restoreCancelled {
		t.Fatal("a now-managed window close was still ignored as foreign lifecycle")
	}
}

func TestSessionRuntimeForeignActivityDoesNotCancelSavedTargets(t *testing.T) {
	for _, change := range []string{"focus", "move", "binding", "saved_focus"} {
		t.Run(change, func(t *testing.T) {
			runtime, compositor, first, now := newMappingFocusScenario(t)
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: first}, now)
			if _, err := runtime.Reconcile(compositor.root, now); err != nil {
				t.Fatal(err)
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: first}, now)
			compositor.drain(runtime, now)
			identity := "org.example.Authentication"
			view := &Node{ID: 43, Type: "con", AppID: &identity, Focused: true}
			workspace := compositor.root.Nodes[0].Nodes[0]
			workspace.FloatingNodes = []*Node{view}
			workspace.Focus = []int64{view.ID, first.ID}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: view}, now)
			if _, err := runtime.Reconcile(compositor.root, now); err != nil {
				t.Fatal(err)
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: view}, now)
			compositor.drain(runtime, now)
			event := swayipc.Event{Type: swayipc.EventWindow, Change: change, Container: view}
			if change == "binding" {
				event.Type = swayipc.EventBinding
			}
			if change == "saved_focus" {
				event.Change, event.Container = "focus", first
			}
			runtime.HandleEvent(event, now)
			wantCancelled := change == "binding" || change == "saved_focus"
			if runtime.restoreCancelled != wantCancelled {
				t.Fatalf("activity %s cancelled=%t, want %t", change, runtime.restoreCancelled, wantCancelled)
			}
		})
	}
}

func TestSessionRuntimeForeignClosePreservesLateApplicationIntent(t *testing.T) {
	for _, userEdit := range []bool{false, true} {
		name := "automatic_close"
		if userEdit {
			name = "layout_edit_before_close"
		}
		t.Run(name, func(t *testing.T) {
			runtime, requester, app, terminal, window, start := delayedApplicationScenario(t)
			root := daemonTree("98", terminal)
			workspace := root.Nodes[0].Nodes[0]
			appID := "org.example.Authentication"
			prompt := &Node{ID: 43, Type: "con", AppID: &appID, Focused: true}
			workspace.FloatingNodes = []*Node{prompt}
			workspace.Focus = []int64{prompt.ID, terminal.ID}
			now := start.Add(11 * time.Second)
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: prompt}, now)
			if _, err := runtime.Reconcile(root, now); err != nil {
				t.Fatal(err)
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: prompt}, now)
			if _, err := runtime.Reconcile(root, now.Add(time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if runtime.restoreCancelled {
				t.Fatal("unregistered dialog mapping cancelled pending saved application layout")
			}
			if userEdit {
				// No binding/focus event accompanies this layout command. The
				// dialog closes before the next observation of the changed tree.
				workspace.Layout = "splitv"
			}
			workspace.FloatingNodes = nil
			workspace.Focus = []int64{terminal.ID}
			terminal.Focused = true
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: prompt}, now)
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: terminal}, now)
			if _, err := runtime.Reconcile(root, now.Add(2*time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if runtime.restoreCancelled != userEdit {
				t.Fatalf("dialog closure was confused with saved-window layout intent: cancelled=%t userEdit=%t", runtime.restoreCancelled, userEdit)
			}
			workspace.Nodes = append(workspace.Nodes, window)
			if _, err := runtime.Reconcile(root, start.Add(12*time.Second)); err != nil {
				t.Fatal(err)
			}
			mark, _ := app.ID.Mark()
			window.Marks = []string{mark}
			before := len(requester.commands)
			if _, err := runtime.Reconcile(root, start.Add(13*time.Second)); err != nil {
				t.Fatal(err)
			}
			structural := false
			for _, command := range requester.commands[before:] {
				structural = structural || strings.Contains(command, sessionstate.RestoreStagingWorkspace) || strings.Contains(command, "layout ")
			}
			if structural == userEdit {
				t.Fatalf("late application lost saved/user layout decision: structural=%t userEdit=%t commands=%q", structural, userEdit, requester.commands[before:])
			}
		})
	}
}

func TestSessionRuntimeForeignQueuedClosuresPreserveSavedIntent(t *testing.T) {
	runtime, _, _, terminal, _, start := delayedApplicationScenario(t)
	root := daemonTree("98", terminal)
	workspace := root.Nodes[0].Nodes[0]
	appID := "org.example.Authentication"
	first := &Node{ID: 43, Type: "con", AppID: &appID, Focused: true}
	second := &Node{ID: 44, Type: "con", AppID: &appID, Focused: true}
	now := start.Add(11 * time.Second)
	workspace.FloatingNodes = []*Node{first}
	workspace.Focus = []int64{first.ID, terminal.ID}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: first}, now)
	if _, err := runtime.Reconcile(root, now); err != nil {
		t.Fatal(err)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: first}, now)
	first.Focused = false
	workspace.FloatingNodes = []*Node{first, second}
	workspace.Focus = []int64{second.ID, first.ID, terminal.ID}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: second}, now)
	if _, err := runtime.Reconcile(root, now); err != nil {
		t.Fatal(err)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: second}, now)
	if runtime.restoreCancelled {
		t.Fatal("ordinary foreign mappings cancelled startup")
	}
	// Both clients have closed before the consumer fetches its next GET_TREE.
	// The lossless event stream still contains close(B), focus(A), close(A), focus(T).
	workspace.FloatingNodes = nil
	workspace.Focus = []int64{terminal.ID}
	terminal.Focused = true
	firstClose := *first
	firstClose.Focused = true
	for _, event := range []swayipc.Event{
		{Type: swayipc.EventWindow, Change: "close", Container: second},
		{Type: swayipc.EventWindow, Change: "focus", Container: &firstClose},
		{Type: swayipc.EventWindow, Change: "close", Container: &firstClose},
		{Type: swayipc.EventWindow, Change: "focus", Container: terminal},
	} {
		runtime.HandleEvent(event, now)
		if runtime.restoreCancelled {
			t.Fatalf("back-to-back automatic foreign closures cancelled saved intent at %s(%d)", event.Change, event.Container.ID)
		}
		if _, err := runtime.Reconcile(root, now); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSessionRuntimeForeignQueuedEventAfterStreamLoss(t *testing.T) {
	for _, change := range []string{"focus", "move", "close"} {
		t.Run(change, func(t *testing.T) {
			runtime, _, _, terminal, _, start := delayedApplicationScenario(t)
			guard := &mutableEventStreamGuard{epoch: 1, connected: true}
			runtime.eventStreamReady, runtime.eventStreamEpoch = true, 1
			runtime.eventStreamState = guard
			root := daemonTree("98", terminal)
			workspace := root.Nodes[0].Nodes[0]
			appID := "org.example.Authentication"
			prompt := &Node{ID: 43, Type: "con", AppID: &appID, Focused: true}
			workspace.FloatingNodes = []*Node{prompt}
			workspace.Focus = []int64{prompt.ID, terminal.ID}
			now := start.Add(11 * time.Second)
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: prompt}, now)
			if _, err := runtime.Reconcile(root, now); err != nil {
				t.Fatal(err)
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: prompt}, now)
			if runtime.restoreCancelled {
				t.Fatal("validated foreign map cancelled restore")
			}
			// The producer publishes disconnection synchronously before its
			// lifecycle event can reach a busy consumer. An older window event
			// already in the queue must not invent manual user intent.
			guard.epoch, guard.connected = 2, false
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: change, Container: prompt}, now)
			if !runtime.restoreCancelled || runtime.restoreCancellationReason != "observation_unavailable" {
				t.Fatalf("synchronous stream loss was attributed to queued foreign activity: cancelled=%t reason=%q", runtime.restoreCancelled, runtime.restoreCancellationReason)
			}
		})
	}
}
