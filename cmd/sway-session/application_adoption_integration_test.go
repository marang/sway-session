package main

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

// Uses a private compositor and a real window; never the login SWAYSOCK.
func TestApplicationAdoptionDaemonRestartHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway integration")
	}
	for _, name := range []string{"sway", "alacritty", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("private integration requires %s: %v", name, err)
		}
	}
	h := newRestoreCleanupHeadless(t)
	h.command("workspace 98")
	container := lateApplicationWindow(t, h)
	app := lifecycleHeadlessApplication("6ba7b810-9dad-11d1-80b4-00c04fd430c8", testManagedContextID, "Adoption.desktop")
	app.App.Identity.WaylandAppID = "Alacritty"
	app.App.RestorePolicy = sessionstate.ApplicationRestorePinned
	if err := sessionstate.RegistryStoreFor(h.state).Save(sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{app}}); err != nil {
		t.Fatal(err)
	}
	launcher := &recordingApplicationLauncher{}
	now := time.Now()
	newRuntime := func() *sessionRuntime {
		t.Helper()
		runtime, err := newSessionRuntimeWithOptions(h.client, sessionRuntimeOptions{
			Context: h.ctx, Root: h.state, CompositorID: strings.Repeat("d", 64), StartedAt: now, ApplicationLauncher: launcher,
			ApplicationRestore: sessionstate.ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: time.Minute, MaxConcurrent: 1},
			IndicatorCatalog:   func() (sessionstate.DesktopCatalog, error) { return sessionstate.DesktopCatalog{}, nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		enableApplicationLaunchFixture(runtime, now.Add(2*time.Second))
		return runtime
	}
	runtime := newRuntime()
	for range 4 {
		if _, err := runtime.Reconcile(h.tree(), now); err != nil {
			t.Fatal(err)
		}
	}
	assertRuntimeAdoption(t, h.state, app.ID)
	// Check ownership and actual high-workspace placement immediately before
	// closing the one disposable window created by this test.
	tree := h.tree()
	node := findContainerByID(tree, container)
	mark, _ := app.ID.Mark()
	if node == nil || !slices.Contains(node.Marks, mark) || restoreCleanupWorkspace(tree, container) != "98" {
		t.Fatal("owned application was not adopted on workspace 98")
	}
	h.command(fmt.Sprintf("[con_id=%d] kill", container))
	h.until("owned application closed", func() bool { return findContainerByID(h.tree(), container) == nil })
	now = now.Add(3 * time.Second)
	if _, err := runtime.Reconcile(h.tree(), now); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Shutdown(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	restarted := newRuntime()
	defer func() {
		if err := restarted.Shutdown(); err != nil {
			t.Error(err)
		}
	}()
	for range 4 {
		if _, err := restarted.Reconcile(h.tree(), now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	assertRuntimeAdoption(t, h.state, app.ID)
	if len(launcher.contexts) != 0 {
		t.Fatalf("closed real application relaunched after daemon restart: %v", launcher.contexts)
	}
	t.Log("real private Sway window adopted, closed, and kept closed across runtime restart; no launch attempt persisted")
}
