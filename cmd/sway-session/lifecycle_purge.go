package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/marang/sway-session/internal/diagnostic"
	sessionstate "github.com/marang/sway-session/internal/session"
)

// Resolve the executable only when a recorded target still needs a native
// command. The journal's root remains authoritative across configuration changes.
func nativePurgeDeleter(resolve func(string) (string, error), runner sessionstate.HerdrCommandRunner) sessionstate.LifecycleSessionDeleter {
	return func(ctx context.Context, before sessionstate.Context, target sessionstate.LifecyclePurgeTarget) error {
		if resolve == nil || runner == nil {
			return errors.New("herdr purge dependency is unavailable")
		}
		executable, err := resolve("herdr")
		if err != nil {
			return fmt.Errorf("find Herdr executable for pending purge: %w", err)
		}
		manager := sessionstate.HerdrManager{Executable: executable, Root: target.Root, Runner: runner}
		return manager.DeletePurgeTarget(ctx, before, target)
	}
}

func finishPurgeCommand(ctx context.Context, root string, target sessionstate.Context, id string, now time.Time, deps dependencies) (commandResult, *commandFailure) {
	result := commandResult{
		Command: "purge", Contexts: []sessionstate.Context{target}, Actions: []string{"purge_pending"},
		Message:         "Purge is pending; automatic restore is disabled. Inspect sway-session state operations.",
		StateOperations: &stateOperationsResult{Operations: []lifecycleOperationSummary{}},
	}
	deleter := nativePurgeDeleter(deps.resolveProgram, deps.herdrRunner)
	// Normal stop, delete and final absence observation fit in these passes.
	// Refusals, conflicts and interrupted commands retain their durable intent.
	for range 4 {
		outcome, err := sessionstate.ReconcileLifecycleOperationWithPurgeContext(ctx, root, id, nil, deleter, now)
		// Cancellation or storage failure may precede loading the journal row.
		// The command already knows which irreversible intent it recorded.
		outcome.Kind = sessionstate.LifecyclePurge
		outcome.Phase = sessionstate.LifecycleForward
		result.StateOperations.Outcome = &outcome
		if outcome.Status == "completed" && err == nil {
			result.Actions = []string{"purged"}
			result.Message = "Context and Herdr session purged."
			return result, nil
		}
		if err != nil || outcome.Status == "conflict" || outcome.Status == "retry" {
			if outcome.Status == "conflict" || outcome.Status == "retry" {
				detail := lifecycleOperationDiagnostic{outcome: outcome}.Diagnostic()
				if err != nil {
					detail.Message += ": " + err.Error()
				}
				return result, failures([]diagnostic.Diagnostic{detail})
			}
			return result, stateCommandFailure("lifecycle_operation", "continue pending purge "+id, err)
		}
	}
	return result, nil
}
