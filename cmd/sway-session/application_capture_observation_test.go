package main

import (
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func TestApplicationCaptureUnrelatedPartialXWaylandDoesNotBlockPersistence(t *testing.T) {
	for _, cancelRestore := range []bool{false, true} {
		name := "mapping_observation"
		if cancelRestore {
			name = "application_reconciliation"
		}
		t.Run(name, func(t *testing.T) {
			runtime, _, _, item, start := testApplicationRuntime(t)
			if cancelRestore {
				runtime.cancelConflictingRestore()
			}
			mark, err := item.ID.Mark()
			if err != nil {
				t.Fatal(err)
			}
			appID, sandbox := item.App.Identity.WaylandAppID, item.App.Identity.SandboxAppID
			leaf := &Node{ID: 42, Type: "con", AppID: &appID, SandboxAppID: &sandbox, Marks: []string{mark}}
			fresh := daemonTree("99: changed", leaf)
			fresh.Nodes[0].Nodes[0].FloatingNodes = []*Node{{ID: 43, Type: "con", WindowProperties: swayipc.WindowProperties{Class: "Unregistered"}}}
			if _, err := sessionstate.CaptureLayout(fresh, runtime.registry); err != nil {
				t.Fatalf("independent capture unexpectedly rejected the observation: %v", err)
			}
			for pass := 0; pass < 2; pass++ {
				_, err := runtime.Reconcile(fresh, start.Add(time.Duration(pass)*2*time.Second))
				if err == nil {
					t.Errorf("strict application observation lost its diagnostic on pass %d", pass)
				}
			}
			if _, queued := runtime.debouncer.Deadline(); !queued {
				t.Fatal("manual workspace change was never queued for persistence")
			}
			if err := runtime.Flush(start.Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var saved sessionstate.LayoutSnapshot
			if err := sessionstate.LayoutStoreFor(runtime.root).LoadInto(&saved); err != nil {
				t.Fatal(err)
			}
			if len(saved.Workspaces) != 1 || saved.Workspaces[0].Name != "99: changed" {
				t.Fatalf("manual change was not persisted: %+v", saved)
			}
			if len(runtime.applications.State().Attempts) != 0 || len(runtime.applications.State().Adoptions) != 0 {
				t.Fatal("uncertain application observation authorized launch/adoption")
			}
		})
	}
}

func TestApplicationCaptureCompleteUnrelatedXWaylandControl(t *testing.T) {
	runtime, _, _, item, start := testApplicationRuntime(t)
	runtime.cancelConflictingRestore()
	mark, err := item.ID.Mark()
	if err != nil {
		t.Fatal(err)
	}
	appID, sandbox := item.App.Identity.WaylandAppID, item.App.Identity.SandboxAppID
	fresh := daemonTree("99: changed", &Node{ID: 42, Type: "con", AppID: &appID, SandboxAppID: &sandbox, Marks: []string{mark}})
	fresh.Nodes[0].Nodes[0].FloatingNodes = []*Node{{ID: 43, Type: "con", WindowProperties: swayipc.WindowProperties{Class: "Unregistered", Instance: "unregistered"}}}
	if _, err := runtime.Reconcile(fresh, start); err != nil {
		t.Fatal(err)
	}
	if _, queued := runtime.debouncer.Deadline(); !queued {
		t.Fatal("complete unrelated identity control did not queue capture")
	}
	if err := runtime.Flush(start.Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var saved sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(runtime.root).LoadInto(&saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Workspaces) != 1 || !strings.HasPrefix(saved.Workspaces[0].Name, "99:") {
		t.Fatalf("control did not save changed workspace: %+v", saved)
	}
}

func TestApplicationCaptureRejectsPossiblyRegisteredPartialXWayland(t *testing.T) {
	for _, policy := range []string{"active", "desired_closed", "archived"} {
		t.Run(policy, func(t *testing.T) {
			runtime, requester, launcher, item, start := testApplicationRuntime(t)
			other := sessionstate.Context{
				ID: "22222222-2222-4222-8222-222222222222", Provider: "desktop", State: sessionstate.ContextActive,
				Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherDesktop, DesktopID: "org.example.Other.desktop", DesktopOrigin: sessionstate.DesktopEntrySystem, DesktopPath: "/usr/share/applications/org.example.Other.desktop"},
				App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowXWayland, X11Class: "PossiblyRegistered", X11Instance: "other"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow},
			}
			if policy == "desired_closed" {
				other.App.DesiredOpen = false
			} else if policy == "archived" {
				other.State = sessionstate.ContextArchived
			}
			registry := runtime.registry
			registry.Contexts = append(registry.Contexts, other)
			if err := sessionstate.RegistryStoreFor(runtime.root).Save(registry); err != nil {
				t.Fatal(err)
			}
			runtime.cancelConflictingRestore()
			mark, _ := item.ID.Mark()
			appID, sandbox := item.App.Identity.WaylandAppID, item.App.Identity.SandboxAppID
			fresh := daemonTree("99: changed", &Node{ID: 42, Type: "con", AppID: &appID, SandboxAppID: &sandbox, Marks: []string{mark}})
			fresh.Nodes[0].Nodes[0].FloatingNodes = []*Node{{ID: 43, Type: "con", WindowProperties: swayipc.WindowProperties{Class: "PossiblyRegistered"}}}
			if _, err := runtime.Reconcile(fresh, start); err == nil {
				t.Fatal("possibly registered incomplete identity was accepted")
			}
			if _, queued := runtime.debouncer.Deadline(); queued {
				t.Fatal("uncertain registered identity authorized capture")
			}
			if len(requester.commands) != 0 || launcher.starts != 0 {
				t.Fatal("uncertain identity authorized external effects")
			}
		})
	}
}

func TestApplicationCaptureObservationFailurePreservesMissingStartupIntent(t *testing.T) {
	runtime, requester, launcher, item, start := testApplicationRuntime(t)
	fresh := daemonTree("99: changed", &Node{ID: 43, Type: "con", WindowProperties: swayipc.WindowProperties{Class: "Unregistered"}})
	for _, now := range []time.Time{start, start.Add(2 * time.Second)} {
		if _, err := runtime.Reconcile(fresh, now); err == nil {
			t.Fatal("strict observation failure lost its diagnostic")
		}
		if err := runtime.Flush(now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	var saved sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(runtime.root).LoadInto(&saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Workspaces) != 1 || saved.Workspaces[0].Name != "98: apps" || len(saved.Workspaces[0].PlacementContexts) != 1 || saved.Workspaces[0].PlacementContexts[0] != item.ID {
		t.Fatalf("missing eligible startup intent was overwritten: %+v", saved)
	}
	if len(requester.commands) != 0 || launcher.starts != 0 || len(runtime.applications.State().Attempts) != 0 || len(runtime.applications.State().Adoptions) != 0 {
		t.Fatal("strict observation failure authorized application effects")
	}
}
