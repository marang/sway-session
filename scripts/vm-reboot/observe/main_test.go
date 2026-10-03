package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

var fixtureIDs = []session.ContextID{
	"6ba7b810-9dad-11d1-80b4-00c04fd430c8",
	"6ba7b811-9dad-11d1-80b4-00c04fd430c8",
}

func TestObserveRejectsFixtureRegressions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*session.Registry, *session.LayoutSnapshot, *swayipc.TreeNode)
	}{
		{"live order", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			tree.Nodes[0].Nodes[0], tree.Nodes[0].Nodes[1] = tree.Nodes[0].Nodes[1], tree.Nodes[0].Nodes[0]
		}},
		{"stored order", func(_ *session.Registry, saved *session.LayoutSnapshot, _ *swayipc.TreeNode) {
			c := saved.Workspaces[0].Tiling.Children
			c[0], c[1] = c[1], c[0]
		}},
		{"matching changed order", func(_ *session.Registry, saved *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			c := saved.Workspaces[0].Tiling.Children
			c[0], c[1] = c[1], c[0]
			tree.Nodes[0].Nodes[0], tree.Nodes[0].Nodes[1] = tree.Nodes[0].Nodes[1], tree.Nodes[0].Nodes[0]
		}},
		{"live horizontal split", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			tree.Nodes[0].Layout = "splith"
		}},
		{"matching changed vertical split", func(_ *session.Registry, saved *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			saved.Workspaces[0].Tiling.Layout = session.LayoutSplitVertical
			tree.Nodes[0].Layout = "splitv"
		}},
		{"archived", func(reg *session.Registry, _ *session.LayoutSnapshot, _ *swayipc.TreeNode) {
			reg.Contexts[0].State = session.ContextArchived
		}},
		{"unknown window", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			app := "unknown-terminal"
			tree.Nodes[0].FloatingNodes = []*swayipc.TreeNode{{ID: 22, AppID: &app}}
		}},
		{"missing UUID", func(reg *session.Registry, _ *session.LayoutSnapshot, _ *swayipc.TreeNode) {
			reg.Contexts = reg.Contexts[:1]
		}},
		{"unknown managed UUID", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			app := "sway-session.6ba7b812-9dad-11d1-80b4-00c04fd430c8"
			tree.Nodes[0].Nodes[0].AppID = &app
			tree.Nodes[0].Nodes[0].Marks = nil
		}},
		{"duplicate live UUID", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			tree.Nodes[0].Nodes = append(tree.Nodes[0].Nodes, tree.Nodes[0].Nodes[0])
		}},
		{"floating membership", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			w := tree.Nodes[0]
			w.FloatingNodes = w.Nodes[:1]
			w.FloatingNodes[0].Rect = swayipc.Rect{Width: 400, Height: 300}
			w.Nodes = w.Nodes[1:]
		}},
		{"live fullscreen", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			tree.Nodes[0].Nodes[0].FullscreenMode = 1
		}},
		{"stored fullscreen", func(_ *session.Registry, saved *session.LayoutSnapshot, _ *swayipc.TreeNode) {
			saved.Workspaces[0].Tiling.Fullscreen = session.FullscreenWorkspace
		}},
		{"singleton tabbed hierarchy", func(_ *session.Registry, saved *session.LayoutSnapshot, _ *swayipc.TreeNode) {
			c := saved.Workspaces[0].Tiling.Children
			c[0] = session.LayoutNode{Layout: session.LayoutTabbed, Children: []session.LayoutNode{c[0]}}
		}},
		{"singleton stacked hierarchy", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			c := tree.Nodes[0].Nodes
			c[0] = &swayipc.TreeNode{ID: 30, Layout: "stacked", Nodes: []*swayipc.TreeNode{c[0]}}
		}},
		{"meaningful grouped hierarchy", func(_ *session.Registry, saved *session.LayoutSnapshot, _ *swayipc.TreeNode) {
			saved.Workspaces[0].Tiling = &session.LayoutNode{Layout: session.LayoutTabbed, Children: []session.LayoutNode{{Layout: session.LayoutStacked, Children: saved.Workspaces[0].Tiling.Children}}}
		}},
		{"other workspace", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			tree.Nodes[0].Name = "99"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry, saved, tree := fixture(t)
			tc.change(&registry, &saved, tree)
			if got, err := Observe(registry, saved, tree, fixtureIDs); err == nil {
				t.Fatalf("regression accepted: %+v", got)
			}
		})
	}
}

func TestObserverCLIDoesNotInitializeAnAbsentStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"--state-root", root, "--socket", "/not/a/live/socket", "--expected", strings.Join([]string{string(fixtureIDs[0]), string(fixtureIDs[1])}, ",")}, &stdout, &stderr)
	if code == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "registry") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("read-only CLI created state: %v", err)
	}
}

func TestObserverCLIReadsCanonicalStoresAndOnlyRequestsGetTree(t *testing.T) {
	registry, saved, tree := fixture(t)
	directory, err := os.MkdirTemp("", "ss-observer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	stateRoot := filepath.Join(directory, "state")
	if err := session.RegistryStoreFor(stateRoot).Save(registry); err != nil {
		t.Fatal(err)
	}
	if err := session.LayoutStoreFor(stateRoot).Save(saved); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(directory, "sway.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	served := make(chan error, 1)
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			served <- err
			return
		}
		defer peer.Close()
		_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
		header := make([]byte, 14)
		if _, err := io.ReadFull(peer, header); err != nil {
			served <- err
			return
		}
		if string(header[:6]) != "i3-ipc" || binary.LittleEndian.Uint32(header[6:10]) != 0 || binary.LittleEndian.Uint32(header[10:]) != 4 {
			served <- fmt.Errorf("observer sent a non-GET_TREE request: %v", header)
			return
		}
		payload, err := json.Marshal(tree)
		if err != nil {
			served <- err
			return
		}
		binary.LittleEndian.PutUint32(header[6:10], uint32(len(payload)))
		_, err = peer.Write(append(header, payload...))
		served <- err
	}()
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"--state-root", stateRoot, "--socket", socket,
		"--expected", string(fixtureIDs[0]) + "," + string(fixtureIDs[1])}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	var observed Observation
	if err := json.Unmarshal(stdout.Bytes(), &observed); err != nil {
		t.Fatal(err)
	}
	if !observed.MatchesFixture || observed.TerminalCount != 2 || strings.Contains(stdout.String(), "PRIVATE WINDOW TITLE") {
		t.Fatalf("unexpected CLI observation: %s", stdout.String())
	}
	var after session.Registry
	if err := session.RegistryStoreFor(stateRoot).LoadInto(&after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, registry) {
		t.Fatalf("read-only CLI changed registry: %+v", after)
	}
}

func TestObserveRejectsManagedWindowsHiddenInStaging(t *testing.T) {
	registry, stored, tree := fixture(t)
	tree.Nodes = append(tree.Nodes, &swayipc.TreeNode{
		Type: "workspace", Name: session.RestoreStagingWorkspace, Layout: "splith",
		Nodes: []*swayipc.TreeNode{tree.Nodes[0].Nodes[0]},
	})
	if _, err := Observe(registry, stored, tree, fixtureIDs); err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatalf("managed staging window accepted: %v", err)
	}
}

func fixture(t *testing.T) (session.Registry, session.LayoutSnapshot, *swayipc.TreeNode) {
	t.Helper()
	registry := session.Registry{Version: session.ContextsSchemaVersion, Contexts: []session.Context{}}
	children := []session.LayoutNode{}
	windows := []*swayipc.TreeNode{}
	for i, id := range fixtureIDs {
		name, err := session.DeriveTerminalInstanceSessionName(id)
		if err != nil {
			t.Fatal(err)
		}
		registry.Contexts = append(registry.Contexts, session.Context{
			ID: id, State: session.ContextActive, Provider: session.TerminalContextProvider,
			Launcher: session.Launcher{Kind: session.LauncherHerdr, Session: name,
				Cwd: "/home/ss-test/project", Terminal: &session.TerminalLauncher{
					Adapter: session.TerminalAdapterAlacritty, Instance: true,
				}},
		})
		children = append(children, session.LayoutNode{ContextID: &id})
		appID, _ := id.AppID()
		mark, _ := id.Mark()
		windows = append(windows, &swayipc.TreeNode{
			ID: int64(10 + i), Type: "con", Name: "PRIVATE WINDOW TITLE",
			AppID: &appID, Marks: []string{mark},
		})
	}
	stored := session.LayoutSnapshot{Version: session.LayoutSchemaVersion, Workspaces: []session.WorkspaceLayout{{
		Name: "98", RestoreMode: session.WorkspaceRestoreLayout,
		Tiling: &session.LayoutNode{Layout: session.LayoutTabbed, Children: children},
	}}}
	tree := &swayipc.TreeNode{Type: "root", Nodes: []*swayipc.TreeNode{{
		Type: "workspace", Name: "98", Layout: "tabbed", Nodes: windows,
	}}}
	return registry, stored, tree
}

func TestObserveRequiresTheTwoOrderedTabbedTerminals(t *testing.T) {
	registry, stored, tree := fixture(t)
	got, err := Observe(registry, stored, tree, fixtureIDs)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"version":1,"workspace":"98","contexts":[{"id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","state":"active"},{"id":"6ba7b811-9dad-11d1-80b4-00c04fd430c8","state":"active"}],"live":{"workspaces":[{"name":"98","restore_mode":"layout","tiling":{"layout":"tabbed","children":[{"context_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8"},{"context_id":"6ba7b811-9dad-11d1-80b4-00c04fd430c8"}]},"floating":[]}]},"stored":{"workspaces":[{"name":"98","restore_mode":"layout","tiling":{"layout":"tabbed","children":[{"context_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8"},{"context_id":"6ba7b811-9dad-11d1-80b4-00c04fd430c8"}]},"floating":[]}]},"terminal_count":2,"staging_managed_windows":0,"matches_fixture":true,"exclusions":["proportions","geometry","focus"]}`
	if string(encoded) != want {
		t.Fatalf("observation = %s\nwant %s", encoded, want)
	}
}

func TestObserveIgnoresOnlyPlainSingletonSplitWrappers(t *testing.T) {
	registry, stored, tree := fixture(t)
	stored.Workspaces[0].Tiling = &session.LayoutNode{
		Layout: session.LayoutSplitHorizontal, Children: []session.LayoutNode{*stored.Workspaces[0].Tiling},
	}
	workspace := tree.Nodes[0]
	workspace.Layout = "splitv"
	workspace.Nodes = []*swayipc.TreeNode{{ID: 30, Type: "con", Layout: "tabbed", Nodes: workspace.Nodes}}
	if _, err := Observe(registry, stored, tree, fixtureIDs); err != nil {
		t.Fatalf("irrelevant split wrapper prevented semantic agreement: %v", err)
	}
}

func TestObserveSortsRegistryEvidenceWithoutSortingTabOrder(t *testing.T) {
	registry, stored, tree := fixture(t)
	c := stored.Workspaces[0].Tiling.Children
	c[0], c[1] = c[1], c[0]
	tree.Nodes[0].Nodes[0], tree.Nodes[0].Nodes[1] = tree.Nodes[0].Nodes[1], tree.Nodes[0].Nodes[0]
	got, err := Observe(registry, stored, tree, []session.ContextID{fixtureIDs[1], fixtureIDs[0]})
	if err != nil {
		t.Fatal(err)
	}
	if got.Contexts[0].ID != fixtureIDs[0] || got.Contexts[1].ID != fixtureIDs[1] ||
		*got.Live.Workspaces[0].Tiling.Children[0].ContextID != fixtureIDs[1] {
		t.Fatalf("registry order or tab order was lost: %+v", got)
	}
}

func TestObserveDoesNotLoseMeaningfulSingletonWorkspaceGroups(t *testing.T) {
	for _, kind := range []string{"tabbed", "stacked"} {
		t.Run(kind, func(t *testing.T) {
			registry, stored, tree := fixture(t)
			workspace := tree.Nodes[0]
			workspace.Layout = kind
			workspace.Nodes = []*swayipc.TreeNode{{ID: 30, Type: "con", Layout: "tabbed", Nodes: workspace.Nodes}}
			if _, err := Observe(registry, stored, tree, fixtureIDs); err == nil {
				t.Fatal("canonical workspace capture hid a meaningful singleton group")
			}
		})
	}
}

func TestObserveExplicitlyExcludesFocusAndProportions(t *testing.T) {
	registry, stored, tree := fixture(t)
	stored.Workspaces[0].FocusedContext = &fixtureIDs[0]
	stored.Workspaces[0].Tiling.Children[0].Proportion = 0.25
	stored.Workspaces[0].Tiling.Children[1].Proportion = 0.75
	tree.Nodes[0].Focus = []int64{11, 10}
	if _, err := Observe(registry, stored, tree, fixtureIDs); err != nil {
		t.Fatalf("excluded focus/proportion difference prevented observation: %v", err)
	}
}
