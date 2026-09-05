package shutdownwatch

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testSessionPath = "/org/freedesktop/login1/session/_31"

func TestMonitorStartsSafeAfterCompleteValidation(t *testing.T) {
	client := newFakeClient()
	monitor := startFake(t, client)

	generation, safe := monitor.Snapshot()
	if generation != 1 || !safe {
		t.Fatalf("Snapshot() = (%d, %t), want (1, true)", generation, safe)
	}
	wantCalls := []string{"subscribe", "owner", "inhibit", "session", "state", "shutdown", "sleep"}
	if calls := client.callSnapshot(); !equalStrings(calls, wantCalls) {
		t.Fatalf("startup calls = %v, want %v", calls, wantCalls)
	}
}

func TestPreparationSignalsDisableBeforeReleasingInhibitor(t *testing.T) {
	for _, test := range []struct {
		name  string
		event event
	}{
		{name: "shutdown", event: event{kind: eventPrepareShutdown, source: ":1.44", active: true}},
		{name: "sleep", event: event{kind: eventPrepareSleep, source: ":1.44", active: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newFakeClient()
			var monitor *Monitor
			releasedUnsafe := make(chan bool, 1)
			client.inhibitor.onClose = func() {
				_, safe := monitor.Snapshot()
				releasedUnsafe <- !safe
			}
			monitor = startFake(t, client)

			client.emit(test.event)

			generation, safe := monitor.Snapshot()
			if generation != 2 || safe {
				t.Fatalf("Snapshot() = (%d, %t), want (2, false)", generation, safe)
			}
			if unsafe := <-releasedUnsafe; !unsafe {
				t.Fatal("inhibitor was released before the guard became unsafe")
			}
			if err := monitor.Err(); err != nil {
				t.Fatalf("Err() during an expected preparation cycle = %v", err)
			}
			if got := client.inhibitor.closeCount.Load(); got != 1 {
				t.Fatalf("inhibitor close count = %d, want 1", got)
			}

			client.emit(event{kind: test.event.kind, source: ":1.44", active: false})
			waitSnapshot(t, monitor, 3, true)
		})
	}
}

func TestCanceledPreparationCycleRevalidatesBeforeBecomingSafe(t *testing.T) {
	client := newFakeClient()
	monitor := startFake(t, client)

	client.emit(event{kind: eventPrepareShutdown, source: ":1.44", active: true})
	if generation, safe := monitor.Snapshot(); generation != 2 || safe {
		t.Fatalf("Snapshot() during preparation = (%d, %t), want (2, false)", generation, safe)
	}
	client.emit(event{kind: eventPrepareShutdown, source: ":1.44", active: false})
	waitSnapshot(t, monitor, 3, true)
	wantTail := []string{"inhibit", "owner", "session", "state", "shutdown", "sleep"}
	calls := client.callSnapshot()
	if len(calls) < len(wantTail) || !equalStrings(calls[len(calls)-len(wantTail):], wantTail) {
		t.Fatalf("revalidation call tail = %v, want %v", calls, wantTail)
	}
}

func TestOverlappingPreparationCyclesRemainUnsafeUntilBothEnd(t *testing.T) {
	client := newFakeClient()
	monitor := startFake(t, client)

	client.emit(event{kind: eventPrepareShutdown, source: ":1.44", active: true})
	client.emit(event{kind: eventPrepareSleep, source: ":1.44", active: true})
	client.emit(event{kind: eventPrepareShutdown, source: ":1.44", active: false})
	time.Sleep(10 * time.Millisecond)
	if generation, safe := monitor.Snapshot(); generation != 2 || safe {
		t.Fatalf("Snapshot() while sleep remains active = (%d, %t), want (2, false)", generation, safe)
	}
	client.emit(event{kind: eventPrepareSleep, source: ":1.44", active: false})
	waitSnapshot(t, monitor, 3, true)
}

func TestPreparationInterruptsInFlightRearmAndReleasesNewInhibitor(t *testing.T) {
	client := newFakeClient()
	monitor := startFake(t, client)
	enteredOwnerCheck := make(chan struct{})
	releaseOwnerCheck := make(chan struct{})
	var ownerChecks atomic.Int32
	client.ownerHook = func() {
		if ownerChecks.Add(1) == 1 {
			close(enteredOwnerCheck)
			<-releaseOwnerCheck
		}
	}

	client.emit(event{kind: eventPrepareSleep, source: ":1.44", active: true})
	client.emit(event{kind: eventPrepareSleep, source: ":1.44", active: false})
	waitDone(t, enteredOwnerCheck)
	client.emit(event{kind: eventPrepareSleep, source: ":1.44", active: true})
	if generation, safe := monitor.Snapshot(); generation != 2 || safe {
		t.Fatalf("Snapshot() after interrupted rearm = (%d, %t), want (2, false)", generation, safe)
	}
	if got := client.inhibitor.closeCount.Load(); got != 2 {
		t.Fatalf("inhibitor close count = %d, want initial and rearm inhibitors released", got)
	}
	close(releaseOwnerCheck)
	client.emit(event{kind: eventPrepareSleep, source: ":1.44", active: false})
	waitSnapshot(t, monitor, 3, true)
}

func TestRearmFailurePermanentlyDisablesMonitor(t *testing.T) {
	client := newFakeClient()
	monitor := startFake(t, client)
	client.emit(event{kind: eventPrepareSleep, source: ":1.44", active: true})
	client.inhibitErr = errors.New("rejected")
	client.emit(event{kind: eventPrepareSleep, source: ":1.44", active: false})

	waitDone(t, monitor.Done())
	if _, safe := monitor.Snapshot(); safe {
		t.Fatal("monitor became safe after rearm failure")
	}
	if err := monitor.Err(); err == nil || !strings.Contains(err.Error(), "reacquire") {
		t.Fatalf("Err() = %v, want reacquisition diagnostic", err)
	}
}

func TestPreparationInhibitorReleaseFailureIsTerminal(t *testing.T) {
	client := newFakeClient()
	monitor := startFake(t, client)
	client.inhibitor.closeErr = errors.New("close failed")
	client.emit(event{kind: eventPrepareShutdown, source: ":1.44", active: true})

	waitDone(t, monitor.Done())
	if _, safe := monitor.Snapshot(); safe {
		t.Fatal("monitor remained safe after inhibitor release failed")
	}
	if err := monitor.Err(); err == nil || !strings.Contains(err.Error(), "release logind delay inhibitor") {
		t.Fatalf("Err() = %v, want inhibitor release diagnostic", err)
	}
}

func TestCloseWaitsForInFlightInhibitorAcquisition(t *testing.T) {
	client := newFakeClient()
	monitor := startFake(t, client)
	entered := make(chan struct{})
	release := make(chan struct{})
	lateInhibitor := &fakeCloser{}
	client.inhibitHook = func(context.Context) (io.Closer, error) {
		close(entered)
		<-release
		return lateInhibitor, nil
	}

	client.emit(event{kind: eventPrepareSleep, source: ":1.44", active: true})
	client.emit(event{kind: eventPrepareSleep, source: ":1.44", active: false})
	waitDone(t, entered)
	closed := make(chan error, 1)
	go func() { closed <- monitor.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before acquired inhibitor was accounted for: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close() = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after inhibitor acquisition returned")
	}
	if got := lateInhibitor.closeCount.Load(); got != 1 {
		t.Fatalf("late inhibitor close count = %d, want 1", got)
	}
}

func TestLifecycleFailuresStayUnsafe(t *testing.T) {
	tests := []struct {
		name  string
		event event
		text  string
	}{
		{
			name:  "name owner changed",
			event: event{kind: eventNameOwnerChanged, source: busInterface},
			text:  "name owner changed",
		},
		{
			name:  "own session removed",
			event: event{kind: eventSessionRemoved, source: ":1.44", path: testSessionPath},
			text:  "session was removed",
		},
		{
			name:  "session closing",
			event: event{kind: eventSessionState, source: ":1.44", path: testSessionPath, state: "closing"},
			text:  "became unusable",
		},
		{
			name:  "session state invalidated",
			event: event{kind: eventSessionState, source: ":1.44", path: testSessionPath, invalidated: true},
			text:  "became unusable",
		},
		{
			name:  "unexpected sender",
			event: event{kind: eventPrepareShutdown, source: ":1.99", active: true},
			text:  "signal sender changed",
		},
		{
			name:  "malformed signal",
			event: event{kind: eventInvalid, err: errors.New("bad signal")},
			text:  "bad signal",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newFakeClient()
			monitor := startFake(t, client)
			client.emit(test.event)
			waitDone(t, monitor.Done())
			if _, safe := monitor.Snapshot(); safe {
				t.Fatal("monitor remained safe after lifecycle failure")
			}
			if err := monitor.Err(); err == nil || !strings.Contains(err.Error(), test.text) {
				t.Fatalf("Err() = %v, want text %q", err, test.text)
			}
		})
	}
}

func TestOtherSessionChangesDoNotDisableMonitor(t *testing.T) {
	client := newFakeClient()
	monitor := startFake(t, client)

	client.emit(event{kind: eventSessionRemoved, source: ":1.44", path: "/org/freedesktop/login1/session/_88"})
	client.emit(event{kind: eventSessionState, source: ":1.44", path: "/org/freedesktop/login1/session/_88", state: "closing"})
	if generation, safe := monitor.Snapshot(); generation != 1 || !safe {
		t.Fatalf("Snapshot() = (%d, %t), want unrelated changes ignored", generation, safe)
	}
}

func TestActiveAndOnlineSessionTransitionsRemainSafe(t *testing.T) {
	client := newFakeClient()
	monitor := startFake(t, client)

	client.emit(event{kind: eventSessionState, source: ":1.44", path: testSessionPath, state: "online"})
	client.emit(event{kind: eventSessionState, source: ":1.44", path: testSessionPath, state: "active"})
	if generation, safe := monitor.Snapshot(); generation != 1 || !safe {
		t.Fatalf("Snapshot() = (%d, %t), want valid transitions ignored", generation, safe)
	}
}

func TestBusLossAndContextCancellationDisableMonitor(t *testing.T) {
	t.Run("bus loss", func(t *testing.T) {
		client := newFakeClient()
		monitor := startFake(t, client)
		client.disconnect()
		waitDone(t, monitor.Done())
		if _, safe := monitor.Snapshot(); safe {
			t.Fatal("monitor remained safe after bus loss")
		}
		if err := monitor.Err(); err == nil || !strings.Contains(err.Error(), "connection closed") {
			t.Fatalf("Err() = %v, want connection diagnostic", err)
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		client := newFakeClient()
		monitor, err := start(ctx, 42, time.Second, fakeConnector(client))
		if err != nil {
			t.Fatalf("start monitor: %v", err)
		}
		cancel()
		waitDone(t, monitor.Done())
		if _, safe := monitor.Snapshot(); safe {
			t.Fatal("monitor remained safe after context cancellation")
		}
		if !errors.Is(monitor.Err(), context.Canceled) {
			t.Fatalf("Err() = %v, want context.Canceled", monitor.Err())
		}
	})
}

func TestCloseIsIdempotentAndSuppressesFailure(t *testing.T) {
	client := newFakeClient()
	client.inhibitor.closeErr = errors.New("close inhibitor")
	client.closeErr = errors.New("close bus")
	monitor := startFake(t, client)

	err := monitor.Close()
	if err == nil || !strings.Contains(err.Error(), "close inhibitor") || !strings.Contains(err.Error(), "close bus") {
		t.Fatalf("Close() = %v, want both resource errors", err)
	}
	if second := monitor.Close(); second == nil || second.Error() != err.Error() {
		t.Fatalf("second Close() = %v, want %v", second, err)
	}
	if monitor.Err() != nil {
		t.Fatalf("Err() after explicit close = %v, want nil", monitor.Err())
	}
	if got := client.inhibitor.closeCount.Load(); got != 1 {
		t.Fatalf("inhibitor close count = %d, want 1", got)
	}
	if got := client.closeCount.Load(); got != 1 {
		t.Fatalf("client close count = %d, want 1", got)
	}
}

func TestStartupRejectsUnsafeOrIncompleteState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeClient)
		text   string
	}{
		{name: "subscription", mutate: func(client *fakeClient) { client.subscribeErr = errors.New("denied") }, text: "subscribe"},
		{name: "owner lookup", mutate: func(client *fakeClient) { client.ownerErr = errors.New("gone") }, text: "resolve logind bus owner"},
		{name: "invalid owner", mutate: func(client *fakeClient) { client.owner = "org.freedesktop.login1" }, text: "invalid unique name"},
		{name: "inhibitor", mutate: func(client *fakeClient) { client.inhibitErr = errors.New("denied") }, text: "delay inhibitor"},
		{name: "no own session", mutate: func(client *fakeClient) { client.sessionErr = errors.New("no session") }, text: "resolve logind session"},
		{name: "closing session", mutate: func(client *fakeClient) { client.state = "closing" }, text: "not usable"},
		{name: "shutdown underway", mutate: func(client *fakeClient) { client.preparingShutdown = true }, text: "already preparing for shutdown"},
		{name: "sleep underway", mutate: func(client *fakeClient) { client.preparingSleep = true }, text: "already preparing for sleep"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newFakeClient()
			test.mutate(client)
			monitor, err := start(context.Background(), 42, time.Second, fakeConnector(client))
			if err == nil || !strings.Contains(err.Error(), test.text) {
				t.Fatalf("start() = (%v, %v), want error containing %q", monitor, err, test.text)
			}
			if monitor != nil {
				t.Fatal("start returned a partially initialized monitor")
			}
			if client.inhibitorWasReturned() && client.inhibitor.closeCount.Load() != 1 {
				t.Fatal("startup failure leaked inhibitor")
			}
			if client.closeCount.Load() != 1 {
				t.Fatal("startup failure did not close D-Bus client")
			}
		})
	}
}

func TestSignalDuringStartupCannotEnableMonitor(t *testing.T) {
	client := newFakeClient()
	client.sleepHook = func() {
		client.emit(event{kind: eventPrepareShutdown, source: ":1.44", active: true})
	}
	monitor, err := start(context.Background(), 42, time.Second, fakeConnector(client))
	if err == nil || !strings.Contains(err.Error(), "shutdown preparation changed during monitor startup") {
		t.Fatalf("start() = (%v, %v), want shutdown invalidation", monitor, err)
	}
	if monitor != nil {
		t.Fatal("start returned monitor after startup invalidation")
	}
	if client.inhibitor.closeCount.Load() != 1 {
		t.Fatal("startup invalidation leaked inhibitor")
	}
}

func TestStartupTimeoutIsBounded(t *testing.T) {
	client := newFakeClient()
	client.subscribeHook = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	started := time.Now()
	monitor, err := start(context.Background(), 42, 20*time.Millisecond, fakeConnector(client))
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("start() = (%v, %v), want deadline exceeded", monitor, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("startup took %s despite bounded context", elapsed)
	}
}

func TestNilMonitorIsConservativelyUnsafe(t *testing.T) {
	var monitor *Monitor
	if generation, safe := monitor.Snapshot(); generation != 0 || safe {
		t.Fatalf("nil Snapshot() = (%d, %t), want (0, false)", generation, safe)
	}
	if err := monitor.Close(); err != nil {
		t.Fatalf("nil Close() = %v", err)
	}
	waitDone(t, monitor.Done())
}

type fakeClient struct {
	mu       sync.Mutex
	sink     func(event)
	calls    []string
	done     chan struct{}
	doneOnce sync.Once

	owner             string
	state             string
	sessionPath       string
	preparingShutdown bool
	preparingSleep    bool
	inhibitor         *fakeCloser

	subscribeErr error
	ownerErr     error
	inhibitErr   error
	sessionErr   error
	stateErr     error
	shutdownErr  error
	sleepErr     error
	closeErr     error

	subscribeHook func(context.Context) error
	inhibitHook   func(context.Context) (io.Closer, error)
	ownerHook     func()
	sleepHook     func()
	closeCount    atomic.Int32
	inhibitCalls  atomic.Int32
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		done:        make(chan struct{}),
		owner:       ":1.44",
		state:       "active",
		sessionPath: testSessionPath,
		inhibitor:   &fakeCloser{},
	}
}

func (client *fakeClient) Subscribe(ctx context.Context, sink func(event)) error {
	client.record("subscribe")
	client.mu.Lock()
	client.sink = sink
	client.mu.Unlock()
	if client.subscribeHook != nil {
		return client.subscribeHook(ctx)
	}
	return client.subscribeErr
}

func (client *fakeClient) NameOwner(context.Context) (string, error) {
	client.record("owner")
	if client.ownerHook != nil {
		client.ownerHook()
	}
	return client.owner, client.ownerErr
}

func (client *fakeClient) Inhibit(ctx context.Context) (io.Closer, error) {
	client.record("inhibit")
	client.inhibitCalls.Add(1)
	if client.inhibitHook != nil {
		return client.inhibitHook(ctx)
	}
	if client.inhibitErr != nil {
		return nil, client.inhibitErr
	}
	return client.inhibitor, nil
}

func (client *fakeClient) SessionByPID(context.Context, uint32) (string, error) {
	client.record("session")
	return client.sessionPath, client.sessionErr
}

func (client *fakeClient) SessionState(context.Context, string) (string, error) {
	client.record("state")
	return client.state, client.stateErr
}

func (client *fakeClient) PreparingForShutdown(context.Context) (bool, error) {
	client.record("shutdown")
	return client.preparingShutdown, client.shutdownErr
}

func (client *fakeClient) PreparingForSleep(context.Context) (bool, error) {
	client.record("sleep")
	if client.sleepHook != nil {
		client.sleepHook()
	}
	return client.preparingSleep, client.sleepErr
}

func (client *fakeClient) Done() <-chan struct{} {
	return client.done
}

func (client *fakeClient) Close() error {
	client.closeCount.Add(1)
	client.disconnect()
	return client.closeErr
}

func (client *fakeClient) disconnect() {
	client.doneOnce.Do(func() { close(client.done) })
}

func (client *fakeClient) emit(received event) {
	client.mu.Lock()
	sink := client.sink
	client.mu.Unlock()
	if sink != nil {
		sink(received)
	}
}

func (client *fakeClient) record(call string) {
	client.mu.Lock()
	client.calls = append(client.calls, call)
	client.mu.Unlock()
}

func (client *fakeClient) callSnapshot() []string {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]string(nil), client.calls...)
}

func (client *fakeClient) inhibitorWasReturned() bool {
	return client.inhibitCalls.Load() != 0 && client.inhibitErr == nil
}

type fakeCloser struct {
	closeCount atomic.Int32
	closeErr   error
	onClose    func()
}

func (closer *fakeCloser) Close() error {
	closer.closeCount.Add(1)
	if closer.onClose != nil {
		closer.onClose()
	}
	return closer.closeErr
}

func fakeConnector(client loginClient) connectFunc {
	return func(context.Context, context.Context) (loginClient, error) {
		return client, nil
	}
}

func startFake(t *testing.T, client *fakeClient) *Monitor {
	t.Helper()
	monitor, err := start(context.Background(), 42, time.Second, fakeConnector(client))
	if err != nil {
		t.Fatalf("start monitor: %v", err)
	}
	t.Cleanup(func() { _ = monitor.Close() })
	return monitor
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for monitor shutdown")
	}
}

func waitSnapshot(t *testing.T, monitor *Monitor, generation uint64, safe bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		actualGeneration, actualSafe := monitor.Snapshot()
		if actualGeneration == generation && actualSafe == safe {
			return
		}
		time.Sleep(time.Millisecond)
	}
	actualGeneration, actualSafe := monitor.Snapshot()
	t.Fatalf("Snapshot() = (%d, %t), want (%d, %t)", actualGeneration, actualSafe, generation, safe)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
