package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"time"

	"github.com/marang/sway-session/internal/diagnostic"
	sessionstate "github.com/marang/sway-session/internal/session"
)

type lifecycleOperationSummary struct {
	OperationID string                               `json:"operation_id"`
	Kind        sessionstate.LifecycleOperationKind  `json:"kind"`
	Phase       sessionstate.LifecycleOperationPhase `json:"phase"`
	Status      string                               `json:"status"`
	ContextIDs  []sessionstate.ContextID             `json:"context_ids"`
	Reason      string                               `json:"reason,omitempty"`
	Attempts    int                                  `json:"attempts"`
	NextAttempt time.Time                            `json:"next_attempt"`
}

type stateOperationsResult struct {
	Operations []lifecycleOperationSummary    `json:"operations"`
	NextID     string                         `json:"next_id,omitempty"`
	Outcome    *sessionstate.LifecycleOutcome `json:"outcome,omitempty"`
}

func executeStateOperations(ctx context.Context, arguments []string, deps dependencies) (commandResult, *commandFailure) {
	flags := newFlagSet("state operations")
	retry := flags.String("retry", "", "retry one exact operation UUID")
	cancel := flags.String("cancel", "", "request rollback of one application operation UUID")
	after := flags.String("after", "", "continue the read-only operation listing after a UUID")
	socket := flags.String("socket", "", "Sway IPC socket for an application retry or rollback")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return commandResult{}, usageFailure("state", "state operations accepts --after UUID or --retry UUID or --cancel UUID, with optional --socket PATH for retry/cancel")
	}
	empty := false
	flags.Visit(func(option *flag.Flag) { empty = empty || option.Value.String() == "" })
	if empty {
		return commandResult{}, usageFailure("state", "operation options require nonempty values")
	}
	for _, id := range []string{*retry, *cancel, *after} {
		if id != "" && sessionstate.ContextID(id).Validate() != nil {
			return commandResult{}, usageFailure("state", "operation selectors must be exact UUIDs")
		}
	}
	if (*retry != "" && *cancel != "") || (*after != "" && (*retry != "" || *cancel != "")) {
		return commandResult{}, usageFailure("state", "--retry, --cancel, and --after are mutually exclusive")
	}
	if *socket != "" && (*retry == "" && *cancel == "" || !filepath.IsAbs(*socket) || filepath.Clean(*socket) != *socket) {
		return commandResult{}, usageFailure("state", "--socket requires --retry or --cancel and a clean absolute path")
	}
	if deps.stateRoot == nil {
		return commandResult{}, failure("state_path", "resolve state directory", "State directory dependency is unavailable.")
	}
	root, problem := stateRoot(deps)
	if problem != nil {
		return commandResult{}, problem
	}
	now := time.Now().UTC()
	if deps.now != nil {
		now = deps.now().UTC()
	}
	result := commandResult{Command: "state operations", StateOperations: &stateOperationsResult{Operations: []lifecycleOperationSummary{}}}
	if *retry == "" && *cancel == "" {
		if deps.listLifecycleOperations == nil {
			return commandResult{}, failure("lifecycle_operations", "list lifecycle operations", "Operation listing dependency is unavailable.")
		}
		listed, err := deps.listLifecycleOperations(ctx, root, *after, lifecyclePassLimit, now)
		if err != nil {
			return commandResult{}, stateCommandFailure("lifecycle_operations", "list lifecycle operations", err)
		}
		result.StateOperations = &listed
		return result, nil
	}
	if deps.runLifecycleOperation == nil {
		return commandResult{}, failure("lifecycle_operation", "resume lifecycle operation", "Operation reconciliation dependency is unavailable.")
	}
	id := *retry
	if *cancel != "" {
		id = *cancel
	}
	outcome, err := deps.runLifecycleOperation(ctx, root, id, *cancel != "", *socket, now, deps)
	result.StateOperations.Outcome = &outcome
	if err != nil {
		result.Message = "Lifecycle operation did not complete; inspect its status before retrying."
		problem := stateCommandFailure("lifecycle_operation", "resume lifecycle operation "+id, err)
		if outcome.Status == "conflict" || outcome.Status == "retry" {
			problem = failures([]diagnostic.Diagnostic{lifecycleOperationDiagnostic{outcome: outcome}.Diagnostic()})
		}
		return result, problem
	}
	if outcome.Status != "completed" {
		result.Message = "The bounded pass finished; the durable operation remains pending."
	}
	return result, nil
}

func listLifecycleOperationSummaries(ctx context.Context, root, after string, limit int, now time.Time) (stateOperationsResult, error) {
	operations, next, err := sessionstate.ListLifecycleOperationsContext(ctx, root, after, limit)
	if err != nil {
		return stateOperationsResult{}, err
	}
	result := stateOperationsResult{Operations: make([]lifecycleOperationSummary, 0, len(operations)), NextID: next}
	for _, operation := range operations {
		status := "pending"
		switch {
		case operation.Blocked:
			status = "conflict"
		case now.Before(operation.NextAttempt):
			status = "retry"
		case operation.Phase == sessionstate.LifecycleRollback:
			status = "rollback"
		}
		ids := make([]sessionstate.ContextID, 0, len(operation.Before)+len(operation.After))
		for _, items := range [][]sessionstate.Context{operation.Before, operation.After} {
			for _, item := range items {
				if !slices.Contains(ids, item.ID) {
					ids = append(ids, item.ID)
				}
			}
		}
		slices.Sort(ids)
		result.Operations = append(result.Operations, lifecycleOperationSummary{
			OperationID: operation.ID, Kind: operation.Kind, Phase: operation.Phase, Status: status,
			ContextIDs: ids, Reason: operation.Reason, Attempts: operation.Attempts, NextAttempt: operation.NextAttempt,
		})
	}
	return result, nil
}

func writeStateOperations(writer io.Writer, result commandResult) error {
	value := result.StateOperations
	if result.Message != "" {
		if _, err := fmt.Fprintln(writer, result.Message); err != nil {
			return err
		}
	}
	if value.Outcome != nil {
		outcome := value.Outcome
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", outcome.OperationID, outcome.Kind, outcome.Status, outcome.Reason); err != nil {
			return err
		}
		if outcome.Status != "completed" {
			return writeLifecycleOperationHint(writer, outcome.OperationID, outcome.Kind)
		}
		return nil
	}
	if len(value.Operations) == 0 {
		_, err := fmt.Fprintln(writer, "No pending lifecycle operations in this page.")
		return err
	}
	for _, operation := range value.Operations {
		if _, err := fmt.Fprintf(writer, "%s\t%s\tphase=%s\tstatus=%s\treason=%s\tattempts=%d\tnext=%s\n", operation.OperationID, operation.Kind, operation.Phase, operation.Status, operation.Reason, operation.Attempts, operation.NextAttempt.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
		if err := writeLifecycleOperationHint(writer, operation.OperationID, operation.Kind); err != nil {
			return err
		}
	}
	if value.NextID != "" {
		_, err := fmt.Fprintf(writer, "Next page: sway-session state operations --after %s\n", value.NextID)
		return err
	}
	return nil
}

func writeLifecycleOperationHint(writer io.Writer, id string, kind sessionstate.LifecycleOperationKind) error {
	if _, err := fmt.Fprintf(writer, "Retry: sway-session state operations --retry %s\n", id); err != nil {
		return err
	}
	if kind == sessionstate.LifecyclePurge {
		_, err := fmt.Fprintln(writer, "Purge is irreversible; automatic restore remains disabled while cleanup is pending.")
		return err
	}
	_, err := fmt.Fprintf(writer, "Request rollback: sway-session state operations --cancel %s\n", id)
	return err
}
