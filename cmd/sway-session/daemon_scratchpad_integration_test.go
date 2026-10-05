package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// SWAY_SESSION_HEADLESS_INTEGRATION=1 GOTOOLCHAIN=go1.26.5 GOMAXPROCS=4 GOFLAGS=-p=1 go test ./cmd/sway-session -run '^TestSessionRuntimeScratchpadHeadless$' -count=1 -timeout=3m -v
// Sequential subtests stop and reap the capture compositor before starting
// the restore compositor. Only the owner-only database survives that boundary.
func TestSessionRuntimeScratchpadHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway/Alacritty scratchpad integration")
	}
	for _, name := range []string{"sway", "alacritty", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("headless integration requires %s: %v", name, err)
		}
	}
	for _, protocol := range []sessionstate.WindowProtocol{sessionstate.WindowWayland, sessionstate.WindowXWayland} {
		t.Run(string(protocol), func(t *testing.T) {
			if protocol == sessionstate.WindowXWayland {
				for _, name := range []string{"Xwayland", "env", "printenv"} {
					if _, err := exec.LookPath(name); err != nil {
						t.Skipf("private XWayland integration requires %s: %v", name, err)
					}
				}
			}
			state := filepath.Join(t.TempDir(), "sway-session")
			registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{}}
			want := []sessionstate.ScratchpadPlacement{
				{ContextID: "11111111-1111-4111-8111-111111111111"},
				{ContextID: "22222222-2222-4222-8222-222222222222", Visible: true, Workspace: "98"},
				{ContextID: "33333333-3333-4333-8333-333333333333", Visible: true, Workspace: "99"},
			}
			for index, placement := range want {
				identity := scratchpadHeadlessIdentity(protocol, fmt.Sprintf("org.example.LAB92.App%d", index))
				registry.Contexts = append(registry.Contexts, sessionstate.Context{
					ID: placement.ContextID, Label: fmt.Sprintf("LAB-92 private application %d", index),
					Provider: "desktop", State: sessionstate.ContextActive,
					Launcher: sessionstate.Launcher{
						Kind: sessionstate.LauncherDesktop, DesktopID: fmt.Sprintf("lab92-%d.desktop", index),
						DesktopOrigin: sessionstate.DesktopEntrySystem,
						DesktopPath:   filepath.Join(state, fmt.Sprintf("lab92-%d.desktop", index)),
					},
					App: &sessionstate.Application{Identity: identity, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow},
				})
			}
			if err := sessionstate.RegistryStoreFor(state).Save(registry); err != nil {
				t.Fatal(err)
			}
			var captureRoot, captureSocket, captureCompositor string
			var persisted sessionstate.LayoutSnapshot
			if !t.Run("capture", func(t *testing.T) {
				h := newScratchpadHeadless(t, protocol)
				h.state = state
				captureRoot, captureSocket = h.root, h.socket
				var err error
				captureCompositor, err = compositorIdentity(h.socket)
				if err != nil {
					t.Fatal(err)
				}
				windows := make(map[sessionstate.ContextID]int64)
				for index, item := range registry.Contexts {
					h.command("workspace 98")
					id := scratchpadHeadlessWindow(h, item.App.Identity)
					windows[item.ID] = id
					mark, err := item.ID.Mark()
					if err != nil {
						t.Fatal(err)
					}
					h.command(fmt.Sprintf("[con_id=%d] mark --add %s", id, quoteSwayString(mark)))
					h.command(fmt.Sprintf("[con_id=%d] move scratchpad", id))
					if want[index].Visible {
						h.command("workspace " + want[index].Workspace)
						h.command(fmt.Sprintf("[con_id=%d] scratchpad show", id))
					}
				}
				h.command("workspace 99")
				fixture := scratchpadHeadlessWindow(h, scratchpadHeadlessIdentity(protocol, "org.example.LAB92.Focused"))
				if focusedContainerID(h.tree()) != fixture {
					t.Fatal("capture fixture did not acquire private workspace 99 focus")
				}
				requester := &restoreCleanupRealRequester{Client: h.client}
				launcher := &applicationLaunchHooks{}
				now := time.Now()
				stream := &swayipc.EventStreamState{}
				runtime := scratchpadHeadlessRuntime(h, requester, launcher, captureCompositor, &now, stream)
				h.subscribe(runtime, stream)
				t.Cleanup(func() {
					if err := runtime.Shutdown(); err != nil {
						t.Error(err)
					}
				})
				for range 3 {
					if _, err := runtime.Reconcile(h.tree(), now); err != nil {
						t.Fatal(err)
					}
					h.drain(runtime)
					now = now.Add(sessionStartupSettleDelay)
					if err := runtime.Flush(now); err != nil {
						t.Fatal(err)
					}
				}
				assertScratchpadHeadlessCapture(h, registry, windows, want)
				if err := sessionstate.LayoutStoreFor(state).LoadInto(&persisted); err != nil {
					t.Fatal(err)
				}
				assertScratchpadHeadlessSnapshot(t, persisted, want)
				if len(requester.commands) != 0 || len(launcher.starts) != 0 {
					t.Fatalf("capture mutated adopted scratchpads: commands=%q starts=%v", requester.commands, launcher.starts)
				}
			}) {
				return
			}
			// Cleanup is part of t.Run's return: the first compositor and all
			// its terminal process groups are gone, not merely disconnected.
			if _, err := os.Lstat(captureRoot); !os.IsNotExist(err) {
				t.Fatalf("capture compositor root survived cleanup: %v", err)
			}
			t.Run("restore_after_compositor_restart", func(t *testing.T) {
				h := newScratchpadHeadless(t, protocol)
				h.state = state
				compositor, err := compositorIdentity(h.socket)
				if err != nil {
					t.Fatal(err)
				}
				if h.socket == captureSocket || compositor == captureCompositor {
					t.Fatal("restore reused the capture compositor")
				}
				// A different, unregistered scratchpad comes first in Sway's
				// scratchpad order. An unqualified show must never reveal it.
				hidden := scratchpadHeadlessWindow(h, scratchpadHeadlessIdentity(protocol, "org.example.LAB92.UnrelatedHidden"))
				h.command(fmt.Sprintf("[con_id=%d] move scratchpad", hidden))
				h.command("workspace 99")
				fixture := scratchpadHeadlessWindow(h, scratchpadHeadlessIdentity(protocol, "org.example.LAB92.Focused"))
				h.command(fmt.Sprintf(`[con_id=%d] mark --add "lab92-unrelated"`, fixture))
				// Mapping precedes Wayland's decoration/configure acknowledgement.
				// Observe the configured fixture before comparing its geometry.
				h.until("configured unrelated fixture", func() bool {
					node := findContainerByID(h.tree(), fixture)
					return node != nil && node.DecoRect.Height > 0
				})
				initial := h.tree()
				fixtureBefore, hiddenBefore := *findContainerByID(initial, fixture), *findContainerByID(initial, hidden)
				if focusedContainerID(initial) != fixture {
					t.Fatal("restore fixture is not focused on private workspace 99")
				}
				requester := &restoreCleanupRealRequester{Client: h.client}
				launcher := &scratchpadHeadlessLauncher{h: h, starts: make(map[sessionstate.ContextID]int), windows: make(map[sessionstate.ContextID]int64)}
				now := time.Now()
				stream := &swayipc.EventStreamState{}
				runtime := scratchpadHeadlessRuntime(h, requester, launcher, compositor, &now, stream)
				if !reflect.DeepEqual(runtime.desired, persisted) {
					t.Fatalf("restart did not load persisted scratchpad layout: %+v", runtime.desired)
				}
				if runtime.startupComplete {
					t.Error("scratchpad-only persisted layout did not arm startup restoration")
				}
				h.subscribe(runtime, stream)
				t.Cleanup(func() {
					if err := runtime.Shutdown(); err != nil {
						t.Error(err)
					}
				})
				moves, shows, marks := make(map[sessionstate.ContextID]int), make(map[sessionstate.ContextID]int), make(map[sessionstate.ContextID]int)
				showFocus, showMoves, workspaceFocus := 0, 0, 0
				settled := false
				for pass := range 64 {
					before := h.tree()
					commandsBefore := len(requester.commands)
					refresh, err := runtime.Reconcile(before, now)
					if err != nil {
						t.Fatalf("restore pass %d: %v; commands=%q", pass, err, requester.commands)
					}
					observed := h.tree()
					// Newly mapped saved scratchpad windows must be identifiable
					// from a fresh tree without needing their queued new events.
					if _, err := runtime.observeRestoreWindows(observed, registry); err != nil {
						t.Fatal(err)
					}
					focusBefore := fmt.Sprintf("%+v", runtime.expectedFocus)
					events := h.drain(runtime)
					if runtime.restoreCancelled {
						t.Fatalf("daemon's real map/move/show/focus events cancelled restore at pass %d: %v; expected focus=%s; commands=%q", pass, scratchpadHeadlessEventSummary(events), focusBefore, requester.commands)
					}
					for _, event := range events {
						if event.Container != nil && event.Container.ID == hidden && event.Type == swayipc.EventWindow && (event.Change == "move" || event.Change == "focus") {
							t.Fatalf("unrelated hidden scratchpad was shown: %+v", event)
						}
						if event.Container != nil && event.Container.ID == fixture && event.Type == swayipc.EventWindow && (event.Change == "move" || event.Change == "floating") {
							t.Fatalf("unrelated focused fixture changed placement: %+v", event)
						}
						if event.Type == swayipc.EventWorkspace && event.Change == "focus" {
							workspaceFocus++
						}
					}
					for _, command := range requester.commands[commandsBefore:] {
						for index, item := range registry.Contexts {
							id := launcher.windows[item.ID]
							criterion := fmt.Sprintf("[con_id=%d]", id)
							if id == 0 || !strings.Contains(command, criterion) {
								continue
							}
							groups, err := sessionstate.ObserveApplicationGroups(before, registry)
							if err != nil {
								t.Fatal(err)
							}
							group := groups[item.ID]
							show := strings.Contains(command, criterion+" scratchpad show")
							if strings.Contains(command, criterion+" move scratchpad") && !show {
								if group.Anchor == nil || group.AnchorMarked || group.Anchor.Scratchpad || moves[item.ID] != 0 {
									t.Fatalf("move did not use a fresh unmarked normal anchor: %+v; %q", group, command)
								}
								moves[item.ID]++
							}
							if show {
								if !want[index].Visible || group.Anchor == nil || group.AnchorMarked || !group.Anchor.Scratchpad || group.Anchor.Workspace != "__i3_scratch" || moves[item.ID] != 1 || shows[item.ID] != 0 {
									t.Fatalf("show did not follow a fresh hidden anchor: %+v; %q", group, command)
								}
								shows[item.ID]++
								for _, event := range events {
									if event.Type == swayipc.EventWindow && event.Container != nil && event.Container.ID == id {
										if event.Change == "focus" {
											showFocus++
										} else if event.Change == "move" {
											showMoves++
										}
									}
								}
							}
							mark, _ := item.ID.Mark()
							if strings.Contains(command, criterion+" mark --add "+quoteSwayString(mark)) {
								workspace := "__i3_scratch"
								if want[index].Visible {
									workspace = want[index].Workspace
								}
								if group.Anchor == nil || group.AnchorMarked || !group.Anchor.Scratchpad || group.Anchor.Workspace != workspace || moves[item.ID] != 1 || want[index].Visible && shows[item.ID] != 1 || marks[item.ID] != 0 {
									t.Fatalf("mark preceded a fresh restored membership/visibility observation: %+v; %q", group, command)
								}
								marks[item.ID]++
							}
						}
					}
					now = now.Add(sessionStartupSettleDelay)
					if err := runtime.Flush(now); err != nil {
						t.Fatal(err)
					}
					if !refresh && runtime.startupComplete && runtime.restoreProgress == nil && !runtime.restoreCleanupPending && len(marks) == len(want) {
						settled = true
						break
					}
				}
				if !settled || showFocus != 2 || showMoves != 2 || workspaceFocus < 2 {
					t.Fatalf("real restore did not converge: settled=%v show focus=%d move=%d workspace focus=%d; commands=%q", settled, showFocus, showMoves, workspaceFocus, requester.commands)
				}
				assertScratchpadHeadlessCapture(h, registry, launcher.windows, want)
				assertScratchpadHeadlessQuiescent(h, runtime, requester, &now)
				for _, item := range registry.Contexts {
					if launcher.starts[item.ID] != 1 || moves[item.ID] != 1 || marks[item.ID] != 1 {
						t.Errorf("context %s effects: starts=%d moves=%d marks=%d; want one each", item.ID, launcher.starts[item.ID], moves[item.ID], marks[item.ID])
					}
				}
				final := h.tree()
				if focusedContainerID(final) != fixture || restoreCleanupWorkspace(final, fixture) != "99" || !reflect.DeepEqual(findContainerByID(final, fixture), &fixtureBefore) {
					t.Errorf("unrelated focused fixture changed: before=%+v after=%+v", fixtureBefore, findContainerByID(final, fixture))
				}
				if restoreCleanupWorkspace(final, hidden) != "__i3_scratch" || !reflect.DeepEqual(findContainerByID(final, hidden), &hiddenBefore) {
					t.Errorf("unrelated hidden scratchpad changed: before=%+v after=%+v", hiddenBefore, findContainerByID(final, hidden))
				}
				var saved sessionstate.LayoutSnapshot
				if err := sessionstate.LayoutStoreFor(state).LoadInto(&saved); err != nil {
					t.Fatal(err)
				}
				assertScratchpadHeadlessSnapshot(t, saved, want)
				assertRestoreFocusNoTemporaryState(t, final)
				t.Logf("two private compositors; 3 launches, 3 moves, 2 shows, 3 marks; real show focus=%d move=%d workspace focus=%d; 4 reconciles without effects", showFocus, showMoves, workspaceFocus)
			})
			for index, workspace := range []string{"98", "99"} {
				t.Run("empty_original_workspace_show_"+workspace, func(t *testing.T) {
					scratchpadHeadlessEmptyFocus(t, protocol, registry.Contexts[index+1], want[index+1])
				})
			}
			for _, visible := range []bool{false, true} {
				t.Run(fmt.Sprintf("normal_layout_from_scratchpad_visible_%t", visible), func(t *testing.T) {
					scratchpadHeadlessNormalPlacement(t, protocol, registry.Contexts[0], visible)
				})
			}
			for _, nested := range []bool{false, true} {
				t.Run(fmt.Sprintf("hide_on_inactive_workspace_nested_%t", nested), func(t *testing.T) {
					scratchpadHeadlessInactiveHide(t, protocol, registry.Contexts[0], nested)
				})
			}
		})
	}
}

func scratchpadHeadlessInactiveHide(t *testing.T, protocol sessionstate.WindowProtocol, item sessionstate.Context, nested bool) {
	t.Helper()
	h := newScratchpadHeadless(t, protocol)
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{item}}
	if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
		t.Fatal(err)
	}
	desired := sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}, Scratchpad: []sessionstate.ScratchpadPlacement{{ContextID: item.ID}}}
	if err := sessionstate.LayoutStoreFor(h.state).Save(desired); err != nil {
		t.Fatal(err)
	}
	h.command("workspace 98")
	if nested {
		scratchpadHeadlessWindow(h, scratchpadHeadlessIdentity(protocol, "org.example.LAB92.TopSibling"))
	}
	id := scratchpadHeadlessWindow(h, item.App.Identity)
	if nested {
		h.command(fmt.Sprintf("[con_id=%d] splitv", id))
		scratchpadHeadlessWindow(h, scratchpadHeadlessIdentity(protocol, "org.example.LAB92.NestedSibling"))
		if len(containerPath(h.tree(), id)) < 5 {
			t.Fatal("private app did not acquire nested split parent")
		}
	}
	h.command("workspace 99")
	fixture := scratchpadHeadlessWindow(h, scratchpadHeadlessIdentity(protocol, "org.example.LAB92.InactiveFocus"))
	compositor, err := compositorIdentity(h.socket)
	if err != nil {
		t.Fatal(err)
	}
	requester := &restoreCleanupRealRequester{Client: h.client}
	now := time.Now()
	stream := &swayipc.EventStreamState{}
	runtime := scratchpadHeadlessRuntime(h, requester, &applicationLaunchHooks{}, compositor, &now, stream)
	h.subscribe(runtime, stream)
	t.Cleanup(func() {
		if err := runtime.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	for range 8 {
		refresh, err := runtime.Reconcile(h.tree(), now)
		if err != nil {
			t.Fatalf("inactive hide: %v commands=%q", err, requester.commands)
		}
		h.drain(runtime)
		if runtime.restoreCancelled || focusedContainerID(h.tree()) != fixture {
			t.Fatalf("inactive hide stole focus or cancelled restore: %q", requester.commands)
		}
		now = now.Add(sessionStartupSettleDelay)
		if err := runtime.Flush(now); err != nil {
			t.Fatal(err)
		}
		if !refresh && runtime.startupComplete {
			break
		}
	}
	assertScratchpadHeadlessCapture(h, registry, map[sessionstate.ContextID]int64{item.ID: id}, desired.Scratchpad)
	assertScratchpadHeadlessQuiescent(h, runtime, requester, &now)
	t.Log("unfocused app on workspace 98 hidden; original focused fixture on workspace 99 preserved")
}

func scratchpadHeadlessNormalPlacement(t *testing.T, protocol sessionstate.WindowProtocol, item sessionstate.Context, visible bool) {
	t.Helper()
	h := newScratchpadHeadless(t, protocol)
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{item}}
	if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
		t.Fatal(err)
	}
	desired := placementOnlySnapshot("98", item.ID)
	if err := sessionstate.LayoutStoreFor(h.state).Save(desired); err != nil {
		t.Fatal(err)
	}
	id := scratchpadHeadlessWindow(h, item.App.Identity)
	h.command(fmt.Sprintf("[con_id=%d] move scratchpad", id))
	if visible {
		h.command("workspace 98")
		h.command(fmt.Sprintf("[con_id=%d] scratchpad show", id))
	}
	h.command("workspace 99")
	fixture := scratchpadHeadlessWindow(h, scratchpadHeadlessIdentity(protocol, "org.example.LAB92.NormalFocus"))
	compositor, err := compositorIdentity(h.socket)
	if err != nil {
		t.Fatal(err)
	}
	requester := &restoreCleanupRealRequester{Client: h.client}
	now := time.Now()
	stream := &swayipc.EventStreamState{}
	runtime := scratchpadHeadlessRuntime(h, requester, &applicationLaunchHooks{}, compositor, &now, stream)
	h.subscribe(runtime, stream)
	t.Cleanup(func() {
		if err := runtime.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	settled := false
	for range 16 {
		refresh, err := runtime.Reconcile(h.tree(), now)
		if err != nil {
			t.Fatalf("normal placement: %v; commands=%q", err, requester.commands)
		}
		h.drain(runtime)
		if runtime.restoreCancelled {
			t.Fatalf("own scratchpad exit cancelled restore: %q", requester.commands)
		}
		now = now.Add(sessionStartupSettleDelay)
		if err := runtime.Flush(now); err != nil {
			t.Fatal(err)
		}
		if !refresh && runtime.startupComplete {
			settled = true
			break
		}
	}
	final := h.tree()
	groups, err := sessionstate.ObserveApplicationGroups(final, registry)
	group := groups[item.ID]
	if err != nil || !settled || group.Anchor == nil || !group.AnchorMarked || group.Anchor.Scratchpad || group.Anchor.Workspace != "98" || focusedContainerID(final) != fixture {
		t.Fatalf("normal placement kept scratchpad membership or lost focus: %+v error=%v settled=%v commands=%q", group, err, settled, requester.commands)
	}
	assertScratchpadHeadlessQuiescent(h, runtime, requester, &now)
	t.Logf("saved normal layout restored from scratchpad; initially shown=%t; final membership=false, workspace=98, original focus retained", visible)
}

func scratchpadHeadlessEmptyFocus(t *testing.T, protocol sessionstate.WindowProtocol, item sessionstate.Context, placement sessionstate.ScratchpadPlacement) {
	t.Helper()
	h := newScratchpadHeadless(t, protocol)
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{item}}
	if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
		t.Fatal(err)
	}
	desired := sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}, Scratchpad: []sessionstate.ScratchpadPlacement{placement}}
	if err := sessionstate.LayoutStoreFor(h.state).Save(desired); err != nil {
		t.Fatal(err)
	}
	id := scratchpadHeadlessWindow(h, item.App.Identity)
	h.command(fmt.Sprintf("[con_id=%d] move scratchpad", id))
	h.command("workspace 99")
	before := h.tree()
	original := restoreCleanupNamedWorkspace(before, "99")
	if original == nil || !original.Focused || len(original.Nodes)+len(original.FloatingNodes) != 0 {
		t.Fatal("fixture original workspace 99 is not empty and focused")
	}
	if placement.Workspace == "98" && restoreCleanupNamedWorkspace(before, "98") != nil {
		t.Fatal("fixture destination 98 should not exist before show")
	}
	compositor, err := compositorIdentity(h.socket)
	if err != nil {
		t.Fatal(err)
	}
	requester := &restoreCleanupRealRequester{Client: h.client}
	launcher := &applicationLaunchHooks{}
	now := time.Now()
	stream := &swayipc.EventStreamState{}
	runtime := scratchpadHeadlessRuntime(h, requester, launcher, compositor, &now, stream)
	h.subscribe(runtime, stream)
	t.Cleanup(func() {
		if err := runtime.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	shows, focusEvents := 0, 0
	settled := false
	for pass := range 16 {
		commandsBefore := len(requester.commands)
		refresh, err := runtime.Reconcile(h.tree(), now)
		if err != nil {
			t.Fatalf("empty original workspace restore pass %d: %v; hidden target=%+v; commands=%q", pass, err, findContainerByID(h.tree(), id), requester.commands)
		}
		events := h.drain(runtime)
		if runtime.restoreCancelled {
			t.Fatalf("show's real focus events cancelled empty-workspace restore: %v; commands=%q", scratchpadHeadlessEventSummary(events), requester.commands)
		}
		for _, command := range requester.commands[commandsBefore:] {
			if strings.Contains(command, fmt.Sprintf("[con_id=%d] scratchpad show", id)) {
				shows++
				for _, event := range events {
					if event.Change == "focus" {
						focusEvents++
					}
				}
			}
		}
		observed := h.tree()
		path := focusedTreePath(observed)
		if len(path) == 0 || path[len(path)-1].Type != "workspace" || path[len(path)-1].Name != "99" {
			t.Fatalf("show did not preserve original workspace focus: %+v; commands=%q", path, requester.commands)
		}
		now = now.Add(sessionStartupSettleDelay)
		if err := runtime.Flush(now); err != nil {
			t.Fatal(err)
		}
		if !refresh && runtime.startupComplete {
			settled = true
			break
		}
	}
	if !settled || shows != 1 || focusEvents == 0 || len(launcher.starts) != 0 {
		t.Fatalf("empty-workspace show did not settle once: settled=%v shows=%d focus events=%d starts=%v", settled, shows, focusEvents, launcher.starts)
	}
	assertScratchpadHeadlessCapture(h, registry, map[sessionstate.ContextID]int64{item.ID: id}, desired.Scratchpad)
	assertScratchpadHeadlessQuiescent(h, runtime, requester, &now)
	var saved sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(h.state).LoadInto(&saved); err != nil {
		t.Fatal(err)
	}
	assertScratchpadHeadlessSnapshot(t, saved, desired.Scratchpad)
	t.Logf("original workspace 99 has no focused leaf; saved destination=%s; real show focus events=%d; original workspace focus preserved", placement.Workspace, focusEvents)
}

func assertScratchpadHeadlessQuiescent(h *restoreCleanupHeadless, runtime *sessionRuntime, requester *restoreCleanupRealRequester, now *time.Time) {
	h.t.Helper()
	commandsBefore := len(requester.commands)
	for range 4 {
		refresh, err := runtime.Reconcile(h.tree(), *now)
		if err != nil || refresh {
			h.t.Fatalf("settled reconcile requested another effect: refresh=%v error=%v", refresh, err)
		}
		h.drain(runtime)
		*now = now.Add(sessionStartupSettleDelay)
		if err := runtime.Flush(*now); err != nil {
			h.t.Fatal(err)
		}
	}
	if len(requester.commands) != commandsBefore || runtime.restoreCancelled || len(runtime.expectedMoves) != 0 || len(runtime.expectedFocus) != 0 {
		h.t.Fatalf("settled restore replayed effects or left attribution: commands=%q moves=%v focus=%+v", requester.commands[commandsBefore:], runtime.expectedMoves, runtime.expectedFocus)
	}
}

func scratchpadHeadlessEventSummary(events []swayipc.Event) []string {
	var result []string
	for _, event := range events {
		if event.Container != nil {
			result = append(result, fmt.Sprintf("%s::%s(%d,%s)", event.Type, event.Change, event.Container.ID, event.Container.Type))
		} else if event.Current != nil {
			result = append(result, fmt.Sprintf("%s::%s(%d,%s)", event.Type, event.Change, event.Current.ID, event.Current.Name))
		} else {
			result = append(result, fmt.Sprintf("%s::%s(%s)", event.Type, event.Change, event.Payload))
		}
	}
	return result
}

func newScratchpadHeadless(t *testing.T, protocol sessionstate.WindowProtocol) *restoreCleanupHeadless {
	t.Helper()
	h := newRestoreCleanupHeadless(t, protocol == sessionstate.WindowXWayland)
	if protocol == sessionstate.WindowXWayland {
		// Sway sets DISPLAY for its own exec children. The compositor's
		// environment allowlist excludes the login session's DISPLAY.
		path := filepath.Join(h.root, "private-display")
		h.command("exec /usr/bin/printenv DISPLAY > " + quoteSwayString(path))
		var display string
		h.until("private compositor XWayland DISPLAY", func() bool {
			contents, err := os.ReadFile(path)
			display = strings.TrimSpace(string(contents))
			return err == nil && display != ""
		})
		if !strings.HasPrefix(display, ":") {
			t.Fatalf("private compositor returned a nonlocal DISPLAY %q", display)
		}
		if _, err := strconv.ParseUint(strings.TrimPrefix(display, ":"), 10, 32); err != nil {
			t.Fatalf("invalid private XWayland DISPLAY %q: %v", display, err)
		}
		h.env = append(h.env, "DISPLAY="+display)
	}
	return h
}

func scratchpadHeadlessIdentity(protocol sessionstate.WindowProtocol, name string) sessionstate.ApplicationIdentity {
	if protocol == sessionstate.WindowXWayland {
		return sessionstate.ApplicationIdentity{Protocol: protocol, X11Class: name, X11Instance: name + ".instance"}
	}
	return sessionstate.ApplicationIdentity{Protocol: protocol, WaylandAppID: name}
}

func scratchpadHeadlessWindow(h *restoreCleanupHeadless, identity sessionstate.ApplicationIdentity) int64 {
	h.t.Helper()
	config := filepath.Join(h.root, "config", "lab92-alacritty.toml")
	if err := os.WriteFile(config, []byte("[window]\ndynamic_title = false\n"), 0600); err != nil {
		h.t.Fatal(err)
	}
	class := identity.WaylandAppID
	if identity.Protocol == sessionstate.WindowXWayland {
		class = identity.X11Class + "," + identity.X11Instance
	}
	args := []string{"--config-file", config, "--class", class, "--title", "LAB-92 private scratchpad test", "-e", "/usr/bin/sleep", "120"}
	var command *exec.Cmd
	if identity.Protocol == sessionstate.WindowXWayland {
		alacritty, err := exec.LookPath("alacritty")
		if err != nil {
			h.t.Fatal(err)
		}
		command = h.start("env", append([]string{"-u", "WAYLAND_DISPLAY", "-u", "WINIT_UNIX_BACKEND", alacritty}, args...)...)
	} else {
		command = h.start("alacritty", args...)
	}
	var id int64
	h.until("private "+string(identity.Protocol)+" application", func() bool {
		var visit func(*Node)
		visit = func(node *Node) {
			matches := identity.Protocol == sessionstate.WindowWayland && node.AppID != nil && *node.AppID == identity.WaylandAppID && node.Window == nil ||
				identity.Protocol == sessionstate.WindowXWayland && node.AppID == nil && node.Window != nil && node.WindowProperties.Class == identity.X11Class && node.WindowProperties.Instance == identity.X11Instance
			if matches && node.PID == command.Process.Pid {
				if id != 0 && id != node.ID {
					h.t.Fatal("private application mapped multiple matching windows")
				}
				id = node.ID
			}
			for _, children := range [][]*Node{node.Nodes, node.FloatingNodes} {
				for _, child := range children {
					visit(child)
				}
			}
		}
		visit(h.tree())
		return id != 0
	})
	return id
}

func scratchpadHeadlessRuntime(h *restoreCleanupHeadless, requester swayRequester, launcher applicationContextLauncher, compositor string, now *time.Time, stream *swayipc.EventStreamState) *sessionRuntime {
	h.t.Helper()
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Context: h.ctx, Root: h.state, EventStreamState: stream, CompositorID: compositor,
		StartedAt: now.Add(-2 * time.Second), Now: func() time.Time { return *now }, ApplicationLauncher: launcher,
		ApplicationRestore: sessionstate.ApplicationRestoreOptions{
			AdoptionGrace: time.Second, CloseGrace: 2 * time.Second, LaunchTimeout: time.Minute, MaxConcurrent: 3,
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return runtime
}

func assertScratchpadHeadlessSnapshot(t *testing.T, snapshot sessionstate.LayoutSnapshot, want []sessionstate.ScratchpadPlacement) {
	t.Helper()
	if snapshot.Version != 2 || len(snapshot.Workspaces) != 0 || !slices.Equal(snapshot.Scratchpad, want) {
		t.Fatalf("captured layout lost scratchpad membership/visibility or duplicated workspace leaves: %+v; want %+v", snapshot, want)
	}
}

func assertScratchpadHeadlessCapture(h *restoreCleanupHeadless, registry sessionstate.Registry, windows map[sessionstate.ContextID]int64, want []sessionstate.ScratchpadPlacement) {
	h.t.Helper()
	tree := h.tree()
	groups, err := sessionstate.ObserveApplicationGroups(tree, registry)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, placement := range want {
		group := groups[placement.ContextID]
		workspace := "__i3_scratch"
		if placement.Visible {
			workspace = placement.Workspace
		}
		if len(group.Windows) != 1 || group.Ambiguous || !group.AnchorMarked || group.Anchor == nil || !group.Anchor.Scratchpad || group.Anchor.ContainerID != windows[placement.ContextID] || group.Anchor.Workspace != workspace {
			h.t.Fatalf("real scratchpad group %s = %+v, want one marked anchor on %s", placement.ContextID, group, workspace)
		}
	}
	snapshot, err := sessionstate.CaptureLayout(tree, registry)
	if err != nil {
		h.t.Fatal(err)
	}
	assertScratchpadHeadlessSnapshot(h.t, snapshot, want)
}

type scratchpadHeadlessLauncher struct {
	h       *restoreCleanupHeadless
	starts  map[sessionstate.ContextID]int
	windows map[sessionstate.ContextID]int64
}

func (launcher *scratchpadHeadlessLauncher) Prepare(ctx context.Context, item sessionstate.Context) (preparedApplicationLaunch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &scratchpadHeadlessPrepared{launcher: launcher, item: item}, nil
}

type scratchpadHeadlessPrepared struct {
	launcher *scratchpadHeadlessLauncher
	item     sessionstate.Context
}

func (prepared *scratchpadHeadlessPrepared) Start() error {
	launcher, item := prepared.launcher, prepared.item
	launcher.starts[item.ID]++
	if launcher.starts[item.ID] != 1 {
		return fmt.Errorf("duplicate private application start for %s", item.ID)
	}
	var state sessionstate.ApplicationSessionState
	if err := sessionstate.ApplicationSessionStoreFor(launcher.h.state).LoadInto(&state); err != nil {
		return err
	}
	if !slices.ContainsFunc(state.Attempts, func(attempt sessionstate.ApplicationLaunchAttempt) bool { return attempt.ContextID == item.ID }) {
		return fmt.Errorf("private application start has no durable launch intent for %s", item.ID)
	}
	launcher.windows[item.ID] = scratchpadHeadlessWindow(launcher.h, item.App.Identity)
	return nil
}
