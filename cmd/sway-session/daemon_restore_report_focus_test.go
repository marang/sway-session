package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

const restoreReportActiveID sessionstate.ContextID = "33333333-3333-4333-8333-333333333333"

func restoreReportLocalFocusFixture(t *testing.T) (sessionstate.Registry, sessionstate.LayoutSnapshot, *Node) {
	t.Helper()
	registry := sessionRegistryIDs(testManagedContextID, restoreReportSecondID, restoreReportActiveID)
	snapshot := exactDaemonSnapshot("98", testManagedContextID, restoreReportSecondID)
	inactiveFocus := restoreReportSecondID
	snapshot.Workspaces[0].FocusedContext = &inactiveFocus
	active := exactDaemonSnapshot("99", restoreReportActiveID).Workspaces[0]
	activeFocus := restoreReportActiveID
	active.FocusedContext = &activeFocus
	snapshot.Workspaces = append(snapshot.Workspaces, active)

	root := daemonTree("98", managedDaemonLeaf(t, 41, testManagedContextID), managedDaemonLeaf(t, 42, restoreReportSecondID))
	root.Focus = []int64{2}
	output := root.Nodes[0]
	output.Focus = []int64{4, 3}
	output.Nodes[0].Focus = []int64{42, 41}
	activeLeaf := managedDaemonLeaf(t, 43, restoreReportActiveID)
	activeLeaf.Focused = true
	output.Nodes = append(output.Nodes, &Node{ID: 4, Name: "99", Type: "workspace", Layout: "splith", Focus: []int64{43}, Nodes: []*Node{activeLeaf}})
	return registry, snapshot, root
}

func assertRestoreReportObservationPreserved(t *testing.T, runtime *sessionRuntime, tree *Node, before []byte, snapshot sessionstate.LayoutSnapshot) {
	t.Helper()
	requester := runtime.client.(*recordingRequester)
	if len(requester.commands) != 0 || len(requester.barriers) != 0 {
		t.Errorf("report observation issued compositor effects: commands=%v barriers=%v", requester.commands, requester.barriers)
	}
	after, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("report observation changed the tree or focus")
	}
	var stored sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(runtime.root).LoadIntoContext(t.Context(), &stored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, snapshot) || !reflect.DeepEqual(runtime.persisted, snapshot) {
		t.Error("report observation changed the saved workspace layout or focus")
	}
}

func TestRestoreReportCompletesInactiveWorkspaceWithLocalFocus(t *testing.T) {
	for _, test := range []struct {
		name         string
		observations []time.Duration
	}{
		{name: "initial_and_after_deadline", observations: []time.Duration{0, restoreReportTimeout + time.Second}},
		{name: "first_observation_after_deadline", observations: []time.Duration{restoreReportTimeout + time.Second}},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry, snapshot, tree := restoreReportLocalFocusFixture(t)
			runtime, now := restoreReportRuntime(t, registry, snapshot)
			before, err := json.Marshal(tree)
			if err != nil {
				t.Fatal(err)
			}
			requested := restoreReportOutcome(t, runtime, "automatic", restoreReportSecondID).Requested
			if !requested.Layout || len(requested.LayoutDigest) != 64 {
				t.Fatalf("fixture did not request exact layout: %+v", requested)
			}
			for _, delay := range test.observations {
				if err := runtime.observeRestoreReport(tree, registry, now.Add(delay)); err != nil {
					t.Fatal(err)
				}
				for _, id := range []sessionstate.ContextID{testManagedContextID, restoreReportSecondID, restoreReportActiveID} {
					record := restoreReportOutcome(t, runtime, "automatic", id)
					if record.Status != "completed" || record.Reason != "restore_complete" || !record.WindowMapped || !record.PlacementApplied || !record.LayoutApplied {
						t.Errorf("at %s, correct workspace for %s did not complete: %+v", delay, id, record)
					}
				}
				if record := restoreReportOutcome(t, runtime, "automatic", restoreReportSecondID); record.Requested != requested {
					t.Error("completion changed the recorded request or layout digest")
				}
				assertRestoreReportObservationPreserved(t, runtime, tree, before, snapshot)
			}
		})
	}
}

func TestRestoreReportRejectsIncorrectInactiveWorkspaceLocalFocus(t *testing.T) {
	for _, test := range []struct {
		name  string
		focus []int64
	}{
		{name: "wrong_selected_context", focus: []int64{41, 42}},
		{name: "malformed_focus_path", focus: []int64{777, 42, 41}},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry, snapshot, tree := restoreReportLocalFocusFixture(t)
			tree.Nodes[0].Nodes[0].Focus = test.focus
			runtime, now := restoreReportRuntime(t, registry, snapshot)
			before, err := json.Marshal(tree)
			if err != nil {
				t.Fatal(err)
			}
			requested := restoreReportOutcome(t, runtime, "automatic", restoreReportSecondID).Requested
			for _, delay := range []time.Duration{0, restoreReportTimeout + time.Second} {
				if err := runtime.observeRestoreReport(tree, registry, now.Add(delay)); err != nil {
					t.Fatal(err)
				}
				for _, id := range []sessionstate.ContextID{testManagedContextID, restoreReportSecondID} {
					record := restoreReportOutcome(t, runtime, "automatic", id)
					wantStatus, wantReason := "pending", "placement_applied"
					if delay > restoreReportTimeout {
						wantStatus, wantReason = "failed", "layout_timeout"
					}
					if record.Status != wantStatus || record.Reason != wantReason || !record.WindowMapped || !record.PlacementApplied || record.LayoutApplied {
						t.Errorf("at %s, incorrect local focus for %s credited as restored: %+v", delay, id, record)
					}
				}
				if record := restoreReportOutcome(t, runtime, "automatic", restoreReportActiveID); record.Status != "completed" || !record.LayoutApplied {
					t.Errorf("incorrect inactive focus blocked the active workspace: %+v", record)
				}
				if record := restoreReportOutcome(t, runtime, "automatic", restoreReportSecondID); record.Requested != requested {
					t.Error("rejected proof changed the recorded request or layout digest")
				}
				assertRestoreReportObservationPreserved(t, runtime, tree, before, snapshot)
			}
		})
	}
}
