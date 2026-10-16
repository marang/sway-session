package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/statefile"
	"github.com/marang/sway-session/internal/swayipc"
)

type reachInvocation struct {
	ctx     context.Context
	request reachRequest
	reply   chan reachReply
}

type reachReply struct {
	result sessionstate.ReachResult
	err    error
}

// One daemon actor owns explicit focus authority for its compositor. A new
// request cancels older queued work before it can cross an external boundary.
// No active environment or pending focus is persisted/replayed after restart.
type reachDispatcher struct {
	compositorID string
	inbox        chan reachInvocation
	mu           sync.Mutex
	stop         context.CancelFunc
	revocations  uint64
	shutdown     bool // daemon actor only
}

func newReachDispatcher(compositorID string) *reachDispatcher {
	return &reachDispatcher{compositorID: compositorID, inbox: make(chan reachInvocation, 8)}
}

func (dispatcher *reachDispatcher) call(ctx context.Context, request reachRequest) (sessionstate.ReachResult, error) {
	if err := request.validate(); err != nil {
		return sessionstate.ReachResult{}, err
	}
	if request.CompositorID != dispatcher.compositorID {
		return sessionstate.ReachResult{}, errors.New("reach belongs to another Sway session")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	dispatcher.mu.Lock()
	if dispatcher.stop != nil {
		dispatcher.stop()
	}
	dispatcher.stop = cancel
	dispatcher.mu.Unlock()
	invocation := reachInvocation{ctx: ctx, request: request, reply: make(chan reachReply, 1)}
	select {
	case dispatcher.inbox <- invocation:
	case <-ctx.Done():
		return sessionstate.ReachResult{}, ctx.Err()
	}
	select {
	case reply := <-invocation.reply:
		return reply.result, reply.err
	case <-ctx.Done():
		return sessionstate.ReachResult{}, ctx.Err()
	}
}

func (dispatcher *reachDispatcher) cancel() {
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	dispatcher.revocations++
	if dispatcher.stop != nil {
		dispatcher.stop()
		dispatcher.stop = nil
	}
}

func (dispatcher *reachDispatcher) revocationGeneration() uint64 {
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	return dispatcher.revocations
}

func (runtime *sessionRuntime) reachInbox() <-chan reachInvocation {
	if runtime.reach == nil {
		return nil
	}
	return runtime.reach.inbox
}

// Drain already queued evidence before revalidation, retaining the daemon's
// ordinary lifecycle bookkeeping. Focus/binding/disconnect after the initial
// observation invalidates the request even if the user later focuses back.
func (runtime *sessionRuntime) drainReachEvents(ctx context.Context, events <-chan swayipc.Event) bool {
	changed := false
	for range 64 {
		if ctx.Err() != nil {
			return true
		}
		select {
		case event, open := <-events:
			if !open {
				return true
			}
			if event.Type == swayipc.EventShutdown {
				runtime.reach.shutdown = true
				changed = true
			}
			generation := runtime.reach.revocationGeneration()
			runtime.HandleEvent(event, runtime.now())
			// HandleEvent distinguishes attributed command feedback from user
			// intent. Seeing a focus tuple alone must not revoke a newer Reach.
			changed = changed || runtime.reach.revocationGeneration() != generation
		default:
			return changed
		}
	}
	// A busy event source is uncertain evidence, not permission to starve the
	// actor or continue after cancellation. A later request can observe again.
	return true
}

func (runtime *sessionRuntime) executeReach(ctx context.Context, request reachRequest, events <-chan swayipc.Event) (sessionstate.ReachResult, error) {
	ctx, cancel := context.WithTimeout(ctx, terminalObservationTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return sessionstate.ReachResult{}, err
	}
	if runtime.reach == nil || request.CompositorID != runtime.reach.compositorID {
		return sessionstate.ReachResult{}, errors.New("reach compositor changed")
	}
	if runtime.eventStreamState == nil || !runtime.eventStreamReady || runtime.eventStreamEpoch == 0 {
		return sessionstate.ReachResult{}, errors.New("sway event observation is unavailable; wait before reaching work")
	}
	access, err := sessionstate.AcquireStateAccess(ctx, runtime.root, false)
	if err != nil {
		return sessionstate.ReachResult{}, err
	}
	defer access.Close()
	if runtime.drainReachEvents(ctx, events) {
		return sessionstate.ReachResult{}, errors.New("focus changed while Reach was queued; select Reach again when ready")
	}
	if runtime.reach.shutdown {
		return sessionstate.ReachResult{}, errors.New("sway is shutting down")
	}
	if err := runtime.requireCurrentEventStream(); err != nil {
		return sessionstate.ReachResult{}, err
	}
	snapshot, err := sessionstate.ReadReachEnvironmentSnapshotContext(ctx, runtime.root, request.EnvironmentID)
	if err != nil {
		return sessionstate.ReachResult{}, err
	}
	environment := snapshot.Environments[0]
	initial, err := requestTree(ctx, runtime.client)
	if err != nil {
		return sessionstate.ReachResult{}, err
	}
	presence, err := sessionstate.ObserveReachPresence(initial, snapshot.Inventory.Registry)
	if err != nil {
		return sessionstate.ReachResult{}, err
	}
	preview, err := sessionstate.PlanWorkEnvironmentReach(ctx, environment, snapshot.Inventory.Registry, presence, request.Primary)
	if err != nil {
		return sessionstate.ReachResult{}, err
	}
	result := sessionstate.ReachResult{EnvironmentID: environment.ID, Primary: preview.Primary, Members: preview.Members}
	if !preview.Ready {
		return result, nil
	}
	var primaryIndex int
	for index, member := range result.Members {
		if member.Primary {
			primaryIndex = index
			break
		}
	}
	finish := func(status sessionstate.ReachStatus, reason string) {
		result.Members[primaryIndex].Status = status
		result.Members[primaryIndex].Reason = reason
	}
	initialFocus := focusedContainerID(initial)
	anchor := presence[preview.Primary].ContainerID
	identity := sessionstate.Context{}
	for _, item := range snapshot.Inventory.Registry.Contexts {
		if item.ID == preview.Primary {
			identity = item
			break
		}
	}
	err = sessionstate.WithRegistryLockContext(ctx, runtime.root, func(*statefile.LockedPrivateDirectory) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if runtime.drainReachEvents(ctx, events) {
			finish(sessionstate.ReachCanceled, "Focus changed; select Reach again when ready.")
			return nil
		}
		if err := runtime.requireCurrentEventStream(); err != nil {
			return err
		}
		current, err := sessionstate.ReadReachEnvironmentSnapshotContext(ctx, runtime.root, environment.ID)
		if err != nil {
			return err
		}
		if current.Environments[0].Revision != environment.Revision {
			finish(sessionstate.ReachCanceled, "Environment changed; inspect the current definition before reaching it.")
			return nil
		}
		tree, err := requestTree(ctx, runtime.client)
		if err != nil {
			return err
		}
		fresh, err := sessionstate.ObserveReachPresence(tree, current.Inventory.Registry)
		if err != nil {
			return err
		}
		plan, err := sessionstate.PlanWorkEnvironmentReach(ctx, current.Environments[0], current.Inventory.Registry, fresh, request.Primary)
		if err != nil {
			return err
		}
		if !plan.Ready || fresh[preview.Primary].ContainerID != anchor {
			finish(sessionstate.ReachUncertain, "Primary identity, presence or policy changed; refresh before trying again.")
			return nil
		}
		for _, item := range current.Inventory.Registry.Contexts {
			if item.ID == preview.Primary && sessionstate.RestoreContextDigest(item) != sessionstate.RestoreContextDigest(identity) {
				finish(sessionstate.ReachCanceled, "Primary identity changed; refresh before trying again.")
				return nil
			}
		}
		if focusedContainerID(tree) != initialFocus || runtime.drainReachEvents(ctx, events) {
			finish(sessionstate.ReachCanceled, "Focus changed; no focus command was sent.")
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// This explicit user navigation supersedes any startup arrangement work.
		runtime.cancelConflictingRestore()
		// Diagnostic cancellation can wait on SQLite; process user intent which
		// arrived during that wait before authorizing the focus command.
		if runtime.drainReachEvents(ctx, events) {
			finish(sessionstate.ReachCanceled, "Focus changed; no focus command was sent.")
			return nil
		}
		if focusedContainerID(tree) == anchor {
			finish(sessionstate.ReachAlreadyPresent, "Primary is already focused; existing windows remain in place.")
			return nil
		}
		action := sessionstate.RestoreAction{Kind: sessionstate.RestoreFocus, ContextID: preview.Primary, ContainerID: anchor}
		command := fmt.Sprintf("[con_id=%d] focus", anchor)
		if err := runtime.runAttributedCommandContext(ctx, tree, action, command, false); err != nil {
			finish(sessionstate.ReachUncertain, "Focus outcome is uncertain; no automatic retry was made.")
			return nil
		}
		confirmed, err := requestTree(ctx, runtime.client)
		if err != nil {
			finish(sessionstate.ReachUncertain, "Focus could not be confirmed; no automatic retry was made.")
			return nil
		}
		confirmedPresence, err := sessionstate.ObserveReachPresence(confirmed, current.Inventory.Registry)
		if err != nil || confirmedPresence[preview.Primary].ContainerID != anchor || focusedContainerID(confirmed) != anchor {
			finish(sessionstate.ReachUncertain, "Focus changed before confirmation; no automatic retry was made.")
			return nil
		}
		finish(sessionstate.ReachFocused, "Focused the existing primary in place; other windows were left alone.")
		return nil
	})
	if err != nil {
		finish(sessionstate.ReachCanceled, "Reach stopped before focus was confirmed; refresh before trying again.")
	}
	return result, err
}
