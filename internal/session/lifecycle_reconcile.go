package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// LifecycleOutcome describes one observed step, not merely a command acknowledgement.
type LifecycleOutcome struct {
	OperationID string                  `json:"operation_id"`
	Kind        LifecycleOperationKind  `json:"kind"`
	Phase       LifecycleOperationPhase `json:"phase,omitempty"`
	Status      string                  `json:"status"`
	Reason      string                  `json:"reason,omitempty"`
	Effects     bool                    `json:"effects"`
}

type LifecycleReconcileResult struct {
	NextID    string             `json:"next_id,omitempty"`
	Processed int                `json:"processed"`
	Effects   bool               `json:"effects"`
	Outcomes  []LifecycleOutcome `json:"outcomes,omitempty"`
}

// ReconcileLifecycleOperationsContext visits a bounded page, including entries
// in backoff, so one unavailable target cannot starve later operations.
func ReconcileLifecycleOperationsContext(ctx context.Context, root string, client SwayRequestClient, now time.Time, afterID string, limit int) (LifecycleReconcileResult, error) {
	var result LifecycleReconcileResult
	var failures []error
	if ctx == nil {
		return result, errors.New("lifecycle reconciliation context is nil")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if limit < 1 || limit > 32 {
		return result, errors.New("lifecycle reconciliation limit must be between 1 and 32")
	}
	operations, next, err := ListLifecycleOperationsContext(ctx, root, afterID, limit)
	if err != nil {
		return result, err
	}
	result.NextID = afterID
	for _, operation := range operations {
		if err := ctx.Err(); err != nil {
			return result, errors.Join(append(failures, err)...)
		}
		outcome, err := ReconcileLifecycleOperationContext(ctx, root, operation.ID, client, now)
		result.Processed++
		result.NextID = operation.ID
		result.Effects = result.Effects || outcome.Effects
		result.Outcomes = append(result.Outcomes, outcome)
		if err != nil {
			failures = append(failures, err)
		}
	}
	result.NextID = next
	return result, errors.Join(failures...)
}

// The process-shared lifecycle lock spans observation, effects, and their
// durable result. Cancelling and retrying the same operation use this lock too.
func ReconcileLifecycleOperationContext(ctx context.Context, root, id string, client SwayRequestClient, now time.Time) (LifecycleOutcome, error) {
	outcome := LifecycleOutcome{OperationID: id, Status: "pending"}
	entered := false
	err := WithTerminalLifecycleLockContext(ctx, root, func() error {
		return WithLifecycleOperationContext(ctx, root, id, func(handle *LifecycleOperationHandle) error {
			entered = true
			operation := handle.Operation
			outcome.Kind = operation.Kind
			outcome.Phase = operation.Phase
			if operation.Blocked {
				outcome.Status = "conflict"
				outcome.Reason = operation.Reason
				return nil
			}
			if now.Before(operation.NextAttempt) {
				outcome.Status = "retry"
				outcome.Reason = operation.Reason
				return nil
			}
			var complete bool
			var stepErr error
			complete, outcome.Effects, stepErr = reconcileLifecycleApplication(ctx, handle, client)
			if stepErr == nil {
				if complete {
					outcome.Status = "completed"
					return nil
				}
				operation = handle.Operation
				operation.Reason = ""
				operation.NextAttempt = time.Time{}
				operation.UpdatedAt = lifecycleUpdatedAt(operation.UpdatedAt, now)
				return handle.UpdateContext(ctx, operation)
			}
			// A lost final commit acknowledgement may mean that the operation no
			// longer exists. UpdateContext is CAS-only and must never recreate it.
			operation = handle.Operation
			operation.Attempts++
			operation.UpdatedAt = lifecycleUpdatedAt(operation.UpdatedAt, now)
			operation.Blocked = errors.Is(stepErr, ErrLifecycleOperationConflict)
			if operation.Blocked {
				outcome.Status = "conflict"
				operation.Reason = "target_conflict"
				var conflict *lifecycleConflictError
				if errors.As(stepErr, &conflict) {
					operation.Reason = conflict.reason
				}
				operation.NextAttempt = time.Time{}
			} else {
				outcome.Status = "retry"
				operation.Reason = "observation_or_effect_failed"
				delay := time.Second << min(operation.Attempts, 6)
				operation.NextAttempt = now.Add(delay).UTC()
			}
			outcome.Reason = operation.Reason
			if err := handle.UpdateContext(ctx, operation); err != nil {
				return errors.Join(stepErr, err)
			}
			return stepErr
		})
	})
	if !entered && errors.Is(err, os.ErrNotExist) {
		outcome.Status = "completed"
		return outcome, nil
	}
	return outcome, err
}

func RetryLifecycleOperationContext(ctx context.Context, root, id string, now time.Time) error {
	return changeLifecycleOperation(ctx, root, id, now, false)
}

func CancelLifecycleOperationContext(ctx context.Context, root, id string, now time.Time) error {
	return changeLifecycleOperation(ctx, root, id, now, true)
}

func changeLifecycleOperation(ctx context.Context, root, id string, now time.Time, cancel bool) error {
	return WithTerminalLifecycleLockContext(ctx, root, func() error {
		return WithLifecycleOperationContext(ctx, root, id, func(handle *LifecycleOperationHandle) error {
			operation := handle.Operation
			if cancel {
				operation.Phase = LifecycleRollback
			}
			operation.Blocked = false
			operation.Reason = ""
			operation.NextAttempt = time.Time{}
			operation.UpdatedAt = lifecycleUpdatedAt(operation.UpdatedAt, now)
			return handle.UpdateContext(ctx, operation)
		})
	})
}

func lifecycleConflict(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrLifecycleOperationConflict, fmt.Sprintf(format, args...))
}

func lifecycleUpdatedAt(previous, now time.Time) time.Time {
	if now.Before(previous) {
		return previous
	}
	return now.UTC()
}

type lifecycleConflictError struct{ reason, detail string }

func (err *lifecycleConflictError) Error() string {
	return fmt.Sprintf("%v: %s", ErrLifecycleOperationConflict, err.detail)
}
func (err *lifecycleConflictError) Unwrap() error { return ErrLifecycleOperationConflict }
func lifecycleConflictReason(reason, detail string) error {
	return &lifecycleConflictError{reason: reason, detail: detail}
}
