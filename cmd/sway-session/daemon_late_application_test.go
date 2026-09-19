package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func delayedApplicationScenario(t *testing.T) (*sessionRuntime, *recordingRequester, sessionstate.Context, *Node, *Node, time.Time) {
	t.Helper()
	return applicationStartupScenario(t, true)
}

func applicationStartupScenario(t *testing.T, settle bool) (*sessionRuntime, *recordingRequester, sessionstate.Context, *Node, *Node, time.Time) {
	t.Helper()
	runtime, requester, _, app, start := testApplicationRuntime(t)
	terminalID := sessionstate.ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	registry := sessionRegistryIDs(terminalID)
	registry.Contexts = append(registry.Contexts, app)
	if err := sessionstate.RegistryStoreFor(runtime.root).Save(registry); err != nil {
		t.Fatal(err)
	}
	desired := exactDaemonSnapshot("98", terminalID, app.ID)
	desired.Workspaces[0].Tiling.Layout = sessionstate.LayoutTabbed
	if err := sessionstate.LayoutStoreFor(runtime.root).Save(desired); err != nil {
		t.Fatal(err)
	}
	var err error
	runtime, err = newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Root: runtime.root, CompositorID: strings.Repeat("f", 64), StartedAt: start,
		ApplicationLauncher: &recordingApplicationLauncher{},
		ApplicationRestore: sessionstate.ApplicationRestoreOptions{
			AdoptionGrace: time.Second, CloseGrace: 2 * time.Second, LaunchTimeout: 30 * time.Second, MaxConcurrent: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	terminal := managedDaemonLeaf(t, 41, terminalID)
	terminal.Marks = nil
	if _, err := runtime.Reconcile(daemonTree("98", terminal), start); err != nil {
		t.Fatal(err)
	}
	terminal = managedDaemonLeaf(t, 41, terminalID)
	observation := time.Second
	if settle {
		observation = sessionStartupSettleDelay
	}
	if _, err := runtime.Reconcile(daemonTree("98", terminal), start.Add(observation)); err != nil {
		t.Fatal(err)
	}
	if runtime.startupComplete != settle {
		t.Fatalf("unexpected startup settled state: %t", runtime.startupComplete)
	}
	appID, sandbox := app.App.Identity.WaylandAppID, app.App.Identity.SandboxAppID
	window := &Node{ID: 42, Type: "con", AppID: &appID, SandboxAppID: &sandbox}
	return runtime, requester, app, terminal, window, start
}

func TestSessionRuntimeDelayedStartupApplicationRearmsLayout(t *testing.T) {
	runtime, requester, app, terminal, window, start := delayedApplicationScenario(t)
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
	t.Fatalf("late desired app did not start saved tabbed reconstruction; commands=%q", requester.commands[before:])
}

func TestSessionRuntimeDelayedApplicationPreservesInterveningIntent(t *testing.T) {
	for _, name := range []string{"binding", "focus", "move", "close", "disconnect", "desired-closed", "archived", "identity-changed", "already-marked", "ambiguous-then-unique"} {
		t.Run(name, func(t *testing.T) {
			runtime, requester, app, terminal, window, start := delayedApplicationScenario(t)
			now := start.Add(11 * time.Second)
			switch name {
			case "binding":
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding}, now)
			case "focus", "move", "close":
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: name, Container: terminal}, now)
			case "disconnect":
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 1}, now)
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "disconnected", StreamEpoch: 1}, now)
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 2}, now)
			case "desired-closed", "archived", "identity-changed":
				_, err := sessionstate.UpdateRegistryContext(t.Context(), runtime.root, func(registry *sessionstate.Registry) error {
					for index := range registry.Contexts {
						context := &registry.Contexts[index]
						if context.ID != app.ID {
							continue
						}
						switch name {
						case "desired-closed":
							context.App.DesiredOpen = false
						case "archived":
							context.State = sessionstate.ContextArchived
						case "identity-changed":
							context.App.Identity.WaylandAppID = "org.example.Changed"
							*window.AppID = context.App.Identity.WaylandAppID
						}
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			case "already-marked":
				mark, _ := app.ID.Mark()
				window.Marks = []string{mark}
			case "ambiguous-then-unique":
				other := *window
				other.ID = 43
				if _, err := runtime.Reconcile(daemonTree("98", terminal, window, &other), now); err != nil {
					t.Fatal(err)
				}
			}
			before := len(requester.commands)
			if _, err := runtime.Reconcile(daemonTree("98", terminal, window), now); err != nil {
				t.Fatal(err)
			}
			mark, _ := app.ID.Mark()
			window.Marks = []string{mark}
			if _, err := runtime.Reconcile(daemonTree("98", terminal, window), now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			for _, command := range requester.commands[before:] {
				if strings.Contains(command, sessionstate.RestoreStagingWorkspace) || strings.Contains(command, "layout ") {
					t.Fatalf("new user/app state caused obsolete structural restore: %q", requester.commands[before:])
				}
			}
			if runtime.lateRestorePending {
				t.Fatal("invalidated startup intent rearmed restoration")
			}
		})
	}
}

func TestSessionRuntimeDelayedApplicationMarkFailureCanRetry(t *testing.T) {
	runtime, requester, app, terminal, window, start := delayedApplicationScenario(t)
	requester.failAll = true
	if _, err := runtime.Reconcile(daemonTree("98", terminal, window), start.Add(11*time.Second)); err == nil {
		t.Fatal("mark rejection was not reported")
	}
	requester.failAll = false
	if _, err := runtime.Reconcile(daemonTree("98", terminal, window), start.Add(12*time.Second)); err != nil {
		t.Fatal(err)
	}
	mark, _ := app.ID.Mark()
	window.Marks = []string{mark}
	before := len(requester.commands)
	if _, err := runtime.Reconcile(daemonTree("98", terminal, window), start.Add(13*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(requester.commands) == before || !strings.Contains(requester.commands[len(requester.commands)-1], sessionstate.RestoreStagingWorkspace) {
		t.Fatal("successful mark retry did not resume the saved layout")
	}
}

func TestSessionRuntimeApplicationReopenDoesNotRepeatStartupRestore(t *testing.T) {
	runtime, requester, app, terminal, window, start := delayedApplicationScenario(t)
	mark, _ := app.ID.Mark()
	window.Marks = []string{mark}
	if _, err := runtime.Reconcile(daemonTree("98", terminal, window), start.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Reconcile(daemonTree("98", terminal), start.Add(12*time.Second)); err != nil {
		t.Fatal(err)
	}
	window.ID, window.Marks = 43, nil
	before := len(requester.commands)
	if _, err := runtime.Reconcile(daemonTree("98", terminal, window), start.Add(13*time.Second)); err != nil {
		t.Fatal(err)
	}
	window.Marks = []string{mark}
	if _, err := runtime.Reconcile(daemonTree("98", terminal, window), start.Add(14*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, command := range requester.commands[before:] {
		if strings.Contains(command, sessionstate.RestoreStagingWorkspace) {
			t.Fatal("reopened app reactivated startup layout")
		}
	}
	if runtime.lateRestorePending {
		t.Fatal("reopened app left a late restore pending")
	}
}

func TestSessionRuntimeDelayedApplicationMixedWorkspacePersistsDegradation(t *testing.T) {
	runtime, requester, app, terminal, window, start := delayedApplicationScenario(t)
	unmanagedID := "org.example.Unmanaged"
	unmanaged := &Node{ID: 43, Type: "con", AppID: &unmanagedID}
	tree := daemonTree("98", terminal, window, unmanaged)
	if _, err := runtime.Reconcile(tree, start.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	mark, _ := app.ID.Mark()
	window.Marks = []string{mark}
	before := len(requester.commands)
	for n := range 3 {
		if _, err := runtime.Reconcile(tree, start.Add(time.Duration(12+n)*time.Second)); err != nil {
			var degradation restoreDegradationError
			if !errors.As(err, &degradation) {
				t.Fatal(err)
			}
		}
	}
	if err := runtime.Flush(start.Add(16 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var saved sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(runtime.root).LoadInto(&saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Workspaces) != 1 || saved.Workspaces[0].RestoreMode != sessionstate.WorkspaceRestorePlacementOnly {
		t.Fatalf("mixed current workspace did not retain safe placement-only capture: %+v", saved)
	}
	for _, command := range requester.commands[before:] {
		if strings.Contains(command, sessionstate.RestoreStagingWorkspace) {
			t.Fatal("mixed workspace was structurally reconstructed")
		}
	}
}

func TestSessionRuntimeDelayedApplicationDoesNotReviveDegradedIntent(t *testing.T) {
	runtime, requester, app, terminal, window, start := delayedApplicationScenario(t)
	unmanagedID := "org.example.Unmanaged"
	unmanaged := &Node{ID: 43, Type: "con", AppID: &unmanagedID}
	if _, err := runtime.Reconcile(daemonTree("98", terminal, unmanaged), start.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	if runtime.desired.Workspaces[0].RestoreMode != sessionstate.WorkspaceRestorePlacementOnly {
		t.Fatal("mixed observation did not degrade desired layout")
	}
	if runtime.persisted.Workspaces[0].RestoreMode != sessionstate.WorkspaceRestoreLayout {
		t.Fatal("test must retain unflushed old exact snapshot")
	}
	// Floating an unmanaged window does not necessarily produce a binding,
	// focus, move, or close event. The old exact snapshot must stay retired.
	tree := daemonTree("98", terminal, window)
	tree.Nodes[0].Nodes[0].FloatingNodes = []*Node{unmanaged}
	before := len(requester.commands)
	if _, err := runtime.Reconcile(tree, start.Add(12*time.Second)); err != nil {
		t.Fatal(err)
	}
	mark, _ := app.ID.Mark()
	window.Marks = []string{mark}
	if _, err := runtime.Reconcile(tree, start.Add(13*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, command := range requester.commands[before:] {
		if strings.Contains(command, sessionstate.RestoreStagingWorkspace) {
			t.Fatalf("obsolete exact snapshot was revived after mixed capture: %q", requester.commands[before:])
		}
	}
	if runtime.lateRestorePending {
		t.Fatal("degraded startup intent rearmed restoration")
	}
}
