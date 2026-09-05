package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

type terminalCloseObservation struct {
	containerID int64
	contextID   sessionstate.ContextID
	identity    string
	context     sessionstate.Context
	epoch       uint64
}

type terminalCloseCandidate struct {
	terminalCloseObservation
	deadline         time.Time
	guardGeneration  uint64
	absenceConfirmed bool
}

var errTerminalCloseDiscarded = errors.New("terminal close candidate discarded")

func (runtime *sessionRuntime) queueTerminalClose(node *Node, observedAt time.Time) {
	if runtime == nil || runtime.shutdown || node == nil || node.ID <= 0 || observedAt.IsZero() ||
		runtime.terminalCloseGuard == nil || !runtime.terminalCloseStreamCurrent() {
		return
	}
	contextID, ok := focusedManagedContextID(node)
	if !ok {
		return
	}
	observation, ok := runtime.observedTerminals[node.ID]
	if !ok || observation.contextID != contextID || observation.epoch != runtime.eventStreamEpoch {
		return
	}
	generation, safe := runtime.terminalCloseGuard.Snapshot()
	if !safe {
		return
	}
	if runtime.pendingTerminalClose == nil {
		runtime.pendingTerminalClose = make(map[int64]terminalCloseCandidate)
	}
	candidate := terminalCloseCandidate{
		terminalCloseObservation: observation,
		deadline:                 observedAt.Add(terminalCloseGrace),
		guardGeneration:          generation,
	}
	runtime.pendingTerminalClose[node.ID] = candidate
	runtime.rearmTerminalCloseDeadline()
}

func (runtime *sessionRuntime) terminalCloseStreamCurrent() bool {
	if runtime == nil || !runtime.eventStreamReady {
		return false
	}
	if runtime.eventStreamState == nil {
		return true
	}
	epoch, connected := runtime.eventStreamState.Snapshot()
	return connected && epoch == runtime.eventStreamEpoch
}

// observeTerminalCloseState re-observes candidates from a complete tree. It
// never infers a close from startup absence: only a prior close event creates a
// candidate, and a tree seen before its grace deadline is deliberately ignored.
func (runtime *sessionRuntime) observeTerminalCloseState(root *Node, registry sessionstate.Registry, now time.Time) error {
	if runtime == nil {
		return nil
	}
	if root == nil || root.ID <= 0 || root.Type != "root" {
		return errors.New("Sway tree response has no valid root")
	}
	observed, issues, err := observedActiveHerdrTerminals(root, registry, runtime.eventStreamEpoch)
	if err != nil {
		return fmt.Errorf("observe managed terminal windows: %w", err)
	}
	if runtime.observedTerminals == nil {
		runtime.observedTerminals = make(map[int64]terminalCloseObservation)
	}
	// A close or focus event can be queued behind a reconciliation that already
	// sees its window absent. Keep a prior same-epoch proof until that event is
	// consumed; otherwise a close burst would silently lose later candidates.
	for containerID, previous := range runtime.observedTerminals {
		if previous.epoch != runtime.eventStreamEpoch || !terminalCloseObservationStillEligible(previous, registry) {
			delete(runtime.observedTerminals, containerID)
		}
	}
	for contextID := range issues {
		removeTerminalCloseObservations(runtime.observedTerminals, contextID)
	}
	for _, current := range observed {
		removeTerminalCloseObservations(runtime.observedTerminals, current.contextID)
		runtime.observedTerminals[current.containerID] = current
	}
	if len(runtime.pendingTerminalClose) == 0 {
		return nil
	}
	for containerID, candidate := range runtime.pendingTerminalClose {
		if runtime.context().Err() != nil || !runtime.terminalCloseStreamCurrent() || candidate.epoch != runtime.eventStreamEpoch {
			delete(runtime.pendingTerminalClose, containerID)
			continue
		}
		if current := findContainerByID(root, containerID); current != nil {
			delete(runtime.pendingTerminalClose, containerID)
			continue
		}
		if _, issue := issues[candidate.contextID]; issue {
			delete(runtime.pendingTerminalClose, containerID)
			continue
		}
		if _, present := observedByContext(observed, candidate.contextID); present {
			// One re-opened match is enough to disqualify the close. This also
			// treats duplicate matches as ambiguous instead of picking one.
			delete(runtime.pendingTerminalClose, containerID)
			continue
		}
		if candidate, ok := runtime.pendingTerminalClose[containerID]; ok && !now.Before(candidate.deadline) {
			candidate.absenceConfirmed = true
			runtime.pendingTerminalClose[containerID] = candidate
		}
	}
	runtime.rearmTerminalCloseDeadline()
	return nil
}

func terminalCloseObservationStillEligible(observation terminalCloseObservation, registry sessionstate.Registry) bool {
	index, err := sessionstate.ResolveContext(registry, string(observation.contextID))
	if err != nil {
		return false
	}
	current := registry.Contexts[index]
	return activeHerdrTerminal(current) && terminalCloseIdentity(current) == observation.identity && reflect.DeepEqual(current, observation.context)
}

func removeTerminalCloseObservations(observed map[int64]terminalCloseObservation, contextID sessionstate.ContextID) {
	for containerID, observation := range observed {
		if observation.contextID == contextID {
			delete(observed, containerID)
		}
	}
}

func observedActiveHerdrTerminals(root *Node, registry sessionstate.Registry, epoch uint64) (map[int64]terminalCloseObservation, map[sessionstate.ContextID]struct{}, error) {
	windows, observationIssues, err := sessionstate.ObserveManagedWindowsIsolated(root, registry)
	if err != nil {
		return nil, nil, err
	}
	observed := make(map[int64]terminalCloseObservation)
	for id, window := range windows {
		index, err := sessionstate.ResolveContext(registry, string(id))
		if err != nil {
			continue
		}
		contextValue := registry.Contexts[index]
		if !activeHerdrTerminal(contextValue) {
			continue
		}
		observed[window.ContainerID] = terminalCloseObservation{
			containerID: window.ContainerID,
			contextID:   id,
			identity:    terminalCloseIdentity(contextValue),
			context:     contextValue,
			epoch:       epoch,
		}
	}
	issues := make(map[sessionstate.ContextID]struct{}, len(observationIssues))
	for _, issue := range observationIssues {
		issues[issue.ContextID] = struct{}{}
	}
	return observed, issues, nil
}

func observedByContext(observed map[int64]terminalCloseObservation, id sessionstate.ContextID) (terminalCloseObservation, bool) {
	for _, current := range observed {
		if current.contextID == id {
			return current, true
		}
	}
	return terminalCloseObservation{}, false
}

func activeHerdrTerminal(contextValue sessionstate.Context) bool {
	return contextValue.State == sessionstate.ContextActive &&
		contextValue.Launcher.Kind == sessionstate.LauncherHerdr && contextValue.Launcher.Terminal != nil
}

func terminalCloseIdentity(contextValue sessionstate.Context) string {
	terminal := contextValue.Launcher.Terminal
	if terminal == nil {
		return ""
	}
	identity := "default"
	if terminal.Identity != nil {
		identity = terminal.Identity.String()
	}
	return string(terminal.Adapter) + "\x00" + identity + "\x00" + contextValue.Launcher.Session
}

func (runtime *sessionRuntime) rearmTerminalCloseDeadline() {
	if runtime == nil {
		return
	}
	if len(runtime.pendingTerminalClose) == 0 {
		runtime.terminalCloseDeadline = time.Time{}
		runtime.terminalCloseRetry = 0
		runtime.terminalCloseRetryDeadline = time.Time{}
		runtime.terminalCloseBatchCursor = 0
		runtime.terminalCloseContinuation = time.Time{}
		return
	}
	runtime.terminalCloseDeadline = time.Time{}
	for _, candidate := range runtime.pendingTerminalClose {
		deadline := candidate.deadline
		if runtime.terminalCloseRetryDeadline.After(deadline) {
			deadline = runtime.terminalCloseRetryDeadline
		}
		if deadline.IsZero() {
			continue
		}
		if runtime.terminalCloseDeadline.IsZero() || deadline.Before(runtime.terminalCloseDeadline) {
			runtime.terminalCloseDeadline = deadline
		}
	}
	if !runtime.terminalCloseContinuation.IsZero() &&
		(runtime.terminalCloseDeadline.IsZero() || runtime.terminalCloseContinuation.Before(runtime.terminalCloseDeadline)) {
		runtime.terminalCloseDeadline = runtime.terminalCloseContinuation
	}
}

func (runtime *sessionRuntime) terminalCloseDue(now time.Time) bool {
	if !runtime.terminalCloseRetryDeadline.IsZero() && now.Before(runtime.terminalCloseRetryDeadline) {
		return false
	}
	for _, candidate := range runtime.pendingTerminalClose {
		if !now.Before(candidate.deadline) {
			return true
		}
	}
	return false
}

func (runtime *sessionRuntime) terminalCloseBatch(now time.Time) []int64 {
	containerIDs := make([]int64, 0, maxTerminalCloseBatch)
	for containerID, candidate := range runtime.pendingTerminalClose {
		if candidate.absenceConfirmed && !now.Before(candidate.deadline) {
			containerIDs = append(containerIDs, containerID)
		}
	}
	if len(containerIDs) == 0 {
		return nil
	}
	sort.Slice(containerIDs, func(left, right int) bool { return containerIDs[left] < containerIDs[right] })
	start := sort.Search(len(containerIDs), func(index int) bool {
		return containerIDs[index] > runtime.terminalCloseBatchCursor
	})
	if start == len(containerIDs) {
		start = 0
	}
	batchSize := min(len(containerIDs), maxTerminalCloseBatch)
	batch := make([]int64, 0, batchSize)
	for offset := range batchSize {
		batch = append(batch, containerIDs[(start+offset)%len(containerIDs)])
	}
	return batch
}

func (runtime *sessionRuntime) hasDueTerminalClose(now time.Time) bool {
	for _, candidate := range runtime.pendingTerminalClose {
		if candidate.absenceConfirmed && !now.Before(candidate.deadline) {
			return true
		}
	}
	return false
}

func (runtime *sessionRuntime) flushTerminalClose(now time.Time) error {
	if runtime == nil || len(runtime.pendingTerminalClose) == 0 || !runtime.terminalCloseDue(now) {
		return nil
	}
	if runtime.context().Err() != nil || !runtime.terminalCloseStreamCurrent() || runtime.terminalCloseGuard == nil {
		clear(runtime.pendingTerminalClose)
		runtime.rearmTerminalCloseDeadline()
		return nil
	}

	writeContext, cancel := context.WithTimeout(runtime.context(), terminalCloseWriteTimeout)
	defer cancel()
	archived := make(map[int64]struct{})
	discarded := make(map[int64]struct{})
	var batch []int64
	err := sessionstate.WithTerminalLifecycleLockContext(writeContext, runtime.root, func() error {
		// This is the final, lock-serialized absence confirmation. It remains
		// outside SQLite work: the lifecycle lock protects terminal effects,
		// while the short registry update transaction starts only below.
		root, treeErr := requestTree(writeContext, runtime.client)
		if treeErr != nil {
			return fmt.Errorf("confirm terminal close with Sway tree: %w", treeErr)
		}
		if observeErr := runtime.observeTerminalCloseState(root, runtime.registry, now); observeErr != nil {
			return observeErr
		}
		if len(runtime.pendingTerminalClose) == 0 {
			return nil
		}
		batch = runtime.terminalCloseBatch(now)
		if len(batch) == 0 {
			return nil
		}
		updated, updateErr := sessionstate.UpdateRegistryContext(writeContext, runtime.root, func(registry *sessionstate.Registry) error {
			// The guard is re-read immediately before the state mutation. Its
			// Snapshot implementation is pure memory access, so this does not
			// introduce an external effect into the database write.
			generation, safe := runtime.terminalCloseGuard.Snapshot()
			if !safe || !runtime.terminalCloseStreamCurrent() {
				for containerID := range runtime.pendingTerminalClose {
					discarded[containerID] = struct{}{}
				}
				return errTerminalCloseDiscarded
			}
			for _, containerID := range batch {
				candidate, exists := runtime.pendingTerminalClose[containerID]
				if exists && generation != candidate.guardGeneration {
					for containerID := range runtime.pendingTerminalClose {
						discarded[containerID] = struct{}{}
					}
					return errTerminalCloseDiscarded
				}
			}
			changed := false
			for _, containerID := range batch {
				candidate, exists := runtime.pendingTerminalClose[containerID]
				if !exists {
					continue
				}
				index, resolveErr := sessionstate.ResolveContext(*registry, string(candidate.contextID))
				if resolveErr != nil || !reflect.DeepEqual(registry.Contexts[index], candidate.context) {
					discarded[containerID] = struct{}{}
					continue
				}
				if _, archiveErr := sessionstate.SetContextStateAt(registry, string(candidate.contextID), sessionstate.ContextArchived, now); archiveErr != nil {
					return fmt.Errorf("archive closed terminal %s: %w", candidate.contextID, archiveErr)
				}
				archived[containerID] = struct{}{}
				changed = true
			}
			if !changed {
				return errTerminalCloseDiscarded
			}
			return nil
		})
		if updateErr != nil {
			return updateErr
		}
		runtime.registry = updated
		runtime.registryCacheKnown = false
		return nil
	})
	if errors.Is(err, errTerminalCloseDiscarded) {
		err = nil
	}
	if err != nil {
		for containerID := range discarded {
			delete(runtime.pendingTerminalClose, containerID)
		}
		runtime.scheduleTerminalCloseRetry(now)
		if errors.Is(err, context.DeadlineExceeded) || sessionstate.IsStateDatabaseBusy(err) {
			return nil
		}
		return err
	}
	for containerID := range archived {
		delete(runtime.pendingTerminalClose, containerID)
	}
	for containerID := range discarded {
		delete(runtime.pendingTerminalClose, containerID)
	}
	if len(batch) != 0 {
		runtime.terminalCloseBatchCursor = batch[len(batch)-1]
	}
	// A successful fresh observation clears any prior transport/contention
	// backoff. Remaining candidates use their own grace deadlines or the
	// immediate bounded-batch continuation below.
	runtime.terminalCloseRetry = 0
	runtime.terminalCloseRetryDeadline = time.Time{}
	if runtime.hasDueTerminalClose(now) {
		runtime.terminalCloseContinuation = now
	} else {
		runtime.terminalCloseContinuation = time.Time{}
	}
	runtime.rearmTerminalCloseDeadline()
	return nil
}

func (runtime *sessionRuntime) scheduleTerminalCloseRetry(now time.Time) {
	if runtime == nil {
		return
	}
	runtime.terminalCloseContinuation = time.Time{}
	delay := runtime.terminalCloseRetry
	if delay <= 0 {
		delay = terminalFocusBatchDelay
	} else {
		delay = min(delay*2, terminalCloseRetryMaximum)
	}
	runtime.terminalCloseRetry = delay
	runtime.terminalCloseRetryDeadline = now.Add(delay)
	runtime.rearmTerminalCloseDeadline()
}
