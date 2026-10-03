package session

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/marang/sway-session/internal/swayipc"
)

func scratchpadApplicationTree(t *testing.T, visible, marked bool) (*swayipc.TreeNode, Registry) {
	t.Helper()
	app := applicationContextWithID(testContextID, "org.example.Scratchpad")
	window := appWindow(41, false, app.App.Identity.WaylandAppID, "", "", app.App.Identity.SandboxAppID)
	if marked {
		mark, _ := app.ID.Mark()
		window.Marks = []string{mark}
	}
	root := applicationTree(window)
	workspace := root.Nodes[0].Nodes[0]
	workspace.Name, workspace.Layout = "98", "splith"
	workspace.Nodes = nil
	workspace.FloatingNodes = []*swayipc.TreeNode{{ID: 42, Type: "con", ScratchpadState: "fresh", Nodes: []*swayipc.TreeNode{window}}}
	if !visible {
		workspace.Name = "__i3_scratch"
	}
	return root, Registry{Version: ContextsSchemaVersion, Contexts: []Context{app}}
}

func TestScratchpadCaptureSeparatesHiddenAndShownMembership(t *testing.T) {
	for _, visible := range []bool{false, true} {
		root, registry := scratchpadApplicationTree(t, visible, true)
		snapshot, err := CaptureLayout(root, registry)
		if err != nil {
			t.Fatal(err)
		}
		want := ScratchpadPlacement{ContextID: testContextID, Visible: visible}
		if visible {
			want.Workspace = "98"
		}
		if len(snapshot.Workspaces) != 0 || !reflect.DeepEqual(snapshot.Scratchpad, []ScratchpadPlacement{want}) {
			t.Fatalf("visible=%v: normal layout or membership lost: %+v", visible, snapshot)
		}
		ready, err := StartupCaptureReady(snapshot, snapshot, registry)
		if err != nil || !ready {
			t.Fatalf("scratchpad incorrectly missing from startup: ready=%v error=%v", ready, err)
		}
	}
}

func TestScratchpadPlacementUsesFreshObservationsBeforeMark(t *testing.T) {
	for _, visible := range []bool{false, true} {
		desired := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{}, Scratchpad: []ScratchpadPlacement{{ContextID: testContextID, Visible: visible}}}
		if visible {
			desired.Scratchpad[0].Workspace = "99"
		}
		root, registry := scratchpadApplicationTree(t, true, false)
		wrapper := root.Nodes[0].Nodes[0].FloatingNodes[0]
		wrapper.ScratchpadState = "none"
		groups, err := ObserveApplicationGroups(root, registry)
		if err != nil {
			t.Fatal(err)
		}
		actions, err := PlanApplicationPlacementActions(groups, desired)
		if err != nil || len(actions) != 1 || actions[0].Kind != PlacementMoveScratchpad {
			t.Fatalf("move must precede mark: %+v, %v", actions, err)
		}
		root, registry = scratchpadApplicationTree(t, false, false)
		groups, _ = ObserveApplicationGroups(root, registry)
		actions, err = PlanApplicationPlacementActions(groups, desired)
		want := PlacementAddMark
		if visible {
			want = PlacementShowScratchpad
		}
		if err != nil || len(actions) != 1 || actions[0].Kind != want {
			t.Fatalf("hidden observation: %+v, %v", actions, err)
		}
		if visible {
			if actions[0].Workspace != "99" {
				t.Fatal("saved visible workspace lost")
			}
			root.Nodes[0].Nodes[0].Name = "99"
			groups, _ = ObserveApplicationGroups(root, registry)
			actions, err = PlanApplicationPlacementActions(groups, desired)
			if err != nil || len(actions) != 1 || actions[0].Kind != PlacementAddMark {
				t.Fatalf("confirmed show must mark without toggling: %+v, %v", actions, err)
			}
		}
	}
}

func TestScratchpadMarkedAndAmbiguousApplicationsStayUserControlled(t *testing.T) {
	root, registry := scratchpadApplicationTree(t, true, true)
	desired := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{}, Scratchpad: []ScratchpadPlacement{{ContextID: testContextID}}}
	groups, _ := ObserveApplicationGroups(root, registry)
	actions, err := PlanApplicationPlacementActions(groups, desired)
	if err != nil || len(actions) != 0 {
		t.Fatalf("already marked window changed: %+v, %v", actions, err)
	}
	root, registry = scratchpadApplicationTree(t, false, false)
	workspace := root.Nodes[0].Nodes[0]
	second := *workspace.FloatingNodes[0].Nodes[0]
	second.ID = 43
	workspace.FloatingNodes[0].Nodes = append(workspace.FloatingNodes[0].Nodes, &second)
	groups, _ = ObserveApplicationGroups(root, registry)
	actions, err = PlanApplicationPlacementActions(groups, desired)
	if err != nil || len(actions) != 0 || !groups[testContextID].Ambiguous {
		t.Fatalf("ambiguous scratchpad anchor was guessed: %+v, %+v, %v", groups, actions, err)
	}
}

func TestScratchpadMissingAndNormalWorkspaceTransitions(t *testing.T) {
	root, registry := scratchpadApplicationTree(t, false, true)
	previous, err := CaptureLayout(root, registry)
	if err != nil {
		t.Fatal(err)
	}
	empty := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{}}
	merged, err := PreserveMissingPlacements(previous, empty, registry)
	if err != nil || !reflect.DeepEqual(merged.Scratchpad, previous.Scratchpad) {
		t.Fatalf("late scratchpad placement lost: %+v, %v", merged, err)
	}
	registry.Contexts[0].App.DesiredOpen = false
	merged, err = PreserveMissingPlacements(previous, empty, registry)
	if err != nil || len(merged.Scratchpad) != 0 {
		t.Fatalf("closed application regained placement: %+v, %v", merged, err)
	}
	registry.Contexts[0].App.DesiredOpen = true
	workspace := root.Nodes[0].Nodes[0]
	workspace.Name, workspace.Layout = "99", "splith"
	workspace.Nodes = workspace.FloatingNodes[0].Nodes
	workspace.FloatingNodes = nil
	workspace.Nodes[0].Percent = floatPointer(1)
	current, err := CaptureLayout(root, registry)
	if err != nil {
		t.Fatal(err)
	}
	merged, err = PreserveMissingPlacements(previous, current, registry)
	if err != nil || len(merged.Scratchpad) != 0 || len(merged.Workspaces) != 1 || merged.Workspaces[0].Name != "99" {
		t.Fatalf("manual normal-workspace transition reverted: %+v, %v", merged, err)
	}
}

func TestScratchpadValidationAndVisibilityHash(t *testing.T) {
	hidden := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{}, Scratchpad: []ScratchpadPlacement{{ContextID: testContextID}}}
	shown := hidden
	shown.Scratchpad = []ScratchpadPlacement{{ContextID: testContextID, Visible: true, Workspace: "98"}}
	a, err := SemanticSnapshotHash(hidden)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SemanticSnapshotHash(shown)
	if err != nil || a == b {
		t.Fatal("visibility was not captured semantically")
	}
	for _, invalid := range []ScratchpadPlacement{
		{ContextID: testContextID, Visible: true},
		{ContextID: testContextID, Visible: true, Workspace: "__i3_scratch"},
		{ContextID: testContextID, Workspace: "98"},
	} {
		candidate := hidden
		candidate.Scratchpad = []ScratchpadPlacement{invalid}
		if candidate.Validate() == nil {
			t.Fatalf("invalid visibility accepted: %+v", invalid)
		}
	}
	duplicate := hidden
	duplicate.Scratchpad = append(append([]ScratchpadPlacement{}, hidden.Scratchpad...), hidden.Scratchpad[0])
	if duplicate.Validate() == nil {
		t.Fatal("duplicate scratchpad identity accepted")
	}
}

func TestScratchpadLiveMoveRetiresOldExactLayoutWhileSiblingIsMissing(t *testing.T) {
	root, registry := scratchpadApplicationTree(t, false, true)
	registry.Contexts = append(registry.Contexts, registryWithContexts(secondContextID).Contexts[0])
	appID, terminalID := testContextID, secondContextID
	previous := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{{
		Name: "98", RestoreMode: WorkspaceRestoreLayout,
		Tiling: &LayoutNode{Layout: LayoutTabbed, Children: []LayoutNode{{ContextID: &appID}, {ContextID: &terminalID}}},
	}}}
	current, err := CaptureLayout(root, registry)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := PreserveMissingPlacements(previous, current, registry)
	if err != nil || len(merged.Scratchpad) != 1 || len(merged.Workspaces) != 1 || merged.Workspaces[0].RestoreMode != WorkspaceRestorePlacementOnly || !reflect.DeepEqual(merged.Workspaces[0].PlacementContexts, []ContextID{terminalID}) {
		t.Fatalf("old exact snapshot overrode a live scratchpad transition: %+v, %v", merged, err)
	}
	again, err := PreserveMissingPlacements(merged, current, registry)
	if err != nil || !reflect.DeepEqual(again, merged) {
		t.Fatalf("scratchpad merge is not idempotent: %+v, %v", again, err)
	}
}

func TestScratchpadCaptureDoesNotPersistWindowContents(t *testing.T) {
	root, registry := scratchpadApplicationTree(t, false, true)
	root.Nodes[0].Nodes[0].FloatingNodes[0].Nodes[0].Name = "private-document-and-url-sentinel"
	snapshot, err := CaptureLayout(root, registry)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil || strings.Contains(string(encoded), "private-document") || strings.Contains(string(encoded), "__i3_scratch") {
		t.Fatalf("private title or synthetic workspace leaked into saved state: %s, %v", encoded, err)
	}
}
