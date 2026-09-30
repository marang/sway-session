package main

import (
	"context"
	"os"
	"sync"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

const terminalObservationTimeout = 3 * time.Second

// Inventory is loaded before external observations. Both read-only boundaries
// share one deadline; failure keeps saved metadata and explicit unknown states.
func observeTerminalInventory(ctx context.Context, deps dependencies, registry sessionstate.Registry, items []terminalInventoryResult) {
	if len(items) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, terminalObservationTimeout)
	defer cancel()
	operations := commandTerminalManageOperations{deps: deps}
	var windows terminalManageSnapshot
	var activity map[sessionstate.ContextID]sessionstate.TerminalSessionObservation
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		windows = operations.observeWindows(ctx, os.Getenv("SWAYSOCK"), registry, items)
	}()
	go func() {
		defer workers.Done()
		activity = operations.ObserveActivity(ctx, items)
	}()
	workers.Wait()
	now := time.Now().UTC()
	if deps.now != nil {
		now = deps.now().UTC()
	}
	for index := range items {
		item := &items[index]
		item.WindowPresence = windows.windows[item.ContextID].String()
		if item.WindowPresence == "unknown" {
			item.WindowReason = "window_observation_unavailable"
		} else {
			observed := now
			item.WindowObservedAt = &observed
		}
		item.Activity = activity[item.ContextID]
	}
}
