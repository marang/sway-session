package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
	"github.com/marang/sway-title-animator/internal/statefile"
	"github.com/marang/sway-title-animator/internal/swayipc"
)

type recordingRequester struct {
	commands []string
	barriers []string
	failAt   int
	failure  error
}

type mutableEventStreamGuard struct {
	epoch     uint64
	connected bool
}

func (guard *mutableEventStreamGuard) Snapshot() (uint64, bool) {
	return guard.epoch, guard.connected
}

type disconnectingDaemonRequester struct {
	guard    *mutableEventStreamGuard
	commands int
	barriers int
}

func (requester *disconnectingDaemonRequester) RequestContext(ctx context.Context, messageType swayipc.MessageType, _ []byte) (swayipc.Message, error) {
	if err := ctx.Err(); err != nil {
		return swayipc.Message{}, err
	}
	switch messageType {
	case swayipc.RunCommand:
		requester.commands++
		if requester.commands == 1 {
			requester.guard.epoch = 2
			requester.guard.connected = false
		}
		return swayipc.Message{Type: swayipc.RunCommand, Payload: []byte(`[{"success":true}]`)}, nil
	case swayipc.SendTick:
		requester.barriers++
		return swayipc.Message{Type: swayipc.SendTick, Payload: []byte(`{"success":true}`)}, nil
	default:
		return swayipc.Message{}, errors.New("unexpected fake Sway request")
	}
}

func (*disconnectingDaemonRequester) Close() {}

type recordingApplicationLauncher struct {
	contexts     []sessionstate.Context
	prepareErr   error
	prepareErrBy map[sessionstate.ContextID]error
	startErr     error
	starts       int
}

type recordingPreparedApplicationLaunch struct {
	launcher *recordingApplicationLauncher
	context  sessionstate.Context
}

func (launch recordingPreparedApplicationLaunch) Start() error {
	launch.launcher.starts++
	if launch.launcher.startErr != nil {
		return launch.launcher.startErr
	}
	launch.launcher.contexts = append(launch.launcher.contexts, launch.context)
	return nil
}

func (launcher *recordingApplicationLauncher) Prepare(ctx context.Context, context sessionstate.Context) (preparedApplicationLaunch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if launcher.prepareErr != nil {
		return nil, launcher.prepareErr
	}
	if err := launcher.prepareErrBy[context.ID]; err != nil {
		return nil, err
	}
	return recordingPreparedApplicationLaunch{launcher: launcher, context: context}, nil
}

func (*recordingRequester) Close() {}

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

func TestSessionRuntimeUserActivityCancelsConflictingRestoreAndFocusReset(t *testing.T) {
	runtime := &sessionRuntime{
		persisted: sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{{
			Name: "98: apps", RestoreMode: sessionstate.WorkspaceRestorePlacementOnly, PlacementContexts: []sessionstate.ContextID{testManagedContextID},
		}}},
		restoreProgress: &sessionstate.RestoreProgress{Workspace: "98: apps", Phase: sessionstate.RestoreBuild},
		restoreExcluded: map[string]struct{}{},
	}

	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding, Change: "run"}, time.Now())

	if runtime.restoreProgress != nil || !runtime.originalFocusDone || !runtime.startupComplete {
		t.Fatalf("user activity did not cancel conflicting restore state: %+v", runtime)
	}
	if _, excluded := runtime.restoreExcluded["98: apps"]; !excluded {
		t.Fatal("canceled workspace remained eligible for structural restore")
	}
}

func TestSessionRuntimeFocusActivityCancelsConflictingRestore(t *testing.T) {
	for _, event := range []swayipc.Event{
		{Type: swayipc.EventWindow, Change: "focus"},
		{Type: swayipc.EventWorkspace, Change: "focus"},
	} {
		runtime := &sessionRuntime{
			persisted: sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{{
				Name: "98: apps", RestoreMode: sessionstate.WorkspaceRestorePlacementOnly, PlacementContexts: []sessionstate.ContextID{testManagedContextID},
			}}},
			restoreProgress: &sessionstate.RestoreProgress{Workspace: "98: apps", Phase: sessionstate.RestoreBuild},
			restoreExcluded: map[string]struct{}{},
		}

		runtime.HandleEvent(event, time.Now())

		if runtime.restoreProgress != nil || !runtime.originalFocusDone || !runtime.startupComplete {
			t.Fatalf("%s focus did not cancel conflicting restore state: %+v", event.Type, runtime)
		}
	}
}

func TestSessionRuntimeBatchesConfirmedTerminalFocusActivity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	registry := sessionRegistry(testManagedContextID)
	createdAt := time.Date(2026, 9, 3, 17, 0, 0, 0, time.UTC)
	if err := sessionstate.RegistryFile(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	if err := sessionstate.RecordTerminalCreationContext(t.Context(), root, testManagedContextID, createdAt); err != nil {
		t.Fatal(err)
	}
	registryBefore, err := os.ReadFile(filepath.Join(root, sessionstate.ContextsFilename))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := newSessionRuntimeWithOptions(&recordingRequester{}, sessionRuntimeOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}

	first := createdAt.Add(time.Hour)
	second := first.Add(500 * time.Millisecond)
	leaf := managedDaemonLeaf(t, 42, testManagedContextID)
	leaf.Focused = true
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: leaf}, first)
	deadline, scheduled := runtime.Deadline()
	if !scheduled || !deadline.Equal(first.Add(terminalFocusBatchDelay)) {
		t.Fatalf("focus batch deadline=%v scheduled=%t", deadline, scheduled)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: leaf}, second)
	if deadlineAfter, _ := runtime.Deadline(); !deadlineAfter.Equal(deadline) {
		t.Fatalf("focus storm postponed bounded deadline: first=%v after=%v", deadline, deadlineAfter)
	}

	activityBefore, err := sessionstate.ReadTerminalActivitySnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := sessionstate.FindTerminalActivity(activityBefore, testManagedContextID)
	if before.LastFocusedAt != nil {
		t.Fatalf("focus was persisted before batch deadline: %+v", before)
	}
	if err := runtime.Flush(deadline); err != nil {
		t.Fatal(err)
	}
	afterState, err := sessionstate.ReadTerminalActivitySnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := sessionstate.FindTerminalActivity(afterState, testManagedContextID)
	if after.LastFocusedAt == nil || !after.LastFocusedAt.Equal(second) {
		t.Fatalf("latest confirmed focus was not persisted: %+v", after)
	}
	registryAfter, err := os.ReadFile(filepath.Join(root, sessionstate.ContextsFilename))
	if err != nil || string(registryAfter) != string(registryBefore) {
		t.Fatalf("focus activity changed contexts.json: err=%v\nbefore=%s\nafter=%s", err, registryBefore, registryAfter)
	}
	if runtime.terminalFocusDeadline != (time.Time{}) || len(runtime.pendingTerminalFocus) != 0 {
		t.Fatalf("persisted focus batch remained armed: deadline=%v pending=%v", runtime.terminalFocusDeadline, runtime.pendingTerminalFocus)
	}
}

func TestSessionRuntimeFocusPrunesCapacityFullOrphanActivity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	if err := sessionstate.RegistryFile(root).Save(sessionRegistry(testManagedContextID)); err != nil {
		t.Fatal(err)
	}
	focusedAt := time.Date(2026, 9, 3, 18, 30, 0, 0, time.UTC)
	orphans := sessionstate.TerminalActivityState{
		Version:   sessionstate.TerminalActivitySchemaVersion,
		Terminals: make([]sessionstate.TerminalActivity, 0, sessionstate.MaxContexts),
	}
	for index := 0; index < sessionstate.MaxContexts; index++ {
		id := sessionstate.ContextID(fmt.Sprintf("%08x-1111-4111-8111-%012x", index+2000, index+2000))
		orphans.Terminals = append(orphans.Terminals, sessionstate.TerminalActivity{ContextID: id, LastFocusedAt: &focusedAt})
	}
	if err := sessionstate.TerminalActivityFile(root).Save(orphans); err != nil {
		t.Fatal(err)
	}
	runtime, err := newSessionRuntimeWithOptions(&recordingRequester{}, sessionRuntimeOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	observedAt := focusedAt.Add(time.Minute)
	runtime.HandleEvent(swayipc.Event{
		Type: swayipc.EventWindow, Change: "focus", Container: managedDaemonLeaf(t, 42, testManagedContextID),
	}, observedAt)
	if err := runtime.Flush(observedAt.Add(terminalFocusBatchDelay)); err != nil {
		t.Fatalf("capacity-full orphan focus did not converge: %v", err)
	}
	activity, err := sessionstate.ReadTerminalActivitySnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	recorded, exists := sessionstate.FindTerminalActivity(activity, testManagedContextID)
	if len(activity.Terminals) != 1 || !exists || recorded.LastFocusedAt == nil || !recorded.LastFocusedAt.Equal(observedAt) {
		t.Fatalf("orphan activity was not pruned to the focused terminal: %+v", activity)
	}
	if len(runtime.pendingTerminalFocus) != 0 || runtime.terminalFocusRetry != 0 || !runtime.terminalFocusDeadline.IsZero() {
		t.Fatalf("successful pruned focus retained retry state: pending=%v retry=%v deadline=%v", runtime.pendingTerminalFocus, runtime.terminalFocusRetry, runtime.terminalFocusDeadline)
	}
}

func TestSessionRuntimeIgnoresUntrustedOrNonTerminalFocusActivity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	desktop := sessionstate.Context{
		ID: testManagedContextID, Label: "Example", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherDesktop, DesktopID: "example.desktop", DesktopOrigin: sessionstate.DesktopEntrySystem, DesktopPath: "/usr/share/applications/example.desktop"},
		App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "example"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow},
	}
	if err := sessionstate.RegistryFile(root).Save(sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{desktop}}); err != nil {
		t.Fatal(err)
	}
	runtime, err := newSessionRuntimeWithOptions(&recordingRequester{}, sessionRuntimeOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	conflictingAppID, _ := sessionstate.ContextID("22222222-2222-4222-8222-222222222222").AppID()
	mark, _ := testManagedContextID.Mark()
	for _, event := range []swayipc.Event{
		{Type: swayipc.EventWorkspace, Change: "focus", Current: &Node{ID: 3}},
		{Type: swayipc.EventWindow, Change: "title", Container: managedDaemonLeaf(t, 42, testManagedContextID)},
		{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: 42, Marks: []string{mark}, AppID: &conflictingAppID}},
	} {
		runtime.HandleEvent(event, now)
	}
	if len(runtime.pendingTerminalFocus) != 0 {
		t.Fatalf("untrusted focus event was queued: %+v", runtime.pendingTerminalFocus)
	}

	desktopLeaf := managedDaemonLeaf(t, 43, testManagedContextID)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: desktopLeaf}, now)
	if err := runtime.Flush(now.Add(terminalFocusBatchDelay)); err != nil {
		t.Fatal(err)
	}
	after, err := sessionstate.ReadTerminalActivitySnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Terminals) != 0 {
		t.Fatalf("desktop focus was recorded as terminal activity: %+v", after)
	}
}

func TestSessionRuntimeFocusActivityUsesSilentBackoffDuringLifecycleContention(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	if err := sessionstate.RegistryFile(root).Save(sessionRegistry(testManagedContextID)); err != nil {
		t.Fatal(err)
	}
	if err := sessionstate.RecordTerminalCreationContext(t.Context(), root, testManagedContextID, time.Now()); err != nil {
		t.Fatal(err)
	}
	runtime, err := newSessionRuntimeWithOptions(&recordingRequester{}, sessionRuntimeOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	observedAt := time.Now().UTC()
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: managedDaemonLeaf(t, 42, testManagedContextID)}, observedAt)

	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- statefile.WithPrivateDirectoryLockContext(context.Background(), filepath.Join(root, sessionstate.TerminalActivityDirectory), func(*statefile.LockedPrivateDirectory) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	err = runtime.Flush(observedAt.Add(terminalFocusBatchDelay))
	if err != nil || len(runtime.pendingTerminalFocus) != 1 || runtime.terminalFocusRetry != terminalFocusBatchDelay {
		t.Fatalf("locked activity flush err=%v pending=%v", err, runtime.pendingTerminalFocus)
	}
	firstRetry := runtime.terminalFocusDeadline
	if err := runtime.Flush(firstRetry); err != nil || runtime.terminalFocusRetry != 2*terminalFocusBatchDelay ||
		!runtime.terminalFocusDeadline.Equal(firstRetry.Add(2*terminalFocusBatchDelay)) {
		t.Fatalf("sustained contention did not back off silently: err=%v delay=%v deadline=%v", err, runtime.terminalFocusRetry, runtime.terminalFocusDeadline)
	}
	close(release)
	if lockErr := <-done; lockErr != nil {
		t.Fatal(lockErr)
	}
	if err := runtime.Flush(runtime.terminalFocusDeadline); err != nil {
		t.Fatal(err)
	}
	activity, err := sessionstate.ReadTerminalActivitySnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	recorded, exists := sessionstate.FindTerminalActivity(activity, testManagedContextID)
	if !exists || recorded.LastFocusedAt == nil || !recorded.LastFocusedAt.Equal(observedAt) || len(runtime.pendingTerminalFocus) != 0 {
		t.Fatalf("focus retry did not converge: activity=%+v pending=%v", activity, runtime.pendingTerminalFocus)
	}
	if runtime.terminalFocusRetry != 0 || !runtime.terminalFocusReported.IsZero() {
		t.Fatalf("successful focus persistence retained backoff state: delay=%v reported=%v", runtime.terminalFocusRetry, runtime.terminalFocusReported)
	}
}

func TestSessionRuntimeDelayedExpectedMovePreservesRestoreThenNextMoveCancels(t *testing.T) {
	now := time.Now()
	runtime := &sessionRuntime{
		persisted: sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{{
			Name: "98: apps", RestoreMode: sessionstate.WorkspaceRestorePlacementOnly, PlacementContexts: []sessionstate.ContextID{testManagedContextID},
		}}},
		restoreProgress: &sessionstate.RestoreProgress{Workspace: "98: apps", Phase: sessionstate.RestoreBuild},
		restoreExcluded: map[string]struct{}{},
	}
	runtime.expectMove(41)

	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "move", Container: &Node{ID: 41}}, now.Add(time.Hour))

	if runtime.restoreProgress == nil {
		t.Fatal("delayed daemon move was mistaken for later user intent")
	}
	if len(runtime.expectedMoves) != 0 {
		t.Fatalf("consumed daemon move remained pending: %+v", runtime.expectedMoves)
	}

	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "move", Container: &Node{ID: 41}}, now.Add(2*time.Hour))

	if runtime.restoreProgress != nil {
		t.Fatal("move after the daemon-generated event did not cancel restore")
	}
}

func TestSessionRuntimeReconnectInvalidatesStaleExpectedMove(t *testing.T) {
	runtime := &sessionRuntime{
		persisted: sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{{
			Name: "98: apps", RestoreMode: sessionstate.WorkspaceRestorePlacementOnly, PlacementContexts: []sessionstate.ContextID{testManagedContextID},
		}}},
		restoreProgress: &sessionstate.RestoreProgress{Workspace: "98: apps", Phase: sessionstate.RestoreBuild},
		restoreExcluded: map[string]struct{}{},
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready"}, time.Now())
	if runtime.restoreProgress == nil {
		t.Fatal("initial event-stream readiness canceled startup restore")
	}
	runtime.expectMove(41)

	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready"}, time.Now())
	if runtime.restoreProgress != nil {
		t.Fatal("event-stream reconnect continued restore despite possibly lost user intent")
	}
	if len(runtime.expectedMoves) != 0 {
		t.Fatalf("reconnect retained stale move expectations: %+v", runtime.expectedMoves)
	}
}

func TestSessionRuntimeStreamLossDuringStartupSettlingCancelsRestore(t *testing.T) {
	for _, transition := range []swayipc.Event{
		{Type: swayipc.EventStream, Change: "disconnected"},
		{Type: swayipc.EventStream, Change: "ready"},
	} {
		t.Run(transition.Change, func(t *testing.T) {
			runtime := &sessionRuntime{
				persisted: sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{{
					Name: "98: apps", RestoreMode: sessionstate.WorkspaceRestorePlacementOnly, PlacementContexts: []sessionstate.ContextID{testManagedContextID},
				}}},
				startupDeadline: time.Now().Add(sessionStartupSettleDelay),
				restoreExcluded: map[string]struct{}{},
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready"}, time.Now())
			if runtime.startupComplete {
				t.Fatal("initial event-stream readiness canceled startup settling")
			}

			runtime.HandleEvent(transition, time.Now())

			if !runtime.startupComplete || !runtime.startupDeadline.IsZero() || !runtime.originalFocusDone {
				t.Fatalf("stream loss left startup restore active: %+v", runtime)
			}
			if _, excluded := runtime.restoreExcluded["98: apps"]; !excluded {
				t.Fatal("stream loss left startup workspace eligible for restoration")
			}
		})
	}
}

func TestSessionRuntimeStopsMutatingWhenStreamDisconnectsDuringReconcile(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	setSessionTestStateHome(t, stateHome)
	root := filepath.Join(stateHome, "sway-session")
	if err := sessionstate.RegistryFile(root).Save(sessionRegistry(testManagedContextID)); err != nil {
		t.Fatal(err)
	}
	if err := sessionstate.LayoutFile(root).Save(placementOnlySnapshot("98: saved", testManagedContextID)); err != nil {
		t.Fatal(err)
	}
	guard := &mutableEventStreamGuard{epoch: 1, connected: true}
	requester := &disconnectingDaemonRequester{guard: guard}
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{Root: root, EventStreamState: guard})
	if err != nil {
		t.Fatal(err)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 1}, time.Now())
	appID, _ := testManagedContextID.AppID()

	_, err = runtime.Reconcile(daemonTree("99: current", &Node{ID: 41, Type: "con", AppID: &appID}), time.Now())

	if err == nil || !strings.Contains(err.Error(), "event stream changed") {
		t.Fatalf("disconnecting reconciliation returned %v", err)
	}
	if requester.commands != 1 || requester.barriers != 0 {
		t.Fatalf("stream loss allowed later mutations: commands=%d barriers=%d", requester.commands, requester.barriers)
	}
	if !runtime.startupComplete || runtime.restoreProgress != nil {
		t.Fatalf("stream loss left startup restoration active: %+v", runtime)
	}
}

func TestSessionRuntimeAmbiguousMoveOutcomeStopsRestoreAndExposesLaterUserMove(t *testing.T) {
	for _, failure := range []error{
		&swayipc.CommandOutcomeUnknownError{Cause: errors.New("connection lost")},
		&swayipc.CommandResponseInvalidError{Cause: errors.New("malformed response")},
	} {
		runtime := &sessionRuntime{
			client: &recordingRequester{failAt: 1, failure: failure},
			persisted: sessionstate.LayoutSnapshot{
				Version: sessionstate.LayoutSchemaVersion,
				Workspaces: []sessionstate.WorkspaceLayout{{
					Name:              "98: apps",
					RestoreMode:       sessionstate.WorkspaceRestorePlacementOnly,
					PlacementContexts: []sessionstate.ContextID{testManagedContextID},
				}},
			},
			restoreProgress: &sessionstate.RestoreProgress{Workspace: "98: apps", Phase: sessionstate.RestoreBuild},
			restoreExcluded: map[string]struct{}{},
		}

		err := runtime.applyPlacementAction(sessionstate.PlacementAction{
			Kind:        sessionstate.PlacementMoveWorkspace,
			ContextID:   testManagedContextID,
			ContainerID: 41,
			Workspace:   "98: apps",
		})
		if !errors.Is(err, failure) {
			t.Fatalf("ambiguous move returned %v, want %v", err, failure)
		}
		if runtime.restoreProgress != nil {
			t.Fatal("ambiguous move outcome left conflicting restore active")
		}
		if len(runtime.expectedMoves) != 0 || runtime.consumeExpectedMove(41) {
			t.Fatalf("ambiguous move could hide a later user move: %+v", runtime.expectedMoves)
		}
	}
}

func TestSessionRuntimeNoOpMoveBarrierExpiresAttributionBeforeLaterUserMove(t *testing.T) {
	requester := &recordingRequester{}
	runtime := &sessionRuntime{
		client: requester,
		persisted: sessionstate.LayoutSnapshot{
			Version: sessionstate.LayoutSchemaVersion,
			Workspaces: []sessionstate.WorkspaceLayout{{
				Name:              "98: apps",
				RestoreMode:       sessionstate.WorkspaceRestorePlacementOnly,
				PlacementContexts: []sessionstate.ContextID{testManagedContextID},
			}},
		},
		restoreProgress: &sessionstate.RestoreProgress{Workspace: "98: apps", Phase: sessionstate.RestoreBuild},
		restoreExcluded: map[string]struct{}{},
	}

	if err := runtime.applyPlacementAction(sessionstate.PlacementAction{
		Kind:        sessionstate.PlacementMoveWorkspace,
		ContextID:   testManagedContextID,
		ContainerID: 41,
		Workspace:   "98: apps",
	}); err != nil {
		t.Fatalf("apply no-op move: %v", err)
	}
	if len(requester.barriers) != 1 {
		t.Fatalf("move emitted %d attribution barriers, want 1", len(requester.barriers))
	}

	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventTick, Payload: requester.barriers[0]}, time.Now())
	if runtime.restoreProgress == nil {
		t.Fatal("attribution barrier itself canceled restore")
	}
	if len(runtime.expectedMoves) != 0 {
		t.Fatalf("no-op move remained attributed after barrier: %+v", runtime.expectedMoves)
	}

	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "move", Container: &Node{ID: 41}}, time.Now())
	if runtime.restoreProgress != nil {
		t.Fatal("later user move was hidden by no-op daemon move")
	}
}

func (requester *recordingRequester) RequestContext(ctx context.Context, messageType swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if err := ctx.Err(); err != nil {
		return swayipc.Message{}, err
	}
	if messageType == swayipc.SendTick {
		requester.barriers = append(requester.barriers, string(payload))
		return swayipc.Message{Type: swayipc.SendTick, Payload: []byte(`{"success":true}`)}, nil
	}
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
	setSessionTestStateHome(t, stateHome)
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

func TestSessionRuntimeConfirmedPlacementFailureDoesNotStarveLaterContext(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	secondID := sessionstate.ContextID("22222222-2222-4222-8222-222222222222")
	if err := sessionstate.RegistryFile(root).Save(sessionRegistryIDs(testManagedContextID, secondID)); err != nil {
		t.Fatal(err)
	}
	requester := &recordingRequester{failAt: 1, failure: errors.New("explicit rejection")}
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	firstAppID, _ := testManagedContextID.AppID()
	secondAppID, _ := secondID.AppID()
	tree := daemonTree("98: placement",
		&Node{ID: 41, Name: "first", Type: "con", AppID: &firstAppID},
		&Node{ID: 42, Name: "second", Type: "con", AppID: &secondAppID},
	)

	refresh, err := runtime.Reconcile(tree, time.Unix(100, 0))
	if !refresh || err == nil {
		t.Fatalf("confirmed failure was not degraded around: refresh=%v err=%v", refresh, err)
	}
	if len(requester.commands) != 2 || !strings.Contains(requester.commands[1], string(secondID)) {
		t.Fatalf("first rejected context starved later placement: %v", requester.commands)
	}
}

func TestSessionRuntimeRejectedMoveCannotReplaceLastGoodWorkspace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	registry := sessionRegistry(testManagedContextID)
	if err := sessionstate.RegistryFile(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	previous := placementOnlySnapshot("98: saved", testManagedContextID)
	if err := sessionstate.LayoutFile(root).Save(previous); err != nil {
		t.Fatal(err)
	}
	requester := &recordingRequester{failAt: 1, failure: errors.New("explicit rejection")}
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	runtime.startupComplete = true
	appID, _ := testManagedContextID.AppID()
	wrongWorkspace := daemonTree("99: landing", &Node{ID: 41, Name: "terminal", Type: "con", AppID: &appID})
	now := time.Unix(100, 0)

	refresh, err := runtime.Reconcile(wrongWorkspace, now)
	if refresh || err == nil {
		t.Fatalf("rejected move was not isolated as a confirmed failure: refresh=%v err=%v", refresh, err)
	}
	if targets := snapshotWorkspaceTargets(runtime.desired); targets[testManagedContextID] != "98: saved" {
		t.Fatalf("rejected move replaced desired workspace: %+v", runtime.desired)
	}
	if err := runtime.Flush(now.Add(sessionSnapshotDebounce)); err != nil {
		t.Fatal(err)
	}
	var persisted sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutFile(root).LoadInto(&persisted); err != nil {
		t.Fatal(err)
	}
	if targets := snapshotWorkspaceTargets(persisted); targets[testManagedContextID] != "98: saved" {
		t.Fatalf("rejected move replaced persisted workspace: %+v", persisted)
	}
}

func TestSessionRuntimeRejectedMoveDoesNotBlockIndependentCapture(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	secondID := sessionstate.ContextID("22222222-2222-4222-8222-222222222222")
	if err := sessionstate.RegistryFile(root).Save(sessionRegistryIDs(testManagedContextID, secondID)); err != nil {
		t.Fatal(err)
	}
	previous := sessionstate.LayoutSnapshot{
		Version: sessionstate.LayoutSchemaVersion,
		Workspaces: []sessionstate.WorkspaceLayout{
			placementOnlySnapshot("98: saved", testManagedContextID).Workspaces[0],
			placementOnlySnapshot("97: previous", secondID).Workspaces[0],
		},
	}
	if err := sessionstate.LayoutFile(root).Save(previous); err != nil {
		t.Fatal(err)
	}
	requester := &recordingRequester{failAt: 1, failure: errors.New("explicit rejection")}
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	runtime.startupComplete = true
	firstAppID, _ := testManagedContextID.AppID()
	tree := daemonTree("99: landing", &Node{ID: 41, Name: "first", Type: "con", AppID: &firstAppID})
	tree.Nodes[0].Nodes = append(tree.Nodes[0].Nodes, daemonTree("96: current", managedDaemonLeaf(t, 42, secondID)).Nodes[0].Nodes[0])
	now := time.Unix(200, 0)

	refresh, err := runtime.Reconcile(tree, now)
	if refresh || err == nil {
		t.Fatalf("rejected move was not isolated as a confirmed failure: refresh=%v err=%v", refresh, err)
	}
	if err := runtime.Flush(now.Add(sessionSnapshotDebounce)); err != nil {
		t.Fatal(err)
	}
	var persisted sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutFile(root).LoadInto(&persisted); err != nil {
		t.Fatal(err)
	}
	targets := snapshotWorkspaceTargets(persisted)
	if targets[testManagedContextID] != "98: saved" || targets[secondID] != "96: current" {
		t.Fatalf("capture isolation targets = %+v, snapshot=%+v", targets, persisted)
	}
}

func TestSessionRuntimeConfirmedDesktopPlacementFailureDoesNotStarveLaterApplication(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	ids := []sessionstate.ContextID{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
	}
	contexts := make([]sessionstate.Context, 0, len(ids))
	for index, id := range ids {
		appID := fmt.Sprintf("org.example.App%d", index+1)
		contexts = append(contexts, sessionstate.Context{
			ID: id, Label: appID, Provider: "desktop", State: sessionstate.ContextActive,
			Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: appID, FlatpakInstallation: sessionstate.FlatpakUser},
			App: &sessionstate.Application{
				Identity:    sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: appID, SandboxAppID: appID},
				DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow,
			},
		})
	}
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: contexts}
	if err := sessionstate.RegistryFile(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	desired := sessionstate.LayoutSnapshot{
		Version: sessionstate.LayoutSchemaVersion,
		Workspaces: []sessionstate.WorkspaceLayout{{
			Name: "98: apps", RestoreMode: sessionstate.WorkspaceRestorePlacementOnly, PlacementContexts: ids,
		}},
	}
	if err := sessionstate.LayoutFile(root).Save(desired); err != nil {
		t.Fatal(err)
	}
	requester := &recordingRequester{failAt: 1, failure: errors.New("explicit rejection")}
	start := time.Unix(1000, 0)
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Root: root, CompositorID: strings.Repeat("d", 64), StartedAt: start,
		ApplicationLauncher: &recordingApplicationLauncher{},
		ApplicationRestore: sessionstate.ApplicationRestoreOptions{
			AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstID, secondID := contexts[0].App.Identity.WaylandAppID, contexts[1].App.Identity.WaylandAppID
	firstSandbox, secondSandbox := firstID, secondID
	tree := daemonTree("99: landing",
		&Node{ID: 51, Type: "con", AppID: &firstID, SandboxAppID: &firstSandbox},
		&Node{ID: 52, Type: "con", AppID: &secondID, SandboxAppID: &secondSandbox},
	)

	refresh, err := runtime.Reconcile(tree, start)
	if !refresh || err == nil {
		t.Fatalf("desktop placement failure was not degraded around: refresh=%v err=%v", refresh, err)
	}
	if len(requester.commands) != 3 || !strings.Contains(requester.commands[1], "con_id=52") || !strings.Contains(requester.commands[2], string(ids[1])) {
		t.Fatalf("first rejected application starved later placement: %v", requester.commands)
	}
}

func TestSessionRuntimePublishesIdempotentApplicationIndicatorMarks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	registry := sessionstate.Registry{
		Version:     sessionstate.ContextsSchemaVersion,
		Preferences: sessionstate.RegistryPreferences{DesktopIndicators: true},
		Contexts:    []sessionstate.Context{},
	}
	if err := sessionstate.RegistryFile(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	catalog := testDesktopCatalog(t, map[string]string{
		"org.example.App.desktop": "[Desktop Entry]\nType=Application\nName=Example\nExec=/usr/bin/true\n",
	}, false)
	requester := &recordingRequester{}
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Root: root,
		IndicatorCatalog: func() (sessionstate.DesktopCatalog, error) {
			return catalog, nil
		},
		IndicatorOperations: func() ([]sessionstate.ApplicationOperation, error) {
			return []sessionstate.ApplicationOperation{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	appID := "org.example.App"
	tree := daemonTree("98: apps", &Node{ID: 41, Name: "Example", Type: "con", AppID: &appID})

	refresh, err := runtime.ReconcileIndicators(tree)
	if err != nil || !refresh {
		t.Fatalf("publish indicator: refresh=%v err=%v", refresh, err)
	}
	if len(requester.commands) != 1 || !strings.Contains(requester.commands[0], `_sway_session_app_indicator_v1_unregistered`) {
		t.Fatalf("unexpected indicator commands: %v", requester.commands)
	}

	tree.Nodes[0].Nodes[0].Nodes[0].Marks = []string{"_sway_session_app_indicator_v1_unregistered_41"}
	refresh, err = runtime.ReconcileIndicators(tree)
	if err != nil || refresh {
		t.Fatalf("stable indicator should be a no-op: refresh=%v err=%v", refresh, err)
	}
	if len(requester.commands) != 1 {
		t.Fatalf("stable indicator was rewritten: %v", requester.commands)
	}
}

func TestSessionRuntimeConfirmedIndicatorFailureDoesNotStarveLaterWindow(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	registry := sessionstate.Registry{
		Version:     sessionstate.ContextsSchemaVersion,
		Preferences: sessionstate.RegistryPreferences{DesktopIndicators: true},
		Contexts:    []sessionstate.Context{},
	}
	if err := sessionstate.RegistryFile(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	catalog := testDesktopCatalog(t, map[string]string{
		"org.example.First.desktop":  "[Desktop Entry]\nType=Application\nName=First\nExec=/usr/bin/true\n",
		"org.example.Second.desktop": "[Desktop Entry]\nType=Application\nName=Second\nExec=/usr/bin/true\n",
	}, false)
	requester := &recordingRequester{failAt: 1, failure: errors.New("explicit rejection")}
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Root: root,
		IndicatorCatalog: func() (sessionstate.DesktopCatalog, error) {
			return catalog, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, second := "org.example.First", "org.example.Second"
	tree := daemonTree("98: apps",
		&Node{ID: 41, Name: "First", Type: "con", AppID: &first},
		&Node{ID: 42, Name: "Second", Type: "con", AppID: &second},
	)

	refresh, err := runtime.ReconcileIndicators(tree)
	if !refresh || err == nil {
		t.Fatalf("confirmed indicator failure was not degraded around: refresh=%v err=%v", refresh, err)
	}
	if len(requester.commands) != 2 || !strings.Contains(requester.commands[1], "con_id=42") {
		t.Fatalf("first rejected indicator starved later window: %v", requester.commands)
	}
}

func TestSessionRuntimeRemovesExpiredPendingIndicator(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	registry := sessionstate.Registry{
		Version:     sessionstate.ContextsSchemaVersion,
		Preferences: sessionstate.RegistryPreferences{DesktopIndicators: true},
		Contexts:    []sessionstate.Context{},
	}
	if err := sessionstate.RegistryFile(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	catalog := testDesktopCatalog(t, map[string]string{
		"org.example.App.desktop": "[Desktop Entry]\nType=Application\nName=Example\nExec=/usr/bin/true\n",
	}, false)
	requester := &recordingRequester{}
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Root: root,
		IndicatorCatalog: func() (sessionstate.DesktopCatalog, error) {
			return catalog, nil
		},
		IndicatorOperations: func() ([]sessionstate.ApplicationOperation, error) {
			return []sessionstate.ApplicationOperation{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	appID := "org.example.App"
	tree := daemonTree("98: apps", &Node{
		ID: 41, Name: "Example", Type: "con", AppID: &appID,
		Marks: []string{"_sway_session_app_indicator_v1_pending_41"},
	})

	refresh, err := runtime.ReconcileIndicators(tree)
	if err != nil || !refresh {
		t.Fatalf("remove expired pending indicator: refresh=%v err=%v", refresh, err)
	}
	if len(requester.commands) != 2 ||
		!strings.Contains(requester.commands[0], "unmark") ||
		!strings.Contains(requester.commands[1], "unregistered_41") {
		t.Fatalf("expired pending did not converge to unregistered: %v", requester.commands)
	}
}

func TestPersistentReconciliationConsolidatesDegradedDiagnosticsPerPass(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	registry := sessionRegistry(testManagedContextID)
	registry.Preferences.DesktopIndicators = true
	if err := sessionstate.RegistryFile(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	if err := sessionstate.LayoutFile(root).Save(placementOnlySnapshot("98: saved", testManagedContextID)); err != nil {
		t.Fatal(err)
	}
	appID, _ := testManagedContextID.AppID()
	requester := &daemonLoopRequester{trees: []*Node{
		daemonTree("99: landing", &Node{ID: 42, Name: "terminal", Type: "con", AppID: &appID}),
		daemonTree("98: saved", &Node{ID: 42, Name: "terminal", Type: "con", AppID: &appID}),
		daemonTree("98: saved", managedDaemonLeaf(t, 42, testManagedContextID)),
	}, failAt: 1, failure: errors.New("explicit placement rejection")}
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Root: root,
		IndicatorCatalog: func() (sessionstate.DesktopCatalog, error) {
			return sessionstate.DesktopCatalog{}, errors.New("catalog unavailable")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reported := make([]error, 0)

	reconcilePersistentSession(requester, runtime, func(err error) {
		reported = append(reported, err)
	})

	if len(reported) != 1 ||
		!strings.Contains(reported[0].Error(), "catalog unavailable") ||
		!strings.Contains(reported[0].Error(), "explicit placement rejection") {
		t.Fatalf("diagnostics were not consolidated: %+v", reported)
	}
}

func TestSessionRuntimeLaunchesDesktopAppsOnlyAfterAdoptionAndPersistsIntentFirst(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	context := sessionstate.Context{
		ID: testManagedContextID, Label: "Example", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.App", FlatpakInstallation: sessionstate.FlatpakUser},
		App: &sessionstate.Application{
			Identity:    sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App", SandboxAppID: "org.example.App"},
			DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow,
		},
	}
	if err := sessionstate.RegistryFile(root).Save(sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{context}}); err != nil {
		t.Fatal(err)
	}
	if err := sessionstate.LayoutFile(root).Save(placementOnlySnapshot("98: apps", context.ID)); err != nil {
		t.Fatal(err)
	}
	launcher := &recordingApplicationLauncher{}
	start := time.Unix(1000, 0)
	runtime, err := newSessionRuntimeWithOptions(&recordingRequester{}, sessionRuntimeOptions{
		Root: root, CompositorID: strings.Repeat("e", 64), StartedAt: start,
		ApplicationLauncher: launcher,
		ApplicationRestore: sessionstate.ApplicationRestoreOptions{
			AdoptionGrace: 10 * time.Second, CloseGrace: 2 * time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Reconcile(daemonTree("98: apps"), start.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(launcher.contexts) != 0 {
		t.Fatalf("application launched during adoption grace: %+v", launcher.contexts)
	}
	if _, err := runtime.Reconcile(daemonTree("98: apps"), start.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(launcher.contexts) != 1 || launcher.contexts[0].ID != context.ID {
		t.Fatalf("missing desired application was not launched once: %+v", launcher.contexts)
	}
	var persisted sessionstate.ApplicationSessionState
	if err := sessionstate.ApplicationSessionFile(root).LoadInto(&persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Attempts) != 1 || persisted.Attempts[0].ContextID != context.ID {
		t.Fatalf("launch intent was not durable: %+v", persisted)
	}
}

func TestSessionRuntimePreflightFailureDoesNotStarveLaterLaunchCandidates(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	ids := []sessionstate.ContextID{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333",
	}
	contexts := make([]sessionstate.Context, 0, len(ids))
	for index, id := range ids {
		appID := fmt.Sprintf("org.example.App%d", index+1)
		contexts = append(contexts, sessionstate.Context{
			ID: id, Label: appID, Provider: "desktop", State: sessionstate.ContextActive,
			Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: appID, FlatpakInstallation: sessionstate.FlatpakUser},
			App: &sessionstate.Application{
				Identity:    sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: appID, SandboxAppID: appID},
				DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow,
			},
		})
	}
	if err := sessionstate.RegistryFile(root).Save(sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: contexts}); err != nil {
		t.Fatal(err)
	}
	launcher := &recordingApplicationLauncher{prepareErrBy: map[sessionstate.ContextID]error{ids[0]: errors.New("approval changed")}}
	start := time.Unix(1500, 0)
	runtime, err := newSessionRuntimeWithOptions(&recordingRequester{}, sessionRuntimeOptions{
		Root: root, CompositorID: strings.Repeat("d", 64), StartedAt: start, ApplicationLauncher: launcher,
		ApplicationRestore: sessionstate.ApplicationRestoreOptions{
			AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, reconcileErr := runtime.Reconcile(daemonTree("98: apps"), start.Add(time.Second))
	if reconcileErr == nil || len(launcher.contexts) != 1 || launcher.contexts[0].ID != ids[1] {
		t.Fatalf("bounded preflights did not advance to a valid candidate: launches=%+v err=%v", launcher.contexts, reconcileErr)
	}
	_, reconcileErr = runtime.Reconcile(daemonTree("98: apps"), start.Add(3*time.Second))
	if reconcileErr == nil || len(launcher.contexts) != 2 || launcher.contexts[1].ID != ids[2] {
		t.Fatalf("failed preflight starved a later candidate across observations: launches=%+v err=%v", launcher.contexts, reconcileErr)
	}
}

func TestApplicationLaunchCandidateRotationContinuesAfterBoundedPreflights(t *testing.T) {
	contexts := []sessionstate.Context{
		{ID: "11111111-1111-4111-8111-111111111111"},
		{ID: "22222222-2222-4222-8222-222222222222"},
		{ID: "33333333-3333-4333-8333-333333333333"},
	}

	rotated := rotateApplicationLaunchCandidates(contexts, contexts[1].ID)
	if len(rotated) != 3 || rotated[0].ID != contexts[2].ID || rotated[1].ID != contexts[0].ID || rotated[2].ID != contexts[1].ID {
		t.Fatalf("candidate rotation did not continue after its cursor: %+v", rotated)
	}
}

func TestSessionRuntimeTreatsAmbiguousDesktopWindowsAsPresenceWithoutGuessing(t *testing.T) {
	runtime, requester, launcher, context, start := testApplicationRuntime(t)
	appID := context.App.Identity.WaylandAppID
	sandbox := context.App.Identity.SandboxAppID
	first := &Node{ID: 41, Type: "con", AppID: &appID, SandboxAppID: &sandbox}
	second := &Node{ID: 42, Type: "con", AppID: &appID, SandboxAppID: &sandbox}

	if refresh, err := runtime.Reconcile(daemonTree("98: apps", first, second), start.Add(10*time.Second)); err != nil || refresh {
		t.Fatalf("reconcile ambiguous application group: refresh=%v err=%v", refresh, err)
	}
	if len(launcher.contexts) != 0 || len(requester.commands) != 0 {
		t.Fatalf("ambiguous group was launched or placed: launches=%+v commands=%v", launcher.contexts, requester.commands)
	}
}

func TestSessionRuntimePlacesLateDesktopAnchorWithoutRebuildingLayout(t *testing.T) {
	runtime, requester, _, context, start := testApplicationRuntime(t)
	runtime.startupComplete = true
	appID := context.App.Identity.WaylandAppID
	sandbox := context.App.Identity.SandboxAppID
	window := &Node{ID: 41, Type: "con", AppID: &appID, SandboxAppID: &sandbox}

	refresh, err := runtime.Reconcile(daemonTree("99: landing", window), start.Add(20*time.Second))
	if err != nil || !refresh {
		t.Fatalf("reconcile late application anchor: refresh=%v err=%v", refresh, err)
	}
	if len(requester.commands) != 2 ||
		!strings.Contains(requester.commands[0], `workspace "98: apps"`) ||
		!strings.Contains(requester.commands[1], `mark --add "persist:`) {
		t.Fatalf("late unique anchor did not receive placement then mark: %v", requester.commands)
	}
	if runtime.lateRestorePending {
		t.Fatal("late desktop anchor triggered disruptive full-layout rebuild")
	}
}

func TestSessionRuntimePersistsFollowAppClosedOnlyAfterLastWindowGrace(t *testing.T) {
	runtime, _, launcher, context, start := testApplicationRuntime(t)
	runtime.startupComplete = true
	appID := context.App.Identity.WaylandAppID
	sandbox := context.App.Identity.SandboxAppID
	mark, err := context.ID.Mark()
	if err != nil {
		t.Fatal(err)
	}
	window := &Node{ID: 41, Type: "con", AppID: &appID, SandboxAppID: &sandbox, Marks: []string{mark}}
	if _, err := runtime.Reconcile(daemonTree("98: apps", window), start); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Reconcile(daemonTree("98: apps"), start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var before sessionstate.Registry
	if err := sessionstate.RegistryFile(runtime.root).LoadInto(&before); err != nil {
		t.Fatal(err)
	}
	if !before.Contexts[0].App.DesiredOpen {
		t.Fatal("application closed before the bounded grace period")
	}
	if _, err := runtime.Reconcile(daemonTree("98: apps"), start.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	var after sessionstate.Registry
	if err := sessionstate.RegistryFile(runtime.root).LoadInto(&after); err != nil {
		t.Fatal(err)
	}
	if after.Contexts[0].App.DesiredOpen || len(launcher.contexts) != 0 {
		t.Fatalf("last-window close did not persist without relaunch: app=%+v launches=%+v", after.Contexts[0].App, launcher.contexts)
	}
}

func TestSessionRuntimePreflightFailureRemainsRetryableAfterReapproval(t *testing.T) {
	runtime, _, launcher, context, start := testApplicationRuntime(t)
	launcher.prepareErr = errors.New("desktop entry changed and requires reapproval")
	if _, err := runtime.Reconcile(daemonTree("98: apps"), start.Add(10*time.Second)); err == nil {
		t.Fatal("launcher preflight failure was not reported")
	}
	var state sessionstate.ApplicationSessionState
	if err := sessionstate.ApplicationSessionFile(runtime.root).LoadInto(&state); err != nil {
		t.Fatal(err)
	}
	if len(state.Attempts) != 0 {
		t.Fatalf("preflight failure consumed compositor attempt: %+v", state.Attempts)
	}
	launcher.prepareErr = nil
	if _, err := runtime.Reconcile(daemonTree("98: apps"), start.Add(12*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(launcher.contexts) != 1 || launcher.contexts[0].ID != context.ID {
		t.Fatalf("reapproved launcher was not retried: %+v", launcher.contexts)
	}
}

func TestSessionRuntimeStartFailureRemainsAConservativeSingleAttempt(t *testing.T) {
	runtime, _, launcher, context, start := testApplicationRuntime(t)
	launcher.startErr = errors.New("process start outcome failed")
	if _, err := runtime.Reconcile(daemonTree("98: apps"), start.Add(10*time.Second)); err == nil {
		t.Fatal("process start failure was not reported")
	}
	var state sessionstate.ApplicationSessionState
	if err := sessionstate.ApplicationSessionFile(runtime.root).LoadInto(&state); err != nil {
		t.Fatal(err)
	}
	if launcher.starts != 1 || len(state.Attempts) != 1 || state.Attempts[0].ContextID != context.ID {
		t.Fatalf("failed start did not retain one conservative attempt: starts=%d state=%+v", launcher.starts, state)
	}
	launcher.startErr = nil
	if _, err := runtime.Reconcile(daemonTree("98: apps"), start.Add(12*time.Second)); err != nil {
		t.Fatal(err)
	}
	if launcher.starts != 1 {
		t.Fatalf("failed or ambiguous start was replayed in the same compositor: %d", launcher.starts)
	}
}

func TestSessionRuntimeCancellationReleasesRegistryLockHeldAcrossSwayRequest(t *testing.T) {
	runtime, _, _, registered, start := testApplicationRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	requester := &blockingDaemonRequester{entered: make(chan struct{})}
	runtime.ctx = ctx
	runtime.client = requester
	appID := registered.App.Identity.WaylandAppID
	sandboxID := registered.App.Identity.SandboxAppID
	window := &Node{ID: 41, Type: "con", AppID: &appID, SandboxAppID: &sandboxID}
	reconciled := make(chan error, 1)
	go func() {
		_, err := runtime.Reconcile(daemonTree("98: apps", window), start)
		reconciled <- err
	}()

	select {
	case <-requester.entered:
	case <-time.After(time.Second):
		t.Fatal("runtime did not reach the Sway request while holding the registry transaction")
	}
	cancel()

	select {
	case err := <-reconciled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("reconcile error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime did not return after cancellation")
	}

	updateCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := sessionstate.UpdateRegistryContext(updateCtx, runtime.root, func(*sessionstate.Registry) error { return nil }); err != nil {
		t.Fatalf("registry lock remained held after canceled Sway request: %v", err)
	}
}

func TestNewSessionRuntimeRejectsMalformedApplicationSessionState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	directory := filepath.Join(root, sessionstate.ApplicationSessionDirectory)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, sessionstate.ApplicationSessionFilename), []byte(`{"version":1,"compositor_id":"bad","attempts":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := newSessionRuntimeWithOptions(&recordingRequester{}, sessionRuntimeOptions{
		Root: root, CompositorID: strings.Repeat("a", 64), StartedAt: time.Unix(1000, 0),
		ApplicationLauncher: &recordingApplicationLauncher{},
		ApplicationRestore: sessionstate.ApplicationRestoreOptions{
			AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: time.Second, MaxConcurrent: 2,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "load desktop application restore state") {
		t.Fatalf("malformed application session state was accepted: %v", err)
	}
}

func TestNewSessionRuntimeStopsWaitingForStateLockWhenCanceled(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := sessionstate.RegistryFile(root).Save(sessionRegistry(testManagedContextID)); err != nil {
		t.Fatal(err)
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		_, err := sessionstate.UpdateRegistry(root, func(*sessionstate.Registry) error {
			close(locked)
			<-release
			return nil
		})
		mutationDone <- err
	}()
	<-locked
	t.Cleanup(func() {
		close(release)
		if err := <-mutationDone; err != nil {
			t.Errorf("release registry mutation: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := newSessionRuntimeWithOptions(&recordingRequester{}, sessionRuntimeOptions{Context: ctx, Root: root})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runtime initialization returned %v, want context deadline", err)
	}
}

func testApplicationRuntime(t *testing.T) (*sessionRuntime, *recordingRequester, *recordingApplicationLauncher, sessionstate.Context, time.Time) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	context := sessionstate.Context{
		ID: testManagedContextID, Label: "Example", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.App", FlatpakInstallation: sessionstate.FlatpakUser},
		App: &sessionstate.Application{
			Identity:    sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App", SandboxAppID: "org.example.App"},
			DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow,
		},
	}
	if err := sessionstate.RegistryFile(root).Save(sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{context}}); err != nil {
		t.Fatal(err)
	}
	if err := sessionstate.LayoutFile(root).Save(placementOnlySnapshot("98: apps", context.ID)); err != nil {
		t.Fatal(err)
	}
	requester := &recordingRequester{}
	launcher := &recordingApplicationLauncher{}
	start := time.Unix(2000, 0)
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Root: root, CompositorID: strings.Repeat("f", 64), StartedAt: start, ApplicationLauncher: launcher,
		ApplicationRestore: sessionstate.ApplicationRestoreOptions{
			AdoptionGrace: 10 * time.Second, CloseGrace: 2 * time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime, requester, launcher, context, start
}

func TestSessionRuntimeCapturesManualMoveOfMarkedWindowAfterDebounce(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	setSessionTestStateHome(t, stateHome)
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

func TestSessionRuntimeSchedulesPeriodicObservationToDiscoverFirstRegistry(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	setSessionTestStateHome(t, stateHome)
	runtime, err := newSessionRuntime(&recordingRequester{})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	start := time.Unix(250, 0)
	if _, err := runtime.Reconcile(daemonTree("1"), start); err != nil {
		t.Fatalf("reconcile without registry: %v", err)
	}
	if runtime.ObservationDue(start.Add(sessionObservationDelay - time.Nanosecond)) {
		t.Fatal("missing-registry observation became due early")
	}
	if !runtime.ObservationDue(start.Add(sessionObservationDelay)) {
		t.Fatal("missing registry was not scheduled for external-registration discovery")
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

func TestSessionRuntimeObservesWithoutStartingRestoreWhenRegistryIsMissing(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	setSessionTestStateHome(t, stateHome)
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
	deadline, scheduled := runtime.Deadline()
	if !scheduled || deadline != time.Unix(275, 0).Add(sessionObservationDelay) {
		t.Fatalf("missing registry did not schedule only observation: %v scheduled=%v", deadline, scheduled)
	}
	if !runtime.startupDeadline.IsZero() {
		t.Fatalf("missing registry scheduled startup restore at %v", runtime.startupDeadline)
	}
}

func TestSessionRuntimeArmsRetryBeforeInitialTreeObservation(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	setSessionTestStateHome(t, stateHome)
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
	setSessionTestStateHome(t, stateHome)
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
	setSessionTestStateHome(t, stateHome)
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
	setSessionTestStateHome(t, stateHome)
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
	setSessionTestStateHome(t, stateHome)
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
	setSessionTestStateHome(t, stateHome)
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
	setSessionTestStateHome(t, stateHome)
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
	setSessionTestStateHome(t, stateHome)
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
	setSessionTestStateHome(t, stateHome)
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
	setSessionTestStateHome(t, stateHome)
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
				Kind:     sessionstate.LauncherHerdr,
				Session:  fmt.Sprintf("test-session-%d", index),
				Cwd:      "/work",
				Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty},
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

func snapshotWorkspaceTargets(snapshot sessionstate.LayoutSnapshot) map[sessionstate.ContextID]string {
	targets := make(map[sessionstate.ContextID]string)
	for _, workspace := range snapshot.Workspaces {
		for _, id := range workspace.PlacementContexts {
			targets[id] = workspace.Name
		}
		var collect func(sessionstate.LayoutNode)
		collect = func(node sessionstate.LayoutNode) {
			if node.ContextID != nil {
				targets[*node.ContextID] = workspace.Name
			}
			for _, child := range node.Children {
				collect(child)
			}
		}
		if workspace.Tiling != nil {
			collect(*workspace.Tiling)
		}
		for _, floating := range workspace.Floating {
			collect(floating)
		}
	}
	return targets
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
