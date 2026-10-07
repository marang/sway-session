package main

import (
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func TestSessionRuntimeForeignQueuedNewPreservesLateApplicationIntent(t *testing.T) {
	for _, change := range []string{"automatic_mapping", "layout_edit_before_classification", "resize_before_classification", "foreign_resize_before_classification"} {
		t.Run(change, func(t *testing.T) {
			userEdit := change == "layout_edit_before_classification" || change == "resize_before_classification"
			runtime, requester, app, terminal, window, start := delayedApplicationScenario(t)
			root := daemonTree("98", terminal)
			workspace := root.Nodes[0].Nodes[0]
			identity := "org.example.Authentication"
			prompt := &Node{ID: 43, Type: "con", AppID: &identity, Focused: true}
			workspace.FloatingNodes = []*Node{{ID: 44, Type: "floating_con", Nodes: []*Node{prompt}}}
			workspace.Focus = []int64{44, terminal.ID}
			now := start.Add(11 * time.Second)
			// Mapping can settle existing geometry. The full view-set guard
			// still distinguishes this arrival from later manual resizing.
			terminal.Rect.Width = 640
			// A fresh tree can see the mapped view while its new event is
			// still queued behind another event or a timer reconciliation.
			if _, err := runtime.Reconcile(root, now); err != nil {
				t.Fatal(err)
			}
			if runtime.restoreCancelled {
				t.Fatal("an independent floating view changed saved-window layout")
			}
			switch change {
			case "layout_edit_before_classification":
				// Do not lose a simultaneous eventless layout command by
				// replacing the previous observation during classification.
				workspace.Layout = "splitv"
			case "resize_before_classification":
				terminal.Rect.Width = 320
			case "foreign_resize_before_classification":
				prompt.Rect.Width = 320
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: prompt}, now)
			if _, err := runtime.Reconcile(root, now.Add(time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if runtime.restoreCancelled != userEdit {
				t.Fatalf("foreign classification was confused with a saved-window layout change: cancelled=%t userEdit=%t", runtime.restoreCancelled, userEdit)
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: prompt}, now)
			if runtime.restoreCancelled != userEdit {
				t.Fatal("queued automatic focus changed the saved/user layout decision")
			}
			workspace.Nodes = append(workspace.Nodes, window)
			if _, err := runtime.Reconcile(root, start.Add(12*time.Second)); err != nil {
				t.Fatal(err)
			}
			mark, _ := app.ID.Mark()
			window.Marks = []string{mark}
			before := len(requester.commands)
			if _, err := runtime.Reconcile(root, start.Add(13*time.Second)); err != nil {
				t.Fatal(err)
			}
			structural := false
			for _, command := range requester.commands[before:] {
				structural = structural || strings.Contains(command, sessionstate.RestoreStagingWorkspace) || strings.Contains(command, "layout ")
			}
			if structural == userEdit {
				t.Fatalf("late application lost the saved/user layout decision while the dialog stayed open: structural=%t userEdit=%t commands=%q", structural, userEdit, requester.commands[before:])
			}
		})
	}
}

func TestSessionRuntimePreexistingForeignClosePreservesLateApplicationIntent(t *testing.T) {
	for _, userEdit := range []bool{false, true} {
		name := "automatic_disappearance"
		if userEdit {
			name = "layout_edit_before_disappearance"
		}
		t.Run(name, func(t *testing.T) {
			previous, requester, app, terminal, window, start := delayedApplicationScenario(t)
			identity := "org.example.PreexistingAuthentication"
			prompt := &Node{ID: 43, Type: "con", AppID: &identity}
			root := daemonTree("98", terminal)
			workspace := root.Nodes[0].Nodes[0]
			workspace.FloatingNodes = []*Node{{ID: 44, Type: "floating_con", Nodes: []*Node{prompt}}}
			// The dialog exists before this runtime starts. No new or close
			// event will identify it; only complete trees are observed.
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
			enableApplicationLaunchFixture(runtime, start.Add(time.Second))
			for _, elapsed := range []time.Duration{0, sessionStartupSettleDelay} {
				if _, err := runtime.Reconcile(root, start.Add(elapsed)); err != nil {
					t.Fatal(err)
				}
			}
			if userEdit {
				workspace.Layout = "splitv"
			}
			workspace.FloatingNodes = nil
			if _, err := runtime.Reconcile(root, start.Add(11*time.Second)); err != nil {
				t.Fatal(err)
			}
			if runtime.restoreCancelled != userEdit {
				t.Fatalf("preexisting unrelated view entered saved-window layout intent: cancelled=%t userEdit=%t", runtime.restoreCancelled, userEdit)
			}
			workspace.Nodes = append(workspace.Nodes, window)
			if _, err := runtime.Reconcile(root, start.Add(12*time.Second)); err != nil {
				t.Fatal(err)
			}
			mark, _ := app.ID.Mark()
			window.Marks = []string{mark}
			before := len(requester.commands)
			if _, err := runtime.Reconcile(root, start.Add(13*time.Second)); err != nil {
				t.Fatal(err)
			}
			structural := false
			for _, command := range requester.commands[before:] {
				structural = structural || strings.Contains(command, sessionstate.RestoreStagingWorkspace) || strings.Contains(command, "layout ")
			}
			if structural == userEdit {
				t.Fatalf("late application lost saved/user layout after eventless foreign disappearance: structural=%t userEdit=%t commands=%q", structural, userEdit, requester.commands[before:])
			}
		})
	}
}

func TestSessionRuntimeLateApplicationIdentitySettlementPreservesSavedLayout(t *testing.T) {
	runtime, requester, app, terminal, window, start := delayedApplicationScenario(t)
	identity := "org.example.InitialApplicationIdentity"
	window.AppID, window.SandboxAppID = &identity, nil
	window.Rect.Width, window.Rect.Height = 800, 600
	root := daemonTree("98", terminal)
	workspace := root.Nodes[0].Nodes[0]
	workspace.FloatingNodes = []*Node{window}
	if _, err := runtime.Reconcile(root, start.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	// The same view settles its registered identity without another mapping.
	// New owned geometry must not be compared with an unrelated view's data.
	identity, sandbox := app.App.Identity.WaylandAppID, app.App.Identity.SandboxAppID
	window.AppID, window.SandboxAppID = &identity, &sandbox
	terminal.Rect.Width, window.Rect.Width = 640, 640
	window.Rect.Height = 480
	if _, err := runtime.Reconcile(root, start.Add(12*time.Second)); err != nil {
		t.Fatal(err)
	}
	if runtime.restoreCancelled {
		t.Fatal("settling application ownership was mistaken for a user resize")
	}
	mark, _ := app.ID.Mark()
	window.Marks = []string{mark}
	before := len(requester.commands)
	if _, err := runtime.Reconcile(root, start.Add(13*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, command := range requester.commands[before:] {
		if strings.Contains(command, sessionstate.RestoreStagingWorkspace) {
			return
		}
	}
	t.Fatalf("registered late application did not resume saved layout: commands=%q", requester.commands[before:])
}
