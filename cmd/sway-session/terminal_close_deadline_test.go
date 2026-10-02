package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func TestObservedTerminalCloseDeadlineRetainsCandidateAndArchivesOnRetry(t *testing.T) {
	if terminalCloseWriteTimeout != 250*time.Millisecond {
		t.Fatalf("terminal close write timeout = %v, want 250ms", terminalCloseWriteTimeout)
	}
	guard := &testTerminalCloseGuard{generation: 7, safe: true}
	runtime, compositor, root, now, leaf := armedTerminalClose(t, guard)
	defer runtime.Shutdown()
	requester := &terminalCloseDeadlineRequester{daemonLoopRequester: compositor}
	runtime.client = requester
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: leaf}, now)
	candidate, exists := runtime.pendingTerminalClose[leaf.ID]
	if !exists {
		t.Fatal("close event did not queue a candidate")
	}

	flushAt := now.Add(terminalCloseGrace)
	started := time.Now()
	if err := runtime.Flush(flushAt); err != nil {
		t.Fatalf("terminal close deadline escaped retry handling: %v", err)
	}
	if requester.treeRequests != 1 || !errors.Is(requester.timeout, context.DeadlineExceeded) {
		t.Fatalf("first observation: requests=%d timeout=%v, want one expired request", requester.treeRequests, requester.timeout)
	}
	if requester.deadline.Before(started.Add(terminalCloseWriteTimeout)) || requester.deadline.After(time.Now()) {
		t.Fatalf("request deadline = %v, want the elapsed 250ms Flush deadline", requester.deadline)
	}
	if err := runtime.context().Err(); err != nil {
		t.Fatalf("close deadline canceled the runtime: %v", err)
	}
	assertTerminalCloseState(t, root, sessionstate.ContextActive)
	retained, exists := runtime.pendingTerminalClose[leaf.ID]
	if len(runtime.pendingTerminalClose) != 1 || !exists || !reflect.DeepEqual(retained, candidate) {
		t.Fatalf("deadline lost or changed the pending candidate: %+v", runtime.pendingTerminalClose)
	}
	retryAt := flushAt.Add(terminalFocusBatchDelay)
	if runtime.terminalCloseRetry != terminalFocusBatchDelay || !runtime.terminalCloseRetryDeadline.Equal(retryAt) || !runtime.terminalCloseDeadline.Equal(retryAt) {
		t.Fatalf("deadline did not arm the first retry: delay=%v retry=%v next=%v, want %v", runtime.terminalCloseRetry, runtime.terminalCloseRetryDeadline, runtime.terminalCloseDeadline, retryAt)
	}

	// Advance only the injected observation time; the failed request used the
	// real production deadline, and the retry must obtain a new fake Sway tree.
	if err := runtime.Flush(retryAt.Add(-time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	if requester.treeRequests != 1 {
		t.Fatalf("retry contacted the compositor before its deadline: %d requests", requester.treeRequests)
	}
	assertTerminalCloseState(t, root, sessionstate.ContextActive)
	if err := runtime.Flush(retryAt); err != nil {
		t.Fatalf("fresh absence observation could not archive on retry: %v", err)
	}
	if requester.treeRequests != 2 || compositor.requests != 1 {
		t.Fatalf("retry did not obtain exactly one fresh tree: attempts=%d successful requests=%d", requester.treeRequests, compositor.requests)
	}
	assertTerminalCloseState(t, root, sessionstate.ContextArchived)
	var persisted sessionstate.Registry
	if err := sessionstate.RegistryStoreFor(root).LoadInto(&persisted); err != nil {
		t.Fatal(err)
	}
	transition := persisted.Contexts[0].Lifecycle
	if transition == nil || transition.Reason != sessionstate.LifecycleReasonObservedTerminalClose || !transition.At.Equal(retryAt) {
		t.Fatalf("retry did not persist its archive reason and time: %+v", transition)
	}
	if len(runtime.pendingTerminalClose) != 0 || runtime.terminalCloseRetry != 0 || !runtime.terminalCloseRetryDeadline.IsZero() || !runtime.terminalCloseDeadline.IsZero() {
		t.Fatalf("successful retry retained close work: pending=%+v retry=%v next=%v", runtime.pendingTerminalClose, runtime.terminalCloseRetryDeadline, runtime.terminalCloseDeadline)
	}
	t.Log("250ms compositor deadline preserved persisted active state and candidate; fresh retry durably archived")
}

type terminalCloseDeadlineRequester struct {
	*daemonLoopRequester
	treeRequests int
	deadline     time.Time
	timeout      error
}

func (requester *terminalCloseDeadlineRequester) RequestContext(ctx context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if kind == swayipc.GetTree {
		requester.treeRequests++
		if requester.treeRequests == 1 {
			deadline, ok := ctx.Deadline()
			if !ok {
				return swayipc.Message{}, fmt.Errorf("terminal close observation has no deadline")
			}
			requester.deadline = deadline
			<-ctx.Done()
			requester.timeout = ctx.Err()
			return swayipc.Message{}, requester.timeout
		}
	}
	return requester.daemonLoopRequester.RequestContext(ctx, kind, payload)
}
