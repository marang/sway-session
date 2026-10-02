package main

import (
	"context"
	"errors"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func TestFollowApplicationHealthyCloseRequiresFreshAbsence(t *testing.T) {
	for _, reappeared := range []bool{false, true} {
		t.Run(map[bool]string{false: "still absent", true: "reappeared"}[reappeared], func(t *testing.T) {
			runtime, requester, launcher, _, app, start := guardedApplicationRuntime(t)
			present := applicationCloseTree(t, app)
			absent := daemonTree("98: apps")
			reconcileApplicationClose(t, runtime, requester, present, start)
			reconcileApplicationClose(t, runtime, requester, absent, start.Add(time.Second))
			requester.tree = absent
			if reappeared {
				requester.tree = present
			}
			reads := 0
			requester.beforeTree = func() { reads++ }
			if _, err := runtime.Reconcile(absent, start.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			assertApplicationDesiredOpen(t, runtime.root, app.ID, reappeared)
			if reads != 1 || launcher.starts != 0 {
				t.Fatalf("close confirmation: trees=%d launches=%d", reads, launcher.starts)
			}
		})
	}
}

func TestFollowApplicationUncertainLifecycleRequiresNewHealthyPresence(t *testing.T) {
	for _, uncertainty := range []string{"missing guard", "monitor failure", "unsafe generation", "unseen generation", "disconnect", "synchronous disconnect", "shutdown event"} {
		t.Run(uncertainty, func(t *testing.T) {
			runtime, requester, launcher, guard, app, start := guardedApplicationRuntime(t)
			present, absent := applicationCloseTree(t, app), daemonTree("98: apps")
			reconcileApplicationClose(t, runtime, requester, present, start)
			reconcileApplicationClose(t, runtime, requester, absent, start.Add(time.Second))
			var stream *testApplicationEventStream
			switch uncertainty {
			case "missing guard":
				runtime.terminalCloseGuard = nil
			case "monitor failure", "unsafe generation":
				guard.safe = false
				guard.generation++
			case "unseen generation":
				// Shutdown and cancellation both happened between observations.
				guard.generation += 2
			case "disconnect":
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "disconnected"}, start.Add(2*time.Second))
			case "synchronous disconnect":
				stream = &testApplicationEventStream{epoch: 2, connected: false}
				runtime.eventStreamState = stream
			case "shutdown event":
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventShutdown}, start.Add(2*time.Second))
			}
			requester.tree = absent
			_, err := runtime.Reconcile(absent, start.Add(time.Minute))
			if uncertainty == "synchronous disconnect" {
				if err == nil {
					t.Fatal("stale event stream was accepted")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			assertApplicationDesiredOpen(t, runtime.root, app.ID, true)
			// Seeing presence while unsafe must not arm a historical close.
			if stream == nil {
				reconcileApplicationClose(t, runtime, requester, present, start.Add(2*time.Minute))
			}
			runtime.terminalCloseGuard = guard
			guard.safe = true
			guard.generation++
			if stream != nil {
				stream.epoch, stream.connected = 3, true
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 3}, start.Add(3*time.Minute))
			for _, elapsed := range []time.Duration{3 * time.Minute, 4 * time.Minute} {
				reconcileApplicationClose(t, runtime, requester, absent, start.Add(elapsed))
				assertApplicationDesiredOpen(t, runtime.root, app.ID, true)
			}
			reconcileApplicationClose(t, runtime, requester, present, start.Add(5*time.Minute))
			reconcileApplicationClose(t, runtime, requester, absent, start.Add(5*time.Minute+time.Second))
			assertApplicationDesiredOpen(t, runtime.root, app.ID, true)
			reconcileApplicationClose(t, runtime, requester, absent, start.Add(5*time.Minute+3*time.Second))
			assertApplicationDesiredOpen(t, runtime.root, app.ID, false)
			if launcher.starts != 0 || len(requester.commands) != 0 {
				t.Fatalf("uncertainty triggered effects: starts=%d commands=%v", launcher.starts, requester.commands)
			}
		})
	}
}

func TestFollowApplicationGuardChangesAtFinalRegistryMutation(t *testing.T) {
	for _, change := range []string{"unsafe", "new safe generation", "stream"} {
		t.Run(change, func(t *testing.T) {
			runtime, requester, launcher, guard, app, start := guardedApplicationRuntime(t)
			present, absent := applicationCloseTree(t, app), daemonTree("98: apps")
			reconcileApplicationClose(t, runtime, requester, present, start)
			reconcileApplicationClose(t, runtime, requester, absent, start.Add(time.Second))
			stream := &testApplicationEventStream{epoch: 1, connected: true}
			runtime.eventStreamState = stream
			// The fresh tree succeeds with the original generation. Invalidate
			// only the final memory check after that observation was accepted.
			requester.beforeTree = func() {
				checks := 0
				runtime.terminalCloseGuard = applicationCloseGuardFunc(func() (uint64, bool) {
					checks++
					if checks >= 2 {
						switch change {
						case "unsafe":
							return guard.generation + 1, false
						case "new safe generation":
							return guard.generation + 2, true
						case "stream":
							stream.epoch, stream.connected = 2, false
						}
					}
					return guard.Snapshot()
				})
			}
			requester.tree = absent
			if _, err := runtime.Reconcile(absent, start.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			assertApplicationDesiredOpen(t, runtime.root, app.ID, true)
			if launcher.starts != 0 {
				t.Fatal("discarded close triggered a launch")
			}
		})
	}
}

func TestFollowApplicationFailedFreshObservationPreservesIntent(t *testing.T) {
	runtime, requester, _, _, app, start := guardedApplicationRuntime(t)
	present, absent := applicationCloseTree(t, app), daemonTree("98: apps")
	reconcileApplicationClose(t, runtime, requester, present, start)
	reconcileApplicationClose(t, runtime, requester, absent, start.Add(time.Second))
	runtime.client = applicationCloseFailedRequester{requester}
	if _, err := runtime.Reconcile(absent, start.Add(3*time.Second)); err == nil {
		t.Fatal("fresh observation failure was hidden")
	}
	assertApplicationDesiredOpen(t, runtime.root, app.ID, true)
	runtime.client = requester
	reconcileApplicationClose(t, runtime, requester, absent, start.Add(time.Minute))
	assertApplicationDesiredOpen(t, runtime.root, app.ID, true)
}

func TestFollowApplicationOuterTreeFailureInvalidatesCloseEvidence(t *testing.T) {
	runtime, requester, _, _, app, start := guardedApplicationRuntime(t)
	absent := daemonTree("98: apps")
	reconcileApplicationClose(t, runtime, requester, applicationCloseTree(t, app), start)
	reconcileApplicationClose(t, runtime, requester, absent, start.Add(time.Second))
	reconcilePersistentSession(applicationCloseFailedRequester{requester}, runtime, nil)
	reconcileApplicationClose(t, runtime, requester, absent, start.Add(time.Minute))
	assertApplicationDesiredOpen(t, runtime.root, app.ID, true)
}

func TestFollowApplicationConcurrentPolicyMutationWins(t *testing.T) {
	for _, mutation := range []string{"archive", "pinned", "identity", "reservation"} {
		t.Run(mutation, func(t *testing.T) {
			runtime, requester, _, _, app, start := guardedApplicationRuntime(t)
			reconcileApplicationClose(t, runtime, requester, applicationCloseTree(t, app), start)
			reconcileApplicationClose(t, runtime, requester, daemonTree("98: apps"), start.Add(time.Second))
			groups, err := sessionstate.ObserveApplicationGroups(daemonTree("98: apps"), runtime.registry)
			if err != nil {
				t.Fatal(err)
			}
			plan, generation, err := runtime.planApplicationObservation(groups, start.Add(3*time.Second), runtime.automaticCloseObservation())
			if err != nil || len(plan.DesiredOpen) != 1 || plan.DesiredOpen[0].Open {
				t.Fatalf("expected ready close candidate: %+v %v", plan, err)
			}
			if mutation == "reservation" {
				runtime.lifecycleOperations = &fixtureLifecycleAdapter{blocked: []sessionstate.ContextID{app.ID}}
			} else {
				_, err = sessionstate.UpdateRegistryContext(t.Context(), runtime.root, func(current *sessionstate.Registry) error {
					switch mutation {
					case "archive":
						_, err := sessionstate.SetContextStateAt(current, string(app.ID), sessionstate.ContextArchived, start.Add(2*time.Second))
						return err
					case "pinned":
						current.Contexts[0].App.RestorePolicy = sessionstate.ApplicationRestorePinned
					case "identity":
						current.Contexts[0].App.Identity.WaylandAppID = "org.example.Rebound"
					}
					return current.Validate()
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			requester.beforeTree = func() { t.Fatal("obsolete close candidate requested a tree") }
			if _, err := runtime.persistApplicationDesiredOpen(runtime.registry, plan, generation, start.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			assertApplicationDesiredOpen(t, runtime.root, app.ID, true)
		})
	}
}

func TestApplicationCloseNeverInventsStartupCloseOrChangesPinned(t *testing.T) {
	for _, policy := range []sessionstate.ApplicationRestorePolicy{sessionstate.ApplicationRestoreFollow, sessionstate.ApplicationRestorePinned} {
		t.Run(string(policy), func(t *testing.T) {
			runtime, requester, launcher, guard, app, start := guardedApplicationRuntime(t)
			_, err := sessionstate.UpdateRegistryContext(t.Context(), runtime.root, func(current *sessionstate.Registry) error {
				current.Contexts[0].App.RestorePolicy = policy
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if policy == sessionstate.ApplicationRestorePinned {
				reconcileApplicationClose(t, runtime, requester, applicationCloseTree(t, app), start)
			}
			// Initial absence under a healthy guard is not a close. A desired
			// startup app may still use its existing one launch opportunity.
			for _, elapsed := range []time.Duration{time.Second, 4 * time.Second, time.Minute} {
				requester.tree = daemonTree("98: apps")
				if _, err := runtime.Reconcile(requester.tree, start.Add(elapsed)); err != nil {
					t.Fatal(err)
				}
				assertApplicationDesiredOpen(t, runtime.root, app.ID, true)
			}
			if policy == sessionstate.ApplicationRestorePinned && launcher.starts != 0 {
				t.Fatal("pinned application was relaunched after observed close")
			}
			guard.safe = false
			_, err = sessionstate.UpdateRegistryContext(t.Context(), runtime.root, func(current *sessionstate.Registry) error {
				_, err := sessionstate.SetContextStateAt(current, string(app.ID), sessionstate.ContextArchived, start.Add(2*time.Minute))
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			reconcileApplicationClose(t, runtime, requester, applicationCloseTree(t, app), start.Add(3*time.Minute))
			stored, err := sessionstate.ReadRegistrySnapshotContext(t.Context(), runtime.root)
			if err != nil || stored.Contexts[0].State != sessionstate.ContextArchived {
				t.Fatalf("explicit archive was overridden: %+v %v", stored, err)
			}
		})
	}
}

type applicationCloseGuardFunc func() (uint64, bool)

func (guard applicationCloseGuardFunc) Snapshot() (uint64, bool) { return guard() }

type testApplicationEventStream struct {
	epoch     uint64
	connected bool
}

func (stream *testApplicationEventStream) Snapshot() (uint64, bool) {
	return stream.epoch, stream.connected
}

type applicationCloseFailedRequester struct{ *lifecycleFixtureRequester }

func (requester applicationCloseFailedRequester) RequestContext(ctx context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if kind == swayipc.GetTree {
		return swayipc.Message{}, errors.New("compositor disconnected")
	}
	return requester.lifecycleFixtureRequester.RequestContext(ctx, kind, payload)
}

func TestFollowApplicationShutdownPreservesDesiredOpen(t *testing.T) {
	runtime, requester, launcher, guard, app, start := guardedApplicationRuntime(t)
	reconcileApplicationClose(t, runtime, requester, applicationCloseTree(t, app), start)
	guard.generation++
	guard.safe = false
	for _, elapsed := range []time.Duration{time.Second, 4 * time.Second, time.Minute} {
		reconcileApplicationClose(t, runtime, requester, daemonTree("98: apps"), start.Add(elapsed))
		assertApplicationDesiredOpen(t, runtime.root, app.ID, true)
	}
	if launcher.starts != 0 || len(requester.commands) != 0 {
		t.Fatalf("shutdown observation caused effects: starts=%d commands=%v", launcher.starts, requester.commands)
	}
}

func guardedApplicationRuntime(t *testing.T) (*sessionRuntime, *lifecycleFixtureRequester, *recordingApplicationLauncher, *testTerminalCloseGuard, sessionstate.Context, time.Time) {
	t.Helper()
	runtime, _, launcher, app, start := testApplicationRuntime(t)
	guard := &testTerminalCloseGuard{generation: 7, safe: true}
	runtime.terminalCloseGuard = guard
	runtime.startupComplete = true
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 1}, start)
	requester := &lifecycleFixtureRequester{tree: daemonTree("98: apps")}
	runtime.client = requester
	return runtime, requester, launcher, guard, app, start
}

func applicationCloseTree(t *testing.T, app sessionstate.Context) *Node {
	t.Helper()
	appID, sandbox := app.App.Identity.WaylandAppID, app.App.Identity.SandboxAppID
	mark, err := app.ID.Mark()
	if err != nil {
		t.Fatal(err)
	}
	return daemonTree("98: apps", &Node{ID: 41, Type: "con", AppID: &appID, SandboxAppID: &sandbox, Marks: []string{mark}})
}

func reconcileApplicationClose(t *testing.T, runtime *sessionRuntime, requester *lifecycleFixtureRequester, tree *Node, now time.Time) {
	t.Helper()
	requester.tree = tree
	if refresh, err := runtime.Reconcile(tree, now); err != nil || refresh {
		t.Fatalf("reconcile application lifecycle: refresh=%v err=%v", refresh, err)
	}
}

func assertApplicationDesiredOpen(t *testing.T, root string, id sessionstate.ContextID, want bool) {
	t.Helper()
	registry, err := sessionstate.ReadRegistrySnapshotContext(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range registry.Contexts {
		if app.ID == id && app.App != nil {
			if app.App.DesiredOpen != want {
				t.Fatalf("persisted desired-open = %v, want %v", app.App.DesiredOpen, want)
			}
			return
		}
	}
	t.Fatalf("application %s missing from registry", id)
}
