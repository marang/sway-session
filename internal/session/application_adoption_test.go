package session

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAdoptedPinnedApplicationStaysClosedAfterDaemonRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	app := applicationContextWithID(testContextID, "org.example.Adopted")
	app.App.RestorePolicy = ApplicationRestorePinned
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{app}}
	if err := RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2000, 0).UTC()
	options := ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 1}
	id := strings.Repeat("a", 64)
	coordinator, err := NewApplicationRestoreCoordinator(id, ApplicationSessionState{}, now, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Plan(registry, map[ContextID]ApplicationGroup{app.ID: {Windows: []WindowApplication{{ContainerID: 98}}}}, now); err != nil {
		t.Fatal(err)
	}
	before, err := coordinator.Plan(registry, nil, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	store := ApplicationSessionStoreFor(root)
	if err := store.Save(coordinator.State()); err != nil {
		t.Fatal(err)
	}
	var loaded ApplicationSessionState
	if err := store.LoadInto(&loaded); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewApplicationRestoreCoordinator(id, loaded, now.Add(3*time.Second), options)
	if err != nil {
		t.Fatal(err)
	}
	after, err := restarted.Plan(registry, nil, now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Launch) != 0 || len(after.Launch) != 0 || len(loaded.Attempts) != 0 {
		t.Fatalf("daemon restart forgot satisfied startup opportunity: before=%d after=%d attempts=%d", len(before.Launch), len(after.Launch), len(loaded.Attempts))
	}
}

func TestPersistedAdoptionRespectsFollowRearmAndCompositorIdentity(t *testing.T) {
	app := applicationContextWithID(testContextID, "org.example.Adopted")
	now := time.Unix(2000, 0).UTC()
	state := ApplicationSessionState{Version: ApplicationSessionSchemaVersion, CompositorID: strings.Repeat("a", 64), Attempts: []ApplicationLaunchAttempt{}, Adoptions: []ApplicationAdoption{{ContextID: app.ID, ObservedAt: now}}}
	options := ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: time.Minute, MaxConcurrent: 1}
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{app}}
	for _, test := range []struct {
		name       string
		compositor string
		rearm      bool
		wantLaunch int
	}{
		{"same_compositor", state.CompositorID, false, 0},
		{"new_compositor", strings.Repeat("b", 64), false, 1},
		{"explicit_follow_rearm", state.CompositorID, true, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator, err := NewApplicationRestoreCoordinator(test.compositor, state, now, options)
			if err != nil {
				t.Fatal(err)
			}
			if test.rearm {
				closed := registry
				closed.Contexts = append([]Context(nil), registry.Contexts...)
				application := *app.App
				closed.Contexts[0].App = &application
				closed.Contexts[0].App.DesiredOpen = false
				if _, err := coordinator.Plan(closed, nil, now.Add(2*time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := coordinator.Plan(registry, nil, now.Add(3*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Launch) != test.wantLaunch {
				t.Fatalf("launch candidates %d, want %d", len(plan.Launch), test.wantLaunch)
			}
		})
	}
}

func TestAdoptedLaunchDoesNotKeepConcurrencySlotAfterClosing(t *testing.T) {
	first := applicationContextWithID(testContextID, "org.example.First")
	first.App.RestorePolicy = ApplicationRestorePinned
	second := applicationContextWithID("6ba7b810-9dad-11d1-80b4-00c04fd430c8", "org.example.Second")
	now := time.Unix(2000, 0).UTC()
	state := ApplicationSessionState{Version: ApplicationSessionSchemaVersion, CompositorID: strings.Repeat("a", 64), Attempts: []ApplicationLaunchAttempt{{ContextID: first.ID, StartedAt: now}}, Adoptions: []ApplicationAdoption{{ContextID: first.ID, ObservedAt: now.Add(time.Second)}}}
	coordinator, err := NewApplicationRestoreCoordinator(state.CompositorID, state, now, ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: time.Minute, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := coordinator.Plan(Registry{Version: ContextsSchemaVersion, Contexts: []Context{first, second}}, nil, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if plan.LaunchSlots != 1 || len(plan.Launch) != 1 || plan.Launch[0].ID != second.ID {
		t.Fatalf("completed adoption retained launch slot: %+v", plan)
	}
	if !reflect.DeepEqual(coordinator.State(), state) {
		t.Fatal("observation consumed or replaced an unrelated launch guard")
	}
	copy := coordinator.State()
	copy.Adoptions[0].ContextID = second.ID
	if !reflect.DeepEqual(coordinator.State(), state) {
		t.Fatal("returned state aliases coordinator adoption memory")
	}
}

func TestRejectedRetryRearmsAdoptionOnlyWhenCandidateIsAdopted(t *testing.T) {
	app := applicationContextWithID(testContextID, "org.example.Adopted")
	app.App.RestorePolicy = ApplicationRestorePinned
	now := time.Unix(2000, 0).UTC()
	state := ApplicationSessionState{Version: ApplicationSessionSchemaVersion, CompositorID: strings.Repeat("a", 64), Attempts: []ApplicationLaunchAttempt{{ContextID: app.ID, StartedAt: now}}, Adoptions: []ApplicationAdoption{{ContextID: app.ID, ObservedAt: now.Add(time.Second)}}}
	coordinator, err := NewApplicationRestoreCoordinator(state.CompositorID, state, now, ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: time.Minute, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, matched, err := coordinator.RetryRejectedAttempt(app.ID, now.Add(time.Second)); err != nil || matched {
		t.Fatalf("stale retry matched current attempt: %t %v", matched, err)
	}
	candidate, matched, err := coordinator.RetryRejectedAttempt(app.ID, now)
	if err != nil || !matched || len(candidate.Adoptions) != 0 || len(candidate.Attempts) != 0 {
		t.Fatalf("exact retry did not prepare rearm: %+v %t %v", candidate, matched, err)
	}
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{app}}
	plan, err := coordinator.Plan(registry, nil, now.Add(2*time.Second))
	if err != nil || len(plan.Launch) != 0 || !reflect.DeepEqual(coordinator.State(), state) {
		t.Fatalf("uncommitted retry changed launch authority: %+v %v", plan, err)
	}
	if err := coordinator.RestoreState(candidate); err != nil {
		t.Fatal(err)
	}
	plan, err = coordinator.Plan(registry, nil, now.Add(3*time.Second))
	if err != nil || len(plan.Launch) != 1 || plan.Launch[0].ID != app.ID {
		t.Fatalf("committed retry did not rearm normal planner: %+v %v", plan, err)
	}
}
