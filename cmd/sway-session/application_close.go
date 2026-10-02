package main

import (
	"context"
	"fmt"
	"reflect"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

func (runtime *sessionRuntime) resetApplicationCloseObservations() {
	if runtime != nil && runtime.applications != nil {
		runtime.applications.ResetCloseObservations()
		runtime.applicationCloseGeneration = automaticCloseGeneration{}
	}
}

func (runtime *sessionRuntime) planApplicationObservation(groups map[sessionstate.ContextID]sessionstate.ApplicationGroup, now time.Time, observation automaticCloseObservation) (sessionstate.ApplicationRestorePlan, automaticCloseGeneration, error) {
	generation, safe := runtime.automaticCloseSnapshot()
	safe = safe && observation.safe && generation == observation.generation
	if !safe || generation != runtime.applicationCloseGeneration {
		runtime.resetApplicationCloseObservations()
	}
	runtime.applicationCloseGeneration = generation
	plan, err := runtime.applications.PlanWithCloseTracking(runtime.registry, groups, now, runtime.lifecycleBlocked, safe)
	if err == nil {
		err = runtime.persistApplicationObservations()
	}
	return plan, observation.generation, err
}

// Persist observation evidence before placement, policy changes or launches.
// Keep an uncommitted candidate in memory so a later fresh pass can retry it
// even if the observed window has disappeared in the meantime.
func (runtime *sessionRuntime) persistApplicationObservations() error {
	candidate := runtime.applications.State()
	if reflect.DeepEqual(candidate, runtime.applicationPersistedState) {
		return nil
	}
	if err := sessionstate.ApplicationSessionStoreFor(runtime.root).SaveContext(runtime.context(), candidate); err != nil {
		return fmt.Errorf("persist desktop application observation: %w", err)
	}
	runtime.applicationPersistedState = candidate
	return nil
}

func (runtime *sessionRuntime) applicationCloseStillSafe(expected automaticCloseGeneration) bool {
	generation, safe := runtime.automaticCloseSnapshot()
	if !safe || generation != expected {
		runtime.resetApplicationCloseObservations()
		return false
	}
	return true
}

func (runtime *sessionRuntime) persistApplicationDesiredOpen(registry sessionstate.Registry, plan sessionstate.ApplicationRestorePlan, generation automaticCloseGeneration, now time.Time) (sessionstate.Registry, error) {
	type desiredChange struct {
		open     bool
		observed sessionstate.Context
	}
	changes := make(map[sessionstate.ContextID]desiredChange, len(plan.DesiredOpen))
	for _, change := range plan.DesiredOpen {
		for _, item := range registry.Contexts {
			if item.ID == change.ContextID && item.App != nil {
				changes[item.ID] = desiredChange{open: change.Open, observed: item}
				break
			}
		}
	}
	return sessionstate.UpdateRegistryContext(runtime.context(), runtime.root, func(current *sessionstate.Registry) error {
		// Mutation callbacks hold the registry lock, but run before SQLite's
		// short write transaction. Recheck reservations and fetch at most one
		// fresh tree for the whole close batch here, never inside a transaction.
		if err := runtime.refreshLifecycleBlocked(); err != nil {
			runtime.resetApplicationCloseObservations()
			return err
		}
		needsAbsence := false
		for _, item := range current.Contexts {
			change, exists := changes[item.ID]
			_, blocked := runtime.lifecycleBlocked[item.ID]
			if exists && !blocked && !change.open && reflect.DeepEqual(item, change.observed) {
				needsAbsence = true
			}
		}
		confirmed := make(map[sessionstate.ContextID]bool)
		if needsAbsence && runtime.applicationCloseStillSafe(generation) {
			observationCtx, cancel := context.WithTimeout(runtime.context(), terminalCloseWriteTimeout)
			defer cancel()
			fresh, err := requestTree(observationCtx, runtime.client)
			if err != nil {
				runtime.resetApplicationCloseObservations()
				return fmt.Errorf("confirm Follow application close: %w", err)
			}
			groups, err := sessionstate.ObserveApplicationGroups(fresh, *current)
			if err != nil {
				runtime.resetApplicationCloseObservations()
				return err
			}
			if runtime.applicationCloseStillSafe(generation) {
				freshPlan, err := runtime.applications.PlanWithCloseTracking(*current, groups, now, runtime.lifecycleBlocked, true)
				if err != nil {
					return err
				}
				if err := runtime.persistApplicationObservations(); err != nil {
					return err
				}
				for _, change := range freshPlan.DesiredOpen {
					if !change.Open {
						confirmed[change.ContextID] = true
					}
				}
			}
		}
		// Only pure memory reads remain between this final guard check and the
		// mutation. Concurrent archive, policy, launcher or identity changes win.
		closeSafe := runtime.applicationCloseStillSafe(generation)
		for index, item := range current.Contexts {
			change, exists := changes[item.ID]
			_, blocked := runtime.lifecycleBlocked[item.ID]
			if !exists || blocked || item.State != sessionstate.ContextActive || !reflect.DeepEqual(item, change.observed) {
				continue
			}
			if !change.open && (!closeSafe || !confirmed[item.ID] || item.App.RestorePolicy != sessionstate.ApplicationRestoreFollow) {
				continue
			}
			current.Contexts[index].App.DesiredOpen = change.open
		}
		return current.Validate()
	})
}
