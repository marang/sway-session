package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// These cases use real private Sway windows and IPC events with an injected
// healthy shutdown guard and clock. The adapter runs sleep, not Herdr or an
// agent. Window close/reopen is not evidence of logind ordering or a reboot.
// SWAY_SESSION_HEADLESS_INTEGRATION=1 GOTOOLCHAIN=go1.26.5 go test -race ./cmd/sway-session -run '^TestTerminalLifecycle.*Headless$' -count=1 -v
func TestTerminalLifecycleHealthyCloseReopenHeadless(t *testing.T) {
	f := newTerminalLifecycleHeadless(t)
	first := f.mapTerminal()
	f.closeTerminal(first)
	closedAt := f.now
	f.now = closedAt.Add(terminalCloseGrace - time.Nanosecond)
	reads := f.requester.treeReads
	f.flush()
	if f.requester.treeReads != reads {
		t.Fatal("close confirmation queried Sway before the grace elapsed")
	}
	f.registry(sessionstate.ContextActive)

	f.now = closedAt.Add(terminalCloseGrace)
	f.flush()
	if f.requester.treeReads != reads+1 {
		t.Fatal("healthy close did not use one fresh Sway absence observation")
	}
	archived := f.registry(sessionstate.ContextArchived).Contexts[0]
	if archived.Lifecycle == nil || archived.Lifecycle.Reason != sessionstate.LifecycleReasonObservedTerminalClose || !archived.Lifecycle.At.Equal(f.now) {
		t.Fatalf("close transition was not committed with archive: %+v", archived.Lifecycle)
	}
	if archived.ArchivedAt == nil || !archived.ArchivedAt.Equal(f.now) {
		t.Fatalf("archive timestamp = %v, want %v", archived.ArchivedAt, f.now)
	}
	for range 3 {
		f.reconcile()
		f.flush()
		f.registry(sessionstate.ContextArchived)
	}
	if findContainerByID(f.h.tree(), first) != nil {
		t.Fatal("closed adapter reappeared")
	}

	// Use the public core lifecycle interface, including its durable registry
	// update, rather than assigning state directly in the test fixture.
	f.now = f.now.Add(time.Second)
	_, err := sessionstate.UpdateRegistryContext(f.h.ctx, f.h.state, func(registry *sessionstate.Registry) error {
		_, err := sessionstate.SetContextStateAt(registry, string(f.original.ID), sessionstate.ContextActive, f.now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	activated := f.registry(sessionstate.ContextActive).Contexts[0]
	if activated.ArchivedAt != nil || activated.Lifecycle == nil || activated.Lifecycle.Reason != sessionstate.LifecycleReasonExplicitActivate || !activated.Lifecycle.At.Equal(f.now) {
		t.Fatalf("explicit activation was not committed: %+v", activated)
	}
	second := f.mapTerminal()
	if second == first {
		t.Fatal("reopen reused the closed Sway container rather than mapping a new adapter")
	}
	for range 3 {
		f.reconcile()
		f.flush()
		f.assertPresent(second)
	}
	if findContainerByID(f.h.tree(), first) != nil {
		t.Fatal("reopen duplicated the old adapter")
	}
	t.Log("real new/focus/close IPC events; grace-confirmed durable archive; explicit activation and one newly mapped adapter retain the same context and launcher identity")
}

func TestTerminalLifecycleReopenBeforeGraceHeadless(t *testing.T) {
	f := newTerminalLifecycleHeadless(t)
	first := f.mapTerminal()
	f.closeTerminal(first)
	closedAt := f.now
	f.now = closedAt.Add(terminalCloseGrace / 2)
	second := f.h.terminal(f.original.ID)
	f.h.command(fmt.Sprintf("[con_id=%d] focus", second))
	events := f.drain()
	f.assertWindowEvent(events, "new", second)
	f.assertWindowEvent(events, "focus", second)
	if second == first {
		t.Fatal("replacement adapter did not receive a new container identity")
	}
	// Do not reconcile the replacement first: the pending close must be
	// rejected by Flush's own fresh IPC tree observation after its deadline.
	f.now = closedAt.Add(terminalCloseGrace)
	reads := f.requester.treeReads
	f.flush()
	if f.requester.treeReads != reads+1 {
		t.Fatal("reopened close candidate did not receive a fresh Sway presence check")
	}
	if len(f.runtime.pendingTerminalClose) != 0 {
		t.Fatal("freshly mapped replacement retained a close candidate")
	}
	for range 3 {
		f.now = f.now.Add(terminalCloseGrace)
		f.reconcile()
		f.flush()
		f.assertPresent(second)
		if f.registry(sessionstate.ContextActive).Contexts[0].Lifecycle != nil {
			t.Fatal("reopening before grace invented a lifecycle transition")
		}
	}
	t.Log("replacement maps before injected grace; fresh Sway presence prevents archival and duplicate adapters")
}

func TestTerminalLifecycleAlreadyMissingStartupHeadless(t *testing.T) {
	f := newTerminalLifecycleHeadless(t)
	for _, elapsed := range []time.Duration{0, terminalCloseGrace, sessionStartupSettleDelay + terminalCloseGrace, time.Minute} {
		f.now = f.start.Add(elapsed)
		f.reconcile()
		f.flush()
		registry := f.registry(sessionstate.ContextActive)
		windows, err := sessionstate.ObserveManagedWindows(f.h.tree(), registry)
		if err != nil || len(windows) != 0 {
			t.Fatalf("initially absent terminal acquired a window: %+v, %v", windows, err)
		}
		if registry.Contexts[0].Lifecycle != nil || registry.Contexts[0].ArchivedAt != nil || len(f.runtime.pendingTerminalClose) != 0 {
			t.Fatal("startup absence invented an archive or close transition")
		}
	}
	t.Log("healthy injected guard and real empty Sway observations beyond grace leave the already-missing context active")
}

type terminalLifecycleRequester struct {
	*restoreCleanupRealRequester
	treeReads int
}

func (r *terminalLifecycleRequester) RequestContext(ctx context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if kind == swayipc.GetTree {
		r.treeReads++
	}
	return r.restoreCleanupRealRequester.RequestContext(ctx, kind, payload)
}

type terminalLifecycleHeadless struct {
	t         *testing.T
	h         *restoreCleanupHeadless
	runtime   *sessionRuntime
	requester *terminalLifecycleRequester
	original  sessionstate.Context
	start     time.Time
	now       time.Time
}

func newTerminalLifecycleHeadless(t *testing.T) *terminalLifecycleHeadless {
	t.Helper()
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway/Alacritty integration")
	}
	for _, name := range []string{"sway", "alacritty", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("headless integration requires %s: %v", name, err)
		}
	}
	h := newRestoreCleanupHeadless(t)
	registry := sessionRegistry(testManagedContextID)
	registry.Contexts[0].Launcher.Cwd = h.root
	if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC()
	f := &terminalLifecycleHeadless{t: t, h: h, original: registry.Contexts[0], start: start, now: start}
	f.requester = &terminalLifecycleRequester{restoreCleanupRealRequester: &restoreCleanupRealRequester{Client: h.client}}
	stream := &swayipc.EventStreamState{}
	runtime, err := newSessionRuntimeWithOptions(f.requester, sessionRuntimeOptions{
		Context: h.ctx, Root: h.state, StartedAt: start, Now: func() time.Time { return f.now },
		EventStreamState: stream, TerminalCloseGuard: &testTerminalCloseGuard{generation: 7, safe: true},
		IndicatorCatalog: func() (sessionstate.DesktopCatalog, error) { return sessionstate.DesktopCatalog{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.runtime = runtime
	t.Cleanup(func() {
		if err := runtime.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	h.subscribe(runtime, stream)
	return f
}

func (f *terminalLifecycleHeadless) mapTerminal() int64 {
	f.t.Helper()
	f.h.command("workspace 98")
	id := f.h.terminal(f.original.ID)
	f.h.command(fmt.Sprintf("[con_id=%d] focus", id))
	events := f.drain()
	f.assertWindowEvent(events, "new", id)
	f.assertWindowEvent(events, "focus", id)
	f.reconcile()
	f.assertPresent(id)
	return id
}

func (f *terminalLifecycleHeadless) closeTerminal(id int64) {
	f.t.Helper()
	f.assertPresent(id)
	f.h.command(fmt.Sprintf("[con_id=%d] focus", id))
	f.drain()
	f.h.command(fmt.Sprintf("[con_id=%d] kill", id))
	f.h.until("owned closed adapter absent", func() bool { return findContainerByID(f.h.tree(), id) == nil })
	f.assertWindowEvent(f.drain(), "close", id)
	candidate, exists := f.runtime.pendingTerminalClose[id]
	if !exists || !candidate.deadline.Equal(f.now.Add(terminalCloseGrace)) {
		f.t.Fatal("real terminal close did not arm the injected grace deadline")
	}
}

func (f *terminalLifecycleHeadless) reconcile() {
	f.t.Helper()
	for range 8 {
		refresh, err := f.runtime.Reconcile(f.h.tree(), f.now)
		if err != nil {
			f.t.Fatal(err)
		}
		f.drain()
		if !refresh {
			return
		}
	}
	f.t.Fatal("terminal lifecycle reconciliation did not converge")
}

func (f *terminalLifecycleHeadless) flush() {
	f.t.Helper()
	if err := f.runtime.Flush(f.now); err != nil {
		f.t.Fatal(err)
	}
}

func (f *terminalLifecycleHeadless) registry(want sessionstate.ContextState) sessionstate.Registry {
	f.t.Helper()
	registry, err := sessionstate.ReadRegistrySnapshotContext(f.h.ctx, f.h.state)
	if err != nil {
		f.t.Fatal(err)
	}
	if len(registry.Contexts) != 1 || registry.Contexts[0].ID != f.original.ID || registry.Contexts[0].State != want || !reflect.DeepEqual(registry.Contexts[0].Launcher, f.original.Launcher) {
		f.t.Fatalf("durable terminal context changed identity or state: %+v; want %s", registry.Contexts, want)
	}
	return registry
}

func (f *terminalLifecycleHeadless) assertPresent(id int64) {
	f.t.Helper()
	registry := f.registry(sessionstate.ContextActive)
	tree := f.h.tree()
	windows, err := sessionstate.ObserveManagedWindows(tree, registry)
	if err != nil || len(windows) != 1 || windows[f.original.ID].ContainerID != id {
		f.t.Fatalf("expected exactly one adapter for stable context: %+v, %v", windows, err)
	}
	node := findContainerByID(tree, id)
	mark, err := f.original.ID.Mark()
	if err != nil || node == nil || !slices.Contains(node.Marks, mark) || restoreCleanupWorkspace(tree, id) != "98" {
		f.t.Fatalf("adapter missing its persistent mark or private workspace: %+v, %v", node, err)
	}
}

func (f *terminalLifecycleHeadless) assertWindowEvent(events []swayipc.Event, change string, id int64) {
	f.t.Helper()
	if !slices.ContainsFunc(events, func(event swayipc.Event) bool {
		return event.Type == swayipc.EventWindow && event.Change == change && event.Container != nil && event.Container.ID == id
	}) {
		f.t.Fatalf("missing real %s event for owned window %d", change, id)
	}
}

// Like the shared harness drain, this uses a real IPC tick barrier. Delivering
// every preceding event with f.now keeps mapping and close grace deterministic.
func (f *terminalLifecycleHeadless) drain() []swayipc.Event {
	f.t.Helper()
	f.h.tick++
	barrier := fmt.Sprintf("lab134-terminal-lifecycle-%d", f.h.tick)
	message, err := f.h.client.RequestContext(f.h.ctx, swayipc.SendTick, []byte(barrier))
	if err == nil {
		err = swayipc.CheckSendTickResponse(message)
	}
	if err != nil {
		f.t.Fatal(err)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var events []swayipc.Event
	for {
		select {
		case event := <-f.h.events:
			if event.Type == swayipc.EventShutdown || event.Type == swayipc.EventStream {
				f.t.Fatalf("private event stream lost continuity: %+v", event)
			}
			f.runtime.HandleEvent(event, f.now)
			events = append(events, event)
			if event.Type == swayipc.EventTick && event.Payload == barrier {
				return events
			}
		case <-timer.C:
			f.t.Fatal("timed out draining real terminal lifecycle events")
		}
	}
}
