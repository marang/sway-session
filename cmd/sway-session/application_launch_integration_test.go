package main

import (
	"context"
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

// LAB-140: the caller's absent tree becomes stale while Prepare maps a real
// matching window. All compositor, application and database state is private.
// SWAY_SESSION_HEADLESS_INTEGRATION=1 GOTOOLCHAIN=go1.26.5 go test ./cmd/sway-session -run '^TestSessionRuntimeApplicationLaunchHeadless$' -count=1 -timeout=2m -v
func TestSessionRuntimeApplicationLaunchHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway/Alacritty integration")
	}
	for _, name := range []string{"sway", "alacritty", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("headless integration requires %s: %v", name, err)
		}
	}
	for _, test := range []struct {
		name             string
		mapDuringPrepare bool
		wantStarts       int
	}{
		{name: "maps_during_prepare", mapDuringPrepare: true},
		{name: "still_absent", wantStarts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newRestoreCleanupHeadless(t)
			desktop := filepath.Join(h.root, "data", "Alacritty.desktop")
			if err := os.WriteFile(desktop, []byte("[Desktop Entry]\nType=Application\nName=LAB-140 private application\nExec=/usr/bin/false\nStartupWMClass=Alacritty\n"), 0600); err != nil {
				t.Fatal(err)
			}
			app := sessionstate.Context{
				ID: "6ba7b810-9dad-11d1-80b4-00c04fd430c8", Label: "LAB-140 private application",
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
			registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{app}}
			if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
				t.Fatal(err)
			}
			compositorID := strings.Repeat("c", 64)
			launcher := &applicationLaunchHeadlessLauncher{
				h: h, contextID: app.ID, compositorID: compositorID, mapDuringPrepare: test.mapDuringPrepare,
			}
			stream := &swayipc.EventStreamState{}
			runtime, err := newSessionRuntimeWithOptions(&restoreCleanupRealRequester{Client: h.client}, sessionRuntimeOptions{
				Context: h.ctx, Root: h.state, EventStreamState: stream,
				// Adoption grace has already elapsed, without sleeping or passing
				// a future timestamp to the runtime's real clock after preflight.
				StartedAt: time.Now().Add(-2 * time.Second), CompositorID: compositorID,
				ApplicationLauncher: launcher,
				ApplicationRestore: sessionstate.ApplicationRestoreOptions{
					AdoptionGrace: time.Second, CloseGrace: 2 * time.Second, LaunchTimeout: time.Minute, MaxConcurrent: 1,
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
			h.subscribe(runtime, stream)
			h.command("workspace 98")
			h.drain(runtime)
			generation, connected := stream.Snapshot()
			if generation == 0 || !connected {
				t.Fatal("real private event subscription is not connected")
			}
			observe := func(tree *Node) sessionstate.ApplicationGroup {
				t.Helper()
				groups, err := sessionstate.ObserveApplicationGroups(tree, registry)
				if err != nil {
					t.Fatal(err)
				}
				return groups[app.ID]
			}
			loadState := func() sessionstate.ApplicationSessionState {
				t.Helper()
				var state sessionstate.ApplicationSessionState
				if err := sessionstate.ApplicationSessionStoreFor(h.state).LoadIntoContext(h.ctx, &state); err != nil {
					t.Fatal(err)
				}
				if state.CompositorID != compositorID {
					t.Fatalf("application state belongs to another compositor: %+v", state)
				}
				return state
			}
			reconcile := func(tree *Node) {
				t.Helper()
				_, _, degraded, err := runtime.reconcileApplications(tree, registry, time.Now())
				if err != nil || degraded != nil {
					t.Fatalf("reconcile real application launch: error=%v degraded=%v", err, degraded)
				}
			}
			initial := h.tree()
			if group := observe(initial); len(group.Windows) != 0 {
				t.Fatalf("caller tree already contains the application: %+v", group)
			}
			if state := loadState(); len(state.Attempts) != 0 {
				t.Fatalf("private compositor already has launch attempts: %+v", state)
			}
			reconcile(initial)
			if launcher.prepares != 1 {
				t.Fatalf("Prepare calls = %d, want 1", launcher.prepares)
			}
			events := h.drain(runtime)
			if current, connected := stream.Snapshot(); current != generation || !connected {
				t.Fatalf("event stream lost continuity during Prepare: generation=%d connected=%v", current, connected)
			}
			live := observe(h.tree())
			if test.mapDuringPrepare {
				if len(live.Windows) != 1 || live.Ambiguous || live.Anchor == nil ||
					live.Anchor.ContainerID != launcher.windowID || live.Anchor.Workspace != "98" {
					t.Fatalf("Prepare did not map one matching application on private workspace 98: %+v", live)
				}
				if !slices.ContainsFunc(events, func(event swayipc.Event) bool {
					return event.Type == swayipc.EventWindow && event.Change == "new" &&
						event.Container != nil && event.Container.ID == launcher.windowID
				}) {
					t.Fatalf("real subscription did not observe Prepare's mapped window %d", launcher.windowID)
				}
				if group := observe(initial); len(group.Windows) != 0 {
					t.Fatalf("original absent caller tree was modified: %+v", group)
				}
			} else if len(live.Windows) != 0 || launcher.windowID != 0 {
				t.Fatalf("absent control unexpectedly mapped an application: %+v", live)
			}
			assertAttempts := func() {
				t.Helper()
				if launcher.starts != test.wantStarts {
					t.Errorf("Start calls = %d, want %d", launcher.starts, test.wantStarts)
				}
				state := loadState()
				if len(state.Attempts) != test.wantStarts {
					t.Errorf("durable launch attempts = %+v, want %d attempts", state.Attempts, test.wantStarts)
				}
				if state := runtime.applications.State(); len(state.Attempts) != test.wantStarts {
					t.Errorf("in-memory launch attempts = %+v, want %d attempts", state.Attempts, test.wantStarts)
				}
				if test.wantStarts == 1 && (len(launcher.intentAtStart.Attempts) != 1 ||
					len(state.Attempts) != 1 || state.Attempts[0].ContextID != app.ID ||
					!state.Attempts[0].StartedAt.Equal(launcher.intentAtStart.Attempts[0].StartedAt)) {
					t.Errorf("durable attempt differs from intent read inside Start: %+v; %+v", state, launcher.intentAtStart)
				}
			}
			assertAttempts()
			if t.Failed() {
				return
			}
			// Two fresh passes permit adoption to converge and prove that the
			// absent control's recorded intent prevents another fake Start.
			for range 2 {
				reconcile(h.tree())
				h.drain(runtime)
				assertAttempts()
			}
			t.Logf("real subscription generation=%d; Prepare=%d Start=%d durable attempts=%d", generation, launcher.prepares, launcher.starts, len(loadState().Attempts))
		})
	}
}

type applicationLaunchHeadlessLauncher struct {
	h                *restoreCleanupHeadless
	contextID        sessionstate.ContextID
	compositorID     string
	mapDuringPrepare bool
	prepares         int
	starts           int
	windowID         int64
	intentAtStart    sessionstate.ApplicationSessionState
}

func (launcher *applicationLaunchHeadlessLauncher) Prepare(ctx context.Context, app sessionstate.Context) (preparedApplicationLaunch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if app.ID != launcher.contextID {
		return nil, fmt.Errorf("unexpected prepared context %q", app.ID)
	}
	launcher.prepares++
	if launcher.mapDuringPrepare {
		launcher.windowID = lateApplicationWindow(launcher.h.t, launcher.h)
	}
	return &applicationLaunchHeadlessPrepared{launcher: launcher, ctx: ctx}, nil
}

type applicationLaunchHeadlessPrepared struct {
	launcher *applicationLaunchHeadlessLauncher
	ctx      context.Context
}

func (prepared *applicationLaunchHeadlessPrepared) Start() error {
	launcher := prepared.launcher
	launcher.starts++
	// Read through a separate store inside the effect boundary. Start never
	// executes the desktop command or maps a window, even on a regressing runtime.
	if err := sessionstate.ApplicationSessionStoreFor(launcher.h.state).LoadIntoContext(prepared.ctx, &launcher.intentAtStart); err != nil {
		return fmt.Errorf("read durable intent before fake Start: %w", err)
	}
	state := launcher.intentAtStart
	if state.CompositorID != launcher.compositorID || len(state.Attempts) != 1 ||
		state.Attempts[0].ContextID != launcher.contextID || state.Attempts[0].StartedAt.IsZero() {
		return fmt.Errorf("fake Start has no matching durable launch intent: %+v", state)
	}
	return nil
}
