package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/marang/sway-session/internal/diagnostic"
	sessionstate "github.com/marang/sway-session/internal/session"
)

const cliRestoreHistoryTimeout = 250 * time.Millisecond

type cliRestoreReporter struct {
	ctx     context.Context
	root    string
	records map[sessionstate.ContextID]sessionstate.RestoreOutcome
	now     func() time.Time
}

// Begin records intent before any process, compositor or launcher effects.
func beginCLIRestoreReport(ctx context.Context, root, selector string, requireActive bool, deps dependencies) (*cliRestoreReporter, error) {
	var reporter *cliRestoreReporter
	err := sessionstate.WithTerminalLifecycleLockContext(ctx, root, func() error {
		var err error
		reporter, err = beginCLIRestoreReportLocked(ctx, root, selector, requireActive, deps)
		return err
	})
	return reporter, err
}

func beginCLIRestoreReportLocked(ctx context.Context, root, selector string, requireActive bool, deps dependencies) (*cliRestoreReporter, error) {
	registry, err := sessionstate.ReadRegistrySnapshotContext(ctx, root)
	if err != nil {
		return nil, err
	}
	var targets []sessionstate.Context
	if selector != "" {
		index, err := sessionstate.ResolveContext(registry, selector)
		if err != nil {
			return nil, err
		}
		selected := registry.Contexts[index]
		if (requireActive || selected.App != nil) && selected.State != sessionstate.ContextActive {
			return nil, errors.New("selected context is archived")
		}
		if deps.requireRestoreEligibility && !sessionstate.EvaluateRestorePolicy(selected).Eligible {
			return nil, errors.New("selected context is not eligible for restore")
		}
		targets = []sessionstate.Context{selected}
	} else {
		targets, err = restoreTargets(registry, "", requireActive)
		if err != nil {
			return nil, err
		}
	}
	if len(targets) == 0 {
		return nil, nil
	}
	var layout sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(root).LoadIntoContext(ctx, &layout); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	store := sessionstate.RestoreReportStoreFor(root)
	history, err := store.LoadContext(ctx)
	if err != nil {
		return nil, err
	}
	if deps.requireRestoreEligibility && selector != "" {
		previous, exists := latestCLIRestoreOutcomes(history)[targets[0].ID]
		if !exists || (previous.Status != "failed" && previous.Status != "interrupted") {
			return nil, errors.New("latest restore attempt is no longer failed or interrupted")
		}
	}
	now := time.Now
	if deps.now != nil {
		now = deps.now
	}
	reporter := &cliRestoreReporter{ctx: ctx, root: root, records: make(map[sessionstate.ContextID]sessionstate.RestoreOutcome), now: now}
	records := make([]sessionstate.RestoreOutcome, 0, len(targets))
	// One token identifies this invocation across all contexts and restore waves.
	attempt, err := sessionstate.NewContextID()
	if err != nil {
		return nil, err
	}
	at := now().UTC()
	selectedIDs := make(map[sessionstate.ContextID]bool, len(targets))
	for _, target := range targets {
		selectedIDs[target.ID] = true
	}
	for _, previous := range history.Outcomes {
		if previous.Source == "explicit" && selectedIDs[previous.ContextID] && !at.After(previous.StartedAt) {
			at = previous.StartedAt.Add(time.Nanosecond)
		}
	}
	for _, target := range targets {
		record := sessionstate.RestoreOutcome{ContextID: target.ID, AttemptID: string(attempt), Source: "explicit", StartedAt: at, UpdatedAt: at, Status: "pending", Reason: "restore_requested", Requested: restoreRequestedWork(layout, target.ID), IdentityDigest: sessionstate.RestoreContextDigest(target)}
		reporter.records[target.ID] = record
		records = append(records, record)
	}
	if err := store.BeginExplicitContext(ctx, records); err != nil {
		return nil, err
	}
	return reporter, nil
}

func (reporter *cliRestoreReporter) launchAccepted(ctx context.Context, id sessionstate.ContextID) error {
	if reporter == nil {
		return nil
	}
	record, ok := reporter.records[id]
	if !ok {
		return nil
	}
	record.LaunchAccepted = true
	reporter.records[id] = record
	store := sessionstate.RestoreReportStoreFor(reporter.root)
	history, err := store.LoadContext(ctx)
	if err != nil {
		return err
	}
	pending := false
	for _, current := range history.Outcomes {
		if current.Source == "explicit" && current.ContextID == id && current.AttemptID == record.AttemptID && current.Status == "pending" {
			pending = true
		}
	}
	if !pending {
		return nil
	}
	_, err = store.UpdateContext(ctx, "explicit", id, record.AttemptID, sessionstate.RestoreOutcomeUpdate{LaunchAccepted: true, Status: "pending", Reason: "launch_accepted", UpdatedAt: reporter.updateTime(record)})
	return err
}

func (reporter *cliRestoreReporter) updateTime(record sessionstate.RestoreOutcome) time.Time {
	at := reporter.now().UTC()
	if at.Before(record.StartedAt) {
		return record.StartedAt
	}
	return at
}

func (reporter *cliRestoreReporter) finish(result commandResult, commandFailure *commandFailure, ctxErr error) error {
	if reporter == nil {
		return nil
	}
	// An expired operation context must not prevent recording its interruption.
	ctx := reporter.ctx
	if ctxErr != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), cliRestoreHistoryTimeout)
		defer cancel()
	}
	mapped := make(map[sessionstate.ContextID]bool)
	identityChanged := make(map[sessionstate.ContextID]bool)
	for _, value := range result.Contexts {
		record, exists := reporter.records[value.ID]
		if !exists {
			continue
		}
		if record.IdentityDigest != sessionstate.RestoreContextDigest(value) {
			identityChanged[value.ID] = true
			continue
		}
		if value.App == nil && value.Launcher.Kind == sessionstate.LauncherHerdr {
			mapped[value.ID] = true
		}
	}
	failures := make(map[sessionstate.ContextID]string)
	unknownLaunch := make(map[sessionstate.ContextID]bool)
	globalReason := ""
	historyUnavailable := false
	if commandFailure != nil {
		for _, item := range commandFailure.diagnostics {
			if item.Code == "restore_history" {
				historyUnavailable = true
				continue
			}
			reason := cliRestoreFailureReason(item.Code)
			id := sessionstate.ContextID(fmt.Sprint(item.Details["context_id"]))
			if _, exists := reporter.records[id]; exists {
				if item.Code == "launch_outcome_unknown" {
					unknownLaunch[id] = true
				}
				failures[id] = reason
			} else if item.Details["context_id"] == nil {
				globalReason = reason
			}
		}
	}
	for id := range unknownLaunch {
		failures[id] = "observation_unavailable"
	}
	history, err := sessionstate.RestoreReportStoreFor(reporter.root).LoadContext(ctx)
	if err != nil {
		return err
	}
	pending := make(map[sessionstate.ContextID]string)
	for _, current := range history.Outcomes {
		if current.Source == "explicit" && current.Status == "pending" {
			pending[current.ContextID] = current.AttemptID
		}
	}
	var joined error
	for id, record := range reporter.records {
		if pending[id] != record.AttemptID {
			continue
		}
		update := sessionstate.RestoreOutcomeUpdate{UpdatedAt: reporter.updateTime(record), Status: "pending", Reason: "awaiting_daemon", WindowMapped: mapped[id], LaunchAccepted: record.LaunchAccepted}
		if identityChanged[id] {
			update.Status, update.Reason = "failed", "identity_changed"
		} else if mapped[id] && !historyUnavailable && !record.Requested.Placement && !record.Requested.Layout {
			update.Status, update.Reason = "completed", "restore_complete"
		} else if ctxErr != nil {
			update.Status, update.Reason = "interrupted", "interrupted"
		} else if reason := failures[id]; reason != "" {
			update.Status, update.Reason = "failed", reason
		} else if !mapped[id] && globalReason != "" {
			update.Status, update.Reason = "failed", globalReason
		} else if historyUnavailable {
			update.Reason = "history_unavailable"
		}
		_, err := sessionstate.RestoreReportStoreFor(reporter.root).UpdateContext(ctx, "explicit", id, record.AttemptID, update)
		joined = errors.Join(joined, err)
	}
	return joined
}

func cliRestoreFailureReason(code string) string {
	switch code {
	case "mapping_timeout":
		return "mapping_timeout"
	case "restore_deferred":
		return "restore_deferred"
	case "duplicate_window", "duplicate_launch":
		return "ambiguous_window"
	case "launch_outcome_unknown", "mapping_unstable", "sway_tree", "sway_socket", "process_observation":
		return "observation_unavailable"
	default:
		return "launch_failed"
	}
}

func appendRestoreHistoryFailure(original *commandFailure, err error) *commandFailure {
	if err == nil {
		return original
	}
	item := diagnostic.Diagnostic{Level: diagnostic.LevelError, Code: "restore_history", Message: "restore outcome history could not be recorded", Hint: "Inspect the current window before retrying; restore effects may already have occurred."}
	if original == nil {
		return &commandFailure{diagnostics: []diagnostic.Diagnostic{item}}
	}
	copied := *original
	copied.diagnostics = append(append([]diagnostic.Diagnostic(nil), original.diagnostics...), item)
	return &copied
}

type restoreReportCommandItem struct {
	sessionstate.RestoreOutcome
	Label string `json:"label"`
}

type restoreReportTotals struct {
	Pending     int `json:"pending"`
	Completed   int `json:"completed"`
	Skipped     int `json:"skipped"`
	Failed      int `json:"failed"`
	Interrupted int `json:"interrupted"`
}

type restoreReportCommandResult struct {
	sessionstate.RestoreReport
	Outcomes []restoreReportCommandItem `json:"outcomes"`
	Totals   restoreReportTotals        `json:"totals"`
}

func executeRestoreReport(ctx context.Context, arguments []string, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("restore-report")
	retry := set.String("retry", "", "retry one failed or interrupted context by exact UUID")
	socket := set.String("socket", "", "Sway IPC socket")
	if err := set.Parse(arguments); err != nil || set.NArg() != 0 {
		return commandResult{}, usageFailure("restore-report", "restore-report accepts --retry UUID and --socket PATH")
	}
	var restoreFailure *commandFailure
	if *retry != "" {
		retried, retryFailure := executeCLIRestoreRetry(ctx, sessionstate.ContextID(*retry), *socket, deps)
		restoreFailure = retryFailure
		if restoreFailure != nil {
			return retried, restoreFailure
		}
	}
	root, failure := stateRoot(deps)
	if failure != nil {
		return commandResult{}, failure
	}
	report, err := sessionstate.RestoreReportStoreFor(root).LoadContext(ctx)
	if err != nil {
		return commandResult{}, classifyStateError("read restore report", err)
	}
	registry := sessionstate.Registry{}
	if len(report.Outcomes) != 0 {
		registry, err = sessionstate.ReadRegistrySnapshotContext(ctx, root)
		if err != nil {
			return commandResult{}, classifyStateError("read restore labels", err)
		}
	}
	labels := make(map[sessionstate.ContextID]string)
	for _, item := range registry.Contexts {
		labels[item.ID] = item.Label
	}
	output := &restoreReportCommandResult{RestoreReport: report, Outcomes: []restoreReportCommandItem{}}
	for _, record := range report.Outcomes {
		output.Outcomes = append(output.Outcomes, restoreReportCommandItem{RestoreOutcome: record, Label: labels[record.ContextID]})
	}
	for _, record := range output.Outcomes {
		switch record.Status {
		case "pending":
			output.Totals.Pending++
		case "completed":
			output.Totals.Completed++
		case "skipped":
			output.Totals.Skipped++
		case "failed":
			output.Totals.Failed++
		case "interrupted":
			output.Totals.Interrupted++
		}
	}
	sortCLIRestoreRecords(output.Outcomes)
	return commandResult{Command: "restore-report", RestoreReport: output, Contexts: []sessionstate.Context{}}, nil
}

func writeRestoreReport(writer io.Writer, report restoreReportCommandResult) error {
	if report.AutomaticRunID != "" {
		if _, err := fmt.Fprintf(writer, "Automatic restore run: %s\n", report.AutomaticRunID); err != nil {
			return err
		}
	}
	if report.InterruptedRunID != "" {
		at := "unknown time"
		if report.InterruptedAt != nil {
			at = report.InterruptedAt.Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(writer, "Previous automatic run interrupted: %s · %s\n", report.InterruptedRunID, at); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(writer, "Restore totals: %d pending · %d completed · %d skipped · %d failed · %d interrupted\n", report.Totals.Pending, report.Totals.Completed, report.Totals.Skipped, report.Totals.Failed, report.Totals.Interrupted); err != nil {
		return err
	}
	if len(report.Outcomes) == 0 {
		message := "No restore history recorded."
		if report.AutomaticRunID != "" || report.InterruptedRunID != "" {
			message = "No context outcomes recorded."
		}
		_, err := fmt.Fprintln(writer, message)
		return err
	}
	for _, item := range report.Outcomes {
		label := item.Label
		if label == "" {
			label = string(item.ContextID)
		}
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s · %s\t%s\t%s\n", label, item.ContextID, item.Source, item.Status, terminalManageRestoreStage(terminalInventoryResult{RestoreOutcome: &item.RestoreOutcome}), terminalManageRestoreReason(item.Reason), item.UpdatedAt.Format(time.RFC3339)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(writer, "  Attempt %s · requested: window=%t placement=%t layout=%t · observed: launch=%t window=%t placement=%t layout=%t\n", item.AttemptID, item.Requested.Window, item.Requested.Placement, item.Requested.Layout, item.LaunchAccepted, item.WindowMapped, item.PlacementApplied, item.LayoutApplied); err != nil {
			return err
		}
	}
	return nil
}

func latestCLIRestoreOutcomes(report sessionstate.RestoreReport) map[sessionstate.ContextID]sessionstate.RestoreOutcome {
	latest := make(map[sessionstate.ContextID]sessionstate.RestoreOutcome)
	for _, record := range report.Outcomes {
		previous, exists := latest[record.ContextID]
		if !exists || record.StartedAt.After(previous.StartedAt) || (record.StartedAt.Equal(previous.StartedAt) && record.Source == "explicit") {
			latest[record.ContextID] = record
		}
	}
	return latest
}

func decorateTerminalRestoreHistory(ctx context.Context, deps dependencies, items []terminalInventoryResult) *commandFailure {
	root, failure := stateRoot(deps)
	if failure != nil {
		return failure
	}
	report, err := sessionstate.RestoreReportStoreFor(root).LoadContext(ctx)
	if err != nil {
		return classifyStateError("read terminal restore history", err)
	}
	latest := latestCLIRestoreOutcomes(report)
	for index := range items {
		items[index].RestoreOutcome = nil
		if record, exists := latest[items[index].ContextID]; exists {
			items[index].RestoreOutcome = &record
		}
	}
	return nil
}

func executeRestoreRetry(ctx context.Context, id sessionstate.ContextID, socket string, deps dependencies) error {
	_, failure := executeCLIRestoreRetry(ctx, id, socket, deps)
	return terminalManageFailure(failure)
}

func executeCLIRestoreRetry(ctx context.Context, id sessionstate.ContextID, socket string, deps dependencies) (commandResult, *commandFailure) {
	if err := id.Validate(); err != nil {
		return commandResult{}, usageFailure("restore-report", "retry requires an exact context UUID")
	}
	root, failure := stateRoot(deps)
	if failure != nil {
		return commandResult{}, failure
	}
	registry, err := sessionstate.ReadRegistrySnapshotContext(ctx, root)
	if err != nil {
		return commandResult{}, classifyStateError("read retry policy", err)
	}
	index, err := sessionstate.ResolveContext(registry, string(id))
	if err != nil {
		return commandResult{}, classifyStateError("select restore retry", err)
	}
	selected := registry.Contexts[index]
	if !sessionstate.EvaluateRestorePolicy(selected).Eligible {
		return commandResult{}, failureForRestoreRetry("context is not eligible for automatic restore")
	}
	report, err := sessionstate.RestoreReportStoreFor(root).LoadContext(ctx)
	if err != nil {
		return commandResult{}, classifyStateError("read retry history", err)
	}
	previous, exists := latestCLIRestoreOutcomes(report)[id]
	if !exists || (previous.Status != "failed" && previous.Status != "interrupted") {
		return commandResult{}, failureForRestoreRetry("latest restore attempt is not failed or interrupted")
	}
	deps.requireRestoreEligibility = true
	args := []string{"--require-active"}
	if socket != "" {
		args = append(args, "--socket", socket)
	}
	args = append(args, string(id))
	return executeRestore(ctx, args, deps)
}

func failureForRestoreRetry(message string) *commandFailure {
	return failure("restore_retry", message, "Select an eligible context whose latest restore attempt failed or was interrupted.")
}

// Keep report presentation deterministic even if a future store changes ordering.
func sortCLIRestoreRecords(records []restoreReportCommandItem) {
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Source != records[j].Source {
			return records[i].Source < records[j].Source
		}
		return records[i].ContextID < records[j].ContextID
	})
}
