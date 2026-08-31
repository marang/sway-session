package session

import (
	"strings"
	"testing"

	"github.com/marang/sway-title-animator/internal/swayipc"
)

func TestResolveFocusedApplicationWaylandXWaylandAndFlatpak(t *testing.T) {
	catalog := buildDesktopCatalog(map[string]DesktopEntry{
		"org.example.Native.desktop": {
			ID: "org.example.Native.desktop", Name: "Native", Path: "/usr/share/applications/org.example.Native.desktop", Origin: DesktopEntrySystem,
		},
		"legacy.desktop": {
			ID: "legacy.desktop", Name: "Legacy", Path: "/usr/share/applications/legacy.desktop", Origin: DesktopEntrySystem, StartupWMClass: "LegacyApp",
		},
		"org.example.Flat.desktop": {
			ID: "org.example.Flat.desktop", Name: "Flat", Path: "/var/lib/flatpak/exports/share/applications/org.example.Flat.desktop", Origin: DesktopEntrySystem,
			FlatpakID: "org.example.Flat", FlatpakInstallation: FlatpakSystem,
		},
	}, nil)
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{}}

	tests := []struct {
		name string
		node *swayipc.TreeNode
		id   string
	}{
		{name: "wayland", node: appWindow(11, true, "org.example.Native", "", "", ""), id: "org.example.Native.desktop"},
		{name: "xwayland", node: appWindow(12, true, "", "LegacyApp", "legacy", ""), id: "legacy.desktop"},
		{name: "flatpak", node: appWindow(13, true, "org.example.Flat", "", "", "org.example.Flat"), id: "org.example.Flat.desktop"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := ResolveFocusedApplication(applicationTree(test.node), catalog, registry)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Candidates) != 1 || result.Candidates[0].ID != test.id || result.Window.Workspace != "2:web" {
				t.Fatalf("unexpected resolution: %+v", result)
			}
		})
	}
}

func TestResolveFocusedApplicationNeverGuessesAmbiguousEntry(t *testing.T) {
	catalog := buildDesktopCatalog(map[string]DesktopEntry{
		"first.desktop":  {ID: "first.desktop", Name: "First", StartupWMClass: "Shared"},
		"second.desktop": {ID: "second.desktop", Name: "Second", StartupWMClass: "Shared"},
	}, nil)
	window := appWindow(11, true, "", "Shared", "shared", "")
	result, err := ResolveFocusedApplication(applicationTree(window), catalog, Registry{Version: ContextsSchemaVersion, Contexts: []Context{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 2 || result.Candidates[0].ID != "first.desktop" || result.Candidates[1].ID != "second.desktop" {
		t.Fatalf("ambiguity was guessed or unstable: %+v", result.Candidates)
	}
}

func TestResolveFocusedApplicationLeavesNoMatchUnregistered(t *testing.T) {
	result, err := ResolveFocusedApplication(
		applicationTree(appWindow(11, true, "org.example.Unknown", "", "", "")),
		buildDesktopCatalog(map[string]DesktopEntry{"other.desktop": {ID: "other.desktop", StartupWMClass: "Other"}}, nil),
		Registry{Version: ContextsSchemaVersion, Contexts: []Context{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Registered != nil || len(result.Candidates) != 0 {
		t.Fatalf("unmatched application gained a registration candidate: %+v", result)
	}
}

func TestResolveFocusedApplicationReportsExistingRegistration(t *testing.T) {
	registered := desktopApplicationContext("org.example.Native.desktop", "org.example.Native")
	registered.ID = testContextID
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{registered}}
	result, err := ResolveFocusedApplication(
		applicationTree(appWindow(11, true, "org.example.Native", "", "", "")),
		buildDesktopCatalog(map[string]DesktopEntry{"org.example.Native.desktop": {ID: "org.example.Native.desktop"}}, nil),
		registry,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Registered == nil || result.Registered.ID != registered.ID {
		t.Fatalf("registration was not recognized: %+v", result)
	}
}

func TestResolveFocusedApplicationRejectsStaleKnownMarkInsteadOfDoubleRegistration(t *testing.T) {
	registered := desktopApplicationContext("org.example.Old.desktop", "org.example.Old")
	registered.ID = testContextID
	mark, _ := registered.ID.Mark()
	window := appWindow(11, true, "org.example.New", "", "", "")
	window.Marks = []string{mark}
	_, err := ResolveFocusedApplication(applicationTree(window), DesktopCatalog{}, Registry{Version: ContextsSchemaVersion, Contexts: []Context{registered}})
	if err == nil || !strings.Contains(err.Error(), "rebind-focused") {
		t.Fatalf("stale known mark did not require explicit rebind: %v", err)
	}
}

func TestResolveFocusedApplicationRejectsUnknownAndMalformedPersistentMarks(t *testing.T) {
	window := appWindow(11, true, "org.example.App", "", "", "")
	window.Marks = []string{"persist:22222222-2222-4222-8222-222222222222"}
	_, err := ResolveFocusedApplication(applicationTree(window), DesktopCatalog{}, Registry{Version: ContextsSchemaVersion, Contexts: []Context{}})
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown persistent mark was accepted: %v", err)
	}
	window.Marks = []string{"persist:not-a-uuid"}
	if _, err := FocusedApplicationWindow(applicationTree(window)); err == nil {
		t.Fatal("malformed persistent mark was accepted")
	}
}

func TestFocusedWorkspaceApplicationsRejectsScratchpadAndListsOnlyCurrentWorkspace(t *testing.T) {
	first := appWindow(11, true, "org.example.First", "", "", "")
	second := appWindow(12, false, "org.example.Second", "", "", "")
	other := appWindow(13, false, "org.example.Other", "", "", "")
	root := &swayipc.TreeNode{ID: 1, Type: "root", Nodes: []*swayipc.TreeNode{
		{ID: 2, Type: "output", Nodes: []*swayipc.TreeNode{
			{ID: 3, Type: "workspace", Name: "2:web", Nodes: []*swayipc.TreeNode{first, second}},
			{ID: 4, Type: "workspace", Name: "3", Nodes: []*swayipc.TreeNode{other}},
		}},
	}}
	windows, err := FocusedWorkspaceApplications(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 2 || windows[0].ContainerID != 11 || windows[1].ContainerID != 12 {
		t.Fatalf("unexpected workspace windows: %+v", windows)
	}

	root.Nodes[0].Nodes[0].Name = "__i3_scratch"
	if _, err := FocusedApplicationWindow(root); err == nil || !strings.Contains(err.Error(), "not an eligible") {
		t.Fatalf("scratchpad focus was accepted: %v", err)
	}
}

func TestFocusedApplicationRejectsTransientAndDialogSurfaces(t *testing.T) {
	parent := int64(7)
	for _, properties := range []swayipc.WindowProperties{
		{Class: "Example", Instance: "example", WindowType: "dialog"},
		{Class: "Example", Instance: "example", TransientFor: &parent},
	} {
		window := appWindow(11, true, "", properties.Class, properties.Instance, "")
		window.WindowProperties = properties
		if _, err := FocusedApplicationWindow(applicationTree(window)); err == nil {
			t.Fatalf("dialog/transient surface was accepted: %+v", properties)
		}
	}
}

func appWindow(id int64, focused bool, appID string, class string, instance string, sandbox string) *swayipc.TreeNode {
	node := &swayipc.TreeNode{ID: id, Type: "con", Focused: focused, WindowProperties: swayipc.WindowProperties{Class: class, Instance: instance}}
	if appID != "" {
		node.AppID = &appID
	}
	if sandbox != "" {
		node.SandboxAppID = &sandbox
	}
	return node
}

func applicationTree(window *swayipc.TreeNode) *swayipc.TreeNode {
	return &swayipc.TreeNode{ID: 1, Type: "root", Nodes: []*swayipc.TreeNode{{
		ID: 2, Type: "output", Nodes: []*swayipc.TreeNode{{ID: 3, Type: "workspace", Name: "2:web", Nodes: []*swayipc.TreeNode{window}}},
	}}}
}
