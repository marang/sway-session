package session

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/marang/sway-title-animator/internal/swayipc"
)

const (
	secondContextID = ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	thirdContextID  = ContextID("6ba7b811-9dad-11d1-80b4-00c04fd430c8")
)

func TestCaptureLayoutPreservesNestedTilingAndFloatingState(t *testing.T) {
	half := 0.5
	root := treeWithWorkspaces(
		&swayipc.TreeNode{
			Name:   "2: work",
			Type:   "workspace",
			Layout: "splith",
			Nodes: []*swayipc.TreeNode{
				managedTreeLeaf(t, 11, testContextID, &half, true),
				{
					ID:      12,
					Layout:  "tabbed",
					Percent: &half,
					Nodes: []*swayipc.TreeNode{
						managedTreeLeaf(t, 13, secondContextID, floatPointer(1), false),
					},
				},
			},
			FloatingNodes: []*swayipc.TreeNode{
				{
					ID:     20,
					Layout: "splitv",
					Rect:   swayipc.Rect{X: 40, Y: 50, Width: 800, Height: 600},
					Nodes: []*swayipc.TreeNode{
						managedTreeLeaf(t, 21, thirdContextID, floatPointer(1), false),
					},
				},
			},
		},
	)

	snapshot, err := CaptureLayout(root, registryWithContexts(testContextID, secondContextID, thirdContextID))
	if err != nil {
		t.Fatalf("capture layout: %v", err)
	}
	if len(snapshot.Workspaces) != 1 {
		t.Fatalf("unexpected workspaces: %+v", snapshot.Workspaces)
	}
	workspace := snapshot.Workspaces[0]
	if workspace.RestoreMode != WorkspaceRestoreLayout || workspace.Tiling == nil {
		t.Fatalf("expected exact layout capture: %+v", workspace)
	}
	if workspace.Tiling.Layout != LayoutSplitHorizontal || len(workspace.Tiling.Children) != 2 {
		t.Fatalf("unexpected tiling root: %+v", workspace.Tiling)
	}
	if workspace.FocusedContext == nil || *workspace.FocusedContext != testContextID {
		t.Fatalf("unexpected focus: %+v", workspace.FocusedContext)
	}
	if len(workspace.Floating) != 1 || workspace.Floating[0].Geometry == nil || workspace.Floating[0].Geometry.Width != 800 {
		t.Fatalf("unexpected floating capture: %+v", workspace.Floating)
	}
}

func TestCaptureFullscreenPreservesUnderlyingSplitProportion(t *testing.T) {
	quarter := 0.25
	fullscreenPercent := 1.0
	fullscreen := managedTreeLeaf(t, 12, secondContextID, &fullscreenPercent, true)
	fullscreen.FullscreenMode = 1
	root := treeWithWorkspaces(&swayipc.TreeNode{
		Name:   "2: work",
		Type:   "workspace",
		Layout: "splith",
		Nodes: []*swayipc.TreeNode{
			managedTreeLeaf(t, 11, testContextID, &quarter, false),
			fullscreen,
			managedTreeLeaf(t, 13, thirdContextID, &quarter, false),
			managedTreeLeaf(t, 14, ContextID("6ba7b812-9dad-11d1-80b4-00c04fd430c8"), &quarter, false),
		},
	})

	snapshot, err := CaptureLayout(root, registryWithContexts(
		testContextID,
		secondContextID,
		thirdContextID,
		ContextID("6ba7b812-9dad-11d1-80b4-00c04fd430c8"),
	))
	if err != nil {
		t.Fatalf("capture fullscreen layout: %v", err)
	}
	children := snapshot.Workspaces[0].Tiling.Children
	if children[1].Proportion != quarter || children[1].Fullscreen != FullscreenWorkspace {
		t.Fatalf("fullscreen presentation percent replaced underlying split share: %+v", children[1])
	}
}

func TestCaptureFullscreenOmitsPresentationPercentWhenSiblingShareIsUnknown(t *testing.T) {
	fullscreenPercent := 1.0
	fullscreen := managedTreeLeaf(t, 11, testContextID, &fullscreenPercent, true)
	fullscreen.FullscreenMode = 1
	root := treeWithWorkspaces(&swayipc.TreeNode{
		Name:   "2: work",
		Type:   "workspace",
		Layout: "splith",
		Nodes: []*swayipc.TreeNode{
			fullscreen,
			managedTreeLeaf(t, 12, secondContextID, nil, false),
		},
	})

	snapshot, err := CaptureLayout(root, registryWithContexts(testContextID, secondContextID))
	if err != nil {
		t.Fatalf("capture fullscreen layout: %v", err)
	}
	if got := snapshot.Workspaces[0].Tiling.Children[0].Proportion; got != 0 {
		t.Fatalf("captured fullscreen presentation percent without sibling evidence: %v", got)
	}
}

func TestCaptureFloatingGeometryIncludesDecoration(t *testing.T) {
	leaf := managedTreeLeaf(t, 11, testContextID, nil, true)
	leaf.Rect = swayipc.Rect{X: 100, Y: 147, Width: 420, Height: 233}
	leaf.DecoRect = swayipc.Rect{X: 100, Y: 120, Width: 420, Height: 27}
	root := treeWithWorkspaces(&swayipc.TreeNode{
		Name:          "2: work",
		Type:          "workspace",
		Layout:        "splith",
		FloatingNodes: []*swayipc.TreeNode{leaf},
	})

	snapshot, err := CaptureLayout(root, registryWithContexts(testContextID))
	if err != nil {
		t.Fatalf("capture decorated floating geometry: %v", err)
	}
	want := &Geometry{X: 100, Y: 120, Width: 420, Height: 260}
	if got := snapshot.Workspaces[0].Floating[0].Geometry; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected outer floating geometry: got %+v want %+v", got, want)
	}
}

func TestCaptureLayoutUsesFocusOrderForInactiveWorkspace(t *testing.T) {
	root := treeWithWorkspaces(&swayipc.TreeNode{
		Name:   "2: inactive",
		Type:   "workspace",
		Layout: "splith",
		Focus:  []int64{12, 11},
		Nodes: []*swayipc.TreeNode{
			managedTreeLeaf(t, 11, testContextID, nil, false),
			{
				ID:     12,
				Layout: "tabbed",
				Focus:  []int64{14, 13},
				Nodes: []*swayipc.TreeNode{
					managedTreeLeaf(t, 13, secondContextID, nil, false),
					managedTreeLeaf(t, 14, thirdContextID, nil, false),
				},
			},
		},
	})

	snapshot, err := CaptureLayout(root, registryWithContexts(testContextID, secondContextID, thirdContextID))
	if err != nil {
		t.Fatalf("capture inactive workspace focus: %v", err)
	}
	focused := snapshot.Workspaces[0].FocusedContext
	if focused == nil || *focused != thirdContextID {
		t.Fatalf("inactive workspace focus path was not captured: %v", focused)
	}
}

func TestCaptureLayoutRejectsInvalidFocusPath(t *testing.T) {
	root := treeWithWorkspaces(&swayipc.TreeNode{
		Name:   "2",
		Type:   "workspace",
		Layout: "splith",
		Focus:  []int64{99},
		Nodes:  []*swayipc.TreeNode{managedTreeLeaf(t, 11, testContextID, nil, false)},
	})
	if _, err := CaptureLayout(root, registryWithContexts(testContextID)); err == nil {
		t.Fatal("expected invalid workspace focus path to fail closed")
	}
}

func TestCaptureLayoutDegradesMixedTilingWithoutCollapsingNeighbors(t *testing.T) {
	root := treeWithWorkspaces(&swayipc.TreeNode{
		Name:   "2: mixed",
		Type:   "workspace",
		Layout: "splith",
		Nodes: []*swayipc.TreeNode{
			managedTreeLeaf(t, 11, testContextID, nil, false),
			{ID: 12, Name: "browser", Type: "con", AppID: stringPointer("firefox")},
			managedTreeLeaf(t, 13, secondContextID, nil, false),
		},
	})

	snapshot, err := CaptureLayout(root, registryWithContexts(testContextID, secondContextID))
	if err != nil {
		t.Fatalf("capture mixed layout: %v", err)
	}
	want := []ContextID{testContextID, secondContextID}
	workspace := snapshot.Workspaces[0]
	if workspace.RestoreMode != WorkspaceRestorePlacementOnly || !reflect.DeepEqual(workspace.PlacementContexts, want) {
		t.Fatalf("mixed layout was not safely degraded: %+v", workspace)
	}
	if workspace.Tiling != nil || len(workspace.Floating) != 0 {
		t.Fatalf("placement-only capture retained layout state: %+v", workspace)
	}
}

func TestCaptureLayoutIgnoresIndependentUnmanagedFloatingWindow(t *testing.T) {
	root := treeWithWorkspaces(&swayipc.TreeNode{
		Name:   "3",
		Type:   "workspace",
		Layout: "splith",
		Nodes:  []*swayipc.TreeNode{managedTreeLeaf(t, 11, testContextID, nil, false)},
		FloatingNodes: []*swayipc.TreeNode{
			{ID: 12, Name: "dialog", Type: "con", AppID: stringPointer("dialog"), Rect: swayipc.Rect{Width: 200, Height: 100}},
		},
	})

	snapshot, err := CaptureLayout(root, registryWithContexts(testContextID))
	if err != nil {
		t.Fatalf("capture layout: %v", err)
	}
	if snapshot.Workspaces[0].RestoreMode != WorkspaceRestoreLayout || len(snapshot.Workspaces[0].Floating) != 0 {
		t.Fatalf("independent unmanaged floating window degraded managed tiling: %+v", snapshot.Workspaces[0])
	}
}

func TestCaptureLayoutDegradesMixedGroupedFloatingSubtree(t *testing.T) {
	root := treeWithWorkspaces(&swayipc.TreeNode{
		Name: "4",
		Type: "workspace",
		FloatingNodes: []*swayipc.TreeNode{
			{
				ID:     20,
				Layout: "splith",
				Rect:   swayipc.Rect{Width: 900, Height: 700},
				Nodes: []*swayipc.TreeNode{
					managedTreeLeaf(t, 21, testContextID, nil, false),
					{ID: 22, Name: "browser", Type: "con", AppID: stringPointer("firefox")},
				},
			},
		},
	})

	snapshot, err := CaptureLayout(root, registryWithContexts(testContextID))
	if err != nil {
		t.Fatalf("capture grouped floating layout: %v", err)
	}
	if snapshot.Workspaces[0].RestoreMode != WorkspaceRestorePlacementOnly {
		t.Fatalf("mixed floating group was not degraded: %+v", snapshot.Workspaces[0])
	}
}

func TestCaptureLayoutRejectsDuplicateContextWindows(t *testing.T) {
	root := treeWithWorkspaces(&swayipc.TreeNode{
		Name:   "5",
		Type:   "workspace",
		Layout: "splith",
		Nodes: []*swayipc.TreeNode{
			managedTreeLeaf(t, 11, testContextID, nil, false),
			managedTreeLeaf(t, 12, testContextID, nil, false),
			{ID: 13, Name: "browser", Type: "con", AppID: stringPointer("firefox")},
		},
	})

	if _, err := CaptureLayout(root, registryWithContexts(testContextID)); err == nil {
		t.Fatal("expected duplicate context windows to be rejected")
	}
}

func TestScratchpadIsExcludedFromCaptureButIncludedInDuplicateDetection(t *testing.T) {
	regular := &swayipc.TreeNode{
		Name:  "2",
		Type:  "workspace",
		Nodes: []*swayipc.TreeNode{managedTreeLeaf(t, 11, testContextID, nil, false)},
	}
	scratchpad := &swayipc.TreeNode{
		Name:          "__i3_scratch",
		Type:          "workspace",
		FloatingNodes: []*swayipc.TreeNode{managedTreeLeaf(t, 12, testContextID, nil, false)},
	}
	root := treeWithWorkspaces(regular, scratchpad)
	registry := registryWithContexts(testContextID)

	snapshot, err := CaptureLayout(treeWithWorkspaces(scratchpad), registry)
	if err != nil {
		t.Fatalf("capture scratchpad-only tree: %v", err)
	}
	if len(snapshot.Workspaces) != 0 {
		t.Fatalf("scratchpad leaked into persistent layout: %+v", snapshot)
	}
	empty := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{}}
	if _, err := PlanPlacementActions(root, registry, empty); err == nil {
		t.Fatal("duplicate context hidden in scratchpad was not detected")
	}
}

func TestArchivedContextIsNeitherCapturedNorPlaced(t *testing.T) {
	registry := registryWithContexts(testContextID)
	registry.Contexts[0].State = ContextArchived
	marked := managedTreeLeaf(t, 11, testContextID, nil, true)
	root := treeWithWorkspaces(&swayipc.TreeNode{Name: "98: apps", Type: "workspace", Nodes: []*swayipc.TreeNode{marked}})

	snapshot, err := CaptureLayout(root, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Workspaces) != 0 {
		t.Fatalf("archived context was captured as managed: %+v", snapshot)
	}

	unmarked := managedTreeLeaf(t, 12, testContextID, nil, false)
	unmarked.Marks = nil
	root = treeWithWorkspaces(&swayipc.TreeNode{Name: "99: landing", Type: "workspace", Nodes: []*swayipc.TreeNode{unmarked}})
	desired := placementSnapshot("98: apps", testContextID)
	actions, err := PlanPlacementActions(root, registry, desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("archived context received placement actions: %+v", actions)
	}
}

func TestPlanPlacementActionsMovesThenMarksNewStableApplicationID(t *testing.T) {
	appID, err := testContextID.AppID()
	if err != nil {
		t.Fatalf("derive application ID: %v", err)
	}
	root := treeWithWorkspaces(&swayipc.TreeNode{
		Name:  "1",
		Type:  "workspace",
		Nodes: []*swayipc.TreeNode{{ID: 42, Name: "terminal", Type: "con", AppID: &appID}},
	})
	desired := LayoutSnapshot{
		Version: LayoutSchemaVersion,
		Workspaces: []WorkspaceLayout{{
			Name:              "9: saved",
			RestoreMode:       WorkspaceRestorePlacementOnly,
			PlacementContexts: []ContextID{testContextID},
		}},
	}

	actions, err := PlanPlacementActions(root, registryWithContexts(testContextID), desired)
	if err != nil {
		t.Fatalf("plan placement: %v", err)
	}
	want := []PlacementAction{
		{Kind: PlacementMoveWorkspace, ContextID: testContextID, ContainerID: 42, Workspace: "9: saved"},
		{Kind: PlacementAddMark, ContextID: testContextID, ContainerID: 42},
	}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("unexpected actions:\n got: %+v\nwant: %+v", actions, want)
	}
}

func TestPlanPlacementActionsDoesNotMoveAlreadyMarkedWindow(t *testing.T) {
	root := treeWithWorkspaces(&swayipc.TreeNode{
		Name:  "manual destination",
		Type:  "workspace",
		Nodes: []*swayipc.TreeNode{managedTreeLeaf(t, 42, testContextID, nil, false)},
	})
	desired := LayoutSnapshot{
		Version: LayoutSchemaVersion,
		Workspaces: []WorkspaceLayout{{
			Name:              "old saved workspace",
			RestoreMode:       WorkspaceRestorePlacementOnly,
			PlacementContexts: []ContextID{testContextID},
		}},
	}

	actions, err := PlanPlacementActions(root, registryWithContexts(testContextID), desired)
	if err != nil {
		t.Fatalf("plan placement: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("already marked window was moved back: %+v", actions)
	}
}

func TestPlanPlacementActionsRejectsUnboundedCommandBatch(t *testing.T) {
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{}}
	nodes := make([]*swayipc.TreeNode, 0, maxPlacementActions+1)
	for index := range maxPlacementActions + 1 {
		id := ContextID(fmt.Sprintf("123e4567-e89b-12d3-a456-%012x", index+1))
		registry.Contexts = append(registry.Contexts, Context{
			ID:    id,
			State: ContextActive,
			Launcher: Launcher{
				Kind:     LauncherHerdr,
				Session:  fmt.Sprintf("session-%d", index),
				Cwd:      "/work",
				Terminal: &TerminalLauncher{Adapter: TerminalAdapterAlacritty},
			},
		})
		appID, err := id.AppID()
		if err != nil {
			t.Fatalf("derive application ID %d: %v", index, err)
		}
		nodes = append(nodes, &swayipc.TreeNode{ID: int64(index + 1), Type: "con", AppID: &appID})
	}
	root := treeWithWorkspaces(&swayipc.TreeNode{Name: "1", Type: "workspace", Nodes: nodes})
	empty := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{}}
	if _, err := PlanPlacementActions(root, registry, empty); err == nil {
		t.Fatal("expected oversized placement command batch to be rejected")
	}
}

func TestPlanPlacementActionsRejectsConflictingOrDuplicateIdentity(t *testing.T) {
	secondAppID, err := secondContextID.AppID()
	if err != nil {
		t.Fatalf("derive application ID: %v", err)
	}
	mark, err := testContextID.Mark()
	if err != nil {
		t.Fatalf("derive mark: %v", err)
	}
	registry := registryWithContexts(testContextID, secondContextID)
	empty := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{}}

	conflicting := treeWithWorkspaces(&swayipc.TreeNode{
		Name: "1", Type: "workspace",
		Nodes: []*swayipc.TreeNode{{ID: 1, Type: "con", AppID: &secondAppID, Marks: []string{mark}}},
	})
	if _, err := PlanPlacementActions(conflicting, registry, empty); err == nil {
		t.Fatal("expected conflicting app ID and mark to be rejected")
	}

	duplicate := treeWithWorkspaces(&swayipc.TreeNode{
		Name: "1", Type: "workspace",
		Nodes: []*swayipc.TreeNode{
			managedTreeLeaf(t, 1, testContextID, nil, false),
			managedTreeLeaf(t, 2, testContextID, nil, false),
		},
	})
	if _, err := PlanPlacementActions(duplicate, registry, empty); err == nil {
		t.Fatal("expected duplicate managed windows to be rejected")
	}
}

func TestCaptureRejectsReservedIdentityOnLayoutParent(t *testing.T) {
	mark, err := secondContextID.Mark()
	if err != nil {
		t.Fatalf("derive unregistered mark: %v", err)
	}
	root := treeWithWorkspaces(&swayipc.TreeNode{
		Name: "1", Type: "workspace",
		Nodes: []*swayipc.TreeNode{{
			ID:     10,
			Layout: "splith",
			Marks:  []string{mark},
			Nodes:  []*swayipc.TreeNode{managedTreeLeaf(t, 11, testContextID, nil, false)},
		}},
	})
	if _, err := CaptureLayout(root, registryWithContexts(testContextID)); err == nil {
		t.Fatal("expected reserved identity on a layout parent to be rejected")
	}
}

func TestCaptureRejectsNilTreeEntriesInsteadOfSavingPartialState(t *testing.T) {
	root := treeWithWorkspaces(&swayipc.TreeNode{
		Name: "1", Type: "workspace", Nodes: []*swayipc.TreeNode{nil},
	})
	if _, err := CaptureLayout(root, registryWithContexts(testContextID)); err == nil {
		t.Fatal("expected nil tree entry to fail closed")
	}
}

func treeWithWorkspaces(workspaces ...*swayipc.TreeNode) *swayipc.TreeNode {
	return &swayipc.TreeNode{
		ID:   1,
		Type: "root",
		Nodes: []*swayipc.TreeNode{{
			ID:    2,
			Type:  "output",
			Nodes: workspaces,
		}},
	}
}

func managedTreeLeaf(t *testing.T, containerID int64, id ContextID, percent *float64, focused bool) *swayipc.TreeNode {
	t.Helper()
	mark, err := id.Mark()
	if err != nil {
		t.Fatalf("derive mark: %v", err)
	}
	appID, err := id.AppID()
	if err != nil {
		t.Fatalf("derive application ID: %v", err)
	}
	return &swayipc.TreeNode{
		ID:      containerID,
		Name:    "terminal",
		Type:    "con",
		Percent: percent,
		AppID:   &appID,
		Marks:   []string{mark},
		Focused: focused,
	}
}

func registryWithContexts(ids ...ContextID) Registry {
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{}}
	for index, id := range ids {
		registry.Contexts = append(registry.Contexts, Context{
			ID:    id,
			State: ContextActive,
			Launcher: Launcher{
				Kind:     LauncherHerdr,
				Session:  "session-" + string(rune('a'+index)),
				Cwd:      "/work",
				Terminal: &TerminalLauncher{Adapter: TerminalAdapterAlacritty},
			},
		})
	}
	return registry
}

func floatPointer(value float64) *float64 {
	return &value
}

func stringPointer(value string) *string {
	return &value
}
