package session

import (
	"strings"
	"testing"

	"github.com/marang/sway-session/internal/swayipc"
)

func TestObserveRegisteredWindowContextUsesEventIdentityWithoutLiveTree(t *testing.T) {
	terminalRegistry := registryWithContexts(testContextID)
	terminal := managedTreeLeaf(t, 41, testContextID, nil, false)
	application := desktopApplicationContext("org.example.App.desktop", "org.example.App")
	application.ID, application.App.DesiredOpen = testContextID, true
	appRegistry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{application}}
	mark, _ := testContextID.Mark()
	unknownID := ContextID("22222222-2222-4222-8222-222222222222")
	unknownMark, _ := unknownID.Mark()
	transientFor := int64(42)
	tests := []struct {
		name     string
		registry Registry
		node     *swayipc.TreeNode
		want     bool
	}{
		{name: "terminal close payload", registry: terminalRegistry, node: terminal, want: true},
		{name: "terminal before mark adoption", registry: terminalRegistry, node: &swayipc.TreeNode{ID: 41, AppID: terminal.AppID}, want: true},
		{name: "unmarked desktop before adoption", registry: appRegistry, node: appWindow(41, false, "org.example.App", "", "", ""), want: true},
		{name: "marked desktop close payload", registry: appRegistry, node: &swayipc.TreeNode{ID: 41, AppID: stringPointer("org.example.App"), Marks: []string{mark}}, want: true},
		{name: "unregistered desktop", registry: appRegistry, node: appWindow(41, false, "org.example.Other", "", "", "")},
		{name: "unregistered terminal", registry: terminalRegistry, node: managedTreeLeaf(t, 41, unknownID, nil, false)},
		{name: "unknown mark keeps separate ownership", registry: appRegistry, node: &swayipc.TreeNode{ID: 41, AppID: stringPointer("org.example.App"), Marks: []string{unknownMark}}},
		{name: "transient dialog", registry: appRegistry, node: &swayipc.TreeNode{ID: 41, AppID: stringPointer("org.example.App"), WindowProperties: swayipc.WindowProperties{TransientFor: &transientFor}}},
		{name: "non-normal window", registry: appRegistry, node: &swayipc.TreeNode{ID: 41, AppID: stringPointer("org.example.App"), WindowProperties: swayipc.WindowProperties{WindowType: "dialog"}}},
		{name: "unidentified leaf", registry: appRegistry, node: &swayipc.TreeNode{ID: 41}},
		{name: "structural group", registry: terminalRegistry, node: &swayipc.TreeNode{ID: 40, Nodes: []*swayipc.TreeNode{terminal}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			id, relevant, err := ObserveRegisteredWindowContext(test.node, test.registry)
			if err != nil || relevant != test.want || test.want && id != testContextID || !test.want && id != "" {
				t.Fatalf("event identity = (%q, %t, %v), want relevant=%t", id, relevant, err, test.want)
			}
		})
	}
}

func TestObserveRegisteredWindowContextPreservesIdentityUncertainty(t *testing.T) {
	application := applicationContextWithID(testContextID, "org.example.App")
	second := applicationContextWithID("22222222-2222-4222-8222-222222222222", "org.example.Other")
	second.App.Identity.WaylandAppID = application.App.Identity.WaylandAppID
	appRegistry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{application, second}}
	x11 := captureTestXWaylandContext(testContextID, "org.example.Legacy")
	x11Registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{x11}}
	terminalRegistry := registryWithContexts(testContextID)
	mark, _ := testContextID.Mark()
	otherMark, _ := second.ID.Mark()
	terminalAppID, _ := testContextID.AppID()
	otherAppID, _ := second.ID.AppID()
	tests := []struct {
		name     string
		registry Registry
		node     *swayipc.TreeNode
		error    string
		want     bool
	}{
		{name: "unrelated partial X11", registry: x11Registry, node: appWindow(41, false, "", "Other", "", "")},
		{name: "registered partial X11 class", registry: x11Registry, node: appWindow(41, false, "", "Registered", "", ""), error: "requires both class and instance"},
		{name: "registered partial X11 instance", registry: x11Registry, node: appWindow(41, false, "", "", "registered", ""), error: "requires both class and instance"},
		{name: "complete registered X11", registry: x11Registry, node: appWindow(41, false, "", "Registered", "registered", "org.example.Legacy"), want: true},
		{name: "missing sandbox is ambiguous", registry: appRegistry, node: appWindow(41, false, "org.example.App", "", "", ""), error: "overlaps 2 registered contexts"},
		{name: "observed sandbox selects context", registry: appRegistry, node: appWindow(41, false, "org.example.App", "", "", "org.example.App"), want: true},
		{name: "marked sandbox still uncertain", registry: appRegistry, node: &swayipc.TreeNode{ID: 41, AppID: stringPointer("org.example.App"), Marks: []string{mark}}, error: "overlaps 2 registered contexts"},
		{name: "marked app native drift", registry: appRegistry, node: &swayipc.TreeNode{ID: 41, AppID: stringPointer("org.example.Drift"), Marks: []string{mark}}, error: "identity changed"},
		{name: "marked app loses native identity", registry: appRegistry, node: &swayipc.TreeNode{ID: 41, Marks: []string{mark}}, error: "identity changed"},
		{name: "malformed reserved mark", registry: terminalRegistry, node: &swayipc.TreeNode{ID: 41, Marks: []string{MarkPrefix + "invalid"}}, error: "invalid managed mark"},
		{name: "malformed reserved app ID", registry: terminalRegistry, node: &swayipc.TreeNode{ID: 41, AppID: stringPointer(AppIDPrefix + "invalid")}, error: "invalid managed application ID"},
		{name: "conflicting marks", registry: terminalRegistry, node: &swayipc.TreeNode{ID: 41, Marks: []string{mark, otherMark}}, error: "conflicting managed identities"},
		{name: "mark conflicts with stable app ID", registry: terminalRegistry, node: &swayipc.TreeNode{ID: 41, Marks: []string{mark}, AppID: &otherAppID}, error: "conflicting managed identities"},
		{name: "reserved identity on parent", registry: terminalRegistry, node: &swayipc.TreeNode{ID: 40, AppID: &terminalAppID, Nodes: []*swayipc.TreeNode{{ID: 41}}}, error: "layout parent"},
		{name: "native window on parent", registry: appRegistry, node: &swayipc.TreeNode{ID: 40, AppID: stringPointer("org.example.App"), Nodes: []*swayipc.TreeNode{{ID: 41}}}, error: "layout parent"},
		{name: "invalid registry checked before unrelated leaf", registry: Registry{}, node: &swayipc.TreeNode{ID: 41}, error: "validate context registry"},
		{name: "nil node", registry: terminalRegistry, error: "invalid compositor window node"},
		{name: "invalid container ID", registry: terminalRegistry, node: &swayipc.TreeNode{}, error: "invalid compositor window node"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			id, relevant, err := ObserveRegisteredWindowContext(test.node, test.registry)
			if test.error != "" {
				if err == nil || !strings.Contains(err.Error(), test.error) || relevant || id != "" {
					t.Fatalf("event identity = (%q, %t, %v), want error containing %q", id, relevant, err, test.error)
				}
				return
			}
			if err != nil || relevant != test.want || test.want && id != testContextID || !test.want && id != "" {
				t.Fatalf("event identity = (%q, %t, %v), want relevant=%t", id, relevant, err, test.want)
			}
		})
	}
}

func TestObserveRegisteredWindowContextRequiresRestoreEligibility(t *testing.T) {
	for _, kind := range []string{"terminal", "unmarked app", "marked app"} {
		for _, state := range []string{"archived", "desired closed"} {
			if kind == "terminal" && state == "desired closed" {
				continue
			}
			t.Run(kind+"/"+state, func(t *testing.T) {
				registry := registryWithContexts(testContextID)
				node := managedTreeLeaf(t, 41, testContextID, nil, false)
				if kind != "terminal" {
					context := applicationContextWithID(testContextID, "org.example.App")
					registry.Contexts = []Context{context}
					node = appWindow(41, false, "org.example.App", "", "", "org.example.App")
					if kind == "marked app" {
						mark, _ := testContextID.Mark()
						node.Marks = []string{mark}
					}
				}
				if state == "archived" {
					registry.Contexts[0].State = ContextArchived
				} else {
					registry.Contexts[0].App.DesiredOpen = false
				}
				id, relevant, err := ObserveRegisteredWindowContext(node, registry)
				if err != nil || relevant || id != "" {
					t.Fatalf("inactive event identity = (%q, %t, %v)", id, relevant, err)
				}
			})
		}
	}
}
