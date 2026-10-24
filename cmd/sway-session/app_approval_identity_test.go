package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func ordinaryApprovalFixture(t *testing.T) (dependencies, *appCommandClient, sessionstate.DesktopEntry, string) {
	t.Helper()
	deps := testDependencies(t)
	executable := filepath.Join(t.TempDir(), "reviewed-app")
	if err := os.WriteFile(executable, []byte("reviewed executable bytes"), 0o700); err != nil {
		t.Fatal(err)
	}
	catalog := testDesktopCatalog(t, map[string]string{
		"org.example.App.desktop": "[Desktop Entry]\nType=Application\nName=Reviewed App\nExec=" + executable + " --reviewed\n",
	}, false)
	entry, _ := catalog.ByID("org.example.App.desktop")
	deps.desktopCatalog = func() (sessionstate.DesktopCatalog, error) {
		return sessionstate.LoadDesktopCatalog([]sessionstate.DesktopSearchDirectory{{Path: filepath.Dir(entry.Path), Origin: sessionstate.DesktopEntryUser}})
	}
	appID := "org.example.App"
	client := &appCommandClient{tree: applicationCommandTree(&swayipc.TreeNode{ID: 42, Type: "con", Focused: true, AppID: &appID})}
	deps.newSwayClient = func(string) swayRequester { return client }
	store := sessionstate.ApplicationOperationStore{RuntimeRoot: filepath.Join(t.TempDir(), "runtime"), Now: deps.now}
	deps.operationStore = func() (sessionstate.ApplicationOperationStore, error) { return store, nil }
	return deps, client, entry, executable
}

func TestAppOrdinaryApprovalRejectsChangedReviewedLauncher(t *testing.T) {
	for _, command := range []string{"register-focused", "rebind-focused", "reapprove"} {
		for _, mutation := range []string{"desktop", "executable-path", "executable"} {
			t.Run(command+"/"+mutation, func(t *testing.T) {
				deps, client, entry, executable := ordinaryApprovalFixture(t)
				root, _ := deps.stateRoot()
				if command != "register-focused" {
					approval, err := sessionstate.PrepareDesktopApproval(root, testContextID, entry)
					if err != nil {
						t.Fatal(err)
					}
					previous := sessionstate.Context{ID: testContextID, State: sessionstate.ContextActive, Launcher: approval.Launcher,
						App: &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestorePinned}}
					if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error { return sessionstate.AddContext(registry, previous) }); err != nil {
						t.Fatal(err)
					}
				}
				before := loadRegistryOrEmpty(t, deps).Contexts
				var message string
				var choices []sessionstate.ApprovalChoice
				deps.presentApproval = func(got string, gotChoices []sessionstate.ApprovalChoice) error {
					message, choices = got, gotChoices
					return nil
				}
				args := []string{"app", command}
				if command != "reapprove" {
					args = append(args, "--socket", "/run/user/1000/sway.sock")
				}
				if command != "register-focused" {
					args = append(args, string(testContextID))
				}
				if code, _, stderr := runAdministrativeCommand(t, deps, args...); code != exitSuccess || len(choices) != 1 || !strings.Contains(message, executable) {
					t.Fatalf("reviewed launcher preview failed: code=%d message=%q choices=%+v stderr=%q", code, message, choices, stderr)
				}
				store, _ := deps.operationStore()
				active, err := store.Active()
				if err != nil || len(active) != 1 || active[0].Items[0].ReviewedLauncherSHA256 == "" {
					t.Fatalf("preview omitted reviewed identity: %+v err=%v", active, err)
				}
				if mutation == "desktop" || mutation == "executable-path" {
					data, err := os.ReadFile(entry.Path)
					if err != nil {
						t.Fatal(err)
					}
					changed := strings.Replace(string(data), "--reviewed", "--unreviewed", 1)
					if mutation == "executable-path" {
						payload := filepath.Join(t.TempDir(), "unreviewed-app")
						if err := os.WriteFile(payload, []byte("unreviewed executable bytes"), 0o700); err != nil {
							t.Fatal(err)
						}
						changed = strings.Replace(string(data), executable, payload, 1)
					}
					if err := os.WriteFile(entry.Path, []byte(changed), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(executable, []byte("unreviewed executable bytes"), 0o700); err != nil {
					t.Fatal(err)
				}
				if code, _, stderr := runAdministrativeCommand(t, deps, "app", "confirm", choices[0].Token); code != exitOperation || !strings.Contains(stderr, "changed while approval was pending") {
					t.Fatalf("changed ordinary launcher received stale approval: code=%d stderr=%q", code, stderr)
				}
				if len(client.commands) != 0 || !reflect.DeepEqual(loadRegistryOrEmpty(t, deps).Contexts, before) {
					t.Fatal("stale ordinary confirmation changed registry or window marks")
				}
			})
		}
	}
}

func TestAppOrdinaryApprovalAcceptsUnchangedReviewedLauncher(t *testing.T) {
	deps, _, _, _ := ordinaryApprovalFixture(t)
	var choices []sessionstate.ApprovalChoice
	deps.presentApproval = func(_ string, got []sessionstate.ApprovalChoice) error { choices = got; return nil }
	if code, _, stderr := runAdministrativeCommand(t, deps, "app", "register-focused", "--socket", "/run/user/1000/sway.sock"); code != exitSuccess || len(choices) != 1 {
		t.Fatalf("preview failed: code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := runAdministrativeCommand(t, deps, "app", "confirm", choices[0].Token); code != exitSuccess || len(loadTestRegistry(t, deps).Contexts) != 1 {
		t.Fatalf("unchanged reviewed launcher was rejected: code=%d stderr=%q", code, stderr)
	}
}

func TestAppApprovalRequiresFreshPreviewForLegacyUnboundToken(t *testing.T) {
	deps, _, entry, _ := ordinaryApprovalFixture(t)
	root, _ := deps.stateRoot()
	item := sessionstate.ApplicationOperationItem{ContextID: testContextID, DesktopID: entry.ID}
	if _, err := prepareApplicationOperationApproval(context.Background(), root, item, entry); err == nil || !strings.Contains(err.Error(), "fresh preview") {
		t.Fatalf("legacy unbound approval did not require fresh review: %v", err)
	}
}

func TestPreparedApplicationApprovalRejectsChangedIdentityAndCleansSnapshot(t *testing.T) {
	deps, _, entry, _ := ordinaryApprovalFixture(t)
	root, _ := deps.stateRoot()
	item := sessionstate.ApplicationOperationItem{ContextID: testContextID, DesktopID: entry.ID}
	if err := bindApplicationOperationApproval(context.Background(), &item, entry); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(entry.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry.Path, append(data, []byte("Comment=changed after precheck\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := deps.desktopCatalog()
	if err != nil {
		t.Fatal(err)
	}
	changed, _ := catalog.ByID(entry.ID)
	prepared, err := sessionstate.PrepareDesktopApproval(root, testContextID, changed)
	if err != nil || !prepared.SnapshotCreated {
		t.Fatalf("prepare changed race fixture: %+v err=%v", prepared, err)
	}
	if _, err := validatePreparedApplicationOperationApproval(context.Background(), root, item, prepared); err == nil || !strings.Contains(err.Error(), "changed while approval was pending") {
		t.Fatalf("prepared evidence bypassed reviewed identity: %v", err)
	}
	if _, err := os.Stat(prepared.SnapshotPath); !os.IsNotExist(err) {
		t.Fatalf("rejected preparation left a snapshot: %v", err)
	}
}
