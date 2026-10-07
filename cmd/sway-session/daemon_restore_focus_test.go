package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/statefile"
	"github.com/marang/sway-session/internal/swayipc"
)

// focusedMoveCompositor models the measured Sway sequence: moving the focused
// leaf emits move(mover), focus(next source focus-stack leaf), then the tick.
type focusedMoveCompositor struct{ *cleanupCompositor }

func (c *focusedMoveCompositor) RequestContext(ctx context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if kind != swayipc.RunCommand || !strings.Contains(string(payload), "move container to workspace") {
		return c.cleanupCompositor.RequestContext(ctx, kind, payload)
	}
	before := len(c.events)
	reply, err := c.cleanupCompositor.RequestContext(ctx, kind, payload)
	if err != nil || len(c.events) == before {
		return reply, err
	}
	mover := c.events[before].Container
	c.events = c.events[:before+1]
	for _, ws := range c.root.Nodes[0].Nodes {
		ws.Focus = slices.DeleteFunc(ws.Focus, func(id int64) bool {
			return !slices.ContainsFunc(ws.Nodes, func(n *Node) bool { return n.ID == id })
		})
		if slices.Contains(ws.Nodes, mover) && !slices.Contains(ws.Focus, mover.ID) {
			ws.Focus = append(ws.Focus, mover.ID)
		}
	}
	if mover.Focused {
		mover.Focused = false
		ws := c.root.Nodes[0].Nodes[0]
		for _, id := range ws.Focus {
			for _, node := range ws.Nodes {
				if node.ID == id {
					node.Focused = true
					c.events = append(c.events, swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: node})
					return reply, nil
				}
			}
		}
	}
	return reply, nil
}

func TestSessionRuntimeOwnStagingFocusContinuesReconstruction(t *testing.T) {
	runtime, c, now := newCleanupScenario(t)
	ws := c.root.Nodes[0].Nodes[0]
	ws.Focus = []int64{41, 42}
	findContainerByID(c.root, 41).Focused = true
	runtime.client = &focusedMoveCompositor{c}
	c.drain(runtime, now)
	if _, err := runtime.Reconcile(c.root, now); err != nil {
		t.Fatal(err)
	}
	c.drain(runtime, now)
	if _, err := runtime.Reconcile(c.root, now); err != nil {
		t.Fatal(err)
	}
	for _, workspace := range c.root.Nodes[0].Nodes {
		if workspace.Name == sessionstate.RestoreStagingWorkspace && len(workspace.Nodes) == 2 {
			return
		}
	}
	t.Fatal("own staging focus cancelled tabbed reconstruction instead of staging the second window")
}

func TestSessionRuntimePlacementReobservesBeforeNextFocusedMove(t *testing.T) {
	runtime, c, now := newCleanupScenario(t)
	ws := c.root.Nodes[0].Nodes[0]
	ws.Name, ws.Focus = "99", []int64{41, 42}
	for _, node := range ws.Nodes {
		node.Marks = nil
	}
	findContainerByID(c.root, 41).Focused = true
	runtime.client = &focusedMoveCompositor{c}
	c.drain(runtime, now)
	for pass := range 2 {
		before := len(c.commands)
		if _, err := runtime.Reconcile(c.root, now); err != nil {
			t.Fatal(err)
		}
		moves := 0
		for _, command := range c.commands[before:] {
			if strings.Contains(command, "move container") {
				moves++
			}
		}
		if moves != 1 {
			t.Fatalf("pass %d performed %d moves without fresh observation", pass, moves)
		}
		c.drain(runtime, now)
		if runtime.startupComplete {
			t.Fatal("placement focus feedback cancelled startup")
		}
	}
	for _, workspace := range c.root.Nodes[0].Nodes {
		if workspace.Name == "98" && len(workspace.Nodes) == 2 {
			return
		}
	}
	t.Fatal("placement did not progress after yielding for observation")
}

func newFocusCommandScenario(t *testing.T) (*sessionRuntime, *recordingRequester, *Node) {
	t.Helper()
	requester := &recordingRequester{}
	runtime := &sessionRuntime{
		client: requester, eventStreamReady: true, eventStreamEpoch: 1,
		restoreProgress: &sessionstate.RestoreProgress{Workspace: "98", Phase: sessionstate.RestoreBuild},
	}
	root := daemonTree("98", &Node{ID: 41, Type: "con", Focused: true}, &Node{ID: 42, Type: "con"})
	root.Nodes[0].Nodes[0].Focus = []int64{41, 42}
	root.Nodes[0].Nodes = append(root.Nodes[0].Nodes, &Node{ID: 4, Type: "workspace", Name: "99", Nodes: []*Node{{ID: 43, Type: "con"}}})
	return runtime, requester, root
}

func TestSessionRuntimeOwnExplicitFocusConsumesWorkspaceThenWindow(t *testing.T) {
	runtime, requester, root := newFocusCommandScenario(t)
	if err := runtime.applyRestoreAction(root, sessionstate.RestoreAction{Kind: sessionstate.RestoreFocus, ContainerID: 43}); err != nil {
		t.Fatal(err)
	}
	if len(requester.barriers) != 1 {
		t.Fatal("focus command did not establish an ordering barrier")
	}
	// Deliver the measured sequence well after issuance: elapsed wall time is
	// not an attribution mechanism. The stream ordering barrier is.
	now := time.Now().Add(time.Hour)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWorkspace, Change: "focus", Old: &Node{ID: 3}, Current: &Node{ID: 4}}, now)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: 43}}, now)
	if runtime.restoreProgress == nil || runtime.originalFocusDone {
		t.Fatal("own focus feedback suppressed remaining restore or original focus")
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventTick, Payload: requester.barriers[0]}, now)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: 43}}, now)
	if runtime.restoreProgress == nil {
		t.Fatal("window focus outside the command cancelled restore")
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWorkspace, Change: "focus", Old: &Node{ID: 3}, Current: &Node{ID: 4}}, now)
	if runtime.restoreProgress != nil {
		t.Fatal("workspace focus outside the command was swallowed")
	}
}

func TestSessionRuntimeFocusAttributionDoesNotHideUserIntent(t *testing.T) {
	for _, name := range []string{"binding", "wrong-workspace", "expired", "reconnect", "epoch-change", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			runtime, requester, root := newFocusCommandScenario(t)
			if err := runtime.applyRestoreAction(root, sessionstate.RestoreAction{Kind: sessionstate.RestoreFocus, ContainerID: 43}); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			event := swayipc.Event{Type: swayipc.EventWorkspace, Change: "focus", Old: &Node{ID: 3}, Current: &Node{ID: 4}}
			switch name {
			case "binding":
				event = swayipc.Event{Type: swayipc.EventBinding}
			case "wrong-workspace":
				event = swayipc.Event{Type: swayipc.EventWorkspace, Change: "focus", Old: &Node{ID: 3}, Current: &Node{ID: 5}}
			case "expired":
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventTick, Payload: requester.barriers[0]}, now)
				// This is the exact first expected tuple, so a missing expiry
				// cannot pass merely because window/workspace order mismatches.
				event = swayipc.Event{Type: swayipc.EventWorkspace, Change: "focus", Old: &Node{ID: 3}, Current: &Node{ID: 4}}
			case "reconnect":
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 2}, now)
			case "epoch-change":
				runtime.eventStreamState = &mutableEventStreamGuard{epoch: 2, connected: true}
			case "duplicate":
				workspace := swayipc.Event{Type: swayipc.EventWorkspace, Change: "focus", Old: &Node{ID: 3}, Current: &Node{ID: 4}}
				runtime.HandleEvent(workspace, now)
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: 43}}, now)
				event = workspace
			}
			runtime.HandleEvent(event, now)
			if runtime.restoreProgress != nil || len(runtime.expectedFocus) != 0 {
				t.Fatal("independent user activity retained structural work or stale focus allowances")
			}
		})
	}
}

func TestSessionRuntimeStagingFocusRequiresPrecedingOwnMove(t *testing.T) {
	runtime, _, root := newFocusCommandScenario(t)
	if err := runtime.applyRestoreAction(root, sessionstate.RestoreAction{Kind: sessionstate.RestoreMoveWorkspace, ContainerID: 41, Target: sessionstate.RestoreStagingWorkspace}); err != nil {
		t.Fatal(err)
	}
	focus := swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: 42}}
	if runtime.consumeRestoreFocus(focus) {
		t.Fatal("focus arriving before the own move event was attributed to it")
	}
	if !runtime.consumeExpectedMove(41) || !runtime.consumeRestoreFocus(focus) {
		t.Fatal("ordered own move did not release its predicted focus")
	}
}

func TestSessionRuntimeFailedFocusCannotConsumeLaterUserFocus(t *testing.T) {
	for _, failure := range []error{errors.New("rejected"), &swayipc.CommandOutcomeUnknownError{Cause: errors.New("connection lost")}, &swayipc.CommandResponseInvalidError{Cause: errors.New("invalid reply")}} {
		runtime, requester, root := newFocusCommandScenario(t)
		requester.failAt, requester.failure = 1, failure
		if err := runtime.applyRestoreAction(root, sessionstate.RestoreAction{Kind: sessionstate.RestoreFocus, ContainerID: 43}); !errors.Is(err, failure) {
			t.Fatalf("unexpected command error: %v", err)
		}
		if len(runtime.expectedFocus) != 0 {
			t.Fatal("failed command left a focus allowance")
		}
		runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWorkspace, Change: "focus", Old: &Node{ID: 3}, Current: &Node{ID: 4}}, time.Now())
		if runtime.restoreProgress != nil {
			t.Fatal("failed command hid a later user focus")
		}
	}
}

func TestSessionRuntimeFocusUsesNestedFocusStackRatherThanChildOrder(t *testing.T) {
	runtime, _, root := newFocusCommandScenario(t)
	ws := root.Nodes[0].Nodes[0]
	ws.Nodes = append(ws.Nodes, &Node{ID: 44, Type: "con", Focus: []int64{46, 45}, Nodes: []*Node{{ID: 45, Type: "con"}, {ID: 46, Type: "con"}}})
	ws.Focus = []int64{41, 44, 42}
	if err := runtime.applyRestoreAction(root, sessionstate.RestoreAction{Kind: sessionstate.RestoreMoveWorkspace, ContainerID: 41, Target: sessionstate.RestoreStagingWorkspace}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "move", Container: &Node{ID: 41}}, now)
	if !runtime.consumeRestoreFocus(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: 46}}) {
		t.Fatal("nested focus-stack successor was not attributed")
	}
	if runtime.consumeRestoreFocus(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: 45}}) {
		t.Fatal("subsequent nested focus consumed an unrelated command allowance")
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: 45}}, now)
	if runtime.restoreProgress == nil {
		t.Fatal("window focus alone cancelled remaining restore")
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding}, now)
	if runtime.restoreProgress != nil {
		t.Fatal("subsequent user binding was swallowed")
	}
}

func TestSessionRuntimeFullscreenFocusIsBoundedByBarrier(t *testing.T) {
	for _, mode := range []sessionstate.FullscreenMode{sessionstate.FullscreenWorkspace, sessionstate.FullscreenGlobal} {
		runtime, requester, root := newFocusCommandScenario(t)
		target := int64(42)
		if mode == sessionstate.FullscreenGlobal {
			target = 43
		}
		if err := runtime.applyRestoreAction(root, sessionstate.RestoreAction{Kind: sessionstate.RestoreSetFullscreen, ContainerID: target, Fullscreen: mode}); err != nil {
			t.Fatal(err)
		}
		if len(requester.barriers) != 1 {
			t.Fatal("fullscreen focus effect has no barrier")
		}
		now := time.Now()
		if mode == sessionstate.FullscreenGlobal {
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWorkspace, Change: "focus", Old: &Node{ID: 3}, Current: &Node{ID: 4}}, now)
		}
		runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: target}}, now)
		if runtime.restoreProgress == nil {
			t.Fatal("own fullscreen focus cancelled restore")
		}
		runtime.HandleEvent(swayipc.Event{Type: swayipc.EventTick, Payload: requester.barriers[0]}, now)
		runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: 41}}, now)
		if runtime.restoreProgress == nil {
			t.Fatal("fullscreen followed by window focus cancelled remaining restore")
		}
		runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWorkspace, Change: "focus", Old: &Node{ID: 3}, Current: &Node{ID: 4}}, now)
		if runtime.restoreProgress != nil {
			t.Fatal("fullscreen swallowed later workspace focus")
		}
	}
}

type failedFocusBarrierRequester struct{ recordingRequester }

func (r *failedFocusBarrierRequester) RequestContext(ctx context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if kind == swayipc.SendTick {
		return swayipc.Message{}, errors.New("tick reply lost")
	}
	return r.recordingRequester.RequestContext(ctx, kind, payload)
}

func TestSessionRuntimeFocusBarrierFailureCancelsAndInvalidatesAttribution(t *testing.T) {
	runtime, _, root := newFocusCommandScenario(t)
	runtime.client = &failedFocusBarrierRequester{}
	err := runtime.applyRestoreAction(root, sessionstate.RestoreAction{Kind: sessionstate.RestoreFocus, ContainerID: 43})
	var unknown *swayipc.CommandOutcomeUnknownError
	if !errors.As(err, &unknown) || runtime.restoreProgress != nil || len(runtime.expectedFocus) != 0 {
		t.Fatalf("lost barrier retained unsafe restoration: err=%v, progress=%+v, expectations=%+v", err, runtime.restoreProgress, runtime.expectedFocus)
	}
}

func TestSessionRuntimeFocusAttributionBacklogIsBounded(t *testing.T) {
	runtime, requester, root := newFocusCommandScenario(t)
	for range 64 {
		if err := runtime.applyRestoreAction(root, sessionstate.RestoreAction{Kind: sessionstate.RestoreFocus, ContainerID: 43}); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.applyRestoreAction(root, sessionstate.RestoreAction{Kind: sessionstate.RestoreFocus, ContainerID: 43}); err == nil {
		t.Fatal("unconsumed focus allowances were unbounded")
	}
	if len(requester.commands) != 64 || len(runtime.expectedFocus) != 0 || runtime.restoreProgress != nil {
		t.Fatal("overflow did not stop before issuing a further command")
	}
}

func TestSessionRuntimeGlobalFullscreenGroupKeepsRemainingRestore(t *testing.T) {
	runtime, requester, root := newFocusCommandScenario(t)
	ws := root.Nodes[0].Nodes[1]
	ws.Nodes = []*Node{{ID: 44, Type: "con", Layout: "tabbed", Focus: []int64{43, 45}, Nodes: []*Node{{ID: 43, Type: "con"}, {ID: 45, Type: "con"}}}}
	if err := runtime.applyRestoreAction(root, sessionstate.RestoreAction{Kind: sessionstate.RestoreSetFullscreen, ContainerID: 44, Fullscreen: sessionstate.FullscreenGlobal}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// A fullscreen group has no view: Sway emits the workspace transition,
	// without a window-focus event for either the group or its descendants.
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWorkspace, Change: "focus", Old: &Node{ID: 3}, Current: &Node{ID: 4}}, now)
	if runtime.restoreProgress == nil {
		t.Fatal("own fullscreen group focus cancelled remaining reconstruction")
	}
	if len(requester.barriers) != 1 {
		t.Fatal("fullscreen group focus has no ordering barrier")
	}
	// Descendant window focus alone remains irrelevant. A new workspace
	// transition must still override the finished command's allowance.
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: 43}}, now)
	if runtime.restoreProgress == nil {
		t.Fatal("fullscreen group descendant focus cancelled remaining restore")
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWorkspace, Change: "focus", Old: &Node{ID: 4}, Current: &Node{ID: 3}}, now)
	if runtime.restoreProgress != nil {
		t.Fatal("fullscreen group hid a later workspace transition")
	}
}

func newMappingFocusScenario(t *testing.T) (*sessionRuntime, *cleanupCompositor, *Node, time.Time) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	secondID := sessionstate.ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if err := sessionstate.RegistryStoreFor(state).Save(sessionRegistryIDs(testManagedContextID, secondID)); err != nil {
		t.Fatal(err)
	}
	desired := exactDaemonSnapshot("98", testManagedContextID, secondID)
	desired.Workspaces[0].Tiling.Layout = sessionstate.LayoutTabbed
	if err := sessionstate.LayoutStoreFor(state).Save(desired); err != nil {
		t.Fatal(err)
	}
	c := &cleanupCompositor{t: t, root: daemonTree("98")}
	runtime, err := newSessionRuntimeWithOptions(c, sessionRuntimeOptions{Root: state})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := runtime.Reconcile(c.root, now); err != nil {
		t.Fatal(err)
	}
	leaf := managedDaemonLeaf(t, 41, testManagedContextID)
	leaf.Marks, leaf.Focused = nil, true
	c.root.Nodes[0].Nodes[0].Nodes = []*Node{leaf}
	c.root.Nodes[0].Nodes[0].Focus = []int64{41}
	return runtime, c, leaf, now
}

func TestSessionRuntimeNewMappingFocusKeepsStartupAlive(t *testing.T) {
	runtime, c, leaf, now := newMappingFocusScenario(t)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: leaf}, now)
	if _, err := runtime.Reconcile(c.root, now); err != nil {
		t.Fatal(err)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: leaf}, now)
	c.drain(runtime, now)
	if runtime.startupComplete || runtime.restoreCancelled {
		t.Fatal("automatic map focus cancelled startup while another saved window was still missing")
	}
}

func TestSessionRuntimeMappingFocusSurvivesConcurrentCLIRestoreLock(t *testing.T) {
	for _, binding := range []bool{false, true} {
		t.Run(fmt.Sprintf("binding_%t", binding), func(t *testing.T) {
			runtime, c, leaf, now := newMappingFocusScenario(t)
			coordinator, err := sessionstate.NewApplicationRestoreCoordinator(strings.Repeat("a", 64), sessionstate.ApplicationSessionState{}, now, sessionstate.ApplicationRestoreOptions{
				AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: time.Second, MaxConcurrent: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			runtime.applications = coordinator
			runtime.applicationPersistedState = coordinator.State()
			entered, release := make(chan struct{}), make(chan struct{})
			locked := make(chan error, 1)
			go func() {
				locked <- sessionstate.WithRegistryLockContext(t.Context(), runtime.root, func(*statefile.LockedPrivateDirectory) error {
					close(entered)
					<-release
					return nil
				})
			}()
			<-entered
			unlocked := false
			t.Cleanup(func() {
				if !unlocked {
					close(release)
					<-locked
				}
			})
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: leaf}, now)
			before := len(c.commands)
			if refresh, err := runtime.Reconcile(c.root, now); err == nil || refresh {
				t.Fatalf("application pass must wait for the CLI registry lock: refresh=%t err=%v", refresh, err)
			}
			if len(c.commands) != before || len(runtime.restoreEligible) != 0 {
				t.Fatal("focus observation bypassed lifecycle effect locking")
			}
			if binding {
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding}, now)
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "title", Container: leaf}, now)
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: leaf}, now)
			if runtime.restoreCancelled != binding {
				t.Fatalf("automatic mapping focus cancelled restore while its identity was already observable: cancelled=%t binding=%t", runtime.restoreCancelled, binding)
			}
			close(release)
			if err := <-locked; err != nil {
				t.Fatal(err)
			}
			unlocked = true
			if _, err := runtime.Reconcile(c.root, now); err != nil {
				t.Fatal(err)
			}
			if _, adopted := runtime.restoreEligible[testManagedContextID]; !adopted || len(leaf.Marks) == 0 || runtime.restoreCancelled != binding {
				t.Fatal("normal adoption did not resume with the original cancellation policy")
			}
			if err := runtime.Flush(now.Add(sessionSnapshotDebounce)); err != nil {
				t.Fatal(err)
			}
			var saved sessionstate.LayoutSnapshot
			if err := sessionstate.LayoutStoreFor(runtime.root).LoadInto(&saved); err != nil {
				t.Fatal(err)
			}
			if len(saved.Workspaces) != 1 || saved.Workspaces[0].Tiling.Layout != sessionstate.LayoutTabbed {
				t.Fatalf("lost original tabbed target while the other terminal is still missing: %+v", saved)
			}
		})
	}
}

func TestSessionRuntimeQueuedAndRepeatedWindowFocusKeepsRestoreAlive(t *testing.T) {
	runtime, c, first, now := newMappingFocusScenario(t)
	second := managedDaemonLeaf(t, 42, "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	second.Marks = nil
	c.root.Nodes[0].Nodes[0].Nodes = append(c.root.Nodes[0].Nodes[0].Nodes, second)
	// The first new event reconciles a tree in which both windows already
	// exist. Both are adopted before the second new event is processed.
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: first}, now)
	if _, err := runtime.Reconcile(c.root, now); err != nil {
		t.Fatal(err)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: first}, now)
	c.drain(runtime, now)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: second}, now)
	for _, leaf := range []*Node{second, first, second} {
		runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: leaf}, now)
		if runtime.restoreCancelled {
			t.Fatal("queued or repeated saved-window focus cancelled restoration")
		}
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding}, now)
	if !runtime.restoreCancelled {
		t.Fatal("window focus hid a subsequent binding")
	}
}

func TestSessionRuntimeRestoreWindowObservationBeyondPlacementBatch(t *testing.T) {
	runtime, c, leaf, _ := newMappingFocusScenario(t)
	registry := runtime.registry
	for index := range 260 {
		id := sessionstate.ContextID(fmt.Sprintf("00000000-0000-4000-8000-%012d", index))
		item := sessionRegistry(id).Contexts[0]
		item.Launcher.Session = fmt.Sprintf("batch-session-%d", index)
		registry.Contexts = append(registry.Contexts, item)
		node := managedDaemonLeaf(t, int64(1000+index), id)
		node.Marks = nil
		c.root.Nodes[0].Nodes[0].Nodes = append(c.root.Nodes[0].Nodes[0].Nodes, node)
	}
	actions, err := sessionstate.PlanPlacementActions(c.root, registry, runtime.desired)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(actions, func(action sessionstate.PlacementAction) bool { return action.ContainerID == leaf.ID }) {
		t.Fatal("fixture did not place the saved mapping beyond the bounded command batch")
	}
	owned, err := runtime.observeRestoreWindows(c.root, registry)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := owned[leaf.ID]; !exists || len(owned) != 261 {
		t.Fatal("full-tree ownership observation omitted registered views beyond the command batch")
	}
	if len(runtime.restoreEligible) != 0 || len(c.commands) != 0 {
		t.Fatal("read-only ownership observation adopted windows or performed effects")
	}
}

func TestSessionRuntimeRestoreWindowObservationUsesRegisteredOwnership(t *testing.T) {
	for _, name := range []string{"active", "archived", "ambiguous", "marked", "reopened", "unsaved", "unregistered"} {
		t.Run(name, func(t *testing.T) {
			runtime, c, leaf, _ := newMappingFocusScenario(t)
			registry := runtime.registry
			switch name {
			case "archived":
				registry.Contexts[0].State = sessionstate.ContextArchived
			case "ambiguous":
				duplicate := *leaf
				duplicate.ID++
				c.root.Nodes[0].Nodes[0].Nodes = append(c.root.Nodes[0].Nodes[0].Nodes, &duplicate)
			case "marked":
				mark, err := testManagedContextID.Mark()
				if err != nil {
					t.Fatal(err)
				}
				leaf.Marks = []string{mark}
			case "reopened":
				runtime.restoreEligible[testManagedContextID] = struct{}{}
			case "unsaved":
				runtime.desired = exactDaemonSnapshot("98", registry.Contexts[1].ID)
			case "unregistered":
				identity := "org.example.Unregistered"
				leaf.AppID = &identity
			}
			before := len(c.commands)
			owned, err := runtime.observeRestoreWindows(c.root, registry)
			if err != nil {
				t.Fatal(err)
			}
			_, present := owned[leaf.ID]
			wantOwned := name != "archived" && name != "ambiguous" && name != "unregistered"
			if present != wantOwned {
				t.Fatalf("ownership %s selected=%t, want %t", name, present, wantOwned)
			}
			if len(c.commands) != before || runtime.restoreCancelled {
				t.Fatal("ownership observation issued effects or cancelled restore")
			}
		})
	}
}

func TestSessionRuntimeRestoreWindowObservationRejectsApplicationParentMark(t *testing.T) {
	runtime, _, _, item, _ := testApplicationRuntime(t)
	leaf := applicationLaunchWindow(item, 41)
	mark, err := item.ID.Mark()
	if err != nil {
		t.Fatal(err)
	}
	parent := &Node{ID: 42, Type: "con", Layout: "splith", Marks: []string{mark}, Nodes: []*Node{leaf}}
	tree := daemonTree("98: apps", parent)
	groups, err := sessionstate.ObserveApplicationGroups(tree, runtime.registry)
	if err != nil || groups[item.ID].Ambiguous || groups[item.ID].Anchor == nil || groups[item.ID].AnchorMarked {
		t.Fatalf("fixture must expose one unmarked application anchor: %+v err=%v", groups[item.ID], err)
	}
	_, issues, err := sessionstate.ObserveManagedWindowsIsolated(tree, runtime.registry)
	if err != nil || len(issues) != 1 || issues[0].ContextID != item.ID {
		t.Fatalf("fixture must reject the context mark on its layout parent: %+v err=%v", issues, err)
	}
	owned, err := runtime.observeRestoreWindows(tree, runtime.registry)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := owned[leaf.ID]; exists {
		t.Fatal("application anchor selection hid a malformed managed parent identity")
	}
}

func TestSessionRuntimeRestoreWindowObservationSelectsApplicationAnchor(t *testing.T) {
	for _, name := range []string{"unique-unmarked", "marked-with-secondary", "ambiguous", "archived", "desired-closed"} {
		t.Run(name, func(t *testing.T) {
			runtime, _, _, app, _ := testApplicationRuntime(t)
			leaf := applicationLaunchWindow(app, 41)
			root := daemonTree("98", leaf)
			switch name {
			case "marked-with-secondary":
				mark, _ := app.ID.Mark()
				leaf.Marks = []string{mark}
				root.Nodes[0].Nodes[0].Nodes = append(root.Nodes[0].Nodes[0].Nodes, applicationLaunchWindow(app, 42))
			case "ambiguous":
				root.Nodes[0].Nodes[0].Nodes = append(root.Nodes[0].Nodes[0].Nodes, applicationLaunchWindow(app, 42))
			case "archived":
				runtime.registry.Contexts[0].State = sessionstate.ContextArchived
			case "desired-closed":
				runtime.registry.Contexts[0].App.DesiredOpen = false
			}
			owned, err := runtime.observeRestoreWindows(root, runtime.registry)
			if err != nil {
				t.Fatal(err)
			}
			wantOwned := name == "unique-unmarked" || name == "marked-with-secondary"
			wantCount := 0
			if wantOwned {
				wantCount = 1
			}
			_, present := owned[leaf.ID]
			if present != wantOwned || len(owned) != wantCount {
				t.Fatalf("application ownership %s selected %v, want one anchor=%t", name, owned, wantOwned)
			}
		})
	}
}

func TestSessionRuntimeMappedWindowsStillHonorIndependentUserIntent(t *testing.T) {
	for _, name := range []string{"binding-before-map", "binding-after-adoption", "workspace-focus", "saved-move", "saved-close"} {
		t.Run(name, func(t *testing.T) {
			runtime, c, leaf, now := newMappingFocusScenario(t)
			if name == "binding-before-map" {
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding}, now)
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: leaf}, now)
			if _, err := runtime.Reconcile(c.root, now); err != nil {
				t.Fatal(err)
			}
			c.drain(runtime, now)
			switch name {
			case "binding-after-adoption":
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding}, now)
			case "workspace-focus":
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWorkspace, Change: "focus", Old: &Node{ID: 3}, Current: &Node{ID: 4}}, now)
			case "saved-move":
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "move", Container: leaf}, now)
			case "saved-close":
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: leaf}, now)
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: leaf}, now)
			if !runtime.restoreCancelled || runtime.restoreProgress != nil || runtime.lateRestorePending {
				t.Fatal("window mapping/focus hid independent user intent or revived cancelled restoration")
			}
			// A later saved window may still be placed/marked, but its adoption
			// must not restart structural work after the user's cancellation.
			second := managedDaemonLeaf(t, 42, "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
			second.Marks = nil
			c.root.Nodes[0].Nodes[0].Nodes = append(c.root.Nodes[0].Nodes[0].Nodes, second)
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: second}, now)
			if _, err := runtime.Reconcile(c.root, now); err != nil {
				t.Fatal(err)
			}
			if runtime.restoreProgress != nil || runtime.lateRestorePending {
				t.Fatal("later window mapping rearmed cancelled structural work")
			}
		})
	}
}
