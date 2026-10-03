package main

import (
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

func TestScratchpadReportRequiresObservedMembershipAndVisibility(t *testing.T) {
	for _, visible := range []bool{false, true} {
		t.Run(map[bool]string{false: "hidden", true: "shown"}[visible], func(t *testing.T) {
			_, _, _, app, _ := testApplicationRuntime(t)
			registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{app}}
			placement := sessionstate.ScratchpadPlacement{ContextID: app.ID, Visible: visible}
			if visible {
				placement.Workspace = "98"
			}
			snapshot := sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}, Scratchpad: []sessionstate.ScratchpadPlacement{placement}}
			runtime, now := restoreReportRuntime(t, registry, snapshot)
			identity := app.App.Identity.WaylandAppID
			leaf := &Node{ID: 41, Type: "con", AppID: &identity}
			tree := daemonTree("98", leaf)
			if err := runtime.observeRestoreReport(tree, registry, now); err != nil {
				t.Fatal(err)
			}
			record := restoreReportOutcome(t, runtime, "automatic", app.ID)
			if !record.WindowMapped || record.Status != "pending" || record.PlacementApplied || record.Requested.Scratchpad == nil || record.Requested.Workspace != "" {
				t.Fatalf("window mapping falsely completed scratchpad placement: %+v", record)
			}
			leaf.ScratchpadState = "fresh"
			workspace := tree.Nodes[0].Nodes[0]
			workspace.Nodes = nil
			workspace.FloatingNodes = []*Node{leaf}
			// The opposite visibility is still not sufficient evidence.
			if visible {
				workspace.Name = "__i3_scratch"
			}
			if err := runtime.observeRestoreReport(tree, registry, now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			record = restoreReportOutcome(t, runtime, "automatic", app.ID)
			if record.Status != "pending" || record.PlacementApplied {
				t.Fatalf("opposite visibility proved placement: %+v", record)
			}
			if visible {
				workspace.Name = "98"
			} else {
				workspace.Name = "__i3_scratch"
			}
			if err := runtime.observeRestoreReport(tree, registry, now.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			record = restoreReportOutcome(t, runtime, "automatic", app.ID)
			if record.Status != "completed" || !record.PlacementApplied {
				t.Fatalf("observed scratchpad placement not completed: %+v", record)
			}
		})
	}
}
