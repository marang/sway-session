package session

import (
	"strings"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/swayipc"
	"github.com/marang/sway-session/internal/titleindicator"
)

func TestPlanApplicationIndicatorsDerivesFourVisibleLifecycleStates(t *testing.T) {
	registeredID := ContextID("11111111-1111-4111-8111-111111111111")
	pinnedID := ContextID("22222222-2222-4222-8222-222222222222")
	pendingID := ContextID("33333333-3333-4333-8333-333333333333")
	registry := Registry{
		Version:     ContextsSchemaVersion,
		Preferences: RegistryPreferences{DesktopIndicators: true},
		Contexts: []Context{
			applicationIndicatorContext(registeredID, "org.example.Registered", ApplicationRestoreFollow),
			applicationIndicatorContext(pinnedID, "org.example.Pinned", ApplicationRestorePinned),
		},
	}
	registeredMark, _ := registeredID.Mark()
	pinnedMark, _ := pinnedID.Mark()
	root := applicationIndicatorTree(
		applicationIndicatorLeaf(41, "org.example.Unregistered"),
		applicationIndicatorLeaf(42, "org.example.Pending"),
		applicationIndicatorMarkedLeaf(43, "org.example.Registered", registeredMark),
		applicationIndicatorMarkedLeaf(44, "org.example.Pinned", pinnedMark),
		applicationIndicatorLeaf(45, "org.example.NoDesktopEntry"),
	)
	catalog := buildDesktopCatalog(map[string]DesktopEntry{
		"org.example.Unregistered.desktop": {ID: "org.example.Unregistered.desktop"},
		"org.example.Pending.desktop":      {ID: "org.example.Pending.desktop"},
		"org.example.Registered.desktop":   {ID: "org.example.Registered.desktop"},
		"org.example.Pinned.desktop":       {ID: "org.example.Pinned.desktop"},
	}, nil)
	pendingWindow, err := ApplicationWindowByContainer(root, 42)
	if err != nil {
		t.Fatal(err)
	}
	operations := []ApplicationOperation{{
		Version: ApplicationOperationVersion, Kind: OperationRegister, ExpiresAt: time.Now().Add(time.Minute),
		Items: []ApplicationOperationItem{{ContextID: pendingID, Window: &pendingWindow, DesktopID: "org.example.Pending.desktop"}},
	}}

	actions, err := PlanApplicationIndicatorActions(root, registry, catalog, operations)
	if err != nil {
		t.Fatalf("plan indicators: %v", err)
	}
	want := map[int64]titleindicator.State{
		41: titleindicator.Unregistered,
		42: titleindicator.Pending,
		43: titleindicator.Registered,
		44: titleindicator.Pinned,
	}
	got := make(map[int64]titleindicator.State)
	for _, action := range actions {
		if action.Kind == ApplicationIndicatorAdd {
			got[action.ContainerID] = action.State
		}
	}
	if len(got) != len(want) {
		t.Fatalf("added indicator states = %+v, want %+v", got, want)
	}
	for containerID, state := range want {
		if got[containerID] != state {
			t.Fatalf("container %d state = %q, want %q", containerID, got[containerID], state)
		}
	}
}

func TestPlanApplicationIndicatorsInactiveModeCleansOnlyOwnedVersion(t *testing.T) {
	appID := "org.example.App"
	root := applicationIndicatorTree(applicationIndicatorMarkedLeaf(
		41,
		appID,
		"_sway_session_app_indicator_v1_registered_41",
	))
	root.Nodes[0].Nodes[0].Nodes[0].Marks = append(
		root.Nodes[0].Nodes[0].Nodes[0].Marks,
		"_sway_session_app_indicator_v2_registered_41",
		"user-visible",
	)
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{}}

	actions, err := PlanApplicationIndicatorActions(root, registry, DesktopCatalog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Kind != ApplicationIndicatorRemove ||
		actions[0].State != titleindicator.Registered || actions[0].ContainerID != 41 {
		t.Fatalf("inactive cleanup actions = %+v", actions)
	}
}

func TestPlanApplicationIndicatorsSuppressesHerdrAndKeepsArchivedRegistration(t *testing.T) {
	herdrID := ContextID("11111111-1111-4111-8111-111111111111")
	archivedID := ContextID("22222222-2222-4222-8222-222222222222")
	herdrMark, _ := herdrID.Mark()
	archivedMark, _ := archivedID.Mark()
	registry := Registry{
		Version:     ContextsSchemaVersion,
		Preferences: RegistryPreferences{DesktopIndicators: true},
		Contexts: []Context{
			{ID: herdrID, State: ContextActive, Launcher: Launcher{Kind: LauncherHerdr, Session: "work", Cwd: "/work", Terminal: &TerminalLauncher{Adapter: TerminalAdapterAlacritty}}},
			applicationIndicatorContext(archivedID, "org.example.Archived", ApplicationRestoreFollow),
		},
	}
	registry.Contexts[1].State = ContextArchived
	root := applicationIndicatorTree(
		applicationIndicatorMarkedLeaf(41, "persist:herdr", herdrMark),
		applicationIndicatorMarkedLeaf(42, "org.example.Archived", archivedMark),
	)
	catalog := buildDesktopCatalog(map[string]DesktopEntry{
		"org.example.Archived.desktop": {ID: "org.example.Archived.desktop"},
	}, nil)

	actions, err := PlanApplicationIndicatorActions(root, registry, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Kind != ApplicationIndicatorAdd ||
		actions[0].ContainerID != 42 || actions[0].State != titleindicator.Registered {
		t.Fatalf("Herdr/archive indicator actions = %+v", actions)
	}
}

func TestPlanApplicationIndicatorsIgnoreStalePendingRebinds(t *testing.T) {
	contextID := ContextID("11111111-1111-4111-8111-111111111111")
	window := WindowApplication{
		ContainerID:  41,
		Workspace:    "98: apps",
		Identity:     ApplicationIdentity{Protocol: WindowWayland, WaylandAppID: "org.example.App"},
		ContextMarks: []ContextID{},
	}
	root := applicationIndicatorTree(applicationIndicatorLeaf(window.ContainerID, window.Identity.WaylandAppID))
	catalog := buildDesktopCatalog(map[string]DesktopEntry{
		"org.example.App.desktop": {ID: "org.example.App.desktop"},
	}, nil)
	operation := ApplicationOperation{
		Version: ApplicationOperationVersion,
		Kind:    OperationRebind,
		Items: []ApplicationOperationItem{{
			ContextID:       contextID,
			ContextRevision: strings.Repeat("a", 64),
			Window:          &window,
			DesktopID:       "org.example.App.desktop",
		}},
	}

	t.Run("forgotten context", func(t *testing.T) {
		registry := Registry{
			Version:     ContextsSchemaVersion,
			Preferences: RegistryPreferences{DesktopIndicators: true},
			Contexts:    []Context{},
		}
		actions, err := PlanApplicationIndicatorActions(root, registry, catalog, []ApplicationOperation{operation})
		if err != nil {
			t.Fatal(err)
		}
		if len(actions) != 1 || actions[0].Kind != ApplicationIndicatorAdd || actions[0].State != titleindicator.Unregistered {
			t.Fatalf("forgotten-context actions = %+v", actions)
		}
	})

	t.Run("removed desktop candidate", func(t *testing.T) {
		registry := Registry{
			Version:     ContextsSchemaVersion,
			Preferences: RegistryPreferences{DesktopIndicators: true},
			Contexts:    []Context{},
		}
		stale := ApplicationOperation{
			Version: ApplicationOperationVersion,
			Kind:    OperationRegister,
			Items: []ApplicationOperationItem{{
				ContextID: contextID,
				Window:    &window,
				DesktopID: "removed.desktop",
			}},
		}
		actions, err := PlanApplicationIndicatorActions(root, registry, catalog, []ApplicationOperation{stale})
		if err != nil {
			t.Fatal(err)
		}
		if len(actions) != 1 || actions[0].Kind != ApplicationIndicatorAdd || actions[0].State != titleindicator.Unregistered {
			t.Fatalf("removed-candidate actions = %+v", actions)
		}
	})

	t.Run("superseded context revision", func(t *testing.T) {
		context := applicationIndicatorContext(contextID, "org.example.App", ApplicationRestoreFollow)
		currentRevision, err := ApplicationOperationContextRevision(context)
		if err != nil {
			t.Fatal(err)
		}
		if currentRevision == operation.Items[0].ContextRevision {
			t.Fatal("test revisions unexpectedly match")
		}
		registry := Registry{
			Version:     ContextsSchemaVersion,
			Preferences: RegistryPreferences{DesktopIndicators: true},
			Contexts:    []Context{context},
		}
		actions, err := PlanApplicationIndicatorActions(root, registry, catalog, []ApplicationOperation{operation})
		if err != nil {
			t.Fatal(err)
		}
		if len(actions) != 1 || actions[0].Kind != ApplicationIndicatorAdd || actions[0].State != titleindicator.Registered {
			t.Fatalf("superseded-rebind actions = %+v", actions)
		}
	})
}

func TestPlanApplicationIndicatorsConvergesTwoWindowsInTheSameState(t *testing.T) {
	registry := Registry{
		Version:     ContextsSchemaVersion,
		Preferences: RegistryPreferences{DesktopIndicators: true},
		Contexts:    []Context{},
	}
	catalog := buildDesktopCatalog(map[string]DesktopEntry{
		"org.example.First.desktop":  {ID: "org.example.First.desktop"},
		"org.example.Second.desktop": {ID: "org.example.Second.desktop"},
	}, nil)
	root := applicationIndicatorTree(
		applicationIndicatorLeaf(41, "org.example.First"),
		applicationIndicatorLeaf(42, "org.example.Second"),
	)

	actions, err := PlanApplicationIndicatorActions(root, registry, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[0].Mark == actions[1].Mark {
		t.Fatalf("same-state windows did not receive unique marks: %+v", actions)
	}
	for _, action := range actions {
		node := findApplicationIndicatorNode(root, action.ContainerID)
		node.Marks = append(node.Marks, action.Mark)
	}
	actions, err = PlanApplicationIndicatorActions(root, registry, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("two-window indicator state did not converge: %+v", actions)
	}
}

func TestPlanApplicationIndicatorsReturnsResumableBoundedBatch(t *testing.T) {
	leaves := make([]*swayipc.TreeNode, 0, maxApplicationIndicatorActions+1)
	for index := range maxApplicationIndicatorActions + 1 {
		leaves = append(leaves, applicationIndicatorLeaf(int64(index+40), "org.example.App"))
	}
	registry := Registry{
		Version:     ContextsSchemaVersion,
		Preferences: RegistryPreferences{DesktopIndicators: true},
		Contexts:    []Context{},
	}
	catalog := buildDesktopCatalog(map[string]DesktopEntry{
		"org.example.App.desktop": {ID: "org.example.App.desktop"},
	}, nil)

	actions, err := PlanApplicationIndicatorActions(applicationIndicatorTree(leaves...), registry, catalog, nil)
	if err != nil {
		t.Fatalf("plan bounded indicator batch: %v", err)
	}
	if got := len(actions); got != maxApplicationIndicatorActions {
		t.Fatalf("indicator action count = %d, want %d", got, maxApplicationIndicatorActions)
	}
	next, err := PlanApplicationIndicatorActionsAfter(
		applicationIndicatorTree(leaves...), registry, catalog, nil, &actions[len(actions)-1],
	)
	if err != nil {
		t.Fatalf("plan rotated indicator batch: %v", err)
	}
	if len(next) == 0 || next[0].ContainerID != int64(maxApplicationIndicatorActions+40) {
		t.Fatalf("rotated batch did not advance beyond rejected prefix: %+v", next)
	}
}

func applicationIndicatorContext(id ContextID, appID string, policy ApplicationRestorePolicy) Context {
	return Context{
		ID: id, Label: appID, Provider: "desktop", State: ContextActive,
		Launcher: Launcher{Kind: LauncherDesktop, DesktopID: appID + ".desktop", DesktopOrigin: DesktopEntrySystem, DesktopPath: "/usr/share/applications/" + appID + ".desktop"},
		App: &Application{
			Identity:    ApplicationIdentity{Protocol: WindowWayland, WaylandAppID: appID},
			DesiredOpen: true, RestorePolicy: policy,
		},
	}
}

func applicationIndicatorTree(leaves ...*swayipc.TreeNode) *swayipc.TreeNode {
	return &swayipc.TreeNode{ID: 1, Type: "root", Nodes: []*swayipc.TreeNode{{
		ID: 2, Type: "output", Nodes: []*swayipc.TreeNode{{ID: 3, Name: "98: apps", Type: "workspace", Nodes: leaves}},
	}}}
}

func applicationIndicatorLeaf(id int64, appID string) *swayipc.TreeNode {
	return &swayipc.TreeNode{ID: id, Type: "con", Name: appID, AppID: &appID}
}

func applicationIndicatorMarkedLeaf(id int64, appID string, mark string) *swayipc.TreeNode {
	leaf := applicationIndicatorLeaf(id, appID)
	leaf.Marks = []string{mark}
	return leaf
}

func findApplicationIndicatorNode(node *swayipc.TreeNode, id int64) *swayipc.TreeNode {
	if node == nil || node.ID == id {
		return node
	}
	for _, children := range [][]*swayipc.TreeNode{node.Nodes, node.FloatingNodes} {
		for _, child := range children {
			if found := findApplicationIndicatorNode(child, id); found != nil {
				return found
			}
		}
	}
	return nil
}
