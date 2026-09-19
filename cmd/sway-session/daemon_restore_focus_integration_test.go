package main

import (
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// SWAY_SESSION_HEADLESS_INTEGRATION=1 go test ./cmd/sway-session -run '^TestSessionRuntimeRestoreFocusHeadless$' -count=1 -v
// All commands and events use the cleanup harness's private compositor, XDG
// roots and terminals. HOME and the login compositor are never modified.
func TestSessionRuntimeRestoreFocusHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway/Alacritty integration")
	}
	for _, name := range []string{"sway", "alacritty", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("headless integration requires %s: %v", name, err)
		}
	}
	for _, test := range []struct {
		name              string
		originalWorkspace string
		globalGroup       bool
	}{
		{name: "original_focus_98", originalWorkspace: "98"},
		{name: "original_focus_99", originalWorkspace: "99"},
		{name: "global_fullscreen_group", originalWorkspace: "98", globalGroup: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			originalWorkspace := test.originalWorkspace
			h := newRestoreCleanupHeadless(t)
			ids := []sessionstate.ContextID{
				testManagedContextID,
				"6ba7b810-9dad-11d1-80b4-00c04fd430c8",
				"6ba7b811-9dad-11d1-80b4-00c04fd430c8",
				"6ba7b812-9dad-11d1-80b4-00c04fd430c8",
			}
			registry := sessionRegistryIDs(ids...)
			for index := range registry.Contexts {
				registry.Contexts[index].Launcher.Cwd = h.root
			}
			if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
				t.Fatal(err)
			}
			desired := exactDaemonSnapshot("98", ids[:2]...)
			desired.Workspaces[0].Tiling.Layout = sessionstate.LayoutTabbed
			desired.Workspaces[0].FocusedContext = &ids[1]
			second := exactDaemonSnapshot("99", ids[2:]...).Workspaces[0]
			second.Tiling.Layout = sessionstate.LayoutStacked
			second.FocusedContext = &ids[3]
			if test.globalGroup {
				desired.Workspaces[0].Tiling.Fullscreen = sessionstate.FullscreenGlobal
				// A global fullscreen group prevents focus on the other workspace.
				// Restore its structure without asking for impossible leaf focus.
				second.FocusedContext = nil
			}
			desired.Workspaces = append(desired.Workspaces, second)
			if err := sessionstate.LayoutStoreFor(h.state).Save(desired); err != nil {
				t.Fatal(err)
			}

			owned := make([]int64, len(ids))
			for index, id := range ids {
				if index%2 == 0 {
					h.command(fmt.Sprintf("workspace %d", 98+index/2))
				}
				owned[index] = h.terminal(id)
			}
			originalIndex := 0
			if originalWorkspace == "99" {
				originalIndex = 2
			}
			// Focus the first leaf, which staging removes first. Sway focuses
			// its source sibling, not the moved leaf; these are real events.
			h.command(fmt.Sprintf("[con_id=%d] focus", owned[originalIndex]))
			if got := focusedContainerID(h.tree()); got != owned[originalIndex] {
				t.Fatalf("fixture focus = %d, want %d", got, owned[originalIndex])
			}
			requester := &restoreCleanupRealRequester{Client: h.client}
			streamState := &swayipc.EventStreamState{}
			runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
				Context: h.ctx, Root: h.state, EventStreamState: streamState,
			})
			if err != nil {
				t.Fatal(err)
			}
			h.subscribe(runtime, streamState)
			now := time.Now()
			windowFocus, workspaceFocus, moves := 0, 0, 0
			stagingRefocus, crossWorkspaceRefocus := false, false
			groupTransition := false
			staged := make(map[int64]bool)
			settled := 0
			for pass := range 128 {
				before := h.tree()
				commandsBefore := len(requester.commands)
				if _, err := runtime.Reconcile(before, now); err != nil {
					t.Fatalf("pass %d: %v; commands: %q", pass, err, requester.commands)
				}
				// Observe every mutation afresh and deliver actual Sway events
				// through HandleEvent before the next bounded reconcile pass.
				observed := h.tree()
				for _, id := range owned {
					if restoreCleanupWorkspace(observed, id) == sessionstate.RestoreStagingWorkspace {
						staged[id] = true
					}
				}
				events := h.drain(runtime)
				for _, command := range requester.commands[commandsBefore:] {
					if !strings.HasSuffix(command, " fullscreen enable global") {
						continue
					}
					groupTransition = true
					if restoreCleanupWorkspace(before, focusedContainerID(before)) != "99" {
						t.Fatal("global fullscreen group was not enabled from the other workspace")
					}
					pending := restoreCleanupNamedWorkspace(observed, "99")
					if pending == nil || pending.Layout != "splith" || len(pending.Nodes) != 2 || staged[owned[2]] || staged[owned[3]] {
						t.Fatal("second workspace was not still awaiting reconstruction at group fullscreen transition")
					}
					workspaceEvents, windowEvents := 0, 0
					for _, event := range events {
						if event.Type == swayipc.EventWorkspace && event.Change == "focus" && event.Old != nil && event.Current != nil && event.Old.Name == "99" && event.Current.Name == "98" {
							workspaceEvents++
						}
						if event.Type == swayipc.EventWindow && event.Change == "focus" {
							windowEvents++
						}
					}
					if workspaceEvents != 1 || windowEvents != 0 {
						t.Fatalf("global group focus events: workspace 99->98=%d, window=%d; want 1, 0", workspaceEvents, windowEvents)
					}
					if runtime.startupComplete || runtime.restoreProgress == nil {
						t.Error("workspace-only group fullscreen focus cancelled the remaining restore")
					}
					t.Log("global fullscreen group emitted workspace focus 99->98 and no window focus while workspace 99 was still split")
				}
				movedOriginal := false
				focusedWorkspace := ""
				for _, event := range events {
					switch {
					case event.Type == swayipc.EventWindow && event.Change == "focus":
						windowFocus++
						if event.Container != nil {
							if movedOriginal && event.Container.ID == owned[originalIndex+1] {
								stagingRefocus = true
							}
							if focusedWorkspace != "" && restoreCleanupWorkspace(observed, event.Container.ID) == focusedWorkspace {
								crossWorkspaceRefocus = true
							}
						}
					case event.Type == swayipc.EventWorkspace && event.Change == "focus":
						workspaceFocus++
						if event.Old != nil && event.Current != nil && event.Old.ID != event.Current.ID {
							focusedWorkspace = event.Current.Name
						}
					case event.Type == swayipc.EventWindow && event.Change == "move":
						moves++
						if event.Container != nil && event.Container.ID == owned[originalIndex] && restoreCleanupWorkspace(observed, event.Container.ID) == sessionstate.RestoreStagingWorkspace {
							movedOriginal = true
						}
					}
				}
				now = now.Add(sessionStartupSettleDelay)
				if pass == 0 && test.globalGroup {
					// Capture original focus on 98 first, then exercise a daemon
					// focus action to 99. This leaves 98 hidden when its saved group
					// fullscreen is restored, without making original focus conflict
					// with that global fullscreen state at the end of restoration.
					if runtime.originalFocusID != owned[0] || runtime.startupComplete {
						t.Fatal("original focus was not captured before the daemon focus action")
					}
					if err := runtime.applyRestoreAction(h.tree(), sessionstate.RestoreAction{Kind: sessionstate.RestoreFocus, ContainerID: owned[2]}); err != nil {
						t.Fatal(err)
					}
					h.drain(runtime)
				}
				if runtime.startupComplete && runtime.restoreProgress == nil && !runtime.restoreCleanupPending {
					settled++
					// Several extra passes catch delayed cancellation or rearming.
					if settled == 4 {
						break
					}
				} else {
					settled = 0
				}
			}
			tree := h.tree()
			for index, workspace := range desired.Workspaces {
				node := restoreCleanupNamedWorkspace(tree, workspace.Name)
				// Sway may wrap a tabbed/stacked container in a singleton
				// workspace split. This is the same full layout on screen.
				if node != nil && len(node.Nodes) == 1 && len(node.FloatingNodes) == 0 {
					node = node.Nodes[0]
				}
				if node == nil || node.Layout != string(workspace.Tiling.Layout) || len(node.Nodes) != 2 || len(node.FloatingNodes) != 0 {
					t.Errorf("workspace %s did not converge to full %s layout: %+v; commands: %q", workspace.Name, workspace.Tiling.Layout, node, requester.commands)
					continue
				}
				if test.globalGroup && index == 0 && (node.Type != "con" || node.FullscreenMode != 2) {
					t.Errorf("workspace 98 did not retain global fullscreen on its layout group: %+v", node)
				}
				for childIndex, child := range node.Nodes {
					if want := owned[index*2+childIndex]; child.ID != want || len(child.Nodes) != 0 || len(child.FloatingNodes) != 0 {
						t.Errorf("workspace %s child %d = %+v, want managed leaf %d", workspace.Name, childIndex, child, want)
					}
				}
			}
			if got := focusedContainerID(tree); got != owned[originalIndex] {
				t.Errorf("original focus lost: got %d, want %d on workspace %s", got, owned[originalIndex], originalWorkspace)
			}
			for index, id := range owned {
				node := findContainerByID(tree, id)
				mark, err := ids[index].Mark()
				if err != nil {
					t.Fatal(err)
				}
				if node == nil || !slices.Contains(node.Marks, mark) {
					t.Errorf("managed leaf %d lost persistent identity: %+v", id, node)
				}
			}
			assertRestoreFocusNoTemporaryState(t, tree)
			if settled != 4 || !runtime.originalFocusDone {
				t.Errorf("restore did not settle within 128 passes: progress=%+v startup=%t cleanup=%t", runtime.restoreProgress, runtime.startupComplete, runtime.restoreCleanupPending)
			}
			if len(staged) != len(owned) || windowFocus == 0 || workspaceFocus == 0 || moves == 0 {
				t.Errorf("missing real restore effects: staged=%d/%d window focus=%d workspace focus=%d moves=%d", len(staged), len(owned), windowFocus, workspaceFocus, moves)
			}
			if originalWorkspace == "98" && !test.globalGroup && !stagingRefocus {
				t.Error("did not observe staging move followed by source-sibling focus before the tick barrier")
			}
			if !test.globalGroup && !crossWorkspaceRefocus {
				t.Error("did not observe cross-workspace focus followed by target-window focus before the tick barrier")
			}
			if test.globalGroup && !groupTransition {
				t.Error("did not exercise saved global fullscreen group transition")
			}
			// Verify the durable capture independently of the live tree. The
			// restored original focus differs from the seed snapshot, so loading
			// that untouched seed cannot masquerade as a successful Flush.
			now = now.Add(sessionSnapshotDebounce)
			if err := runtime.Flush(now); err != nil {
				t.Fatalf("flush converged layout: %v", err)
			}
			var saved sessionstate.LayoutSnapshot
			if err := sessionstate.LayoutStoreFor(h.state).LoadInto(&saved); err != nil {
				t.Fatal(err)
			}
			assertRestoreFocusSavedLayout(t, saved, ids, originalWorkspace, ids[originalIndex], test.globalGroup)
			commandsAfterSettle := len(requester.commands)
			for pass := range 4 {
				now = now.Add(sessionStartupSettleDelay)
				if _, err := runtime.Reconcile(h.tree(), now); err != nil {
					t.Fatalf("steady pass %d: %v", pass, err)
				}
				h.drain(runtime)
				now = now.Add(sessionSnapshotDebounce)
				if err := runtime.Flush(now); err != nil {
					t.Fatalf("steady flush %d: %v", pass, err)
				}
				var again sessionstate.LayoutSnapshot
				if err := sessionstate.LayoutStoreFor(h.state).LoadInto(&again); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(again, saved) {
					t.Fatalf("steady pass %d changed persisted layout: before=%+v after=%+v", pass, saved, again)
				}
				// This settled fixture needs no further Sway commands at all;
				// in particular it must never restart structural restoration.
				if len(requester.commands) != commandsAfterSettle {
					t.Fatalf("steady pass %d issued additional commands: %q", pass, requester.commands[commandsAfterSettle:])
				}
				assertRestoreFocusNoTemporaryState(t, h.tree())
			}
			t.Logf("real events: window focus=%d workspace focus=%d moves=%d; staged=%d; original workspace=%s", windowFocus, workspaceFocus, moves, len(staged), originalWorkspace)
		})
	}
}

// Unlike the warm fixtures above, subscribe and reconcile the empty compositor
// before mapping either terminal. Each new event runs the production bounded
// reconciliation loop before the already queued mapping-focus event is read.
func TestSessionRuntimeRestoreColdStartFocusHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway/Alacritty integration")
	}
	for _, name := range []string{"sway", "alacritty", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("headless integration requires %s: %v", name, err)
		}
	}
	for _, test := range []struct {
		name    string
		binding bool
		reverse bool
	}{
		{name: "mapping_focus"},
		{name: "mapping_focus_reverse_creation", reverse: true},
		{name: "binding_before_mapping_focus", binding: true},
		{name: "binding_before_mapping_focus_reverse_creation", binding: true, reverse: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			binding := test.binding
			h := newRestoreCleanupHeadless(t)
			ids := []sessionstate.ContextID{testManagedContextID, "6ba7b810-9dad-11d1-80b4-00c04fd430c8"}
			registry := sessionRegistryIDs(ids...)
			for index := range registry.Contexts {
				registry.Contexts[index].Launcher.Cwd = h.root
			}
			if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
				t.Fatal(err)
			}
			desired := exactDaemonSnapshot("98", ids...)
			desired.Workspaces[0].Tiling.Layout = sessionstate.LayoutTabbed
			if err := sessionstate.LayoutStoreFor(h.state).Save(desired); err != nil {
				t.Fatal(err)
			}
			requester := &restoreCleanupRealRequester{Client: h.client}
			streamState := &swayipc.EventStreamState{}
			runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
				Context: h.ctx, Root: h.state, EventStreamState: streamState,
				IndicatorCatalog: func() (sessionstate.DesktopCatalog, error) { return sessionstate.DesktopCatalog{}, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			h.subscribe(runtime, streamState)
			reconcile := func() {
				reconcilePersistentSession(requester, runtime, func(err error) { t.Fatal(err) })
			}
			reconcile()
			if runtime.startupComplete || runtime.restoreProgress != nil {
				t.Fatal("cold fixture must await terminals before starting reconstruction")
			}
			creationOrder := []int{0, 1}
			if test.reverse {
				slices.Reverse(creationOrder)
			}
			// Keep saved context order fixed while reversing actual window birth.
			// GET_TREE can adopt the second arrival before its queued new event;
			// its earlier mapping focus must not inherit command-effect ordering.
			owned := make([]int64, len(ids))
			for _, index := range creationOrder {
				owned[index] = h.terminal(ids[index])
			}
			newEvents, focusEvents := 0, 0
			var mapped []int64
			bindingCommands := -1
			handle := func(event swayipc.Event) {
				if event.Type == swayipc.EventWindow && event.Change == "new" && event.Container != nil && slices.Contains(owned, event.Container.ID) {
					newEvents++
					mapped = append(mapped, event.Container.ID)
				}
				if event.Type == swayipc.EventWindow && event.Change == "focus" && event.Container != nil && slices.Contains(owned, event.Container.ID) {
					focusEvents++
					if binding && bindingCommands < 0 && newEvents != 0 {
						if runtime.restoreProgress == nil {
							t.Fatal("binding was not interleaved with active restore before queued mapping focus")
						}
						// Synthesize only explicit keyboard intent; mapping, focus,
						// moves and ticks all arrive from the private compositor.
						runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding, Change: "run"}, time.Now())
						bindingCommands = len(requester.commands)
						if runtime.restoreProgress != nil || !runtime.startupComplete {
							t.Fatal("binding did not cancel cold-start reconstruction")
						}
					}
				}
				restoring := runtime.restoreProgress != nil
				runtime.HandleEvent(event, time.Now())
				if !binding && restoring && runtime.restoreProgress == nil && event.Type == swayipc.EventWindow && event.Change == "focus" && event.Container != nil {
					t.Logf("cold restore cancelled by queued focus of container %d after new=%d; commands=%q", event.Container.ID, newEvents, requester.commands)
				}
				if event.AffectsSessionLayout() {
					reconcile()
				}
			}
			now := time.Now()
			settled := 0
			for pass := range 64 {
				drainRestoreFocusDaemonEvents(t, h, handle)
				// Advance only the timer-driven settle observation; the event path
				// above retains the production loop and its four-observation bound.
				now = now.Add(sessionStartupSettleDelay)
				if _, err := runtime.Reconcile(h.tree(), now); err != nil {
					t.Fatalf("cold-start settle pass %d: %v", pass, err)
				}
				if runtime.startupComplete && runtime.restoreProgress == nil && !runtime.lateRestorePending && !runtime.restoreCleanupPending {
					settled++
					if settled == 4 {
						break
					}
				} else {
					settled = 0
				}
			}
			if newEvents != 2 || focusEvents == 0 || settled != 4 {
				t.Fatalf("cold-start event loop incomplete: new=%d focus=%d settled=%d", newEvents, focusEvents, settled)
			}
			if want := []int64{owned[creationOrder[0]], owned[creationOrder[1]]}; !slices.Equal(mapped, want) {
				t.Fatalf("real window birth order = %v, want %v", mapped, want)
			}
			wantLayout := "tabbed"
			if binding {
				wantLayout = "splith"
				if bindingCommands < 0 {
					t.Fatal("explicit binding was never interleaved")
				}
				for _, command := range requester.commands[bindingCommands:] {
					if !strings.HasSuffix(command, `] move container to workspace "98"`) {
						t.Errorf("binding was followed by reconstruction command: %q", command)
					}
				}
			}
			tree := h.tree()
			node := restoreCleanupNamedWorkspace(tree, "98")
			if node != nil && len(node.Nodes) == 1 && len(node.FloatingNodes) == 0 {
				node = node.Nodes[0]
			}
			if node == nil || node.Layout != wantLayout || len(node.Nodes) != 2 || len(node.FloatingNodes) != 0 {
				t.Fatalf("cold startup lost %s layout: %+v; commands: %q", wantLayout, node, requester.commands)
			}
			for index, child := range node.Nodes {
				if !slices.Contains(owned, child.ID) || !binding && child.ID != owned[index] {
					t.Errorf("unexpected cold-start child %d: %+v", index, child)
				}
				idIndex := slices.Index(owned, child.ID)
				if idIndex >= 0 {
					mark, err := ids[idIndex].Mark()
					if err != nil || !slices.Contains(child.Marks, mark) {
						t.Errorf("cold-start leaf %d lost persistent identity: %+v; %v", child.ID, child, err)
					}
				}
			}
			assertRestoreFocusNoTemporaryState(t, tree)
			if err := runtime.Flush(now.Add(sessionSnapshotDebounce)); err != nil {
				t.Fatal(err)
			}
			var saved sessionstate.LayoutSnapshot
			if err := sessionstate.LayoutStoreFor(h.state).LoadInto(&saved); err != nil {
				t.Fatal(err)
			}
			workspace, exists := workspaceByName(saved, "98")
			if !exists || len(saved.Workspaces) != 1 || workspace.Tiling == nil || string(workspace.Tiling.Layout) != wantLayout || len(workspace.Tiling.Children) != 2 || len(workspace.Floating) != 0 {
				t.Fatalf("cold-start Flush did not preserve %s layout: %+v", wantLayout, saved)
			}
			for index, child := range workspace.Tiling.Children {
				idIndex := slices.Index(owned, node.Nodes[index].ID)
				if idIndex < 0 || child.ContextID == nil || *child.ContextID != ids[idIndex] || len(child.Children) != 0 {
					t.Errorf("cold-start saved child %d differs from the live managed leaf: %+v", index, child)
				}
			}
			// Startup had no focused managed leaf. Check the saved workspace's
			// selected descendant, which Sway records even when no leaf carries
			// the active focused flag. A non-nil saved selection also proves Flush
			// wrote a capture rather than merely leaving the unfocused seed intact.
			if len(node.Focus) == 0 {
				t.Fatal("cold-start workspace layout has no selected descendant")
			}
			focusedIndex := slices.Index(owned, node.Focus[0])
			if focusedIndex < 0 || workspace.FocusedContext == nil || *workspace.FocusedContext != ids[focusedIndex] {
				t.Fatalf("cold-start Flush did not capture selected context: focus order=%v, saved=%+v", node.Focus, workspace)
			}
			commandsAfterSettle := len(requester.commands)
			for range 4 {
				reconcile()
				drainRestoreFocusDaemonEvents(t, h, handle)
				now = now.Add(sessionSnapshotDebounce)
				if err := runtime.Flush(now); err != nil {
					t.Fatal(err)
				}
				var again sessionstate.LayoutSnapshot
				if err := sessionstate.LayoutStoreFor(h.state).LoadInto(&again); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(again, saved) || len(requester.commands) != commandsAfterSettle {
					t.Fatalf("cold-start steady state changed: saved=%+v; commands=%q", again, requester.commands[commandsAfterSettle:])
				}
				assertRestoreFocusNoTemporaryState(t, h.tree())
			}
			t.Logf("subscribed before both maps; real new=%d focus=%d; reverse creation=%t binding=%t; live and saved layout=%s", newEvents, focusEvents, test.reverse, binding, wantLayout)
		})
	}
}

// A tick bounds each event-loop turn even when reconciliation generates more
// events. Those later events are consumed on the next bounded turn.
func drainRestoreFocusDaemonEvents(t *testing.T, h *restoreCleanupHeadless, handle func(swayipc.Event)) {
	t.Helper()
	h.tick++
	barrier := fmt.Sprintf("lab142-cold-start-%d", h.tick)
	message, err := h.client.RequestContext(h.ctx, swayipc.SendTick, []byte(barrier))
	if err == nil {
		err = swayipc.CheckSendTickResponse(message)
	}
	if err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for range 512 {
		select {
		case event := <-h.events:
			if event.Type == swayipc.EventShutdown || event.Type == swayipc.EventStream {
				t.Fatalf("cold-start event stream lost continuity: %+v", event)
			}
			handle(event)
			if event.Type == swayipc.EventTick && event.Payload == barrier {
				return
			}
		case <-timer.C:
			t.Fatal("cold-start event loop did not reach its tick barrier")
		}
	}
	t.Fatal("cold-start event loop exceeded 512 events before its tick barrier")
}

func assertRestoreFocusSavedLayout(t *testing.T, saved sessionstate.LayoutSnapshot, ids []sessionstate.ContextID, originalWorkspace string, originalContext sessionstate.ContextID, globalGroup bool) {
	t.Helper()
	if saved.Version != sessionstate.LayoutSchemaVersion || len(saved.Workspaces) != 2 {
		t.Fatalf("saved layout must contain exactly the two restored workspaces: %+v", saved)
	}
	for _, workspace := range saved.Workspaces {
		if workspace.Name == sessionstate.RestoreStagingWorkspace {
			t.Fatal("saved layout contains staging workspace")
		}
	}
	for index, name := range []string{"98", "99"} {
		workspace, exists := workspaceByName(saved, name)
		if !exists || workspace.RestoreMode != sessionstate.WorkspaceRestoreLayout || len(workspace.Floating) != 0 || len(workspace.PlacementContexts) != 0 {
			t.Fatalf("saved workspace %s is not a full tiled layout: %+v", name, workspace)
		}
		node := workspace.Tiling
		// Ignore singleton layout wrappers, as with the independent live-tree
		// assertion, but preserve the parent and order of the managed leaves.
		for node != nil && node.ContextID == nil && len(node.Children) == 1 {
			node = &node.Children[0]
		}
		wantLayout := []sessionstate.LayoutKind{sessionstate.LayoutTabbed, sessionstate.LayoutStacked}[index]
		if node == nil || node.ContextID != nil || node.Layout != wantLayout || len(node.Children) != 2 {
			t.Fatalf("saved workspace %s lost %s layout: %+v", name, wantLayout, node)
		}
		wantFullscreen := sessionstate.FullscreenNone
		if globalGroup && name == "98" {
			wantFullscreen = sessionstate.FullscreenGlobal
		}
		if node.Fullscreen != wantFullscreen {
			t.Fatalf("saved workspace %s group fullscreen = %q, want %q", name, node.Fullscreen, wantFullscreen)
		}
		for childIndex, child := range node.Children {
			want := ids[index*2+childIndex]
			if child.ContextID == nil || *child.ContextID != want || len(child.Children) != 0 || child.Layout != "" || child.Fullscreen != sessionstate.FullscreenNone {
				t.Fatalf("saved workspace %s child %d = %+v, want context %s", name, childIndex, child, want)
			}
		}
		if name == originalWorkspace && (workspace.FocusedContext == nil || *workspace.FocusedContext != originalContext) {
			t.Fatalf("saved workspace %s did not capture restored original focus %s: %+v", name, originalContext, workspace)
		}
	}
}

func assertRestoreFocusNoTemporaryState(t *testing.T, node *Node) {
	t.Helper()
	if node.Type == "workspace" && node.Name == sessionstate.RestoreStagingWorkspace {
		t.Errorf("restore left staging workspace: %+v", node)
	}
	for _, mark := range node.Marks {
		if strings.HasPrefix(mark, "_sway_session_restore_") {
			t.Errorf("container %d retains temporary restore mark %q", node.ID, mark)
		}
	}
	for _, child := range append(slices.Clone(node.Nodes), node.FloatingNodes...) {
		assertRestoreFocusNoTemporaryState(t, child)
	}
}
