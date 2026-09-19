package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func TestSessionRuntimeDelayedApplicationPreservesObservedLayoutChange(t *testing.T) {
	runtime, requester, app, terminal, window, start := delayedApplicationScenario(t)
	tree := daemonTree("98", terminal)
	// An IPC layout command need not generate a binding or focus event.
	tree.Nodes[0].Nodes[0].Layout = "stacked"
	if _, err := runtime.Reconcile(tree, start.Add(10500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	tree.Nodes[0].Nodes[0].Nodes = append(tree.Nodes[0].Nodes[0].Nodes, window)
	if _, err := runtime.Reconcile(tree, start.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	mark, _ := app.ID.Mark()
	window.Marks = []string{mark}
	before := len(requester.commands)
	if _, err := runtime.Reconcile(tree, start.Add(12*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, command := range requester.commands[before:] {
		if strings.Contains(command, sessionstate.RestoreStagingWorkspace) || strings.Contains(command, "layout ") {
			t.Fatalf("observed user layout was replaced by startup reconstruction: %q", requester.commands[before:])
		}
	}
}

func TestSessionRuntimeApplicationPreservesOwnPlacement(t *testing.T) {
	for _, workspace := range []string{"98", "99"} {
		for _, userEdit := range []bool{false, true} {
			name := "source"
			if workspace == "99" {
				name = "destination"
			}
			if userEdit {
				name += "/subsequent_user_layout"
			} else {
				name += "/restore"
			}
			t.Run(name, func(t *testing.T) {
				runtime, requester, app, tree, target, window, start := applicationOwnPlacementScenario(t, workspace)
				if _, err := runtime.Reconcile(tree, start.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				if runtime.restoreCancelled {
					t.Fatal("successful own placement cancelled pending application restore")
				}
				if userEdit {
					// Observe this after the fresh tree containing the completed
					// owned move. A one-shot baseline reset must not mask later edits.
					target.Layout = "stacked"
					if _, err := runtime.Reconcile(tree, start.Add(2*time.Second)); err != nil {
						t.Fatal(err)
					}
					if !runtime.restoreCancelled {
						t.Fatal("subsequent user layout change did not cancel restoration")
					}
				}
				target.Nodes = append(target.Nodes, window)
				before := len(requester.commands)
				if _, err := runtime.Reconcile(tree, start.Add(3*time.Second)); err != nil {
					t.Fatal(err)
				}
				mark, _ := app.ID.Mark()
				window.Marks = []string{mark}
				if _, err := runtime.Reconcile(tree, start.Add(4*time.Second)); err != nil {
					t.Fatal(err)
				}
				structural := false
				for _, command := range requester.commands[before:] {
					structural = structural || strings.Contains(command, sessionstate.RestoreStagingWorkspace) || strings.Contains(command, "layout ")
				}
				if structural == userEdit {
					t.Fatalf("startup reconstruction after own placement: structural=%t userEdit=%t commands=%q", structural, userEdit, requester.commands[before:])
				}
			})
		}
	}
}

// Return a fresh tree after the runtime successfully moves terminal B from 98
// to 99. The missing application belongs to either the source or destination;
// both fixtures retain a live terminal A there while waiting for it.
func applicationOwnPlacementScenario(t *testing.T, workspace string) (*sessionRuntime, *recordingRequester, sessionstate.Context, *Node, *Node, *Node, time.Time) {
	t.Helper()
	previous, requester, app, terminal, window, start := applicationStartupScenario(t, false)
	terminalID := sessionstate.ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	otherID := sessionstate.ContextID("6ba7b811-9dad-11d1-80b4-00c04fd430c8")
	registry := sessionRegistryIDs(terminalID, otherID)
	registry.Contexts = append(registry.Contexts, app)
	if err := sessionstate.RegistryStoreFor(previous.root).Save(registry); err != nil {
		t.Fatal(err)
	}
	desired := exactDaemonSnapshot(workspace, terminalID, app.ID)
	if workspace == "98" {
		desired.Workspaces = append(desired.Workspaces, exactDaemonSnapshot("99", otherID).Workspaces...)
	} else {
		desired = exactDaemonSnapshot(workspace, terminalID, otherID, app.ID)
	}
	desired.Workspaces[0].Tiling.Layout = sessionstate.LayoutTabbed
	if err := sessionstate.LayoutStoreFor(previous.root).Save(desired); err != nil {
		t.Fatal(err)
	}
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Root: previous.root, StartedAt: start, CompositorID: strings.Repeat("f", 64),
		ApplicationLauncher: &recordingApplicationLauncher{},
		ApplicationRestore: sessionstate.ApplicationRestoreOptions{
			AdoptionGrace: time.Second, CloseGrace: 2 * time.Second, LaunchTimeout: 30 * time.Second, MaxConcurrent: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	terminal.Marks = nil
	other := managedDaemonLeaf(t, 43, otherID)
	other.Marks = nil
	tree := daemonTree("98", other)
	source := tree.Nodes[0].Nodes[0]
	destination := &Node{ID: 4, Name: "99", Type: "workspace", Layout: "splith"}
	tree.Nodes[0].Nodes = append(tree.Nodes[0].Nodes, destination)
	target := source
	if workspace == "99" {
		target = destination
	}
	target.Nodes = append(target.Nodes, terminal)
	before := len(requester.commands)
	if _, err := runtime.Reconcile(tree, start); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(requester.commands[before:], `[con_id=43] move container to workspace "99"`) {
		t.Fatalf("fixture did not exercise owned placement: %q", requester.commands[before:])
	}
	// Apply only the commands acknowledged by the recording compositor.
	terminalMark, _ := terminalID.Mark()
	otherMark, _ := otherID.Mark()
	terminal.Marks, other.Marks = []string{terminalMark}, []string{otherMark}
	if workspace == "98" {
		source.Nodes = []*Node{terminal}
	} else {
		source.Nodes = nil
	}
	destination.Nodes = append(destination.Nodes, other)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "move", Container: other}, start.Add(time.Second))
	return runtime, requester, app, tree, target, window, start
}

func TestSessionRuntimeDelayedApplicationPreservesObservedResize(t *testing.T) {
	runtime, requester, app, terminal, window, start := delayedApplicationScenario(t)
	// Keep the window set and hierarchy unchanged: only the fresh geometry
	// differs from the last observation, as with an eventless IPC resize.
	terminal.Rect.Width = 640
	if _, err := runtime.Reconcile(daemonTree("98", terminal), start.Add(10500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Reconcile(daemonTree("98", terminal, window), start.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	mark, _ := app.ID.Mark()
	window.Marks = []string{mark}
	before := len(requester.commands)
	if _, err := runtime.Reconcile(daemonTree("98", terminal, window), start.Add(12*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, command := range requester.commands[before:] {
		if strings.Contains(command, sessionstate.RestoreStagingWorkspace) {
			t.Fatalf("observed resize was replaced by startup reconstruction: %q", requester.commands[before:])
		}
	}
}

func TestSessionRuntimeDelayedApplicationAllowsMappingResize(t *testing.T) {
	runtime, requester, app, terminal, window, start := delayedApplicationScenario(t)
	// Mapping a sibling normally changes existing rectangles and percentages.
	terminal.Rect.Width, window.Rect.Width = 640, 640
	percent := 0.5
	terminal.Percent, window.Percent = &percent, &percent
	if _, err := runtime.Reconcile(daemonTree("98", terminal, window), start.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	mark, _ := app.ID.Mark()
	window.Marks = []string{mark}
	before := len(requester.commands)
	if _, err := runtime.Reconcile(daemonTree("98", terminal, window), start.Add(12*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, command := range requester.commands[before:] {
		if strings.Contains(command, sessionstate.RestoreStagingWorkspace) {
			return
		}
	}
	t.Fatalf("normal mapping-induced resize suppressed startup reconstruction: %q", requester.commands[before:])
}

func TestSessionRuntimeApplicationPreservesLayoutEditBeforeStartupTimeout(t *testing.T) {
	for _, name := range []string{"timely", "late"} {
		t.Run(name, func(t *testing.T) {
			runtime, requester, app, terminal, window, start := applicationStartupScenario(t, false)
			tree := daemonTree("98", terminal)
			tree.Nodes[0].Nodes[0].Layout = "stacked"
			if _, err := runtime.Reconcile(tree, start.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			arrival := 3 * time.Second
			if name == "late" {
				if _, err := runtime.Reconcile(tree, start.Add(sessionStartupSettleDelay)); err != nil {
					t.Fatal(err)
				}
				arrival = 11 * time.Second
			}
			tree.Nodes[0].Nodes[0].Nodes = append(tree.Nodes[0].Nodes[0].Nodes, window)
			before := len(requester.commands)
			if _, err := runtime.Reconcile(tree, start.Add(arrival)); err != nil {
				t.Fatal(err)
			}
			mark, _ := app.ID.Mark()
			window.Marks = []string{mark}
			if _, err := runtime.Reconcile(tree, start.Add(arrival+time.Second)); err != nil {
				t.Fatal(err)
			}
			for _, command := range requester.commands[before:] {
				if strings.Contains(command, sessionstate.RestoreStagingWorkspace) || strings.Contains(command, "layout ") {
					t.Fatalf("layout edit during startup was overwritten: %q", requester.commands[before:])
				}
			}
		})
	}
}
