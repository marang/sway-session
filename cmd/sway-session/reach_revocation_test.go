package main

import (
	"context"
	"errors"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// Snapshot is already the executor's synchronous compositor-authority seam.
// A one-shot hook lets these tests change intent at a boundary without sleeps.
type reachRevocationGuard struct{ snapshot func() }

func (guard *reachRevocationGuard) Snapshot() (uint64, bool) {
	if guard.snapshot != nil {
		guard.snapshot()
	}
	return 1, true
}

func TestReachRevocationQueuedFocusBeforeInitialObservation(t *testing.T) {
	runtime, client, environment, _ := reachExecutionFixture(t)
	events := make(chan swayipc.Event, 2)
	events <- swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: 42, Type: "con"}, StreamEpoch: 1}
	events <- swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: 41, Type: "con"}, StreamEpoch: 1}
	_, err := runtime.executeReach(t.Context(), reachExecutionRequest(environment), events)
	if err == nil || len(client.commands) != 0 || client.reads != 0 {
		t.Fatalf("queued focus-away/back became a new baseline: err=%v reads=%d commands=%v", err, client.reads, client.commands)
	}
}

func TestReachRevocationHandledEventCancelsQueuedInvocation(t *testing.T) {
	runtime, client, environment, _ := reachExecutionFixture(t)
	defer runtime.reach.cancel()
	done := make(chan error, 1)
	go func() { _, err := runtime.reach.call(t.Context(), reachExecutionRequest(environment)); done <- err }()
	invocation := reachTestReceive(t, runtime.reach.inbox)
	// This is the actor ordering where the event branch wins before execution.
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: 42, Type: "con"}, StreamEpoch: 1}, time.Now())
	if !errors.Is(invocation.ctx.Err(), context.Canceled) {
		t.Fatal("event handler retained queued focus authority")
	}
	_, err := runtime.executeReach(invocation.ctx, invocation.request, nil)
	if !errors.Is(err, context.Canceled) || len(client.commands) != 0 || client.reads != 0 {
		t.Fatalf("revoked invocation executed: %v reads=%d commands=%v", err, client.reads, client.commands)
	}
	if err := reachTestReceive(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting caller did not observe revocation: %v", err)
	}
}

func TestReachRevocationExpectedFeedbackDoesNotCancelNewSelection(t *testing.T) {
	runtime, client, environment, _ := reachExecutionFixture(t)
	result, err := runtime.executeReach(t.Context(), reachExecutionRequest(environment), nil)
	if err != nil || result.Members[0].Status != sessionstate.ReachFocused || len(client.barriers) != 1 || len(runtime.expectedFocus) == 0 {
		t.Fatalf("first effect not attributed: %+v %v barriers=%v", result, err, client.barriers)
	}
	// A second valid explicit primary requires another focus effect. Registration
	// and environment edits are ordinary metadata operations between requests.
	if err := sessionstate.RegistryStoreFor(runtime.root).Save(sessionRegistryIDs(testManagedContextID, restoreReportSecondID)); err != nil {
		t.Fatal(err)
	}
	store := sessionstate.WorkEnvironmentStoreFor(runtime.root)
	environment, err = store.AddMemberContext(t.Context(), environment.ID, environment.Revision, restoreReportSecondID)
	if err != nil {
		t.Fatal(err)
	}
	client.tree.Nodes[0].Nodes[0].Nodes[1] = managedDaemonLeaf(t, 42, restoreReportSecondID)
	events := make(chan swayipc.Event, 2)
	events <- swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: 41, Type: "con"}, StreamEpoch: 1}
	events <- swayipc.Event{Type: swayipc.EventTick, Payload: client.barriers[0], StreamEpoch: 1}
	generation := runtime.reach.revocationGeneration()
	request := reachExecutionRequest(environment)
	request.Primary = restoreReportSecondID
	result, err = runtime.executeReach(t.Context(), request, events)
	if err != nil || len(result.Members) != 2 || result.Members[1].Status != sessionstate.ReachFocused || len(client.commands) != 2 || client.commands[1] != "[con_id=42] focus" {
		t.Fatalf("old command feedback revoked new selection: %+v %v commands=%v", result, err, client.commands)
	}
	if runtime.reach.revocationGeneration() != generation {
		t.Fatal("attributed feedback was counted as user revocation")
	}
}

func TestReachRevocationEventBacklogIsBoundedAndCancellationLeavesWork(t *testing.T) {
	for _, cancelWithin := range []bool{false, true} {
		t.Run(map[bool]string{false: "batch-budget", true: "canceled-during-drain"}[cancelWithin], func(t *testing.T) {
			runtime, client, environment, _ := reachExecutionFixture(t)
			events := make(chan swayipc.Event, 256)
			for range cap(events) {
				events <- swayipc.Event{Type: swayipc.EventWindow, Change: "title", Container: &Node{ID: 42, Type: "con"}, StreamEpoch: 1}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelWithin {
				runtime.eventStreamState = &reachRevocationGuard{snapshot: cancel}
			}
			_, err := runtime.executeReach(ctx, reachExecutionRequest(environment), events)
			if err == nil || len(client.commands) != 0 || client.reads != 0 || len(events) == 0 {
				t.Fatalf("busy drain consumed actor work or authorized effect: %v pending=%d reads=%d commands=%v", err, len(events), client.reads, client.commands)
			}
			if cancelWithin && len(events) != 255 {
				t.Fatalf("drain ignored cancellation between events: pending=%d", len(events))
			}
			if !cancelWithin && len(events) < 192 {
				t.Fatalf("drain exceeded the bounded batch: pending=%d", len(events))
			}
		})
	}
}

func TestReachRevocationSurvivesBlockedRestoreCancellationDiagnostics(t *testing.T) {
	for _, change := range []string{"client-cancel", "newer-selection", "user-focus"} {
		t.Run(change, func(t *testing.T) {
			runtime, client, environment, _ := reachExecutionFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			defer runtime.reach.cancel()
			now := time.Now().UTC()
			record := sessionstate.RestoreOutcome{ContextID: testManagedContextID, AttemptID: "33333333-3333-4333-8333-333333333333", Source: "automatic", StartedAt: now, UpdatedAt: now, Status: "pending", Reason: "restore_requested", Requested: sessionstate.RestoreWork{Window: true}, OwnerRunID: runtime.restoreRunID, IdentityDigest: sessionstate.RestoreContextDigest(sessionRegistry(testManagedContextID).Contexts[0])}
			if err := sessionstate.RestoreReportStoreFor(runtime.root).BeginAutomaticContext(t.Context(), runtime.restoreRunID, []sessionstate.RestoreOutcome{record}); err != nil {
				t.Fatal(err)
			}
			runtime.restoreReportSeeded = true
			database := openRawStateDatabaseForTest(t, runtime.root)
			writer, err := database.Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			defer func() { _, _ = writer.ExecContext(context.Background(), "ROLLBACK") }()
			events := make(chan swayipc.Event, 1)
			locked, injected := false, false
			var newer *reachInvocation
			var oldDone, newDone chan error
			request := reachExecutionRequest(environment)
			if change == "newer-selection" {
				oldDone = make(chan error, 1)
				go func() { _, err := runtime.reach.call(ctx, request); oldDone <- err }()
				invocation := reachTestReceive(t, runtime.reach.inbox)
				ctx = invocation.ctx
			}
			client.beforeTree = func(read int) {
				if read != 2 {
					return
				}
				if _, err := writer.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
					t.Fatal(err)
				}
				locked = true
			}
			runtime.eventStreamState = &reachRevocationGuard{snapshot: func() {
				if !locked || injected {
					return
				}
				injected = true
				// This synchronous guard is reached inside cancelConflictingRestore after
				// the last pre-effect check. The real diagnostic transaction then attempts
				// the held writer and reaches its bounded busy failure. Intent must still
				// be rechecked after that I/O, before any external command.
				switch change {
				case "client-cancel":
					cancel()
				case "user-focus":
					events <- swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: 42, Type: "con"}, StreamEpoch: 1}
				case "newer-selection":
					newDone = make(chan error, 1)
					go func() { _, err := runtime.reach.call(t.Context(), request); newDone <- err }()
					invocation := reachTestReceive(t, runtime.reach.inbox)
					newer = &invocation
				}
			}}
			result, err := runtime.executeReach(ctx, request, events)
			if !locked || !injected || runtime.restoreReportErr == nil {
				t.Fatalf("test did not cross blocked diagnostic I/O: locked=%t injected=%t diagnostic=%v result=%+v err=%v", locked, injected, runtime.restoreReportErr, result, err)
			}
			if len(client.commands) != 0 {
				t.Fatalf("revoked intent escaped diagnostic wait (%s): %v", change, client.commands)
			}
			if len(result.Members) != 1 || result.Members[0].Status != sessionstate.ReachCanceled {
				t.Fatalf("revoked result not explicit: %+v %v", result, err)
			}
			if oldDone != nil {
				if err := reachTestReceive(t, oldDone); !errors.Is(err, context.Canceled) {
					t.Fatalf("older caller survived new selection: %v", err)
				}
			}
			if newer != nil {
				if newer.ctx.Err() != nil {
					t.Fatalf("new intent was canceled with old one: %v", newer.ctx.Err())
				}
				newer.reply <- reachReply{result: sessionstate.ReachResult{EnvironmentID: environment.ID}}
				if err := reachTestReceive(t, newDone); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
