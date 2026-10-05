package session

import (
	"reflect"
	"testing"

	"github.com/marang/sway-session/internal/swayipc"
)

func TestCaptureLayoutStartupPromptKeepsNestedTabsAndLimitsMixedBrowserDegradation(t *testing.T) {
	fourthID := ContextID("6ba7b812-9dad-41d1-80b4-00c04fd430c8")
	registry := registryWithContexts(testContextID, secondContextID, thirdContextID, fourthID)
	for _, test := range []struct {
		name             string
		unmanagedBrowser bool
	}{
		{name: "independent floating prompt"},
		{name: "additional unmanaged browser", unmanagedBrowser: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Sway's workspace wrapper may remain splith while its only
			// tiling child owns the tabbed layout. The prompt is independent
			// of that managed subtree.
			workspace := &swayipc.TreeNode{
				ID: 10, Name: "98", Type: "workspace", Layout: "splith",
				Nodes: []*swayipc.TreeNode{{
					ID: 20, Type: "con", Layout: "tabbed",
					Nodes: []*swayipc.TreeNode{
						managedTreeLeaf(t, 21, testContextID, nil, false),
						managedTreeLeaf(t, 22, secondContextID, nil, false),
					},
				}},
				FloatingNodes: []*swayipc.TreeNode{{
					ID: 30, Type: "con", Name: "Unlock keyring",
					WindowProperties: swayipc.WindowProperties{Class: "Gcr-prompter", Instance: "gcr-prompter"},
					Rect:             swayipc.Rect{Width: 400, Height: 200},
				}},
			}
			if test.unmanagedBrowser {
				workspace.Nodes = append(workspace.Nodes, &swayipc.TreeNode{
					ID: 31, Type: "con", Name: "Unregistered Chrome",
					WindowProperties: swayipc.WindowProperties{Class: "Google-chrome", Instance: "google-chrome"},
				})
			}
			root := treeWithWorkspaces(workspace,
				&swayipc.TreeNode{ID: 40, Name: "99", Type: "workspace", Layout: "splith", Nodes: []*swayipc.TreeNode{
					managedTreeLeaf(t, 41, thirdContextID, nil, false),
				}},
				&swayipc.TreeNode{ID: 50, Name: "100", Type: "workspace", Layout: "splith", Nodes: []*swayipc.TreeNode{
					managedTreeLeaf(t, 51, fourthID, nil, false),
				}})

			captured, err := CaptureLayout(root, registry)
			if err != nil {
				t.Fatalf("capture startup tree: %v", err)
			}
			want := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{
				exactWorkspace("98", LayoutTabbed, testContextID, secondContextID),
				{Name: "99", RestoreMode: WorkspaceRestoreLayout, Tiling: &LayoutNode{ContextID: contextIDPointer(thirdContextID)}},
				{Name: "100", RestoreMode: WorkspaceRestoreLayout, Tiling: &LayoutNode{ContextID: contextIDPointer(fourthID)}},
			}}
			if test.unmanagedBrowser {
				want.Workspaces[0] = WorkspaceLayout{
					Name: "98", RestoreMode: WorkspaceRestorePlacementOnly,
					PlacementContexts: []ContextID{testContextID, secondContextID},
				}
			}
			captured, err = canonicalSnapshot(captured)
			if err != nil {
				t.Fatalf("canonicalize captured startup tree: %v", err)
			}
			want, err = canonicalSnapshot(want)
			if err != nil {
				t.Fatalf("validate expected startup layout: %v", err)
			}
			if !reflect.DeepEqual(captured, want) {
				t.Fatalf("startup prompt or browser changed unrelated managed layouts:\n got: %+v\nwant: %+v", captured, want)
			}
		})
	}
}
