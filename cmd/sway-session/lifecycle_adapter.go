package main

import (
	"context"
	"fmt"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

// Pass the original transport through: the core pins a fresh connection for
// each application operation, independently of normal daemon reconnection.
type lifecycleCoreAdapter struct {
	root    string
	client  sessionstate.SwayRequestClient
	deleter sessionstate.LifecycleSessionDeleter
}

func (adapter lifecycleCoreAdapter) Reconcile(ctx context.Context, now time.Time, after string, limit int) (sessionstate.LifecycleReconcileResult, error) {
	return sessionstate.ReconcileLifecycleOperationsWithPurgeContext(ctx, adapter.root, adapter.client, adapter.deleter, now, after, limit)
}

func (adapter lifecycleCoreAdapter) BlockedContextIDs(ctx context.Context) ([]sessionstate.ContextID, error) {
	blocked, err := sessionstate.BlockedLifecycleContextIDsContext(ctx, adapter.root)
	if err != nil {
		return nil, err
	}
	ids := make([]sessionstate.ContextID, 0, len(blocked))
	for id := range blocked {
		ids = append(ids, id)
	}
	return ids, nil
}

func runLifecycleOperationAction(ctx context.Context, root, id string, cancel bool, socket string, now time.Time, deps dependencies) (sessionstate.LifecycleOutcome, error) {
	outcome := sessionstate.LifecycleOutcome{OperationID: id, Status: "pending"}
	operation, err := sessionstate.LoadLifecycleOperationContext(ctx, root, id)
	if err != nil {
		return outcome, err
	}
	outcome.Kind = operation.Kind
	if cancel {
		err = sessionstate.CancelLifecycleOperationContext(ctx, root, id, now)
	} else {
		err = sessionstate.RetryLifecycleOperationContext(ctx, root, id, now)
	}
	if err != nil {
		return outcome, err
	}
	if cancel {
		outcome.Status = "rollback"
	}
	if operation.Kind == sessionstate.LifecyclePurge {
		outcome, err = sessionstate.ReconcileLifecycleOperationWithPurgeContext(ctx, root, id, nil, nativePurgeDeleter(deps.resolveProgram, deps.herdrRunner), now)
		outcome.Kind = operation.Kind
		outcome.Phase = operation.Phase
		return outcome, err
	}
	var problem *commandFailure
	socket, problem = applicationSocket(socket)
	if problem != nil {
		return outcome, fmt.Errorf("operation %s remains pending: a valid Sway socket is required; retry with --socket PATH", id)
	}
	if deps.newSwayClient == nil {
		return outcome, fmt.Errorf("operation %s remains pending: Sway client dependency is unavailable", id)
	}
	client := deps.newSwayClient(socket)
	if client == nil {
		return outcome, fmt.Errorf("operation %s remains pending: Sway client dependency returned no transport; retry when the client is available", id)
	}
	defer client.Close()
	return sessionstate.ReconcileLifecycleOperationContext(ctx, root, id, client, now)
}
