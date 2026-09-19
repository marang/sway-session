package main

import (
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
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
