package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

const restoreReportBatch = 64
const restoreReportTimeout = 30 * time.Second
const restoreReportWriteTimeout = 250 * time.Millisecond

// restoreRequestedWork captures intent, never evidence of an effect. Both the
// daemon and explicit CLI requests use the same workspace encoding.
func restoreRequestedWork(snapshot sessionstate.LayoutSnapshot, id sessionstate.ContextID) sessionstate.RestoreWork {
	work := sessionstate.RestoreWork{Window: true}
	if placement, found := savedScratchpadPlacement(snapshot, id); found {
		work.Placement = true
		work.Scratchpad = &sessionstate.ScratchpadRestoreWork{Visible: placement.Visible, Workspace: placement.Workspace}
		return work
	}
	name, found := snapshotContextWorkspace(snapshot, id)
	if !found {
		return work
	}
	work.Placement, work.Workspace = true, name
	workspace, found := workspaceByName(snapshot, name)
	if found && workspace.RestoreMode == sessionstate.WorkspaceRestoreLayout {
		work.Layout = true
		encoded, err := json.Marshal(workspace)
		if err == nil {
			sum := sha256.Sum256(encoded)
			work.LayoutDigest = hex.EncodeToString(sum[:])
		}
	}
	return work
}

type rejectedApplicationStart struct {
	startedAt      time.Time
	identityDigest string
}

type restoreReportEffect struct {
	identityDigest string
	id             sessionstate.ContextID
	workspace      string
	accepted       bool
	reason         string
	uncertain      bool
	records        []sessionstate.RestoreOutcome
	cursor         int
}

// Effects are drained after reconciliation, outside registry locks and Sway
// calls. They are diagnostics only and never control the restore algorithm.
func (runtime *sessionRuntime) recordRestoreReportEffect(effect restoreReportEffect) {
	if runtime.restoreReportSeedStarted.IsZero() {
		return
	}
	if effect.id != "" {
		if index, exists := runtime.restoreReportSeedIndex[effect.id]; exists && index >= runtime.restoreReportSeedCursor {
			selected := runtime.restoreReportSeedContexts[index]
			if effect.identityDigest == "" || effect.identityDigest == sessionstate.RestoreContextDigest(selected) {
				if runtime.restoreReportUnseededEffects == nil {
					runtime.restoreReportUnseededEffects = make(map[sessionstate.ContextID][]restoreReportEffect)
				}
				runtime.restoreReportUnseededEffects[effect.id] = append(runtime.restoreReportUnseededEffects[effect.id], effect)
			}
		}
	}
	// Keep one cursor per bounded restore operation, rather than expanding a
	// workspace failure into a queued mutation for every registered context.
	if effect.id == "" {
		effect.records = runtime.restoreReportObserved
	} else {
		for _, record := range runtime.restoreReportObserved {
			if effect.id == record.ContextID && (effect.identityDigest == "" || effect.identityDigest == record.IdentityDigest) {
				effect.records = append(effect.records, record)
			}
		}
	}
	if len(effect.records) != 0 {
		runtime.restoreReportEffects = append(runtime.restoreReportEffects, effect)
	}

}

func (runtime *sessionRuntime) seedRestoreReport(registry sessionstate.Registry, now time.Time) (resultErr error) {
	ctx, finish := runtime.restoreReportIOContext()
	defer finish()
	defer func() { resultErr = runtime.restoreReportIOResult(ctx, resultErr) }()
	if runtime.root == "" {
		return nil
	}
	store := sessionstate.RestoreReportStoreFor(runtime.root)
	if runtime.restoreReportSeedStarted.IsZero() {
		runtime.restoreReportSeedStarted = now.UTC()
		runtime.restoreReportSeedContexts = append([]sessionstate.Context(nil), registry.Contexts...)
		runtime.restoreReportSeedIndex = make(map[sessionstate.ContextID]int, len(registry.Contexts))
		for i := range runtime.restoreReportSeedContexts {
			item := &runtime.restoreReportSeedContexts[i]
			runtime.restoreReportSeedIndex[item.ID] = i
			if item.App != nil {
				app := *item.App
				item.App = &app
			}
			if item.Launcher.Terminal != nil {
				terminal := *item.Launcher.Terminal
				if terminal.Identity != nil {
					identity := *terminal.Identity
					terminal.Identity = &identity
				}
				item.Launcher.Terminal = &terminal
			}
		}
		// Persisted snapshots are immutable values in the runtime; retain the
		// first actual reconcile's requested work while rows are appended later.
		runtime.restoreReportSeedSnapshot = runtime.persisted
	}
	if runtime.restoreRunID == "" {
		id, err := sessionstate.NewContextID()
		if err != nil {
			return fmt.Errorf("create restore run ID: %w", err)
		}
		runtime.restoreRunID = string(id)
	}
	if !runtime.restoreReportSeeded {
		if err := store.BeginAutomaticContext(ctx, runtime.restoreRunID, nil); err != nil {
			return fmt.Errorf("begin automatic restore outcomes: %w", err)
		}
		runtime.restoreReportSeeded = true
	}
	if runtime.restoreReportSeedCursor == len(runtime.restoreReportSeedContexts) {
		return nil
	}
	if len(runtime.restoreReportSeedRecords) == 0 {
		end := min(runtime.restoreReportSeedCursor+restoreReportBatch, len(runtime.restoreReportSeedContexts))
		for _, item := range runtime.restoreReportSeedContexts[runtime.restoreReportSeedCursor:end] {
			attempt, err := sessionstate.NewContextID()
			if err != nil {
				return fmt.Errorf("create restore attempt ID: %w", err)
			}
			record := sessionstate.RestoreOutcome{ContextID: item.ID, AttemptID: string(attempt), Source: "automatic", StartedAt: runtime.restoreReportSeedStarted, UpdatedAt: now.UTC(), Status: "pending", Reason: "restore_requested", Requested: restoreRequestedWork(runtime.restoreReportSeedSnapshot, item.ID), OwnerRunID: runtime.restoreRunID, IdentityDigest: sessionstate.RestoreContextDigest(item)}
			policy := sessionstate.EvaluateRestorePolicy(item)
			if !policy.Eligible {
				record.Status, record.Reason, record.Requested = "skipped", "policy_not_desired", sessionstate.RestoreWork{}
				if policy.Reason == "archived" {
					record.Reason = "policy_archived"
				}
			} else if runtime.restoreCancelled {
				record.Status, record.Reason = "interrupted", "user_cancelled"
				if runtime.restoreCancellationReason != "" {
					record.Reason = runtime.restoreCancellationReason
				}
			}
			runtime.restoreReportSeedRecords = append(runtime.restoreReportSeedRecords, record)
		}
	}
	for i := range runtime.restoreReportSeedRecords {
		record := &runtime.restoreReportSeedRecords[i]
		if record.Status != "pending" {
			continue
		}
		for _, effect := range runtime.restoreReportUnseededEffects[record.ContextID] {
			record.LaunchAccepted = record.LaunchAccepted || effect.accepted
			if effect.reason != "" {
				record.Status, record.Reason = "failed", effect.reason
				if effect.uncertain {
					record.Status, record.Reason = "pending", "observation_unavailable"
				}
			}
		}
		if record.Requested.Layout && runtime.restoreFailures[record.Requested.Workspace] != nil {
			record.Status, record.Reason = "failed", "layout_failed"
		}
	}
	matched, err := store.AppendAutomaticContext(ctx, runtime.restoreRunID, runtime.restoreReportSeedRecords)
	if err != nil {
		return fmt.Errorf("append automatic restore outcomes: %w", err)
	}
	if !matched {
		return errors.New("automatic restore run was replaced while seeding")
	}
	for _, record := range runtime.restoreReportSeedRecords {
		delete(runtime.restoreReportUnseededEffects, record.ContextID)
	}
	runtime.restoreReportSeedCursor += len(runtime.restoreReportSeedRecords)
	runtime.restoreReportSeedRecords = nil
	return nil
}

// observeRestoreReport runs only at the start of a pass on its fresh tree.
// Even after startup completes, newly persisted explicit attempts are observed.
type restorePendingOutcomeUpdater func(context.Context, string, sessionstate.ContextID, string, sessionstate.RestoreOutcomeUpdate) (bool, error)

func (runtime *sessionRuntime) observeRestoreReport(root *Node, registry sessionstate.Registry, now time.Time) error {
	return runtime.observeRestoreReportWithUpdater(root, registry, now, sessionstate.RestoreReportStoreFor(runtime.root).UpdatePendingContext)
}

// The updater seam lets tests interleave another writer after the journal read.
// Production uses the pending-only token CAS; a general progress update may
// match a finalized token and must never authorize a new restore operation.
func (runtime *sessionRuntime) observeRestoreReportWithUpdater(root *Node, registry sessionstate.Registry, now time.Time, updatePending restorePendingOutcomeUpdater) (resultErr error) {
	ctx, finish := runtime.restoreReportIOContext()
	defer finish()
	defer func() { resultErr = runtime.restoreReportIOResult(ctx, resultErr) }()
	runtime.restoreReportObserved = nil
	if !runtime.restoreReportSeeded {
		return nil
	}
	store := sessionstate.RestoreReportStoreFor(runtime.root)
	report, err := store.LoadContext(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load restore outcomes: %w", err)
	}
	windows, issues, windowErr := sessionstate.ObserveManagedWindowsIsolated(root, registry)
	groups, groupErr := sessionstate.ObserveApplicationGroups(root, registry)
	if windowErr != nil {
		return fmt.Errorf("observe restore outcome windows: %w", windowErr)
	}
	ambiguous := make(map[sessionstate.ContextID]bool, len(issues))
	for _, issue := range issues {
		ambiguous[issue.ContextID] = true
	}
	current := make(map[sessionstate.ContextID]sessionstate.Context, len(registry.Contexts))
	explicitAttempts := make(map[sessionstate.ContextID]string)
	for _, record := range report.Outcomes {
		if record.Source == "explicit" {
			explicitAttempts[record.ContextID] = record.AttemptID
		}
	}
	for id, token := range runtime.restoreReportLaunchRearmed {
		if explicitAttempts[id] != token {
			delete(runtime.restoreReportLaunchRearmed, id)
		}
	}
	for id, token := range runtime.restoreReportRearmed {
		if explicitAttempts[id] != token {
			delete(runtime.restoreReportRearmed, id)
		}
	}
	for _, item := range registry.Contexts {
		current[item.ID] = item
	}
	runtime.restoreReportObserved = nil
	pending := make([]sessionstate.RestoreOutcome, 0)
	for _, record := range report.Outcomes {
		if record.Status == "pending" && (record.Source != "automatic" || record.OwnerRunID == runtime.restoreRunID) {
			pending = append(pending, record)
		}
	}
	for _, record := range pending {
		item, exists := current[record.ContextID]
		if exists && record.IdentityDigest == sessionstate.RestoreContextDigest(item) {
			runtime.restoreReportObserved = append(runtime.restoreReportObserved, record)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return restoreOutcomeCursor(pending[i]) < restoreOutcomeCursor(pending[j]) })
	start := sort.Search(len(pending), func(i int) bool { return restoreOutcomeCursor(pending[i]) > runtime.restoreReportCursor })
	layoutProof := make(map[string]bool)
	var reportErrors []error
	for offset := 0; offset < min(len(pending), restoreReportBatch); offset++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(reportErrors, err)...)
		}
		record := pending[(start+offset)%len(pending)]
		update := sessionstate.RestoreOutcomeUpdate{UpdatedAt: now.UTC(), OwnerRunID: runtime.restoreRunID, Status: "pending", Reason: record.Reason}
		layoutRetryReady := false
		item, exists := current[record.ContextID]
		if !exists {
			update.Status, update.Reason = "failed", "context_missing"
		} else if record.IdentityDigest == "" || record.IdentityDigest != sessionstate.RestoreContextDigest(item) {
			update.Status, update.Reason = "failed", "identity_changed"
		} else if item.State != sessionstate.ContextActive {
			update.Status, update.Reason = "interrupted", "user_cancelled"
		} else {
			mapped, workspace := false, ""
			if item.App == nil {
				window, found := windows[item.ID]
				mapped, workspace = found && !ambiguous[item.ID], window.Workspace
				if mapped {
					node := findContainerByID(root, window.ContainerID)
					mark, markErr := item.ID.Mark()
					layoutRetryReady = node != nil && markErr == nil && slices.Contains(node.Marks, mark)
				}
			} else if groupErr == nil {
				group := groups[item.ID]
				if group.Anchor != nil && !group.Ambiguous && !ambiguous[item.ID] {
					mapped, workspace = true, group.Anchor.Workspace
					layoutRetryReady = group.AnchorMarked
				}
			}
			update.WindowMapped = mapped
			if mapped {
				update.Reason = "window_mapped"
			}
			update.PlacementApplied = mapped && record.Requested.Placement && workspace == record.Requested.Workspace
			if intent := record.Requested.Scratchpad; intent != nil {
				group := groups[item.ID]
				update.PlacementApplied = mapped && group.Anchor != nil && group.Anchor.Scratchpad &&
					(intent.Visible && workspace == intent.Workspace || !intent.Visible && workspace == "__i3_scratch")
			} else if item.App != nil && groups[item.ID].Anchor != nil && groups[item.ID].Anchor.Scratchpad {
				update.PlacementApplied = false
			}
			if update.PlacementApplied {
				update.Reason = "placement_applied"
			}
			if record.Requested.Layout && update.PlacementApplied {
				desired, found := workspaceByName(runtime.persisted, record.Requested.Workspace)
				requested := restoreRequestedWork(runtime.persisted, item.ID)
				if found && requested.Layout && requested.LayoutDigest == record.Requested.LayoutDigest {
					proof, known := layoutProof[requested.LayoutDigest]
					if !known {
						observed, observeErr := sessionstate.ObserveWorkspaceRestoreComplete(root, registry, desired)
						proof = observeErr == nil && observed
						layoutProof[requested.LayoutDigest] = proof
					}
					update.LayoutApplied = proof
				}
			}
			if (!record.Requested.Window || mapped) && (!record.Requested.Placement || update.PlacementApplied) && (!record.Requested.Layout || update.LayoutApplied) {
				update.Status, update.Reason = "completed", "restore_complete"
			} else if !now.Before(record.StartedAt.Add(restoreReportTimeout)) {
				update.Status = "failed"
				switch {
				case !mapped:
					update.Reason = "mapping_timeout"
				case record.Requested.Placement && !update.PlacementApplied:
					update.Reason = "placement_timeout"
				default:
					update.Reason = "layout_timeout"
				}
			}
		}
		retryIntent := false
		if update.Status == "pending" && record.Source == "explicit" {
			if update.WindowMapped && layoutRetryReady {
				retryIntent = runtime.canRearmExplicitRestoreReport(record, item)
			} else if item.App != nil && groupErr == nil {
				group := groups[item.ID]
				retryIntent = !ambiguous[item.ID] && !group.Ambiguous && group.Anchor == nil && len(group.Windows) == 0 && runtime.canRetryRejectedApplicationReport(record, item)
			}
		}
		changed := (update.WindowMapped && !record.WindowMapped) || (update.PlacementApplied && !record.PlacementApplied) || (update.LayoutApplied && !record.LayoutApplied) || update.Status != record.Status || update.Reason != record.Reason || update.OwnerRunID != record.OwnerRunID
		if !changed && !retryIntent {
			runtime.restoreReportCursor = restoreOutcomeCursor(record)
			continue
		}
		matched, err := updatePending(ctx, record.Source, record.ContextID, record.AttemptID, update)
		if err != nil {
			reportErrors = append(reportErrors, fmt.Errorf("update observed restore outcome: %w", err))
			break
		}
		if matched && update.Status == "pending" && record.Source == "explicit" {
			if update.WindowMapped {
				// A fresh unmarked window must first pass through normal adoption,
				// which records its mapping-focus provenance before eligibility.
				// Report-driven eligibility would otherwise make that adoption look
				// like a user-controlled reopen and cancel startup on automatic focus.
				if layoutRetryReady {
					runtime.rearmExplicitRestoreReport(record, item)
				}
			} else if item.App != nil && groupErr == nil {
				group := groups[item.ID]
				if !ambiguous[item.ID] && !group.Ambiguous && group.Anchor == nil && len(group.Windows) == 0 {
					if err := runtime.retryRejectedApplicationReport(ctx, record, item); err != nil {
						reportErrors = append(reportErrors, err)
					}
				}
			}
		}
		runtime.restoreReportCursor = restoreOutcomeCursor(record)
	}
	// An application matching error cannot prove an anchor. Independent terminal
	// records still progress, and the original diagnostic remains visible.
	return errors.Join(append(reportErrors, groupErr)...)
}

func restoreOutcomeCursor(record sessionstate.RestoreOutcome) string {
	return string(record.Source) + ":" + string(record.ContextID) + ":" + record.AttemptID
}

func (runtime *sessionRuntime) flushRestoreReportEffects(now time.Time) (resultErr error) {
	ctx, finish := runtime.restoreReportIOContext()
	defer finish()
	defer func() { resultErr = runtime.restoreReportIOResult(ctx, resultErr) }()
	if !runtime.restoreReportSeeded || len(runtime.restoreReportEffects) == 0 {
		return nil
	}
	store := sessionstate.RestoreReportStoreFor(runtime.root)
	var errs []error
	for drained := 0; drained < restoreReportBatch && len(runtime.restoreReportEffects) != 0; drained++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		effect := &runtime.restoreReportEffects[0]
		if effect.cursor == len(effect.records) {
			runtime.restoreReportEffects = runtime.restoreReportEffects[1:]
			continue
		}
		record := effect.records[effect.cursor]
		matches := effect.id == record.ContextID || (effect.id == "" && effect.workspace != "" && effect.workspace == record.Requested.Workspace)
		if !matches {
			effect.cursor++
			continue
		}
		update := sessionstate.RestoreOutcomeUpdate{UpdatedAt: now.UTC(), OwnerRunID: runtime.restoreRunID, LaunchAccepted: effect.accepted, Status: "pending", Reason: "launch_accepted"}
		if effect.reason != "" {
			update.Status, update.Reason = "failed", effect.reason
			if effect.uncertain {
				update.Status, update.Reason = "pending", "observation_unavailable"
			}
		}
		if _, err := store.UpdateContext(ctx, record.Source, record.ContextID, record.AttemptID, update); err != nil {
			errs = append(errs, fmt.Errorf("update restore action outcome: %w", err))
			break
		}
		effect.cursor++
	}

	return errors.Join(errs...)
}

func restoreReportCommandUncertain(err error) bool {
	var unknown *swayipc.CommandOutcomeUnknownError
	var invalid *swayipc.CommandResponseInvalidError
	return errors.As(err, &unknown) || errors.As(err, &invalid)
}

func (runtime *sessionRuntime) interruptRestoreReport(reason string) error {
	if !runtime.restoreReportSeeded || runtime.root == "" {
		return nil
	}
	// Shutdown commonly follows cancellation of runtime.ctx. Use a new, short
	// deadline so the diagnostic transition can still commit.
	ctx, cancel := context.WithTimeout(context.Background(), restoreReportWriteTimeout)
	defer cancel()
	return sessionstate.RestoreReportStoreFor(runtime.root).InterruptPendingContext(ctx, runtime.restoreRunID, time.Now().UTC(), reason)
}

// Reconcile shares one diagnostic I/O budget across seeding, observation and
// effect draining. Operational work between these calls does not spend it.
func (runtime *sessionRuntime) restoreReportIOContext() (context.Context, func()) {
	budget := restoreReportWriteTimeout
	if runtime.restoreReportInPass {
		budget = max(runtime.restoreReportBudget, 0)
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(runtime.context(), budget)
	return ctx, func() {
		cancel()
		if runtime.restoreReportInPass {
			runtime.restoreReportBudget -= time.Since(started)
		}
	}
}

// A fresh explicit token is a user request to retry the existing algorithm,
// not a new desired-layout authority. Only the current saved exact layout can
// be rearmed; no report row supplies launch or application policy.
func (runtime *sessionRuntime) canRearmExplicitRestoreReport(record sessionstate.RestoreOutcome, item sessionstate.Context) bool {
	if !record.Requested.Layout || !sessionstate.EvaluateRestorePolicy(item).Eligible || runtime.restoreProgress != nil || runtime.restoreCleanupPending || runtime.restoreReportRearmed[item.ID] == record.AttemptID {
		return false
	}
	desired := restoreRequestedWork(runtime.desired, item.ID)
	persisted := restoreRequestedWork(runtime.persisted, item.ID)
	return desired.Layout && desired == record.Requested && persisted == record.Requested
}

func (runtime *sessionRuntime) rearmExplicitRestoreReport(record sessionstate.RestoreOutcome, item sessionstate.Context) {
	if !runtime.canRearmExplicitRestoreReport(record, item) {
		return
	}
	if runtime.restoreReportRearmed == nil {
		runtime.restoreReportRearmed = make(map[sessionstate.ContextID]string)
	}
	runtime.restoreReportRearmed[item.ID] = record.AttemptID
	if runtime.restoreEligible == nil {
		runtime.restoreEligible = make(map[sessionstate.ContextID]struct{})
	}
	runtime.restoreEligible[item.ID] = struct{}{}
	runtime.lateRestorePending = true
	delete(runtime.restoreExcluded, record.Requested.Workspace)
	delete(runtime.restoreFailures, record.Requested.Workspace)
	// Only rejected detail actions are skipped by the production planner.
	for key := range runtime.restoreSkipped {
		for _, kind := range []sessionstate.RestoreActionKind{sessionstate.RestoreRemoveMark, sessionstate.RestoreSetProportion, sessionstate.RestoreResizeFloating, sessionstate.RestoreMoveFloating, sessionstate.RestoreSetFullscreen, sessionstate.RestoreFocus} {
			if strings.HasPrefix(key, record.Requested.Workspace+":"+string(kind)+":") {
				delete(runtime.restoreSkipped, key)
				break
			}
		}
	}
}

// Exhausting this optional diagnostic budget is a resumable yield. The
// runtime's own cancellation/deadline and unrelated storage errors survive.
func (runtime *sessionRuntime) restoreReportIOResult(ctx context.Context, err error) error {
	if err == nil || runtime.context().Err() != nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		retained := make([]error, 0, len(children))
		for _, child := range children {
			retained = append(retained, runtime.restoreReportIOResult(ctx, child))
		}
		return errors.Join(retained...)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

// A missing window alone never clears a launch guard. Only a new explicit
// token and this runtime's definitive rejection of that exact launch allow it.
func (runtime *sessionRuntime) canRetryRejectedApplicationReport(record sessionstate.RestoreOutcome, item sessionstate.Context) bool {
	if runtime.applications == nil || !sessionstate.EvaluateRestorePolicy(item).Eligible || runtime.restoreReportLaunchRearmed[item.ID] == record.AttemptID {
		return false
	}
	rejected, exists := runtime.rejectedApplicationStarts[item.ID]
	return exists && record.StartedAt.After(rejected.startedAt) && rejected.identityDigest == record.IdentityDigest
}

func (runtime *sessionRuntime) retryRejectedApplicationReport(ctx context.Context, record sessionstate.RestoreOutcome, item sessionstate.Context) error {
	if !runtime.canRetryRejectedApplicationReport(record, item) {
		return nil
	}
	rejected := runtime.rejectedApplicationStarts[item.ID]
	candidate, matched, err := runtime.applications.RetryRejectedAttempt(item.ID, rejected.startedAt)
	if err != nil {
		return fmt.Errorf("prepare rejected application retry: %w", err)
	}
	if !matched {
		return nil
	}
	if err := sessionstate.ApplicationSessionStoreFor(runtime.root).SaveContext(ctx, candidate); err != nil {
		return fmt.Errorf("persist rejected application retry: %w", err)
	}
	if err := runtime.applications.RestoreState(candidate); err != nil {
		return fmt.Errorf("adopt rejected application retry: %w", err)
	}
	runtime.applicationPersistedState = candidate
	if runtime.restoreReportLaunchRearmed == nil {
		runtime.restoreReportLaunchRearmed = make(map[sessionstate.ContextID]string)
	}
	runtime.restoreReportLaunchRearmed[item.ID] = record.AttemptID
	delete(runtime.rejectedApplicationStarts, item.ID)
	return nil
}
