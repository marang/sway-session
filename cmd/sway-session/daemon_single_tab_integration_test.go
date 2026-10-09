package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

const singleTabHeadlessContextID sessionstate.ContextID = "55555555-5555-4555-8555-555555555555"

// Captures a real single-window tab, closes its owned window, and restores a
// newly mapped flat window. No desktop launcher, Herdr, or provider is started.
func TestSessionRuntimeSingleWindowTabHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway/Alacritty integration")
	}
	for _, name := range []string{"sway", "alacritty", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("private integration requires %s: %v", name, err)
		}
	}
	for _, kind := range []string{"desktop", "terminal"} {
		t.Run(kind, func(t *testing.T) {
			h, registry, desired, containerID := newSingleTabHeadlessFixture(t, kind)
			desiredJSON := singleTabLayoutJSON(t, desired)
			requester := &restoreCleanupRealRequester{Client: h.client}
			launcher := &recordingApplicationLauncher{}
			stream := &swayipc.EventStreamState{}
			start := time.Now()
			now := start
			runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
				Context: h.ctx, Root: h.state, StartedAt: start, Now: func() time.Time { return now }, EventStreamState: stream,
				CompositorID: strings.Repeat("a", 64), ApplicationLauncher: launcher,
				ApplicationRestore: sessionstate.ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: 30 * time.Second, MaxConcurrent: 1},
				IndicatorCatalog:   func() (sessionstate.DesktopCatalog, error) { return sessionstate.DesktopCatalog{}, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			h.subscribe(runtime, stream)
			settled := 0
			observations := 0
			for pass := range 24 {
				observations = pass + 1
				refresh, err := runtime.Reconcile(h.tree(), now)
				if err != nil {
					t.Errorf("restore pass %d: %v", pass, err)
				}
				drainRestoreFocusDaemonEvents(t, h, func(event swayipc.Event) { runtime.HandleEvent(event, now) })
				// Fresh observations after acknowledged commands do not each wait
				// through startup's settle delay. Advance that gate only when an
				// idle pass is actually waiting for it, as with a staged window.
				now = now.Add(100 * time.Millisecond)
				if !refresh && !runtime.startupComplete && !runtime.startupDeadline.IsZero() && now.Before(runtime.startupDeadline) {
					now = runtime.startupDeadline
				}
				if runtime.startupComplete && runtime.restoreProgress == nil && !runtime.restoreCleanupPending && !runtime.lateRestorePending {
					settled++
					if settled == 4 {
						break
					}
				} else {
					settled = 0
				}
			}
			if settled != 4 {
				t.Errorf("single-window tab did not settle in 24 passes; commands=%q", requester.commands)
			}
			final := h.tree()
			captured, err := sessionstate.CaptureLayout(final, registry)
			if err != nil {
				t.Fatal(err)
			}
			assertSingleTabCapturedLayout(t, captured)
			if capturedJSON := singleTabLayoutJSON(t, captured); capturedJSON != desiredJSON {
				t.Errorf("restored topology, proportions, or local focus differ: captured=%s desired=%s; commands=%q", capturedJSON, desiredJSON, requester.commands)
			}
			if got := restoreCleanupWorkspace(final, containerID); got != "98" {
				t.Errorf("restored owned window is on %q, want 98", got)
			}
			assertRestoreFocusNoTemporaryState(t, final)
			outcome := restoreReportOutcome(t, runtime, "automatic", singleTabHeadlessContextID)
			if outcome.Status != "completed" || outcome.Reason != "restore_complete" || !outcome.WindowMapped || !outcome.PlacementApplied || !outcome.LayoutApplied {
				t.Errorf("captured tab restore was not completed: %+v", outcome)
			}
			if outcome.Requested != restoreRequestedWork(desired, singleTabHeadlessContextID) {
				t.Error("restore changed the recorded request or layout digest")
			}
			t.Logf("singleton tab restored in %d observations; report convergence at %s", observations, outcome.UpdatedAt.Sub(start))
			if launcher.starts != 0 {
				t.Errorf("already mapped fixture triggered %d desktop launches", launcher.starts)
			}
			splits := make(map[string]struct{})
			for _, command := range requester.commands {
				if strings.Contains(command, "] split ") {
					if _, repeated := splits[command]; repeated {
						t.Errorf("restore repeated a split without creating the singleton group: %q", command)
					}
					splits[command] = struct{}{}
				}
			}
			if err := runtime.Flush(now.Add(sessionSnapshotDebounce)); err != nil {
				t.Fatal(err)
			}
			var saved sessionstate.LayoutSnapshot
			if err := sessionstate.LayoutStoreFor(h.state).LoadIntoContext(t.Context(), &saved); err != nil {
				t.Fatal(err)
			}
			if savedJSON := singleTabLayoutJSON(t, saved); savedJSON != desiredJSON {
				t.Errorf("durable capture lost saved singleton tab: saved=%s desired=%s", savedJSON, desiredJSON)
			}
			commandsAfterRestore := len(requester.commands)
			if deadline := outcome.StartedAt.Add(restoreReportTimeout + time.Second); now.Before(deadline) {
				now = deadline
			}
			for pass := range 4 {
				now = now.Add(time.Second)
				if _, err := runtime.Reconcile(h.tree(), now); err != nil {
					t.Fatalf("steady pass %d: %v", pass, err)
				}
				drainRestoreFocusDaemonEvents(t, h, func(event swayipc.Event) { runtime.HandleEvent(event, now) })
				if err := runtime.Flush(now.Add(sessionSnapshotDebounce)); err != nil {
					t.Fatal(err)
				}
				var again sessionstate.LayoutSnapshot
				if err := sessionstate.LayoutStoreFor(h.state).LoadIntoContext(t.Context(), &again); err != nil {
					t.Fatal(err)
				}
				if againJSON := singleTabLayoutJSON(t, again); againJSON != desiredJSON || len(requester.commands) != commandsAfterRestore {
					t.Errorf("steady pass %d changed captured state or replayed commands: saved=%s commands=%q", pass, againJSON, requester.commands[commandsAfterRestore:])
				}
				record := restoreReportOutcome(t, runtime, "automatic", singleTabHeadlessContextID)
				if record.AttemptID != outcome.AttemptID || record.Status != "completed" || record.Reason != "restore_complete" || !record.WindowMapped || !record.PlacementApplied || !record.LayoutApplied || record.Requested != outcome.Requested {
					t.Errorf("steady pass %d beyond the original deadline changed completed outcome: %+v", pass, record)
				}
			}
		})
	}
}

func newSingleTabHeadlessFixture(t *testing.T, kind string) (*restoreCleanupHeadless, sessionstate.Registry, sessionstate.LayoutSnapshot, int64) {
	t.Helper()
	h := newRestoreCleanupHeadless(t)
	registry := sessionRegistryIDs(singleTabHeadlessContextID)
	registry.Contexts[0].Launcher.Cwd = h.root
	mapWindow := func() int64 { return h.terminal(singleTabHeadlessContextID) }
	if kind == "desktop" {
		identity := "diagnostic.SingleTabRestore"
		desktopPath := filepath.Join(h.root, "data", "SingleTabRestore.desktop")
		if err := os.WriteFile(desktopPath, []byte("[Desktop Entry]\nType=Application\nName=Single tab restore surrogate\nExec=/usr/bin/alacritty\n"), 0600); err != nil {
			t.Fatal(err)
		}
		registry.Contexts[0] = sessionstate.Context{
			ID: singleTabHeadlessContextID, Provider: "desktop", State: sessionstate.ContextActive,
			Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherDesktop, DesktopID: "SingleTabRestore.desktop", DesktopOrigin: sessionstate.DesktopEntrySystem, DesktopPath: desktopPath},
			App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: identity}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow},
		}
		mapWindow = func() int64 {
			_, containerID := startupPromptWindow(t, h, identity)
			return containerID
		}
	} else if kind != "terminal" {
		t.Fatalf("unknown fixture kind %q", kind)
	}
	h.command("workspace 98")
	original := mapWindow()
	mark, err := singleTabHeadlessContextID.Mark()
	if err != nil {
		t.Fatal(err)
	}
	h.command(fmt.Sprintf("[con_id=%d] mark --add %q", original, mark))
	h.command(fmt.Sprintf("[con_id=%d] layout tabbed", original))
	desired, err := sessionstate.CaptureLayout(h.tree(), registry)
	if err != nil {
		t.Fatal(err)
	}
	assertSingleTabCapturedLayout(t, desired)
	if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
		t.Fatal(err)
	}
	if err := sessionstate.LayoutStoreFor(h.state).Save(desired); err != nil {
		t.Fatal(err)
	}
	// Check the target immediately before closing only the fixture-owned window.
	tree := h.tree()
	node := findContainerByID(tree, original)
	if node == nil || !slices.Contains(node.Marks, mark) || restoreCleanupWorkspace(tree, original) != "98" {
		t.Fatal("captured window lost fixture ownership or private workspace placement")
	}
	h.command(fmt.Sprintf("[con_id=%d] kill", original))
	h.until("captured singleton window closed", func() bool { return findContainerByID(h.tree(), original) == nil })
	h.command("workspace 98")
	containerID := mapWindow()
	workspace := restoreCleanupNamedWorkspace(h.tree(), "98")
	if original == containerID || workspace == nil || workspace.Layout != "splith" || len(workspace.Nodes) != 1 || workspace.Nodes[0].ID != containerID || len(workspace.FloatingNodes) != 0 {
		t.Fatal("replacement fixture is not a newly mapped flat leaf on workspace 98")
	}
	h.command("workspace 99")
	return h, registry, desired, containerID
}

func assertSingleTabCapturedLayout(t *testing.T, snapshot sessionstate.LayoutSnapshot) {
	t.Helper()
	if len(snapshot.Workspaces) != 1 {
		t.Errorf("captured layout must contain one managed workspace: %+v", snapshot)
		return
	}
	workspace := snapshot.Workspaces[0]
	if workspace.Name != "98" || workspace.RestoreMode != sessionstate.WorkspaceRestoreLayout || len(workspace.Floating) != 0 || len(workspace.PlacementContexts) != 0 ||
		workspace.Tiling == nil || workspace.Tiling.Layout != sessionstate.LayoutTabbed || workspace.Tiling.Proportion != 1 || len(workspace.Tiling.Children) != 1 {
		t.Errorf("capture lost actual singleton tab parent: %s", singleTabLayoutJSON(t, snapshot))
		return
	}
	leaf := workspace.Tiling.Children[0]
	if leaf.ContextID == nil || *leaf.ContextID != singleTabHeadlessContextID || leaf.Proportion != 1 || len(leaf.Children) != 0 ||
		workspace.FocusedContext == nil || *workspace.FocusedContext != singleTabHeadlessContextID {
		t.Errorf("capture lost singleton identity, proportion, or local focus: %s", singleTabLayoutJSON(t, snapshot))
	}
}

func singleTabLayoutJSON(t *testing.T, snapshot sessionstate.LayoutSnapshot) string {
	t.Helper()
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
