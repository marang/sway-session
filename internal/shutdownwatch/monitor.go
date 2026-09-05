// Package shutdownwatch provides a conservative logind shutdown and sleep
// guard for automatic persistence work.
package shutdownwatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const startupTimeout = 5 * time.Second

var errStartupInvalidated = errors.New("logind state changed during shutdown monitor startup")

type eventKind uint8

const (
	eventInvalid eventKind = iota
	eventPrepareShutdown
	eventPrepareSleep
	eventSessionRemoved
	eventSessionState
	eventNameOwnerChanged
)

type event struct {
	kind        eventKind
	source      string
	active      bool
	path        string
	state       string
	invalidated bool
	err         error
}

type loginClient interface {
	Subscribe(context.Context, func(event)) error
	NameOwner(context.Context) (string, error)
	Inhibit(context.Context) (io.Closer, error)
	SessionByPID(context.Context, uint32) (string, error)
	SessionState(context.Context, string) (string, error)
	PreparingForShutdown(context.Context) (bool, error)
	PreparingForSleep(context.Context) (bool, error)
	Done() <-chan struct{}
	Close() error
}

type connectFunc func(context.Context, context.Context) (loginClient, error)

// Monitor publishes whether logind can currently be trusted to let the daemon
// finish automatic persistence work. A monitor starts unsafe and becomes safe
// only after its signal subscriptions, delay inhibitor, session, and live
// preparation properties have all been validated.
//
// A preparation event disables the monitor and releases the delay inhibitor.
// After cancellation or resume, the monitor remains unsafe until a fresh
// inhibitor and all live state have been revalidated. Monitoring failures are
// permanent and require a daemon restart.
type Monitor struct {
	epoch               atomic.Uint64
	preparationSequence atomic.Uint64

	mu          sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	client      loginClient
	inhibitor   *heldInhibitor
	owner       string
	sessionPath string
	pid         uint32
	ready       bool
	shutdown    bool
	sleep       bool
	rearming    bool
	rearmID     uint64
	finished    bool
	err         error
	closeErr    error

	finishOnce sync.Once
	rearmWG    sync.WaitGroup
	done       chan struct{}
}

// Start connects to the system bus and establishes a delay inhibitor for
// shutdown and sleep. It returns an error rather than an unsafe monitor if the
// complete guard cannot be established within a bounded startup interval.
func Start(ctx context.Context) (*Monitor, error) {
	return start(ctx, os.Getpid(), startupTimeout, connectSystemBus)
}

func start(ctx context.Context, pid int, timeout time.Duration, connect connectFunc) (*Monitor, error) {
	if ctx == nil {
		return nil, errors.New("start shutdown monitor: nil context")
	}
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("start shutdown monitor: invalid process ID %d", pid)
	}
	if timeout <= 0 {
		return nil, errors.New("start shutdown monitor: startup timeout must be positive")
	}
	if connect == nil {
		return nil, errors.New("start shutdown monitor: nil system bus connector")
	}

	runCtx, cancel := context.WithCancel(ctx)
	monitor := &Monitor{
		ctx:    runCtx,
		cancel: cancel,
		done:   make(chan struct{}),
		pid:    uint32(pid),
	}
	go func() {
		<-runCtx.Done()
		monitor.finish(runCtx.Err())
	}()

	setupCtx, stopSetup := context.WithTimeout(runCtx, timeout)
	defer stopSetup()
	client, err := connect(runCtx, setupCtx)
	if err != nil {
		monitor.finish(fmt.Errorf("connect to system bus: %w", err))
		return nil, monitor.startError()
	}
	if client == nil {
		monitor.finish(errors.New("connect to system bus: connector returned no client"))
		return nil, monitor.startError()
	}
	if !monitor.bindClient(client) {
		_ = client.Close()
		return nil, monitor.startError()
	}

	if err := monitor.initialize(setupCtx, uint32(pid)); err != nil {
		monitor.finish(err)
		return nil, monitor.startError()
	}
	if !monitor.activate() {
		monitor.finish(errStartupInvalidated)
		return nil, monitor.startError()
	}

	return monitor, nil
}

func (monitor *Monitor) initialize(ctx context.Context, pid uint32) error {
	monitor.mu.Lock()
	client := monitor.client
	monitor.mu.Unlock()
	if client == nil {
		return errors.New("system D-Bus client is unavailable")
	}
	if err := client.Subscribe(ctx, monitor.handleEvent); err != nil {
		return fmt.Errorf("subscribe to logind lifecycle signals: %w", err)
	}

	owner, err := client.NameOwner(ctx)
	if err != nil {
		return fmt.Errorf("resolve logind bus owner: %w", err)
	}
	if owner == "" || owner[0] != ':' {
		return fmt.Errorf("resolve logind bus owner: invalid unique name %q", owner)
	}
	if !monitor.setOwner(owner) {
		return errStartupInvalidated
	}

	inhibitor, err := client.Inhibit(ctx)
	if err != nil {
		return fmt.Errorf("acquire logind delay inhibitor: %w", err)
	}
	if inhibitor == nil {
		return errors.New("acquire logind delay inhibitor: empty file descriptor")
	}
	if !monitor.bindInhibitor(inhibitor) {
		_ = inhibitor.Close()
		return errStartupInvalidated
	}

	sessionPath, err := client.SessionByPID(ctx, pid)
	if err != nil {
		return fmt.Errorf("resolve logind session for PID %d: %w", pid, err)
	}
	if sessionPath == "" {
		return fmt.Errorf("resolve logind session for PID %d: empty object path", pid)
	}
	if !monitor.setSessionPath(sessionPath) {
		return errStartupInvalidated
	}

	state, err := client.SessionState(ctx, sessionPath)
	if err != nil {
		return fmt.Errorf("read logind session state: %w", err)
	}
	if !validSessionState(state) {
		return fmt.Errorf("logind session is not usable: state %q", state)
	}

	preparingShutdown, err := client.PreparingForShutdown(ctx)
	if err != nil {
		return fmt.Errorf("read logind shutdown preparation state: %w", err)
	}
	if preparingShutdown {
		return errors.New("logind is already preparing for shutdown")
	}

	preparingSleep, err := client.PreparingForSleep(ctx)
	if err != nil {
		return fmt.Errorf("read logind sleep preparation state: %w", err)
	}
	if preparingSleep {
		return errors.New("logind is already preparing for sleep")
	}
	return nil
}

// Snapshot returns the lifecycle generation and whether automatic persistence
// work is currently safe. Generations change before safe changes are exposed.
func (monitor *Monitor) Snapshot() (uint64, bool) {
	if monitor == nil {
		return 0, false
	}
	epoch := monitor.epoch.Load()
	return epoch, epoch%2 == 1
}

// Done is closed after the monitor becomes permanently unavailable and all
// owned resources have been released.
func (monitor *Monitor) Done() <-chan struct{} {
	if monitor == nil {
		return closedDone
	}
	return monitor.done
}

// Err reports why monitoring stopped. It is nil while monitoring is active and
// after an explicit Close.
func (monitor *Monitor) Err() error {
	if monitor == nil {
		return nil
	}
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	return monitor.err
}

// Close disables the guard and releases the inhibitor and private D-Bus
// connection. It is safe to call more than once.
func (monitor *Monitor) Close() error {
	if monitor == nil {
		return nil
	}
	monitor.finish(nil)
	<-monitor.done
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	return monitor.closeErr
}

func (monitor *Monitor) bindClient(client loginClient) bool {
	if client == nil {
		return false
	}
	monitor.mu.Lock()
	if monitor.finished {
		monitor.mu.Unlock()
		return false
	}
	monitor.client = client
	monitor.mu.Unlock()

	go func() {
		select {
		case <-client.Done():
			monitor.finish(errors.New("system D-Bus connection closed"))
		case <-monitor.done:
		}
	}()
	return true
}

type heldInhibitor struct {
	closer io.Closer
	rearm  uint64
}

func (monitor *Monitor) bindInhibitor(inhibitor io.Closer) bool {
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	if monitor.finished {
		return false
	}
	monitor.inhibitor = &heldInhibitor{closer: inhibitor}
	return true
}

func (monitor *Monitor) setOwner(owner string) bool {
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	if monitor.finished {
		return false
	}
	monitor.owner = owner
	return true
}

func (monitor *Monitor) setSessionPath(path string) bool {
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	if monitor.finished {
		return false
	}
	monitor.sessionPath = path
	return true
}

func (monitor *Monitor) activate() bool {
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	if monitor.finished || monitor.client == nil || monitor.inhibitor == nil ||
		monitor.owner == "" || monitor.sessionPath == "" || monitor.shutdown || monitor.sleep ||
		monitor.preparationSequence.Load() != 0 {
		return false
	}
	monitor.epoch.Add(1)
	if monitor.preparationSequence.Load() != 0 {
		monitor.forceUnsafe()
		return false
	}
	monitor.ready = true
	return true
}

func (monitor *Monitor) handleEvent(received event) {
	if (received.kind == eventPrepareShutdown || received.kind == eventPrepareSleep) &&
		received.active && received.err == nil {
		// Publish unsafe before contending on the lifecycle mutex. The second
		// transition in handlePreparation closes the race with a concurrent
		// rearm commit that had already passed its sequence check.
		monitor.preparationSequence.Add(1)
		monitor.forceUnsafe()
	}
	monitor.mu.Lock()
	if monitor.finished {
		monitor.mu.Unlock()
		return
	}
	owner := monitor.owner
	sessionPath := monitor.sessionPath
	monitor.mu.Unlock()

	if received.err != nil || received.kind == eventInvalid {
		if received.err == nil {
			received.err = errors.New("invalid logind lifecycle signal")
		}
		monitor.finish(fmt.Errorf("monitor logind lifecycle: %w", received.err))
		return
	}
	if received.kind != eventNameOwnerChanged && owner != "" && received.source != owner {
		monitor.finish(fmt.Errorf("monitor logind lifecycle: signal sender changed from %q to %q", owner, received.source))
		return
	}

	switch received.kind {
	case eventPrepareShutdown:
		monitor.handlePreparation(true, received.active)
	case eventPrepareSleep:
		monitor.handlePreparation(false, received.active)
	case eventNameOwnerChanged:
		monitor.finish(errors.New("logind D-Bus name owner changed"))
	case eventSessionRemoved:
		if sessionPath == "" || received.path == sessionPath {
			monitor.finish(errors.New("own logind session was removed"))
		}
	case eventSessionState:
		if sessionPath != "" && received.path == sessionPath &&
			(received.invalidated || !validSessionState(received.state)) {
			monitor.finish(fmt.Errorf("own logind session became unusable: state %q", received.state))
		}
	}
}

func (monitor *Monitor) handlePreparation(shutdown, active bool) {
	monitor.mu.Lock()
	if monitor.finished {
		monitor.mu.Unlock()
		return
	}
	if !monitor.ready {
		monitor.mu.Unlock()
		if shutdown {
			monitor.finish(errors.New("logind shutdown preparation changed during monitor startup"))
		} else {
			monitor.finish(errors.New("logind sleep preparation changed during monitor startup"))
		}
		return
	}
	if shutdown {
		monitor.shutdown = active
	} else {
		monitor.sleep = active
	}

	if active {
		monitor.rearmID++
		monitor.rearming = false
		monitor.makeUnsafeLocked()
		held := monitor.inhibitor
		monitor.inhibitor = nil
		monitor.mu.Unlock()
		if err := closeHeld(held); err != nil {
			monitor.finish(fmt.Errorf("release logind delay inhibitor for preparation: %w", err))
		}
		return
	}
	if monitor.shutdown || monitor.sleep || monitor.rearming {
		monitor.mu.Unlock()
		return
	}

	// A false signal normally follows a true signal. Treat an isolated false
	// conservatively as a new validation boundary too.
	monitor.makeUnsafeLocked()
	held := monitor.inhibitor
	monitor.inhibitor = nil
	monitor.rearmID++
	rearmID := monitor.rearmID
	preparationSequence := monitor.preparationSequence.Load()
	monitor.rearming = true
	monitor.rearmWG.Add(1)
	monitor.mu.Unlock()
	if err := closeHeld(held); err != nil {
		monitor.finish(fmt.Errorf("release stale logind delay inhibitor: %w", err))
		return
	}
	go func() {
		defer monitor.rearmWG.Done()
		monitor.rearm(rearmID, preparationSequence)
	}()
}

func (monitor *Monitor) rearm(rearmID, preparationSequence uint64) {
	ctx, cancel := context.WithTimeout(monitor.ctx, startupTimeout)
	defer cancel()

	monitor.mu.Lock()
	client := monitor.client
	expectedOwner := monitor.owner
	expectedSession := monitor.sessionPath
	pid := monitor.pid
	monitor.mu.Unlock()
	if client == nil {
		monitor.failRearm(rearmID, errors.New("system D-Bus client is unavailable"))
		return
	}

	inhibitor, err := client.Inhibit(ctx)
	if err != nil {
		monitor.failRearm(rearmID, fmt.Errorf("reacquire logind delay inhibitor: %w", err))
		return
	}
	if inhibitor == nil {
		monitor.failRearm(rearmID, errors.New("reacquire logind delay inhibitor: empty file descriptor"))
		return
	}
	held := &heldInhibitor{closer: inhibitor, rearm: rearmID}
	if !monitor.bindRearmInhibitor(held) {
		_ = inhibitor.Close()
		return
	}

	owner, err := client.NameOwner(ctx)
	if err != nil {
		monitor.failRearm(rearmID, fmt.Errorf("revalidate logind bus owner: %w", err))
		return
	}
	if owner != expectedOwner {
		monitor.failRearm(rearmID, fmt.Errorf("revalidate logind bus owner: changed from %q to %q", expectedOwner, owner))
		return
	}
	sessionPath, err := client.SessionByPID(ctx, pid)
	if err != nil {
		monitor.failRearm(rearmID, fmt.Errorf("revalidate logind session for PID %d: %w", pid, err))
		return
	}
	if sessionPath != expectedSession {
		monitor.failRearm(rearmID, fmt.Errorf("revalidate logind session: changed from %q to %q", expectedSession, sessionPath))
		return
	}
	state, err := client.SessionState(ctx, sessionPath)
	if err != nil {
		monitor.failRearm(rearmID, fmt.Errorf("revalidate logind session state: %w", err))
		return
	}
	if !validSessionState(state) {
		monitor.failRearm(rearmID, fmt.Errorf("revalidate logind session: unusable state %q", state))
		return
	}
	preparingShutdown, err := client.PreparingForShutdown(ctx)
	if err != nil {
		monitor.failRearm(rearmID, fmt.Errorf("revalidate logind shutdown preparation state: %w", err))
		return
	}
	preparingSleep, err := client.PreparingForSleep(ctx)
	if err != nil {
		monitor.failRearm(rearmID, fmt.Errorf("revalidate logind sleep preparation state: %w", err))
		return
	}
	if preparingShutdown || preparingSleep {
		monitor.failRearm(rearmID, errors.New("logind still reports active shutdown or sleep preparation"))
		return
	}

	monitor.mu.Lock()
	if monitor.finished || monitor.rearmID != rearmID || monitor.shutdown || monitor.sleep ||
		monitor.inhibitor != held || monitor.preparationSequence.Load() != preparationSequence {
		monitor.mu.Unlock()
		monitor.releaseIfOwned(held)
		return
	}
	monitor.epoch.Add(1)
	if monitor.preparationSequence.Load() != preparationSequence {
		monitor.forceUnsafe()
		monitor.mu.Unlock()
		return
	}
	monitor.rearming = false
	monitor.mu.Unlock()
}

func (monitor *Monitor) bindRearmInhibitor(held *heldInhibitor) bool {
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	if monitor.finished || monitor.rearmID != held.rearm || monitor.shutdown || monitor.sleep {
		return false
	}
	monitor.inhibitor = held
	return true
}

func (monitor *Monitor) releaseIfOwned(held *heldInhibitor) {
	monitor.mu.Lock()
	if monitor.inhibitor != held {
		monitor.mu.Unlock()
		return
	}
	monitor.inhibitor = nil
	monitor.mu.Unlock()
	_ = closeHeld(held)
}

func (monitor *Monitor) failRearm(rearmID uint64, err error) {
	monitor.mu.Lock()
	current := !monitor.finished && monitor.rearmID == rearmID
	monitor.mu.Unlock()
	if current {
		// finish waits for every rearm worker so Close cannot return while a
		// newly received inhibitor descriptor is still local to one. Run it
		// outside this worker to avoid waiting on ourselves.
		go monitor.finish(fmt.Errorf("restore shutdown monitor after preparation cycle: %w", err))
	}
}

func (monitor *Monitor) makeUnsafeLocked() {
	monitor.forceUnsafe()
}

func (monitor *Monitor) forceUnsafe() {
	for {
		epoch := monitor.epoch.Load()
		if epoch%2 == 0 || monitor.epoch.CompareAndSwap(epoch, epoch+1) {
			return
		}
	}
}

func closeHeld(held *heldInhibitor) error {
	if held != nil {
		return held.closer.Close()
	}
	return nil
}

func validSessionState(state string) bool {
	return state == "active" || state == "online"
}

func (monitor *Monitor) finish(cause error) {
	monitor.finishOnce.Do(func() {
		monitor.mu.Lock()
		monitor.finished = true
		monitor.forceUnsafe()
		monitor.err = cause
		inhibitor := monitor.inhibitor
		monitor.inhibitor = nil
		client := monitor.client
		monitor.client = nil
		monitor.mu.Unlock()

		var closeErr error
		if inhibitor != nil {
			closeErr = errors.Join(closeErr, inhibitor.closer.Close())
		}
		monitor.cancel()
		if client != nil {
			closeErr = errors.Join(closeErr, client.Close())
		}
		monitor.rearmWG.Wait()

		monitor.mu.Lock()
		monitor.closeErr = closeErr
		monitor.mu.Unlock()
		close(monitor.done)
	})
}

func (monitor *Monitor) startError() error {
	<-monitor.done
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	if monitor.err == nil {
		return errors.New("start shutdown monitor: monitor stopped during startup")
	}
	return fmt.Errorf("start shutdown monitor: %w", monitor.err)
}

var closedDone = func() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}()
