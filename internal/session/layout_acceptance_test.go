package session

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/swayipc"
)

// These cases cross capture, stale-snapshot merge, durable state, and the
// restore planner's first step. Action replay and full compositor convergence
// are exercised by the daemon's integration tests, not this synthetic tree.
func TestLayoutAcceptanceSelectsRestoreFromReloadedExactSnapshot(t *testing.T) {
	archivedID := ContextID("6ba7b812-9dad-41d1-80b4-00c04fd430c8")
	registry := registryWithContexts(testContextID, secondContextID, thirdContextID)
	archived := applicationContextWithID(archivedID, "org.example.Archived")
	archived.State = ContextArchived
	registry.Contexts = append(registry.Contexts, archived)
	stale := placementSnapshot("98: layout", testContextID, secondContextID, thirdContextID, archivedID)

	tests := []struct {
		name       string
		workspace  func(*testing.T) *swayipc.TreeNode
		want       WorkspaceLayout
		change     func(*swayipc.TreeNode)
		wantPhase  RestorePhase
		wantAction RestoreActionKind
	}{
		{
			name: "stacked",
			workspace: func(t *testing.T) *swayipc.TreeNode {
				return restoreWorkspace("98: layout", "stacked",
					managedTreeLeaf(t, 11, testContextID, nil, false),
					managedTreeLeaf(t, 12, secondContextID, nil, false),
					managedTreeLeaf(t, 13, thirdContextID, nil, false))
			},
			want:      exactWorkspace("98: layout", LayoutStacked, testContextID, secondContextID, thirdContextID),
			change:    func(workspace *swayipc.TreeNode) { workspace.Layout = "splith" },
			wantPhase: RestoreStageOut, wantAction: RestoreMoveWorkspace,
		},
		{
			name: "vertical split",
			workspace: func(t *testing.T) *swayipc.TreeNode {
				return restoreWorkspace("98: layout", "splitv",
					managedTreeLeaf(t, 11, testContextID, nil, false),
					managedTreeLeaf(t, 12, secondContextID, nil, false),
					managedTreeLeaf(t, 13, thirdContextID, nil, false))
			},
			want:      exactWorkspace("98: layout", LayoutSplitVertical, testContextID, secondContextID, thirdContextID),
			change:    func(workspace *swayipc.TreeNode) { workspace.Layout = "splith" },
			wantPhase: RestoreStageOut, wantAction: RestoreMoveWorkspace,
		},
		{
			name: "nested tabbed",
			workspace: func(t *testing.T) *swayipc.TreeNode {
				return restoreWorkspace("98: layout", "splith",
					managedTreeLeaf(t, 11, testContextID, nil, false),
					&swayipc.TreeNode{ID: 20, Type: "con", Layout: "tabbed", Nodes: []*swayipc.TreeNode{
						managedTreeLeaf(t, 12, secondContextID, nil, false),
						managedTreeLeaf(t, 13, thirdContextID, nil, false),
					}})
			},
			want: WorkspaceLayout{Name: "98: layout", RestoreMode: WorkspaceRestoreLayout, Tiling: &LayoutNode{
				Layout: LayoutSplitHorizontal, Children: []LayoutNode{
					{ContextID: contextIDPointer(testContextID)},
					{Layout: LayoutTabbed, Children: []LayoutNode{
						{ContextID: contextIDPointer(secondContextID)},
						{ContextID: contextIDPointer(thirdContextID)},
					}},
				},
			}},
			change:    func(workspace *swayipc.TreeNode) { workspace.Nodes[1].Layout = "stacked" },
			wantPhase: RestoreStageOut, wantAction: RestoreMoveWorkspace,
		},
		{
			name: "floating geometry",
			workspace: func(t *testing.T) *swayipc.TreeNode {
				floating := managedTreeLeaf(t, 13, thirdContextID, nil, false)
				floating.Type = "floating_con"
				floating.Rect = swayipc.Rect{X: 30, Y: 40, Width: 500, Height: 300}
				workspace := restoreWorkspace("98: layout", "splith",
					managedTreeLeaf(t, 11, testContextID, nil, false),
					managedTreeLeaf(t, 12, secondContextID, nil, false))
				workspace.FloatingNodes = []*swayipc.TreeNode{floating}
				return workspace
			},
			want: WorkspaceLayout{Name: "98: layout", RestoreMode: WorkspaceRestoreLayout, Tiling: &LayoutNode{
				Layout: LayoutSplitHorizontal, Children: []LayoutNode{
					{ContextID: contextIDPointer(testContextID)},
					{ContextID: contextIDPointer(secondContextID)},
				},
			}, Floating: []LayoutNode{{ContextID: contextIDPointer(thirdContextID), Geometry: &Geometry{X: 30, Y: 40, Width: 500, Height: 300}}}},
			change:    func(workspace *swayipc.TreeNode) { workspace.FloatingNodes[0].Rect.X = 80 },
			wantPhase: RestoreBuild, wantAction: RestoreMoveFloating,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatalf("protect state root: %v", err)
			}
			if err := RegistryStoreFor(root).Save(registry); err != nil {
				t.Fatalf("save registry: %v", err)
			}
			if err := LayoutStoreFor(root).Save(stale); err != nil {
				t.Fatalf("save stale layout: %v", err)
			}
			var loadedRegistry Registry
			if err := RegistryStoreFor(root).LoadInto(&loadedRegistry); err != nil {
				t.Fatalf("reload registry: %v", err)
			}
			if !reflect.DeepEqual(loadedRegistry, registry) {
				t.Fatalf("archived registry row changed after reload: %+v", loadedRegistry)
			}
			var previous LayoutSnapshot
			if err := LayoutStoreFor(root).LoadInto(&previous); err != nil {
				t.Fatalf("reload stale layout: %v", err)
			}

			workspace := test.workspace(t)
			live := restoreTree(workspace)
			captured, err := CaptureLayout(live, loadedRegistry)
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			want, err := canonicalSnapshot(layoutSnapshot(test.want))
			if err != nil {
				t.Fatalf("validate expected layout: %v", err)
			}
			canonicalCaptured, err := canonicalSnapshot(captured)
			if err != nil {
				t.Fatalf("canonicalize capture: %v", err)
			}
			if !reflect.DeepEqual(canonicalCaptured, want) {
				gotJSON, _ := json.Marshal(captured)
				wantJSON, _ := json.Marshal(want)
				t.Fatalf("capture lost current structure:\n got: %s\nwant: %s", gotJSON, wantJSON)
			}
			merged, err := PreserveMissingPlacements(previous, captured, loadedRegistry)
			if err != nil {
				t.Fatalf("merge stale layout: %v", err)
			}
			if !reflect.DeepEqual(merged, want) {
				t.Fatalf("stale archived placement displaced current layout:\n got: %+v\nwant: %+v", merged, want)
			}
			if err := LayoutStoreFor(root).Save(merged); err != nil {
				t.Fatalf("persist merged layout: %v", err)
			}
			var restored LayoutSnapshot
			if err := LayoutStoreFor(root).LoadInto(&restored); err != nil {
				t.Fatalf("reload merged layout: %v", err)
			}
			if !reflect.DeepEqual(restored, want) {
				t.Fatalf("SQLite reload changed exact layout: %+v", restored)
			}

			eligible := map[ContextID]struct{}{testContextID: {}}
			selection, err := SelectRestoreWorkspace(live, loadedRegistry, restored, eligible, nil)
			if err != nil {
				t.Fatalf("select already restored workspace: %v", err)
			}
			if selection.Progress != nil || len(selection.Degradations) != 0 {
				t.Fatalf("matching layout was not idempotent: %+v", selection)
			}
			repeated, err := PreserveMissingPlacements(restored, captured, loadedRegistry)
			if err != nil || !reflect.DeepEqual(repeated, restored) {
				t.Fatalf("repeated merge changed persisted layout: %+v, %v", repeated, err)
			}

			test.change(workspace)
			selection, err = SelectRestoreWorkspace(live, loadedRegistry, restored, eligible, nil)
			if err != nil {
				t.Fatalf("select changed workspace: %v", err)
			}
			if selection.Progress == nil || selection.Progress.Phase != test.wantPhase || len(selection.Degradations) != 0 {
				t.Fatalf("fresh exact layout was not selected for restore: %+v", selection)
			}
			step, err := PlanWorkspaceRestoreStep(live, loadedRegistry, restored.Workspaces[0], *selection.Progress, nil)
			if err != nil {
				t.Fatalf("plan restore action: %v", err)
			}
			if step.Action == nil || step.Action.Kind != test.wantAction || step.Action.ContextID == archivedID {
				t.Fatalf("unexpected restore action: %+v", step)
			}
		})
	}

}

func TestLayoutAcceptanceMixedArchivedWindowKeepsPlacementOnlyAfterReload(t *testing.T) {
	archivedID := ContextID("6ba7b812-9dad-41d1-80b4-00c04fd430c8")
	registry := registryWithContexts(testContextID, secondContextID)
	archived := applicationContextWithID(archivedID, "org.example.Archived")
	archived.State = ContextArchived
	registry.Contexts = append(registry.Contexts, archived)
	stateRoot := t.TempDir()
	if err := os.Chmod(stateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := RegistryStoreFor(stateRoot).Save(registry); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	if err := LayoutStoreFor(stateRoot).Save(placementSnapshot("98: mixed", testContextID, secondContextID, archivedID)); err != nil {
		t.Fatalf("save stale layout: %v", err)
	}
	var loadedRegistry Registry
	if err := RegistryStoreFor(stateRoot).LoadInto(&loadedRegistry); err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	if !reflect.DeepEqual(loadedRegistry, registry) {
		t.Fatalf("archived context was removed from registry: %+v", loadedRegistry)
	}
	var previous LayoutSnapshot
	if err := LayoutStoreFor(stateRoot).LoadInto(&previous); err != nil {
		t.Fatalf("reload stale layout: %v", err)
	}
	// The archived mark is still in the compositor tree, but its leaf is now
	// unregistered for capture. The active siblings must not become an exact
	// layout merely because the old placement row contained the archived ID.
	live := restoreTree(restoreWorkspace("98: mixed", "splith",
		managedTreeLeaf(t, 11, testContextID, nil, false),
		managedTreeLeaf(t, 12, archivedID, nil, false),
		managedTreeLeaf(t, 13, secondContextID, nil, false)))
	captured, err := CaptureLayout(live, loadedRegistry)
	if err != nil {
		t.Fatalf("capture mixed tree: %v", err)
	}
	merged, err := PreserveMissingPlacements(previous, captured, loadedRegistry)
	if err != nil {
		t.Fatalf("merge mixed tree: %v", err)
	}
	want, err := canonicalSnapshot(placementSnapshot("98: mixed", testContextID, secondContextID))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("mixed tree was promoted or archived placement retained: %+v", merged)
	}
	if err := LayoutStoreFor(stateRoot).Save(merged); err != nil {
		t.Fatalf("persist mixed layout: %v", err)
	}
	var restored LayoutSnapshot
	if err := LayoutStoreFor(stateRoot).LoadInto(&restored); err != nil {
		t.Fatalf("reload mixed layout: %v", err)
	}
	if !reflect.DeepEqual(restored, want) {
		t.Fatalf("mixed degradation did not survive SQLite reload: %+v", restored)
	}
	selection, err := SelectRestoreWorkspace(live, loadedRegistry, restored, map[ContextID]struct{}{testContextID: {}}, nil)
	if err != nil || selection.Progress != nil || len(selection.Degradations) != 0 {
		t.Fatalf("placement-only workspace was selected for exact restore: %+v, %v", selection, err)
	}
	repeated, err := PreserveMissingPlacements(restored, captured, loadedRegistry)
	if err != nil || !reflect.DeepEqual(repeated, restored) {
		t.Fatalf("repeated mixed merge changed layout: %+v, %v", repeated, err)
	}

	coordinator, err := NewApplicationRestoreCoordinator(
		stringsOfLength("8", 64), ApplicationSessionState{}, time.Unix(1000, 0),
		ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 2},
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := coordinator.Plan(loadedRegistry, nil, time.Unix(1010, 0))
	if err != nil || len(plan.Launch) != 0 || len(plan.DesiredOpen) != 0 {
		t.Fatalf("archived registry application received lifecycle work: %+v, %v", plan, err)
	}
}
