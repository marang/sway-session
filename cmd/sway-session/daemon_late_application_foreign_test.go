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
