package main

import (
	"context"
	"errors"
	"reflect"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

func (runtime *sessionRuntime) applicationLaunchStreamCurrent(epoch uint64) error {
	if err := runtime.context().Err(); err != nil {
		return err
	}
	if runtime.shutdown || runtime.eventStreamState == nil || !runtime.eventStreamReady || runtime.eventStreamEpoch != epoch {
		return errors.New("desktop application launch deferred: Sway event stream is unavailable or changed")
	}
	return runtime.requireCurrentEventStream()
}

// Called under the lifecycle/registry lock, outside any SQLite transaction.
// Each prepared candidate gets its own bounded observation: a preceding start
// can map another candidate, and preparation can block on external validation.
func (runtime *sessionRuntime) confirmApplicationLaunch(prepared sessionstate.Context, epoch uint64) (bool, bool, time.Time, error) {
	if err := runtime.applicationLaunchStreamCurrent(epoch); err != nil {
		return false, false, time.Time{}, err
	}
	observation := runtime.automaticCloseObservation()
	observationCtx, cancel := context.WithTimeout(runtime.context(), sessionObservationDelay)
	defer cancel()
	fresh, err := requestTree(observationCtx, runtime.client)
	if err != nil {
		return false, false, time.Time{}, err
	}
	if fresh.Type != "root" || fresh.ID <= 0 {
		return false, false, time.Time{}, errors.New("desktop application launch deferred: invalid Sway tree root")
	}
	if err := runtime.applicationLaunchStreamCurrent(epoch); err != nil {
		return false, false, time.Time{}, err
	}
	current, available, err := runtime.loadRegistry()
	if err != nil || !available {
		if err == nil {
			err = errors.New("persistent context registry disappeared before application launch")
		}
		return false, false, time.Time{}, err
	}
	if _, err := runtime.lifecyclePlanningRegistry(current); err != nil {
		return false, false, time.Time{}, err
	}
	groups, err := sessionstate.ObserveApplicationGroups(fresh, current)
	if err != nil {
		return false, false, time.Time{}, err
	}
	launchNow := runtime.now()
	plan, _, err := runtime.planApplicationObservation(groups, launchNow, observation)
	if err != nil {
		return false, false, time.Time{}, err
	}
	present := len(groups[prepared.ID].Windows) != 0
	// Presence, ambiguity, policy changes, lifecycle reservations, adoption,
	// attempts and current concurrency all use the coordinator's normal rules.
	eligible := false
	if plan.LaunchSlots > 0 {
		for _, candidate := range plan.Launch {
			if candidate.ID == prepared.ID && reflect.DeepEqual(candidate, prepared) {
				eligible = true
				break
			}
		}
	}
	if err := runtime.applicationLaunchStreamCurrent(epoch); err != nil {
		return false, false, time.Time{}, err
	}
	return eligible, present, launchNow, nil
}
