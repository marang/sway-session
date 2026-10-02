package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// Shutdown safety and grace timestamps are injected. Windows, close events,
// event-stream continuity and final absence checks use private Sway.
// SWAY_SESSION_HEADLESS_INTEGRATION=1 GOTOOLCHAIN=go1.26.5 go test ./cmd/sway-session -run '^TestFollowApplicationShutdownHeadless$' -count=1 -v
func TestFollowApplicationShutdownHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway/Alacritty integration")
	}
	for _, name := range []string{"sway", "alacritty", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("headless integration requires %s: %v", name, err)
		}
	}
	for _, missingGuard := range []bool{false, true} {
		name := "unsafe_guard"
		if missingGuard {
			name = "missing_guard"
		}
		t.Run(name, func(t *testing.T) {
			h := newRestoreCleanupHeadless(t)
			desktop := filepath.Join(h.root, "data", "Alacritty.desktop")
			if err := os.WriteFile(desktop, []byte("[Desktop Entry]\nType=Application\nName=LAB-141 private application\nExec=/usr/bin/alacritty\nStartupWMClass=Alacritty\n"), 0600); err != nil {
				t.Fatal(err)
			}
			app := sessionstate.Context{
				ID: "6ba7b810-9dad-11d1-80b4-00c04fd430c8", Label: "LAB-141 private application",
				Provider: "desktop", State: sessionstate.ContextActive,
				Launcher: sessionstate.Launcher{
					Kind: sessionstate.LauncherDesktop, DesktopID: "Alacritty.desktop",
					DesktopOrigin: sessionstate.DesktopEntrySystem, DesktopPath: desktop,
				},
				App: &sessionstate.Application{
					Identity:    sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "Alacritty"},
					DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow,
				},
			}
			if err := sessionstate.RegistryStoreFor(h.state).Save(sessionstate.Registry{
				Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{app},
			}); err != nil {
				t.Fatal(err)
			}
			guard := &testTerminalCloseGuard{generation: 7, safe: true}
			requester := &applicationCloseRealRequester{restoreCleanupRealRequester: &restoreCleanupRealRequester{Client: h.client}}
			launcher := &recordingApplicationLauncher{}
			stream := &swayipc.EventStreamState{}
			start := time.Now()
			now := start
			runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
				Context: h.ctx, Root: h.state, StartedAt: start, EventStreamState: stream,
				CompositorID: strings.Repeat("e", 64), ApplicationLauncher: launcher,
				ApplicationRestore: sessionstate.ApplicationRestoreOptions{
					AdoptionGrace: time.Second, CloseGrace: applicationCloseGrace,
					LaunchTimeout: time.Minute, MaxConcurrent: 1,
				},
				IndicatorCatalog: func() (sessionstate.DesktopCatalog, error) { return sessionstate.DesktopCatalog{}, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := runtime.Shutdown(); err != nil {
					t.Error(err)
				}
			})
			runtime.terminalCloseGuard = guard
			h.subscribe(runtime, stream)
			reconcile := func() {
				t.Helper()
				for range 4 {
					refresh, err := runtime.Reconcile(h.tree(), now)
					if err != nil {
						t.Fatalf("reconcile at %s: %v", now.Sub(start), err)
					}
					if !refresh {
						return
					}
				}
				t.Fatal("application observation did not converge")
			}
			adopt := func() int64 {
				t.Helper()
				h.command("workspace 98")
				id := lateApplicationWindow(t, h)
				h.drain(runtime)
				reconcile()
				h.drain(runtime)
				tree := h.tree()
				node := findContainerByID(tree, id)
				mark, err := app.ID.Mark()
				if err != nil || node == nil || node.AppID == nil || *node.AppID != "Alacritty" ||
					!slices.Contains(node.Marks, mark) || restoreCleanupWorkspace(tree, id) != "98" {
					t.Fatalf("real top-level application was not adopted on workspace 98: %+v; %v", node, err)
				}
				assertApplicationDesiredOpen(t, h.state, app.ID, true)
				return id
			}
			// Adopt actual healthy presence before removing the guard. An empty
			// desired layout keeps this focused on lifecycle rather than restore.
			first := adopt()
			now = start.Add(2 * time.Second)
			reconcile()
			guard.generation++
			guard.safe = false
			if missingGuard {
				runtime.terminalCloseGuard = nil
			}
			// Focus a different private workspace before either close. Killing
			// the owned Alacritty PID models application death while Sway lives.
			h.command("workspace 99")
			h.drain(runtime)
			node := findContainerByID(h.tree(), first)
			if node == nil || node.PID <= 0 {
				t.Fatal("owned application disappeared before process termination")
			}
			commands := len(requester.commands)
			assertApplicationCloseNoEffects(t, h, requester, launcher, commands)
			if err := syscall.Kill(node.PID, syscall.SIGTERM); err != nil {
				t.Fatalf("terminate owned application PID %d: %v", node.PID, err)
			}
			h.until("terminated application absent from live Sway", func() bool {
				return findContainerByID(h.tree(), first) == nil
			})
			assertApplicationCloseEvent(t, h.drain(runtime), first)
			absenceAt := now
			for _, elapsed := range []time.Duration{0, applicationCloseGrace - time.Nanosecond, applicationCloseGrace + time.Second, 2 * time.Minute} {
				now = absenceAt.Add(elapsed)
				reconcile()
				h.drain(runtime)
				assertApplicationDesiredOpen(t, h.state, app.ID, true)
				assertApplicationCloseNoEffects(t, h, requester, launcher, commands)
			}
			// Re-arming alone cannot reuse pre-shutdown presence as close proof.
			guard.generation++
			guard.safe = true
			runtime.terminalCloseGuard = guard
			for range 2 {
				now = now.Add(applicationCloseGrace + time.Second)
				reconcile()
				assertApplicationDesiredOpen(t, h.state, app.ID, true)
				assertApplicationCloseNoEffects(t, h, requester, launcher, commands)
			}
			// Fresh healthy live presence must re-arm normal Follow closes.
			second := adopt()
			h.command("workspace 99")
			h.drain(runtime)
			if restoreCleanupWorkspace(h.tree(), second) != "98" {
				t.Fatal("user close target is not on its owned private workspace")
			}
			commands = len(requester.commands)
			assertApplicationCloseNoEffects(t, h, requester, launcher, commands)
			h.command(fmt.Sprintf("[con_id=%d] kill", second))
			h.until("user-closed application absent from live Sway", func() bool {
				return findContainerByID(h.tree(), second) == nil
			})
			assertApplicationCloseEvent(t, h.drain(runtime), second)
			now = now.Add(time.Second)
			reconcile()
			assertApplicationDesiredOpen(t, h.state, app.ID, true)
			absenceAt = now
			now = absenceAt.Add(applicationCloseGrace - time.Nanosecond)
			reconcile()
			assertApplicationDesiredOpen(t, h.state, app.ID, true)
			requests := requester.treeRequests
			now = absenceAt.Add(applicationCloseGrace)
			reconcile()
			if requester.treeRequests != requests+1 {
				t.Fatalf("healthy close used %d fresh runtime tree requests, want 1", requester.treeRequests-requests)
			}
			assertApplicationDesiredOpen(t, h.state, app.ID, false)
			for range 3 {
				now = now.Add(time.Minute)
				reconcile()
				h.drain(runtime)
				assertApplicationDesiredOpen(t, h.state, app.ID, false)
				assertApplicationCloseNoEffects(t, h, requester, launcher, commands)
			}
			t.Log("real application death and close events; live Sway absence beyond injected grace preserves desired_open=true under uncertainty; fresh healthy presence and confirmed absence disable it; zero launches or runtime focus commands")
		})
	}
}

type applicationCloseRealRequester struct {
	*restoreCleanupRealRequester
	treeRequests int
}

func (requester *applicationCloseRealRequester) RequestContext(ctx context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if kind == swayipc.GetTree {
		requester.treeRequests++
	}
	return requester.restoreCleanupRealRequester.RequestContext(ctx, kind, payload)
}

func assertApplicationCloseEvent(t *testing.T, events []swayipc.Event, id int64) {
	t.Helper()
	if !slices.ContainsFunc(events, func(event swayipc.Event) bool {
		return event.Type == swayipc.EventWindow && event.Change == "close" && event.Container != nil && event.Container.ID == id
	}) {
		t.Fatalf("no real close event for owned application %d: %+v", id, events)
	}
}

func assertApplicationCloseNoEffects(t *testing.T, h *restoreCleanupHeadless, requester *applicationCloseRealRequester, launcher *recordingApplicationLauncher, commands int) {
	t.Helper()
	if launcher.starts != 0 || len(requester.commands) != commands {
		t.Fatalf("application disappearance caused effects: launches=%d commands=%q", launcher.starts, requester.commands[commands:])
	}
	workspace := pathWorkspace(focusedTreePath(h.tree()))
	if workspace == nil || workspace.Name != "99" {
		t.Fatalf("application disappearance moved focus from private workspace 99: %+v", workspace)
	}
}
