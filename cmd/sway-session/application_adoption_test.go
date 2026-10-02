package main

import (
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

func TestRuntimeAdoptionSurvivesDaemonRestartButNotNewCompositor(t *testing.T) {
	runtime, client, launcher, _, item, now := launchFixture(t)
	item.App.RestorePolicy = sessionstate.ApplicationRestorePinned
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{item}}
	if err := sessionstate.RegistryStoreFor(runtime.root).Save(registry); err != nil {
		t.Fatal(err)
	}
	client.tree = daemonTree("98: apps", applicationLaunchWindow(item, 41))
	if _, err := runtime.Reconcile(client.tree, now); err != nil {
		t.Fatal(err)
	}
	assertRuntimeAdoption(t, runtime.root, item.ID)
	client.tree = daemonTree("98: apps")
	if _, err := runtime.Reconcile(client.tree, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Shutdown(); err != nil {
		t.Fatal(err)
	}
	for _, compositor := range []string{strings.Repeat("f", 64), strings.Repeat("e", 64)} {
		now = now.Add(time.Minute)
		restarted, err := newSessionRuntimeWithOptions(client, sessionRuntimeOptions{
			Root: runtime.root, CompositorID: compositor, StartedAt: now, ApplicationLauncher: launcher,
			ApplicationRestore: sessionstate.ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: time.Minute, MaxConcurrent: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		enableApplicationLaunchFixture(restarted, now.Add(2*time.Second))
		if _, err := restarted.Reconcile(client.tree, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		want := 0
		if compositor == strings.Repeat("e", 64) {
			want = 1
		}
		if len(launcher.starts) != want {
			t.Fatalf("compositor %q: launch count %d, want %d", compositor, len(launcher.starts), want)
		}
		if err := restarted.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRuntimeRetriesFailedAdoptionSaveBeforeExternalEffects(t *testing.T) {
	runtime, client, launcher, _, item, now := launchFixture(t)
	item.App.RestorePolicy = sessionstate.ApplicationRestorePinned
	if err := sessionstate.RegistryStoreFor(runtime.root).Save(sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{item}}); err != nil {
		t.Fatal(err)
	}
	// Session metadata changes in the same transaction as the adoption rows.
	// Abort that real SQLite write without adding a production-only test hook.
	execStateDatabaseForTest(t, runtime.root, `CREATE TRIGGER reject_adoption BEFORE UPDATE ON application_session BEGIN SELECT RAISE(ABORT, 'adoption persistence unavailable'); END`)
	client.tree = daemonTree("98: apps", applicationLaunchWindow(item, 41))
	if _, err := runtime.Reconcile(client.tree, now); err == nil || !strings.Contains(err.Error(), "adoption persistence unavailable") {
		t.Fatalf("expected failed adoption persistence: %v", err)
	}
	if len(client.commands) != 0 || len(launcher.starts) != 0 {
		t.Fatal("failed adoption save crossed an external effect boundary")
	}
	if len(runtime.applications.State().Adoptions) != 1 {
		t.Fatal("failed save discarded the pending observation")
	}
	execStateDatabaseForTest(t, runtime.root, `DROP TRIGGER reject_adoption`)
	client.tree = daemonTree("98: apps")
	if _, err := runtime.Reconcile(client.tree, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertRuntimeAdoption(t, runtime.root, item.ID)
	if len(launcher.starts) != 0 {
		t.Fatal("lost observation relaunched the now closed application")
	}
}

func assertRuntimeAdoption(t *testing.T, root string, id sessionstate.ContextID) {
	t.Helper()
	var state sessionstate.ApplicationSessionState
	if err := sessionstate.ApplicationSessionStoreFor(root).LoadInto(&state); err != nil {
		t.Fatal(err)
	}
	if len(state.Adoptions) != 1 || state.Adoptions[0].ContextID != id || len(state.Attempts) != 0 {
		t.Fatalf("expected one durable adoption without a launch attempt: %+v", state)
	}
}

func TestRuntimeFreshCloseConfirmationPersistsOtherAdoptionsBeforePolicy(t *testing.T) {
	runtime, client, launcher, _, app, now := guardedApplicationRuntime(t)
	other := app
	other.ID = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	otherApp := *app.App
	other.App = &otherApp
	other.App.RestorePolicy = sessionstate.ApplicationRestorePinned
	other.App.Identity.WaylandAppID = "org.example.Other"
	other.App.Identity.SandboxAppID = "org.example.Other"
	other.Launcher.FlatpakID = "org.example.Other"
	if err := sessionstate.RegistryStoreFor(runtime.root).Save(sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{app, other}}); err != nil {
		t.Fatal(err)
	}
	reconcileApplicationClose(t, runtime, client, applicationCloseTree(t, app), now)
	absent := daemonTree("98: apps")
	reconcileApplicationClose(t, runtime, client, absent, now.Add(time.Second))
	client.tree = daemonTree("98: apps", applicationLaunchWindow(other, 42))
	client.beforeTree = func() {
		execStateDatabaseForTest(t, runtime.root, `CREATE TRIGGER reject_adoption BEFORE UPDATE ON application_session BEGIN SELECT RAISE(ABORT, 'fresh adoption persistence unavailable'); END`)
		client.beforeTree = nil
	}
	if _, err := runtime.Reconcile(absent, now.Add(3*time.Second)); err == nil {
		t.Fatal("failed fresh adoption save was ignored")
	}
	assertApplicationDesiredOpen(t, runtime.root, app.ID, true)
	if launcher.starts != 0 {
		t.Fatal("fresh adoption failure launched an application")
	}
	execStateDatabaseForTest(t, runtime.root, `DROP TRIGGER reject_adoption`)
	client.tree = absent
	if _, err := runtime.Reconcile(absent, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertRuntimeAdoption(t, runtime.root, other.ID)
	assertApplicationDesiredOpen(t, runtime.root, app.ID, false)
}

func TestRuntimeCancelledCleanupPersistsVisibleAppBeforeReturning(t *testing.T) {
	for _, failSave := range []bool{false, true} {
		name := "saved"
		if failSave {
			name = "retry failed save"
		}
		t.Run(name, func(t *testing.T) {
			runtime, client, launcher, _, item, now := launchFixture(t)
			item.App.RestorePolicy = sessionstate.ApplicationRestorePinned
			if err := sessionstate.RegistryStoreFor(runtime.root).Save(sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{item}}); err != nil {
				t.Fatal(err)
			}
			window := applicationLaunchWindow(item, 41)
			mark, _ := item.ID.Mark()
			window.Marks = []string{mark}
			client.tree = daemonTree(sessionstate.RestoreStagingWorkspace, window)
			runtime.cancelConflictingRestore()
			if failSave {
				execStateDatabaseForTest(t, runtime.root, `CREATE TRIGGER reject_adoption BEFORE UPDATE ON application_session BEGIN SELECT RAISE(ABORT, 'cleanup adoption persistence unavailable'); END`)
				if _, err := runtime.Reconcile(client.tree, now); err == nil || len(client.commands) != 0 {
					t.Fatalf("cleanup moved before persisting adoption: %v %v", client.commands, err)
				}
				execStateDatabaseForTest(t, runtime.root, `DROP TRIGGER reject_adoption`)
			}
			if refresh, err := runtime.Reconcile(client.tree, now); err != nil || !refresh {
				t.Fatalf("cleanup did not dispatch compensating return: %t %v", refresh, err)
			}
			assertRuntimeAdoption(t, runtime.root, item.ID)
			if len(client.commands) != 1 || !strings.Contains(client.commands[0], `move container to workspace "98: apps"`) {
				t.Fatalf("unexpected cleanup effects: %v", client.commands)
			}
			// Close immediately after cleanup, before another observation pass.
			client.tree = daemonTree("98: apps")
			if err := runtime.Shutdown(); err != nil {
				t.Fatal(err)
			}
			restarted, err := newSessionRuntimeWithOptions(client, sessionRuntimeOptions{
				Root: runtime.root, CompositorID: strings.Repeat("f", 64), StartedAt: now.Add(time.Minute), ApplicationLauncher: launcher,
				ApplicationRestore: sessionstate.ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: time.Minute, MaxConcurrent: 1},
			})
			if err != nil {
				t.Fatal(err)
			}
			enableApplicationLaunchFixture(restarted, now.Add(2*time.Minute))
			if _, err := restarted.Reconcile(client.tree, now.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if len(launcher.starts) != 0 {
				t.Fatal("cleanup lost adoption and relaunched closed pinned app")
			}
		})
	}
}

func TestRuntimeCancelledCleanupMatchingFailureInvalidatesFollowClose(t *testing.T) {
	runtime, client, _, _, app, now := guardedApplicationRuntime(t)
	reconcileApplicationClose(t, runtime, client, applicationCloseTree(t, app), now)
	absent := daemonTree("98: apps")
	reconcileApplicationClose(t, runtime, client, absent, now.Add(time.Second))
	// An interrupted cleanup remains pending while identity evidence becomes
	// unusable. Two persistent identities are an observation failure, not close
	// evidence for the earlier Follow absence timer.
	runtime.restoreRecoveryPending = false
	runtime.restoreCleanupPending = true
	window := applicationLaunchWindow(app, 41)
	first, _ := app.ID.Mark()
	second, _ := sessionstate.ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8").Mark()
	window.Marks = []string{first, second}
	client.tree = daemonTree("98: apps", window)
	if _, err := runtime.Reconcile(client.tree, now.Add(2*time.Second)); err == nil {
		t.Fatal("ambiguous cleanup observation was accepted")
	}
	reconcileApplicationClose(t, runtime, client, absent, now.Add(time.Minute))
	assertApplicationDesiredOpen(t, runtime.root, app.ID, true)
}
