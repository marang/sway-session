package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/marang/sway-session/internal/diagnostic"
	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/statefile"
)

const (
	lifecyclePassLimit    = 32
	lifecyclePollDelay    = 2 * time.Second
	lifecycleRetryMaximum = 30 * time.Second
)

var errLifecyclePlanChanged = errors.New("lifecycle reservation or context changed after planning")

// Operation begin/complete and generic effects share this file lock. Refresh
// reservations and compare the planned context under it, closing the interval
// between an otherwise valid planning observation and external dispatch.
func (runtime *sessionRuntime) withLifecycleContextGuard(expected []sessionstate.Context, action func() error) error {
	if runtime.lifecycleOperations == nil || len(expected) == 0 {
		return action()
	}
	return sessionstate.WithRegistryLockContext(runtime.context(), runtime.root, func(*statefile.LockedPrivateDirectory) error {
		if err := runtime.refreshLifecycleBlocked(); err != nil {
			return err
		}
		current, err := sessionstate.ReadRegistrySnapshotContext(runtime.context(), runtime.root)
		if err != nil {
			return err
		}
		for _, planned := range expected {
			if _, blocked := runtime.lifecycleBlocked[planned.ID]; blocked {
				return errLifecyclePlanChanged
			}
			index, err := sessionstate.ResolveContext(current, string(planned.ID))
			if err != nil || !reflect.DeepEqual(current.Contexts[index], planned) {
				return errLifecyclePlanChanged
			}
		}
		return action()
	})
}

// lifecycleRuntimeOperations is shared by the real core adapter and isolated
// daemon fixtures. The core owns durable retry deadlines and operation locks.
type lifecycleRuntimeOperations interface {
	Reconcile(context.Context, time.Time, string, int) (sessionstate.LifecycleReconcileResult, error)
	BlockedContextIDs(context.Context) ([]sessionstate.ContextID, error)
}

type lifecycleOperationDiagnostic struct{ outcome sessionstate.LifecycleOutcome }

func (err lifecycleOperationDiagnostic) Error() string {
	return fmt.Sprintf("lifecycle operation %s (%s): %s: %s", err.outcome.OperationID, err.outcome.Kind, err.outcome.Status, err.outcome.Reason)
}

func (err lifecycleOperationDiagnostic) Diagnostic() diagnostic.Diagnostic {
	hint := "Inspect sway-session state operations. Retry this exact operation with sway-session state operations --retry " + err.outcome.OperationID + "."
	if err.outcome.Kind == sessionstate.LifecycleRegister || err.outcome.Kind == sessionstate.LifecycleRebind {
		hint += " Request rollback with sway-session state operations --cancel " + err.outcome.OperationID + "; identity checks still apply."
	}
	return diagnostic.Diagnostic{
		Level: diagnostic.LevelError, Code: "lifecycle_operation_" + err.outcome.Status,
		Message: err.Error(), Hint: hint,
		Details: map[string]any{"operation_id": err.outcome.OperationID, "kind": err.outcome.Kind, "status": err.outcome.Status, "reason": err.outcome.Reason},
	}
}

func (runtime *sessionRuntime) reconcileLifecycleOperations(now time.Time) error {
	if runtime == nil || runtime.shutdown || runtime.lifecycleOperations == nil {
		return nil
	}
	if err := runtime.requireCurrentEventStream(); err != nil {
		return err
	}
	// Reservations must remain visible even while this process backs off from
	// an unavailable database or while individual operations await retry.
	if err := runtime.refreshLifecycleBlocked(); err != nil {
		runtime.postponeLifecycleFailure(now)
		return err
	}
	if !runtime.lifecycleDeadline.IsZero() && now.Before(runtime.lifecycleDeadline) {
		return nil
	}
	result, err := runtime.lifecycleOperations.Reconcile(runtime.context(), now, runtime.lifecycleCursor, lifecyclePassLimit)
	runtime.lifecycleCursor = result.NextID
	// A completed or rolled-back operation releases its reservation. Refresh
	// even after a partial failure; neither the old block list nor registry is
	// authoritative after the core has attempted a durable transition.
	blockedErr := runtime.refreshLifecycleBlocked()
	if err != nil || blockedErr != nil {
		runtime.postponeLifecycleFailure(now)
	} else {
		runtime.lifecycleRetry = 0
		runtime.lifecycleDeadline = now.Add(lifecyclePollDelay)
	}
	var reported []error
	for _, outcome := range result.Outcomes {
		if outcome.Status == "conflict" || outcome.Status == "retry" {
			reported = append(reported, lifecycleOperationDiagnostic{outcome: outcome})
		}
	}
	return errors.Join(append(reported, err, blockedErr)...)
}

func (runtime *sessionRuntime) postponeLifecycleFailure(now time.Time) {
	if runtime.lifecycleRetry == 0 {
		runtime.lifecycleRetry = lifecyclePollDelay
	} else {
		runtime.lifecycleRetry = min(runtime.lifecycleRetry*2, lifecycleRetryMaximum)
	}
	runtime.lifecycleDeadline = now.Add(runtime.lifecycleRetry)
}

func (runtime *sessionRuntime) refreshLifecycleBlocked() error {
	if runtime.lifecycleOperations == nil {
		return nil
	}
	ids, err := runtime.lifecycleOperations.BlockedContextIDs(runtime.context())
	if err != nil {
		runtime.lifecycleBlockedKnown = false
		return fmt.Errorf("load pending lifecycle reservations: %w", err)
	}
	blocked := make(map[sessionstate.ContextID]struct{}, len(ids))
	for _, id := range ids {
		blocked[id] = struct{}{}
	}
	runtime.lifecycleBlocked = blocked
	runtime.lifecycleBlockedKnown = true
	return nil
}

// lifecyclePlanningRegistry never changes the cached or persisted registry.
// Call it again after a locked reload, so a concurrently created reservation
// cannot be lost merely because a caller refreshed its registry snapshot.
func (runtime *sessionRuntime) lifecyclePlanningRegistry(registry sessionstate.Registry) (sessionstate.Registry, error) {
	if err := runtime.refreshLifecycleBlocked(); err != nil {
		return sessionstate.Registry{}, err
	}
	if len(runtime.lifecycleBlocked) == 0 {
		return registry, nil
	}
	filtered := registry
	filtered.Contexts = slices.Clone(registry.Contexts)
	for index := range filtered.Contexts {
		if _, blocked := runtime.lifecycleBlocked[filtered.Contexts[index].ID]; blocked {
			// Only placement/capture eligibility changes in this view. Lifecycle
			// coordinators use the authoritative registry with explicit suspension.
			filtered.Contexts[index].State = sessionstate.ContextArchived
			filtered.Contexts[index].Lifecycle = nil
		}
	}
	return filtered, nil
}

func (runtime *sessionRuntime) lifecycleBlockedWorkspaces() map[string]struct{} {
	blocked := make(map[string]struct{})
	for _, item := range runtime.registry.Contexts {
		if item.App == nil {
			continue
		}
		if _, pending := runtime.lifecycleBlocked[item.ID]; !pending {
			continue
		}
		if name, found := snapshotContextWorkspace(runtime.persisted, item.ID); found {
			blocked[name] = struct{}{}
		}
	}
	return blocked
}

type suspendedLifecycleRestore struct {
	progress         sessionstate.RestoreProgress
	observation      *startupApplicationLayout
	workspacePresent bool
	workspaceLayout  string
}

func (runtime *sessionRuntime) suspendLifecycleRestore(root *Node) {
	blocked := runtime.lifecycleBlockedWorkspaces()
	if progress := runtime.restoreProgress; progress != nil {
		if _, paused := blocked[progress.Workspace]; paused {
			if runtime.restoreSuspended == nil {
				runtime.restoreSuspended = make(map[string]suspendedLifecycleRestore)
			}
			runtime.restoreSuspended[progress.Workspace] = suspendedLifecycleRestore{progress: *progress}
			runtime.restoreProgress = nil
		}
	}
	if len(runtime.restoreSuspended) == 0 {
		return
	}
	workspaces := make(map[string]*Node)
	var visit func(*Node)
	visit = func(node *Node) {
		if node == nil {
			return
		}
		if node.Type == "workspace" {
			workspaces[node.Name] = node
			return
		}
		for _, child := range node.Nodes {
			visit(child)
		}
	}
	visit(root)
	for name, suspended := range runtime.restoreSuspended {
		workspace := workspaces[name]
		current := startupApplicationLayoutFingerprint(workspace, nil)
		if previous := suspended.observation; previous != nil {
			// The view projection omits empty workspaces, but their layout may
			// still change while all owned windows are temporarily in staging.
			if suspended.workspacePresent && workspace != nil && suspended.workspaceLayout != workspace.Layout {
				runtime.cancelConflictingRestore()
				return
			}
			// Normal mapping may resize existing siblings. Compare their
			// projected structure, and geometry only with the same window set.
			survivors := startupApplicationLayoutFingerprint(workspaces[name], previous.windows)
			if !slices.Equal(previous.structure, survivors.structure) ||
				len(previous.windows) == len(current.windows) && !slices.Equal(previous.geometry, current.geometry) {
				runtime.cancelConflictingRestore()
				return
			}
		}
		// Refresh only after accepting the observation. New mappings become
		// the baseline so a later resize with the same window set is checked.
		suspended.observation = current
		suspended.workspacePresent = workspace != nil
		if workspace != nil {
			suspended.workspaceLayout = workspace.Layout
		}
		runtime.restoreSuspended[name] = suspended
		if _, paused := blocked[name]; !paused && runtime.startupComplete {
			runtime.lateRestorePending = true
		}
	}
}

func (runtime *sessionRuntime) resumeLifecycleRestore() {
	if runtime.restoreProgress != nil {
		return
	}
	blocked := runtime.lifecycleBlockedWorkspaces()
	names := make([]string, 0, len(runtime.restoreSuspended))
	for name := range runtime.restoreSuspended {
		if _, paused := blocked[name]; !paused {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if len(names) != 0 {
		progress := runtime.restoreSuspended[names[0]].progress
		runtime.restoreProgress = &progress
		delete(runtime.restoreSuspended, names[0])
	}
}

// The cleanup ledger stays intact while a structural cursor is paused. Only
// workspaces ready for compensation may be observed or dispatched by cleanup.
func (runtime *sessionRuntime) lifecycleRestoreExclusions() map[string]struct{} {
	excluded := runtime.lifecycleBlockedWorkspaces()
	for name := range runtime.restoreSuspended {
		excluded[name] = struct{}{}
	}
	if runtime.restoreProgress != nil {
		excluded[runtime.restoreProgress.Workspace] = struct{}{}
	}
	return excluded
}

// Preserve complete workspace trees, not a capture derived from the temporary
// eligibility view. If a sibling moved across workspaces, the two observations
// cannot safely be merged; wait for the reservation to resolve instead.
func (runtime *sessionRuntime) preserveLifecycleCapture(candidate sessionstate.LayoutSnapshot) (sessionstate.LayoutSnapshot, bool) {
	blocked := runtime.lifecycleBlockedWorkspaces()
	for name := range runtime.restoreSuspended {
		blocked[name] = struct{}{}
	}
	if len(blocked) == 0 {
		return candidate, true
	}
	result := candidate
	result.Workspaces = append([]sessionstate.WorkspaceLayout(nil), candidate.Workspaces...)
	for _, previous := range runtime.persisted.Workspaces {
		if _, keep := blocked[previous.Name]; !keep {
			continue
		}
		replaced := false
		for index := range result.Workspaces {
			if result.Workspaces[index].Name == previous.Name {
				result.Workspaces[index] = previous
				replaced = true
				break
			}
		}
		if !replaced {
			result.Workspaces = append(result.Workspaces, previous)
		}
	}
	return result, result.Validate() == nil
}
