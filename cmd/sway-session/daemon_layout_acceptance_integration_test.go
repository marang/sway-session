package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// This test uses only a private compositor and disposable state. It maps new
// windows, restores each saved shape, and persists the fresh capture.
func TestSessionRuntimeLayoutShapesHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway/Alacritty integration")
	}
	for _, name := range []string{"sway", "alacritty", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("headless integration requires %s: %v", name, err)
		}
	}
	ids := []sessionstate.ContextID{
		testManagedContextID,
		"6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"6ba7b811-9dad-11d1-80b4-00c04fd430c8",
	}
	for _, shape := range []struct {
		name    string
		desired func() sessionstate.LayoutSnapshot
	}{
		{name: "vertical_split", desired: func() sessionstate.LayoutSnapshot {
			snapshot := exactDaemonSnapshot("98", ids...)
			snapshot.Workspaces[0].Tiling.Layout = sessionstate.LayoutSplitVertical
			return snapshot
		}},
		{name: "nested_tabbed", desired: func() sessionstate.LayoutSnapshot {
			snapshot := exactDaemonSnapshot("98", ids...)
			children := snapshot.Workspaces[0].Tiling.Children
			snapshot.Workspaces[0].Tiling.Children = []sessionstate.LayoutNode{
				children[0], {Layout: sessionstate.LayoutTabbed, Children: children[1:]},
			}
			return snapshot
		}},
		{name: "floating", desired: func() sessionstate.LayoutSnapshot {
			snapshot := exactDaemonSnapshot("98", ids...)
			snapshot.Workspaces[0].Tiling.Children = snapshot.Workspaces[0].Tiling.Children[:2]
			snapshot.Workspaces[0].Floating = []sessionstate.LayoutNode{{
				ContextID: &ids[2], Geometry: &sessionstate.Geometry{X: 200, Y: 150, Width: 500, Height: 300},
			}}
			return snapshot
		}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			h := newRestoreCleanupHeadless(t)
			registry := sessionRegistryIDs(ids...)
			for index := range registry.Contexts {
				registry.Contexts[index].Launcher.Cwd = h.root
			}
			if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
				t.Fatal(err)
			}
			if err := sessionstate.LayoutStoreFor(h.state).Save(shape.desired()); err != nil {
				t.Fatal(err)
			}
			h.command("workspace 98")
			owned := make([]int64, 0, len(ids))
			for _, id := range ids {
				owned = append(owned, h.terminal(id))
			}
			h.command("workspace 99")
			requester := &restoreCleanupRealRequester{Client: h.client}
			stream := &swayipc.EventStreamState{}
			start := time.Now()
			now := start
			runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
				Context: h.ctx, Root: h.state, StartedAt: start, EventStreamState: stream,
				IndicatorCatalog: func() (sessionstate.DesktopCatalog, error) { return sessionstate.DesktopCatalog{}, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			h.subscribe(runtime, stream)
			settled := 0
			for pass := range 128 {
				_, err := runtime.Reconcile(h.tree(), now)
				if err != nil {
					t.Fatalf("pass %d: %v; commands=%q", pass, err, requester.commands)
				}
				h.drain(runtime)
				now = now.Add(time.Second)
				if runtime.startupComplete && runtime.restoreProgress == nil && !runtime.restoreCleanupPending {
					settled++
					if settled == 4 {
						break
					}
				} else {
					settled = 0
				}
			}
			if settled != 4 || !slices.ContainsFunc(requester.commands, func(command string) bool {
				return strings.Contains(command, sessionstate.RestoreStagingWorkspace)
			}) {
				t.Fatalf("did not reconstruct %s: settled=%d commands=%q", shape.name, settled, requester.commands)
			}
			captured, err := sessionstate.CaptureLayout(h.tree(), registry)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := layoutTopology(captured), layoutTopology(shape.desired()); !reflect.DeepEqual(got, want) {
				currentJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				t.Fatalf("did not converge to saved %s shape: current=%s want=%s commands=%q", shape.name, currentJSON, wantJSON, requester.commands)
			}
			if shape.name == "floating" {
				got := captured.Workspaces[0].Floating[0].Geometry
				want := shape.desired().Workspaces[0].Floating[0].Geometry
				if got == nil || *got != *want {
					t.Fatalf("floating geometry was not restored: got=%+v want=%+v", got, want)
				}
			}
			now = now.Add(sessionSnapshotDebounce)
			if err := runtime.Flush(now); err != nil {
				t.Fatal(err)
			}
			var saved sessionstate.LayoutSnapshot
			if err := sessionstate.LayoutStoreFor(h.state).LoadInto(&saved); err != nil {
				t.Fatal(err)
			}
			savedJSON, _ := json.Marshal(saved)
			capturedJSON, _ := json.Marshal(captured)
			if string(savedJSON) != string(capturedJSON) {
				t.Fatalf("durable capture differs from the restored Sway tree: saved=%s captured=%s", savedJSON, capturedJSON)
			}
			if got, want := layoutTopology(saved), layoutTopology(shape.desired()); !reflect.DeepEqual(got, want) {
				t.Fatalf("did not persist the restored shape: got=%+v want=%+v", got, want)
			}
			commandsAfterRestore := len(requester.commands)
			for pass := range 4 {
				now = now.Add(sessionStartupSettleDelay)
				if _, err := runtime.Reconcile(h.tree(), now); err != nil {
					t.Fatalf("steady pass %d: %v", pass, err)
				}
				h.drain(runtime)
				if err := runtime.Flush(now.Add(sessionSnapshotDebounce)); err != nil {
					t.Fatal(err)
				}
				var again sessionstate.LayoutSnapshot
				if err := sessionstate.LayoutStoreFor(h.state).LoadInto(&again); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(again, saved) || len(requester.commands) != commandsAfterRestore {
					t.Fatalf("steady pass %d changed layout or issued commands: saved=%+v again=%+v commands=%q", pass, saved, again, requester.commands[commandsAfterRestore:])
				}
			}
			for _, id := range owned {
				if findContainerByID(h.tree(), id) == nil {
					t.Fatalf("lost a managed window %d", id)
				}
			}
		})
	}
}

// Sway may retain proportions, focus, and singleton wrappers which were not
// specified in the seed. Only the structural tree and child order matter here.
func layoutTopology(snapshot sessionstate.LayoutSnapshot) sessionstate.LayoutSnapshot {
	result := sessionstate.LayoutSnapshot{Version: snapshot.Version, Workspaces: make([]sessionstate.WorkspaceLayout, len(snapshot.Workspaces))}
	var node func(sessionstate.LayoutNode) sessionstate.LayoutNode
	node = func(current sessionstate.LayoutNode) sessionstate.LayoutNode {
		if current.ContextID != nil {
			return sessionstate.LayoutNode{ContextID: current.ContextID}
		}
		children := make([]sessionstate.LayoutNode, len(current.Children))
		for index, child := range current.Children {
			children[index] = node(child)
		}
		if len(children) == 1 {
			return children[0]
		}
		return sessionstate.LayoutNode{Layout: current.Layout, Children: children}
	}
	for index, workspace := range snapshot.Workspaces {
		result.Workspaces[index] = sessionstate.WorkspaceLayout{Name: workspace.Name, RestoreMode: workspace.RestoreMode, PlacementContexts: workspace.PlacementContexts}
		if workspace.Tiling != nil {
			clean := node(*workspace.Tiling)
			result.Workspaces[index].Tiling = &clean
		}
		for _, floating := range workspace.Floating {
			result.Workspaces[index].Floating = append(result.Workspaces[index].Floating, node(floating))
		}
	}
	return result
}
