package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// Uses only the private compositor and disposable roots supplied by the cleanup
// harness. Opt in with SWAY_SESSION_HEADLESS_INTEGRATION=1.
func TestSessionRuntimeLateApplicationHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway/Alacritty integration")
	}
	for _, name := range []string{"sway", "alacritty", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("headless integration requires %s: %v", name, err)
		}
	}
	for _, test := range []struct {
		name          string
		late          bool
		appFirst      bool
		binding       bool
		stacking      bool
		earlyStacking bool
	}{
		{name: "after_startup_timeout/application_first", late: true, appFirst: true},
		{name: "after_startup_timeout/terminal_first", late: true},
		{name: "before_startup_timeout/application_first", appFirst: true},
		{name: "before_startup_timeout/terminal_first"},
		{name: "binding_during_wait", late: true, appFirst: true, binding: true},
		{name: "ipc_stacking_during_wait", late: true, appFirst: true, stacking: true},
		{name: "ipc_stacking_before_timeout/timely_arrival", appFirst: true, stacking: true, earlyStacking: true},
		{name: "ipc_stacking_before_timeout/late_arrival", late: true, appFirst: true, stacking: true, earlyStacking: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			preserveUserLayout := test.binding || test.stacking
			h := newRestoreCleanupHeadless(t)
			ids := []sessionstate.ContextID{testManagedContextID, "6ba7b810-9dad-11d1-80b4-00c04fd430c8"}
			registry := sessionRegistryIDs(ids[0])
			registry.Contexts[0].Launcher.Cwd = h.root
			// The fake launcher never executes this desktop file. Keeping even
			// its identity under the private root avoids host catalog dependencies.
			desktop := filepath.Join(h.root, "data", "Alacritty.desktop")
			if err := os.WriteFile(desktop, []byte("[Desktop Entry]\nType=Application\nName=LAB-178\nExec=/usr/bin/alacritty\nStartupWMClass=Alacritty\n"), 0600); err != nil {
				t.Fatal(err)
			}
			registry.Contexts = append(registry.Contexts, sessionstate.Context{
				ID: ids[1], Label: "LAB-178 application", Provider: "desktop", State: sessionstate.ContextActive,
				Launcher: sessionstate.Launcher{
					Kind: sessionstate.LauncherDesktop, DesktopID: "Alacritty.desktop",
					DesktopOrigin: sessionstate.DesktopEntrySystem, DesktopPath: desktop,
				},
				App: &sessionstate.Application{
					Identity:    sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "Alacritty"},
					DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow,
				},
			})
			if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
				t.Fatal(err)
			}
			// Application-first order stages the last-mapped (focused) window
			// first, forcing Sway to focus its sibling during the daemon's move.
			desired := exactDaemonSnapshot("98", ids...)
			if test.appFirst {
				slices.Reverse(desired.Workspaces[0].Tiling.Children)
			}
			desired.Workspaces[0].Tiling.Layout = sessionstate.LayoutTabbed
			if err := sessionstate.LayoutStoreFor(h.state).Save(desired); err != nil {
				t.Fatal(err)
			}
			requester := &restoreCleanupRealRequester{Client: h.client}
			launcher := &recordingApplicationLauncher{}
			stream := &swayipc.EventStreamState{}
			start := time.Now()
			now := start
			runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
				Context: h.ctx, Root: h.state, StartedAt: start, EventStreamState: stream,
				CompositorID:        strings.Repeat("a", 64),
				ApplicationLauncher: launcher,
				ApplicationRestore: sessionstate.ApplicationRestoreOptions{
					AdoptionGrace: time.Second, CloseGrace: 2 * time.Second, LaunchTimeout: time.Minute, MaxConcurrent: 2,
				},
				IndicatorCatalog: func() (sessionstate.DesktopCatalog, error) { return sessionstate.DesktopCatalog{}, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			h.subscribe(runtime, stream)
			var applicationWindow int64
			stagedObservations := 0
			stagingWait := false
			// Mirror reconcilePersistentSession's four fresh observations, with
			// one injected clock for event handling and reconciliation alike.
			reconcile := func() {
				for range 4 {
					tree := h.tree()
					if applicationWindow != 0 && restoreCleanupWorkspace(tree, applicationWindow) == sessionstate.RestoreStagingWorkspace {
						stagedObservations++
						if stagedObservations == 2 {
							// Model a slow restore after presence has already been
							// observed in staging. Crossing close grace must not
							// turn the still-live desired application into a close.
							now = now.Add(3 * time.Second)
							stagingWait = true
						}
					}
					refresh, err := runtime.Reconcile(tree, now)
					if err != nil {
						t.Fatalf("reconcile at %s: %v; commands=%q", now.Sub(start), err, requester.commands)
					}
					indicatorRefresh, err := runtime.ReconcileIndicators(tree)
					if err != nil {
						t.Fatal(err)
					}
					if !refresh && !indicatorRefresh {
						return
					}
				}
			}
			newEvents, mappingFocus, ownMoves, ownFocus := 0, 0, 0, 0
			mapped := make(map[int64]bool)
			checkingLayoutCommand := false
			handle := func(event swayipc.Event) {
				if checkingLayoutCommand && (event.Type == swayipc.EventBinding ||
					(event.Type == swayipc.EventWindow || event.Type == swayipc.EventWorkspace) && event.Change == "focus") {
					t.Fatalf("IPC layout command unexpectedly supplied binding/focus intent: %+v", event)
				}
				if event.Type == swayipc.EventWorkspace && event.Change == "focus" && len(runtime.expectedFocus) != 0 {
					ownFocus++
				}
				if event.Type == swayipc.EventWindow && event.Container != nil {
					switch event.Change {
					case "new":
						newEvents++
						mapped[event.Container.ID] = true
					case "focus":
						if mapped[event.Container.ID] {
							mappingFocus++
							delete(mapped, event.Container.ID)
						} else if len(runtime.expectedFocus) != 0 {
							ownFocus++
						}
					case "move":
						if len(runtime.expectedMoves[event.Container.ID]) != 0 {
							ownMoves++
						}
					}
				}
				runtime.HandleEvent(event, now)
				if !preserveUserLayout && runtime.restoreCancelled {
					t.Fatalf("real event cancelled restoration: %+v; commands=%q", event, requester.commands)
				}
				if event.AffectsSessionLayout() {
					reconcile()
				}
			}
			drain := func() { drainRestoreFocusDaemonEvents(t, h, handle) }
			reconcile()
			owned := []int64{h.terminal(ids[0])}
			drain()
			now = start.Add(2 * time.Second)
			reconcile()
			drain()
			if launcher.starts != 1 || len(launcher.contexts) != 1 || launcher.contexts[0].ID != ids[1] {
				t.Fatalf("desired application was not launched exactly once: %+v", launcher)
			}
			stackViaIPC := func() {
				// A real IPC layout edit need not produce a binding or focus event.
				// Observe it while the application is absent, before its later map
				// can try to revive the original saved tabbed structure.
				if focusedContainerID(h.tree()) != owned[0] {
					t.Fatal("terminal must retain focus before IPC layout edit")
				}
				checkingLayoutCommand = true
				h.command("layout stacking")
				drain()
				now = now.Add(sessionObservationDelay)
				reconcile()
				drain()
				checkingLayoutCommand = false
				tree := h.tree()
				workspace := restoreCleanupNamedWorkspace(tree, "98")
				for workspace != nil && workspace.Layout != "stacked" && len(workspace.Nodes) == 1 && len(workspace.FloatingNodes) == 0 {
					workspace = workspace.Nodes[0]
				}
				if workspace == nil || workspace.Layout != "stacked" || len(workspace.Nodes) != 1 || workspace.Nodes[0].ID != owned[0] || focusedContainerID(tree) != owned[0] {
					t.Fatalf("IPC edit did not leave the focused terminal in stacked layout: %+v", workspace)
				}
			}
			if test.earlyStacking {
				if runtime.startupComplete || !now.Before(start.Add(sessionStartupSettleDelay)) {
					t.Fatal("early IPC edit must precede the actual startup deadline")
				}
				stackViaIPC()
			}
			if test.binding {
				// The only synthetic event is explicit keyboard intent while the
				// requested application is still waiting to map.
				handle(swayipc.Event{Type: swayipc.EventBinding, Change: "run"})
				if !runtime.restoreCancelled || runtime.restoreProgress != nil {
					t.Fatal("binding did not cancel reconstruction while waiting for application")
				}
			}
			if test.late {
				now = start.Add(sessionStartupSettleDelay + time.Second)
				reconcile()
				drain()
				if !runtime.startupComplete {
					t.Fatal("startup did not finish before delayed application mapped")
				}
			} else if !test.earlyStacking && runtime.startupComplete {
				t.Fatal("timely control passed startup gate before application mapped")
			}
			if test.stacking && !test.earlyStacking {
				stackViaIPC()
			}
			if test.earlyStacking {
				// Cancellation may complete startup early. Check the actual
				// injected arrival time, not a mutable runtime completion flag.
				beforeDeadline := now.Before(start.Add(sessionStartupSettleDelay))
				if beforeDeadline == test.late {
					t.Fatalf("incorrect application arrival time %s for late=%t", now.Sub(start), test.late)
				}
				t.Logf("real IPC stacking at 2s without binding/focus; application mapping at %s", now.Sub(start))
			}
			applicationWindow = lateApplicationWindow(t, h)
			owned = append(owned, applicationWindow)
			settled := 0
			for range 64 {
				drain()
				now = now.Add(time.Second)
				reconcile()
				if runtime.startupComplete && runtime.restoreProgress == nil && !runtime.lateRestorePending && !runtime.restoreCleanupPending {
					settled++
					if settled == 4 {
						break
					}
				} else {
					settled = 0
				}
			}
			drain()
			wantLayout := "tabbed"
			wantIDs, wantOwned := slices.Clone(ids), slices.Clone(owned)
			if test.binding {
				wantLayout = "splith"
			} else if test.stacking {
				wantLayout = "stacked"
			} else {
				for index, child := range desired.Workspaces[0].Tiling.Children {
					wantIDs[index] = *child.ContextID
					wantOwned[index] = owned[slices.Index(ids, wantIDs[index])]
				}
			}
			tree := h.tree()
			node := restoreCleanupNamedWorkspace(tree, "98")
			for node != nil && len(node.Nodes) == 1 && len(node.FloatingNodes) == 0 {
				node = node.Nodes[0]
			}
			if settled != 4 || node == nil || node.Layout != wantLayout || len(node.Nodes) != 2 || len(node.FloatingNodes) != 0 {
				t.Fatalf("application arrival lost exact %s layout: settled=%d tree=%+v commands=%q", wantLayout, settled, node, requester.commands)
			}
			for index, child := range node.Nodes {
				mark, err := wantIDs[index].Mark()
				if err != nil || child.ID != wantOwned[index] || !slices.Contains(child.Marks, mark) || len(child.Nodes) != 0 || len(child.FloatingNodes) != 0 {
					t.Fatalf("restored child %d is not marked context %s/window %d: %+v; %v", index, wantIDs[index], wantOwned[index], child, err)
				}
			}
			assertRestoreFocusNoTemporaryState(t, tree)
			if newEvents != 2 || mappingFocus != 2 || !preserveUserLayout && (ownMoves == 0 || test.appFirst && ownFocus == 0) {
				t.Fatalf("missing real event coverage: new=%d mapping focus=%d own moves=%d own focus=%d", newEvents, mappingFocus, ownMoves, ownFocus)
			}
			if preserveUserLayout {
				if ownMoves != 0 || ownFocus != 0 || stagedObservations != 0 {
					t.Fatal("user layout authority was followed by structural restore effects")
				}
				for _, command := range requester.commands {
					if strings.Contains(command, sessionstate.RestoreStagingWorkspace) {
						t.Fatalf("user layout authority was followed by staging: %q", command)
					}
				}
			}
			if !preserveUserLayout && !stagingWait {
				t.Fatal("did not hold the real application in staging beyond close grace")
			}
			now = now.Add(sessionSnapshotDebounce)
			if err := runtime.Flush(now); err != nil {
				t.Fatal(err)
			}
			var saved sessionstate.LayoutSnapshot
			if err := sessionstate.LayoutStoreFor(h.state).LoadInto(&saved); err != nil {
				t.Fatal(err)
			}
			workspace, exists := workspaceByName(saved, "98")
			if !exists || len(saved.Workspaces) != 1 || workspace.RestoreMode != sessionstate.WorkspaceRestoreLayout || len(workspace.Floating) != 0 || len(workspace.PlacementContexts) != 0 {
				t.Fatalf("Flush did not save exact workspace: %+v", saved)
			}
			layout := workspace.Tiling
			for layout != nil && layout.ContextID == nil && len(layout.Children) == 1 {
				layout = &layout.Children[0]
			}
			if layout == nil || string(layout.Layout) != wantLayout || len(layout.Children) != 2 {
				t.Fatalf("Flush lost %s layout: %+v", wantLayout, saved)
			}
			for index, child := range layout.Children {
				if child.ContextID == nil || *child.ContextID != wantIDs[index] || len(child.Children) != 0 {
					t.Fatalf("Flush lost child %d: %+v", index, child)
				}
			}
			// The seed has no selected context, so this proves a real capture
			// reached storage instead of passing by reading the untouched seed.
			if len(node.Focus) == 0 || workspace.FocusedContext == nil {
				t.Fatalf("Flush did not capture selected descendant: %+v", workspace)
			}
			selected := slices.Index(owned, node.Focus[0])
			if selected < 0 || *workspace.FocusedContext != ids[selected] {
				t.Fatalf("Flush selection differs from live layout: focus=%v saved=%+v", node.Focus, workspace)
			}
			commands := len(requester.commands)
			for range 4 {
				now = now.Add(sessionStartupSettleDelay)
				reconcile()
				drain()
				if err := runtime.Flush(now.Add(sessionSnapshotDebounce)); err != nil {
					t.Fatal(err)
				}
				var again sessionstate.LayoutSnapshot
				if err := sessionstate.LayoutStoreFor(h.state).LoadInto(&again); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(saved, again) || len(requester.commands) != commands {
					t.Fatalf("repeated observation changed layout or issued commands: saved=%+v commands=%q", again, requester.commands[commands:])
				}
				assertRestoreFocusNoTemporaryState(t, h.tree())
			}
			var finalRegistry sessionstate.Registry
			if err := sessionstate.RegistryStoreFor(h.state).LoadInto(&finalRegistry); err != nil {
				t.Fatal(err)
			}
			appIndex := slices.IndexFunc(finalRegistry.Contexts, func(context sessionstate.Context) bool { return context.ID == ids[1] })
			if appIndex < 0 || finalRegistry.Contexts[appIndex].State != sessionstate.ContextActive || finalRegistry.Contexts[appIndex].App == nil || !finalRegistry.Contexts[appIndex].App.DesiredOpen || launcher.starts != 1 {
				t.Fatalf("restore changed application lifecycle: registry=%+v launches=%d", finalRegistry, launcher.starts)
			}
			t.Logf("real new=%d mapping focus=%d own moves=%d own focus=%d; live and captured layout=%s", newEvents, mappingFocus, ownMoves, ownFocus, wantLayout)
		})
	}
}

func lateApplicationWindow(t *testing.T, h *restoreCleanupHeadless) int64 {
	t.Helper()
	config := filepath.Join(h.root, "config", "application.toml")
	if err := os.WriteFile(config, []byte("[window]\ndynamic_title = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Ordinary desktop identity, deliberately not ContextID.AppID(). No shell,
	// browser, login configuration, or real desktop launcher participates.
	command := h.start("alacritty", "--config-file", config, "--class", "Alacritty", "--title", "LAB-178 private application", "-e", "/usr/bin/sleep", "120")
	var id int64
	h.until("ordinary Alacritty application", func() bool {
		var visit func(*Node)
		visit = func(node *Node) {
			if node.AppID != nil && *node.AppID == "Alacritty" && node.PID == command.Process.Pid {
				id = node.ID
			}
			for _, child := range append(slices.Clone(node.Nodes), node.FloatingNodes...) {
				visit(child)
			}
		}
		visit(h.tree())
		return id != 0
	})
	return id
}
