package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

const restoreReportSecondID sessionstate.ContextID = "22222222-2222-4222-8222-222222222222"

func restoreReportRuntime(t *testing.T, registry sessionstate.Registry, snapshot sessionstate.LayoutSnapshot) (*sessionRuntime, time.Time) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	if err := sessionstate.RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	if err := sessionstate.LayoutStoreFor(root).Save(snapshot); err != nil {
		t.Fatal(err)
	}
	runtime, err := newSessionRuntimeWithOptions(&recordingRequester{}, sessionRuntimeOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for pass := 0; pass < len(registry.Contexts)+2; pass++ {
		if err := runtime.seedRestoreReport(registry, now); err != nil {
			t.Fatal(err)
		}
		if runtime.restoreReportSeeded && runtime.restoreReportSeedCursor == len(registry.Contexts) {
			break
		}
	}
	if !runtime.restoreReportSeeded || runtime.restoreReportSeedCursor != len(registry.Contexts) {
		t.Fatal("automatic seeding never progressed")
	}
	return runtime, now
}

func restoreReportOutcome(t *testing.T, runtime *sessionRuntime, source string, id sessionstate.ContextID) sessionstate.RestoreOutcome {
	t.Helper()
	report, err := sessionstate.RestoreReportStoreFor(runtime.root).LoadContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range report.Outcomes {
		if record.Source == source && record.ContextID == id {
			return record
		}
	}
	t.Fatalf("no %s outcome for %s: %+v", source, id, report)
	return sessionstate.RestoreOutcome{}
}

func restoreReportExplicit(t *testing.T, runtime *sessionRuntime, item sessionstate.Context, work sessionstate.RestoreWork, now time.Time) sessionstate.RestoreOutcome {
	t.Helper()
	attempt, err := sessionstate.NewContextID()
	if err != nil {
		t.Fatal(err)
	}
	record := sessionstate.RestoreOutcome{ContextID: item.ID, AttemptID: string(attempt), Source: "explicit", StartedAt: now.UTC(), UpdatedAt: now.UTC(), Status: "pending", Reason: "restore_requested", Requested: work, IdentityDigest: sessionstate.RestoreContextDigest(item)}
	if err := sessionstate.RestoreReportStoreFor(runtime.root).BeginExplicitContext(t.Context(), []sessionstate.RestoreOutcome{record}); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestRestoreRequestedWorkCapturesExactMembershipAndDigest(t *testing.T) {
	snapshot := exactDaemonSnapshot("98", testManagedContextID, restoreReportSecondID)
	work := restoreRequestedWork(snapshot, testManagedContextID)
	if !work.Window || !work.Placement || !work.Layout || work.Workspace != "98" || len(work.LayoutDigest) != 64 {
		t.Fatalf("requested = %+v", work)
	}
	if work != restoreRequestedWork(snapshot, testManagedContextID) {
		t.Fatal("digest is unstable")
	}
	snapshot.Workspaces[0].Tiling.Layout = sessionstate.LayoutTabbed
	if restoreRequestedWork(snapshot, testManagedContextID).LayoutDigest == work.LayoutDigest {
		t.Fatal("different desired structure has same digest")
	}
	snapshot.Workspaces[0] = sessionstate.WorkspaceLayout{Name: "98", RestoreMode: sessionstate.WorkspaceRestorePlacementOnly, PlacementContexts: []sessionstate.ContextID{testManagedContextID}}
	placement := restoreRequestedWork(snapshot, testManagedContextID)
	if !placement.Placement || placement.Layout || placement.LayoutDigest != "" {
		t.Fatalf("placement-only = %+v", placement)
	}
	if missing := restoreRequestedWork(snapshot, restoreReportSecondID); missing != (sessionstate.RestoreWork{Window: true}) {
		t.Fatalf("absent target = %+v", missing)
	}
}

func TestRestoreReportMappingDoesNotProvePlacementOrCommandAcknowledgement(t *testing.T) {
	registry := sessionRegistry(testManagedContextID)
	snapshot := sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{{Name: "98", RestoreMode: sessionstate.WorkspaceRestorePlacementOnly, PlacementContexts: []sessionstate.ContextID{testManagedContextID}}}}
	runtime, now := restoreReportRuntime(t, registry, snapshot)
	leaf := managedDaemonLeaf(t, 41, testManagedContextID)
	leaf.Marks = nil
	tree := daemonTree("99", leaf)
	refresh, err := runtime.Reconcile(tree, now)
	if err != nil || !refresh {
		t.Fatalf("reconcile = %t, %v", refresh, err)
	}
	record := restoreReportOutcome(t, runtime, "automatic", testManagedContextID)
	if !record.WindowMapped || record.PlacementApplied || record.Status != "pending" {
		t.Fatalf("ack was used as proof: %+v", record)
	}
	if err := runtime.observeRestoreReport(daemonTree("98", managedDaemonLeaf(t, 41, testManagedContextID)), registry, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	record = restoreReportOutcome(t, runtime, "automatic", testManagedContextID)
	if record.Status != "completed" || !record.PlacementApplied {
		t.Fatalf("fresh placement not proved: %+v", record)
	}
}

func TestRestoreReportExactLayoutRequiresObservedConvergence(t *testing.T) {
	registry := sessionRegistryIDs(testManagedContextID, restoreReportSecondID)
	snapshot := exactDaemonSnapshot("98", testManagedContextID, restoreReportSecondID)
	runtime, now := restoreReportRuntime(t, registry, snapshot)
	// Same workspace and top-level mode; wrong child order is still not restored.
	reversed := daemonTree("98", managedDaemonLeaf(t, 42, restoreReportSecondID), managedDaemonLeaf(t, 41, testManagedContextID))
	if err := runtime.observeRestoreReport(reversed, registry, now); err != nil {
		t.Fatal(err)
	}
	record := restoreReportOutcome(t, runtime, "automatic", testManagedContextID)
	if !record.WindowMapped || !record.PlacementApplied || record.LayoutApplied || record.Status != "pending" {
		t.Fatalf("layout mode mistaken for convergence: %+v", record)
	}
	correct := daemonTree("98", managedDaemonLeaf(t, 41, testManagedContextID), managedDaemonLeaf(t, 42, restoreReportSecondID))
	if err := runtime.observeRestoreReport(correct, registry, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	record = restoreReportOutcome(t, runtime, "automatic", testManagedContextID)
	if !record.LayoutApplied || record.Status != "completed" {
		t.Fatalf("converged layout not proved: %+v", record)
	}
}

func TestRestoreReportRejectsRequestedLayoutDigestDrift(t *testing.T) {
	registry := sessionRegistryIDs(testManagedContextID, restoreReportSecondID)
	snapshot := exactDaemonSnapshot("98", testManagedContextID, restoreReportSecondID)
	runtime, now := restoreReportRuntime(t, registry, snapshot)
	runtime.persisted.Workspaces[0].Tiling.Layout = sessionstate.LayoutTabbed
	tree := daemonTree("98", managedDaemonLeaf(t, 41, testManagedContextID), managedDaemonLeaf(t, 42, restoreReportSecondID))
	tree.Nodes[0].Nodes[0].Layout = "tabbed"
	if err := runtime.observeRestoreReport(tree, registry, now); err != nil {
		t.Fatal(err)
	}
	record := restoreReportOutcome(t, runtime, "automatic", testManagedContextID)
	if record.LayoutApplied || record.Status != "pending" {
		t.Fatalf("different persisted request claimed: %+v", record)
	}
}

func TestRestoreReportActionFailurePreservesOperationError(t *testing.T) {
	registry := sessionRegistry(testManagedContextID)
	snapshot := sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{{Name: "98", RestoreMode: sessionstate.WorkspaceRestorePlacementOnly, PlacementContexts: []sessionstate.ContextID{testManagedContextID}}}}
	runtime, now := restoreReportRuntime(t, registry, snapshot)
	failure := errors.New("private compositor failure text")
	runtime.client = &recordingRequester{failAll: true, failure: failure}
	leaf := managedDaemonLeaf(t, 41, testManagedContextID)
	leaf.Marks = nil
	_, err := runtime.Reconcile(daemonTree("99", leaf), now)
	if !errors.Is(err, failure) {
		t.Fatalf("operation error lost: %v", err)
	}
	record := restoreReportOutcome(t, runtime, "automatic", testManagedContextID)
	if record.Status != "failed" || record.Reason != "placement_failed" || strings.Contains(record.Reason, "private") {
		t.Fatalf("unstable failure outcome: %+v", record)
	}
}

func TestRestoreReportCancelAndShutdownInterruptWithFreshContext(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "shutdown"}[shutdown], func(t *testing.T) {
			registry := sessionRegistry(testManagedContextID)
			runtime, now := restoreReportRuntime(t, registry, sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}})
			restoreReportExplicit(t, runtime, registry.Contexts[0], sessionstate.RestoreWork{Window: true}, now.Add(time.Millisecond))
			if err := runtime.observeRestoreReport(daemonTree("98"), registry, now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			runtime.ctx = ctx
			cancel()
			if shutdown {
				if err := runtime.Shutdown(); err != nil {
					t.Fatal(err)
				}
			} else {
				runtime.cancelConflictingRestore()
				if runtime.restoreReportErr != nil {
					t.Fatal(runtime.restoreReportErr)
				}
			}
			for _, source := range []string{"automatic", "explicit"} {
				record := restoreReportOutcome(t, runtime, source, testManagedContextID)
				want := "user_cancelled"
				if shutdown {
					want = "interrupted"
				}
				if record.Status != "interrupted" || record.Reason != want {
					t.Fatalf("%s not interrupted: %+v", source, record)
				}
			}
		})
	}
}

func TestRestoreReportExplicitLateArrivalAndStaleAttempt(t *testing.T) {
	registry := sessionRegistry(testManagedContextID)
	runtime, now := restoreReportRuntime(t, registry, sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}})
	runtime.startupComplete = true
	restoreReportExplicit(t, runtime, registry.Contexts[0], sessionstate.RestoreWork{Window: true}, now.Add(time.Millisecond))
	if err := runtime.observeRestoreReport(daemonTree("98"), registry, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	runtime.recordRestoreReportEffect(restoreReportEffect{id: testManagedContextID, reason: "launch_failed"})
	newer := restoreReportExplicit(t, runtime, registry.Contexts[0], sessionstate.RestoreWork{Window: true}, now.Add(2*time.Second))
	if err := runtime.flushRestoreReportEffects(now.Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	record := restoreReportOutcome(t, runtime, "explicit", testManagedContextID)
	if record.AttemptID != newer.AttemptID || record.Status != "pending" {
		t.Fatalf("old error contaminated new attempt: %+v", record)
	}
	if err := runtime.observeRestoreReport(daemonTree("98", managedDaemonLeaf(t, 41, testManagedContextID)), registry, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	record = restoreReportOutcome(t, runtime, "explicit", testManagedContextID)
	if record.Status != "completed" || !record.WindowMapped {
		t.Fatalf("late explicit request not observed: %+v", record)
	}
}

func TestRestoreReportIdentityChangeAndDeletedContext(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "rebound", true: "deleted"}[missing], func(t *testing.T) {
			registry := sessionRegistry(testManagedContextID)
			runtime, now := restoreReportRuntime(t, registry, sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}})
			if missing {
				registry.Contexts = []sessionstate.Context{}
			} else {
				registry.Contexts[0].Launcher.Session = "rebound-session"
			}
			if err := runtime.observeRestoreReport(daemonTree("98", managedDaemonLeaf(t, 41, testManagedContextID)), registry, now); err != nil {
				t.Fatal(err)
			}
			record := restoreReportOutcome(t, runtime, "automatic", testManagedContextID)
			want := "identity_changed"
			if missing {
				want = "context_missing"
			}
			if record.Status != "failed" || record.Reason != want || record.WindowMapped {
				t.Fatalf("stale target credited: %+v", record)
			}
		})
	}
}

func TestRestoreReportRestartRecoversBeforeFirstReconcile(t *testing.T) {
	registry := sessionRegistry(testManagedContextID)
	runtime, now := restoreReportRuntime(t, registry, sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}})
	restoreReportExplicit(t, runtime, registry.Contexts[0], sessionstate.RestoreWork{Window: true}, now)
	if err := runtime.observeRestoreReport(daemonTree("98"), registry, now); err != nil {
		t.Fatal(err)
	}
	restarted, err := newSessionRuntimeWithOptions(&recordingRequester{}, sessionRuntimeOptions{Root: runtime.root})
	if err != nil {
		t.Fatal(err)
	}
	if restarted.restoreRunID == runtime.restoreRunID || restarted.restoreReportSeeded {
		t.Fatal("restart reused run or seeded before reconciliation")
	}
	for _, source := range []string{"automatic", "explicit"} {
		record := restoreReportOutcome(t, restarted, source, testManagedContextID)
		if record.Status != "interrupted" || record.Reason != "daemon_restarted" {
			t.Fatalf("restart lost interruption: %+v", record)
		}
	}
	// A request born after construction survives first reconciliation/recovery.
	restoreReportExplicit(t, restarted, registry.Contexts[0], sessionstate.RestoreWork{Window: true}, time.Now().UTC())
	if _, err := restarted.Reconcile(daemonTree("98"), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	record := restoreReportOutcome(t, restarted, "explicit", testManagedContextID)
	if record.Status != "pending" {
		t.Fatalf("fresh CLI request recovered as old: %+v", record)
	}
}

func TestRestoreReportTimeoutFailsAndRequiresNewAttempt(t *testing.T) {
	registry := sessionRegistry(testManagedContextID)
	runtime, now := restoreReportRuntime(t, registry, sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}})
	if err := runtime.observeRestoreReport(daemonTree("98"), registry, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	record := restoreReportOutcome(t, runtime, "automatic", testManagedContextID)
	if record.Status != "failed" || record.Reason != "mapping_timeout" {
		t.Fatalf("timeout not settled: %+v", record)
	}
	if err := runtime.observeRestoreReport(daemonTree("98", managedDaemonLeaf(t, 41, testManagedContextID)), registry, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if record := restoreReportOutcome(t, runtime, "automatic", testManagedContextID); record.Status != "failed" || record.WindowMapped {
		t.Fatalf("late mapping rewrote expired attempt: %+v", record)
	}
	restoreReportExplicit(t, runtime, registry.Contexts[0], sessionstate.RestoreWork{Window: true}, now.Add(3*time.Minute))
	if err := runtime.observeRestoreReport(daemonTree("98", managedDaemonLeaf(t, 41, testManagedContextID)), registry, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if record := restoreReportOutcome(t, runtime, "explicit", testManagedContextID); record.Status != "completed" {
		t.Fatalf("new retry not completed: %+v", record)
	}
}

func restoreReportDesktop() sessionstate.Context {
	return sessionstate.Context{
		ID: testManagedContextID, State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherDesktop, DesktopID: "example.desktop", DesktopOrigin: sessionstate.DesktopEntrySystem, DesktopPath: "/usr/share/applications/example.desktop"},
		App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "example"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow},
	}
}

func TestRestoreReportApplicationAnchorMustBeUnambiguous(t *testing.T) {
	item := restoreReportDesktop()
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{item}}
	runtime, now := restoreReportRuntime(t, registry, sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}})
	appID := "example"
	tree := daemonTree("98", &Node{ID: 41, Type: "con", AppID: &appID}, &Node{ID: 42, Type: "con", AppID: &appID})
	if err := runtime.observeRestoreReport(tree, registry, now); err != nil {
		t.Fatal(err)
	}
	record := restoreReportOutcome(t, runtime, "automatic", item.ID)
	if record.WindowMapped || record.Status != "pending" {
		t.Fatalf("ambiguous group guessed: %+v", record)
	}
	mark, _ := item.ID.Mark()
	tree.Nodes[0].Nodes[0].Nodes[0].Marks = []string{mark}
	if err := runtime.observeRestoreReport(tree, registry, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	record = restoreReportOutcome(t, runtime, "automatic", item.ID)
	if !record.WindowMapped || record.Status != "completed" {
		t.Fatalf("marked anchor not observed: %+v", record)
	}
}

func TestRestoreReportApplicationLaunchAcceptanceAndFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "rejected"}[failure], func(t *testing.T) {
			item := restoreReportDesktop()
			registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{item}}
			runtime, now := restoreReportRuntime(t, registry, sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}})
			coordinator, err := sessionstate.NewApplicationRestoreCoordinator(strings.Repeat("a", 64), sessionstate.ApplicationSessionState{}, now.Add(-time.Minute), sessionstate.ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: time.Second, MaxConcurrent: 1})
			if err != nil {
				t.Fatal(err)
			}
			runtime.applications = coordinator
			enableApplicationLaunchFixture(runtime, now)
			launcher := &recordingApplicationLauncher{}
			cause := errors.New("private launch error")
			if failure {
				launcher.startErr = cause
			}
			runtime.applicationLauncher = launcher
			_, err = runtime.Reconcile(daemonTree("98"), now)
			if failure && !errors.Is(err, cause) {
				t.Fatalf("launch error lost: %v", err)
			}
			if !failure && err != nil {
				t.Fatal(err)
			}
			record := restoreReportOutcome(t, runtime, "automatic", item.ID)
			if record.WindowMapped || record.PlacementApplied || record.LayoutApplied {
				t.Fatalf("launch acknowledgement proved window work: %+v", record)
			}
			if failure {
				if record.Status != "failed" || record.Reason != "launch_failed" || record.LaunchAccepted {
					t.Fatalf("launch failure outcome: %+v", record)
				}
			} else if record.Status != "pending" || !record.LaunchAccepted {
				t.Fatalf("launch acceptance outcome: %+v", record)
			}
		})
	}
}

func TestRestoreReportPolicySkipsArchivedAndDesiredClosed(t *testing.T) {
	registry := sessionRegistryIDs(testManagedContextID)
	archivedAt := time.Now().UTC()
	registry.Contexts[0].State = sessionstate.ContextArchived
	registry.Contexts[0].ArchivedAt = &archivedAt
	desktop := restoreReportDesktop()
	desktop.ID = restoreReportSecondID
	desktop.App.DesiredOpen = false
	registry.Contexts = append(registry.Contexts, desktop)
	runtime, _ := restoreReportRuntime(t, registry, sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}})
	archived := restoreReportOutcome(t, runtime, "automatic", testManagedContextID)
	closed := restoreReportOutcome(t, runtime, "automatic", restoreReportSecondID)
	if archived.Status != "skipped" || archived.Reason != "policy_archived" || closed.Status != "skipped" || closed.Reason != "policy_not_desired" {
		t.Fatalf("policy skips: %+v / %+v", archived, closed)
	}
	if archived.Requested.Window || closed.Requested.Window {
		t.Fatal("skipped contexts requested window work")
	}
}

func TestRestoreReportBatchesAllContextsWithoutTotalLimit(t *testing.T) {
	ids := make([]sessionstate.ContextID, restoreReportBatch+1)
	leaves := make([]*Node, len(ids))
	for i := range ids {
		id, err := sessionstate.NewContextID()
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
		leaves[i] = managedDaemonLeaf(t, int64(i+40), id)
	}
	registry := sessionRegistryIDs(ids...)
	runtime, now := restoreReportRuntime(t, registry, sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}})
	tree := daemonTree("98", leaves...)
	if err := runtime.observeRestoreReport(tree, registry, now); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	report, err := sessionstate.RestoreReportStoreFor(runtime.root).LoadContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	completed := 0
	for _, record := range report.Outcomes {
		if record.Status == "completed" {
			completed++
		}
	}
	if len(report.Outcomes) != len(ids) || completed > restoreReportBatch {
		t.Fatalf("first batch contexts:%d completed:%d", len(report.Outcomes), completed)
	}
	// Under the race detector, the wall-time budget may yield before the
	// count budget. Each later pass resumes instead of requiring 64 commits.
	for pass := 1; pass <= 10; pass++ {
		if err := runtime.observeRestoreReport(tree, registry, now.Add(time.Duration(pass)*time.Second)); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		report, err = sessionstate.RestoreReportStoreFor(runtime.root).LoadContext(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		done := true
		for _, record := range report.Outcomes {
			if record.Status != "completed" {
				done = false
			}
		}
		if done {
			return
		}
	}
	t.Fatal("later contexts starved after repeated bounded passes")
}

func TestRestoreReportExplicitRetryRearmsMarkedLayoutOnceAfterCancellation(t *testing.T) {
	registry := sessionRegistryIDs(testManagedContextID, restoreReportSecondID)
	snapshot := exactDaemonSnapshot("98", testManagedContextID, restoreReportSecondID)
	runtime, now := restoreReportRuntime(t, registry, snapshot)
	runtime.startupComplete = true
	runtime.restoreExcluded["98"] = struct{}{}
	runtime.restoreFailures["98"] = errors.New("previous layout failure")
	runtime.restoreCancelled = true
	runtime.restoreExcluded["99"] = struct{}{}
	rejected := sessionstate.RestoreAction{Kind: sessionstate.RestoreSetProportion, Workspace: "98", ContainerID: 41, Axis: "width", Amount: 50}.Key()
	unrelated := sessionstate.RestoreAction{Kind: sessionstate.RestoreSetProportion, Workspace: "99", ContainerID: 43, Axis: "width", Amount: 50}.Key()
	runtime.restoreSkipped[rejected], runtime.restoreSkipped[unrelated] = struct{}{}, struct{}{}
	record := restoreReportExplicit(t, runtime, registry.Contexts[0], restoreRequestedWork(snapshot, testManagedContextID), now.Add(time.Millisecond))
	reversed := daemonTree("98", managedDaemonLeaf(t, 42, restoreReportSecondID), managedDaemonLeaf(t, 41, testManagedContextID))
	refresh, err := runtime.Reconcile(reversed, now.Add(time.Second))
	if err != nil || !refresh {
		t.Fatalf("explicit retry did not execute existing planner: %t, %v", refresh, err)
	}
	requester := runtime.client.(*recordingRequester)
	if len(requester.commands) == 0 || !strings.Contains(requester.commands[0], sessionstate.RestoreStagingWorkspace) {
		t.Fatalf("marked windows were not retried: %v", requester.commands)
	}
	if !runtime.restoreCancelled {
		t.Fatal("explicit retry cleared global automatic cancellation")
	}
	if _, excluded := runtime.restoreExcluded["99"]; !excluded {
		t.Fatal("explicit retry cleared unrelated cancellation")
	}
	if runtime.restoreReportRearmed[testManagedContextID] != record.AttemptID {
		t.Fatal("retry token not consumed")
	}
	if _, exists := runtime.restoreSkipped[rejected]; exists {
		t.Fatal("selected workspace rejection survived retry")
	}
	if _, exists := runtime.restoreSkipped[unrelated]; !exists {
		t.Fatal("retry cleared unrelated rejection")
	}
	outcome := restoreReportOutcome(t, runtime, "explicit", testManagedContextID)
	if outcome.Status != "pending" || outcome.LayoutApplied {
		t.Fatalf("retry command acknowledged as layout proof: %+v", outcome)
	}
	// The same pending token cannot continually restart a workspace restore.
	runtime.restoreProgress = nil
	runtime.lateRestorePending = false
	runtime.restoreExcluded["98"] = struct{}{}
	if err := runtime.observeRestoreReport(reversed, registry, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if runtime.lateRestorePending {
		t.Fatal("same token rearmed twice")
	}
}

func TestRestoreReportRestartPreservesOwnerlessCLIRequest(t *testing.T) {
	registry := sessionRegistry(testManagedContextID)
	runtime, now := restoreReportRuntime(t, registry, sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}})
	restoreReportExplicit(t, runtime, registry.Contexts[0], sessionstate.RestoreWork{Window: true}, now)
	restarted, err := newSessionRuntimeWithOptions(&recordingRequester{}, sessionRuntimeOptions{Root: runtime.root})
	if err != nil {
		t.Fatal(err)
	}
	record := restoreReportOutcome(t, restarted, "explicit", testManagedContextID)
	if record.Status != "pending" || record.OwnerRunID != "" {
		t.Fatalf("active CLI request interrupted: %+v", record)
	}
}

func TestRestoreReportBudgetExhaustionResumesObservationAndEffects(t *testing.T) {
	registry := sessionRegistry(testManagedContextID)
	runtime, now := restoreReportRuntime(t, registry, sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}})
	runtime.restoreReportInPass = true
	runtime.restoreReportBudget = 0
	if err := runtime.observeRestoreReport(daemonTree("98", managedDaemonLeaf(t, 41, testManagedContextID)), registry, now); err != nil {
		t.Fatalf("zero budget = %v", err)
	}
	if runtime.restoreReportCursor != "" {
		t.Fatal("exhausted budget advanced observation cursor")
	}
	runtime.restoreReportInPass = false
	if err := runtime.observeRestoreReport(daemonTree("98"), registry, now); err != nil {
		t.Fatal(err)
	}
	runtime.recordRestoreReportEffect(restoreReportEffect{id: testManagedContextID, reason: "launch_failed"})
	runtime.restoreReportInPass = true
	runtime.restoreReportBudget = 0
	if err := runtime.flushRestoreReportEffects(now); err != nil {
		t.Fatalf("zero effect budget = %v", err)
	}
	if len(runtime.restoreReportEffects) != 1 || runtime.restoreReportEffects[0].cursor != 0 {
		t.Fatal("unwritten effect discarded")
	}
	runtime.restoreReportInPass = false
	if err := runtime.flushRestoreReportEffects(now); err != nil {
		t.Fatal(err)
	}
	record := restoreReportOutcome(t, runtime, "automatic", testManagedContextID)
	if record.Status != "failed" || record.Reason != "launch_failed" {
		t.Fatalf("effect not resumed: %+v", record)
	}
}

func TestRestoreReportObserverDoesNotRearmReplacedOrFinalizedAttempt(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "finalized_same_token", true: "replaced_token"}[replace], func(t *testing.T) {
			registry := sessionRegistryIDs(testManagedContextID, restoreReportSecondID)
			snapshot := exactDaemonSnapshot("98", testManagedContextID, restoreReportSecondID)
			runtime, now := restoreReportRuntime(t, registry, snapshot)
			runtime.startupComplete = true
			runtime.restoreCancelled = true
			runtime.restoreExcluded["98"] = struct{}{}
			previousFailure := errors.New("previous workspace failure")
			runtime.restoreFailures["98"] = previousFailure
			original := restoreReportExplicit(t, runtime, registry.Contexts[0], restoreRequestedWork(snapshot, testManagedContextID), now.Add(time.Millisecond))
			store := sessionstate.RestoreReportStoreFor(runtime.root)
			interleaved := false
			latestToken := original.AttemptID
			update := func(ctx context.Context, source string, id sessionstate.ContextID, token string, change sessionstate.RestoreOutcomeUpdate) (bool, error) {
				if source == "explicit" && id == testManagedContextID && token == original.AttemptID {
					if interleaved {
						t.Fatal("attempt observed twice in one pass")
					}
					interleaved = true
					// The observer has loaded the old pending token and computed a layout
					// retry, but has not performed its CAS or rearmed the runtime yet.
					if change.Status != "pending" || !change.WindowMapped || change.LayoutApplied {
						t.Fatalf("not at retry boundary: %+v", change)
					}
					if replace {
						latest := restoreReportExplicit(t, runtime, registry.Contexts[0], original.Requested, now.Add(2*time.Millisecond))
						latestToken = latest.AttemptID
					} else {
						matched, err := store.UpdateContext(ctx, source, id, token, sessionstate.RestoreOutcomeUpdate{Status: "failed", Reason: "layout_failed", UpdatedAt: now.Add(2 * time.Millisecond)})
						if err != nil || !matched {
							t.Fatalf("concurrent finalizer: %t, %v", matched, err)
						}
					}
				}
				return store.UpdatePendingContext(ctx, source, id, token, change)
			}
			reversed := daemonTree("98", managedDaemonLeaf(t, 42, restoreReportSecondID), managedDaemonLeaf(t, 41, testManagedContextID))
			if err := runtime.observeRestoreReportWithUpdater(reversed, registry, now.Add(time.Second), update); err != nil {
				t.Fatal(err)
			}
			if !interleaved {
				t.Fatal("writer interleaving was not exercised")
			}
			if runtime.lateRestorePending || runtime.restoreReportRearmed[testManagedContextID] != "" {
				t.Fatal("stale or finalized attempt rearmed layout")
			}
			if _, exists := runtime.restoreEligible[testManagedContextID]; exists {
				t.Fatal("stale attempt changed eligibility")
			}
			if _, excluded := runtime.restoreExcluded["98"]; !excluded || runtime.restoreFailures["98"] != previousFailure {
				t.Fatal("stale attempt cleared failure/cancellation state")
			}
			record := restoreReportOutcome(t, runtime, "explicit", testManagedContextID)
			if record.AttemptID != latestToken || record.WindowMapped || record.PlacementApplied || record.OwnerRunID != "" {
				t.Fatalf("observer changed replacement/finalized record: %+v", record)
			}
			if replace && record.Status != "pending" {
				t.Fatalf("replacement changed: %+v", record)
			}
			if !replace && (record.Status != "failed" || record.Reason != "layout_failed") {
				t.Fatalf("finalization changed: %+v", record)
			}
		})
	}
}

func restoreReportApplicationCoordinator(t *testing.T, runtime *sessionRuntime, now time.Time) {
	t.Helper()
	coordinator, err := sessionstate.NewApplicationRestoreCoordinator(strings.Repeat("a", 64), sessionstate.ApplicationSessionState{}, now.Add(-time.Minute), sessionstate.ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	runtime.applications = coordinator
	enableApplicationLaunchFixture(runtime, now)
}

func TestRestoreReportExplicitApplicationRetryOnlyAfterDefinitiveRejection(t *testing.T) {
	for _, mode := range []string{"rejected", "unknown", "inflight", "mapped", "ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			item := restoreReportDesktop()
			registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{item}}
			runtime, now := restoreReportRuntime(t, registry, sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}})
			restoreReportApplicationCoordinator(t, runtime, now)
			launcher := &recordingApplicationLauncher{}
			cause := errors.New("rejected process start")
			if mode == "unknown" {
				launcher.startErr = &sessionstate.ProcessLaunchOutcomeUnknownError{Err: cause}
			} else if mode != "inflight" {
				launcher.startErr = cause
			}
			runtime.applicationLauncher = launcher
			_, err := runtime.Reconcile(daemonTree("98"), now)
			if mode == "inflight" && err != nil {
				t.Fatal(err)
			}
			if mode != "inflight" && !errors.Is(err, cause) {
				t.Fatalf("start error lost: %v", err)
			}
			if launcher.starts != 1 {
				t.Fatalf("initial starts = %d", launcher.starts)
			}
			initial := restoreReportOutcome(t, runtime, "automatic", item.ID)
			if mode == "unknown" && (!initial.LaunchAccepted || initial.Status != "pending" || initial.Reason != "observation_unavailable") {
				t.Fatalf("unknown start reported as rejection: %+v", initial)
			}
			launcher.startErr = nil
			restoreReportExplicit(t, runtime, item, sessionstate.RestoreWork{Window: true}, now.Add(time.Second))
			tree := daemonTree("98")
			appID := "example"
			if mode == "mapped" || mode == "ambiguous" {
				tree = daemonTree("98", &Node{ID: 41, Type: "con", AppID: &appID})
			}
			if mode == "ambiguous" {
				tree.Nodes[0].Nodes[0].Nodes = append(tree.Nodes[0].Nodes[0].Nodes, &Node{ID: 42, Type: "con", AppID: &appID})
			}
			if _, err := runtime.Reconcile(tree, now.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			wantStarts := 1
			if mode == "rejected" {
				wantStarts = 2
			}
			if launcher.starts != wantStarts {
				t.Fatalf("retry starts = %d, want %d", launcher.starts, wantStarts)
			}
			if _, err := runtime.Reconcile(tree, now.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			if launcher.starts != wantStarts {
				t.Fatal("same token repeated process launch")
			}
			record := restoreReportOutcome(t, runtime, "explicit", item.ID)
			if mode == "rejected" && (!record.LaunchAccepted || record.WindowMapped || record.Status != "pending") {
				t.Fatalf("retry acceptance proof: %+v", record)
			}
			if mode == "ambiguous" && (record.WindowMapped || record.LaunchAccepted) {
				t.Fatalf("ambiguous group claimed: %+v", record)
			}
		})
	}
}

func TestRestoreReportOlderPendingExplicitTokenCannotRetryRejectedStart(t *testing.T) {
	item := restoreReportDesktop()
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{item}}
	runtime, now := restoreReportRuntime(t, registry, sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}})
	restoreReportApplicationCoordinator(t, runtime, now)
	launcher := &recordingApplicationLauncher{startErr: errors.New("rejected")}
	runtime.applicationLauncher = launcher
	restoreReportExplicit(t, runtime, item, sessionstate.RestoreWork{Window: true}, now)
	if err := runtime.observeRestoreReport(daemonTree("98"), registry, now); err != nil {
		t.Fatal(err)
	}
	// Deliberately omit effect draining to reproduce a failure write deferred by
	// the diagnostic budget. The original request is still pending in SQLite.
	_, _, degraded, err := runtime.reconcileApplications(daemonTree("98"), registry, now.Add(time.Second))
	if err != nil || degraded == nil || launcher.starts != 1 {
		t.Fatalf("rejection fixture: %v, %v, %d", degraded, err, launcher.starts)
	}
	launcher.startErr = nil
	if err := runtime.observeRestoreReport(daemonTree("98"), registry, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = runtime.reconcileApplications(daemonTree("98"), registry, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if launcher.starts != 1 || runtime.restoreReportLaunchRearmed[item.ID] != "" {
		t.Fatal("old pending token automatically retried rejected launch")
	}
}

func TestRestoreReportProgressiveSeedRetainsUnpublishedLaunchEffects(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "rejected"}[rejected], func(t *testing.T) {
			ids := make([]sessionstate.ContextID, restoreReportBatch)
			for i := range ids {
				ids[i] = sessionstate.ContextID(fmt.Sprintf("00000000-0000-4000-8000-%012x", i+1))
			}
			registry := sessionRegistryIDs(ids...)
			item := restoreReportDesktop()
			item.ID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
			registry.Contexts = append(registry.Contexts, item)
			root := filepath.Join(t.TempDir(), "state")
			if err := sessionstate.RegistryStoreFor(root).Save(registry); err != nil {
				t.Fatal(err)
			}
			runtime, err := newSessionRuntimeWithOptions(&recordingRequester{}, sessionRuntimeOptions{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			restoreReportApplicationCoordinator(t, runtime, now)
			launcher := &recordingApplicationLauncher{}
			if rejected {
				launcher.startErr = errors.New("rejected")
			}
			runtime.applicationLauncher = launcher
			// A diagnostic I/O budget can yield without advancing the cursor,
			// especially under race instrumentation. Resume until one bounded
			// batch is published; the app must remain unseeded before launch.
			for pass := 0; pass < 16 && runtime.restoreReportSeedCursor == 0; pass++ {
				if err := runtime.seedRestoreReport(registry, now); err != nil {
					t.Fatal(err)
				}
			}
			if runtime.restoreReportSeedCursor != restoreReportBatch {
				t.Fatalf("unbounded first seed: %d", runtime.restoreReportSeedCursor)
			}
			if err := runtime.observeRestoreReport(daemonTree("98"), registry, now); err != nil {
				t.Fatal(err)
			}
			_, _, _, err = runtime.reconcileApplications(daemonTree("98"), registry, now)
			if err != nil {
				t.Fatal(err)
			}
			if launcher.starts != 1 {
				t.Fatalf("unpublished app did not launch: %d", launcher.starts)
			}
			if len(runtime.restoreReportUnseededEffects[item.ID]) == 0 {
				t.Fatal("unpublished launch evidence discarded")
			}
			for pass := 0; pass < 16 && runtime.restoreReportSeedCursor < len(registry.Contexts); pass++ {
				if err := runtime.seedRestoreReport(registry, now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			if runtime.restoreReportSeedCursor != len(registry.Contexts) {
				t.Fatal("resumed seed did not publish the app outcome")
			}
			record := restoreReportOutcome(t, runtime, "automatic", item.ID)
			if rejected {
				if record.Status != "failed" || record.Reason != "launch_failed" || record.LaunchAccepted {
					t.Fatalf("unpublished rejection lost: %+v", record)
				}
			} else if !record.LaunchAccepted || record.Status != "pending" || record.WindowMapped {
				t.Fatalf("unpublished acceptance lost: %+v", record)
			}
		})
	}
}

func TestRestoreReportBudgetYieldPreservesOtherErrorsAndRuntimeCancellation(t *testing.T) {
	registry := sessionRegistry(testManagedContextID)
	runtime, _ := restoreReportRuntime(t, registry, sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}})
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	corruption := errors.New("journal corruption")
	err := runtime.restoreReportIOResult(ctx, errors.Join(context.DeadlineExceeded, corruption))
	if !errors.Is(err, corruption) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("budget error filtering: %v", err)
	}
	runtime.ctx = ctx
	if err := runtime.restoreReportIOResult(ctx, context.DeadlineExceeded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("runtime deadline swallowed")
	}
}

func TestRestoreReportNoProgressPollingDoesNotWriteJournal(t *testing.T) {
	registry := sessionRegistry(testManagedContextID)
	runtime, now := restoreReportRuntime(t, registry, sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}})
	before := restoreReportOutcome(t, runtime, "automatic", testManagedContextID)
	calls := 0
	update := func(context.Context, string, sessionstate.ContextID, string, sessionstate.RestoreOutcomeUpdate) (bool, error) {
		calls++
		return false, errors.New("unchanged polling must not write")
	}
	for pass := 1; pass <= 3; pass++ {
		if err := runtime.observeRestoreReportWithUpdater(daemonTree("98"), registry, now.Add(time.Duration(pass)*time.Second), update); err != nil {
			t.Fatal(err)
		}
	}
	after := restoreReportOutcome(t, runtime, "automatic", testManagedContextID)
	if calls != 0 || !after.UpdatedAt.Equal(before.UpdatedAt) || after.Reason != before.Reason {
		t.Fatalf("poll manufactured progress: calls:%d before:%+v after:%+v", calls, before, after)
	}
}

func TestRestoreReportManagedIdentityIssuePreventsRejectedApplicationRetry(t *testing.T) {
	for _, parentMark := range []bool{false, true} {
		t.Run(map[bool]string{false: "conflicting_marks", true: "mark_on_layout_parent"}[parentMark], func(t *testing.T) {
			item := restoreReportDesktop()
			registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{item}}
			runtime, now := restoreReportRuntime(t, registry, sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}})
			restoreReportApplicationCoordinator(t, runtime, now)
			launcher := &recordingApplicationLauncher{startErr: errors.New("rejected process start")}
			runtime.applicationLauncher = launcher
			if _, err := runtime.Reconcile(daemonTree("98"), now); err == nil || launcher.starts != 1 {
				t.Fatalf("initial rejection fixture: starts:%d err:%v", launcher.starts, err)
			}
			launcher.startErr = nil
			explicit := restoreReportExplicit(t, runtime, item, sessionstate.RestoreWork{Window: true}, now.Add(time.Second))
			mark, _ := item.ID.Mark()
			otherMark, _ := restoreReportSecondID.Mark()
			invalid := &Node{ID: 41, Type: "con", Marks: []string{mark, otherMark}}
			if parentMark {
				unrelated := "unrelated"
				invalid = &Node{ID: 40, Type: "con", Marks: []string{mark}, Nodes: []*Node{{ID: 41, Type: "con", AppID: &unrelated}}}
			}
			tree := daemonTree("98", invalid)
			_, issues, err := sessionstate.ObserveManagedWindowsIsolated(tree, registry)
			if err != nil || len(issues) != 1 || issues[0].ContextID != item.ID {
				t.Fatalf("managed ambiguity fixture: %+v, %v", issues, err)
			}
			groups, err := sessionstate.ObserveApplicationGroups(tree, registry)
			if err != nil {
				t.Fatal(err)
			}
			group := groups[item.ID]
			if group.Ambiguous || group.Anchor != nil || len(group.Windows) != 0 {
				t.Fatalf("fixture must evade application-group ambiguity: %+v", group)
			}
			store := sessionstate.RestoreReportStoreFor(runtime.root)
			calls := 0
			update := func(ctx context.Context, source string, id sessionstate.ContextID, token string, change sessionstate.RestoreOutcomeUpdate) (bool, error) {
				calls++
				return store.UpdatePendingContext(ctx, source, id, token, change)
			}
			// Diagnostic I/O may yield without an error before ownership commits.
			// Confirm the exact pending token's durable owner before measuring
			// no-progress polling; every setup pass must preserve the launch guard.
			owned := false
			for pass := 0; pass < 16; pass++ {
				if err := runtime.observeRestoreReportWithUpdater(tree, registry, now.Add(2*time.Second), update); err != nil {
					t.Fatal(err)
				}
				if runtime.restoreReportLaunchRearmed[item.ID] != "" || len(runtime.applications.State().Attempts) != 1 {
					t.Fatal("managed ambiguity cleared rejected-launch guard")
				}
				record := restoreReportOutcome(t, runtime, "explicit", item.ID)
				if record.AttemptID != explicit.AttemptID || record.Status != "pending" {
					t.Fatalf("explicit ownership fixture changed: %+v", record)
				}
				if record.OwnerRunID == runtime.restoreRunID {
					owned = true
					break
				}
			}
			if !owned {
				t.Fatal("explicit ownership fixture did not commit within bounded observation passes")
			}
			calls = 0
			if err := runtime.observeRestoreReportWithUpdater(tree, registry, now.Add(3*time.Second), update); err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatalf("ambiguous managed identity forced a retry CAS: %d", calls)
			}
			_, _, _, err = runtime.reconcileApplications(tree, registry, now.Add(3*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if launcher.starts != 1 {
				t.Fatal("unresolved managed window caused duplicate desktop launch")
			}
		})
	}
}

func TestPendingExplicitRestoreDoesNotStealNewMappingFocus(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprintf("late_%t", late), func(t *testing.T) {
			runtime, compositor, leaf, now := newMappingFocusScenario(t)
			runtime.startupComplete = late
			registry, err := sessionstate.ReadRegistrySnapshotContext(t.Context(), runtime.root)
			if err != nil {
				t.Fatal(err)
			}
			restoreReportExplicit(t, runtime, registry.Contexts[0], restoreRequestedWork(runtime.persisted, testManagedContextID), now)
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: leaf}, now)
			if _, err := runtime.Reconcile(compositor.root, now); err != nil {
				t.Fatal(err)
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "title", Container: leaf}, now)
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: leaf}, now)
			if runtime.restoreCancelled || (!late && runtime.startupComplete) || (late && !runtime.lateRestorePending) {
				t.Fatal("pending CLI restore report cancelled automatic mapping focus")
			}
			if err := runtime.Flush(now.Add(sessionSnapshotDebounce)); err != nil {
				t.Fatal(err)
			}
			var saved sessionstate.LayoutSnapshot
			if err := sessionstate.LayoutStoreFor(runtime.root).LoadInto(&saved); err != nil {
				t.Fatal(err)
			}
			if saved.Workspaces[0].Tiling == nil || saved.Workspaces[0].Tiling.Layout != sessionstate.LayoutTabbed {
				t.Fatal("pending restore report lost the saved tabbed target")
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: leaf}, now)
			if runtime.restoreCancelled {
				t.Fatal("repeated window focus cancelled saved placement")
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding}, now)
			if !runtime.restoreCancelled {
				t.Fatal("the pending report hid a real user binding")
			}
		})
	}
}
