package main

import (
	"reflect"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func TestSessionRuntimePromptPausePreservesEventlessUserLayoutChange(t *testing.T) {
	runtime, compositor, prompt, now := newPausedPromptScenario(t)
	workspace := compositor.root.Nodes[0].Nodes[0]
	before := len(compositor.commands)
	// Observe an unchanged paused tree before the eventless edit, just as the
	// daemon's periodic observation would. No binding or focus event follows.
	if _, err := runtime.Reconcile(compositor.root, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	workspace.Layout = "splitv"
	if _, err := runtime.Reconcile(compositor.root, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if !runtime.restoreCancelled {
		t.Fatal("eventless layout change during the prompt pause retained obsolete startup reconstruction")
	}
	if len(compositor.commands) != before || workspace.Layout != "splitv" || !prompt.Focused {
		t.Fatalf("cancelling paused reconstruction disturbed the user's layout or prompt: layout=%q focus=%t commands=%q", workspace.Layout, prompt.Focused, compositor.commands[before:])
	}
	// The prompt stays mapped: cancellation must release capture independently
	// of its lifetime, so the user's replacement layout becomes durable.
	if _, err := runtime.Reconcile(compositor.root, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Flush(now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var stored sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(runtime.root).LoadInto(&stored); err != nil {
		t.Fatal(err)
	}
	want := exactDaemonSnapshot("98", testManagedContextID, "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	want.Workspaces[0].Tiling.Layout = sessionstate.LayoutSplitVertical
	if !reflect.DeepEqual(stored, want) {
		t.Fatalf("eventless user layout did not replace paused startup intent:\n got: %+v\nwant: %+v", stored, want)
	}
}

func newPausedPromptScenario(t *testing.T) (*sessionRuntime, *cleanupCompositor, *Node, time.Time) {
	t.Helper()
	runtime, compositor, first, now := newMappingFocusScenario(t)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: first}, now)
	if _, err := runtime.Reconcile(compositor.root, now); err != nil {
		t.Fatal(err)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: first}, now)
	compositor.drain(runtime, now)

	secondID := sessionstate.ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	second := managedDaemonLeaf(t, 42, secondID)
	promptAppID := "org.example.Authentication"
	prompt := &Node{
		ID: 43, Type: "con", AppID: &promptAppID, Focused: true,
		Rect: swayipc.Rect{Width: 400, Height: 200},
	}
	workspace := compositor.root.Nodes[0].Nodes[0]
	first.Focused = false
	workspace.Nodes = append(workspace.Nodes, second)
	workspace.FloatingNodes = []*Node{prompt}
	workspace.Focus = []int64{prompt.ID, first.ID, second.ID}
	before := len(compositor.commands)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: prompt}, now)
	if _, err := runtime.Reconcile(compositor.root, now); err != nil {
		t.Fatal(err)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: prompt}, now)
	compositor.drain(runtime, now)
	if runtime.restoreCancelled || runtime.startupComplete || len(compositor.commands) != before {
		t.Fatalf("mapping the unrelated prompt did not pause pending reconstruction: cancelled=%t complete=%t commands=%q", runtime.restoreCancelled, runtime.startupComplete, compositor.commands[before:])
	}
	return runtime, compositor, prompt, now
}

func TestSessionRuntimePromptStreamLossPreservesSavedIntent(t *testing.T) {
	runtime, compositor, prompt, now := newPausedPromptScenario(t)
	want := runtime.persisted
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 1}, now)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "disconnected", StreamEpoch: 1}, now)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 2}, now)
	for range 3 {
		if _, err := runtime.Reconcile(compositor.root, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.Flush(now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var stored sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(runtime.root).LoadInto(&stored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, want) {
		t.Fatal("uncertain event-stream loss replaced the original saved tabbed intent with incomplete live capture")
	}
	if !prompt.Focused {
		t.Fatal("uncertain stream loss disturbed the prompt")
	}
	if outcome := restoreReportOutcome(t, runtime, "automatic", testManagedContextID); outcome.Reason == "user_cancelled" {
		t.Fatal("event-stream loss was reported as deliberate user cancellation")
	}
	// Explicit compositor intent releases the conservative capture guard.
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding}, now.Add(2*time.Second))
	compositor.root.Nodes[0].Nodes[0].Layout = "splitv"
	if _, err := runtime.Reconcile(compositor.root, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Flush(now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := sessionstate.LayoutStoreFor(runtime.root).LoadInto(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Workspaces[0].Tiling == nil || stored.Workspaces[0].Tiling.Layout != sessionstate.LayoutSplitVertical {
		t.Fatal("explicit user intent did not release uncertain capture preservation")
	}
}

func TestSessionRuntimePromptStreamLossStillCleansOwnedStaging(t *testing.T) {
	runtime, compositor, now := newCleanupScenario(t)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 1}, now)
	if _, err := runtime.Reconcile(compositor.root, now); err != nil {
		t.Fatal(err)
	}
	// This fake emits focus even when moving an unfocused window. Deliver
	// the actual move/tick effects, without inventing that focus transition.
	for _, event := range compositor.events {
		if event.Change != "focus" {
			runtime.HandleEvent(event, now)
		}
	}
	compositor.events = nil
	staging := namedWorkspaceNode(compositor.root, sessionstate.RestoreStagingWorkspace)
	if staging == nil || len(staging.Nodes) == 0 {
		t.Fatal("fixture did not own a staged window")
	}
	workspace := namedWorkspaceNode(compositor.root, "98")
	promptAppID := "org.example.Authentication"
	prompt := &Node{ID: 43, Type: "con", AppID: &promptAppID, Focused: true}
	workspace.FloatingNodes = []*Node{prompt}
	workspace.Focus = []int64{prompt.ID, 42}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: prompt}, now)
	if _, err := runtime.Reconcile(compositor.root, now); err != nil {
		t.Fatal(err)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: prompt}, now)
	compositor.drain(runtime, now)
	if !runtime.restoreInterruptionPaused {
		t.Fatal("fixture did not pause staged reconstruction")
	}
	want := runtime.persisted
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "disconnected", StreamEpoch: 1}, now)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 2}, now)
	workspace.FloatingNodes = nil
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: prompt}, now)
	finishCleanupScenario(t, runtime, compositor, now.Add(time.Second))
	if err := runtime.Flush(now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var stored sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(runtime.root).LoadInto(&stored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, want) {
		t.Fatal("owned cleanup released uncertain replacement capture")
	}
}

func TestSessionRuntimePromptAllowsMappedContextToResume(t *testing.T) {
	runtime, compositor, prompt, now := newPausedPromptScenario(t)
	workspace := compositor.root.Nodes[0].Nodes[0]
	second := workspace.Nodes[1]
	second.Marks = nil
	second.Focused, prompt.Focused = true, false
	workspace.Focus = []int64{second.ID, prompt.ID, workspace.Nodes[0].ID}
	before := len(compositor.commands)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "new", Container: second}, now)
	if _, err := runtime.Reconcile(compositor.root, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: second}, now)
	if runtime.restoreCancelled || runtime.restoreInterruptionPaused || len(compositor.commands) == before {
		t.Fatal("an unfocused foreign window kept blocking the mapped saved context's restore")
	}
}

func TestSessionRuntimePromptPauseHonorsIndependentUserIntent(t *testing.T) {
	for _, action := range []string{"binding", "focus", "workspace", "move", "repeated-prompt-focus", "stale-close"} {
		t.Run(action, func(t *testing.T) {
			runtime, compositor, prompt, now := newPausedPromptScenario(t)
			event := swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: compositor.root.Nodes[0].Nodes[0].Nodes[0]}
			switch action {
			case "binding":
				event = swayipc.Event{Type: swayipc.EventBinding}
			case "workspace":
				event = swayipc.Event{Type: swayipc.EventWorkspace, Change: "focus", Old: &Node{ID: 3}, Current: &Node{ID: 4}}
			case "move":
				event.Change = "move"
			case "repeated-prompt-focus":
				event.Container = prompt
			case "stale-close":
				event.Change, event.Container, event.StreamEpoch = "close", prompt, 99
			}
			before := len(compositor.commands)
			runtime.HandleEvent(event, now)
			if !runtime.restoreCancelled {
				t.Fatal("prompt focus allowance hid independent user intent or a stale close")
			}
			if _, err := runtime.Reconcile(compositor.root, now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if len(compositor.commands) != before || runtime.restoreProgress != nil || runtime.lateRestorePending {
				t.Fatal("the user's cancellation was followed by new reconstruction effects")
			}
		})
	}
}
