package main

import (
	"context"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

type testApplicationGenerationBoundary struct {
	*fixtureLifecycleAdapter
	reads    int
	interval func()
}

func (adapter *testApplicationGenerationBoundary) BlockedContextIDs(ctx context.Context) ([]sessionstate.ContextID, error) {
	adapter.reads++
	if adapter.reads == 2 {
		// This reservation refresh occurs under the registry lock between the
		// two application planning passes. Keep Snapshot itself pure memory.
		adapter.interval()
	}
	return adapter.fixtureLifecycleAdapter.BlockedContextIDs(ctx)
}

func TestFollowApplicationOldTreeCannotRearmClose(t *testing.T) {
	for _, mode := range []string{"unseen_sleep_resume", "presence_seen_unsafe"} {
		t.Run(mode, func(t *testing.T) {
			runtime, requester, launcher, guard, app, start := guardedApplicationRuntime(t)
			present, absent := applicationCloseTree(t, app), daemonTree("98: apps")
			reconcileApplicationClose(t, runtime, requester, present, start)
			if mode == "presence_seen_unsafe" {
				guard.generation++
				guard.safe = false
			}
			adapter := &testApplicationGenerationBoundary{
				fixtureLifecycleAdapter: &fixtureLifecycleAdapter{},
				interval: func() {
					// The app dies in the unsafe interval, before rearm.
					requester.tree = absent
					if mode == "unseen_sleep_resume" {
						guard.generation += 2
					} else {
						guard.generation++
						guard.safe = true
					}
				},
			}
			runtime.lifecycleOperations = adapter
			reconcileApplicationClose(t, runtime, requester, present, start.Add(500*time.Millisecond))
			if adapter.reads < 2 || !guard.safe {
				t.Fatal("fixture did not cross a rearm interval between planners")
			}
			reconcileApplicationClose(t, runtime, requester, absent, start.Add(time.Second))
			reconcileApplicationClose(t, runtime, requester, absent, start.Add(3*time.Second))
			if launcher.starts != 0 || len(requester.commands) != 0 {
				t.Fatalf("unexpected effects: starts=%d commands=%v", launcher.starts, requester.commands)
			}
			assertApplicationDesiredOpen(t, runtime.root, app.ID, true)
		})
	}
}

func TestFollowApplicationGuardTransitionDuringTreeAcquisition(t *testing.T) {
	for _, unsafeStart := range []bool{false, true} {
		name := "sleep and resume during acquisition"
		if unsafeStart {
			name = "unsafe presence before rearm"
		}
		t.Run(name, func(t *testing.T) {
			runtime, requester, launcher, guard, app, start := guardedApplicationRuntime(t)
			present, absent := applicationCloseTree(t, app), daemonTree("98: apps")
			reconcileApplicationClose(t, runtime, requester, present, start)
			if unsafeStart {
				guard.generation++
				guard.safe = false
			}
			requester.beforeTree = func() {
				// The returned tree was captured before this generation boundary.
				guard.generation += 2
				guard.safe = true
				requester.beforeTree = nil
			}
			requester.tree = present
			reconcilePersistentSession(requester, runtime, func(err error) { t.Fatal(err) })
			for _, elapsed := range []time.Duration{time.Minute, 2 * time.Minute} {
				reconcileApplicationClose(t, runtime, requester, absent, start.Add(elapsed))
				assertApplicationDesiredOpen(t, runtime.root, app.ID, true)
			}
			if launcher.starts != 0 {
				t.Fatal("uncertain acquisition triggered a relaunch")
			}
		})
	}
}
