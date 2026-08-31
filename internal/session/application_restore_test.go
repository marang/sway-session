package session

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestObserveApplicationGroupsTreatsMatchingTopLevelsAsOnePresence(t *testing.T) {
	context := flatpakApplicationContext("org.example.App", "org.example.App")
	context.ID = testContextID
	mark, err := context.ID.Mark()
	if err != nil {
		t.Fatal(err)
	}
	first := appWindow(41, false, "org.example.App", "", "", "org.example.App")
	first.Marks = []string{mark}
	second := appWindow(42, true, "org.example.App", "", "", "org.example.App")
	root := applicationTree(first)
	root.Nodes[0].Nodes[0].Nodes = append(root.Nodes[0].Nodes[0].Nodes, second)

	groups, err := ObserveApplicationGroups(root, Registry{
		Version:  ContextsSchemaVersion,
		Contexts: []Context{context},
	})

	if err != nil {
		t.Fatal(err)
	}
	group, exists := groups[context.ID]
	if !exists || len(group.Windows) != 2 || group.Anchor == nil || group.Anchor.ContainerID != 41 || group.Ambiguous {
		t.Fatalf("matching top-levels were not one anchored presence group: %+v", group)
	}
}

func TestObserveApplicationGroupsIsolatesMarkedIdentityDriftAsAmbiguousPresence(t *testing.T) {
	context := applicationContextWithID(testContextID, "org.example.Old")
	mark, err := context.ID.Mark()
	if err != nil {
		t.Fatal(err)
	}
	window := appWindow(41, true, "org.example.New", "", "", "org.example.New")
	window.Marks = []string{mark}
	groups, err := ObserveApplicationGroups(applicationTree(window), Registry{
		Version: ContextsSchemaVersion, Contexts: []Context{context},
	})
	if err != nil {
		t.Fatal(err)
	}
	group := groups[context.ID]
	if len(group.Windows) != 1 || !group.Ambiguous || group.Anchor != nil {
		t.Fatalf("marked identity drift was not isolated safely: %+v", group)
	}
}

func TestObserveApplicationGroupsCountsScratchpadAsPresenceWithoutRestoringIt(t *testing.T) {
	context := applicationContextWithID(testContextID, "org.example.App")
	appID := context.App.Identity.WaylandAppID
	sandbox := context.App.Identity.SandboxAppID
	window := appWindow(41, true, appID, "", "", sandbox)
	root := applicationTree(window)
	root.Nodes[0].Nodes[0].Name = "__i3_scratch"

	groups, err := ObserveApplicationGroups(root, Registry{Version: ContextsSchemaVersion, Contexts: []Context{context}})
	if err != nil {
		t.Fatal(err)
	}
	group := groups[context.ID]
	if len(group.Windows) != 1 || group.Anchor != nil || !group.Ambiguous {
		t.Fatalf("scratchpad presence became restorable placement: %+v", group)
	}
}

func TestObserveApplicationGroupsIgnoresArchivedApplication(t *testing.T) {
	context := applicationContextWithID(testContextID, "org.example.App")
	context.State = ContextArchived
	appID := context.App.Identity.WaylandAppID
	sandbox := context.App.Identity.SandboxAppID
	window := appWindow(41, true, appID, "", "", sandbox)

	groups, err := ObserveApplicationGroups(applicationTree(window), Registry{Version: ContextsSchemaVersion, Contexts: []Context{context}})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Fatalf("archived application remained under daemon management: %+v", groups)
	}
}

func TestPlanApplicationPlacementMarksOnlyOneUniqueAnchor(t *testing.T) {
	context := applicationContextWithID(testContextID, "org.example.App")
	desired := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{{
		Name: "98: apps", RestoreMode: WorkspaceRestorePlacementOnly, PlacementContexts: []ContextID{context.ID},
	}}}
	unique := map[ContextID]ApplicationGroup{context.ID: {
		Windows: []WindowApplication{{ContainerID: 41, Workspace: "99: landing"}},
		Anchor:  &WindowApplication{ContainerID: 41, Workspace: "99: landing"},
	}}
	actions, err := PlanApplicationPlacementActions(unique, desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[0].Kind != PlacementMoveWorkspace || actions[0].Workspace != "98: apps" || actions[1].Kind != PlacementAddMark {
		t.Fatalf("unique application anchor did not receive move then mark: %+v", actions)
	}
	ambiguous := map[ContextID]ApplicationGroup{context.ID: {
		Windows: []WindowApplication{{ContainerID: 41}, {ContainerID: 42}}, Ambiguous: true,
	}}
	actions, err = PlanApplicationPlacementActions(ambiguous, desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("ambiguous application windows were guessed between: %+v", actions)
	}
}

func TestApplicationSessionFilePersistsConservativeAttemptEvidence(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	started := time.Date(2026, 8, 31, 18, 0, 0, 0, time.UTC)
	want := ApplicationSessionState{
		Version:      ApplicationSessionSchemaVersion,
		CompositorID: stringsOfLength("d", 64),
		Attempts:     []ApplicationLaunchAttempt{{ContextID: testContextID, StartedAt: started}},
	}
	if err := ApplicationSessionFile(root).Save(want); err != nil {
		t.Fatal(err)
	}
	var got ApplicationSessionState
	if err := ApplicationSessionFile(root).LoadInto(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attempt evidence changed across persistence: got=%+v want=%+v", got, want)
	}
}

func TestApplicationRestoreCoordinatorWaitsForAdoptionBeforeLaunchingMissingDesiredApp(t *testing.T) {
	now := time.Date(2026, 8, 31, 18, 0, 0, 0, time.UTC)
	context := flatpakApplicationContext("org.example.App", "org.example.App")
	context.ID = testContextID
	context.App.DesiredOpen = true
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{context}}
	groups := map[ContextID]ApplicationGroup{context.ID: {Windows: []WindowApplication{}}}
	coordinator, err := NewApplicationRestoreCoordinator(
		stringsOfLength("a", 64),
		ApplicationSessionState{Version: ApplicationSessionSchemaVersion, CompositorID: stringsOfLength("a", 64), Attempts: []ApplicationLaunchAttempt{}},
		now,
		ApplicationRestoreOptions{AdoptionGrace: 10 * time.Second, CloseGrace: 2 * time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 2},
	)
	if err != nil {
		t.Fatal(err)
	}

	before, err := coordinator.Plan(registry, groups, now.Add(9*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	after, err := coordinator.Plan(registry, groups, now.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}

	if len(before.Launch) != 0 || len(after.Launch) != 1 || after.Launch[0].ID != context.ID {
		t.Fatalf("adoption grace did not gate launch: before=%+v after=%+v", before.Launch, after.Launch)
	}
}

func TestApplicationRestoreCoordinatorFollowsLastWindowWithCloseGrace(t *testing.T) {
	now := time.Date(2026, 8, 31, 18, 0, 0, 0, time.UTC)
	context := flatpakApplicationContext("org.example.App", "org.example.App")
	context.ID = testContextID
	context.App.DesiredOpen = false
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{context}}
	coordinator, err := NewApplicationRestoreCoordinator(
		stringsOfLength("b", 64),
		ApplicationSessionState{Version: ApplicationSessionSchemaVersion, CompositorID: stringsOfLength("b", 64), Attempts: []ApplicationLaunchAttempt{}},
		now,
		ApplicationRestoreOptions{AdoptionGrace: 10 * time.Second, CloseGrace: 2 * time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 2},
	)
	if err != nil {
		t.Fatal(err)
	}
	present := map[ContextID]ApplicationGroup{context.ID: {Windows: []WindowApplication{{ContainerID: 41}}}}

	opened, err := coordinator.Plan(registry, present, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(opened.DesiredOpen) != 1 || !opened.DesiredOpen[0].Open {
		t.Fatalf("manual appearance did not become desired-open: %+v", opened.DesiredOpen)
	}
	registry.Contexts[0].App.DesiredOpen = true
	absent := map[ContextID]ApplicationGroup{context.ID: {Windows: []WindowApplication{}}}
	before, err := coordinator.Plan(registry, absent, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	after, err := coordinator.Plan(registry, absent, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(before.DesiredOpen) != 0 || len(before.Launch) != 0 || len(after.DesiredOpen) != 1 || after.DesiredOpen[0].Open {
		t.Fatalf("last-window close grace was not respected: before=%+v after=%+v", before, after.DesiredOpen)
	}
}

func TestApplicationRestoreCoordinatorPersistsAttemptsAndBoundsParallelLaunches(t *testing.T) {
	now := time.Date(2026, 8, 31, 18, 0, 0, 0, time.UTC)
	contexts := []Context{
		applicationContextWithID("11111111-1111-4111-8111-111111111111", "org.example.First"),
		applicationContextWithID("22222222-2222-4222-8222-222222222222", "org.example.Second"),
		applicationContextWithID("33333333-3333-4333-8333-333333333333", "org.example.Third"),
	}
	registry := Registry{Version: ContextsSchemaVersion, Contexts: contexts}
	groups := map[ContextID]ApplicationGroup{}
	compositorID := stringsOfLength("c", 64)
	options := ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: 2 * time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 2}
	coordinator, err := NewApplicationRestoreCoordinator(compositorID, ApplicationSessionState{}, now, options)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := coordinator.Plan(registry, groups, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Launch) != 3 || plan.LaunchSlots != 2 {
		t.Fatalf("parallel bound did not expose all candidates and exactly two slots: %+v", plan)
	}
	_, err = coordinator.BeginAttempt(plan.Launch[0].ID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	state, err := coordinator.BeginAttempt(plan.Launch[1].ID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewApplicationRestoreCoordinator(compositorID, state, now, options)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := restarted.Plan(registry, groups, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked.Launch) != 0 || blocked.LaunchSlots != 0 {
		t.Fatalf("persisted in-flight attempts were repeated after daemon restart: %+v", blocked.Launch)
	}
	groups[plan.Launch[0].ID] = ApplicationGroup{Windows: []WindowApplication{{ContainerID: 41}}}
	next, err := restarted.Plan(registry, groups, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Launch) != 1 || next.Launch[0].ID != contexts[2].ID || next.LaunchSlots != 1 {
		t.Fatalf("completed presence did not release one launch slot: %+v", next.Launch)
	}
}

func TestApplicationRestoreCoordinatorFutureAttemptDoesNotConsumeLaunchSlot(t *testing.T) {
	now := time.Unix(2000, 0)
	first := applicationContextWithID("11111111-1111-4111-8111-111111111111", "org.example.First")
	second := applicationContextWithID("22222222-2222-4222-8222-222222222222", "org.example.Second")
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{first, second}}
	state := ApplicationSessionState{
		Version: ApplicationSessionSchemaVersion, CompositorID: stringsOfLength("c", 64),
		Attempts: []ApplicationLaunchAttempt{{ContextID: first.ID, StartedAt: now.Add(time.Hour)}},
	}
	coordinator, err := NewApplicationRestoreCoordinator(
		state.CompositorID, state, now.Add(-time.Second),
		ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 1},
	)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := coordinator.Plan(registry, map[ContextID]ApplicationGroup{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Launch) != 1 || plan.Launch[0].ID != second.ID {
		t.Fatalf("future attempt consumed the only launch slot: %+v", plan.Launch)
	}
}

func TestApplicationRestoreCoordinatorUsesFreshAttemptBudgetForNewCompositor(t *testing.T) {
	now := time.Date(2026, 8, 31, 18, 0, 0, 0, time.UTC)
	context := applicationContextWithID(testContextID, "org.example.App")
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{context}}
	old := ApplicationSessionState{
		Version: ApplicationSessionSchemaVersion, CompositorID: stringsOfLength("1", 64),
		Attempts: []ApplicationLaunchAttempt{{ContextID: context.ID, StartedAt: now.Add(-time.Minute)}},
	}
	coordinator, err := NewApplicationRestoreCoordinator(
		stringsOfLength("2", 64), old, now,
		ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 2},
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := coordinator.Plan(registry, map[ContextID]ApplicationGroup{}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Launch) != 1 || plan.Launch[0].ID != context.ID {
		t.Fatalf("new compositor inherited an old launch attempt: %+v", plan.Launch)
	}
}

func TestApplicationRestoreCoordinatorPrunesAttemptsForForgottenApps(t *testing.T) {
	now := time.Date(2026, 8, 31, 18, 0, 0, 0, time.UTC)
	forgottenID := ContextID("11111111-1111-4111-8111-111111111111")
	current := applicationContextWithID("22222222-2222-4222-8222-222222222222", "org.example.Current")
	compositorID := stringsOfLength("6", 64)
	coordinator, err := NewApplicationRestoreCoordinator(
		compositorID,
		ApplicationSessionState{Version: ApplicationSessionSchemaVersion, CompositorID: compositorID, Attempts: []ApplicationLaunchAttempt{{ContextID: forgottenID, StartedAt: now}}},
		now,
		ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 2},
	)
	if err != nil {
		t.Fatal(err)
	}
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{current}}
	plan, err := coordinator.Plan(registry, map[ContextID]ApplicationGroup{}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Launch) != 1 || plan.Launch[0].ID != current.ID {
		t.Fatalf("forgotten attempt blocked current app: %+v", plan.Launch)
	}
	state, err := coordinator.BeginAttempt(current.ID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Attempts) != 1 || state.Attempts[0].ContextID != current.ID {
		t.Fatalf("forgotten attempt survived compaction: %+v", state.Attempts)
	}
	coordinator.seenPresent[forgottenID] = true
	coordinator.missingSince[forgottenID] = now
	if _, err := coordinator.Plan(registry, map[ContextID]ApplicationGroup{}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if coordinator.seenPresent[forgottenID] || !coordinator.missingSince[forgottenID].IsZero() {
		t.Fatal("forgotten application retained unbounded in-memory lifecycle state")
	}
}

func TestApplicationRestoreCoordinatorPreservesProfilePickerTransition(t *testing.T) {
	now := time.Date(2026, 8, 31, 18, 0, 0, 0, time.UTC)
	context := applicationContextWithID(testContextID, "org.example.App")
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{context}}
	coordinator, err := NewApplicationRestoreCoordinator(
		stringsOfLength("3", 64), ApplicationSessionState{}, now,
		ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: 3 * time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 2},
	)
	if err != nil {
		t.Fatal(err)
	}
	present := map[ContextID]ApplicationGroup{context.ID: {Windows: []WindowApplication{{ContainerID: 41}}}}
	if _, err := coordinator.Plan(registry, present, now); err != nil {
		t.Fatal(err)
	}
	absent := map[ContextID]ApplicationGroup{context.ID: {Windows: []WindowApplication{}}}
	if _, err := coordinator.Plan(registry, absent, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	transitioned, err := coordinator.Plan(registry, present, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(transitioned.DesiredOpen) != 0 {
		t.Fatalf("short profile-picker transition closed the application: %+v", transitioned.DesiredOpen)
	}
}

func TestApplicationRestoreCoordinatorNeverClosesPinnedState(t *testing.T) {
	now := time.Date(2026, 8, 31, 18, 0, 0, 0, time.UTC)
	context := applicationContextWithID(testContextID, "org.example.App")
	context.App.RestorePolicy = ApplicationRestorePinned
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{context}}
	coordinator, err := NewApplicationRestoreCoordinator(
		stringsOfLength("4", 64), ApplicationSessionState{}, now,
		ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 2},
	)
	if err != nil {
		t.Fatal(err)
	}
	present := map[ContextID]ApplicationGroup{context.ID: {Windows: []WindowApplication{{ContainerID: 41}}}}
	if _, err := coordinator.Plan(registry, present, now); err != nil {
		t.Fatal(err)
	}
	absent := map[ContextID]ApplicationGroup{context.ID: {Windows: []WindowApplication{}}}
	plan, err := coordinator.Plan(registry, absent, now.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.DesiredOpen) != 0 || len(plan.Launch) != 0 {
		t.Fatalf("pinned application became desired-closed or a same-session watchdog: %+v", plan)
	}
}

func TestApplicationRestoreCoordinatorIgnoresArchivedLifecycle(t *testing.T) {
	now := time.Date(2026, 8, 31, 18, 0, 0, 0, time.UTC)
	context := applicationContextWithID(testContextID, "org.example.App")
	context.State = ContextArchived
	context.App.DesiredOpen = false
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{context}}
	coordinator, err := NewApplicationRestoreCoordinator(
		stringsOfLength("7", 64), ApplicationSessionState{}, now,
		ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 2},
	)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[ContextID]ApplicationGroup{context.ID: {Windows: []WindowApplication{{ContainerID: 41}}}}

	plan, err := coordinator.Plan(registry, groups, now.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.DesiredOpen) != 0 || len(plan.Launch) != 0 {
		t.Fatalf("archived application received lifecycle work: %+v", plan)
	}
}

func TestApplicationRestoreCoordinatorDoesNotRelaunchAJustClosedFollowApp(t *testing.T) {
	now := time.Date(2026, 8, 31, 18, 0, 0, 0, time.UTC)
	context := applicationContextWithID(testContextID, "org.example.App")
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{context}}
	coordinator, err := NewApplicationRestoreCoordinator(
		stringsOfLength("5", 64), ApplicationSessionState{}, now,
		ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 2},
	)
	if err != nil {
		t.Fatal(err)
	}
	present := map[ContextID]ApplicationGroup{context.ID: {Windows: []WindowApplication{{ContainerID: 41}}}}
	if _, err := coordinator.Plan(registry, present, now); err != nil {
		t.Fatal(err)
	}
	absent := map[ContextID]ApplicationGroup{context.ID: {Windows: []WindowApplication{}}}
	missing, err := coordinator.Plan(registry, absent, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(missing.Launch) != 0 {
		t.Fatalf("follow application relaunched during close grace: %+v", missing.Launch)
	}
	closed, err := coordinator.Plan(registry, absent, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(closed.DesiredOpen) != 1 || closed.DesiredOpen[0].Open || len(closed.Launch) != 0 {
		t.Fatalf("just-closed follow application was not closed cleanly: %+v", closed)
	}
	retry, err := coordinator.Plan(registry, absent, now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(retry.DesiredOpen) != 1 || retry.DesiredOpen[0].Open || len(retry.Launch) != 0 {
		t.Fatalf("uncommitted desired-close was not retryable: %+v", retry)
	}
}

func applicationContextWithID(id ContextID, appID string) Context {
	context := flatpakApplicationContext(appID, appID)
	context.ID = id
	context.App.DesiredOpen = true
	return context
}

func stringsOfLength(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}
