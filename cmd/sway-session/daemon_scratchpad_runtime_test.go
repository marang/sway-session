package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func scratchpadRuntimeFixture(t *testing.T, failure error) (*sessionRuntime, *recordingRequester, *Node, sessionstate.LayoutSnapshot, time.Time) {
	t.Helper()
	old, _, _, first, start := testApplicationRuntime(t)
	second := first
	second.ID = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	app := *first.App
	second.App = &app
	second.App.Identity.WaylandAppID = "org.example.Other"
	second.App.Identity.SandboxAppID = "org.example.Other"
	second.Launcher.FlatpakID = "org.example.Other"
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{first, second}}
	if err := sessionstate.RegistryStoreFor(old.root).Save(registry); err != nil {
		t.Fatal(err)
	}
	desired := sessionstate.LayoutSnapshot{
		Version:    sessionstate.LayoutSchemaVersion,
		Workspaces: []sessionstate.WorkspaceLayout{{Name: "98", RestoreMode: sessionstate.WorkspaceRestorePlacementOnly, PlacementContexts: []sessionstate.ContextID{second.ID}}},
		Scratchpad: []sessionstate.ScratchpadPlacement{{ContextID: first.ID}},
	}
	if err := sessionstate.LayoutStoreFor(old.root).Save(desired); err != nil {
		t.Fatal(err)
	}
	requester := &recordingRequester{failAt: 1, failure: failure}
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Root: old.root, StartedAt: start, CompositorID: strings.Repeat("f", 64),
		ApplicationLauncher: &recordingApplicationLauncher{},
		ApplicationRestore:  sessionstate.ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstID, secondID := first.App.Identity.WaylandAppID, second.App.Identity.WaylandAppID
	root := daemonTree("99", &Node{ID: 51, Type: "con", AppID: &firstID, Focused: true}, &Node{ID: 52, Type: "con", AppID: &secondID})
	root.Nodes[0].Nodes[0].Focus = []int64{51, 52}
	return runtime, requester, root, desired, start
}

func TestScratchpadRuntimeRejectedMoveDoesNotStarveAnotherApplication(t *testing.T) {
	runtime, requester, root, desired, start := scratchpadRuntimeFixture(t, errors.New("explicit rejection"))
	refresh, err := runtime.Reconcile(root, start)
	if !refresh || err == nil {
		t.Fatalf("confirmed rejection must degrade independently: refresh=%v error=%v", refresh, err)
	}
	if len(requester.commands) != 3 || !strings.Contains(requester.commands[0], "move scratchpad") || !strings.Contains(requester.commands[1], "con_id=52") || !strings.Contains(requester.commands[2], string(desired.Workspaces[0].PlacementContexts[0])) {
		t.Fatalf("rejected scratchpad move starved normal placement or marked failed context: %v", requester.commands)
	}
	var stored sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(runtime.root).LoadInto(&stored); err != nil || len(stored.Scratchpad) != 1 {
		t.Fatalf("rejected move lost durable scratchpad intent: %+v, %v", stored, err)
	}
}

func TestScratchpadRuntimeUnknownMoveWaitsForObservation(t *testing.T) {
	unknown := &swayipc.CommandOutcomeUnknownError{Cause: errors.New("lost acknowledgement")}
	runtime, requester, root, _, start := scratchpadRuntimeFixture(t, unknown)
	refresh, err := runtime.Reconcile(root, start)
	if !refresh || !errors.As(err, &unknown) || len(requester.commands) != 1 || !strings.Contains(requester.commands[0], "move scratchpad") {
		t.Fatalf("unknown move was followed by effects: refresh=%v commands=%v error=%v", refresh, requester.commands, err)
	}
}

func TestScratchpadRuntimeUserCancellationOnlyAdoptsCurrentPlacement(t *testing.T) {
	runtime, requester, root, _, start := scratchpadRuntimeFixture(t, nil)
	requester.failAt = 0
	runtime.cancelConflictingRestore()
	if _, err := runtime.Reconcile(root, start); err != nil {
		t.Fatal(err)
	}
	for _, command := range requester.commands {
		if strings.Contains(command, "scratchpad") {
			t.Fatalf("user cancellation was overridden by saved scratchpad intent: %v", requester.commands)
		}
	}
}

func TestScratchpadCapturePreservesLifecycleReservation(t *testing.T) {
	id := testManagedContextID
	previous := sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}, Scratchpad: []sessionstate.ScratchpadPlacement{{ContextID: id}}}
	runtime := &sessionRuntime{persisted: previous, lifecycleBlocked: map[sessionstate.ContextID]struct{}{id: {}}}
	candidate := sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}, Scratchpad: []sessionstate.ScratchpadPlacement{{ContextID: id, Visible: true, Workspace: "98"}}}
	protected, safe := runtime.preserveLifecycleCapture(candidate)
	if !safe || len(protected.Scratchpad) != 1 || protected.Scratchpad[0].Visible || len(candidate.Scratchpad) != 1 || !candidate.Scratchpad[0].Visible {
		t.Fatalf("reserved scratchpad state was replaced or input mutated: safe=%v result=%+v candidate=%+v", safe, protected, candidate)
	}
	// A normal-workspace capture cannot safely coexist with the retained
	// scratchpad intent for the same reserved identity. Defer the write.
	conflict := placementOnlySnapshot("99", id)
	if _, safe := runtime.preserveLifecycleCapture(conflict); safe {
		t.Fatal("reserved context was persisted in two placements")
	}
	runtime.lifecycleBlocked = nil
	protected, safe = runtime.preserveLifecycleCapture(candidate)
	if !safe || !protected.Scratchpad[0].Visible {
		t.Fatal("completed reservation kept blocking live scratchpad capture")
	}
}

func TestScratchpadHideAttributesFocusBeforeMove(t *testing.T) {
	runtime, _, root, action := newScratchpadShowScenario(t)
	leaf := root.Nodes[1].Nodes[0].FloatingNodes[0]
	root.Nodes = root.Nodes[:1]
	workspace := root.Nodes[0].Nodes[0]
	workspace.Nodes[0].Focused = false
	leaf.Focused, leaf.ScratchpadState = true, "none"
	workspace.Nodes = append(workspace.Nodes, leaf)
	workspace.Focus = []int64{leaf.ID, 41}
	action.Kind, action.Workspace = sessionstate.PlacementMoveScratchpad, ""
	if err := runtime.applyPlannedPlacementAction(root, action); err != nil {
		t.Fatal(err)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: workspace.Nodes[0], StreamEpoch: 1}, time.Now())
	if runtime.restoreCancelled || len(runtime.expectedFocus) != 0 {
		t.Fatal("own scratchpad hide focus before move cancelled restore")
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding, StreamEpoch: 1}, time.Now())
	if !runtime.restoreCancelled {
		t.Fatal("real user binding did not retain precedence")
	}
}

func TestScratchpadShowAcceptsRealFloatingLeaf(t *testing.T) {
	runtime, _, root, action := newScratchpadShowScenario(t)
	root.Nodes[1].Nodes[0].FloatingNodes[0].Type = "floating_con"
	root.Nodes[0].Nodes[1].FullscreenMode = 1
	if err := runtime.showRestoredScratchpad(root, action); err != nil {
		t.Fatalf("real Sway hidden leaf was rejected: %v", err)
	}
}

func TestScratchpadLateApplicationStillRestoresAfterStartupDeadline(t *testing.T) {
	runtime, requester, late, _, start := scratchpadRuntimeFixture(t, nil)
	requester.failAt = 0
	enableApplicationLaunchFixture(runtime, start.Add(time.Second))
	empty := daemonTree("99")
	if _, err := runtime.Reconcile(empty, start); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Reconcile(empty, start.Add(sessionStartupSettleDelay)); err != nil {
		t.Fatal(err)
	}
	if !runtime.startupComplete {
		t.Fatal("startup did not finish with absent applications")
	}
	before := len(requester.commands)
	if _, err := runtime.Reconcile(late, start.Add(sessionStartupSettleDelay+time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, command := range requester.commands[before:] {
		if strings.Contains(command, "con_id=51") && strings.Contains(command, "move scratchpad") {
			return
		}
	}
	t.Fatalf("late mapping lost saved scratchpad intent: %q", requester.commands[before:])
}

func TestScratchpadExternalMoveCancelsSavedShowIncludingLateMapping(t *testing.T) {
	for _, late := range []bool{false, true} {
		runtime, requester, root, action := newScratchpadShowScenario(t)
		runtime.restoreProgress = nil
		runtime.startupComplete = late
		leaf := root.Nodes[1].Nodes[0].FloatingNodes[0]
		runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "move", Container: leaf, StreamEpoch: 1}, time.Now())
		if !runtime.restoreCancelled {
			t.Fatalf("external hide did not cancel saved show; late=%t", late)
		}
		placement := runtime.desired
		placement.Workspaces, placement.Scratchpad = []sessionstate.WorkspaceLayout{}, nil
		groups, err := sessionstate.ObserveApplicationGroups(root, runtime.registry)
		if err != nil {
			t.Fatal(err)
		}
		actions, err := sessionstate.PlanApplicationPlacementActions(groups, placement)
		if err != nil || len(actions) != 1 || actions[0].Kind != sessionstate.PlacementAddMark || actions[0].ContextID != action.ContextID {
			t.Fatalf("cancelled live placement was not adopted: %+v %v", actions, err)
		}
		if err := runtime.applyPlannedPlacementAction(root, actions[0]); err != nil {
			t.Fatal(err)
		}
		for _, command := range requester.commands {
			if strings.Contains(command, "scratchpad") {
				t.Fatalf("external hide was overridden: %q", requester.commands)
			}
		}
	}
}
