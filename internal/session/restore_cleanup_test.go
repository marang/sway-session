package session

import (
	"testing"

	"github.com/marang/sway-session/internal/swayipc"
)

func TestRestoreCleanupPinsWindowIdentityAndIgnoresUnownedStaging(t *testing.T) {
	for _, scenario := range []string{"closed", "replaced", "duplicate", "user moved"} {
		t.Run(scenario, func(t *testing.T) {
			cleanup := RestoreCleanup{}
			cleanup.Remember(RestoreAction{Kind: RestoreMoveWorkspace, Workspace: "98", ContextID: testContextID, ContainerID: 11, Target: RestoreStagingWorkspace})
			owned := managedTreeLeaf(t, 11, testContextID, nil, false)
			unowned := managedTreeLeaf(t, 12, secondContextID, nil, false)
			stage := restoreWorkspace(RestoreStagingWorkspace, "splith", owned, unowned)
			root := restoreTree(stage)
			switch scenario {
			case "closed":
				stage.Nodes = stage.Nodes[1:]
			case "replaced":
				owned.ID = 13
			case "duplicate":
				stage.Nodes = append(stage.Nodes, managedTreeLeaf(t, 13, testContextID, nil, false))
			case "user moved":
				stage.Nodes = stage.Nodes[1:]
				root = restoreTree(stage, restoreWorkspace("99", "splith", owned))
			}
			action, err := cleanup.Plan(root, registryWithContexts(testContextID, secondContextID))
			if scenario == "duplicate" {
				if err == nil || !cleanup.Pending() {
					t.Fatal("ambiguous identity did not retain cleanup for reobservation")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if action != nil {
				t.Fatalf("cleanup moved an unowned or user-controlled window: %+v", action)
			}
		})
	}
}

func TestRestoreCleanupRemovesOnlyOwnedTemporaryMarks(t *testing.T) {
	mark := temporaryMark("98", "root")
	owned := &swayipc.TreeNode{ID: 20, Type: "con", Layout: "tabbed", Marks: []string{mark, "user-mark"}, Nodes: []*swayipc.TreeNode{managedTreeLeaf(t, 11, testContextID, nil, false)}}
	root := restoreTree(restoreWorkspace("98", "splith", owned))
	registry := registryWithContexts(testContextID)
	cleanup := RestoreCleanup{}
	cleanup.Remember(RestoreAction{Kind: RestoreAddTemporaryMark, Workspace: "98", ContainerID: 20, Target: mark})
	action, err := cleanup.Plan(root, registry)
	if err != nil {
		t.Fatal(err)
	}
	if action == nil || action.Kind != RestoreRemoveMark || action.Target != mark || action.ContainerID != 20 {
		t.Fatalf("unexpected cleanup action: %+v", action)
	}
	owned.Marks = []string{"user-mark"}
	if action, err = cleanup.Plan(root, registry); err != nil || action != nil || cleanup.Pending() {
		t.Fatalf("cleanup touched an unrelated mark: action=%+v err=%v", action, err)
	}
}

func TestRestoreCleanupRecoversMarksAfterRestart(t *testing.T) {
	desired := exactWorkspace("98", LayoutTabbed, testContextID, secondContextID)
	mark := temporaryMark("98", "r")
	group := &swayipc.TreeNode{ID: 20, Type: "con", Layout: "tabbed", Marks: []string{mark}, Nodes: []*swayipc.TreeNode{managedTreeLeaf(t, 11, testContextID, nil, false), managedTreeLeaf(t, 12, secondContextID, nil, false)}}
	root := restoreTree(restoreWorkspace("98", "splith", group))
	registry := registryWithContexts(testContextID, secondContextID)
	cleanup := RestoreCleanup{}
	if err := cleanup.Recover(root, registry, layoutSnapshot(desired)); err != nil {
		t.Fatal(err)
	}
	action, err := cleanup.Plan(root, registry)
	if err != nil {
		t.Fatal(err)
	}
	if action == nil || action.Kind != RestoreRemoveMark || action.Target != mark {
		t.Fatalf("restart forgot owned restore mark: %+v", action)
	}
}
