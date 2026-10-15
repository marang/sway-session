package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func appInventoryFixture(t *testing.T) (dependencies, sessionstate.Context) {
	t.Helper()
	deps := testDependencies(t)
	app := sessionstate.Context{ID: testContextID, Label: "Editor", Provider: "desktop", State: sessionstate.ContextActive, Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.Editor", FlatpakInstallation: sessionstate.FlatpakUser}, App: &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.Editor", SandboxAppID: "org.example.Editor"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestorePinned}}
	root, _ := deps.stateRoot()
	if err := sessionstate.RegistryStoreFor(root).Save(sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{app}}); err != nil {
		t.Fatal(err)
	}
	deps.observeApplicationLauncher = func(context.Context, sessionstate.Context) sessionstate.ApplicationLauncherObservation {
		return sessionstate.ApplicationLauncherObservation{State: sessionstate.ApplicationLauncherMissing, Reason: "flatpak_not_installed"}
	}
	deps.desktopCatalog = func() (sessionstate.DesktopCatalog, error) {
		t.Fatal("inventory loaded catalog")
		return sessionstate.DesktopCatalog{}, nil
	}
	deps.presentApproval = func(string, []sessionstate.ApprovalChoice) error { t.Fatal("inventory requested approval"); return nil }
	return deps, app
}

func TestApplicationInventoryOfflineIsReadOnlyAndLegacyListUnchanged(t *testing.T) {
	t.Setenv("SWAYSOCK", "")
	deps, app := appInventoryFixture(t)
	deps.newSwayClient = func(string) swayRequester { t.Fatal("offline inventory contacted Sway"); return nil }
	root, _ := deps.stateRoot()
	before := loadTestRegistry(t, deps)
	for _, args := range [][]string{{"app", "list", "--inventory"}, {"app", "status", "--context", "Editor"}} {
		var out, errout bytes.Buffer
		code := runWith(append([]string{"--json"}, args...), strings.NewReader(""), &out, &errout, deps)
		var result commandResult
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || code != exitSuccess || errout.Len() != 0 {
			t.Fatalf("offline result: %d %s %s %v", code, out.String(), errout.String(), err)
		}
		if len(result.Contexts) != 0 || result.Applications == nil || len(*result.Applications) != 1 {
			t.Fatalf("wrong additive contract: %+v", result)
		}
		item := (*result.Applications)[0]
		if item.ContextID != app.ID || item.Presence.State != "unknown" || item.Presence.WindowCount != nil || item.Presence.Anchor != nil || item.LauncherValidity.State != sessionstate.ApplicationLauncherMissing || !item.RestorePolicy.Eligible {
			t.Fatalf("dimensions collapsed: %+v", item)
		}
	}
	legacy, failed := executeAppList(t.Context(), nil, deps)
	if failed != nil || legacy.Applications != nil || !reflect.DeepEqual(legacy.Contexts, before.Contexts) {
		t.Fatalf("legacy list changed: %+v %+v", legacy, failed)
	}
	if after := loadTestRegistry(t, deps); !reflect.DeepEqual(after, before) {
		t.Fatal("inventory changed registry")
	}
	var layout sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(root).LoadInto(&layout); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inventory initialized layout: %v", err)
	}
	if _, failed := executeAppList(t.Context(), []string{"--socket", "/private/socket"}, deps); failed == nil || !failed.usage {
		t.Fatal("socket without inventory accepted")
	}
	if _, failed := executeAppStatus(t.Context(), []string{"--context="}, deps); failed == nil || !failed.usage {
		t.Fatal("empty context silently selected focus")
	}
}

func TestApplicationInventoryUsesOneReadOnlyObservationAndExactAnchor(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(fmt.Sprint(marked), func(t *testing.T) {
			deps, app := appInventoryFixture(t)
			id, sandbox := "org.example.Editor", "org.example.Editor"
			first := &swayipc.TreeNode{ID: 42, Type: "con", AppID: &id, SandboxAppID: &sandbox, Focused: true}
			second := &swayipc.TreeNode{ID: 43, Type: "con", AppID: &id, SandboxAppID: &sandbox}
			if marked {
				mark, _ := app.ID.Mark()
				second.Marks = []string{mark}
			}
			tree := applicationCommandTree(first)
			tree.Nodes[0].Nodes[0].Nodes = append(tree.Nodes[0].Nodes[0].Nodes, second)
			client := &appCommandClient{tree: tree}
			deps.newSwayClient = func(string) swayRequester { return client }
			before := loadTestRegistry(t, deps)
			result, failed := executeAppStatus(t.Context(), []string{"--inventory", "--socket", "/fixture/sway.sock"}, deps)
			if failed != nil {
				t.Fatalf("focused inventory failed: %+v", failed)
			}
			item := (*result.Applications)[0]
			if marked {
				if item.Presence.State != "present" || item.Presence.Anchor.ContainerID != 43 {
					t.Fatalf("exact anchor lost: %+v", item)
				}
			} else {
				if item.Presence.State != "ambiguous" || item.Presence.Anchor != nil {
					t.Fatalf("anchor guessed: %+v", item)
				}
			}
			if client.getTreeCalls != 1 || len(client.commands) != 0 || !reflect.DeepEqual(loadTestRegistry(t, deps), before) {
				t.Fatalf("inventory mutated or repeated observation: %+v", client)
			}
		})
	}
}

func TestApplicationInventoryObservesArchivedRegistrationsWithoutActivating(t *testing.T) {
	deps, app := appInventoryFixture(t)
	root, _ := deps.stateRoot()
	if _, err := sessionstate.UpdateRegistry(root, func(r *sessionstate.Registry) error {
		_, err := sessionstate.SetContextState(r, string(app.ID), sessionstate.ContextArchived)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	id, sandbox := "org.example.Editor", "org.example.Editor"
	client := &appCommandClient{tree: applicationCommandTree(&swayipc.TreeNode{ID: 42, Type: "con", AppID: &id, SandboxAppID: &sandbox})}
	deps.newSwayClient = func(string) swayRequester { return client }
	result, failed := executeAppList(t.Context(), []string{"--inventory", "--socket", "/fixture/sway.sock"}, deps)
	if failed != nil {
		t.Fatal(failed)
	}
	item := (*result.Applications)[0]
	if item.Presence.State != "present" || item.RestorePolicy.Eligible || item.State != sessionstate.ContextArchived || loadTestRegistry(t, deps).Contexts[0].State != sessionstate.ContextArchived {
		t.Fatalf("archive conflated with presence: %+v", item)
	}
}

func TestApplicationInventoryBoundsLauncherWorkersAndSharedDeadline(t *testing.T) {
	deps := dependencies{}
	var running, peak, calls atomic.Int32
	deps.observeApplicationLauncher = func(ctx context.Context, _ sessionstate.Context) sessionstate.ApplicationLauncherObservation {
		calls.Add(1)
		n := running.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		defer running.Add(-1)
		<-ctx.Done()
		return sessionstate.ApplicationLauncherObservation{State: sessionstate.ApplicationLauncherUnknown, Reason: "observation_timeout"}
	}
	items := make([]sessionstate.ApplicationInventoryItem, 65)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	observeApplicationLaunchers(ctx, "", deps, sessionstate.Registry{}, items)
	if peak.Load() > 4 || calls.Load() > 4 || peak.Load() < 1 {
		t.Fatalf("unbounded workers or restarted deadline: peak=%d calls=%d", peak.Load(), calls.Load())
	}
	for _, item := range items {
		if item.LauncherValidity.State != sessionstate.ApplicationLauncherUnknown || item.LauncherValidity.Reason != "observation_timeout" {
			t.Fatalf("lost unknown: %+v", item)
		}
	}
}

func TestApplicationInventoryUnavailableObservationRedactsErrors(t *testing.T) {
	deps, _ := appInventoryFixture(t)
	client := &fakeSwayClient{err: errors.New("private compositor payload at /private/socket")}
	deps.newSwayClient = func(string) swayRequester { return client }
	result, failed := executeAppList(t.Context(), []string{"--inventory", "--socket", "/private/socket"}, deps)
	if failed != nil {
		t.Fatal(failed)
	}
	item := (*result.Applications)[0]
	if item.Presence.State != "unknown" || item.Presence.WindowCount != nil || item.Presence.ObservedAt != nil || !client.closed {
		t.Fatalf("unavailable evidence invented: %+v", item)
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), "private compositor") || strings.Contains(string(encoded), "/private/socket") {
		t.Fatalf("private observation error exported: %s %v", encoded, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, failed := executeAppList(ctx, []string{"--inventory"}, deps); failed == nil {
		t.Fatal("parent cancellation returned successful inventory")
	}
}

func TestApplicationInventoryContextSelectionRejectsTerminalsAndAmbiguousLabels(t *testing.T) {
	deps, app := appInventoryFixture(t)
	root, _ := deps.stateRoot()
	registry := loadTestRegistry(t, deps)
	other := app
	other.ID = "123e4567-e89b-12d3-a456-426614174001"
	other.Launcher.FlatpakID = "org.example.Other"
	other.App = &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.Other", SandboxAppID: "org.example.Other"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow}
	registry.Contexts = append(registry.Contexts, other)
	if err := sessionstate.RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	if _, failed := executeAppStatus(t.Context(), []string{"--context", "Editor"}, deps); failed == nil {
		t.Fatal("ambiguous saved label selected")
	}
	terminal := registerTestContext(t, testDependencies(t))
	terminal.ID = "123e4567-e89b-12d3-a456-426614174002"
	registry.Contexts = append(registry.Contexts, terminal)
	if err := sessionstate.RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	if _, failed := executeAppStatus(t.Context(), []string{"--context", string(terminal.ID)}, deps); failed == nil {
		t.Fatal("terminal selected as application")
	}
}

func TestApplicationInventoryHumanEmptyAndOfflineExplanations(t *testing.T) {
	t.Setenv("SWAYSOCK", "")
	deps, _ := appInventoryFixture(t)
	result, failed := executeAppList(t.Context(), []string{"--inventory"}, deps)
	if failed != nil {
		t.Fatal(failed)
	}
	var out bytes.Buffer
	if err := writeResult(&out, false, result); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"intent: desired open; policy: pinned", "next login: pinned application is desired open", "window: unknown; launcher: missing", "saved placement: unknown", "restore: no recorded outcome"} {
		if !strings.Contains(out.String(), text) {
			t.Fatalf("missing %q in %s", text, out.String())
		}
	}
	t.Log(out.String())
	out.Reset()
	if err := writeApplicationInventory(&out, nil); err != nil || out.String() != "No registered desktop applications.\n" {
		t.Fatalf("empty state: %q %v", out.String(), err)
	}
}

func TestApplicationInventoryPendingProofAndPrivacyThroughState(t *testing.T) {
	t.Setenv("SWAYSOCK", "")
	deps, app := appInventoryFixture(t)
	root, _ := deps.stateRoot()
	record := cliRestoreTestRecord(t, app, "pending", time.Now().UTC())
	record.Requested.Placement = true
	record.Requested.Workspace = "98"
	record.LaunchAccepted = true
	record.WindowMapped = true
	record.Reason = "window_mapped"
	cliRestoreTestSeed(t, root, record)
	deps.observeApplicationLauncher = nil
	// Create a real private user approval with an argument that must not enter
	// the reduced inventory, then validate it through the production assembly.
	privateRoot := t.TempDir()
	executable := filepath.Join(privateRoot, "app")
	if err := os.WriteFile(executable, []byte("approved executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	catalog := testDesktopCatalog(t, map[string]string{"org.example.Editor.desktop": "[Desktop Entry]\nType=Application\nName=Editor\nExec=" + executable + " --private-argument=secret\n"}, false)
	entry := catalog.Entries()[0]
	approval, err := sessionstate.PrepareDesktopApproval(root, app.ID, entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionstate.UpdateRegistry(root, func(r *sessionstate.Registry) error {
		r.Contexts[0].Launcher = approval.Launcher
		r.Contexts[0].App.Identity.SandboxAppID = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	result, failed := executeAppStatus(t.Context(), []string{"--context", string(app.ID)}, deps)
	if failed != nil {
		t.Fatal(failed)
	}
	item := (*result.Applications)[0]
	if item.LauncherValidity.State != sessionstate.ApplicationLauncherValid || len(item.PendingRestore) != 1 || item.PendingRestore[0].Stage != "window_mapped" || item.PendingRestore[0].Identity != "changed" || item.PendingRestore[0].Outcome.PlacementApplied {
		t.Fatalf("lost validation or historical pending proof: %+v", item)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{privateRoot, root, "private-argument", "secret", "approved_desktop_path", "approved_executable_path"} {
		if strings.Contains(string(data), private) {
			t.Fatalf("private data %q exported: %s", private, data)
		}
	}
	if after := cliRestoreTestHistory(t, root); len(after.Outcomes) != 1 || !reflect.DeepEqual(after.Outcomes[0], record) {
		t.Fatalf("inventory modified report: %+v", after)
	}
}
