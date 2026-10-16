package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func administrativeCommandFixture(t *testing.T) (dependencies, *appCommandClient, sessionstate.DesktopCatalog) {
	t.Helper()
	deps := testDependencies(t)
	helper := administrativeCommandHelper()
	if helper == "" {
		helper = "/usr/bin/pkexec" // Default rejection tests need no installed helper.
	}
	catalog := testDesktopCatalog(t, map[string]string{
		"org.example.Admin.desktop": "[Desktop Entry]\nType=Application\nName=Administrative Example\nExec=" + helper + " /usr/bin/true --private-argument=not-for-preview\nStartupWMClass=AdministrativeExample\n",
	}, false)
	deps.desktopCatalog = func() (sessionstate.DesktopCatalog, error) { return catalog, nil }
	appID := "org.example.Admin"
	client := &appCommandClient{tree: applicationCommandTree(&swayipc.TreeNode{ID: 42, Type: "con", Focused: true, AppID: &appID})}
	deps.newSwayClient = func(string) swayRequester { return client }
	store := sessionstate.ApplicationOperationStore{RuntimeRoot: filepath.Join(t.TempDir(), "runtime"), Now: deps.now}
	deps.operationStore = func() (sessionstate.ApplicationOperationStore, error) { return store, nil }
	return deps, client, catalog
}

func administrativeCommandHelper() string {
	for _, path := range []string{"/usr/bin/pkexec", "/usr/bin/sudo"} {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path
		}
	}
	return ""
}

func requireAdministrativeCommandHelper(t *testing.T) {
	t.Helper()
	if administrativeCommandHelper() == "" {
		t.Skip("positive trusted-helper approval requires optional /usr/bin/pkexec or /usr/bin/sudo; no administrative command is executed")
	}
}

func runAdministrativeCommand(t *testing.T, deps dependencies, args ...string) (int, string, string) {
	t.Helper()
	var out, err bytes.Buffer
	code := runWith(args, strings.NewReader(""), &out, &err, deps)
	return code, out.String(), err.String()
}

func TestAppAdministrativeRegistrationRequiresSeparateOptIn(t *testing.T) {
	for _, command := range []string{"register-focused", "register-workspace"} {
		t.Run(command, func(t *testing.T) {
			deps, client, _ := administrativeCommandFixture(t)
			code, _, stderr := runAdministrativeCommand(t, deps, "app", command, "--yes", "--socket", "/run/user/1000/sway.sock")
			if code != exitOperation || !strings.Contains(stderr, "--allow-administrative") {
				t.Fatalf("generic approval authorized administrative registration: code=%d stderr=%q", code, stderr)
			}
			if len(client.commands) != 0 || len(loadRegistryOrEmpty(t, deps).Contexts) != 0 {
				t.Fatal("rejected administrative registration mutated state or Sway")
			}
			t.Run("explicit-opt-in", func(t *testing.T) {
				requireAdministrativeCommandHelper(t)
				code, _, stderr = runAdministrativeCommand(t, deps, "app", command, "--yes", "--allow-administrative", "--socket", "/run/user/1000/sway.sock")
				if code != exitSuccess {
					t.Fatalf("explicit administrative registration failed: code=%d stderr=%q", code, stderr)
				}
				registered := loadTestRegistry(t, deps).Contexts
				if len(registered) != 1 || registered[0].Launcher.Administrative == nil {
					t.Fatalf("administrative permission was not durably recorded: %+v", registered)
				}
			})
		})
	}
}

func TestAppAdministrativePreviewTokenCarriesOnlyExplicitOptIn(t *testing.T) {
	requireAdministrativeCommandHelper(t)
	t.Setenv("SWAYSOCK", "/run/user/1000/sway.sock")
	for _, command := range []string{"register-focused", "register-workspace"} {
		t.Run(command, func(t *testing.T) {
			deps, client, _ := administrativeCommandFixture(t)
			var message string
			var choices []sessionstate.ApprovalChoice
			deps.presentApproval = func(got string, gotChoices []sessionstate.ApprovalChoice) error {
				message, choices = got, gotChoices
				return nil
			}
			code, _, stderr := runAdministrativeCommand(t, deps, "app", command, "--allow-administrative", "--socket", "/run/user/1000/sway.sock")
			if code != exitSuccess || len(choices) != 1 || !strings.Contains(strings.ToLower(message), "authentication") || !strings.Contains(choices[0].Label, "authentication") {
				t.Fatalf("administrative warning missing: code=%d message=%q choices=%+v stderr=%q", code, message, choices, stderr)
			}
			if strings.Contains(message+choices[0].Label, "private-argument") {
				t.Fatal("approval preview exposed desktop arguments")
			}
			if len(client.commands) != 0 || len(loadRegistryOrEmpty(t, deps).Contexts) != 0 {
				t.Fatal("preview registered the application before confirmation")
			}
			store, _ := deps.operationStore()
			active, err := store.Active()
			if err != nil || len(active) != 1 || !active[0].Items[0].AllowAdministrative {
				t.Fatalf("preview did not bind permission to its one-time token: %+v err=%v", active, err)
			}
			code, _, stderr = runAdministrativeCommand(t, deps, "app", "confirm", choices[0].Token)
			if code != exitSuccess || loadTestRegistry(t, deps).Contexts[0].Launcher.Administrative == nil {
				t.Fatalf("confirmed administrative opt-in was lost: code=%d stderr=%q", code, stderr)
			}
			if code, _, _ = runAdministrativeCommand(t, deps, "app", "confirm", choices[0].Token); code != exitOperation {
				t.Fatal("administrative approval token was replayable")
			}
		})
	}
}

func TestAppAdministrativeRebindRequiresFreshOptIn(t *testing.T) {
	deps, _, _ := administrativeCommandFixture(t)
	root, _ := deps.stateRoot()
	old := sessionstate.Context{
		ID: testContextID, Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.Old", FlatpakInstallation: sessionstate.FlatpakUser},
		App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.Old", SandboxAppID: "org.example.Old"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestorePinned},
	}
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error { return sessionstate.AddContext(registry, old) }); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runAdministrativeCommand(t, deps, "app", "rebind-focused", "--yes", "--socket", "/run/user/1000/sway.sock", string(old.ID))
	if code != exitOperation || !strings.Contains(stderr, "--allow-administrative") {
		t.Fatalf("rebind inherited generic approval: code=%d stderr=%q", code, stderr)
	}
	t.Run("explicit-opt-in", func(t *testing.T) {
		requireAdministrativeCommandHelper(t)
		code, _, stderr = runAdministrativeCommand(t, deps, "app", "rebind-focused", "--yes", "--allow-administrative", "--socket", "/run/user/1000/sway.sock", string(old.ID))
		if code != exitSuccess {
			t.Fatalf("explicit administrative rebind failed: code=%d stderr=%q", code, stderr)
		}
		registered := loadTestRegistry(t, deps).Contexts[0]
		if registered.ID != old.ID || registered.Launcher.Administrative == nil || registered.App.RestorePolicy != sessionstate.ApplicationRestorePinned {
			t.Fatalf("rebind lost explicit permission or existing policy: %+v", registered)
		}
	})
}

func TestAppAdministrativeSystemReapprovalRequiresExplicitOptIn(t *testing.T) {
	requireAdministrativeCommandHelper(t)
	deps, _, catalog := administrativeCommandFixture(t)
	root, _ := deps.stateRoot()
	entry, _ := catalog.ByID("org.example.Admin.desktop")
	approval, err := sessionstate.PrepareDesktopApprovalContextWithOptions(context.Background(), root, testContextID, entry, sessionstate.DesktopApprovalOptions{AllowAdministrative: true})
	if err != nil {
		t.Fatal(err)
	}
	// Persist a valid system-origin previous approval as a state fixture. The
	// current catalog entry is a user-local override, so no root-owned files
	// are created or modified by this unit test.
	launcher := approval.Launcher
	launcher.DesktopOrigin = sessionstate.DesktopEntrySystem
	launcher.DesktopPath = "/usr/share/applications/org.example.Admin.desktop"
	launcher.DesktopEntrySHA256, launcher.ApprovedDesktopPath, launcher.ApprovedExecutablePath, launcher.ApprovedExecutableSHA256 = "", "", "", ""
	previous := sessionstate.Context{ID: testContextID, State: sessionstate.ContextActive, Launcher: launcher,
		App: &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.Admin"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestorePinned}}
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error { return sessionstate.AddContext(registry, previous) }); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runAdministrativeCommand(t, deps, "app", "reapprove", "--yes", string(testContextID)); code != exitOperation || !strings.Contains(stderr, "--allow-administrative") {
		t.Fatalf("system administrative reapproval silently reused permission: code=%d stderr=%q", code, stderr)
	}
	var message string
	var choices []sessionstate.ApprovalChoice
	deps.presentApproval = func(got string, gotChoices []sessionstate.ApprovalChoice) error {
		message, choices = got, gotChoices
		return nil
	}
	if code, _, stderr := runAdministrativeCommand(t, deps, "app", "reapprove", "--allow-administrative", string(testContextID)); code != exitSuccess {
		t.Fatalf("existing system administrative metadata could not be reapproved: code=%d stderr=%q", code, stderr)
	}
	if len(choices) != 1 || !strings.Contains(message, "authentication") || !strings.Contains(choices[0].Label, "authentication") {
		t.Fatalf("reapproval did not warn about later authentication: message=%q choices=%+v", message, choices)
	}
	if code, _, stderr := runAdministrativeCommand(t, deps, "app", "confirm", choices[0].Token); code != exitSuccess {
		t.Fatalf("system-origin administrative token failed its apply check: code=%d stderr=%q", code, stderr)
	}
	updated := loadTestRegistry(t, deps).Contexts[0]
	if updated.Launcher.Administrative == nil || updated.App.RestorePolicy != sessionstate.ApplicationRestorePinned {
		t.Fatalf("reapproval lost administrative permission or policy: %+v", updated)
	}
	encoded, err := json.Marshal(updated)
	if err != nil || bytes.Contains(encoded, []byte("private-argument")) {
		t.Fatal("administrative context persisted raw desktop arguments")
	}
}

func TestAppAdministrativeOptInCanUpgradeExistingSystemRegistration(t *testing.T) {
	requireAdministrativeCommandHelper(t)
	deps, _, _ := administrativeCommandFixture(t)
	root, _ := deps.stateRoot()
	previous := sessionstate.Context{
		ID: testContextID, State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherDesktop, DesktopOrigin: sessionstate.DesktopEntrySystem,
			DesktopID: "org.example.Admin.desktop", DesktopPath: "/usr/share/applications/org.example.Admin.desktop"},
		App: &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.Admin"},
			DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestorePinned},
	}
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error { return sessionstate.AddContext(registry, previous) }); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runAdministrativeCommand(t, deps, "app", "reapprove", "--yes", "--allow-administrative", string(testContextID)); code != exitSuccess {
		t.Fatalf("explicit opt-in could not upgrade an existing system registration: code=%d stderr=%q", code, stderr)
	}
	if got := loadTestRegistry(t, deps).Contexts[0]; got.ID != previous.ID || got.Launcher.Administrative == nil || got.App.RestorePolicy != sessionstate.ApplicationRestorePinned {
		t.Fatalf("opt-in lost context or policy: %+v", got)
	}
}

func TestAppAdministrativeConfirmationRejectsChangedReviewedEntry(t *testing.T) {
	requireAdministrativeCommandHelper(t)
	t.Setenv("SWAYSOCK", "/run/user/1000/sway.sock")
	for _, command := range []string{"register-focused", "register-workspace"} {
		t.Run(command, func(t *testing.T) {
			deps, client, catalog := administrativeCommandFixture(t)
			entry, _ := catalog.ByID("org.example.Admin.desktop")
			deps.desktopCatalog = func() (sessionstate.DesktopCatalog, error) {
				return sessionstate.LoadDesktopCatalog([]sessionstate.DesktopSearchDirectory{{Path: filepath.Dir(entry.Path), Origin: sessionstate.DesktopEntryUser}})
			}
			var choices []sessionstate.ApprovalChoice
			deps.presentApproval = func(_ string, got []sessionstate.ApprovalChoice) error { choices = got; return nil }
			if code, _, stderr := runAdministrativeCommand(t, deps, "app", command, "--allow-administrative", "--socket", "/run/user/1000/sway.sock"); code != exitSuccess {
				t.Fatalf("preview failed: code=%d stderr=%q", code, stderr)
			}
			store, _ := deps.operationStore()
			active, err := store.Active()
			if err != nil || len(active) != 1 || active[0].Items[0].AdministrativeApprovalSHA256 == "" {
				t.Fatalf("preview lacks reviewed launcher identity: %+v err=%v", active, err)
			}
			unrelated := active[0]
			unrelated.Items = append([]sessionstate.ApplicationOperationItem(nil), unrelated.Items...)
			unrelated.Items[0].ContextID = "22222222-2222-4222-8222-222222222222"
			unrelated.Items[0].AllowAdministrative, unrelated.Items[0].AdministrativeApprovalSHA256 = false, ""
			if _, err := store.Create(unrelated); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(entry.Path)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.Replace(data, []byte("/usr/bin/true"), []byte("/usr/bin/false"), 1)
			if err := os.WriteFile(entry.Path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if code, _, stderr := runAdministrativeCommand(t, deps, "app", "confirm", choices[0].Token); code != exitOperation || !strings.Contains(stderr, "changed while approval was pending") {
				t.Fatalf("changed administrative launcher received stale opt-in: code=%d stderr=%q", code, stderr)
			}
			if len(client.commands) != 0 || len(loadRegistryOrEmpty(t, deps).Contexts) != 0 {
				t.Fatal("stale administrative confirmation registered or marked a window")
			}
			active, err = store.Active()
			if err != nil || len(active) != 1 || active[0].Items[0].ContextID != unrelated.Items[0].ContextID {
				t.Fatalf("rejection consumed an unrelated approval: %+v err=%v", active, err)
			}
			root, _ := deps.stateRoot()
			files, err := os.ReadDir(filepath.Join(root, "desktop-approvals"))
			if !os.IsNotExist(err) && (err != nil || len(files) != 0) {
				t.Fatalf("rejected stale approval left a desktop snapshot: files=%v err=%v", files, err)
			}
		})
	}
}

func TestAppOrdinaryEntryDoesNotInheritAdministrativeOptIn(t *testing.T) {
	deps, _, _ := administrativeCommandFixture(t)
	catalog := testDesktopCatalog(t, map[string]string{
		"org.example.Admin.desktop": "[Desktop Entry]\nType=Application\nName=Ordinary Example\nExec=/usr/bin/true\n",
	}, false)
	deps.desktopCatalog = func() (sessionstate.DesktopCatalog, error) { return catalog, nil }
	deps.presentApproval = func(string, []sessionstate.ApprovalChoice) error { return nil }
	if code, _, stderr := runAdministrativeCommand(t, deps, "app", "register-focused", "--allow-administrative", "--socket", "/run/user/1000/sway.sock"); code != exitSuccess {
		t.Fatalf("ordinary preview with an unused permission flag failed: code=%d stderr=%q", code, stderr)
	}
	store, _ := deps.operationStore()
	active, err := store.Active()
	if err != nil || len(active) != 1 {
		t.Fatalf("read ordinary pending approval: %+v err=%v", active, err)
	}
	item := active[0].Items[0]
	if item.AllowAdministrative || item.AdministrativeApprovalSHA256 != "" {
		t.Fatalf("ordinary entry acquired permission for a later administrative replacement: %+v", item)
	}
	data, err := json.Marshal(item)
	if err != nil || bytes.Contains(data, []byte("administrative")) {
		t.Fatalf("ordinary operation wire shape gained administrative fields: %s err=%v", data, err)
	}
}
