package session

import (
	"reflect"
	"testing"
	"time"
)

func TestSemanticSnapshotHashIgnoresNonSemanticOrdering(t *testing.T) {
	first := LayoutSnapshot{
		Version: LayoutSchemaVersion,
		Workspaces: []WorkspaceLayout{
			{Name: "2", RestoreMode: WorkspaceRestorePlacementOnly, PlacementContexts: []ContextID{secondContextID, testContextID}},
			{Name: "1", RestoreMode: WorkspaceRestorePlacementOnly, PlacementContexts: []ContextID{thirdContextID}},
		},
	}
	second := LayoutSnapshot{
		Version: LayoutSchemaVersion,
		Workspaces: []WorkspaceLayout{
			{Name: "1", RestoreMode: WorkspaceRestorePlacementOnly, PlacementContexts: []ContextID{thirdContextID}},
			{Name: "2", RestoreMode: WorkspaceRestorePlacementOnly, PlacementContexts: []ContextID{testContextID, secondContextID}},
		},
	}

	firstHash, err := SemanticSnapshotHash(first)
	if err != nil {
		t.Fatalf("hash first snapshot: %v", err)
	}
	secondHash, err := SemanticSnapshotHash(second)
	if err != nil {
		t.Fatalf("hash second snapshot: %v", err)
	}
	if firstHash != secondHash {
		t.Fatal("non-semantic ordering changed the snapshot hash")
	}
}

func TestSemanticSnapshotHashPreservesLayoutChildOrder(t *testing.T) {
	first := twoChildLayout(testContextID, secondContextID)
	second := twoChildLayout(secondContextID, testContextID)
	firstHash, err := SemanticSnapshotHash(first)
	if err != nil {
		t.Fatalf("hash first snapshot: %v", err)
	}
	secondHash, err := SemanticSnapshotHash(second)
	if err != nil {
		t.Fatalf("hash second snapshot: %v", err)
	}
	if firstHash == secondHash {
		t.Fatal("semantic child ordering did not change the snapshot hash")
	}
}

func TestSnapshotDebouncerWaitsForOneQuietPeriodAndCopiesCandidate(t *testing.T) {
	previous := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{}}
	debouncer, err := NewSnapshotDebouncer(previous, time.Second)
	if err != nil {
		t.Fatalf("create debouncer: %v", err)
	}
	candidate := placementSnapshot("2", testContextID)
	start := time.Unix(100, 0)
	if scheduled, err := debouncer.Observe(candidate, start); err != nil || !scheduled {
		t.Fatalf("observe candidate: scheduled=%v err=%v", scheduled, err)
	}
	candidate.Workspaces[0].Name = "mutated by caller"
	if _, due := debouncer.Due(start.Add(999 * time.Millisecond)); due {
		t.Fatal("candidate became due before the quiet period")
	}
	pending, due := debouncer.Due(start.Add(time.Second))
	if !due || pending.Workspaces[0].Name != "2" {
		t.Fatalf("unexpected immutable pending candidate: due=%v snapshot=%+v", due, pending)
	}

	if _, err := debouncer.Observe(pending, start.Add(1500*time.Millisecond)); err != nil {
		t.Fatalf("reobserve pending candidate: %v", err)
	}
	if _, due := debouncer.Due(start.Add(2 * time.Second)); due {
		t.Fatal("relevant observation did not restart the quiet period")
	}
	if err := debouncer.MarkPersisted(pending); err != nil {
		t.Fatalf("mark persisted: %v", err)
	}
	if _, exists := debouncer.Deadline(); exists {
		t.Fatal("persisted candidate remained scheduled")
	}
}

func TestStartupCaptureReadyRejectsEmptyOrPartialTree(t *testing.T) {
	previous := twoChildLayout(testContextID, secondContextID)
	registry := registryWithContexts(testContextID, secondContextID)
	empty := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{}}
	ready, err := StartupCaptureReady(previous, empty, registry)
	if err != nil {
		t.Fatalf("check empty startup capture: %v", err)
	}
	if ready {
		t.Fatal("empty startup tree was considered complete")
	}
	partial := placementSnapshot("2", testContextID)
	ready, err = StartupCaptureReady(previous, partial, registry)
	if err != nil {
		t.Fatalf("check partial startup capture: %v", err)
	}
	if ready {
		t.Fatal("partial startup tree was considered complete")
	}
	ready, err = StartupCaptureReady(previous, previous, registry)
	if err != nil || !ready {
		t.Fatalf("complete startup tree was not ready: ready=%v err=%v", ready, err)
	}
}

func TestStartupCaptureReadyDoesNotWaitForPurgedContext(t *testing.T) {
	previous := placementSnapshot("2", testContextID)
	empty := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{}}
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{}}
	ready, err := StartupCaptureReady(previous, empty, registry)
	if err != nil || !ready {
		t.Fatalf("purged context blocked startup: ready=%v err=%v", ready, err)
	}
}

func TestStartupCaptureReadyDoesNotWaitForArchivedContext(t *testing.T) {
	previous := placementSnapshot("2", testContextID)
	empty := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{}}
	registry := registryWithContexts(testContextID)
	registry.Contexts[0].State = ContextArchived
	ready, err := StartupCaptureReady(previous, empty, registry)
	if err != nil || !ready {
		t.Fatalf("archived context blocked startup: ready=%v err=%v", ready, err)
	}
}

func TestPreserveMissingPlacementsKeepsExactWorkspaceWhileLeafIsAbsent(t *testing.T) {
	previous := LayoutSnapshot{
		Version: LayoutSchemaVersion,
		Workspaces: []WorkspaceLayout{
			twoChildLayout(testContextID, secondContextID).Workspaces[0],
			{Name: "3", RestoreMode: WorkspaceRestoreLayout, Tiling: &LayoutNode{ContextID: contextIDPointerValue(thirdContextID)}},
		},
	}
	captured := LayoutSnapshot{
		Version: LayoutSchemaVersion,
		Workspaces: []WorkspaceLayout{
			{Name: "2", RestoreMode: WorkspaceRestoreLayout, Tiling: &LayoutNode{ContextID: contextIDPointerValue(secondContextID)}},
			{Name: "3", RestoreMode: WorkspaceRestoreLayout, Tiling: &LayoutNode{ContextID: contextIDPointerValue(thirdContextID)}},
		},
	}

	merged, err := PreserveMissingPlacements(previous, captured, registryWithContexts(testContextID, secondContextID, thirdContextID))
	if err != nil {
		t.Fatalf("preserve missing placement: %v", err)
	}
	if !reflect.DeepEqual(merged.Workspaces[0], previous.Workspaces[0]) {
		t.Fatalf("last exact workspace was not preserved: %+v", merged.Workspaces[0])
	}
	if merged.Workspaces[1].RestoreMode != WorkspaceRestoreLayout {
		t.Fatalf("unaffected workspace lost exact layout: %+v", merged.Workspaces[1])
	}
}

func TestPreserveMissingPlacementsDegradesWhenSiblingMovedElsewhere(t *testing.T) {
	previous := twoChildLayout(testContextID, secondContextID)
	captured := placementSnapshot("7: moved", secondContextID)
	merged, err := PreserveMissingPlacements(previous, captured, registryWithContexts(testContextID, secondContextID))
	if err != nil {
		t.Fatalf("preserve divergent placement: %v", err)
	}
	if len(merged.Workspaces) != 2 {
		t.Fatalf("unexpected divergent workspaces: %+v", merged.Workspaces)
	}
	var oldWorkspace *WorkspaceLayout
	for index := range merged.Workspaces {
		if merged.Workspaces[index].Name == "2" {
			oldWorkspace = &merged.Workspaces[index]
		}
	}
	if oldWorkspace == nil || oldWorkspace.RestoreMode != WorkspaceRestorePlacementOnly ||
		!reflect.DeepEqual(oldWorkspace.PlacementContexts, []ContextID{testContextID}) {
		t.Fatalf("divergent old workspace was not safely degraded: %+v", oldWorkspace)
	}
}

func TestPreserveMissingPlacementsDoesNotOverrideCurrentMixedDegradation(t *testing.T) {
	previous := twoChildLayout(testContextID, secondContextID)
	captured := placementSnapshot("2", secondContextID)
	merged, err := PreserveMissingPlacements(previous, captured, registryWithContexts(testContextID, secondContextID))
	if err != nil {
		t.Fatalf("preserve mixed placement: %v", err)
	}
	workspace := merged.Workspaces[0]
	if workspace.RestoreMode != WorkspaceRestorePlacementOnly ||
		!reflect.DeepEqual(workspace.PlacementContexts, []ContextID{testContextID, secondContextID}) {
		t.Fatalf("old exact tree overrode current mixed degradation: %+v", workspace)
	}
}

func TestPreserveMissingPlacementsDropsUnregisteredContext(t *testing.T) {
	previous := placementSnapshot("2", testContextID)
	empty := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{}}
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{}}
	merged, err := PreserveMissingPlacements(previous, empty, registry)
	if err != nil {
		t.Fatalf("drop unregistered placement: %v", err)
	}
	if len(merged.Workspaces) != 0 {
		t.Fatalf("unregistered context placement was retained: %+v", merged)
	}
}

func TestPreserveFailedRestoreWorkspacesKeepsLastGoodTree(t *testing.T) {
	previous := twoChildLayout(testContextID, secondContextID)
	candidate := placementSnapshot("2", testContextID, secondContextID)
	result, preserved, err := PreserveFailedRestoreWorkspaces(
		previous,
		candidate,
		map[string]struct{}{"2": {}},
	)
	if err != nil {
		t.Fatalf("preserve failed restore: %v", err)
	}
	if _, exists := preserved["2"]; !exists {
		t.Fatal("failed workspace was not reported as preserved")
	}
	if !reflect.DeepEqual(result, previous) {
		t.Fatalf("degraded restore replaced the last-good tree: %+v", result)
	}
}

func TestPreserveFailedRestoreWorkspacesReleasesChangedContextSet(t *testing.T) {
	previous := twoChildLayout(testContextID, secondContextID)
	candidate := placementSnapshot("2", testContextID)
	result, preserved, err := PreserveFailedRestoreWorkspaces(
		previous,
		candidate,
		map[string]struct{}{"2": {}},
	)
	if err != nil {
		t.Fatalf("release changed failed restore: %v", err)
	}
	if _, exists := preserved["2"]; exists {
		t.Fatal("changed context set remained quarantined")
	}
	if !reflect.DeepEqual(result, candidate) {
		t.Fatalf("later context-set change was discarded: %+v", result)
	}
}

func placementSnapshot(workspace string, ids ...ContextID) LayoutSnapshot {
	return LayoutSnapshot{
		Version: LayoutSchemaVersion,
		Workspaces: []WorkspaceLayout{{
			Name:              workspace,
			RestoreMode:       WorkspaceRestorePlacementOnly,
			PlacementContexts: append([]ContextID(nil), ids...),
		}},
	}
}

func twoChildLayout(first ContextID, second ContextID) LayoutSnapshot {
	return LayoutSnapshot{
		Version: LayoutSchemaVersion,
		Workspaces: []WorkspaceLayout{{
			Name:        "2",
			RestoreMode: WorkspaceRestoreLayout,
			Tiling: &LayoutNode{
				Layout: LayoutSplitHorizontal,
				Children: []LayoutNode{
					{ContextID: contextIDPointerValue(first), Proportion: 0.5},
					{ContextID: contextIDPointerValue(second), Proportion: 0.5},
				},
			},
		}},
	}
}
