package main

import (
	"errors"
	"fmt"
	"os"
	"reflect"
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
	applicationAdoptionGrace  = 5 * time.Second
	applicationCloseGrace     = 2 * time.Second
	applicationLaunchTimeout  = 10 * time.Second
	expectedMoveLifetime      = 500 * time.Millisecond
	maxApplicationPreflights  = 2
)

type Node = swayipc.TreeNode

type applicationContextLauncher interface {
	Prepare(sessionstate.Context) (preparedApplicationLaunch, error)
}

type preparedApplicationLaunch interface {
	Start() error
}

type sessionRuntimeOptions struct {
	Root                string
	CompositorID        string
	StartedAt           time.Time
	ApplicationLauncher applicationContextLauncher
	ApplicationRestore  sessionstate.ApplicationRestoreOptions
	IndicatorCatalog    func() (sessionstate.DesktopCatalog, error)
	IndicatorOperations func() ([]sessionstate.ApplicationOperation, error)
}

type sessionRuntime struct {
	client              swayRequester
	root                string
	persisted           sessionstate.LayoutSnapshot
	desired             sessionstate.LayoutSnapshot
	debouncer           *sessionstate.SnapshotDebouncer
	registryPresent     bool
	restoreProgress     *sessionstate.RestoreProgress
	restoreEligible     map[sessionstate.ContextID]struct{}
	restoreExcluded     map[string]struct{}
	restoreSkipped      map[string]struct{}
	restoreFailures     map[string]error
	lateRestorePending  bool
	originalFocusID     int64
	originalFocusSet    bool
	originalFocusDone   bool
	startupComplete     bool
	startupDeadline     time.Time
	observeDeadline     time.Time
	shutdown            bool
	applications        *sessionstate.ApplicationRestoreCoordinator
	applicationLauncher applicationContextLauncher
	applicationCursor   sessionstate.ContextID
	expectedMoves       map[int64]expectedMove
	indicatorCatalog    func() (sessionstate.DesktopCatalog, error)
	indicatorOperations func() ([]sessionstate.ApplicationOperation, error)
}

type expectedMove struct {
	count     int
	expiresAt time.Time
}

func newSessionRuntime(client swayRequester) (*sessionRuntime, error) {
	root, err := sessionstate.DefaultStateRoot()
	if err != nil {
		return nil, err
	}
	return newSessionRuntimeWithOptions(client, sessionRuntimeOptions{Root: root})
}

func newSessionRuntimeWithOptions(client swayRequester, options sessionRuntimeOptions) (*sessionRuntime, error) {
	root := options.Root
	if root == "" {
		var err error
		root, err = sessionstate.DefaultStateRoot()
		if err != nil {
			return nil, err
		}
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
	runtime := &sessionRuntime{
		client:              client,
		root:                root,
		persisted:           previous,
		desired:             previous,
		debouncer:           debouncer,
		registryPresent:     registryPresent,
		restoreEligible:     make(map[sessionstate.ContextID]struct{}),
		restoreExcluded:     make(map[string]struct{}),
		restoreSkipped:      make(map[string]struct{}),
		restoreFailures:     make(map[string]error),
		startupComplete:     len(previous.Workspaces) == 0,
		applicationLauncher: options.ApplicationLauncher,
		expectedMoves:       make(map[int64]expectedMove),
		indicatorCatalog:    options.IndicatorCatalog,
		indicatorOperations: options.IndicatorOperations,
	}
	if options.CompositorID != "" {
		applicationState := sessionstate.ApplicationSessionState{}
		if err := sessionstate.ApplicationSessionFile(root).LoadInto(&applicationState); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("load desktop application restore state: %w", err)
		}
		startedAt := options.StartedAt
		if startedAt.IsZero() {
			startedAt = time.Now()
		}
		coordinator, err := sessionstate.NewApplicationRestoreCoordinator(
			options.CompositorID, applicationState, startedAt, options.ApplicationRestore,
		)
		if err != nil {
			return nil, fmt.Errorf("initialize desktop application restore: %w", err)
		}
		if applicationState.CompositorID != options.CompositorID {
			if err := sessionstate.ApplicationSessionFile(root).Save(coordinator.State()); err != nil {
				return nil, fmt.Errorf("persist new Sway compositor application session: %w", err)
			}
		}
		runtime.applications = coordinator
	}
	return runtime, nil
}

// ReconcileIndicators is deliberately independent from core capture and
// restore. Presentation failures are reported by the caller but never prevent
// session convergence.
func (runtime *sessionRuntime) ReconcileIndicators(root *Node) (bool, error) {
	if runtime == nil || runtime.shutdown {
		return false, nil
	}
	registry, available, err := runtime.loadRegistry()
	if err != nil {
		return false, err
	}
	if !available {
		registry = sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{}}
	}
	catalog := sessionstate.DesktopCatalog{}
	operations := []sessionstate.ApplicationOperation{}
	if registry.Preferences.DesktopIndicators {
		if runtime.indicatorCatalog == nil {
			return false, errors.New("desktop application indicator catalog is unavailable")
		}
		catalog, err = runtime.indicatorCatalog()
		if err != nil {
			return false, fmt.Errorf("load desktop application indicator catalog: %w", err)
		}
		if runtime.indicatorOperations != nil {
			operations, err = runtime.indicatorOperations()
			if err != nil {
				return false, fmt.Errorf("load pending desktop application indicators: %w", err)
			}
		}
	}
	actions, err := sessionstate.PlanApplicationIndicatorActions(root, registry, catalog, operations)
	if err != nil {
		return false, fmt.Errorf("plan desktop application indicators: %w", err)
	}
	refresh := false
	var actionErrors []error
	for _, action := range actions {
		verb := "unmark"
		if action.Kind == sessionstate.ApplicationIndicatorAdd {
			verb = "mark --add"
		}
		command := fmt.Sprintf("[con_id=%d] %s %s", action.ContainerID, verb, quoteSwayString(action.Mark))
		if err := runtime.runSwayCommand(command); err != nil {
			var unknown *swayipc.CommandOutcomeUnknownError
			var invalid *swayipc.CommandResponseInvalidError
			wrapped := fmt.Errorf("apply desktop application indicator on container %d: %w", action.ContainerID, err)
			if errors.As(err, &unknown) || errors.As(err, &invalid) {
				return true, errors.Join(errors.Join(actionErrors...), wrapped)
			}
			actionErrors = append(actionErrors, wrapped)
			continue
		}
		refresh = true
	}
	return refresh, errors.Join(actionErrors...)
}

// HandleEvent records live user intent before the next tree reconciliation.
// Binding, focus, close, and non-daemon move activity supersede conflicting
// startup reconstruction; application launch/adoption remains independent.
func (runtime *sessionRuntime) HandleEvent(event swayipc.Event, now time.Time) {
	if runtime == nil || runtime.shutdown {
		return
	}
	interactiveFocus :=
		event.Type == swayipc.EventWindow && event.Change == "focus" ||
			event.Type == swayipc.EventWorkspace && event.Change == "focus"
	if event.Type == swayipc.EventBinding || interactiveFocus {
		runtime.cancelConflictingRestore()
		return
	}
	if event.Type != swayipc.EventWindow || event.Change != "move" && event.Change != "close" {
		return
	}
	if event.Change == "move" && event.Container != nil && runtime.consumeExpectedMove(event.Container.ID, now) {
		return
	}
	if runtime.restoreProgress != nil || runtime.lateRestorePending {
		runtime.cancelConflictingRestore()
	}
}

func (runtime *sessionRuntime) cancelConflictingRestore() {
	runtime.originalFocusDone = true
	runtime.restoreProgress = nil
	runtime.lateRestorePending = false
	runtime.startupComplete = true
	runtime.startupDeadline = time.Time{}
	if runtime.restoreExcluded == nil {
		runtime.restoreExcluded = make(map[string]struct{})
	}
	for _, workspace := range runtime.persisted.Workspaces {
		runtime.restoreExcluded[workspace.Name] = struct{}{}
	}
}

func (runtime *sessionRuntime) consumeExpectedMove(containerID int64, now time.Time) bool {
	expected, exists := runtime.expectedMoves[containerID]
	if containerID <= 0 || !exists {
		return false
	}
	if !now.Before(expected.expiresAt) {
		delete(runtime.expectedMoves, containerID)
		return false
	}
	expected.count--
	if expected.count == 0 {
		delete(runtime.expectedMoves, containerID)
	} else {
		runtime.expectedMoves[containerID] = expected
	}
	return true
}

func (runtime *sessionRuntime) expectMove(containerID int64) {
	if containerID <= 0 {
		return
	}
	if runtime.expectedMoves == nil {
		runtime.expectedMoves = make(map[int64]expectedMove)
	}
	expected := runtime.expectedMoves[containerID]
	expected.count++
	expected.expiresAt = time.Now().Add(expectedMoveLifetime)
	runtime.expectedMoves[containerID] = expected
}

func (runtime *sessionRuntime) discardExpectedMove(containerID int64) {
	_ = runtime.consumeExpectedMove(containerID, time.Now())
}

// Reconcile applies placement for newly mapped stable application IDs. It
// returns true only when the caller must obtain a fresh tree before capture.
func (runtime *sessionRuntime) Reconcile(root *Node, now time.Time) (needsRefresh bool, resultErr error) {
	var degraded []error
	defer func() {
		if len(degraded) != 0 {
			degraded = append(degraded, resultErr)
			resultErr = errors.Join(degraded...)
		}
	}()
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
		// Registration is performed by a separate CLI process. Keep one slow
		// observation armed even before contexts.json exists so the first
		// successful registration cannot be missed if its Sway mark event races
		// the registry commit.
		runtime.observeDeadline = now.Add(sessionObservationDelay)
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
	// The missing-registry branch above schedules a separate discovery tick.
	runtime.observeDeadline = now.Add(sessionObservationDelay)
	applicationRefresh, registry, applicationDegraded, applicationErr := runtime.reconcileApplications(root, registry, now)
	if applicationDegraded != nil {
		degraded = append(degraded, applicationDegraded)
	}
	if applicationRefresh || applicationErr != nil {
		return applicationRefresh, applicationErr
	}
	actions, err := sessionstate.PlanPlacementActions(root, registry, runtime.desired)
	if err != nil {
		return false, err
	}
	failedMoveContexts := make(map[sessionstate.ContextID]struct{})
	if len(actions) != 0 {
		placementRefresh := false
		failedContexts := make(map[sessionstate.ContextID]struct{})
		for _, action := range actions {
			if _, failed := failedContexts[action.ContextID]; failed {
				continue
			}
			// Seeing an unmarked stable application ID proves this is a newly
			// mapped context for the current startup. Record that fact before
			// sending the mark command so an ambiguous response cannot make the
			// subsequent marked observation look like a pre-existing window.
			_, alreadyEligible := runtime.restoreEligible[action.ContextID]
			if action.Kind == sessionstate.PlacementAddMark {
				runtime.restoreEligible[action.ContextID] = struct{}{}
				if runtime.startupComplete {
					runtime.rearmLateRestore(action.ContextID)
				}
			}
			if err := runtime.applyPlacementAction(action); err != nil {
				var unknown *swayipc.CommandOutcomeUnknownError
				var invalid *swayipc.CommandResponseInvalidError
				if errors.As(err, &unknown) || errors.As(err, &invalid) {
					return true, err
				}
				if action.Kind == sessionstate.PlacementAddMark && !alreadyEligible {
					delete(runtime.restoreEligible, action.ContextID)
				}
				failedContexts[action.ContextID] = struct{}{}
				if action.Kind == sessionstate.PlacementMoveWorkspace {
					failedMoveContexts[action.ContextID] = struct{}{}
				}
				degraded = append(degraded, err)
				continue
			}
			placementRefresh = true
		}
		if placementRefresh {
			return true, nil
		}
	}

	captureRegistry := registry
	if len(failedMoveContexts) != 0 {
		captureRegistry.Contexts = append([]sessionstate.Context(nil), registry.Contexts...)
		for index := range captureRegistry.Contexts {
			if _, failed := failedMoveContexts[captureRegistry.Contexts[index].ID]; failed {
				// A window whose absolute placement was rejected is not trustworthy
				// capture evidence for this pass. Treat it as temporarily absent so
				// PreserveMissingPlacements keeps its last-good target without
				// blocking unrelated contexts from being captured.
				captureRegistry.Contexts[index].State = sessionstate.ContextArchived
			}
		}
	}
	captured, err := sessionstate.CaptureLayout(root, captureRegistry)
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

func (runtime *sessionRuntime) reconcileApplications(root *Node, registry sessionstate.Registry, now time.Time) (bool, sessionstate.Registry, error, error) {
	if runtime.applications == nil {
		return false, registry, nil, nil
	}
	groups, err := sessionstate.ObserveApplicationGroups(root, registry)
	if err != nil {
		return false, registry, nil, err
	}
	plan, err := runtime.applications.Plan(registry, groups, now)
	if err != nil {
		return false, registry, nil, err
	}
	if len(plan.DesiredOpen) != 0 {
		type desiredChange struct {
			open     bool
			identity sessionstate.ApplicationIdentity
		}
		changes := make(map[sessionstate.ContextID]desiredChange, len(plan.DesiredOpen))
		for _, change := range plan.DesiredOpen {
			for _, context := range registry.Contexts {
				if context.ID == change.ContextID && context.App != nil {
					changes[change.ContextID] = desiredChange{open: change.Open, identity: context.App.Identity}
					break
				}
			}
		}
		updated, err := sessionstate.UpdateRegistry(runtime.root, func(current *sessionstate.Registry) error {
			for index := range current.Contexts {
				change, exists := changes[current.Contexts[index].ID]
				if !exists || current.Contexts[index].App == nil || current.Contexts[index].App.Identity != change.identity {
					continue
				}
				if current.Contexts[index].App.RestorePolicy == sessionstate.ApplicationRestorePinned && !change.open {
					continue
				}
				current.Contexts[index].App.DesiredOpen = change.open
			}
			return current.Validate()
		})
		if err != nil {
			return false, registry, nil, fmt.Errorf("persist desktop application desired-open state: %w", err)
		}
		registry = updated
	}
	refresh := false
	var degradedErrors []error
	err = sessionstate.InspectRegistryLocked(runtime.root, func(current sessionstate.Registry) error {
		registry = current
		currentGroups, err := sessionstate.ObserveApplicationGroups(root, current)
		if err != nil {
			return err
		}
		currentPlan, err := runtime.applications.Plan(current, currentGroups, now)
		if err != nil {
			return err
		}
		// A concurrent lifecycle mutation which creates another desired-open
		// change is handled on the next periodic observation. Never cross an
		// external placement/launch boundary from stale registry evidence.
		if len(currentPlan.DesiredOpen) != 0 {
			return nil
		}
		placement, err := sessionstate.PlanApplicationPlacementActions(currentGroups, runtime.desired)
		if err != nil {
			return err
		}
		failedContexts := make(map[sessionstate.ContextID]struct{})
		for _, action := range placement {
			if _, failed := failedContexts[action.ContextID]; failed {
				continue
			}
			_, alreadyEligible := runtime.restoreEligible[action.ContextID]
			if action.Kind == sessionstate.PlacementAddMark && !runtime.startupComplete {
				runtime.restoreEligible[action.ContextID] = struct{}{}
			}
			if err := runtime.applyPlacementAction(action); err != nil {
				var unknown *swayipc.CommandOutcomeUnknownError
				var invalid *swayipc.CommandResponseInvalidError
				if errors.As(err, &unknown) || errors.As(err, &invalid) {
					refresh = true
					return err
				}
				if action.Kind == sessionstate.PlacementAddMark && !alreadyEligible {
					delete(runtime.restoreEligible, action.ContextID)
				}
				failedContexts[action.ContextID] = struct{}{}
				degradedErrors = append(degradedErrors, err)
				continue
			}
			refresh = true
		}
		var launchErrors []error
		launchSlots := currentPlan.LaunchSlots
		preflights := 0
		for _, context := range rotateApplicationLaunchCandidates(currentPlan.Launch, runtime.applicationCursor) {
			if launchSlots == 0 {
				break
			}
			if preflights == maxApplicationPreflights {
				break
			}
			preflights++
			runtime.applicationCursor = context.ID
			if runtime.applicationLauncher == nil {
				launchErrors = append(launchErrors, fmt.Errorf("launch desktop application %q: launcher is unavailable", context.ID))
				continue
			}
			prepared, err := runtime.applicationLauncher.Prepare(context)
			if err != nil {
				launchErrors = append(launchErrors, fmt.Errorf("prepare desktop application launch %q: %w", context.ID, err))
				continue
			}
			previousState := runtime.applications.State()
			candidate, err := runtime.applications.BeginAttempt(context.ID, now)
			if err != nil {
				launchErrors = append(launchErrors, fmt.Errorf("begin desktop application launch %q: %w", context.ID, err))
				continue
			}
			if saveErr := sessionstate.ApplicationSessionFile(runtime.root).Save(candidate); saveErr != nil {
				var unknown *statefile.CommitOutcomeUnknownError
				var visible sessionstate.ApplicationSessionState
				confirmed := errors.As(saveErr, &unknown) &&
					sessionstate.ApplicationSessionFile(runtime.root).LoadInto(&visible) == nil &&
					reflect.DeepEqual(visible, candidate)
				if !confirmed {
					_ = runtime.applications.RestoreState(previousState)
					launchErrors = append(launchErrors, fmt.Errorf("persist desktop application launch intent %q: %w", context.ID, saveErr))
					continue
				}
				launchErrors = append(launchErrors, fmt.Errorf("desktop application launch intent %q is visible but crash durability is unknown: %w", context.ID, saveErr))
			}
			launchSlots--
			if err := prepared.Start(); err != nil {
				launchErrors = append(launchErrors, fmt.Errorf("launch desktop application %q: %w", context.ID, err))
			}
		}
		degradedErrors = append(degradedErrors, launchErrors...)
		return nil
	})
	return refresh, registry, errors.Join(degradedErrors...), err
}

func rotateApplicationLaunchCandidates(contexts []sessionstate.Context, after sessionstate.ContextID) []sessionstate.Context {
	if len(contexts) < 2 || after == "" {
		return contexts
	}
	for index := range contexts {
		if contexts[index].ID != after {
			continue
		}
		rotated := make([]sessionstate.Context, 0, len(contexts))
		rotated = append(rotated, contexts[index+1:]...)
		rotated = append(rotated, contexts[:index+1]...)
		return rotated
	}
	return contexts
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
	move := false
	switch action.Kind {
	case sessionstate.PlacementMoveWorkspace:
		move = true
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
	if move {
		runtime.expectMove(action.ContainerID)
	}
	if err := runtime.runSwayCommand(command); err != nil {
		var unknown *swayipc.CommandOutcomeUnknownError
		var invalid *swayipc.CommandResponseInvalidError
		if move && !errors.As(err, &unknown) && !errors.As(err, &invalid) {
			runtime.discardExpectedMove(action.ContainerID)
		}
		return fmt.Errorf("apply %s for context %q: %w", action.Kind, action.ContextID, err)
	}
	return nil
}

func (runtime *sessionRuntime) applyRestoreAction(action sessionstate.RestoreAction) error {
	var command string
	move := false
	switch action.Kind {
	case sessionstate.RestoreMoveWorkspace:
		move = true
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
		move = true
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
		move = true
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
	if move {
		runtime.expectMove(action.ContainerID)
	}
	err := runtime.runSwayCommand(command)
	if err != nil {
		var unknown *swayipc.CommandOutcomeUnknownError
		var invalid *swayipc.CommandResponseInvalidError
		if move && !errors.As(err, &unknown) && !errors.As(err, &invalid) {
			runtime.discardExpectedMove(action.ContainerID)
		}
	}
	return err
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
	if runtime != nil && !runtime.shutdown {
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
	var diagnostics []error
	seenDiagnostics := make(map[string]struct{})
	collect := func(err error) {
		if err == nil {
			return
		}
		if _, duplicate := seenDiagnostics[err.Error()]; duplicate {
			return
		}
		seenDiagnostics[err.Error()] = struct{}{}
		diagnostics = append(diagnostics, err)
	}
	defer func() {
		if report != nil && len(diagnostics) != 0 {
			report(errors.Join(diagnostics...))
		}
	}()
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
		collect(err)
		indicatorRefresh, indicatorErr := runtime.ReconcileIndicators(root)
		collect(indicatorErr)
		if !refresh && !indicatorRefresh {
			return
		}
	}
}

func quoteSwayString(value string) string {
	escaped := strings.ReplaceAll(value, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
	return "\"" + escaped + "\""
}
