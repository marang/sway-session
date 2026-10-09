package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/marang/sway-session/internal/swayipc"
)

func capturedSingleTabWorkspace() WorkspaceLayout {
	return WorkspaceLayout{
		Name: "98", RestoreMode: WorkspaceRestoreLayout,
		Tiling: &LayoutNode{Layout: LayoutTabbed, Proportion: 1, Children: []LayoutNode{{ContextID: contextIDPointer(testContextID), Proportion: 1}}},
	}
}

func TestSelectRestoreWorkspaceSupportsCapturedSingleWindowTab(t *testing.T) {
	desired := capturedSingleTabWorkspace()
	root := restoreTree(restoreWorkspace("98", "splith", managedTreeLeaf(t, 11, testContextID, floatPointer(1), false)))
	selection, err := SelectRestoreWorkspace(root, registryWithContexts(testContextID), layoutSnapshot(desired), map[ContextID]struct{}{testContextID: {}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Progress == nil || len(selection.Degradations) != 0 {
		t.Fatalf("captured top-level single-window tab was not selected: %+v", selection)
	}
}

func TestPlanWorkspaceRestoreCreatesCapturedSingleWindowTabParent(t *testing.T) {
	desired := capturedSingleTabWorkspace()
	leaf := managedTreeLeaf(t, 11, testContextID, floatPointer(1), false)
	workspace := restoreWorkspace("98", "splith", leaf)
	root := restoreTree(workspace)
	registry := registryWithContexts(testContextID)
	step, err := PlanWorkspaceRestoreStep(root, registry, desired, RestoreProgress{Workspace: "98", Phase: RestoreBuild}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if step.Action == nil || step.Action.Kind != RestoreSetLayout || step.Action.Layout != LayoutTabbed || step.Action.ContainerID != leaf.ID || !step.Action.Structural {
		t.Fatalf("flat leaf was not given an action that creates its tab parent: %+v", step.Action)
	}
	// This is the shape produced by Sway's targeted layout-tabbed command.
	workspace.Nodes = []*swayipc.TreeNode{{ID: 20, Type: "con", Layout: "tabbed", Percent: floatPointer(1), Focus: []int64{leaf.ID}, Nodes: []*swayipc.TreeNode{leaf}}}
	workspace.Focus = []int64{20}
	complete, err := PlanWorkspaceRestoreStep(root, registry, desired, step.Progress, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !complete.Done || complete.Action != nil {
		t.Fatalf("freshly observed captured singleton tab did not converge: %+v", complete)
	}
}

func TestSelectRestoreWorkspaceKeepsUnsupportedSingletonShapesDegraded(t *testing.T) {
	for _, test := range []struct {
		name   string
		modify func(*WorkspaceLayout)
	}{
		{name: "horizontal_split", modify: func(workspace *WorkspaceLayout) { workspace.Tiling.Layout = LayoutSplitHorizontal }},
		{name: "vertical_split", modify: func(workspace *WorkspaceLayout) { workspace.Tiling.Layout = LayoutSplitVertical }},
		{name: "stacked", modify: func(workspace *WorkspaceLayout) { workspace.Tiling.Layout = LayoutStacked }},
		{name: "nested_tab", modify: func(workspace *WorkspaceLayout) {
			child := *workspace.Tiling
			workspace.Tiling = &LayoutNode{Layout: LayoutTabbed, Proportion: 1, Children: []LayoutNode{child}}
		}},
		{name: "floating_tab", modify: func(workspace *WorkspaceLayout) {
			floating := *workspace.Tiling
			floating.Proportion = 0
			floating.Geometry = &Geometry{X: 10, Y: 10, Width: 400, Height: 300}
			workspace.Tiling = nil
			workspace.Floating = []LayoutNode{floating}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			desired := capturedSingleTabWorkspace()
			test.modify(&desired)
			root := restoreTree(restoreWorkspace("98", "splith", managedTreeLeaf(t, 11, testContextID, floatPointer(1), false)))
			selection, err := SelectRestoreWorkspace(root, registryWithContexts(testContextID), layoutSnapshot(desired), map[ContextID]struct{}{testContextID: {}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if selection.Progress != nil || len(selection.Degradations) != 1 || !strings.Contains(selection.Degradations[0].Reason, "single-child") {
				t.Fatalf("unsupported singleton shape was selected: %+v", selection)
			}
		})
	}
}

func TestCapturedSingleWindowTabRestoreLeavesUnexpectedNeighborsUntouched(t *testing.T) {
	for _, test := range []struct {
		name     string
		neighbor func(*testing.T) *swayipc.TreeNode
	}{
		{name: "unmanaged_tiling", neighbor: func(*testing.T) *swayipc.TreeNode {
			return &swayipc.TreeNode{ID: 12, Type: "con", AppID: stringPointer("diagnostic.Unregistered"), Percent: floatPointer(0.5)}
		}},
		{name: "unexpected_managed_tiling", neighbor: func(t *testing.T) *swayipc.TreeNode {
			return managedTreeLeaf(t, 12, secondContextID, floatPointer(0.5), false)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			desired := capturedSingleTabWorkspace()
			root := restoreTree(restoreWorkspace("98", "splith", managedTreeLeaf(t, 11, testContextID, floatPointer(0.5), false), test.neighbor(t)))
			before, err := json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			registry := registryWithContexts(testContextID, secondContextID)
			selection, err := SelectRestoreWorkspace(root, registry, layoutSnapshot(desired), map[ContextID]struct{}{testContextID: {}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if selection.Progress != nil || len(selection.Degradations) != 1 {
				t.Fatalf("unexpected neighbor permitted reconstruction: %+v", selection)
			}
			step, err := PlanWorkspaceRestoreStep(root, registry, desired, RestoreProgress{Workspace: "98", Phase: RestoreBuild}, nil)
			if err == nil || step.Action != nil {
				t.Fatalf("unexpected neighbor received a restore action: step=%+v err=%v", step, err)
			}
			after, err := json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Error("restore selection or planning changed a mixed workspace")
			}
		})
	}
}
