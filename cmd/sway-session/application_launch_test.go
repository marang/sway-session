package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/statefile"
	"github.com/marang/sway-session/internal/swayipc"
)

// A separate live tree lets a mapping happen after the caller's observation.
// Stream state can change synchronously, as it does on the IPC reader goroutine.
type applicationLaunchRequester struct {
	recordingRequester
	tree       *Node
	treeErr    error
	treeReads  int
	beforeTree func()
}

func (client *applicationLaunchRequester) RequestContext(ctx context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if kind != swayipc.GetTree {
		return client.recordingRequester.RequestContext(ctx, kind, payload)
	}
	client.treeReads++
	if client.beforeTree != nil {
		client.beforeTree()
	}
	if client.treeErr != nil {
		return swayipc.Message{}, client.treeErr
	}
	encoded, err := json.Marshal(client.tree)
	return swayipc.Message{Type: kind, Payload: encoded}, err
}

type applicationLaunchHooks struct {
	prepare      func(sessionstate.Context)
	start        func(sessionstate.Context)
	preparations []sessionstate.ContextID
	starts       []sessionstate.ContextID
}

type preparedApplicationLaunchHook struct {
	launcher *applicationLaunchHooks
	item     sessionstate.Context
}

func (launcher *applicationLaunchHooks) Prepare(_ context.Context, item sessionstate.Context) (preparedApplicationLaunch, error) {
	launcher.preparations = append(launcher.preparations, item.ID)
	if launcher.prepare != nil {
		launcher.prepare(item)
	}
	return preparedApplicationLaunchHook{launcher, item}, nil
}

func (launch preparedApplicationLaunchHook) Start() error {
	if launch.launcher.start != nil {
		launch.launcher.start(launch.item)
	}
	launch.launcher.starts = append(launch.launcher.starts, launch.item.ID)
	return nil
}

func launchFixture(t *testing.T) (*sessionRuntime, *applicationLaunchRequester, *applicationLaunchHooks, *mutableEventStreamGuard, sessionstate.Context, time.Time) {
	t.Helper()
	runtime, _, _, item, start := testApplicationRuntime(t)
	client := &applicationLaunchRequester{tree: daemonTree("98: apps")}
	launcher := &applicationLaunchHooks{}
	stream := &mutableEventStreamGuard{epoch: 1, connected: true}
	runtime.client = client
	runtime.applicationLauncher = launcher
	runtime.eventStreamState = stream
	runtime.eventStreamReady = true
	runtime.eventStreamEpoch = stream.epoch
	runtime.now = func() time.Time { return start.Add(10 * time.Second) }
	return runtime, client, launcher, stream, item, start.Add(10 * time.Second)
}

func TestApplicationLaunchStreamChangesDuringPreparation(t *testing.T) {
	for _, name := range []string{"disconnect", "reconnect", "ready_processed", "shutdown", "cancel", "no_guard"} {
		t.Run(name, func(t *testing.T) {
			runtime, client, launcher, stream, _, now := launchFixture(t)
			launcher.prepare = func(sessionstate.Context) {
				switch name {
				case "disconnect":
					stream.connected = false
				case "reconnect":
					stream.epoch++
				case "ready_processed":
					stream.epoch++
					runtime.eventStreamEpoch++
				case "shutdown":
					runtime.shutdown = true
				case "cancel":
					cancelled, cancel := context.WithCancel(context.Background())
					cancel()
					runtime.ctx = cancelled
				case "no_guard":
					runtime.eventStreamState = nil
				}
			}
			_, _, degraded, err := runtime.reconcileApplications(client.tree, runtime.registry, now)
			if err != nil || degraded == nil {
				t.Fatalf("expected deferred launch: %v / %v", err, degraded)
			}
			assertNoApplicationLaunchAttempt(t, runtime, launcher)
			if client.treeReads != 0 {
				t.Fatalf("invalid stream acquired %d trees", client.treeReads)
			}
		})
	}
}

func TestApplicationLaunchStreamChangesDuringFreshObservation(t *testing.T) {
	runtime, client, launcher, stream, _, now := launchFixture(t)
	client.beforeTree = func() { stream.epoch++ }
	_, _, degraded, err := runtime.reconcileApplications(client.tree, runtime.registry, now)
	if err != nil || degraded == nil {
		t.Fatalf("expected deferred launch: %v / %v", err, degraded)
	}
	assertNoApplicationLaunchAttempt(t, runtime, launcher)
}

func TestApplicationLaunchUnavailableOrAmbiguousObservation(t *testing.T) {
	for _, name := range []string{"unavailable", "invalid_root", "multiple_windows", "marked_identity_drift"} {
		t.Run(name, func(t *testing.T) {
			runtime, client, launcher, _, item, now := launchFixture(t)
			initial := client.tree
			launcher.prepare = func(sessionstate.Context) {
				switch name {
				case "unavailable":
					client.treeErr = errors.New("IPC unavailable")
				case "invalid_root":
					client.tree = &Node{}
				case "multiple_windows":
					client.tree = daemonTree("98: apps", applicationLaunchWindow(item, 41), applicationLaunchWindow(item, 42))
				case "marked_identity_drift":
					window := applicationLaunchWindow(item, 41)
					other := "org.example.Other"
					window.AppID = &other
					mark, _ := item.ID.Mark()
					window.Marks = []string{mark}
					client.tree = daemonTree("98: apps", window)
				}
			}
			_, _, degraded, err := runtime.reconcileApplications(initial, runtime.registry, now)
			if err != nil {
				t.Fatal(err)
			}
			if (name == "unavailable" || name == "invalid_root") && degraded == nil {
				t.Fatal("unavailable observation was not reported")
			}
			assertNoApplicationLaunchAttempt(t, runtime, launcher)
			if client.treeReads != 1 || len(client.commands) != 0 {
				t.Fatalf("observation was replayed or ambiguous window mutated: reads=%d commands=%v", client.treeReads, client.commands)
			}
		})
	}
}

func TestApplicationLaunchUsesFreshTimeAndDurableIntent(t *testing.T) {
	runtime, client, launcher, _, item, now := launchFixture(t)
	freshNow := now
	runtime.now = func() time.Time { return freshNow }
	launcher.prepare = func(sessionstate.Context) { freshNow = now.Add(30 * time.Second) }
	client.beforeTree = func() { freshNow = freshNow.Add(3 * time.Second) }
	launcher.start = func(sessionstate.Context) {
		var state sessionstate.ApplicationSessionState
		if err := sessionstate.ApplicationSessionStoreFor(runtime.root).LoadInto(&state); err != nil {
			t.Fatal(err)
		}
		if len(state.Attempts) != 1 || state.Attempts[0].ContextID != item.ID || !state.Attempts[0].StartedAt.Equal(freshNow) {
			t.Fatalf("Start crossed boundary without fresh durable intent: %+v want %s", state, freshNow)
		}
	}
	_, _, degraded, err := runtime.reconcileApplications(client.tree, runtime.registry, now)
	if err != nil || degraded != nil {
		t.Fatalf("reconcile: %v / %v", err, degraded)
	}
	if !reflect.DeepEqual(launcher.starts, []sessionstate.ContextID{item.ID}) || client.treeReads != 1 {
		t.Fatalf("unexpected effects: starts=%v reads=%d", launcher.starts, client.treeReads)
	}
	_, _, degraded, err = runtime.reconcileApplications(client.tree, runtime.registry, freshNow.Add(time.Hour))
	if err != nil || degraded != nil || len(launcher.starts) != 1 {
		t.Fatalf("replayed attempt: %v / %v / %v", launcher.starts, err, degraded)
	}
}

type applicationLaunchGuardFunc func() (uint64, bool)

func (guard applicationLaunchGuardFunc) Snapshot() (uint64, bool) { return guard() }

func TestApplicationLaunchLostStreamAfterIntentNeverStarts(t *testing.T) {
	runtime, client, launcher, _, _, now := launchFixture(t)
	runtime.eventStreamState = applicationLaunchGuardFunc(func() (uint64, bool) {
		if len(runtime.applications.State().Attempts) != 0 {
			return 2, false
		}
		return 1, true
	})
	_, _, degraded, err := runtime.reconcileApplications(client.tree, runtime.registry, now)
	if err != nil || degraded == nil || len(launcher.starts) != 0 {
		t.Fatalf("Start crossed lost stream: %v / %v / %v", launcher.starts, err, degraded)
	}
	var state sessionstate.ApplicationSessionState
	if err := sessionstate.ApplicationSessionStoreFor(runtime.root).LoadInto(&state); err != nil {
		t.Fatal(err)
	}
	if len(state.Attempts) != 1 {
		t.Fatalf("durable intent was discarded: %+v", state)
	}
}

func TestApplicationLaunchMapsWhileWaitingForRegistryLock(t *testing.T) {
	runtime, client, launcher, _, item, now := launchFixture(t)
	initial := client.tree
	entered := make(chan struct{})
	released := make(chan struct{})
	locked := make(chan error, 1)
	go func() {
		locked <- sessionstate.WithRegistryLockContext(context.Background(), runtime.root, func(*statefile.LockedPrivateDirectory) error {
			close(entered)
			<-released
			return nil
		})
	}()
	<-entered
	started := make(chan struct{})
	mapped := make(chan struct{})
	client.beforeTree = func() { <-mapped }
	done := make(chan error, 1)
	go func() {
		close(started)
		_, _, degraded, err := runtime.reconcileApplications(initial, runtime.registry, now)
		done <- errors.Join(degraded, err)
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("reconcile bypassed held registry lock: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	client.tree = daemonTree("98: apps", applicationLaunchWindow(item, 41))
	close(mapped)
	close(released)
	if err := <-locked; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertNoApplicationLaunchAttempt(t, runtime, launcher)
}

func TestApplicationLaunchFreshnessPreservesBoundedFairRotation(t *testing.T) {
	runtime, client, launcher, _, item, now := launchFixture(t)
	registry := runtime.registry
	registry.Contexts = nil
	for i := range 5 {
		candidate := item
		candidate.ID = sessionstate.ContextID(fmt.Sprintf("%08d-1111-4111-8111-111111111111", i+1))
		candidate.Launcher.FlatpakID = fmt.Sprintf("org.example.App%d", i)
		candidate.App = &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: candidate.Launcher.FlatpakID, SandboxAppID: candidate.Launcher.FlatpakID}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestorePinned}
		registry.Contexts = append(registry.Contexts, candidate)
	}
	if err := sessionstate.RegistryStoreFor(runtime.root).Save(registry); err != nil {
		t.Fatal(err)
	}
	runtime.registry = registry
	client.treeErr = errors.New("temporary tree failure")
	for range 3 {
		before := len(launcher.preparations)
		_, _, degraded, err := runtime.reconcileApplications(client.tree, registry, now)
		if err != nil || degraded == nil || len(launcher.preparations)-before != maxApplicationPreflights {
			t.Fatalf("bounded pass: prepare=%v err=%v / %v", launcher.preparations, err, degraded)
		}
		assertNoApplicationLaunchAttempt(t, runtime, launcher)
	}
	want := []sessionstate.ContextID{registry.Contexts[0].ID, registry.Contexts[1].ID, registry.Contexts[2].ID, registry.Contexts[3].ID, registry.Contexts[4].ID, registry.Contexts[0].ID}
	if !reflect.DeepEqual(launcher.preparations, want) || client.treeReads != 3*maxApplicationPreflights {
		t.Fatalf("stale observations starved rotation: %v reads=%d", launcher.preparations, client.treeReads)
	}
	client.treeErr = nil
	_, _, degraded, err := runtime.reconcileApplications(client.tree, registry, now)
	if err != nil || degraded != nil || len(launcher.starts) != 2 {
		t.Fatalf("fresh pass did not fill two slots: %v %v / %v", launcher.starts, err, degraded)
	}
	_, _, degraded, err = runtime.reconcileApplications(client.tree, registry, now.Add(time.Second))
	if err != nil || degraded != nil || len(launcher.starts) != 2 {
		t.Fatalf("concurrency bound exceeded: %v %v / %v", launcher.starts, err, degraded)
	}
}

func TestApplicationLaunchEarlierStartMapsNextCandidate(t *testing.T) {
	runtime, client, launcher, _, item, now := launchFixture(t)
	other := item
	other.ID = "22222222-2222-4222-8222-222222222222"
	other.Launcher.FlatpakID = "org.example.Other"
	other.App = &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: other.Launcher.FlatpakID, SandboxAppID: other.Launcher.FlatpakID}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestorePinned}
	registry := runtime.registry
	registry.Contexts = append(registry.Contexts, other)
	if err := sessionstate.RegistryStoreFor(runtime.root).Save(registry); err != nil {
		t.Fatal(err)
	}
	runtime.registry = registry
	initial := client.tree
	launcher.start = func(started sessionstate.Context) {
		if started.ID == item.ID {
			client.tree = daemonTree("98: apps", applicationLaunchWindow(other, 42))
		}
	}
	_, _, degraded, err := runtime.reconcileApplications(initial, registry, now)
	if err != nil || degraded != nil {
		t.Fatalf("reconcile: %v / %v", err, degraded)
	}
	var state sessionstate.ApplicationSessionState
	if err := sessionstate.ApplicationSessionStoreFor(runtime.root).LoadInto(&state); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(launcher.starts, []sessionstate.ContextID{item.ID}) || len(state.Attempts) != 1 || state.Attempts[0].ContextID != item.ID || client.treeReads != 2 {
		t.Fatalf("earlier start caused duplicate launch: starts=%v attempts=%+v reads=%d", launcher.starts, state.Attempts, client.treeReads)
	}
}

func TestApplicationLaunchReservationDuringPreparationDefers(t *testing.T) {
	runtime, client, launcher, _, item, now := launchFixture(t)
	lifecycle := &fixtureLifecycleAdapter{}
	runtime.lifecycleOperations = lifecycle
	launcher.prepare = func(sessionstate.Context) { lifecycle.blocked = []sessionstate.ContextID{item.ID} }
	_, _, degraded, err := runtime.reconcileApplications(client.tree, runtime.registry, now)
	if err != nil || degraded != nil {
		t.Fatalf("reconcile: %v / %v", err, degraded)
	}
	assertNoApplicationLaunchAttempt(t, runtime, launcher)
}

func applicationLaunchWindow(item sessionstate.Context, id int64) *Node {
	appID, sandbox := item.App.Identity.WaylandAppID, item.App.Identity.SandboxAppID
	return &Node{ID: id, Type: "con", AppID: &appID, SandboxAppID: &sandbox}
}

func assertNoApplicationLaunchAttempt(t *testing.T, runtime *sessionRuntime, launcher *applicationLaunchHooks) {
	t.Helper()
	var stored sessionstate.ApplicationSessionState
	if err := sessionstate.ApplicationSessionStoreFor(runtime.root).LoadInto(&stored); err != nil {
		t.Fatal(err)
	}
	if len(launcher.starts) != 0 || len(stored.Attempts) != 0 || len(runtime.applications.State().Attempts) != 0 {
		t.Fatalf("unexpected duplicate launch or consumed attempt: starts=%v stored=%+v memory=%+v", launcher.starts, stored.Attempts, runtime.applications.State().Attempts)
	}
}

func TestApplicationLaunchMapsDuringPreparation(t *testing.T) {
	runtime, client, launcher, _, item, now := launchFixture(t)
	initial := client.tree
	launcher.prepare = func(sessionstate.Context) { client.tree = daemonTree("98: apps", applicationLaunchWindow(item, 41)) }
	_, _, degraded, err := runtime.reconcileApplications(initial, runtime.registry, now)
	if err != nil || degraded != nil {
		t.Fatalf("reconcile: %v / %v", err, degraded)
	}
	assertNoApplicationLaunchAttempt(t, runtime, launcher)
	// Adopting fresh presence must not become a watchdog after it disappears.
	client.tree = daemonTree("98: apps")
	launcher.prepare = nil
	_, _, degraded, err = runtime.reconcileApplications(client.tree, runtime.registry, now.Add(time.Minute))
	if err != nil || degraded != nil {
		t.Fatalf("reconcile disappearance: %v / %v", err, degraded)
	}
	assertNoApplicationLaunchAttempt(t, runtime, launcher)
}
