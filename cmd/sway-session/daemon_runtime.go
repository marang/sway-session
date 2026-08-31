package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
	"github.com/marang/sway-title-animator/internal/statefile"
	"github.com/marang/sway-title-animator/internal/swayipc"
)

const (
	sessionSnapshotDebounce   = time.Second
	sessionObservationDelay   = 2 * time.Second
	sessionStartupSettleDelay = 10 * time.Second
	sessionStartupRetryDelay  = 5 * time.Second
)

type Node = swayipc.TreeNode

type sessionRuntime struct {
	client             swayRequester
	root               string
	persisted          sessionstate.LayoutSnapshot
	desired            sessionstate.LayoutSnapshot
	debouncer          *sessionstate.SnapshotDebouncer
	registryPresent    bool
	restoreProgress    *sessionstate.RestoreProgress
	restoreEligible    map[sessionstate.ContextID]struct{}
	restoreExcluded    map[string]struct{}
	restoreSkipped     map[string]struct{}
	restoreFailures    map[string]error
	lateRestorePending bool
	originalFocusID    int64
	originalFocusSet   bool
	originalFocusDone  bool
	startupComplete    bool
	startupDeadline    time.Time
	observeDeadline    time.Time
	shutdown           bool
}

func newSessionRuntime(client swayRequester) (*sessionRuntime, error) {
	root, err := sessionstate.DefaultStateRoot()
	if err != nil {
		return nil, err
	}
	previous := sessionstate.LayoutSnapshot{
		Version:    sessionstate.LayoutSchemaVersion,
		Workspaces: []sessionstate.WorkspaceLayout{},
	}
	if err := sessionstate.LayoutFile(root).LoadInto(&previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("load persistent Sway layout: %w", err)
	}
	registry := sessionstate.Registry{}
	registryErr := sessionstate.RegistryFile(root).LoadInto(&registry)
	registryPresent := registryErr == nil
	if registryErr != nil && !errors.Is(registryErr, os.ErrNotExist) {
		return nil, fmt.Errorf("load persistent context registry: %w", registryErr)
	}
	debouncer, err := sessionstate.NewSnapshotDebouncer(previous, sessionSnapshotDebounce)
	if err != nil {
		return nil, fmt.Errorf("initialize Sway layout debounce: %w", err)
	}
	return &sessionRuntime{
		client:          client,
		root:            root,
		persisted:       previous,
		desired:         previous,
		debouncer:       debouncer,
		registryPresent: registryPresent,
		restoreEligible: make(map[sessionstate.ContextID]struct{}),
		restoreExcluded: make(map[string]struct{}),
		restoreSkipped:  make(map[string]struct{}),
		restoreFailures: make(map[string]error),
		startupComplete: len(previous.Workspaces) == 0,
	}, nil
}

// Reconcile applies placement for newly mapped stable application IDs. It
// returns true only when the caller must obtain a fresh tree before capture.
func (runtime *sessionRuntime) Reconcile(root *Node, now time.Time) (bool, error) {
	if runtime == nil || runtime.shutdown {
		return false, nil
	}
	registry, available, err := runtime.loadRegistry()
	if err != nil {
		runtime.observeDeadline = now.Add(sessionStartupRetryDelay)
		return false, err
	}
	if !available {
		runtime.registryPresent = false
		runtime.observeDeadline = time.Time{}
		runtime.startupDeadline = time.Time{}
		return false, err
	}
	runtime.registryPresent = true
	if !runtime.startupComplete && runtime.startupDeadline.IsZero() {
		runtime.startupDeadline = now.Add(sessionStartupSettleDelay)
	}
	if !runtime.originalFocusSet {
		runtime.originalFocusID = focusedContainerID(root)
		runtime.originalFocusSet = true
	}
	// Sway does not emit an IPC event for every geometry change (notably
	// resize). Keep a low-frequency semantic observation active only while a
	// persistent registry exists so those changes still reach the debouncer.
	runtime.observeDeadline = now.Add(sessionObservationDelay)
	actions, err := sessionstate.PlanPlacementActions(root, registry, runtime.desired)
	if err != nil {
		return false, err
	}
	if len(actions) != 0 {
		for _, action := range actions {
			// Seeing an unmarked stable application ID proves this is a newly
			// mapped context for the current startup. Record that fact before
			// sending the mark command so an ambiguous response cannot make the
			// subsequent marked observation look like a pre-existing window.
			if action.Kind == sessionstate.PlacementAddMark {
				runtime.restoreEligible[action.ContextID] = struct{}{}
				if runtime.startupComplete {
					runtime.rearmLateRestore(action.ContextID)
				}
			}
			if err := runtime.applyPlacementAction(action); err != nil {
				var unknown *swayipc.CommandOutcomeUnknownError
				var invalid *swayipc.CommandResponseInvalidError
				return errors.As(err, &unknown) || errors.As(err, &invalid), err
			}
		}
		return true, nil
	}

	captured, err := sessionstate.CaptureLayout(root, registry)
	if err != nil {
		return false, err
	}
	if !runtime.startupComplete {
		ready, err := sessionstate.StartupCaptureReady(runtime.desired, captured, registry)
		if err != nil {
			return false, err
		}
		if !ready && now.Before(runtime.startupDeadline) {
			return false, nil
		}
		refresh, done, restoreErr := runtime.restoreStartupLayout(root)
		if refresh || restoreErr != nil {
			return refresh, restoreErr
		}
		if !done {
			return false, nil
		}
		runtime.startupComplete = true
		runtime.startupDeadline = time.Time{}
	} else if runtime.lateRestorePending {
		refresh, done, restoreErr := runtime.restoreStartupLayout(root)
		if refresh || restoreErr != nil {
			return refresh, restoreErr
		}
		if !done {
			return false, nil
		}
		runtime.lateRestorePending = false
	}
	stable, err := sessionstate.PreserveMissingPlacements(runtime.persisted, captured, registry)
	if err != nil {
		return false, err
	}
	failedWorkspaces := make(map[string]struct{}, len(runtime.restoreFailures))
	for workspace := range runtime.restoreFailures {
		if workspace != "" {
			failedWorkspaces[workspace] = struct{}{}
		}
	}
	stable, preservedFailures, err := sessionstate.PreserveFailedRestoreWorkspaces(
		runtime.persisted,
		stable,
		failedWorkspaces,
	)
	if err != nil {
		return false, err
	}
	for workspace := range failedWorkspaces {
		if _, preserved := preservedFailures[workspace]; !preserved {
			delete(runtime.restoreFailures, workspace)
		}
	}
	runtime.desired = stable
	_, err = runtime.debouncer.Observe(stable, now)
	return false, err
}

func (runtime *sessionRuntime) restoreStartupLayout(root *Node) (bool, bool, error) {
	const maximumTransitions = 8
	registry, available, err := runtime.loadRegistry()
	if err != nil || !available {
		return false, false, err
	}
	for range maximumTransitions {
		if runtime.restoreProgress == nil {
			selection, err := sessionstate.SelectRestoreWorkspace(
				root,
				registry,
				runtime.persisted,
				runtime.restoreEligible,
				runtime.restoreExcluded,
			)
			if err != nil {
				return false, false, err
			}
			var degradationErrors []error
			for _, degradation := range selection.Degradations {
				runtime.restoreExcluded[degradation.Workspace] = struct{}{}
				degradationErr := fmt.Errorf(
					"degrade workspace %q restore: %s",
					degradation.Workspace,
					degradation.Reason,
				)
				runtime.restoreFailures[degradation.Workspace] = degradationErr
				degradationErrors = append(degradationErrors, degradationErr)
			}
			if selection.Progress == nil {
				if len(degradationErrors) != 0 {
					return false, false, errors.Join(degradationErrors...)
				}
				if !runtime.originalFocusDone && runtime.originalFocusID > 0 {
					node := findContainerByID(root, runtime.originalFocusID)
					if node != nil && !node.Focused {
						action := sessionstate.RestoreAction{
							Kind:        sessionstate.RestoreFocus,
							ContainerID: node.ID,
						}
						if err := runtime.applyRestoreAction(action); err != nil {
							var unknown *swayipc.CommandOutcomeUnknownError
							var invalid *swayipc.CommandResponseInvalidError
							if errors.As(err, &unknown) || errors.As(err, &invalid) {
								return true, false, err
							}
							runtime.originalFocusDone = true
							return false, true, fmt.Errorf("restore original focus: %w", err)
						}
						return true, false, nil
					}
				}
				runtime.originalFocusDone = true
				return false, true, nil
			}
			runtime.restoreProgress = selection.Progress
			if len(degradationErrors) != 0 {
				return false, false, errors.Join(degradationErrors...)
			}
		}

		desired, exists := workspaceByName(runtime.persisted, runtime.restoreProgress.Workspace)
		if !exists {
			return false, false, fmt.Errorf("restore workspace %q is absent from persisted layout", runtime.restoreProgress.Workspace)
		}
		step, err := sessionstate.PlanWorkspaceRestoreStep(
			root,
			registry,
			desired,
			*runtime.restoreProgress,
			runtime.restoreSkipped,
		)
		if err != nil {
			if runtime.restoreProgress.Phase == sessionstate.RestoreRollbackOut ||
				runtime.restoreProgress.Phase == sessionstate.RestoreRollbackIn {
				workspace := runtime.restoreProgress.Workspace
				runtime.restoreExcluded[workspace] = struct{}{}
				runtime.restoreProgress = nil
				return false, false, fmt.Errorf("plan rollback for workspace %q: %w", workspace, err)
			}
			return runtime.beginRestoreRollback(err)
		}
		runtime.restoreProgress = &step.Progress
		if step.Action == nil {
			if !step.Done {
				continue
			}
			workspace := runtime.restoreProgress.Workspace
			runtime.restoreExcluded[workspace] = struct{}{}
			failed := runtime.restoreProgress.Phase == sessionstate.RestoreRollbackIn
			runtime.restoreProgress = nil
			if failed {
				return false, false, runtime.restoreFailures[workspace]
			}
			continue
		}

		action := *step.Action
		if err := runtime.applyRestoreAction(action); err != nil {
			var unknown *swayipc.CommandOutcomeUnknownError
			var invalid *swayipc.CommandResponseInvalidError
			if errors.As(err, &unknown) || errors.As(err, &invalid) {
				return true, false, err
			}
			if action.Structural {
				if runtime.restoreProgress.Phase == sessionstate.RestoreRollbackOut ||
					runtime.restoreProgress.Phase == sessionstate.RestoreRollbackIn {
					workspace := runtime.restoreProgress.Workspace
					runtime.restoreExcluded[workspace] = struct{}{}
					runtime.restoreProgress = nil
					runtime.restoreFailures[workspace] = err
					return false, false, fmt.Errorf("rollback workspace %q after restore failure: %w", workspace, err)
				}
				return runtime.beginRestoreRollback(err)
			}
			runtime.restoreSkipped[action.Key()] = struct{}{}
			runtime.restoreFailures[action.Workspace] = err
			return true, false, err
		}
		return true, false, nil
	}
	// Yield after bounded in-memory transitions. The periodic observation stays
	// armed, so a startup with many already-converged workspaces continues on a
	// later event-loop turn without emitting a false failure diagnostic.
	return false, false, nil
}

func (runtime *sessionRuntime) beginRestoreRollback(cause error) (bool, bool, error) {
	if runtime.restoreProgress == nil {
		return false, false, cause
	}
	workspace := runtime.restoreProgress.Workspace
	runtime.restoreFailures[workspace] = cause
	runtime.restoreProgress.Phase = sessionstate.RestoreRollbackOut
	// Rollback has its own bounded budget. Reusing a build budget which was
	// already exhausted could strand managed windows on the staging workspace.
	runtime.restoreProgress.Steps = 0
	return true, false, fmt.Errorf("restore workspace %q: %w", workspace, cause)
}

func (runtime *sessionRuntime) rearmLateRestore(id sessionstate.ContextID) {
	runtime.lateRestorePending = true
	if workspace, exists := snapshotContextWorkspace(runtime.desired, id); exists {
		delete(runtime.restoreExcluded, workspace)
	}
}

func snapshotContextWorkspace(snapshot sessionstate.LayoutSnapshot, id sessionstate.ContextID) (string, bool) {
	var contains func(*sessionstate.LayoutNode) bool
	contains = func(node *sessionstate.LayoutNode) bool {
		if node == nil {
			return false
		}
		if node.ContextID != nil && *node.ContextID == id {
			return true
		}
		for index := range node.Children {
			if contains(&node.Children[index]) {
				return true
			}
		}
		return false
	}
	for _, workspace := range snapshot.Workspaces {
		for _, contextID := range workspace.PlacementContexts {
			if contextID == id {
				return workspace.Name, true
			}
		}
		if contains(workspace.Tiling) {
			return workspace.Name, true
		}
		for index := range workspace.Floating {
			if contains(&workspace.Floating[index]) {
				return workspace.Name, true
			}
		}
	}
	return "", false
}

func workspaceByName(snapshot sessionstate.LayoutSnapshot, name string) (sessionstate.WorkspaceLayout, bool) {
	for _, workspace := range snapshot.Workspaces {
		if workspace.Name == name {
			return workspace, true
		}
	}
	return sessionstate.WorkspaceLayout{}, false
}

func (runtime *sessionRuntime) loadRegistry() (sessionstate.Registry, bool, error) {
	registry := sessionstate.Registry{}
	err := sessionstate.RegistryFile(runtime.root).LoadInto(&registry)
	if errors.Is(err, os.ErrNotExist) {
		return sessionstate.Registry{}, false, nil
	}
	if err != nil {
		return sessionstate.Registry{}, false, fmt.Errorf("load persistent context registry: %w", err)
	}
	return registry, true, nil
}

func (runtime *sessionRuntime) applyPlacementAction(action sessionstate.PlacementAction) error {
	var command string
	switch action.Kind {
	case sessionstate.PlacementMoveWorkspace:
		command = fmt.Sprintf(
			"[con_id=%d] move container to workspace %s",
			action.ContainerID,
			quoteSwayString(action.Workspace),
		)
	case sessionstate.PlacementAddMark:
		mark, err := action.ContextID.Mark()
		if err != nil {
			return fmt.Errorf("derive persistent Sway mark: %w", err)
		}
		command = fmt.Sprintf("[con_id=%d] mark --add %s", action.ContainerID, quoteSwayString(mark))
	default:
		return fmt.Errorf("unsupported placement action %q", action.Kind)
	}
	if err := runtime.runSwayCommand(command); err != nil {
		return fmt.Errorf("apply %s for context %q: %w", action.Kind, action.ContextID, err)
	}
	return nil
}

func (runtime *sessionRuntime) applyRestoreAction(action sessionstate.RestoreAction) error {
	var command string
	switch action.Kind {
	case sessionstate.RestoreMoveWorkspace:
		command = fmt.Sprintf("[con_id=%d] move container to workspace %s", action.ContainerID, quoteSwayString(action.Target))
	case sessionstate.RestoreSplit:
		direction := "horizontal"
		if action.Layout == sessionstate.LayoutSplitVertical {
			direction = "vertical"
		}
		command = fmt.Sprintf("[con_id=%d] split %s", action.ContainerID, direction)
	case sessionstate.RestoreSetLayout:
		layout := string(action.Layout)
		if action.Layout == sessionstate.LayoutStacked {
			layout = "stacking"
		}
		command = fmt.Sprintf("[con_id=%d] layout %s", action.ContainerID, layout)
	case sessionstate.RestoreAddTemporaryMark:
		command = fmt.Sprintf("[con_id=%d] mark --add %s", action.ContainerID, quoteSwayString(action.Target))
	case sessionstate.RestoreRemoveMark:
		command = fmt.Sprintf("[con_id=%d] unmark %s", action.ContainerID, quoteSwayString(action.Target))
	case sessionstate.RestoreMoveToMark:
		command = fmt.Sprintf("[con_id=%d] move container to mark %s", action.ContainerID, quoteSwayString(action.Target))
	case sessionstate.RestoreSetFloating:
		value := "disable"
		if action.Enable {
			value = "enable"
		}
		command = fmt.Sprintf("[con_id=%d] floating %s", action.ContainerID, value)
	case sessionstate.RestoreSetProportion:
		command = fmt.Sprintf("[con_id=%d] resize set %s %d ppt", action.ContainerID, action.Axis, action.Amount)
	case sessionstate.RestoreResizeFloating:
		command = fmt.Sprintf(
			"[con_id=%d] resize set %d px %d px",
			action.ContainerID,
			action.Geometry.Width,
			action.Geometry.Height,
		)
	case sessionstate.RestoreMoveFloating:
		command = fmt.Sprintf(
			"[con_id=%d] move absolute position %d px %d px",
			action.ContainerID,
			action.Geometry.X,
			action.Geometry.Y,
		)
	case sessionstate.RestoreSetFullscreen:
		value := "disable"
		if action.Fullscreen == sessionstate.FullscreenWorkspace {
			value = "enable"
		} else if action.Fullscreen == sessionstate.FullscreenGlobal {
			value = "enable global"
		}
		command = fmt.Sprintf("[con_id=%d] fullscreen %s", action.ContainerID, value)
	case sessionstate.RestoreFocus:
		command = fmt.Sprintf("[con_id=%d] focus", action.ContainerID)
	default:
		return fmt.Errorf("unsupported restore action %q", action.Kind)
	}
	return runtime.runSwayCommand(command)
}

func (runtime *sessionRuntime) runSwayCommand(command string) error {
	message, err := runtime.client.Request(swayipc.RunCommand, []byte(command))
	if err != nil {
		return fmt.Errorf("run Sway command: %w", err)
	}
	if err := swayipc.CheckRunCommandResponse(message); err != nil {
		return fmt.Errorf("run Sway command: %w", err)
	}
	return nil
}

func focusedContainerID(root *Node) int64 {
	if root == nil {
		return 0
	}
	if root.Focused && root.ID > 0 &&
		(root.Type == "con" || root.Type == "floating_con") &&
		len(root.Nodes) == 0 && len(root.FloatingNodes) == 0 {
		return root.ID
	}
	for _, child := range root.Nodes {
		if id := focusedContainerID(child); id > 0 {
			return id
		}
	}
	for _, child := range root.FloatingNodes {
		if id := focusedContainerID(child); id > 0 {
			return id
		}
	}
	return 0
}

func findContainerByID(root *Node, id int64) *Node {
	if root == nil {
		return nil
	}
	if root.ID == id {
		return root
	}
	for _, child := range root.Nodes {
		if found := findContainerByID(child, id); found != nil {
			return found
		}
	}
	for _, child := range root.FloatingNodes {
		if found := findContainerByID(child, id); found != nil {
			return found
		}
	}
	return nil
}

func (runtime *sessionRuntime) Deadline() (time.Time, bool) {
	if runtime == nil || runtime.shutdown {
		return time.Time{}, false
	}
	deadline, scheduled := runtime.debouncer.Deadline()
	if !runtime.observeDeadline.IsZero() &&
		(!scheduled || runtime.observeDeadline.Before(deadline)) {
		deadline = runtime.observeDeadline
		scheduled = true
	}
	if !runtime.startupComplete && !runtime.startupDeadline.IsZero() &&
		(!scheduled || runtime.startupDeadline.Before(deadline)) {
		return runtime.startupDeadline, true
	}
	return deadline, scheduled
}

func (runtime *sessionRuntime) ObservationDue(now time.Time) bool {
	return runtime != nil && !runtime.shutdown && !runtime.observeDeadline.IsZero() &&
		!now.Before(runtime.observeDeadline)
}

func (runtime *sessionRuntime) ArmObservationRetry(now time.Time) {
	if runtime != nil && !runtime.shutdown && runtime.registryPresent {
		runtime.observeDeadline = now.Add(sessionObservationDelay)
	}
}

func (runtime *sessionRuntime) PostponeObservation(now time.Time) {
	if runtime != nil && !runtime.shutdown && !runtime.observeDeadline.IsZero() {
		runtime.observeDeadline = now.Add(sessionObservationDelay)
	}
}

func (runtime *sessionRuntime) StartupDue(now time.Time) bool {
	return runtime != nil && !runtime.shutdown && !runtime.startupComplete &&
		!runtime.startupDeadline.IsZero() && !now.Before(runtime.startupDeadline)
}

func (runtime *sessionRuntime) PostponeStartup(now time.Time) {
	if runtime != nil && !runtime.shutdown && !runtime.startupComplete {
		runtime.startupDeadline = now.Add(sessionStartupRetryDelay)
	}
}

func (runtime *sessionRuntime) Flush(now time.Time) error {
	if runtime == nil || runtime.shutdown {
		return nil
	}
	candidate, due := runtime.debouncer.Due(now)
	if !due {
		return nil
	}
	err := sessionstate.LayoutFile(runtime.root).Save(candidate)
	if err == nil {
		runtime.persisted = candidate
		return runtime.debouncer.MarkPersisted(candidate)
	}

	var unknown *statefile.CommitOutcomeUnknownError
	if errors.As(err, &unknown) {
		var visible sessionstate.LayoutSnapshot
		if loadErr := sessionstate.LayoutFile(runtime.root).LoadInto(&visible); loadErr != nil {
			runtime.debouncer.Postpone(now)
			return errors.Join(err, fmt.Errorf("reload layout after unknown commit outcome: %w", loadErr))
		}
		candidateHash, candidateErr := sessionstate.SemanticSnapshotHash(candidate)
		visibleHash, visibleErr := sessionstate.SemanticSnapshotHash(visible)
		if candidateErr != nil || visibleErr != nil || candidateHash != visibleHash {
			runtime.debouncer.Postpone(now)
			return errors.Join(err, candidateErr, visibleErr, errors.New("visible layout differs from the candidate after unknown commit outcome"))
		}
		runtime.desired = visible
		runtime.persisted = visible
		if markErr := runtime.debouncer.MarkPersisted(visible); markErr != nil {
			return errors.Join(err, markErr)
		}
		return err
	}
	runtime.debouncer.Postpone(now)
	return err
}

func (runtime *sessionRuntime) Shutdown() {
	if runtime == nil {
		return
	}
	runtime.shutdown = true
	runtime.startupDeadline = time.Time{}
	runtime.observeDeadline = time.Time{}
	runtime.restoreProgress = nil
	runtime.debouncer.Cancel()
}

func reconcilePersistentSession(client swayRequester, runtime *sessionRuntime, report func(error)) {
	// Bound synchronous IPC work per event-loop turn. Long layout restores keep
	// the periodic observation armed and continue on a later turn; reaching the
	// bound is expected progress, not a failed stabilization attempt.
	const maximumObservations = 4
	if runtime != nil {
		runtime.ArmObservationRetry(time.Now())
	}
	for range maximumObservations {
		root, err := requestTree(client)
		if err != nil {
			// An IPC disconnect preserves the last snapshot. The normal event
			// reconnect path will obtain another tree without turning a socket
			// outage into persistent diagnostic noise.
			return
		}
		if runtime == nil {
			return
		}
		refresh, err := runtime.Reconcile(root, time.Now())
		if err != nil && report != nil {
			report(err)
		}
		if !refresh {
			return
		}
	}
}

func quoteSwayString(value string) string {
	escaped := strings.ReplaceAll(value, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
	return "\"" + escaped + "\""
}
