package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/statefile"
	"github.com/marang/sway-session/internal/swayipc"
)

const (
	sessionSnapshotDebounce    = time.Second
	sessionObservationDelay    = 2 * time.Second
	sessionStartupSettleDelay  = 10 * time.Second
	sessionStartupRetryDelay   = 5 * time.Second
	applicationAdoptionGrace   = 5 * time.Second
	applicationCloseGrace      = 2 * time.Second
	applicationLaunchTimeout   = 10 * time.Second
	applicationResetRetryDelay = 10 * time.Millisecond
	maxApplicationPreflights   = 2
	terminalFocusBatchDelay    = time.Second
	terminalFocusWriteTimeout  = 250 * time.Millisecond
	terminalFocusRetryMaximum  = 30 * time.Second
	terminalFocusReportEvery   = time.Minute
	maxPendingTerminalFocus    = 256
	terminalCloseGrace         = 2 * time.Second
	terminalCloseWriteTimeout  = 250 * time.Millisecond
	terminalCloseRetryMaximum  = 30 * time.Second
	maxTerminalCloseBatch      = 64
	moveBarrierPrefix          = "_sway_session_move_v1:"
)

type Node = swayipc.TreeNode

type applicationContextLauncher interface {
	Prepare(context.Context, sessionstate.Context) (preparedApplicationLaunch, error)
}

type eventStreamGuard interface {
	Snapshot() (uint64, bool)
}

// TerminalCloseGuard reports whether automatic terminal-close archival is safe
// for the current external shutdown generation. A nil guard deliberately
// disables automatic archival.
type TerminalCloseGuard interface {
	Snapshot() (uint64, bool)
}

type preparedApplicationLaunch interface {
	Start() error
}

type sessionRuntimeOptions struct {
	Context             context.Context
	EventStreamState    eventStreamGuard
	TerminalCloseGuard  TerminalCloseGuard
	Root                string
	CompositorID        string
	StartedAt           time.Time
	ApplicationLauncher applicationContextLauncher
	ApplicationRestore  sessionstate.ApplicationRestoreOptions
	IndicatorCatalog    func() (sessionstate.DesktopCatalog, error)
	IndicatorOperations func() ([]sessionstate.ApplicationOperation, error)
}

type sessionRuntime struct {
	ctx                        context.Context
	client                     swayRequester
	root                       string
	persisted                  sessionstate.LayoutSnapshot
	desired                    sessionstate.LayoutSnapshot
	debouncer                  *sessionstate.SnapshotDebouncer
	registry                   sessionstate.Registry
	registryRevision           int64
	registryCacheKnown         bool
	registryPresent            bool
	restoreProgress            *sessionstate.RestoreProgress
	restoreEligible            map[sessionstate.ContextID]struct{}
	restoreExcluded            map[string]struct{}
	restoreSkipped             map[string]struct{}
	restoreFailures            map[string]error
	lateRestorePending         bool
	originalFocusID            int64
	originalFocusSet           bool
	originalFocusDone          bool
	startupComplete            bool
	startupDeadline            time.Time
	observeDeadline            time.Time
	shutdown                   bool
	applications               *sessionstate.ApplicationRestoreCoordinator
	applicationLauncher        applicationContextLauncher
	applicationCursor          sessionstate.ContextID
	applicationPlacementCursor *sessionstate.PlacementAction
	placementCursor            *sessionstate.PlacementAction
	expectedMoves              map[int64][]uint64
	nextMoveSequence           uint64
	eventStreamReady           bool
	eventStreamEpoch           uint64
	eventStreamState           eventStreamGuard
	terminalCloseGuard         TerminalCloseGuard
	indicatorCatalog           func() (sessionstate.DesktopCatalog, error)
	indicatorOperations        func() ([]sessionstate.ApplicationOperation, error)
	indicatorCursor            *sessionstate.ApplicationIndicatorAction
	pendingTerminalFocus       map[sessionstate.ContextID]time.Time
	terminalFocusDeadline      time.Time
	terminalFocusRetry         time.Duration
	terminalFocusReported      time.Time
	observedTerminals          map[int64]terminalCloseObservation
	pendingTerminalClose       map[int64]terminalCloseCandidate
	terminalCloseDeadline      time.Time
	terminalCloseRetry         time.Duration
	terminalCloseRetryDeadline time.Time
	terminalCloseBatchCursor   int64
	terminalCloseContinuation  time.Time
}

func (runtime *sessionRuntime) context() context.Context {
	if runtime == nil || runtime.ctx == nil {
		return context.Background()
	}
	return runtime.ctx
}

func newSessionRuntime(client swayRequester) (*sessionRuntime, error) {
	root, err := sessionstate.DefaultStateRoot()
	if err != nil {
		return nil, err
	}
	return newSessionRuntimeWithOptions(client, sessionRuntimeOptions{Root: root})
}

func newSessionRuntimeWithOptions(client swayRequester, options sessionRuntimeOptions) (*sessionRuntime, error) {
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	root := options.Root
	if root == "" {
		var err error
		root, err = sessionstate.DefaultStateRoot()
		if err != nil {
			return nil, err
		}
	}
	if err := sessionstate.VerifyStateDatabaseContext(ctx, root); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("verify persistent session database: %w", err)
	}
	previous := sessionstate.LayoutSnapshot{
		Version:    sessionstate.LayoutSchemaVersion,
		Workspaces: []sessionstate.WorkspaceLayout{},
	}
	if err := sessionstate.LayoutStoreFor(root).LoadIntoContext(ctx, &previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("load persistent Sway layout: %w", err)
	}
	registry, registryRevision, registryPresent, _, registryErr := sessionstate.RegistryStoreFor(root).LoadIfChangedContext(ctx, -1, false)
	if errors.Is(registryErr, os.ErrNotExist) {
		registry = sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{}}
		registryRevision = 0
		registryPresent = false
		registryErr = nil
	}
	if registryErr != nil {
		return nil, fmt.Errorf("load persistent context registry: %w", registryErr)
	}
	debouncer, err := sessionstate.NewSnapshotDebouncer(previous, sessionSnapshotDebounce)
	if err != nil {
		return nil, fmt.Errorf("initialize Sway layout debounce: %w", err)
	}
	runtime := &sessionRuntime{
		ctx:                  ctx,
		client:               client,
		root:                 root,
		persisted:            previous,
		desired:              previous,
		debouncer:            debouncer,
		registry:             registry,
		registryRevision:     registryRevision,
		registryCacheKnown:   true,
		registryPresent:      registryPresent,
		restoreEligible:      make(map[sessionstate.ContextID]struct{}),
		restoreExcluded:      make(map[string]struct{}),
		restoreSkipped:       make(map[string]struct{}),
		restoreFailures:      make(map[string]error),
		startupComplete:      len(previous.Workspaces) == 0,
		applicationLauncher:  options.ApplicationLauncher,
		expectedMoves:        make(map[int64][]uint64),
		eventStreamState:     options.EventStreamState,
		terminalCloseGuard:   options.TerminalCloseGuard,
		indicatorCatalog:     options.IndicatorCatalog,
		indicatorOperations:  options.IndicatorOperations,
		observedTerminals:    make(map[int64]terminalCloseObservation),
		pendingTerminalClose: make(map[int64]terminalCloseCandidate),
	}
	if options.CompositorID != "" {
		applicationState := sessionstate.ApplicationSessionState{}
		if err := sessionstate.ApplicationSessionStoreFor(root).LoadIntoContext(ctx, &applicationState); err != nil && !errors.Is(err, os.ErrNotExist) {
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
			if err := retryApplicationSessionReset(ctx, func() error {
				return sessionstate.ApplicationSessionStoreFor(root).SaveContext(ctx, coordinator.State())
			}); err != nil {
				return nil, fmt.Errorf("persist new Sway compositor application session: %w", err)
			}
		}
		runtime.applications = coordinator
	}
	return runtime, nil
}

func retryApplicationSessionReset(ctx context.Context, save func() error) error {
	if ctx == nil {
		return errors.New("application session reset context is nil")
	}
	if save == nil {
		return errors.New("application session reset save operation is nil")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := save()
		if !errors.Is(err, sessionstate.ErrApplicationSessionConflict) && !errors.Is(err, sessionstate.ErrRegistryConflict) {
			return err
		}
		timer := time.NewTimer(applicationResetRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
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
	actions, err := sessionstate.PlanApplicationIndicatorActionsAfter(root, registry, catalog, operations, runtime.indicatorCursor)
	if err != nil {
		return false, fmt.Errorf("plan desktop application indicators: %w", err)
	}
	if len(actions) != 0 {
		last := actions[len(actions)-1]
		runtime.indicatorCursor = &last
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
	if event.Type == swayipc.EventShutdown {
		runtime.eventStreamReady = false
		clear(runtime.observedTerminals)
		clear(runtime.pendingTerminalClose)
		runtime.terminalCloseDeadline = time.Time{}
		runtime.terminalCloseRetry = 0
		runtime.terminalCloseRetryDeadline = time.Time{}
		runtime.terminalCloseBatchCursor = 0
		runtime.terminalCloseContinuation = time.Time{}
		return
	}
	if event.Type == swayipc.EventStream && event.Change == "ready" {
		// A reconnect creates a new event-generation boundary. Any move whose
		// event was lost with the old connection must be rediscovered through
		// the fresh tree instead of consuming later user intent.
		clear(runtime.expectedMoves)
		clear(runtime.observedTerminals)
		clear(runtime.pendingTerminalClose)
		runtime.terminalCloseDeadline = time.Time{}
		runtime.terminalCloseRetry = 0
		runtime.terminalCloseRetryDeadline = time.Time{}
		runtime.terminalCloseBatchCursor = 0
		runtime.terminalCloseContinuation = time.Time{}
		if runtime.eventStreamReady && runtime.restoreMayConflictWithUserIntent() {
			// Events may have been lost while disconnected. Continuing could
			// overwrite user changes that the daemon never observed.
			runtime.cancelConflictingRestore()
		}
		runtime.eventStreamReady = true
		runtime.eventStreamEpoch = event.StreamEpoch
		return
	}
	if event.Type == swayipc.EventStream && event.Change == "disconnected" {
		clear(runtime.expectedMoves)
		clear(runtime.observedTerminals)
		clear(runtime.pendingTerminalClose)
		runtime.terminalCloseDeadline = time.Time{}
		runtime.terminalCloseRetry = 0
		runtime.terminalCloseRetryDeadline = time.Time{}
		runtime.terminalCloseBatchCursor = 0
		runtime.terminalCloseContinuation = time.Time{}
		if runtime.eventStreamReady && runtime.restoreMayConflictWithUserIntent() {
			runtime.cancelConflictingRestore()
		}
		runtime.eventStreamReady = false
		return
	}
	if event.Type == swayipc.EventTick {
		if sequence, ok := moveBarrierSequence(event.Payload); ok {
			runtime.expireExpectedMoves(sequence)
		}
		return
	}
	interactiveFocus :=
		event.Type == swayipc.EventWindow && event.Change == "focus" ||
			event.Type == swayipc.EventWorkspace && event.Change == "focus"
	if event.Type == swayipc.EventBinding || interactiveFocus {
		if event.Type == swayipc.EventWindow && event.Change == "focus" {
			runtime.queueTerminalFocus(event.Container, now)
		}
		runtime.cancelConflictingRestore()
		return
	}
	if event.Type != swayipc.EventWindow || event.Change != "move" && event.Change != "close" {
		return
	}
	if event.Change == "close" {
		runtime.queueTerminalClose(event.Container, now)
	}
	if event.Change == "move" && event.Container != nil && runtime.consumeExpectedMove(event.Container.ID) {
		return
	}
	if runtime.restoreProgress != nil || runtime.lateRestorePending {
		runtime.cancelConflictingRestore()
	}
}

func (runtime *sessionRuntime) queueTerminalFocus(node *Node, observedAt time.Time) {
	if runtime == nil || runtime.shutdown || observedAt.IsZero() || node == nil {
		return
	}
	id, ok := focusedManagedContextID(node)
	if !ok {
		return
	}
	if runtime.pendingTerminalFocus == nil {
		runtime.pendingTerminalFocus = make(map[sessionstate.ContextID]time.Time)
	}
	if _, exists := runtime.pendingTerminalFocus[id]; !exists && len(runtime.pendingTerminalFocus) >= maxPendingTerminalFocus {
		return
	}
	canonical := observedAt.UTC()
	if current, exists := runtime.pendingTerminalFocus[id]; !exists || canonical.After(current) {
		runtime.pendingTerminalFocus[id] = canonical
	}
	if runtime.terminalFocusDeadline.IsZero() {
		runtime.terminalFocusDeadline = observedAt.Add(terminalFocusBatchDelay)
	}
}

func focusedManagedContextID(node *Node) (sessionstate.ContextID, bool) {
	identities := make(map[sessionstate.ContextID]struct{})
	for _, mark := range node.Marks {
		if !strings.HasPrefix(mark, sessionstate.MarkPrefix) {
			continue
		}
		id, err := sessionstate.ParseMark(mark)
		if err != nil {
			return "", false
		}
		identities[id] = struct{}{}
	}
	if node.AppID != nil && strings.HasPrefix(*node.AppID, sessionstate.AppIDPrefix) {
		id, err := sessionstate.ParseAppID(*node.AppID)
		if err != nil {
			return "", false
		}
		identities[id] = struct{}{}
	}
	if len(identities) != 1 {
		return "", false
	}
	for id := range identities {
		return id, true
	}
	return "", false
}

func (runtime *sessionRuntime) restoreMayConflictWithUserIntent() bool {
	return !runtime.startupComplete || runtime.restoreProgress != nil || runtime.lateRestorePending
}

func (runtime *sessionRuntime) requireCurrentEventStream() error {
	if runtime == nil || runtime.eventStreamState == nil {
		return nil
	}
	epoch, connected := runtime.eventStreamState.Snapshot()
	if connected && runtime.eventStreamReady && epoch == runtime.eventStreamEpoch {
		return nil
	}
	if runtime.restoreMayConflictWithUserIntent() {
		runtime.cancelConflictingRestore()
	}
	return errors.New("sway event stream changed during session reconciliation")
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

func (runtime *sessionRuntime) consumeExpectedMove(containerID int64) bool {
	sequences := runtime.expectedMoves[containerID]
	if containerID <= 0 || len(sequences) == 0 {
		return false
	}
	sequences = sequences[1:]
	if len(sequences) == 0 {
		delete(runtime.expectedMoves, containerID)
	} else {
		runtime.expectedMoves[containerID] = sequences
	}
	return true
}

func (runtime *sessionRuntime) expectMove(containerID int64) uint64 {
	if containerID <= 0 {
		return 0
	}
	if runtime.expectedMoves == nil {
		runtime.expectedMoves = make(map[int64][]uint64)
	}
	runtime.nextMoveSequence++
	sequence := runtime.nextMoveSequence
	runtime.expectedMoves[containerID] = append(runtime.expectedMoves[containerID], sequence)
	return sequence
}

func (runtime *sessionRuntime) discardExpectedMove(containerID int64, sequence uint64) {
	sequences := runtime.expectedMoves[containerID]
	for index, candidate := range sequences {
		if candidate != sequence {
			continue
		}
		sequences = append(sequences[:index], sequences[index+1:]...)
		if len(sequences) == 0 {
			delete(runtime.expectedMoves, containerID)
		} else {
			runtime.expectedMoves[containerID] = sequences
		}
		return
	}
}

func (runtime *sessionRuntime) expireExpectedMoves(barrier uint64) {
	for containerID, sequences := range runtime.expectedMoves {
		remaining := sequences[:0]
		for _, sequence := range sequences {
			if sequence > barrier {
				remaining = append(remaining, sequence)
			}
		}
		if len(remaining) == 0 {
			delete(runtime.expectedMoves, containerID)
		} else {
			runtime.expectedMoves[containerID] = remaining
		}
	}
}

func moveBarrierPayload(sequence uint64) string {
	return moveBarrierPrefix + strconv.FormatUint(sequence, 10)
}

func moveBarrierSequence(payload string) (uint64, bool) {
	value, ok := strings.CutPrefix(payload, moveBarrierPrefix)
	if !ok || value == "" {
		return 0, false
	}
	sequence, err := strconv.ParseUint(value, 10, 64)
	return sequence, err == nil && sequence > 0
}

func (runtime *sessionRuntime) handleFailedMove(containerID int64, sequence uint64, err error) {
	if containerID <= 0 {
		return
	}
	var unknown *swayipc.CommandOutcomeUnknownError
	var invalid *swayipc.CommandResponseInvalidError
	if errors.As(err, &unknown) || errors.As(err, &invalid) {
		// The command connection and event connection are independent. An
		// ambiguous command response therefore does not imply an event-stream
		// reconnect that could invalidate this expectation. Stop the active
		// restore and clear all expectations so a later user move can never be
		// consumed as the missing daemon event.
		clear(runtime.expectedMoves)
		runtime.cancelConflictingRestore()
		return
	}
	runtime.discardExpectedMove(containerID, sequence)
}

func (runtime *sessionRuntime) sendMoveBarrier(sequence uint64) error {
	if err := runtime.requireCurrentEventStream(); err != nil {
		return err
	}
	message, err := runtime.client.RequestContext(runtime.context(), swayipc.SendTick, []byte(moveBarrierPayload(sequence)))
	if err != nil {
		return err
	}
	return swayipc.CheckSendTickResponse(message)
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
	if err := runtime.requireCurrentEventStream(); err != nil {
		return false, err
	}
	registry, available, err := runtime.loadRegistry()
	if err != nil {
		runtime.observeDeadline = now.Add(sessionStartupRetryDelay)
		return false, err
	}
	if !available {
		runtime.registryPresent = false
		// Registration is performed by a separate CLI process. Keep one slow
		// observation armed even before a registry exists so the first
		// successful registration cannot be missed if its Sway mark event races
		// the registry commit.
		runtime.observeDeadline = now.Add(sessionObservationDelay)
		runtime.startupDeadline = time.Time{}
		return false, err
	}
	runtime.registryPresent = true
	if err := runtime.observeTerminalCloseState(root, registry, now); err != nil {
		return false, err
	}
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
	actions, err := sessionstate.PlanPlacementActionsAfter(root, registry, runtime.desired, runtime.placementCursor)
	if err != nil {
		return false, err
	}
	if len(actions) != 0 {
		last := actions[len(actions)-1]
		runtime.placementCursor = &last
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
		updated, err := sessionstate.UpdateRegistryContext(runtime.context(), runtime.root, func(current *sessionstate.Registry) error {
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
	err = sessionstate.WithRegistryLockContext(runtime.context(), runtime.root, func(*statefile.LockedPrivateDirectory) error {
		current, available, err := runtime.loadRegistry()
		if err != nil {
			return err
		}
		if !available {
			return errors.New("persistent context registry disappeared during application reconciliation")
		}
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
		placement, err := sessionstate.PlanApplicationPlacementActionsAfter(currentGroups, runtime.desired, runtime.applicationPlacementCursor)
		if err != nil {
			return err
		}
		if len(placement) != 0 {
			last := placement[len(placement)-1]
			runtime.applicationPlacementCursor = &last
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
			prepared, err := runtime.applicationLauncher.Prepare(runtime.context(), context)
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
			if saveErr := sessionstate.ApplicationSessionStoreFor(runtime.root).SaveContext(runtime.context(), candidate); saveErr != nil {
				var unknown *statefile.CommitOutcomeUnknownError
				var visible sessionstate.ApplicationSessionState
				confirmed := errors.As(saveErr, &unknown) &&
					sessionstate.ApplicationSessionStoreFor(runtime.root).LoadIntoContext(runtime.context(), &visible) == nil &&
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
	// Rollback starts its own no-progress detection. Reusing the last failed
	// build action could misclassify the first compensating action as a repeat.
	runtime.restoreProgress.Actions = 0
	runtime.restoreProgress.LastActionKey = ""
	runtime.restoreProgress.RepeatedActions = 0
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
	knownRevision := int64(-1)
	knownPresent := false
	if runtime.registryCacheKnown {
		knownRevision = runtime.registryRevision
		knownPresent = runtime.registryPresent
	}
	registry, revision, present, changed, err := sessionstate.RegistryStoreFor(runtime.root).LoadIfChangedContext(runtime.context(), knownRevision, knownPresent)
	if errors.Is(err, os.ErrNotExist) {
		runtime.registry = sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{}}
		runtime.registryRevision = 0
		runtime.registryCacheKnown = true
		runtime.registryPresent = false
		return sessionstate.Registry{}, false, nil
	}
	if err != nil {
		return sessionstate.Registry{}, false, fmt.Errorf("load persistent context registry: %w", err)
	}
	if changed || !runtime.registryCacheKnown {
		runtime.registry = registry
	}
	runtime.registryRevision = revision
	runtime.registryCacheKnown = true
	runtime.registryPresent = present
	if !present {
		return sessionstate.Registry{}, false, nil
	}
	return runtime.registry, true, nil
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
	moveSequence := uint64(0)
	if move {
		moveSequence = runtime.expectMove(action.ContainerID)
	}
	if err := runtime.runSwayCommand(command); err != nil {
		if move {
			runtime.handleFailedMove(action.ContainerID, moveSequence, err)
		}
		return fmt.Errorf("apply %s for context %q: %w", action.Kind, action.ContextID, err)
	}
	if move {
		if err := runtime.sendMoveBarrier(moveSequence); err != nil {
			unknown := &swayipc.CommandOutcomeUnknownError{Cause: fmt.Errorf("establish move attribution barrier: %w", err)}
			runtime.handleFailedMove(action.ContainerID, moveSequence, unknown)
			return fmt.Errorf("apply %s for context %q: %w", action.Kind, action.ContextID, unknown)
		}
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
	moveSequence := uint64(0)
	if move {
		moveSequence = runtime.expectMove(action.ContainerID)
	}
	err := runtime.runSwayCommand(command)
	if err != nil && move {
		runtime.handleFailedMove(action.ContainerID, moveSequence, err)
	}
	if err == nil && move {
		if barrierErr := runtime.sendMoveBarrier(moveSequence); barrierErr != nil {
			err = &swayipc.CommandOutcomeUnknownError{Cause: fmt.Errorf("establish move attribution barrier: %w", barrierErr)}
			runtime.handleFailedMove(action.ContainerID, moveSequence, err)
		}
	}
	return err
}

func (runtime *sessionRuntime) runSwayCommand(command string) error {
	if err := runtime.requireCurrentEventStream(); err != nil {
		return err
	}
	message, err := runtime.client.RequestContext(runtime.context(), swayipc.RunCommand, []byte(command))
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
	if !runtime.terminalFocusDeadline.IsZero() &&
		(!scheduled || runtime.terminalFocusDeadline.Before(deadline)) {
		deadline = runtime.terminalFocusDeadline
		scheduled = true
	}
	if !runtime.terminalCloseDeadline.IsZero() &&
		(!scheduled || runtime.terminalCloseDeadline.Before(deadline)) {
		deadline = runtime.terminalCloseDeadline
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
	focusErr := runtime.flushTerminalFocus(now)
	closeErr := runtime.flushTerminalClose(now)
	candidate, due := runtime.debouncer.Due(now)
	if !due {
		return errors.Join(focusErr, closeErr)
	}
	err := sessionstate.LayoutStoreFor(runtime.root).SaveContext(runtime.context(), candidate)
	if err == nil {
		runtime.persisted = candidate
		return errors.Join(focusErr, closeErr, runtime.debouncer.MarkPersisted(candidate))
	}

	var unknown *statefile.CommitOutcomeUnknownError
	if errors.As(err, &unknown) {
		var visible sessionstate.LayoutSnapshot
		if loadErr := sessionstate.LayoutStoreFor(runtime.root).LoadIntoContext(runtime.context(), &visible); loadErr != nil {
			runtime.debouncer.Postpone(now)
			return errors.Join(focusErr, closeErr, err, fmt.Errorf("reload layout after unknown commit outcome: %w", loadErr))
		}
		candidateHash, candidateErr := sessionstate.SemanticSnapshotHash(candidate)
		visibleHash, visibleErr := sessionstate.SemanticSnapshotHash(visible)
		if candidateErr != nil || visibleErr != nil || candidateHash != visibleHash {
			runtime.debouncer.Postpone(now)
			return errors.Join(focusErr, closeErr, err, candidateErr, visibleErr, errors.New("visible layout differs from the candidate after unknown commit outcome"))
		}
		runtime.desired = visible
		runtime.persisted = visible
		if markErr := runtime.debouncer.MarkPersisted(visible); markErr != nil {
			return errors.Join(focusErr, closeErr, err, markErr)
		}
		return errors.Join(focusErr, closeErr, err)
	}
	runtime.debouncer.Postpone(now)
	return errors.Join(focusErr, closeErr, err)
}

func (runtime *sessionRuntime) flushTerminalFocus(now time.Time) error {
	if runtime == nil || runtime.terminalFocusDeadline.IsZero() || now.Before(runtime.terminalFocusDeadline) {
		return nil
	}
	writeContext, cancel := context.WithTimeout(runtime.context(), terminalFocusWriteTimeout)
	defer cancel()
	processed := make(map[sessionstate.ContextID]struct{}, len(runtime.pendingTerminalFocus))
	err := sessionstate.WithTerminalLifecycleLockContext(writeContext, runtime.root, func() error {
		registry, loadErr := sessionstate.ReadRegistrySnapshotContext(writeContext, runtime.root)
		if loadErr != nil {
			return fmt.Errorf("load terminal contexts for focus activity: %w", loadErr)
		}
		eligible := make(map[sessionstate.ContextID]time.Time, len(runtime.pendingTerminalFocus))
		for id, observedAt := range runtime.pendingTerminalFocus {
			index, resolveErr := sessionstate.ResolveContext(registry, string(id))
			if resolveErr != nil {
				if errors.Is(resolveErr, sessionstate.ErrContextNotFound) {
					processed[id] = struct{}{}
					continue
				}
				return fmt.Errorf("resolve terminal focus activity: %w", resolveErr)
			}
			current := registry.Contexts[index]
			if current.Launcher.Kind != sessionstate.LauncherHerdr || current.Launcher.Terminal == nil {
				processed[id] = struct{}{}
				continue
			}
			eligible[id] = observedAt
		}
		if err := sessionstate.RecordTerminalFocusBatchContext(writeContext, runtime.root, eligible); err != nil {
			return err
		}
		for id := range eligible {
			processed[id] = struct{}{}
		}
		return nil
	})
	if err == nil {
		for id := range processed {
			delete(runtime.pendingTerminalFocus, id)
		}
		if len(runtime.pendingTerminalFocus) == 0 {
			runtime.terminalFocusDeadline = time.Time{}
			runtime.terminalFocusRetry = 0
			runtime.terminalFocusReported = time.Time{}
			return nil
		}
		runtime.terminalFocusDeadline = now.Add(terminalFocusBatchDelay)
		return nil
	}
	runtime.scheduleTerminalFocusRetry(now)
	if (errors.Is(err, context.DeadlineExceeded) || sessionstate.IsStateDatabaseBusy(err)) && runtime.context().Err() == nil {
		return nil
	}
	if runtime.terminalFocusReported.IsZero() || now.Sub(runtime.terminalFocusReported) >= terminalFocusReportEvery {
		runtime.terminalFocusReported = now
		return fmt.Errorf("persist terminal focus activity: %w", err)
	}
	return nil
}

func (runtime *sessionRuntime) scheduleTerminalFocusRetry(now time.Time) {
	delay := runtime.terminalFocusRetry
	if delay <= 0 {
		delay = terminalFocusBatchDelay
	} else {
		delay = min(delay*2, terminalFocusRetryMaximum)
	}
	runtime.terminalFocusRetry = delay
	runtime.terminalFocusDeadline = now.Add(delay)
}

func (runtime *sessionRuntime) Shutdown() {
	if runtime == nil {
		return
	}
	runtime.shutdown = true
	runtime.startupDeadline = time.Time{}
	runtime.observeDeadline = time.Time{}
	runtime.terminalFocusDeadline = time.Time{}
	runtime.terminalFocusRetry = 0
	runtime.terminalFocusReported = time.Time{}
	clear(runtime.pendingTerminalFocus)
	clear(runtime.observedTerminals)
	clear(runtime.pendingTerminalClose)
	runtime.terminalCloseDeadline = time.Time{}
	runtime.terminalCloseRetry = 0
	runtime.terminalCloseRetryDeadline = time.Time{}
	runtime.terminalCloseBatchCursor = 0
	runtime.terminalCloseContinuation = time.Time{}
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
		ctx := runtime.context()
		root, err := requestTree(ctx, client)
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
