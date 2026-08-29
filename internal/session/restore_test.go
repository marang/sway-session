package session

import (
	"reflect"
	"strings"
	"testing"

	"github.com/marang/sway-title-animator/internal/swayipc"
)

func TestSelectRestoreWorkspaceRequiresNewlyMarkedContext(t *testing.T) {
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	root := restoreTree(restoreWorkspace("2", "splitv",
		managedTreeLeaf(t, 11, testContextID, floatPointer(0.5), false),
		managedTreeLeaf(t, 12, secondContextID, floatPointer(0.5), false),
	))
	registry := registryWithContexts(testContextID, secondContextID)

	selection, err := SelectRestoreWorkspace(root, registry, layoutSnapshot(desired), nil, nil)
	if err != nil {
		t.Fatalf("select restore without eligible context: %v", err)
	}
	if selection.Progress != nil {
		t.Fatalf("pre-existing marked windows were selected for restore: %+v", selection.Progress)
	}

	selection, err = SelectRestoreWorkspace(
		root,
		registry,
		layoutSnapshot(desired),
		map[ContextID]struct{}{testContextID: {}},
		nil,
	)
	if err != nil {
		t.Fatalf("select eligible restore: %v", err)
	}
	if selection.Progress == nil || selection.Progress.Phase != RestoreStageOut {
		t.Fatalf("structurally different workspace did not stage: %+v", selection.Progress)
	}
}

func TestSelectRestoreWorkspaceRecoversTransientStagingAfterRestart(t *testing.T) {
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	root := restoreTree(
		restoreWorkspace("2", "splith", managedTreeLeaf(t, 11, testContextID, nil, false)),
		restoreWorkspace(RestoreStagingWorkspace, "splith", managedTreeLeaf(t, 12, secondContextID, nil, false)),
	)

	selection, err := SelectRestoreWorkspace(
		root,
		registryWithContexts(testContextID, secondContextID),
		layoutSnapshot(desired),
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("select interrupted restore: %v", err)
	}
	if selection.Progress == nil || selection.Progress.Phase != RestoreStageOut {
		t.Fatalf("interrupted staging was not recovered first: %+v", selection.Progress)
	}
}

func TestSelectRestoreWorkspaceBuildsDetailsWithoutRestaging(t *testing.T) {
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	desired.Tiling.Children[0].Proportion = 0.7
	desired.Tiling.Children[1].Proportion = 0.3
	root := restoreTree(restoreWorkspace("2", "splith",
		managedTreeLeaf(t, 11, testContextID, floatPointer(0.5), false),
		managedTreeLeaf(t, 12, secondContextID, floatPointer(0.5), false),
	))

	selection, err := SelectRestoreWorkspace(
		root,
		registryWithContexts(testContextID, secondContextID),
		layoutSnapshot(desired),
		map[ContextID]struct{}{testContextID: {}},
		nil,
	)
	if err != nil {
		t.Fatalf("select detail-only restore: %v", err)
	}
	if selection.Progress == nil || selection.Progress.Phase != RestoreBuild {
		t.Fatalf("detail-only restore unnecessarily staged windows: %+v", selection.Progress)
	}
}

func TestSelectRestoreWorkspaceDegradesMixedWorkspace(t *testing.T) {
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	root := restoreTree(restoreWorkspace("2", "splith",
		managedTreeLeaf(t, 11, testContextID, nil, false),
		&swayipc.TreeNode{ID: 99, Type: "con", AppID: stringPointer("firefox")},
		managedTreeLeaf(t, 12, secondContextID, nil, false),
	))

	selection, err := SelectRestoreWorkspace(
		root,
		registryWithContexts(testContextID, secondContextID),
		layoutSnapshot(desired),
		map[ContextID]struct{}{testContextID: {}},
		nil,
	)
	if err != nil {
		t.Fatalf("select mixed workspace: %v", err)
	}
	if selection.Progress != nil || len(selection.Degradations) != 1 ||
		!strings.Contains(selection.Degradations[0].Reason, "placement-only") {
		t.Fatalf("mixed workspace was not explicitly degraded: %+v", selection)
	}
}

func TestSelectRestoreWorkspaceDegradesExtraManagedContext(t *testing.T) {
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	root := restoreTree(restoreWorkspace("2", "splith",
		managedTreeLeaf(t, 11, testContextID, nil, false),
		managedTreeLeaf(t, 12, secondContextID, nil, false),
		managedTreeLeaf(t, 13, thirdContextID, nil, false),
	))

	selection, err := SelectRestoreWorkspace(
		root,
		registryWithContexts(testContextID, secondContextID, thirdContextID),
		layoutSnapshot(desired),
		map[ContextID]struct{}{testContextID: {}},
		nil,
	)
	if err != nil {
		t.Fatalf("select workspace with extra managed context: %v", err)
	}
	if selection.Progress != nil || len(selection.Degradations) != 1 ||
		!strings.Contains(selection.Degradations[0].Reason, "outside") {
		t.Fatalf("extra managed context was not safely degraded: %+v", selection)
	}
}

func TestSelectRestoreWorkspaceDegradesSingletonGroup(t *testing.T) {
	desired := WorkspaceLayout{
		Name:        "2",
		RestoreMode: WorkspaceRestoreLayout,
		Tiling: &LayoutNode{
			Layout:   LayoutTabbed,
			Children: []LayoutNode{{ContextID: contextIDPointer(testContextID)}},
		},
	}
	root := restoreTree(restoreWorkspace("2", "splith", managedTreeLeaf(t, 11, testContextID, nil, false)))

	selection, err := SelectRestoreWorkspace(
		root,
		registryWithContexts(testContextID),
		layoutSnapshot(desired),
		map[ContextID]struct{}{testContextID: {}},
		nil,
	)
	if err != nil {
		t.Fatalf("select singleton group: %v", err)
	}
	if selection.Progress != nil || len(selection.Degradations) != 1 ||
		!strings.Contains(selection.Degradations[0].Reason, "single-child") {
		t.Fatalf("unrestorable singleton group was not degraded: %+v", selection)
	}
}

func TestSelectRestoreWorkspaceResumesExactTreeOnlyToCleanMarks(t *testing.T) {
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	group := &swayipc.TreeNode{
		ID: 20, Type: "con", Layout: "splith", Marks: []string{temporaryMark("2", "t")},
		Nodes: []*swayipc.TreeNode{
			managedTreeLeaf(t, 11, testContextID, nil, false),
			managedTreeLeaf(t, 12, secondContextID, nil, false),
		},
	}
	root := restoreTree(restoreWorkspace("2", "splith", group))

	selection, err := SelectRestoreWorkspace(
		root,
		registryWithContexts(testContextID, secondContextID),
		layoutSnapshot(desired),
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("select exact interrupted tree: %v", err)
	}
	if selection.Progress == nil || selection.Progress.Phase != RestoreBuild {
		t.Fatalf("exact interrupted tree was destructively restaged: %+v", selection.Progress)
	}
}

func TestPlanWorkspaceRestoreStagesFullscreenFromReservedWorkspace(t *testing.T) {
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	fullscreen := managedTreeLeaf(t, 12, secondContextID, nil, false)
	fullscreen.FullscreenMode = 1
	target := restoreWorkspace("2", "splith", managedTreeLeaf(t, 11, testContextID, nil, false))
	target.FullscreenMode = 1
	root := restoreTree(
		target,
		restoreWorkspace(RestoreStagingWorkspace, "splith", fullscreen),
	)

	step := requireRestoreStep(t, root, desired, RestoreProgress{Workspace: "2", Phase: RestoreStageOut})
	if step.Action == nil || step.Action.Kind != RestoreSetFullscreen || step.Action.ContainerID != 12 || !step.Action.Structural {
		t.Fatalf("staged fullscreen container was not normalized: %+v", step.Action)
	}
}

func TestPlanWorkspaceRestoreStagesAndTransitionsToBuild(t *testing.T) {
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	root := restoreTree(restoreWorkspace("2", "splith",
		managedTreeLeaf(t, 11, testContextID, nil, false),
		managedTreeLeaf(t, 12, secondContextID, nil, false),
	))
	progress := RestoreProgress{Workspace: "2", Phase: RestoreStageOut}
	step := requireRestoreStep(t, root, desired, progress)
	if step.Action == nil || step.Action.Kind != RestoreMoveWorkspace || step.Action.Target != RestoreStagingWorkspace {
		t.Fatalf("unexpected first staging step: %+v", step)
	}

	staged := restoreTree(restoreWorkspace(RestoreStagingWorkspace, "splith",
		managedTreeLeaf(t, 11, testContextID, nil, false),
		managedTreeLeaf(t, 12, secondContextID, nil, false),
	))
	step = requireRestoreStep(t, staged, desired, RestoreProgress{Workspace: "2", Phase: RestoreStageOut, Steps: 2})
	if step.Action != nil || step.Progress.Phase != RestoreBuild || step.Done {
		t.Fatalf("staging did not transition without a mutation: %+v", step)
	}
}

func TestPlanWorkspaceRestoreBuildsNestedTreeTopDown(t *testing.T) {
	desired := nestedWorkspace()
	registry := registryWithContexts(testContextID, secondContextID, thirdContextID)
	progress := RestoreProgress{Workspace: "2", Phase: RestoreBuild}

	flat := restoreTree(restoreWorkspace("2", "splith",
		managedTreeLeaf(t, 11, testContextID, nil, false),
		managedTreeLeaf(t, 12, secondContextID, nil, false),
		managedTreeLeaf(t, 13, thirdContextID, nil, false),
	))
	step, err := PlanWorkspaceRestoreStep(flat, registry, desired, progress, nil)
	if err != nil {
		t.Fatalf("plan root split: %v", err)
	}
	if step.Action == nil || step.Action.Kind != RestoreSplit || step.Action.ContainerID != 11 {
		t.Fatalf("unexpected root split action: %+v", step.Action)
	}

	rootMark := temporaryMark("2", "t")
	unmarkedRoot := &swayipc.TreeNode{
		ID: 20, Type: "con", Layout: "splith",
		Nodes: []*swayipc.TreeNode{managedTreeLeaf(t, 11, testContextID, nil, false)},
	}
	afterSplit := restoreTree(restoreWorkspace("2", "splith",
		unmarkedRoot,
		managedTreeLeaf(t, 12, secondContextID, nil, false),
		managedTreeLeaf(t, 13, thirdContextID, nil, false),
	))
	step, err = PlanWorkspaceRestoreStep(afterSplit, registry, desired, progress, nil)
	if err != nil {
		t.Fatalf("plan root temporary mark: %v", err)
	}
	if step.Action == nil || step.Action.Kind != RestoreAddTemporaryMark || step.Action.ContainerID != 20 || step.Action.Target != rootMark {
		t.Fatalf("new root group was not marked: %+v", step.Action)
	}

	unmarkedRoot.Marks = []string{rootMark}
	step, err = PlanWorkspaceRestoreStep(afterSplit, registry, desired, progress, nil)
	if err != nil {
		t.Fatalf("plan move into root group: %v", err)
	}
	if step.Action == nil || step.Action.Kind != RestoreMoveToMark || step.Action.ContainerID != 12 || step.Action.Target != rootMark {
		t.Fatalf("next desired child was not moved into the root group: %+v", step.Action)
	}

	rootGroup := &swayipc.TreeNode{
		ID: 20, Type: "con", Layout: "splith", Marks: []string{rootMark},
		Nodes: []*swayipc.TreeNode{
			managedTreeLeaf(t, 11, testContextID, nil, false),
			managedTreeLeaf(t, 12, secondContextID, nil, false),
		},
	}
	partial := restoreTree(restoreWorkspace("2", "splith", rootGroup, managedTreeLeaf(t, 13, thirdContextID, nil, false)))
	step, err = PlanWorkspaceRestoreStep(partial, registry, desired, progress, nil)
	if err != nil {
		t.Fatalf("plan nested split while a later descendant is still direct: %v", err)
	}
	if step.Action == nil || step.Action.Kind != RestoreSplit || step.Action.ContainerID != 12 {
		t.Fatalf("unexpected nested split action: %+v", step.Action)
	}

	nestedMark := temporaryMark("2", "t_1")
	nested := &swayipc.TreeNode{
		ID: 21, Type: "con", Layout: "splith", Marks: []string{nestedMark},
		Nodes: []*swayipc.TreeNode{
			managedTreeLeaf(t, 12, secondContextID, nil, false),
			managedTreeLeaf(t, 13, thirdContextID, nil, false),
		},
	}
	rootGroup.Nodes = []*swayipc.TreeNode{managedTreeLeaf(t, 11, testContextID, nil, false), nested}
	completeStructure := restoreTree(restoreWorkspace("2", "splith", rootGroup))
	step, err = PlanWorkspaceRestoreStep(completeStructure, registry, desired, progress, nil)
	if err != nil {
		t.Fatalf("plan nested layout: %v", err)
	}
	if step.Action == nil || step.Action.Kind != RestoreSetLayout || step.Action.Layout != LayoutTabbed {
		t.Fatalf("nested tabbed layout was not planned: %+v", step.Action)
	}
}

func TestPlanWorkspaceRestorePlansVerticalAndStackedGroups(t *testing.T) {
	desired := nestedWorkspace()
	desired.Tiling.Layout = LayoutSplitVertical
	desired.Tiling.Children[1].Layout = LayoutStacked
	registry := registryWithContexts(testContextID, secondContextID, thirdContextID)
	progress := RestoreProgress{Workspace: "2", Phase: RestoreBuild}
	flat := restoreTree(restoreWorkspace("2", "splith",
		managedTreeLeaf(t, 11, testContextID, nil, false),
		managedTreeLeaf(t, 12, secondContextID, nil, false),
		managedTreeLeaf(t, 13, thirdContextID, nil, false),
	))
	step, err := PlanWorkspaceRestoreStep(flat, registry, desired, progress, nil)
	if err != nil {
		t.Fatalf("plan vertical root split: %v", err)
	}
	if step.Action == nil || step.Action.Kind != RestoreSplit || step.Action.Layout != LayoutSplitVertical {
		t.Fatalf("vertical split was not planned: %+v", step.Action)
	}

	rootGroup := &swayipc.TreeNode{
		ID: 20, Type: "con", Layout: "splitv", Marks: []string{temporaryMark("2", "t")},
		Nodes: []*swayipc.TreeNode{
			managedTreeLeaf(t, 11, testContextID, nil, false),
			{
				ID: 21, Type: "con", Layout: "splith", Marks: []string{temporaryMark("2", "t_1")},
				Nodes: []*swayipc.TreeNode{
					managedTreeLeaf(t, 12, secondContextID, nil, false),
					managedTreeLeaf(t, 13, thirdContextID, nil, false),
				},
			},
		},
	}
	step, err = PlanWorkspaceRestoreStep(
		restoreTree(restoreWorkspace("2", "splith", rootGroup)),
		registry,
		desired,
		progress,
		nil,
	)
	if err != nil {
		t.Fatalf("plan stacked nested group: %v", err)
	}
	if step.Action == nil || step.Action.Kind != RestoreSetLayout || step.Action.Layout != LayoutStacked {
		t.Fatalf("stacked nested layout was not planned: %+v", step.Action)
	}
}

func TestPlanWorkspaceRestoreDetails(t *testing.T) {
	registry := registryWithContexts(testContextID, secondContextID)
	rootMark := temporaryMark("2", "t")
	firstPercent := 0.5
	secondPercent := 0.5
	root := restoreTree(restoreWorkspace("2", "splith", &swayipc.TreeNode{
		ID: 20, Type: "con", Layout: "splith", Marks: []string{rootMark},
		Nodes: []*swayipc.TreeNode{
			managedTreeLeaf(t, 11, testContextID, &firstPercent, false),
			managedTreeLeaf(t, 12, secondContextID, &secondPercent, false),
		},
	}))
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	desired.Tiling.Children[0].Proportion = 0.7
	desired.Tiling.Children[1].Proportion = 0.3
	step, err := PlanWorkspaceRestoreStep(root, registry, desired, RestoreProgress{Workspace: "2", Phase: RestoreBuild}, nil)
	if err != nil {
		t.Fatalf("plan proportions: %v", err)
	}
	if step.Action == nil || step.Action.Kind != RestoreSetProportion || step.Action.Axis != "width" || step.Action.Amount != 70 {
		t.Fatalf("unexpected proportion action: %+v", step.Action)
	}

	desired.Tiling.Children[0].Proportion = 0.5
	desired.Tiling.Children[1].Proportion = 0.5
	desired.Tiling.Children[1].Fullscreen = FullscreenWorkspace
	step, err = PlanWorkspaceRestoreStep(root, registry, desired, RestoreProgress{Workspace: "2", Phase: RestoreBuild}, nil)
	if err != nil {
		t.Fatalf("plan fullscreen: %v", err)
	}
	if step.Action == nil || step.Action.Kind != RestoreSetFullscreen || step.Action.ContainerID != 12 || step.Action.Fullscreen != FullscreenWorkspace {
		t.Fatalf("unexpected fullscreen action: %+v", step.Action)
	}
}

func TestPlanWorkspaceRestoreUsesExistingUnmarkedStructureForDetails(t *testing.T) {
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	desired.Tiling.Children[0].Proportion = 0.7
	desired.Tiling.Children[1].Proportion = 0.3
	half := 0.5
	root := restoreTree(restoreWorkspace("2", "splith",
		managedTreeLeaf(t, 11, testContextID, &half, false),
		managedTreeLeaf(t, 12, secondContextID, &half, false),
	))

	step := requireRestoreStep(t, root, desired, RestoreProgress{Workspace: "2", Phase: RestoreBuild})
	if step.Action == nil || step.Action.Kind != RestoreSetProportion {
		t.Fatalf("existing exact structure was rebuilt instead of adjusted: %+v", step.Action)
	}
}

func TestPlanWorkspaceRestoreKeepsTilingAndDesiredFloatingGroupsDistinct(t *testing.T) {
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	desired.Floating = []LayoutNode{{
		ContextID: contextIDPointer(thirdContextID),
		Geometry:  &Geometry{X: 50, Y: 60, Width: 400, Height: 300},
	}}
	half := 0.5
	floating := managedTreeLeaf(t, 13, thirdContextID, nil, false)
	floating.Type = "floating_con"
	floating.Rect = swayipc.Rect{X: 50, Y: 60, Width: 400, Height: 300}
	workspace := restoreWorkspace("2", "splith",
		managedTreeLeaf(t, 11, testContextID, &half, false),
		managedTreeLeaf(t, 12, secondContextID, &half, false),
	)
	workspace.Rect = swayipc.Rect{Width: 1920, Height: 1080}
	workspace.FloatingNodes = []*swayipc.TreeNode{floating}

	step := requireRestoreStep(t, restoreTree(workspace), desired, RestoreProgress{Workspace: "2", Phase: RestoreBuild})
	if step.Action != nil || !step.Done {
		t.Fatalf("matching tiling plus floating layout was needlessly rebuilt: %+v", step)
	}
}

func TestPlanWorkspaceRestoreIgnoresWorkspaceFullscreenAggregate(t *testing.T) {
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	desired.Tiling.Children[1].Fullscreen = FullscreenWorkspace
	first := managedTreeLeaf(t, 11, testContextID, nil, false)
	second := managedTreeLeaf(t, 12, secondContextID, nil, false)
	second.FullscreenMode = 1
	workspace := restoreWorkspace("2", "splith", first, second)
	workspace.FullscreenMode = 1

	step := requireRestoreStep(t, restoreTree(workspace), desired, RestoreProgress{Workspace: "2", Phase: RestoreBuild})
	if step.Action != nil || !step.Done {
		t.Fatalf("workspace fullscreen aggregate caused a disable loop: %+v", step)
	}
}

func TestPlanWorkspaceRestoreCreatesContainerForRootFullscreenState(t *testing.T) {
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	desired.Tiling.Fullscreen = FullscreenWorkspace
	workspace := restoreWorkspace("2", "splith",
		managedTreeLeaf(t, 11, testContextID, nil, false),
		managedTreeLeaf(t, 12, secondContextID, nil, false),
	)
	workspace.FullscreenMode = 1

	step := requireRestoreStep(t, restoreTree(workspace), desired, RestoreProgress{Workspace: "2", Phase: RestoreBuild})
	if step.Action == nil || step.Action.Kind != RestoreSplit || step.Action.ContainerID != 11 {
		t.Fatalf("root fullscreen state was incorrectly assigned to the workspace aggregate: %+v", step.Action)
	}
}

func TestPlanWorkspaceRestoreCleanupFailureCanBeSkippedWithoutRollback(t *testing.T) {
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	mark := temporaryMark("2", "t")
	group := &swayipc.TreeNode{
		ID: 20, Type: "con", Layout: "splith", Marks: []string{mark},
		Nodes: []*swayipc.TreeNode{
			managedTreeLeaf(t, 11, testContextID, nil, false),
			managedTreeLeaf(t, 12, secondContextID, nil, false),
		},
	}
	root := restoreTree(restoreWorkspace("2", "splith", group))
	step := requireRestoreStep(t, root, desired, RestoreProgress{Workspace: "2", Phase: RestoreBuild})
	if step.Action == nil || step.Action.Kind != RestoreRemoveMark || step.Action.Structural {
		t.Fatalf("temporary-mark cleanup was treated as destructive structure: %+v", step.Action)
	}

	skipped := map[string]struct{}{step.Action.Key(): {}}
	step, err := PlanWorkspaceRestoreStep(
		root,
		registryWithContexts(testContextID, secondContextID),
		desired,
		RestoreProgress{Workspace: "2", Phase: RestoreBuild, Steps: 1},
		skipped,
	)
	if err != nil {
		t.Fatalf("plan after skipped cleanup: %v", err)
	}
	if step.Action != nil || !step.Done {
		t.Fatalf("failed mark cleanup prevented bounded degradation: %+v", step)
	}
}

func TestPlanWorkspaceRestoreRejectsSpoofedTemporaryMark(t *testing.T) {
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	spoofed := &swayipc.TreeNode{
		ID:     99,
		Type:   "con",
		Layout: "splith",
		Marks:  []string{temporaryMark("2", "t")},
		Nodes:  []*swayipc.TreeNode{{ID: 100, Type: "con", AppID: stringPointer("firefox")}},
	}
	root := restoreTree(restoreWorkspace("2", "splith",
		managedTreeLeaf(t, 11, testContextID, nil, false),
		managedTreeLeaf(t, 12, secondContextID, nil, false),
		spoofed,
	))

	_, err := PlanWorkspaceRestoreStep(
		root,
		registryWithContexts(testContextID, secondContextID),
		desired,
		RestoreProgress{Workspace: "2", Phase: RestoreBuild},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "unregistered window") {
		t.Fatalf("spoofed restore target was not rejected before mutation: %v", err)
	}
}

func TestPlanWorkspaceRestoreRejectsNewMixedFloatingSubtree(t *testing.T) {
	desired := WorkspaceLayout{
		Name:        "2",
		RestoreMode: WorkspaceRestoreLayout,
		Floating: []LayoutNode{{
			ContextID: contextIDPointer(testContextID),
			Geometry:  &Geometry{X: 20, Y: 30, Width: 500, Height: 400},
		}},
	}
	group := &swayipc.TreeNode{
		ID: 20, Type: "floating_con", Layout: "splith", Rect: swayipc.Rect{X: 20, Y: 30, Width: 500, Height: 400},
		Nodes: []*swayipc.TreeNode{
			managedTreeLeaf(t, 11, testContextID, nil, false),
			{ID: 99, Type: "con", AppID: stringPointer("dialog")},
		},
	}
	workspace := restoreWorkspace("2", "splith")
	workspace.FloatingNodes = []*swayipc.TreeNode{group}

	_, err := PlanWorkspaceRestoreStep(
		restoreTree(workspace),
		registryWithContexts(testContextID),
		desired,
		RestoreProgress{Workspace: "2", Phase: RestoreBuild},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "unregistered window") {
		t.Fatalf("new mixed floating subtree was not rejected before mutation: %v", err)
	}
}

func TestPlanWorkspaceRestoreRejectsNewUnexpectedManagedContext(t *testing.T) {
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	root := restoreTree(restoreWorkspace("2", "splith",
		managedTreeLeaf(t, 11, testContextID, nil, false),
		managedTreeLeaf(t, 12, secondContextID, nil, false),
		managedTreeLeaf(t, 13, thirdContextID, nil, false),
	))

	_, err := PlanWorkspaceRestoreStep(
		root,
		registryWithContexts(testContextID, secondContextID, thirdContextID),
		desired,
		RestoreProgress{Workspace: "2", Phase: RestoreBuild},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("new managed context was not rejected before mutation: %v", err)
	}
}

func TestPlanWorkspaceRestoreFloatingGeometryIsClamped(t *testing.T) {
	desired := WorkspaceLayout{
		Name:        "2",
		RestoreMode: WorkspaceRestoreLayout,
		Floating: []LayoutNode{{
			ContextID: contextIDPointer(testContextID),
			Geometry:  &Geometry{X: -100, Y: 900, Width: 2000, Height: 1200},
		}},
	}
	leaf := managedTreeLeaf(t, 11, testContextID, nil, false)
	leaf.Rect = swayipc.Rect{X: 10, Y: 20, Width: 300, Height: 200}
	workspace := restoreWorkspace("2", "splith")
	workspace.Rect = swayipc.Rect{X: 100, Y: 200, Width: 1000, Height: 700}
	workspace.FloatingNodes = []*swayipc.TreeNode{leaf}

	step := requireRestoreStep(t, restoreTree(workspace), desired, RestoreProgress{Workspace: "2", Phase: RestoreBuild})
	want := Geometry{X: 100, Y: 200, Width: 1000, Height: 700}
	if step.Action == nil || step.Action.Kind != RestoreResizeFloating || !reflect.DeepEqual(step.Action.Geometry, want) {
		t.Fatalf("floating geometry was not clamped before resize: %+v", step.Action)
	}
}

func TestPlanWorkspaceRestoreMovesFloatingPositionAndRestoresFocus(t *testing.T) {
	desiredFloating := WorkspaceLayout{
		Name:        "2",
		RestoreMode: WorkspaceRestoreLayout,
		Floating: []LayoutNode{{
			ContextID: contextIDPointer(testContextID),
			Geometry:  &Geometry{X: 300, Y: 250, Width: 400, Height: 300},
		}},
	}
	leaf := managedTreeLeaf(t, 11, testContextID, nil, false)
	leaf.Type = "floating_con"
	leaf.Rect = swayipc.Rect{X: 20, Y: 30, Width: 400, Height: 300}
	workspace := restoreWorkspace("2", "splith")
	workspace.Rect = swayipc.Rect{Width: 1920, Height: 1080}
	workspace.FloatingNodes = []*swayipc.TreeNode{leaf}
	step := requireRestoreStep(t, restoreTree(workspace), desiredFloating, RestoreProgress{Workspace: "2", Phase: RestoreBuild})
	if step.Action == nil || step.Action.Kind != RestoreMoveFloating || step.Action.Geometry.X != 300 || step.Action.Geometry.Y != 250 {
		t.Fatalf("floating position was not planned: %+v", step.Action)
	}

	desiredFocus := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	desiredFocus.FocusedContext = contextIDPointer(secondContextID)
	first := managedTreeLeaf(t, 21, testContextID, nil, true)
	second := managedTreeLeaf(t, 22, secondContextID, nil, false)
	step = requireRestoreStep(
		t,
		restoreTree(restoreWorkspace("2", "splith", first, second)),
		desiredFocus,
		RestoreProgress{Workspace: "2", Phase: RestoreBuild},
	)
	if step.Action == nil || step.Action.Kind != RestoreFocus || step.Action.ContainerID != 22 {
		t.Fatalf("saved focus was not planned: %+v", step.Action)
	}
}

func TestPlanWorkspaceRestoreAcceptsDecoratedOuterFloatingGeometry(t *testing.T) {
	desired := WorkspaceLayout{
		Name:        "2",
		RestoreMode: WorkspaceRestoreLayout,
		Floating: []LayoutNode{{
			ContextID: contextIDPointer(testContextID),
			Geometry:  &Geometry{X: 100, Y: 120, Width: 420, Height: 260},
		}},
		FocusedContext: contextIDPointer(testContextID),
	}
	leaf := managedTreeLeaf(t, 11, testContextID, nil, true)
	leaf.Type = "floating_con"
	leaf.Rect = swayipc.Rect{X: 100, Y: 147, Width: 420, Height: 233}
	leaf.DecoRect = swayipc.Rect{X: 100, Y: 120, Width: 420, Height: 27}
	workspace := restoreWorkspace("2", "splith")
	workspace.Rect = swayipc.Rect{Width: 1920, Height: 1080}
	workspace.FloatingNodes = []*swayipc.TreeNode{leaf}

	step, err := PlanWorkspaceRestoreStep(
		restoreTree(workspace),
		registryWithContexts(testContextID),
		desired,
		RestoreProgress{Workspace: "2", Phase: RestoreBuild},
		nil,
	)
	if err != nil {
		t.Fatalf("plan decorated floating geometry: %v", err)
	}
	if !step.Done || step.Action != nil {
		t.Fatalf("already converged decorated geometry planned another action: %+v", step)
	}
}

func TestPlanWorkspaceRestoreUsesUnderlyingFullscreenProportion(t *testing.T) {
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	desired.Tiling.Children[0].Proportion = 0.5
	desired.Tiling.Children[0].Fullscreen = FullscreenWorkspace
	desired.Tiling.Children[1].Proportion = 0.5
	half := 0.5
	fullscreenPercent := 1.0
	first := managedTreeLeaf(t, 11, testContextID, &fullscreenPercent, true)
	first.FullscreenMode = 1
	second := managedTreeLeaf(t, 12, secondContextID, &half, false)

	step, err := PlanWorkspaceRestoreStep(
		restoreTree(restoreWorkspace("2", "splith", first, second)),
		registryWithContexts(testContextID, secondContextID),
		desired,
		RestoreProgress{Workspace: "2", Phase: RestoreBuild},
		nil,
	)
	if err != nil {
		t.Fatalf("plan fullscreen proportions: %v", err)
	}
	if !step.Done || step.Action != nil {
		t.Fatalf("fullscreen presentation percent prevented convergence: %+v", step)
	}
}

func TestPlanWorkspaceRestoreRollbackAndStepBound(t *testing.T) {
	desired := exactWorkspace("2", LayoutSplitHorizontal, testContextID, secondContextID)
	root := restoreTree(
		restoreWorkspace("2", "splith", managedTreeLeaf(t, 11, testContextID, nil, false)),
		restoreWorkspace(RestoreStagingWorkspace, "splith", managedTreeLeaf(t, 12, secondContextID, nil, false)),
	)
	step := requireRestoreStep(t, root, desired, RestoreProgress{Workspace: "2", Phase: RestoreRollbackOut})
	if step.Action == nil || step.Action.Target != RestoreStagingWorkspace || step.Action.ContainerID != 11 {
		t.Fatalf("rollback did not finish staging all contexts: %+v", step.Action)
	}

	staged := restoreTree(restoreWorkspace(RestoreStagingWorkspace, "splith",
		managedTreeLeaf(t, 11, testContextID, nil, false),
		managedTreeLeaf(t, 12, secondContextID, nil, false),
	))
	step = requireRestoreStep(t, staged, desired, RestoreProgress{Workspace: "2", Phase: RestoreRollbackOut})
	if step.Action != nil || step.Progress.Phase != RestoreRollbackIn {
		t.Fatalf("rollback-out did not transition: %+v", step)
	}

	_, err := PlanWorkspaceRestoreStep(
		root,
		registryWithContexts(testContextID, secondContextID),
		desired,
		RestoreProgress{Workspace: "2", Phase: RestoreBuild, Steps: maxRestoreSteps},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("restore step bound was not enforced: %v", err)
	}
}

func TestPlanWorkspaceRestoreRejectsInvalidDesiredTree(t *testing.T) {
	desired := WorkspaceLayout{Name: "2", RestoreMode: WorkspaceRestoreLayout}
	_, err := PlanWorkspaceRestoreStep(
		restoreTree(restoreWorkspace("2", "splith")),
		registryWithContexts(),
		desired,
		RestoreProgress{Workspace: "2", Phase: RestoreBuild},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "validate desired workspace") {
		t.Fatalf("invalid desired tree was not rejected before recursion: %v", err)
	}
}

func TestReservedStagingWorkspaceIsNeverCapturedOrPersisted(t *testing.T) {
	root := restoreTree(restoreWorkspace(RestoreStagingWorkspace, "splith",
		managedTreeLeaf(t, 11, testContextID, nil, false),
	))
	snapshot, err := CaptureLayout(root, registryWithContexts(testContextID))
	if err != nil {
		t.Fatalf("capture staging workspace: %v", err)
	}
	if len(snapshot.Workspaces) != 0 {
		t.Fatalf("staging workspace leaked into capture: %+v", snapshot)
	}
	invalid := layoutSnapshot(WorkspaceLayout{
		Name:        RestoreStagingWorkspace,
		RestoreMode: WorkspaceRestoreLayout,
		Tiling:      &LayoutNode{ContextID: contextIDPointer(testContextID)},
	})
	if err := invalid.Validate(); err == nil {
		t.Fatal("reserved staging workspace name was accepted by the schema")
	}
}

func requireRestoreStep(t *testing.T, root *swayipc.TreeNode, desired WorkspaceLayout, progress RestoreProgress) RestoreStep {
	t.Helper()
	step, err := PlanWorkspaceRestoreStep(root, registryWithContexts(workspaceContextIDs(desired)...), desired, progress, nil)
	if err != nil {
		t.Fatalf("plan restore step: %v", err)
	}
	return step
}

func exactWorkspace(name string, layout LayoutKind, ids ...ContextID) WorkspaceLayout {
	children := make([]LayoutNode, 0, len(ids))
	for _, id := range ids {
		id := id
		children = append(children, LayoutNode{ContextID: &id})
	}
	return WorkspaceLayout{
		Name:        name,
		RestoreMode: WorkspaceRestoreLayout,
		Tiling:      &LayoutNode{Layout: layout, Children: children},
	}
}

func nestedWorkspace() WorkspaceLayout {
	return WorkspaceLayout{
		Name:        "2",
		RestoreMode: WorkspaceRestoreLayout,
		Tiling: &LayoutNode{
			Layout: LayoutSplitHorizontal,
			Children: []LayoutNode{
				{ContextID: contextIDPointer(testContextID)},
				{
					Layout: LayoutTabbed,
					Children: []LayoutNode{
						{ContextID: contextIDPointer(secondContextID)},
						{ContextID: contextIDPointer(thirdContextID)},
					},
				},
			},
		},
	}
}

func layoutSnapshot(workspace WorkspaceLayout) LayoutSnapshot {
	return LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{workspace}}
}

func restoreWorkspace(name string, layout string, nodes ...*swayipc.TreeNode) *swayipc.TreeNode {
	return &swayipc.TreeNode{ID: nextRestoreTestID(), Name: name, Type: "workspace", Layout: layout, Nodes: nodes}
}

func restoreTree(workspaces ...*swayipc.TreeNode) *swayipc.TreeNode {
	return treeWithWorkspaces(workspaces...)
}

var restoreTestID int64 = 1000

func nextRestoreTestID() int64 {
	restoreTestID++
	return restoreTestID
}
