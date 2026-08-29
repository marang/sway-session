package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
	"github.com/marang/sway-title-animator/internal/swayipc"
)

type recordingRequester struct {
	commands []string
	failAt   int
	failure  error
}

func TestSessionErrorReporterSerializesConcurrentBrokerFailures(t *testing.T) {
	reporter := &sessionErrorReporter{lastMessage: "same", lastAt: time.Now()}
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			reporter.Report(errors.New("same"))
		}()
	}
	wait.Wait()
}

func TestFocusedContainerIDSelectsWindowLeafInsteadOfWorkspace(t *testing.T) {
	leaf := &Node{ID: 42, Type: "con", Focused: true}
	workspace := &Node{ID: 3, Type: "workspace", Focused: true, Nodes: []*Node{leaf}}
	root := &Node{ID: 1, Type: "root", Nodes: []*Node{{ID: 2, Type: "output", Nodes: []*Node{workspace}}}}
	if got := focusedContainerID(root); got != leaf.ID {
		t.Fatalf("focused container ID = %d, want window leaf %d", got, leaf.ID)
	}
}

func TestFocusedContainerIDIgnoresEmptyFocusedWorkspace(t *testing.T) {
	root := daemonTree("1")
	root.Nodes[0].Nodes[0].Focused = true
	if got := focusedContainerID(root); got != 0 {
		t.Fatalf("empty workspace produced restorable focus ID %d", got)
	}
}

func (requester *recordingRequester) Request(messageType swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if messageType != swayipc.RunCommand {
		return swayipc.Message{}, errors.New("unexpected request type")
	}
	requester.commands = append(requester.commands, string(payload))
	if requester.failAt > 0 && len(requester.commands) == requester.failAt {
		return swayipc.Message{}, requester.failure
	}
	return swayipc.Message{Type: swayipc.RunCommand, Payload: []byte(`[{"success":true}]`)}, nil
}

func TestSessionRuntimeMovesThenMarksNewWindowAndCapturesStableTree(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "sway-session")
	registry := sessionRegistry(testManagedContextID)
	if err := sessionstate.RegistryFile(root).Save(registry); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	desired := placementOnlySnapshot("9: saved", testManagedContextID)
	if err := sessionstate.LayoutFile(root).Save(desired); err != nil {
		t.Fatalf("save desired layout: %v", err)
	}

	requester := &recordingRequester{}
	runtime, err := newSessionRuntime(requester)
	if err != nil {
		t.Fatalf("create session runtime: %v", err)
	}
	appID, _ := testManagedContextID.AppID()
	newTree := daemonTree("1", &Node{ID: 42, Name: "terminal", Type: "con", AppID: &appID})
	refresh, err := runtime.Reconcile(newTree, time.Unix(100, 0))
	if err != nil || !refresh {
		t.Fatalf("reconcile new window: refresh=%v err=%v", refresh, err)
	}
	if len(requester.commands) != 2 ||
		!strings.Contains(requester.commands[0], `move container to workspace "9: saved"`) ||
		!strings.Contains(requester.commands[1], `mark --add "persist:`) {
		t.Fatalf("expected move then mark commands, got %v", requester.commands)
	}

	stableTree := daemonTree("9: saved", managedDaemonLeaf(t, 42, testManagedContextID))
	refresh, err = runtime.Reconcile(stableTree, time.Unix(101, 0))
	if err != nil || refresh {
		t.Fatalf("reconcile stable tree: refresh=%v err=%v", refresh, err)
	}
	if _, scheduled := runtime.Deadline(); !scheduled {
		t.Fatal("stable exact tree was not scheduled after placement-only restore")
	}
}

func TestSessionRuntimeCapturesManualMoveOfMarkedWindowAfterDebounce(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "sway-session")
	if err := sessionstate.RegistryFile(root).Save(sessionRegistry(testManagedContextID)); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	if err := sessionstate.LayoutFile(root).Save(placementOnlySnapshot("2", testManagedContextID)); err != nil {
		t.Fatalf("save layout: %v", err)
	}

	requester := &recordingRequester{}
	runtime, err := newSessionRuntime(requester)
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	start := time.Unix(200, 0)
	manualTree := daemonTree("7: manual", managedDaemonLeaf(t, 42, testManagedContextID))
	if refresh, err := runtime.Reconcile(manualTree, start); err != nil || refresh {
		t.Fatalf("capture manual move: refresh=%v err=%v", refresh, err)
	}
	if len(requester.commands) != 0 {
		t.Fatalf("marked window was moved back: %v", requester.commands)
	}
	deadline, scheduled := runtime.Deadline()
	if !scheduled || deadline != start.Add(sessionSnapshotDebounce) {
		t.Fatalf("unexpected debounce deadline: %v scheduled=%v", deadline, scheduled)
	}
	if err := runtime.Flush(deadline); err != nil {
		t.Fatalf("flush captured layout: %v", err)
	}
	var persisted sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutFile(root).LoadInto(&persisted); err != nil {
		t.Fatalf("load captured layout: %v", err)
	}
	if len(persisted.Workspaces) != 1 || persisted.Workspaces[0].Name != "7: manual" {
		t.Fatalf("manual workspace was not persisted: %+v", persisted)
	}
}

func TestSessionRuntimeSchedulesPeriodicObservationOnlyWithRegistry(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	runtime, err := newSessionRuntime(&recordingRequester{})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	start := time.Unix(250, 0)
	if _, err := runtime.Reconcile(daemonTree("1"), start); err != nil {
		t.Fatalf("reconcile without registry: %v", err)
	}
	if runtime.ObservationDue(start.Add(time.Hour)) {
		t.Fatal("missing registry enabled periodic tree observation")
	}

	root := filepath.Join(stateHome, "sway-session")
	if err := sessionstate.RegistryFile(root).Save(sessionRegistry(testManagedContextID)); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	if _, err := runtime.Reconcile(daemonTree("1"), start); err != nil {
		t.Fatalf("reconcile with registry: %v", err)
	}
	if runtime.ObservationDue(start.Add(sessionObservationDelay - time.Nanosecond)) {
		t.Fatal("periodic tree observation became due early")
	}
	if !runtime.ObservationDue(start.Add(sessionObservationDelay)) {
		t.Fatal("periodic tree observation was not scheduled")
	}
	runtime.PostponeObservation(start.Add(sessionObservationDelay))
	if runtime.ObservationDue(start.Add(2*sessionObservationDelay - time.Nanosecond)) {
		t.Fatal("postponed observation became due early")
	}
}

func TestSessionRuntimeDoesNotRetryStartupWithoutRegistry(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "sway-session")
	if err := sessionstate.LayoutFile(root).Save(placementOnlySnapshot("2", testManagedContextID)); err != nil {
		t.Fatalf("save layout without registry: %v", err)
	}
	runtime, err := newSessionRuntime(&recordingRequester{})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	if _, err := runtime.Reconcile(daemonTree("1"), time.Unix(275, 0)); err != nil {
		t.Fatalf("reconcile without registry: %v", err)
	}
	if deadline, scheduled := runtime.Deadline(); scheduled {
		t.Fatalf("missing registry scheduled startup retry at %v", deadline)
	}
}

func TestSessionRuntimeArmsRetryBeforeInitialTreeObservation(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "sway-session")
	if err := sessionstate.RegistryFile(root).Save(sessionRegistry(testManagedContextID)); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	runtime, err := newSessionRuntime(&recordingRequester{})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	start := time.Unix(290, 0)
	runtime.ArmObservationRetry(start)
	if runtime.ObservationDue(start.Add(sessionObservationDelay - time.Nanosecond)) {
		t.Fatal("initial observation retry became due early")
	}
	if !runtime.ObservationDue(start.Add(sessionObservationDelay)) {
		t.Fatal("initial observation failure would not be retried")
	}
}

func TestNewSessionRuntimeRejectsMalformedRegistryOnceAtStartup(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "sway-session")
	malformed := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: nil}
	if err := sessionstate.RegistryFile(root).Save(malformed); err == nil {
		t.Fatal("invalid registry unexpectedly passed storage validation")
	}

	// Save validates candidates, so place malformed JSON through a secure file
	// created by the same owner to exercise startup loading.
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create state root: %v", err)
	}
	registryPath := filepath.Join(root, "contexts.json")
	if err := os.WriteFile(registryPath, []byte(`{"version":1,"contexts":null}\n`), 0o600); err != nil {
		t.Fatalf("write malformed registry: %v", err)
	}
	if _, err := newSessionRuntime(&recordingRequester{}); err == nil ||
		!strings.Contains(err.Error(), "load persistent context registry") {
		t.Fatalf("malformed registry was not rejected at startup: %v", err)
	}
}

func TestSessionRuntimeStartupAndShutdownGuardsKeepPreviousLayout(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "sway-session")
	if err := sessionstate.RegistryFile(root).Save(sessionRegistry(testManagedContextID)); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	previous := placementOnlySnapshot("2", testManagedContextID)
	if err := sessionstate.LayoutFile(root).Save(previous); err != nil {
		t.Fatalf("save layout: %v", err)
	}

	runtime, err := newSessionRuntime(&recordingRequester{})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	emptyTree := daemonTree("1")
	start := time.Unix(300, 0)
	if refresh, err := runtime.Reconcile(emptyTree, start); err != nil || refresh {
		t.Fatalf("reconcile startup empty tree: refresh=%v err=%v", refresh, err)
	}
	deadline, scheduled := runtime.Deadline()
	if !scheduled || deadline != start.Add(sessionObservationDelay) {
		t.Fatalf("startup empty tree did not schedule periodic re-observation: deadline=%v scheduled=%v", deadline, scheduled)
	}

	runtime.Shutdown()
	if err := runtime.Flush(time.Unix(400, 0)); err != nil {
		t.Fatalf("flush after shutdown: %v", err)
	}
	var persisted sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutFile(root).LoadInto(&persisted); err != nil {
		t.Fatalf("load preserved layout: %v", err)
	}
	if persisted.Workspaces[0].Name != "2" {
		t.Fatalf("startup or shutdown guard replaced prior state: %+v", persisted)
	}
}

func TestSessionRuntimeSettlingTimeoutUnblocksCompleteWorkspaces(t *testing.T) {
	secondID := sessionstate.ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "sway-session")
	if err := sessionstate.RegistryFile(root).Save(sessionRegistryIDs(testManagedContextID, secondID)); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	previous := sessionstate.LayoutSnapshot{
		Version: sessionstate.LayoutSchemaVersion,
		Workspaces: []sessionstate.WorkspaceLayout{
			placementOnlySnapshot("2", testManagedContextID).Workspaces[0],
			placementOnlySnapshot("3", secondID).Workspaces[0],
		},
	}
	if err := sessionstate.LayoutFile(root).Save(previous); err != nil {
		t.Fatalf("save layout: %v", err)
	}
	runtime, err := newSessionRuntime(&recordingRequester{})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	start := time.Unix(350, 0)
	partial := daemonTree("8: changed", managedDaemonLeaf(t, 42, secondID))
	if _, err := runtime.Reconcile(partial, start); err != nil {
		t.Fatalf("observe partial startup: %v", err)
	}
	if runtime.startupComplete {
		t.Fatal("partial startup completed before settling timeout")
	}
	if _, err := runtime.Reconcile(partial, start.Add(sessionStartupSettleDelay)); err != nil {
		t.Fatalf("observe after settling timeout: %v", err)
	}
	if !runtime.startupComplete {
		t.Fatal("settling timeout did not unblock capture")
	}
	if _, scheduled := runtime.debouncer.Deadline(); !scheduled {
		t.Fatal("safe post-timeout snapshot was not scheduled")
	}
}

func TestSessionRuntimeRestoresContextWhichMapsAfterStartupTimeout(t *testing.T) {
	secondID := sessionstate.ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "sway-session")
	if err := sessionstate.RegistryFile(root).Save(sessionRegistryIDs(testManagedContextID, secondID)); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	previous := exactDaemonSnapshot("2", testManagedContextID, secondID)
	previous.Workspaces[0].Tiling.Layout = sessionstate.LayoutStacked
	if err := sessionstate.LayoutFile(root).Save(previous); err != nil {
		t.Fatalf("save layout: %v", err)
	}
	requester := &recordingRequester{}
	runtime, err := newSessionRuntime(requester)
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	start := time.Unix(375, 0)
	firstAppID, _ := testManagedContextID.AppID()
	firstUnmarked := &Node{ID: 41, Name: "first", Type: "con", AppID: &firstAppID}
	if refresh, err := runtime.Reconcile(daemonTree("2", firstUnmarked), start); err != nil || !refresh {
		t.Fatalf("mark first startup window: refresh=%v err=%v", refresh, err)
	}
	firstMarked := managedDaemonLeaf(t, 41, testManagedContextID)
	if refresh, err := runtime.Reconcile(daemonTree("2", firstMarked), start.Add(sessionStartupSettleDelay)); err != nil || refresh {
		t.Fatalf("finish partial startup: refresh=%v err=%v", refresh, err)
	}
	if !runtime.startupComplete {
		t.Fatal("partial startup did not finish at its settling deadline")
	}
	runtime.restoreExcluded["2"] = struct{}{}

	secondAppID, _ := secondID.AppID()
	secondUnmarked := &Node{ID: 42, Name: "second", Type: "con", AppID: &secondAppID}
	if refresh, err := runtime.Reconcile(daemonTree("2", firstMarked, secondUnmarked), start.Add(11*time.Second)); err != nil || !refresh {
		t.Fatalf("mark late startup window: refresh=%v err=%v", refresh, err)
	}
	if !runtime.lateRestorePending {
		t.Fatal("late startup window did not re-arm layout restoration")
	}
	if _, excluded := runtime.restoreExcluded["2"]; excluded {
		t.Fatal("late startup window left its workspace excluded from restoration")
	}
	secondMarked := managedDaemonLeaf(t, 42, secondID)
	refresh, err := runtime.Reconcile(daemonTree("2", firstMarked, secondMarked), start.Add(12*time.Second))
	if err != nil || !refresh {
		t.Fatalf("restore late complete workspace: refresh=%v err=%v", refresh, err)
	}
	last := requester.commands[len(requester.commands)-1]
	if !strings.Contains(last, "move container to workspace \""+sessionstate.RestoreStagingWorkspace+"\"") {
		t.Fatalf("late complete workspace did not begin structural restore: %v", requester.commands)
	}
}

func TestSessionRuntimeRequestsFreshTreeAfterUnknownMoveOutcome(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "sway-session")
	if err := sessionstate.RegistryFile(root).Save(sessionRegistry(testManagedContextID)); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	if err := sessionstate.LayoutFile(root).Save(placementOnlySnapshot("9", testManagedContextID)); err != nil {
		t.Fatalf("save layout: %v", err)
	}
	want := &swayipc.CommandOutcomeUnknownError{Cause: errors.New("connection lost")}
	requester := &recordingRequester{failAt: 1, failure: want}
	runtime, err := newSessionRuntime(requester)
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	appID, _ := testManagedContextID.AppID()
	refresh, err := runtime.Reconcile(daemonTree("1", &Node{ID: 42, Type: "con", AppID: &appID}), time.Now())
	if !refresh || !errors.Is(err, want.Cause) {
		t.Fatalf("unknown command outcome did not request observation: refresh=%v err=%v", refresh, err)
	}
}

func TestSessionRuntimeKeepsRestoreEligibilityAfterUnknownMarkOutcome(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "sway-session")
	if err := sessionstate.RegistryFile(root).Save(sessionRegistry(testManagedContextID)); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	if err := sessionstate.LayoutFile(root).Save(placementOnlySnapshot("9", testManagedContextID)); err != nil {
		t.Fatalf("save layout: %v", err)
	}
	want := &swayipc.CommandOutcomeUnknownError{Cause: errors.New("connection lost")}
	requester := &recordingRequester{failAt: 2, failure: want}
	runtime, err := newSessionRuntime(requester)
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	appID, _ := testManagedContextID.AppID()
	refresh, err := runtime.Reconcile(daemonTree("1", &Node{ID: 42, Type: "con", AppID: &appID}), time.Now())
	if !refresh || !errors.Is(err, want.Cause) {
		t.Fatalf("unknown mark outcome did not request observation: refresh=%v err=%v", refresh, err)
	}
	if _, eligible := runtime.restoreEligible[testManagedContextID]; !eligible {
		t.Fatal("ambiguous mark response lost new-window restore eligibility")
	}
}

func TestSessionRuntimeDoesNotUseUnpersistedDegradationAsMergeBase(t *testing.T) {
	secondID := sessionstate.ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "sway-session")
	registry := sessionRegistryIDs(testManagedContextID, secondID)
	if err := sessionstate.RegistryFile(root).Save(registry); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	previous := exactDaemonSnapshot("2", testManagedContextID, secondID)
	if err := sessionstate.LayoutFile(root).Save(previous); err != nil {
		t.Fatalf("save exact layout: %v", err)
	}
	runtime, err := newSessionRuntime(&recordingRequester{})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	start := time.Unix(500, 0)
	originalTree := daemonTree("2",
		managedDaemonLeaf(t, 41, testManagedContextID),
		managedDaemonLeaf(t, 42, secondID),
	)
	if _, err := runtime.Reconcile(originalTree, start); err != nil {
		t.Fatalf("complete startup: %v", err)
	}
	divergentTree := daemonTree("7", managedDaemonLeaf(t, 42, secondID))
	if _, err := runtime.Reconcile(divergentTree, start.Add(time.Second)); err != nil {
		t.Fatalf("observe transient degradation: %v", err)
	}
	if _, scheduled := runtime.Deadline(); !scheduled {
		t.Fatal("transient divergent state was not tracked")
	}
	if _, err := runtime.Reconcile(originalTree, start.Add(1500*time.Millisecond)); err != nil {
		t.Fatalf("observe restored exact state: %v", err)
	}
	if _, scheduled := runtime.debouncer.Deadline(); scheduled {
		t.Fatal("reverted transient degradation remained scheduled")
	}
}

func TestSessionRuntimeDoesNotPersistImmediateFailedRestoreDegradation(t *testing.T) {
	secondID := sessionstate.ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "sway-session")
	if err := sessionstate.RegistryFile(root).Save(sessionRegistryIDs(testManagedContextID, secondID)); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	previous := exactDaemonSnapshot("2", testManagedContextID, secondID)
	if err := sessionstate.LayoutFile(root).Save(previous); err != nil {
		t.Fatalf("save exact layout: %v", err)
	}
	runtime, err := newSessionRuntime(&recordingRequester{})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	runtime.startupComplete = true
	runtime.restoreFailures["2"] = errors.New("restore detail failed")
	appID := "firefox"
	mixed := daemonTree("2",
		managedDaemonLeaf(t, 41, testManagedContextID),
		&Node{ID: 99, Name: "browser", Type: "con", AppID: &appID},
		managedDaemonLeaf(t, 42, secondID),
	)
	if refresh, err := runtime.Reconcile(mixed, time.Unix(550, 0)); err != nil || refresh {
		t.Fatalf("capture failed restore result: refresh=%v err=%v", refresh, err)
	}
	if _, scheduled := runtime.debouncer.Deadline(); scheduled {
		t.Fatal("immediate failed restore degradation was scheduled over the last-good layout")
	}
}

func TestSessionRuntimeRendersRestoreCommandsWithoutShellEvaluation(t *testing.T) {
	tests := []struct {
		name   string
		action sessionstate.RestoreAction
		want   string
	}{
		{name: "move workspace", action: sessionstate.RestoreAction{Kind: sessionstate.RestoreMoveWorkspace, ContainerID: 11, Target: `2: work "quoted"`}, want: `[con_id=11] move container to workspace "2: work \"quoted\""`},
		{name: "split horizontal", action: sessionstate.RestoreAction{Kind: sessionstate.RestoreSplit, ContainerID: 11, Layout: sessionstate.LayoutSplitHorizontal}, want: `[con_id=11] split horizontal`},
		{name: "split vertical", action: sessionstate.RestoreAction{Kind: sessionstate.RestoreSplit, ContainerID: 11, Layout: sessionstate.LayoutSplitVertical}, want: `[con_id=11] split vertical`},
		{name: "stacked layout", action: sessionstate.RestoreAction{Kind: sessionstate.RestoreSetLayout, ContainerID: 11, Layout: sessionstate.LayoutStacked}, want: `[con_id=11] layout stacking`},
		{name: "temporary mark", action: sessionstate.RestoreAction{Kind: sessionstate.RestoreAddTemporaryMark, ContainerID: 11, Target: `_restore"mark`}, want: `[con_id=11] mark --add "_restore\"mark"`},
		{name: "remove mark", action: sessionstate.RestoreAction{Kind: sessionstate.RestoreRemoveMark, ContainerID: 11, Target: `_restore"mark`}, want: `[con_id=11] unmark "_restore\"mark"`},
		{name: "move mark", action: sessionstate.RestoreAction{Kind: sessionstate.RestoreMoveToMark, ContainerID: 11, Target: `_restore"mark`}, want: `[con_id=11] move container to mark "_restore\"mark"`},
		{name: "floating", action: sessionstate.RestoreAction{Kind: sessionstate.RestoreSetFloating, ContainerID: 11, Enable: true}, want: `[con_id=11] floating enable`},
		{name: "proportion", action: sessionstate.RestoreAction{Kind: sessionstate.RestoreSetProportion, ContainerID: 11, Axis: "width", Amount: 70}, want: `[con_id=11] resize set width 70 ppt`},
		{name: "floating size", action: sessionstate.RestoreAction{Kind: sessionstate.RestoreResizeFloating, ContainerID: 11, Geometry: sessionstate.Geometry{Width: 800, Height: 600}}, want: `[con_id=11] resize set 800 px 600 px`},
		{name: "floating position", action: sessionstate.RestoreAction{Kind: sessionstate.RestoreMoveFloating, ContainerID: 11, Geometry: sessionstate.Geometry{X: -20, Y: 40}}, want: `[con_id=11] move absolute position -20 px 40 px`},
		{name: "workspace fullscreen", action: sessionstate.RestoreAction{Kind: sessionstate.RestoreSetFullscreen, ContainerID: 11, Fullscreen: sessionstate.FullscreenWorkspace}, want: `[con_id=11] fullscreen enable`},
		{name: "global fullscreen", action: sessionstate.RestoreAction{Kind: sessionstate.RestoreSetFullscreen, ContainerID: 11, Fullscreen: sessionstate.FullscreenGlobal}, want: `[con_id=11] fullscreen enable global`},
		{name: "disable fullscreen", action: sessionstate.RestoreAction{Kind: sessionstate.RestoreSetFullscreen, ContainerID: 11}, want: `[con_id=11] fullscreen disable`},
		{name: "focus", action: sessionstate.RestoreAction{Kind: sessionstate.RestoreFocus, ContainerID: 11}, want: `[con_id=11] focus`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requester := &recordingRequester{}
			runtime := &sessionRuntime{client: requester}
			if err := runtime.applyRestoreAction(test.action); err != nil {
				t.Fatalf("apply restore action: %v", err)
			}
			if len(requester.commands) != 1 || requester.commands[0] != test.want {
				t.Fatalf("unexpected command: got %v want %q", requester.commands, test.want)
			}
		})
	}
}

func TestSessionRuntimeReobservesAmbiguousRestoreCommand(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "sway-session")
	secondID := sessionstate.ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if err := sessionstate.RegistryFile(root).Save(sessionRegistryIDs(testManagedContextID, secondID)); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	desired := exactDaemonSnapshot("2", testManagedContextID, secondID)
	desired.Workspaces[0].Tiling.Children[0].Proportion = 0.7
	desired.Workspaces[0].Tiling.Children[1].Proportion = 0.3
	want := &swayipc.CommandOutcomeUnknownError{Cause: errors.New("connection lost")}
	runtime := &sessionRuntime{
		client:            &recordingRequester{failAt: 1, failure: want},
		root:              root,
		persisted:         desired,
		restoreEligible:   map[sessionstate.ContextID]struct{}{testManagedContextID: {}},
		restoreExcluded:   make(map[string]struct{}),
		restoreSkipped:    make(map[string]struct{}),
		restoreFailures:   make(map[string]error),
		originalFocusDone: true,
	}
	first := managedDaemonLeaf(t, 41, testManagedContextID)
	second := managedDaemonLeaf(t, 42, secondID)
	half := 0.5
	first.Percent = &half
	second.Percent = &half

	refresh, done, err := runtime.restoreStartupLayout(daemonTree("2", first, second))
	if !refresh || done || !errors.Is(err, want.Cause) {
		t.Fatalf("ambiguous restore did not request re-observation: refresh=%v done=%v err=%v", refresh, done, err)
	}
	if runtime.restoreProgress == nil || runtime.restoreProgress.Phase != sessionstate.RestoreBuild {
		t.Fatalf("ambiguous restore lost resumable progress: %+v", runtime.restoreProgress)
	}
}

func TestSessionRuntimeIsolatesDetailAndStructuralRestoreFailures(t *testing.T) {
	secondID := sessionstate.ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	makeRuntime := func(t *testing.T, desired sessionstate.LayoutSnapshot) *sessionRuntime {
		t.Helper()
		stateHome := filepath.Join(t.TempDir(), "state")
		root := filepath.Join(stateHome, "sway-session")
		if err := sessionstate.RegistryFile(root).Save(sessionRegistryIDs(testManagedContextID, secondID)); err != nil {
			t.Fatalf("save registry: %v", err)
		}
		return &sessionRuntime{
			client:            &recordingRequester{failAt: 1, failure: errors.New("explicit rejection")},
			root:              root,
			persisted:         desired,
			restoreEligible:   map[sessionstate.ContextID]struct{}{testManagedContextID: {}},
			restoreExcluded:   make(map[string]struct{}),
			restoreSkipped:    make(map[string]struct{}),
			restoreFailures:   make(map[string]error),
			originalFocusDone: true,
		}
	}

	detailDesired := exactDaemonSnapshot("2", testManagedContextID, secondID)
	detailDesired.Workspaces[0].Tiling.Children[0].Proportion = 0.7
	detailDesired.Workspaces[0].Tiling.Children[1].Proportion = 0.3
	detailRuntime := makeRuntime(t, detailDesired)
	half := 0.5
	first := managedDaemonLeaf(t, 41, testManagedContextID)
	second := managedDaemonLeaf(t, 42, secondID)
	first.Percent = &half
	second.Percent = &half
	refresh, done, err := detailRuntime.restoreStartupLayout(daemonTree("2", first, second))
	if !refresh || done || err == nil {
		t.Fatalf("detail failure did not request continued restore: refresh=%v done=%v err=%v", refresh, done, err)
	}
	if detailRuntime.restoreProgress == nil || detailRuntime.restoreProgress.Phase != sessionstate.RestoreBuild ||
		len(detailRuntime.restoreSkipped) != 1 {
		t.Fatalf("detail failure escalated to workspace rollback: progress=%+v skipped=%v", detailRuntime.restoreProgress, detailRuntime.restoreSkipped)
	}

	structuralDesired := exactDaemonSnapshot("2", testManagedContextID, secondID)
	structuralDesired.Workspaces[0].Tiling.Layout = sessionstate.LayoutSplitVertical
	structuralRuntime := makeRuntime(t, structuralDesired)
	refresh, done, err = structuralRuntime.restoreStartupLayout(daemonTree("2",
		managedDaemonLeaf(t, 51, testManagedContextID),
		managedDaemonLeaf(t, 52, secondID),
	))
	if !refresh || done || err == nil {
		t.Fatalf("structural failure did not start rollback: refresh=%v done=%v err=%v", refresh, done, err)
	}
	if structuralRuntime.restoreProgress == nil || structuralRuntime.restoreProgress.Phase != sessionstate.RestoreRollbackOut ||
		structuralRuntime.restoreProgress.Steps != 0 {
		t.Fatalf("structural failure was not isolated by rollback: %+v", structuralRuntime.restoreProgress)
	}
}

func TestSessionRuntimeRollbackGetsIndependentStepBudget(t *testing.T) {
	runtime := &sessionRuntime{
		restoreProgress: &sessionstate.RestoreProgress{
			Workspace: "2",
			Phase:     sessionstate.RestoreBuild,
			Steps:     1024,
		},
		restoreFailures: make(map[string]error),
	}
	refresh, done, err := runtime.beginRestoreRollback(errors.New("build budget exhausted"))
	if !refresh || done || err == nil {
		t.Fatalf("rollback was not started: refresh=%v done=%v err=%v", refresh, done, err)
	}
	if runtime.restoreProgress.Phase != sessionstate.RestoreRollbackOut || runtime.restoreProgress.Steps != 0 {
		t.Fatalf("rollback inherited exhausted build budget: %+v", runtime.restoreProgress)
	}
}

var testManagedContextID = sessionstate.ContextID("123e4567-e89b-12d3-a456-426614174000")

func sessionRegistry(id sessionstate.ContextID) sessionstate.Registry {
	return sessionRegistryIDs(id)
}

func sessionRegistryIDs(ids ...sessionstate.ContextID) sessionstate.Registry {
	contexts := make([]sessionstate.Context, 0, len(ids))
	for index, id := range ids {
		contexts = append(contexts, sessionstate.Context{
			ID:    id,
			State: sessionstate.ContextActive,
			Launcher: sessionstate.Launcher{
				Kind:    sessionstate.LauncherHerdr,
				Session: fmt.Sprintf("test-session-%d", index),
				Cwd:     "/work",
			},
		})
	}
	return sessionstate.Registry{
		Version:  sessionstate.ContextsSchemaVersion,
		Contexts: contexts,
	}
}

func placementOnlySnapshot(workspace string, id sessionstate.ContextID) sessionstate.LayoutSnapshot {
	return sessionstate.LayoutSnapshot{
		Version: sessionstate.LayoutSchemaVersion,
		Workspaces: []sessionstate.WorkspaceLayout{{
			Name:              workspace,
			RestoreMode:       sessionstate.WorkspaceRestorePlacementOnly,
			PlacementContexts: []sessionstate.ContextID{id},
		}},
	}
}

func exactDaemonSnapshot(workspace string, ids ...sessionstate.ContextID) sessionstate.LayoutSnapshot {
	children := make([]sessionstate.LayoutNode, 0, len(ids))
	for _, id := range ids {
		id := id
		children = append(children, sessionstate.LayoutNode{ContextID: &id})
	}
	return sessionstate.LayoutSnapshot{
		Version: sessionstate.LayoutSchemaVersion,
		Workspaces: []sessionstate.WorkspaceLayout{{
			Name:        workspace,
			RestoreMode: sessionstate.WorkspaceRestoreLayout,
			Tiling: &sessionstate.LayoutNode{
				Layout:   sessionstate.LayoutSplitHorizontal,
				Children: children,
			},
		}},
	}
}

func daemonTree(workspace string, nodes ...*Node) *Node {
	return &Node{ID: 1, Type: "root", Nodes: []*Node{{
		ID: 2, Type: "output", Nodes: []*Node{{
			ID: 3, Name: workspace, Type: "workspace", Layout: "splith", Nodes: nodes,
		}},
	}}}
}

func managedDaemonLeaf(t *testing.T, containerID int64, id sessionstate.ContextID) *Node {
	t.Helper()
	mark, err := id.Mark()
	if err != nil {
		t.Fatalf("derive mark: %v", err)
	}
	appID, err := id.AppID()
	if err != nil {
		t.Fatalf("derive app ID: %v", err)
	}
	return &Node{ID: containerID, Name: "terminal", Type: "con", AppID: &appID, Marks: []string{mark}}
}
