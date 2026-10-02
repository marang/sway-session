package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

type fixtureLifecycleAdapter struct {
	blocked    []sessionstate.ContextID
	blockedErr error
	reconcile  func(context.Context, time.Time, string, int) (sessionstate.LifecycleReconcileResult, error)
}

func (fixture *fixtureLifecycleAdapter) BlockedContextIDs(context.Context) ([]sessionstate.ContextID, error) {
	return fixture.blocked, fixture.blockedErr
}

func (fixture *fixtureLifecycleAdapter) Reconcile(ctx context.Context, now time.Time, after string, limit int) (sessionstate.LifecycleReconcileResult, error) {
	return fixture.reconcile(ctx, now, after, limit)
}

func TestLifecycleRuntimeBoundedPagingBackoffAndReservationRefresh(t *testing.T) {
	runtime, _, _, app, now := testApplicationRuntime(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runtime.ctx = ctx
	fixture := &fixtureLifecycleAdapter{blocked: []sessionstate.ContextID{app.ID}}
	runtime.lifecycleOperations = fixture
	passes := 0
	fixture.reconcile = func(got context.Context, at time.Time, cursor string, limit int) (sessionstate.LifecycleReconcileResult, error) {
		passes++
		if got != ctx || limit != 32 {
			t.Fatalf("context=%v limit=%d", got, limit)
		}
		if passes == 1 {
			if cursor != "" || !at.Equal(now) {
				t.Fatalf("first cursor=%q at=%v", cursor, at)
			}
			return sessionstate.LifecycleReconcileResult{NextID: string(app.ID), Processed: 32}, nil
		}
		if cursor != string(app.ID) {
			t.Fatalf("lost page cursor: %q", cursor)
		}
		if passes == 2 {
			return sessionstate.LifecycleReconcileResult{NextID: cursor}, errors.New("temporary transport outage")
		}
		fixture.blocked = nil
		return sessionstate.LifecycleReconcileResult{}, nil
	}
	if err := runtime.reconcileLifecycleOperations(now); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if err := runtime.reconcileLifecycleOperations(now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if passes != 1 || len(runtime.lifecycleBlocked) != 1 {
		t.Fatalf("passes=%d blocked=%v", passes, runtime.lifecycleBlocked)
	}
	if err := runtime.reconcileLifecycleOperations(now.Add(2 * time.Second)); err == nil {
		t.Fatal("lost transient error")
	}
	if err := runtime.reconcileLifecycleOperations(now.Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if passes != 2 {
		t.Fatal("event storm bypassed backoff")
	}
	if err := runtime.reconcileLifecycleOperations(now.Add(4 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if passes != 3 || len(runtime.lifecycleBlocked) != 0 || runtime.lifecycleCursor != "" {
		t.Fatalf("completion not refreshed: %+v", runtime.lifecycleBlocked)
	}
	if !runtime.ObservationDue(now.Add(6 * time.Second)) {
		t.Fatal("periodic continuation not armed")
	}
}

func TestLifecycleRuntimeReservationsExcludePlacementAndLaunchWithoutRegistryWrites(t *testing.T) {
	runtime, requester, launcher, app, now := testApplicationRuntime(t)
	runtime.lifecycleOperations = &fixtureLifecycleAdapter{blocked: []sessionstate.ContextID{app.ID}}
	appID, sandbox := app.App.Identity.WaylandAppID, app.App.Identity.SandboxAppID
	for _, tree := range []*Node{
		daemonTree("99: landing", &Node{ID: 42, Type: "con", AppID: &appID, SandboxAppID: &sandbox}),
		daemonTree("99: landing"),
	} {
		if refresh, err := runtime.Reconcile(tree, now.Add(time.Minute)); err != nil || refresh {
			t.Fatalf("refresh=%v err=%v", refresh, err)
		}
	}
	if len(requester.commands) != 0 || launcher.starts != 0 {
		t.Fatalf("reserved app effects: commands=%v launches=%v", requester.commands, launcher.contexts)
	}
	var stored sessionstate.Registry
	if err := sessionstate.RegistryStoreFor(runtime.root).LoadInto(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Contexts) != 1 || !reflect.DeepEqual(stored.Contexts[0], app) {
		t.Fatalf("persisted planning view: %+v", stored)
	}
	if runtime.registry.Contexts[0].State != sessionstate.ContextActive {
		t.Fatal("modified registry cache")
	}
}

func TestLifecycleRuntimeCapturePreservesReservedWorkspaceAndOtherCapture(t *testing.T) {
	runtime, _, _, app, now := testApplicationRuntime(t)
	other := sessionstate.ContextID("22222222-2222-4222-8222-222222222222")
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: append(sessionRegistry(other).Contexts, app)}
	if err := sessionstate.RegistryStoreFor(runtime.root).Save(registry); err != nil {
		t.Fatal(err)
	}
	previous := sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{
		{Name: "98: apps", RestoreMode: sessionstate.WorkspaceRestoreLayout, Tiling: &sessionstate.LayoutNode{ContextID: &app.ID}},
		{Name: "99: terminal", RestoreMode: sessionstate.WorkspaceRestorePlacementOnly, PlacementContexts: []sessionstate.ContextID{other}},
	}}
	if err := sessionstate.LayoutStoreFor(runtime.root).Save(previous); err != nil {
		t.Fatal(err)
	}
	runtime.persisted = previous
	runtime.desired = previous
	runtime.startupComplete = true
	runtime.lifecycleOperations = &fixtureLifecycleAdapter{blocked: []sessionstate.ContextID{app.ID}}
	if refresh, err := runtime.Reconcile(daemonTree("100: manual", managedDaemonLeaf(t, 43, other)), now); err != nil || refresh {
		t.Fatalf("refresh=%v err=%v", refresh, err)
	}
	if err := runtime.Flush(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var stored sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(runtime.root).LoadInto(&stored); err != nil {
		t.Fatal(err)
	}
	protected, exists := workspaceByName(stored, "98: apps")
	if !exists || !reflect.DeepEqual(protected, previous.Workspaces[0]) {
		t.Fatalf("lost reserved structure: %+v", stored)
	}
	if name, found := snapshotContextWorkspace(stored, other); !found || name != "100: manual" {
		t.Fatalf("unrelated capture lost: %+v", stored)
	}
}

func TestLifecycleRuntimeReservationAppearingBeforeFlushProtectsQueuedCapture(t *testing.T) {
	runtime, _, _, app, now := testApplicationRuntime(t)
	fixture := &fixtureLifecycleAdapter{}
	runtime.lifecycleOperations = fixture
	candidate := placementOnlySnapshot("99: transient", app.ID)
	if _, err := runtime.debouncer.Observe(candidate, now); err != nil {
		t.Fatal(err)
	}
	fixture.blocked = []sessionstate.ContextID{app.ID}
	if err := runtime.Flush(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var stored sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(runtime.root).LoadInto(&stored); err != nil {
		t.Fatal(err)
	}
	if name, _ := snapshotContextWorkspace(stored, app.ID); name != "98: apps" {
		t.Fatalf("queued capture overwrote reserved placement: %+v", stored)
	}
}

func TestLifecycleRuntimeReservationReadFailureStopsGenericEffects(t *testing.T) {
	runtime, requester, launcher, _, now := testApplicationRuntime(t)
	runtime.lifecycleOperations = &fixtureLifecycleAdapter{blockedErr: errors.New("database unavailable")}
	if _, err := runtime.Reconcile(daemonTree("98: apps"), now.Add(time.Minute)); err == nil {
		t.Fatal("reservation failure ignored")
	}
	if len(requester.commands) != 0 || launcher.starts != 0 {
		t.Fatal("effects after reservation read failure")
	}
}

func TestLifecycleConflictDiagnosticOffersExactRetryAndRollback(t *testing.T) {
	for _, kind := range []sessionstate.LifecycleOperationKind{sessionstate.LifecycleRegister, sessionstate.LifecycleRebind} {
		problem := lifecycleOperationDiagnostic{outcome: sessionstate.LifecycleOutcome{OperationID: string(testManagedContextID), Kind: kind, Status: "conflict", Reason: "compositor_changed"}}.Diagnostic()
		if problem.Code != "lifecycle_operation_conflict" || !strings.Contains(problem.Hint, "--retry "+string(testManagedContextID)) {
			t.Fatalf("missing retry: %+v", problem)
		}
		if !strings.Contains(problem.Hint, "--cancel "+string(testManagedContextID)) {
			t.Fatalf("wrong cancel hint: %+v", problem)
		}
	}
}

type lifecycleFixtureRequester struct {
	recordingRequester
	tree         *Node
	beforeTree   func()
	afterCommand func()
	closed       bool
}

func (client *lifecycleFixtureRequester) LifecycleCompositorID(context.Context) (string, error) {
	return "fixture-compositor", nil
}
func (client *lifecycleFixtureRequester) Close() { client.closed = true }
func (client *lifecycleFixtureRequester) RequestContext(ctx context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if kind == swayipc.GetTree {
		if client.beforeTree != nil {
			client.beforeTree()
		}
		encoded, err := json.Marshal(client.tree)
		return swayipc.Message{Type: kind, Payload: encoded}, err
	}
	result, err := client.recordingRequester.RequestContext(ctx, kind, payload)
	if err == nil && kind == swayipc.RunCommand && client.afterCommand != nil {
		client.afterCommand()
	}
	return result, err
}

func TestLifecycleRuntimeRecordedConflictDoesNotStopUnrelatedPlacement(t *testing.T) {
	runtime, _, _, app, _ := testApplicationRuntime(t)
	other := sessionstate.ContextID("22222222-2222-4222-8222-222222222222")
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: append(sessionRegistry(other).Contexts, app)}
	if err := sessionstate.RegistryStoreFor(runtime.root).Save(registry); err != nil {
		t.Fatal(err)
	}
	runtime.desired.Workspaces = append(runtime.desired.Workspaces, placementOnlySnapshot("100: terminal", other).Workspaces...)
	terminal := managedDaemonLeaf(t, 43, other)
	terminal.Marks = nil
	client := &lifecycleFixtureRequester{tree: daemonTree("99: landing", terminal)}
	runtime.client = client
	passes := 0
	runtime.lifecycleOperations = &fixtureLifecycleAdapter{
		blocked: []sessionstate.ContextID{app.ID},
		reconcile: func(context.Context, time.Time, string, int) (sessionstate.LifecycleReconcileResult, error) {
			passes++
			return sessionstate.LifecycleReconcileResult{Outcomes: []sessionstate.LifecycleOutcome{{OperationID: string(app.ID), Kind: sessionstate.LifecycleRegister, Status: "conflict", Reason: "compositor_changed"}}}, errors.New("recorded conflict")
		},
	}
	client.beforeTree = func() {
		if passes != 1 {
			t.Fatalf("tree requested before pending reconciliation or pass repeated: %d", passes)
		}
	}
	var reported error
	reconcilePersistentSession(client, runtime, func(err error) { reported = err })
	if passes != 1 || reported == nil || len(client.commands) == 0 {
		t.Fatalf("passes=%d report=%v commands=%v", passes, reported, client.commands)
	}
	for _, command := range client.commands {
		if !strings.Contains(command, "con_id=43") {
			t.Fatalf("reserved application effect: %s", command)
		}
	}
}

func TestLifecycleRuntimeReservationAtDispatchStopsPlannedEffects(t *testing.T) {
	for _, structural := range []bool{false, true} {
		t.Run(map[bool]string{false: "placement", true: "layout"}[structural], func(t *testing.T) {
			runtime, requester, _, app, _ := testApplicationRuntime(t)
			fixture := &fixtureLifecycleAdapter{}
			runtime.lifecycleOperations = fixture
			if _, _, err := runtime.loadRegistry(); err != nil {
				t.Fatal(err)
			}
			fixture.blocked = []sessionstate.ContextID{app.ID}
			var err error
			if structural {
				err = runtime.applyRestoreAction(nil, sessionstate.RestoreAction{Kind: sessionstate.RestoreSetLayout, Workspace: "98: apps", ContainerID: 42, Layout: sessionstate.LayoutTabbed})
			} else {
				err = runtime.applyPlacementAction(nil, sessionstate.PlacementAction{Kind: sessionstate.PlacementAddMark, ContextID: app.ID, ContainerID: 42})
			}
			if !errors.Is(err, errLifecyclePlanChanged) || len(requester.commands) != 0 {
				t.Fatalf("err=%v commands=%v", err, requester.commands)
			}
		})
	}
}

func TestLifecycleRuntimeUnreservedAppStillPlacesUnderExistingLock(t *testing.T) {
	runtime, requester, _, app, now := testApplicationRuntime(t)
	runtime.lifecycleOperations = &fixtureLifecycleAdapter{}
	appID, sandbox := app.App.Identity.WaylandAppID, app.App.Identity.SandboxAppID
	tree := daemonTree("99: landing", &Node{ID: 42, Type: "con", AppID: &appID, SandboxAppID: &sandbox})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	runtime.ctx = ctx
	refresh, err := runtime.Reconcile(tree, now)
	if err != nil || !refresh || len(requester.commands) == 0 {
		t.Fatalf("refresh=%v err=%v commands=%v", refresh, err, requester.commands)
	}
}

func TestLifecycleReservationRetainsLateApplicationStartupIntent(t *testing.T) {
	runtime, requester, app, terminal, window, start := delayedApplicationScenario(t)
	fixture := &fixtureLifecycleAdapter{blocked: []sessionstate.ContextID{app.ID}}
	runtime.lifecycleOperations = fixture
	if _, err := runtime.Reconcile(daemonTree("98", terminal), start.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, exists := runtime.startupApplications[app.ID]; !exists {
		t.Fatal("reservation discarded late startup intent")
	}
	fixture.blocked = nil
	if _, err := runtime.Reconcile(daemonTree("98", terminal, window), start.Add(12*time.Second)); err != nil {
		t.Fatal(err)
	}
	mark, _ := app.ID.Mark()
	window.Marks = []string{mark}
	before := len(requester.commands)
	if _, err := runtime.Reconcile(daemonTree("98", terminal, window), start.Add(13*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, command := range requester.commands[before:] {
		if strings.Contains(command, sessionstate.RestoreStagingWorkspace) {
			return
		}
	}
	t.Fatalf("released reservation could not rearm saved startup layout: %v", requester.commands[before:])
}

func TestLifecycleReservationRetainsAdoptedFollowCloseEvidence(t *testing.T) {
	runtime, _, launcher, app, start := testApplicationRuntime(t)
	fixture := &fixtureLifecycleAdapter{}
	runtime.lifecycleOperations = fixture
	runtime.startupComplete = true
	appID, sandbox := app.App.Identity.WaylandAppID, app.App.Identity.SandboxAppID
	mark, _ := app.ID.Mark()
	window := &Node{ID: 42, Type: "con", AppID: &appID, SandboxAppID: &sandbox, Marks: []string{mark}}
	if _, err := runtime.Reconcile(daemonTree("98: apps", window), start); err != nil {
		t.Fatal(err)
	}
	fixture.blocked = []sessionstate.ContextID{app.ID}
	if _, err := runtime.Reconcile(daemonTree("98: apps"), start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	fixture.blocked = nil
	for _, elapsed := range []time.Duration{2 * time.Minute, 2*time.Minute + 3*time.Second} {
		if _, err := runtime.Reconcile(daemonTree("98: apps"), start.Add(elapsed)); err != nil {
			t.Fatal(err)
		}
	}
	if launcher.starts != 0 {
		t.Fatalf("adopted application relaunched after cancellation: %d", launcher.starts)
	}
	stored, err := sessionstate.ReadRegistrySnapshotContext(t.Context(), runtime.root)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Contexts[0].App.DesiredOpen {
		t.Fatal("lost follow-close transition after reservation release")
	}
}

func TestLifecycleSuspendedWorkspaceRetainsRollbackWhileOtherRestoreAndCaptureProceed(t *testing.T) {
	for _, cancellation := range []string{"resume", "binding", "disconnect", "reload"} {
		t.Run(cancellation, func(t *testing.T) {
			cancelled := cancellation == "binding" || cancellation == "disconnect"

			previous, _, _, app, start := testApplicationRuntime(t)
			firstID := sessionstate.ContextID("22222222-2222-4222-8222-222222222222")
			secondID := sessionstate.ContextID("33333333-3333-4333-8333-333333333333")
			captureID := sessionstate.ContextID("44444444-4444-4444-8444-444444444444")
			registry := sessionRegistryIDs(firstID, secondID, captureID)
			registry.Contexts = append(registry.Contexts, app)
			if err := sessionstate.RegistryStoreFor(previous.root).Save(registry); err != nil {
				t.Fatal(err)
			}
			protected := sessionstate.WorkspaceLayout{Name: "98: reserved", RestoreMode: sessionstate.WorkspaceRestoreLayout, Tiling: &sessionstate.LayoutNode{ContextID: &app.ID}}
			desired := sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{protected}}
			desired.Workspaces = append(desired.Workspaces, exactDaemonSnapshot("99: restore", firstID, secondID).Workspaces...)
			desired.Workspaces[1].Tiling.Children[0].Proportion = 0.7
			desired.Workspaces[1].Tiling.Children[1].Proportion = 0.3
			desired.Workspaces = append(desired.Workspaces, placementOnlySnapshot("100: previous", captureID).Workspaces...)
			if err := sessionstate.LayoutStoreFor(previous.root).Save(desired); err != nil {
				t.Fatal(err)
			}
			appID, sandbox := app.App.Identity.WaylandAppID, app.App.Identity.SandboxAppID
			mark, _ := app.ID.Mark()
			appWindow := &Node{ID: 42, Type: "con", AppID: &appID, SandboxAppID: &sandbox, Marks: []string{mark}}
			first, second := managedDaemonLeaf(t, 51, firstID), managedDaemonLeaf(t, 52, secondID)
			half := 0.5
			first.Percent, second.Percent = &half, &half
			tree := daemonTree(sessionstate.RestoreStagingWorkspace, appWindow)
			stage := tree.Nodes[0].Nodes[0]
			reserved := &Node{ID: 4, Type: "workspace", Name: "98: reserved", Layout: "splith"}
			other := &Node{ID: 5, Type: "workspace", Name: "99: restore", Layout: "splith", Nodes: []*Node{first, second}}
			captured := &Node{ID: 6, Type: "workspace", Name: "101: captured", Layout: "splith", Nodes: []*Node{managedDaemonLeaf(t, 53, captureID)}}
			tree.Nodes[0].Nodes = append(tree.Nodes[0].Nodes, reserved, other, captured)
			client := &lifecycleFixtureRequester{tree: tree}
			client.afterCommand = func() {
				command := client.commands[len(client.commands)-1]
				switch command {
				case `[con_id=51] resize set width 70 ppt`:
					value := 0.7
					first.Percent = &value
				case `[con_id=52] resize set width 30 ppt`:
					value := 0.3
					second.Percent = &value
				case `[con_id=42] move container to workspace "98: reserved"`:
					stage.Nodes = nil
					reserved.Nodes = []*Node{appWindow}
				default:
					t.Fatalf("unexpected fixture effect: %s", command)
				}
			}
			fixture := &fixtureLifecycleAdapter{blocked: []sessionstate.ContextID{app.ID}}
			runtime, err := newSessionRuntimeWithOptions(client, sessionRuntimeOptions{Root: previous.root, LifecycleOperations: fixture})
			if err != nil {
				t.Fatal(err)
			}
			progress := sessionstate.RestoreProgress{Workspace: "98: reserved", Phase: sessionstate.RestoreRollbackIn, Actions: 7, LastActionKey: "saved-step", RepeatedActions: 2}
			runtime.restoreProgress = &progress
			rollbackFailure := errors.New("retained rollback failure")
			runtime.restoreFailures[progress.Workspace] = rollbackFailure
			runtime.restoreEligible[firstID] = struct{}{}
			runtime.startupDeadline = start
			runtime.originalFocusDone = true
			runtime.restoreCleanup.Remember(sessionstate.RestoreAction{Kind: sessionstate.RestoreMoveWorkspace, Workspace: "98: reserved", ContextID: app.ID, ContainerID: 42, Target: sessionstate.RestoreStagingWorkspace})
			if _, err := runtime.Reconcile(tree, start); err != nil {
				t.Fatal(err)
			}
			if got, exists := runtime.restoreSuspended[progress.Workspace]; !exists || got.progress != progress {
				t.Fatalf("lost rollback cursor: %+v", runtime.restoreSuspended)
			}
			if len(client.commands) != 1 || !strings.Contains(client.commands[0], "con_id=51") {
				t.Fatalf("blocked workspace starved other restore: %v", client.commands)
			}
			for range 6 {
				if _, err := runtime.Reconcile(tree, start.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			if !runtime.startupComplete || runtime.restoreProgress != nil || !runtime.restoreCleanup.Pending() {
				t.Fatalf("available work did not settle or staging ownership lost: complete=%v progress=%+v cleanup=%v", runtime.startupComplete, runtime.restoreProgress, runtime.restoreCleanup.Pending())
			}
			if err := runtime.Flush(start.Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var stored sessionstate.LayoutSnapshot
			if err := sessionstate.LayoutStoreFor(runtime.root).LoadInto(&stored); err != nil {
				t.Fatal(err)
			}
			if name, _ := snapshotContextWorkspace(stored, captureID); name != "101: captured" {
				t.Fatalf("blocked cursor stalled unrelated capture: %+v", stored)
			}
			if got, _ := workspaceByName(stored, "98: reserved"); !reflect.DeepEqual(got, protected) {
				t.Fatalf("lost reserved workspace: %+v", got)
			}
			if runtime.restoreFailures[progress.Workspace] == nil {
				t.Fatal("suspension discarded rollback failure evidence")
			}
			if cancellation == "reload" {
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWorkspace, Change: "reload"}, start.Add(2*time.Second))
			}
			if cancelled {
				switch cancellation {
				case "binding":
					runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding}, start.Add(2*time.Second))
				case "disconnect":
					runtime.eventStreamReady = true
					runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "disconnected"}, start.Add(2*time.Second))
					runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 2}, start.Add(2*time.Second))
				}
				if len(runtime.restoreSuspended) != 0 || !runtime.restoreCleanup.Pending() {
					t.Fatal("user cancellation lost staging ownership")
				}
				if _, err := runtime.Reconcile(tree, start.Add(2*time.Second)); err != nil {
					t.Fatal(err)
				}
				if !runtime.restoreCleanup.Pending() {
					t.Fatal("blocked cleanup discarded staging ownership")
				}
			}
			fixture.blocked = nil
			if _, err := runtime.Reconcile(tree, start.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			if cancelled && (runtime.restoreProgress != nil || runtime.lateRestorePending || !runtime.restoreCancelled) {
				t.Fatal("user cancellation resumed structural reconstruction")
			}
			if !cancelled && (runtime.restoreProgress == nil || runtime.restoreProgress.Phase != sessionstate.RestoreRollbackIn || runtime.restoreProgress.Actions != 8 || len(runtime.restoreSuspended) != 0) {
				t.Fatalf("released cursor restarted instead of resuming rollback: %+v", runtime.restoreProgress)
			}
			for range 4 {
				if _, err := runtime.Reconcile(tree, start.Add(4*time.Second)); err != nil && !errors.Is(err, rollbackFailure) {
					t.Fatal(err)
				}
			}
			if runtime.restoreProgress != nil || runtime.restoreCleanup.Pending() || len(stage.Nodes) != 0 {
				t.Fatalf("resumed rollback stranded staged window: progress=%+v cleanup=%v", runtime.restoreProgress, runtime.restoreCleanup.Pending())
			}

		})
	}
}

func TestLifecycleReservationDefersRestartStagingDiscoveryWithoutLosingCancellation(t *testing.T) {
	runtime, _, _, app, now := testApplicationRuntime(t)
	fixture := &fixtureLifecycleAdapter{blocked: []sessionstate.ContextID{app.ID}}
	runtime.lifecycleOperations = fixture
	appID, sandbox := app.App.Identity.WaylandAppID, app.App.Identity.SandboxAppID
	mark, _ := app.ID.Mark()
	window := &Node{ID: 42, Type: "con", AppID: &appID, SandboxAppID: &sandbox, Marks: []string{mark}}
	client := &cleanupCompositor{t: t, root: daemonTree(sessionstate.RestoreStagingWorkspace, window)}
	runtime.client = client
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding}, now)
	if _, err := runtime.Reconcile(client.root, now); err != nil {
		t.Fatal(err)
	}
	if !runtime.restoreRecoveryPending || len(client.commands) != 0 {
		t.Fatalf("reserved restart evidence lost or acted on: pending=%v commands=%v", runtime.restoreRecoveryPending, client.commands)
	}
	fixture.blocked = nil
	if _, err := runtime.Reconcile(client.root, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(client.commands) != 1 || client.commands[0] != `[con_id=42] move container to workspace "98: apps"` {
		t.Fatalf("release did not perform only staged cleanup: %v", client.commands)
	}
	client.drain(runtime, now.Add(time.Second))
	if _, err := runtime.Reconcile(client.root, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if runtime.restoreProgress != nil || runtime.lateRestorePending || runtime.restoreRecoveryPending || runtime.restoreCleanup.Pending() || !runtime.restoreCancelled {
		t.Fatalf("restart cancellation was not retained: progress=%+v cleanup=%v", runtime.restoreProgress, runtime.restoreCleanup.Pending())
	}
}

func TestLifecycleSuspensionRejectsEventlessLayoutDrift(t *testing.T) {
	for _, change := range []string{"layout", "geometry", "proportion"} {
		for _, observeWhileBlocked := range []bool{false, true} {
			t.Run(change+"/"+map[bool]string{false: "release", true: "observe while blocked then undo"}[observeWhileBlocked], func(t *testing.T) {
				runtime, requester, app, terminal, window, start := applicationStartupScenario(t, false)
				mark, _ := app.ID.Mark()
				window.Marks = []string{mark}
				tree := daemonTree("98", terminal, window)
				fixture := &fixtureLifecycleAdapter{blocked: []sessionstate.ContextID{app.ID}}
				runtime.lifecycleOperations = fixture
				runtime.restoreProgress = &sessionstate.RestoreProgress{Workspace: "98", Phase: sessionstate.RestoreBuild}
				runtime.startupDeadline = start
				if _, err := runtime.Reconcile(tree, start.Add(2*time.Second)); err != nil {
					t.Fatal(err)
				}
				if len(runtime.restoreSuspended) != 1 {
					t.Fatal("fixture did not suspend the build cursor")
				}
				before := len(requester.commands)
				// swaymsg layout changes need not produce a binding or focus event.
				switch change {
				case "layout":
					tree.Nodes[0].Nodes[0].Layout = "stacked"
				case "geometry":
					terminal.Rect.Width = 800
				case "proportion":
					value := 0.7
					terminal.Percent = &value
				}
				if observeWhileBlocked {
					if _, err := runtime.Reconcile(tree, start.Add(3*time.Second)); err != nil {
						t.Fatal(err)
					}
					if !runtime.restoreCancelled {
						t.Fatal("ignored observed structural drift while blocked")
					}
					tree.Nodes[0].Nodes[0].Layout = "splith"
					terminal.Rect.Width = 0
					terminal.Percent = nil
				}
				fixture.blocked = nil
				if _, err := runtime.Reconcile(tree, start.Add(4*time.Second)); err != nil {
					t.Fatal(err)
				}
				if !runtime.restoreCancelled || runtime.restoreProgress != nil || len(runtime.restoreSuspended) != 0 {
					t.Fatalf("eventless user layout did not cancel suspended reconstruction: cancelled=%v progress=%+v commands=%v", runtime.restoreCancelled, runtime.restoreProgress, requester.commands[before:])
				}
				if len(requester.commands) != before {
					t.Fatalf("changed workspace received obsolete restore commands: %v", requester.commands[before:])
				}
			})
		}
	}

}

func TestLifecycleSuspensionAllowsMappingAndPresentationChanges(t *testing.T) {
	runtime, _, app, terminal, window, _ := applicationStartupScenario(t, false)
	tree := daemonTree("98", terminal, window)
	runtime.lifecycleBlocked = map[sessionstate.ContextID]struct{}{app.ID: {}}
	progress := sessionstate.RestoreProgress{Workspace: "98", Phase: sessionstate.RestoreBuild, Actions: 7, LastActionKey: "previous effect", RepeatedActions: 2}
	runtime.restoreProgress = &progress
	runtime.suspendLifecycleRestore(tree)
	// Marks, focus and titles are not layout evidence; new views can naturally
	// resize the original siblings without replacing their structural intent.
	terminal.Name = "changed title"
	terminal.Marks = append(terminal.Marks, "presentation-only")
	terminal.Focused = true
	otherID := "org.example.New"
	tree.Nodes[0].Nodes[0].Nodes = append(tree.Nodes[0].Nodes[0].Nodes, &Node{ID: 43, Type: "con", AppID: &otherID})
	terminal.Rect.Width = 800
	value := 0.4
	terminal.Percent = &value
	runtime.suspendLifecycleRestore(tree)
	runtime.lifecycleBlocked = nil
	runtime.suspendLifecycleRestore(tree)
	runtime.resumeLifecycleRestore()
	if runtime.restoreCancelled || runtime.restoreProgress == nil || *runtime.restoreProgress != progress || len(runtime.restoreSuspended) != 0 {
		t.Fatalf("harmless changes cancelled or reset cursor: cancelled=%v progress=%+v", runtime.restoreCancelled, runtime.restoreProgress)
	}
	// A later pause observes the fresh tree after the daemon's preceding effect,
	// rather than comparing against evidence from the completed suspension.
	tree.Nodes[0].Nodes[0].Layout = "tabbed"
	runtime.lifecycleBlocked = map[sessionstate.ContextID]struct{}{app.ID: {}}
	runtime.suspendLifecycleRestore(tree)
	runtime.suspendLifecycleRestore(tree)
	if runtime.restoreCancelled {
		t.Fatal("a new suspension compared against the previous pause baseline")
	}
}

func TestLifecycleSuspensionLayoutDriftRetainsStagingCleanup(t *testing.T) {
	runtime, requester, app, terminal, window, start := applicationStartupScenario(t, false)
	mark, _ := app.ID.Mark()
	window.Marks = []string{mark}
	tree := daemonTree("98", window)
	tree.Nodes[0].Nodes = append(tree.Nodes[0].Nodes, &Node{ID: 4, Type: "workspace", Name: sessionstate.RestoreStagingWorkspace, Layout: "splith", Nodes: []*Node{terminal}})
	fixture := &fixtureLifecycleAdapter{blocked: []sessionstate.ContextID{app.ID}}
	runtime.lifecycleOperations = fixture
	runtime.lifecycleBlocked = map[sessionstate.ContextID]struct{}{app.ID: {}}
	runtime.restoreProgress = &sessionstate.RestoreProgress{Workspace: "98", Phase: sessionstate.RestoreBuild}
	terminalID := sessionstate.ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	runtime.restoreCleanup.Remember(sessionstate.RestoreAction{Kind: sessionstate.RestoreMoveWorkspace, Workspace: "98", ContextID: terminalID, ContainerID: terminal.ID, Target: sessionstate.RestoreStagingWorkspace})
	runtime.suspendLifecycleRestore(tree)
	tree.Nodes[0].Nodes[0].Layout = "stacked"
	runtime.suspendLifecycleRestore(tree)
	if !runtime.restoreCancelled || !runtime.restoreCleanup.Pending() {
		t.Fatal("eventless drift discarded staging ownership")
	}
	before := len(requester.commands)
	if _, err := runtime.Reconcile(tree, start.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(requester.commands) != before || !runtime.restoreCleanup.Pending() {
		t.Fatal("cleanup bypassed the reservation")
	}
	fixture.blocked = nil
	if _, err := runtime.Reconcile(tree, start.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(requester.commands) != before+1 || requester.commands[before] != `[con_id=41] move container to workspace "98"` {
		t.Fatalf("release should perform cleanup only: %v", requester.commands[before:])
	}
}

func TestLifecycleSuspensionDetectsResizeAfterAllowedMapping(t *testing.T) {
	runtime, _, app, terminal, window, _ := applicationStartupScenario(t, false)
	tree := daemonTree("98", terminal, window)
	runtime.lifecycleBlocked = map[sessionstate.ContextID]struct{}{app.ID: {}}
	runtime.restoreProgress = &sessionstate.RestoreProgress{Workspace: "98", Phase: sessionstate.RestoreBuild}
	runtime.suspendLifecycleRestore(tree)
	otherID := "org.example.New"
	tree.Nodes[0].Nodes[0].Nodes = append(tree.Nodes[0].Nodes[0].Nodes, &Node{ID: 43, Type: "con", AppID: &otherID})
	terminal.Rect.Width = 800
	runtime.suspendLifecycleRestore(tree)
	if runtime.restoreCancelled {
		t.Fatal("normal mapping incorrectly cancelled restore")
	}
	// A later observation has the same three windows; only user geometry changes.
	terminal.Rect.Width = 640
	runtime.suspendLifecycleRestore(tree)
	if !runtime.restoreCancelled {
		t.Fatal("eventless user resize after an allowed mapping was ignored")
	}
}

func TestLifecycleSuspensionDetectsEmptyWorkspaceLayoutChange(t *testing.T) {
	runtime, _, app, _, _, _ := applicationStartupScenario(t, false)
	tree := daemonTree("98")
	runtime.lifecycleBlocked = map[sessionstate.ContextID]struct{}{app.ID: {}}
	runtime.restoreProgress = &sessionstate.RestoreProgress{Workspace: "98", Phase: sessionstate.RestoreBuild}
	runtime.suspendLifecycleRestore(tree)
	tree.Nodes[0].Nodes[0].Layout = "stacked"
	runtime.lifecycleBlocked = nil
	runtime.suspendLifecycleRestore(tree)
	runtime.resumeLifecycleRestore()
	if !runtime.restoreCancelled || runtime.restoreProgress != nil || len(runtime.restoreSuspended) != 0 {
		t.Fatal("empty destination layout change was ignored")
	}
}
