package main

import (
	"reflect"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func TestSessionRuntimePromptResumePreservesEventlessUserLayoutChange(t *testing.T) {
	runtime, compositor, prompt, now := newPausedPromptScenario(t)
	secondID := sessionstate.ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	workspace := compositor.root.Nodes[0].Nodes[0]
	first, second := workspace.Nodes[0], workspace.Nodes[1]
	before := len(compositor.commands)

	// An IPC layout command need not emit an event. The prompt closes before
	// the next periodic tree observation, so the changed layout is first seen
	// in the reconciliation which resumes the interrupted restore.
	workspace.Layout = "splitv"
	workspace.FloatingNodes = nil
	workspace.Focus = []int64{first.ID, second.ID}
	first.Focused = true
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: prompt}, now.Add(time.Second))
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: first}, now.Add(time.Second))
	compositor.drain(runtime, now.Add(time.Second))
	refresh, err := runtime.Reconcile(compositor.root, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if refresh || !runtime.restoreCancelled || len(compositor.commands) != before || workspace.Layout != "splitv" {
		t.Fatalf("prompt resume ignored the user's eventless layout edit and continued obsolete reconstruction: refresh=%t cancelled=%t layout=%q commands=%q", refresh, runtime.restoreCancelled, workspace.Layout, compositor.commands[before:])
	}
	if err := runtime.Flush(now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var stored sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(runtime.root).LoadInto(&stored); err != nil {
		t.Fatal(err)
	}
	want := exactDaemonSnapshot("98", testManagedContextID, secondID)
	want.Workspaces[0].Tiling.Layout = sessionstate.LayoutSplitVertical
	want.Workspaces[0].FocusedContext = &testManagedContextID
	if !reflect.DeepEqual(stored, want) {
		t.Fatalf("prompt resume did not persist the user's replacement layout:\n got: %+v\nwant: %+v", stored, want)
	}
}
