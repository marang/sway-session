package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
	"github.com/marang/sway-title-animator/internal/swayipc"
)

func TestAppRegisterFocusedFlatpakIsExplicitMarkedAndIdempotent(t *testing.T) {
	deps := testDependencies(t)
	catalog := testDesktopCatalog(t, map[string]string{
		"org.example.App.desktop": "[Desktop Entry]\nType=Application\nName=Example App\nExec=/usr/bin/flatpak run org.example.App\nX-Flatpak=org.example.App\n",
	}, true)
	deps.desktopCatalog = func() (sessionstate.DesktopCatalog, error) { return catalog, nil }
	appID := "org.example.App"
	sandbox := "org.example.App"
	tree := applicationCommandTree(&swayipc.TreeNode{ID: 42, Type: "con", Focused: true, AppID: &appID, SandboxAppID: &sandbox})
	client := &appCommandClient{tree: tree}
	deps.newSwayClient = func(string) swayRequester { return client }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"--json", "app", "register-focused", "--yes", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("registration failed code=%d stderr=%q", code, stderr.String())
	}
	registry := loadTestRegistry(t, deps)
	if len(registry.Contexts) != 1 || registry.Contexts[0].Launcher.Kind != sessionstate.LauncherFlatpak || !registry.Preferences.DesktopIndicators {
		t.Fatalf("unexpected registered application: %+v", registry)
	}
	mark, _ := registry.Contexts[0].ID.Mark()
	if len(client.commands) != 1 || !strings.Contains(client.commands[0], "[con_id=42] mark --add "+mark) {
		t.Fatalf("registration did not use an exact typed Sway mark: %q", client.commands)
	}

	stdout.Reset()
	stderr.Reset()
	code = runWith([]string{"app", "register-focused", "--yes", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitSuccess || len(loadTestRegistry(t, deps).Contexts) != 1 || !strings.Contains(stdout.String(), "already registered") {
		t.Fatalf("repeat registration was not an idempotent status result: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestDiscardDesktopApprovalsSharesOneTotalCleanupBudget(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := sessionstate.RegistryStoreFor(root).Save(sessionstate.Registry{
		Version:  sessionstate.ContextsSchemaVersion,
		Contexts: []sessionstate.Context{},
	}); err != nil {
		t.Fatal(err)
	}
	lockEntered := make(chan struct{})
	releaseLock := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- sessionstate.InspectRegistryLocked(root, func(sessionstate.Registry) error {
			close(lockEntered)
			<-releaseLock
			return nil
		})
	}()
	<-lockEntered

	approvals := make([]sessionstate.DesktopApproval, 3)
	for index := range approvals {
		approvals[index] = sessionstate.DesktopApproval{
			SnapshotCreated: true,
			Launcher: sessionstate.Launcher{
				ApprovedDesktopPath: filepath.Join(root, "desktop-approvals", fmt.Sprintf("snapshot-%d.desktop", index)),
			},
		}
	}
	started := time.Now()
	err := discardDesktopApprovals(context.Background(), root, approvals)
	elapsed := time.Since(started)
	close(releaseLock)
	if lockErr := <-lockDone; lockErr != nil {
		t.Fatalf("release registry lock: %v", lockErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked cleanup returned %v, want deadline exceeded", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("cleanup restarted its total budget per approval: %v", elapsed)
	}
}

func TestAppRegisterFocusedDoesNotMoveHealthyAnchorToSameApplicationSibling(t *testing.T) {
	deps := testDependencies(t)
	registered := sessionstate.Context{
		ID: testContextID, Label: "Example", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.App", FlatpakInstallation: sessionstate.FlatpakUser},
		App: &sessionstate.Application{
			Identity:      sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App", SandboxAppID: "org.example.App"},
			DesiredOpen:   true,
			RestorePolicy: sessionstate.ApplicationRestoreFollow,
		},
	}
	root, _ := deps.stateRoot()
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
		return sessionstate.AddContext(registry, registered)
	}); err != nil {
		t.Fatal(err)
	}
	mark, _ := registered.ID.Mark()
	appID, sandbox := "org.example.App", "org.example.App"
	anchor := &swayipc.TreeNode{ID: 42, Type: "con", AppID: &appID, SandboxAppID: &sandbox, Marks: []string{mark}}
	sibling := &swayipc.TreeNode{ID: 43, Type: "con", Focused: true, AppID: &appID, SandboxAppID: &sandbox}
	tree := applicationCommandTree(anchor)
	tree.Nodes[0].Nodes[0].Nodes = append(tree.Nodes[0].Nodes[0].Nodes, sibling)
	client := &appCommandClient{tree: tree}
	deps.newSwayClient = func(string) swayRequester { return client }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"app", "register-focused", "--yes", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitSuccess || stderr.Len() != 0 || !strings.Contains(stdout.String(), "already registered") {
		t.Fatalf("registered sibling status failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(client.commands) != 0 || !containsString(anchor.Marks, mark) || containsString(sibling.Marks, mark) {
		t.Fatalf("registered sibling moved the healthy anchor: commands=%q anchor=%q sibling=%q", client.commands, anchor.Marks, sibling.Marks)
	}
}

func TestAppRegisterFocusedRejectsRegistryChangeAfterObservation(t *testing.T) {
	deps := testDependencies(t)
	registered := sessionstate.Context{
		ID: testContextID, Label: "Example", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.App", FlatpakInstallation: sessionstate.FlatpakUser},
		App: &sessionstate.Application{
			Identity:      sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App", SandboxAppID: "org.example.App"},
			DesiredOpen:   true,
			RestorePolicy: sessionstate.ApplicationRestoreFollow,
		},
	}
	root, _ := deps.stateRoot()
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
		return sessionstate.AddContext(registry, registered)
	}); err != nil {
		t.Fatal(err)
	}
	appID, sandbox := "org.example.App", "org.example.App"
	focused := &swayipc.TreeNode{ID: 42, Type: "con", Focused: true, AppID: &appID, SandboxAppID: &sandbox}
	client := &appCommandClient{tree: applicationCommandTree(focused)}
	client.afterGetTree = func(call int) {
		if call != 1 {
			return
		}
		if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
			_, err := sessionstate.RemoveContext(registry, string(registered.ID))
			return err
		}); err != nil {
			t.Fatalf("remove context between observations: %v", err)
		}
	}
	deps.newSwayClient = func(string) swayRequester { return client }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"app", "register-focused", "--yes", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitOperation || !strings.Contains(stderr.String(), "changed") {
		t.Fatalf("stale registry observation was not rejected: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(client.commands) != 0 {
		t.Fatalf("stale registry observation mutated Sway: commands=%q", client.commands)
	}
}

func TestAppRegisterFocusedRejectsMarkOnUnrelatedContainer(t *testing.T) {
	deps := testDependencies(t)
	registered := sessionstate.Context{
		ID: testContextID, Label: "Example", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.App", FlatpakInstallation: sessionstate.FlatpakUser},
		App: &sessionstate.Application{
			Identity:      sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App", SandboxAppID: "org.example.App"},
			DesiredOpen:   true,
			RestorePolicy: sessionstate.ApplicationRestoreFollow,
		},
	}
	root, _ := deps.stateRoot()
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
		return sessionstate.AddContext(registry, registered)
	}); err != nil {
		t.Fatal(err)
	}
	mark, _ := registered.ID.Mark()
	unrelatedAppID, unrelatedSandbox := "org.example.Other", "org.example.Other"
	unrelated := &swayipc.TreeNode{ID: 42, Type: "con", AppID: &unrelatedAppID, SandboxAppID: &unrelatedSandbox, Marks: []string{mark}}
	appID, sandbox := "org.example.App", "org.example.App"
	focused := &swayipc.TreeNode{ID: 43, Type: "con", Focused: true, AppID: &appID, SandboxAppID: &sandbox}
	tree := applicationCommandTree(unrelated)
	tree.Nodes[0].Nodes[0].Nodes = append(tree.Nodes[0].Nodes[0].Nodes, focused)
	client := &appCommandClient{tree: tree}
	deps.newSwayClient = func(string) swayRequester { return client }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"app", "register-focused", "--yes", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitOperation || !strings.Contains(stderr.String(), "does not match") {
		t.Fatalf("unrelated marked container was not rejected: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(client.commands) != 0 || !containsString(unrelated.Marks, mark) || containsString(focused.Marks, mark) {
		t.Fatalf("unrelated mark was moved: commands=%q unrelated=%q focused=%q", client.commands, unrelated.Marks, focused.Marks)
	}
}

func TestAppRegisterFocusedRepairsGloballyMissingApplicationAnchor(t *testing.T) {
	deps := testDependencies(t)
	registered := sessionstate.Context{
		ID: testContextID, Label: "Example", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.App", FlatpakInstallation: sessionstate.FlatpakUser},
		App: &sessionstate.Application{
			Identity:      sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App", SandboxAppID: "org.example.App"},
			DesiredOpen:   true,
			RestorePolicy: sessionstate.ApplicationRestoreFollow,
		},
	}
	root, _ := deps.stateRoot()
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
		return sessionstate.AddContext(registry, registered)
	}); err != nil {
		t.Fatal(err)
	}
	mark, _ := registered.ID.Mark()
	appID, sandbox := "org.example.App", "org.example.App"
	window := &swayipc.TreeNode{ID: 42, Type: "con", Focused: true, AppID: &appID, SandboxAppID: &sandbox}
	client := &appCommandClient{tree: applicationCommandTree(window)}
	deps.newSwayClient = func(string) swayRequester { return client }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"app", "register-focused", "--yes", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("missing anchor repair failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(client.commands) != 1 || !strings.Contains(client.commands[0], "[con_id=42] mark --add "+mark) || !containsString(window.Marks, mark) {
		t.Fatalf("globally missing anchor was not repaired: commands=%q marks=%q", client.commands, window.Marks)
	}
}

func TestAppRegisterFocusedRefusesAmbiguousMissingApplicationAnchor(t *testing.T) {
	deps := testDependencies(t)
	registered := sessionstate.Context{
		ID: testContextID, Label: "Example", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.App", FlatpakInstallation: sessionstate.FlatpakUser},
		App: &sessionstate.Application{
			Identity:      sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App", SandboxAppID: "org.example.App"},
			DesiredOpen:   true,
			RestorePolicy: sessionstate.ApplicationRestoreFollow,
		},
	}
	root, _ := deps.stateRoot()
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
		return sessionstate.AddContext(registry, registered)
	}); err != nil {
		t.Fatal(err)
	}
	appID, sandbox := "org.example.App", "org.example.App"
	first := &swayipc.TreeNode{ID: 42, Type: "con", AppID: &appID, SandboxAppID: &sandbox}
	focused := &swayipc.TreeNode{ID: 43, Type: "con", Focused: true, AppID: &appID, SandboxAppID: &sandbox}
	tree := applicationCommandTree(first)
	tree.Nodes[0].Nodes[0].Nodes = append(tree.Nodes[0].Nodes[0].Nodes, focused)
	client := &appCommandClient{tree: tree}
	deps.newSwayClient = func(string) swayRequester { return client }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"app", "register-focused", "--yes", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitOperation || !strings.Contains(stderr.String(), "ambiguous") {
		t.Fatalf("ambiguous missing anchor was not rejected: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(client.commands) != 0 {
		t.Fatalf("ambiguous missing anchor mutated Sway: commands=%q", client.commands)
	}
}

func TestAppRegisterFocusedRefusesMissingAnchorWithScratchpadSibling(t *testing.T) {
	deps := testDependencies(t)
	registered := sessionstate.Context{
		ID: testContextID, Label: "Example", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.App", FlatpakInstallation: sessionstate.FlatpakUser},
		App: &sessionstate.Application{
			Identity:      sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App", SandboxAppID: "org.example.App"},
			DesiredOpen:   true,
			RestorePolicy: sessionstate.ApplicationRestoreFollow,
		},
	}
	root, _ := deps.stateRoot()
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
		return sessionstate.AddContext(registry, registered)
	}); err != nil {
		t.Fatal(err)
	}
	appID, sandbox := "org.example.App", "org.example.App"
	focused := &swayipc.TreeNode{ID: 42, Type: "con", Focused: true, AppID: &appID, SandboxAppID: &sandbox}
	scratchpadSibling := &swayipc.TreeNode{ID: 43, Type: "con", AppID: &appID, SandboxAppID: &sandbox}
	tree := applicationCommandTree(focused)
	tree.Nodes[0].Nodes = append(tree.Nodes[0].Nodes, &swayipc.TreeNode{
		ID: 4, Type: "workspace", Name: "__i3_scratch", FloatingNodes: []*swayipc.TreeNode{scratchpadSibling},
	})
	client := &appCommandClient{tree: tree}
	deps.newSwayClient = func(string) swayRequester { return client }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"app", "register-focused", "--yes", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitOperation || !strings.Contains(stderr.String(), "ambiguous") {
		t.Fatalf("scratchpad ambiguity was not rejected: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(client.commands) != 0 {
		t.Fatalf("scratchpad ambiguity mutated Sway: commands=%q", client.commands)
	}
}

func TestAppRegisterFocusedDoesNotMoveHealthyStagingAnchorToSibling(t *testing.T) {
	deps := testDependencies(t)
	registered := sessionstate.Context{
		ID: testContextID, Label: "Example", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.App", FlatpakInstallation: sessionstate.FlatpakUser},
		App: &sessionstate.Application{
			Identity:      sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App", SandboxAppID: "org.example.App"},
			DesiredOpen:   true,
			RestorePolicy: sessionstate.ApplicationRestoreFollow,
		},
	}
	root, _ := deps.stateRoot()
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
		return sessionstate.AddContext(registry, registered)
	}); err != nil {
		t.Fatal(err)
	}
	mark, _ := registered.ID.Mark()
	appID, sandbox := "org.example.App", "org.example.App"
	focused := &swayipc.TreeNode{ID: 42, Type: "con", Focused: true, AppID: &appID, SandboxAppID: &sandbox}
	stagingAnchor := &swayipc.TreeNode{ID: 43, Type: "con", AppID: &appID, SandboxAppID: &sandbox, Marks: []string{mark}}
	tree := applicationCommandTree(focused)
	tree.Nodes[0].Nodes = append(tree.Nodes[0].Nodes, &swayipc.TreeNode{
		ID: 4, Type: "workspace", Name: sessionstate.RestoreStagingWorkspace, Nodes: []*swayipc.TreeNode{stagingAnchor},
	})
	client := &appCommandClient{tree: tree}
	deps.newSwayClient = func(string) swayRequester { return client }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"app", "register-focused", "--yes", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitSuccess || stderr.Len() != 0 || !strings.Contains(stdout.String(), "already registered") {
		t.Fatalf("healthy staging anchor status failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(client.commands) != 0 || !containsString(stagingAnchor.Marks, mark) || containsString(focused.Marks, mark) {
		t.Fatalf("healthy staging anchor moved: commands=%q staging=%q focused=%q", client.commands, stagingAnchor.Marks, focused.Marks)
	}
}

func TestAppRegisterFocusedRefusesMissingAnchorWithStagingSibling(t *testing.T) {
	deps := testDependencies(t)
	registered := sessionstate.Context{
		ID: testContextID, Label: "Example", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.App", FlatpakInstallation: sessionstate.FlatpakUser},
		App: &sessionstate.Application{
			Identity:      sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App", SandboxAppID: "org.example.App"},
			DesiredOpen:   true,
			RestorePolicy: sessionstate.ApplicationRestoreFollow,
		},
	}
	root, _ := deps.stateRoot()
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
		return sessionstate.AddContext(registry, registered)
	}); err != nil {
		t.Fatal(err)
	}
	appID, sandbox := "org.example.App", "org.example.App"
	focused := &swayipc.TreeNode{ID: 42, Type: "con", Focused: true, AppID: &appID, SandboxAppID: &sandbox}
	stagingSibling := &swayipc.TreeNode{ID: 43, Type: "con", AppID: &appID, SandboxAppID: &sandbox}
	tree := applicationCommandTree(focused)
	tree.Nodes[0].Nodes = append(tree.Nodes[0].Nodes, &swayipc.TreeNode{
		ID: 4, Type: "workspace", Name: sessionstate.RestoreStagingWorkspace, Nodes: []*swayipc.TreeNode{stagingSibling},
	})
	client := &appCommandClient{tree: tree}
	deps.newSwayClient = func(string) swayRequester { return client }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"app", "register-focused", "--yes", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitOperation || !strings.Contains(stderr.String(), "ambiguous") {
		t.Fatalf("staging ambiguity was not rejected: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(client.commands) != 0 {
		t.Fatalf("staging ambiguity mutated Sway: commands=%q", client.commands)
	}
}

func TestAppListJSONReturnsOnlyApplicationsSortedByContextID(t *testing.T) {
	deps := testDependencies(t)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	firstID := sessionstate.ContextID("11111111-1111-4111-8111-111111111111")
	secondID := sessionstate.ContextID("22222222-2222-4222-8222-222222222222")
	application := func(id sessionstate.ContextID, appID string) sessionstate.Context {
		return sessionstate.Context{
			ID: id, Label: appID, Provider: "desktop", State: sessionstate.ContextActive,
			Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: appID, FlatpakInstallation: sessionstate.FlatpakUser},
			App: &sessionstate.Application{
				Identity: sessionstate.ApplicationIdentity{
					Protocol: sessionstate.WindowWayland, WaylandAppID: appID, SandboxAppID: appID,
				},
				DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow,
			},
		}
	}
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{
		application(secondID, "org.example.Second"),
		{ID: "33333333-3333-4333-8333-333333333333", State: sessionstate.ContextActive, Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: "work", Cwd: "/work", Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}}},
		application(firstID, "org.example.First"),
	}}
	if err := sessionstate.RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"--json", "app", "list"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("app list failed code=%d stderr=%q", code, stderr.String())
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode app list: %v\n%s", err, stdout.String())
	}
	if result.Command != "app list" || len(result.Contexts) != 2 ||
		result.Contexts[0].ID != firstID || result.Contexts[1].ID != secondID {
		t.Fatalf("unexpected application list: %+v", result)
	}
}

func TestAppStatusAndRegisteredNoOpDoNotRequireDesktopCatalog(t *testing.T) {
	deps := testDependencies(t)
	registered := sessionstate.Context{
		ID: testContextID, Label: "Example", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.App", FlatpakInstallation: sessionstate.FlatpakUser},
		App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App", SandboxAppID: "org.example.App"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow},
	}
	root, _ := deps.stateRoot()
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error { return sessionstate.AddContext(registry, registered) }); err != nil {
		t.Fatal(err)
	}
	mark, _ := registered.ID.Mark()
	appID, sandbox := "org.example.App", "org.example.App"
	client := &appCommandClient{tree: applicationCommandTree(&swayipc.TreeNode{ID: 42, Type: "con", Focused: true, AppID: &appID, SandboxAppID: &sandbox, Marks: []string{mark}})}
	deps.newSwayClient = func(string) swayRequester { return client }
	deps.desktopCatalog = func() (sessionstate.DesktopCatalog, error) {
		return sessionstate.DesktopCatalog{}, errors.New("catalog unavailable")
	}
	for _, arguments := range [][]string{
		{"app", "status", "--socket", "/run/user/1000/sway.sock"},
		{"app", "register-focused", "--yes", "--socket", "/run/user/1000/sway.sock"},
	} {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if code := runWith(arguments, strings.NewReader(""), &stdout, &stderr, deps); code != exitSuccess {
			t.Fatalf("%v depended on desktop catalog: code=%d stderr=%q", arguments, code, stderr.String())
		}
	}
}

func TestAppRegisterWorkspaceDeduplicatesApplicationInternalWindows(t *testing.T) {
	deps := testDependencies(t)
	catalog := testDesktopCatalog(t, map[string]string{
		"org.example.App.desktop": "[Desktop Entry]\nType=Application\nName=Example App\nExec=/usr/bin/flatpak run org.example.App\nX-Flatpak=org.example.App\n",
	}, true)
	deps.desktopCatalog = func() (sessionstate.DesktopCatalog, error) { return catalog, nil }
	appID, sandbox := "org.example.App", "org.example.App"
	first := &swayipc.TreeNode{ID: 42, Type: "con", AppID: &appID, SandboxAppID: &sandbox}
	second := &swayipc.TreeNode{ID: 43, Type: "con", Focused: true, AppID: &appID, SandboxAppID: &sandbox}
	tree := applicationCommandTree(first)
	tree.Nodes[0].Nodes[0].Nodes = append(tree.Nodes[0].Nodes[0].Nodes, second)
	client := &appCommandClient{tree: tree}
	deps.newSwayClient = func(string) swayRequester { return client }
	var stderr bytes.Buffer
	code := runWith([]string{"app", "register-workspace", "--yes", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), os.Stdout, &stderr, deps)
	if code != exitSuccess {
		t.Fatalf("workspace registration failed: code=%d stderr=%q", code, stderr.String())
	}
	registry := loadTestRegistry(t, deps)
	if len(registry.Contexts) != 1 || len(client.commands) != 1 || !strings.Contains(client.commands[0], "[con_id=43]") {
		t.Fatalf("application-internal windows were not collapsed to the focused representative: registry=%+v commands=%q", registry, client.commands)
	}
}

func TestAppRegisterFocusedAmbiguityUsesOneTimeTypedSwaynagChoices(t *testing.T) {
	t.Setenv("SWAYSOCK", "/run/user/1000/sway.sock")
	deps := testDependencies(t)
	catalog := testDesktopCatalog(t, map[string]string{
		"first.desktop":  "[Desktop Entry]\nType=Application\nName=First\nExec=/usr/bin/true\nStartupWMClass=Shared\n",
		"second.desktop": "[Desktop Entry]\nType=Application\nName=Second\nExec=/usr/bin/true\nStartupWMClass=Shared\n",
	}, false)
	deps.desktopCatalog = func() (sessionstate.DesktopCatalog, error) { return catalog, nil }
	client := &appCommandClient{tree: applicationCommandTree(&swayipc.TreeNode{ID: 43, Type: "con", Focused: true, WindowProperties: swayipc.WindowProperties{Class: "Shared", Instance: "shared"}})}
	deps.newSwayClient = func(string) swayRequester { return client }
	ids := []sessionstate.ContextID{testContextID, "22222222-2222-4222-8222-222222222222"}
	deps.newContextID = func() (sessionstate.ContextID, error) {
		id := ids[0]
		ids = ids[1:]
		return id, nil
	}
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	deps.now = func() time.Time { return now }
	store := sessionstate.ApplicationOperationStore{RuntimeRoot: filepath.Join(t.TempDir(), "runtime"), Now: func() time.Time { return now }}
	deps.operationStore = func() (sessionstate.ApplicationOperationStore, error) { return store, nil }
	var message string
	var choices []sessionstate.ApprovalChoice
	deps.presentApproval = func(got string, gotChoices []sessionstate.ApprovalChoice) error {
		message = got
		choices = append([]sessionstate.ApprovalChoice(nil), gotChoices...)
		return nil
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"app", "register-focused", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitSuccess || len(choices) != 2 || !strings.Contains(message, "workspace 2:web") {
		t.Fatalf("ambiguous preview was not presented: code=%d message=%q choices=%+v stderr=%q", code, message, choices, stderr.String())
	}
	if len(loadRegistryOrEmpty(t, deps).Contexts) != 0 || len(client.commands) != 0 {
		t.Fatal("preview mutated registry or Sway")
	}

	stdout.Reset()
	stderr.Reset()
	code = runWith([]string{"--json", "app", "confirm", choices[0].Token}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitSuccess || len(loadTestRegistry(t, deps).Contexts) != 1 {
		t.Fatalf("confirmed choice failed code=%d stderr=%q", code, stderr.String())
	}
	if code = runWith([]string{"app", "confirm", choices[0].Token}, strings.NewReader(""), &stdout, &stderr, deps); code != exitOperation {
		t.Fatalf("approval token replay succeeded with code %d", code)
	}
}

func TestAppRegisterFocusedRollsBackApprovalWhenPresentationFails(t *testing.T) {
	deps := testDependencies(t)
	catalog := testDesktopCatalog(t, map[string]string{
		"org.example.App.desktop": "[Desktop Entry]\nType=Application\nName=Example App\nExec=/usr/bin/true\nStartupWMClass=Example\n",
	}, false)
	deps.desktopCatalog = func() (sessionstate.DesktopCatalog, error) { return catalog, nil }
	appID := "org.example.App"
	deps.newSwayClient = func(string) swayRequester {
		return &appCommandClient{tree: applicationCommandTree(&swayipc.TreeNode{ID: 42, Type: "con", Focused: true, AppID: &appID})}
	}
	now := time.Unix(5000, 0).UTC()
	store := sessionstate.ApplicationOperationStore{
		RuntimeRoot: t.TempDir(),
		Now:         func() time.Time { return now },
		Random:      bytes.NewReader(bytes.Repeat([]byte{6}, 16)),
	}
	deps.now = func() time.Time { return now }
	deps.operationStore = func() (sessionstate.ApplicationOperationStore, error) { return store, nil }
	deps.presentApproval = func(string, []sessionstate.ApprovalChoice) error { return errors.New("swaynag unavailable") }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"app", "register-focused", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitOperation {
		t.Fatalf("presentation failure exit code = %d stderr=%q", code, stderr.String())
	}
	active, err := store.Active()
	if err != nil || len(active) != 0 {
		t.Fatalf("failed presenter left a pending approval: %+v err=%v", active, err)
	}
}

func TestAppRegisterFocusedRollsBackEarlierChoicesWhenLaterStoreFails(t *testing.T) {
	deps := testDependencies(t)
	catalog := testDesktopCatalog(t, map[string]string{
		"first.desktop":  "[Desktop Entry]\nType=Application\nName=First\nExec=/usr/bin/true\nStartupWMClass=Shared\n",
		"second.desktop": "[Desktop Entry]\nType=Application\nName=Second\nExec=/usr/bin/true\nStartupWMClass=Shared\n",
	}, false)
	deps.desktopCatalog = func() (sessionstate.DesktopCatalog, error) { return catalog, nil }
	client := &appCommandClient{tree: applicationCommandTree(&swayipc.TreeNode{
		ID: 43, Type: "con", Focused: true,
		WindowProperties: swayipc.WindowProperties{Class: "Shared", Instance: "shared"},
	})}
	deps.newSwayClient = func(string) swayRequester { return client }
	now := time.Unix(5100, 0).UTC()
	store := sessionstate.ApplicationOperationStore{
		RuntimeRoot: t.TempDir(),
		Now:         func() time.Time { return now },
		Random:      bytes.NewReader(bytes.Repeat([]byte{7}, 16)),
	}
	deps.now = func() time.Time { return now }
	deps.operationStore = func() (sessionstate.ApplicationOperationStore, error) { return store, nil }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"app", "register-focused", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitOperation {
		t.Fatalf("partial store failure exit code = %d stderr=%q", code, stderr.String())
	}
	active, err := store.Active()
	if err != nil || len(active) != 0 {
		t.Fatalf("partial choice creation left an unreachable approval: %+v err=%v", active, err)
	}
}

func TestAppPinArchiveActivateAndForgetLifecycle(t *testing.T) {
	deps := testDependencies(t)
	registered := sessionstate.Context{
		ID: testContextID, Label: "Example", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.App", FlatpakInstallation: sessionstate.FlatpakUser},
		App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App", SandboxAppID: "org.example.App"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow},
	}
	root, _ := deps.stateRoot()
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error { return sessionstate.AddContext(registry, registered) }); err != nil {
		t.Fatal(err)
	}
	mark, _ := registered.ID.Mark()
	window := &swayipc.TreeNode{ID: 44, Type: "con", Marks: []string{mark}}
	client := &appCommandClient{tree: applicationCommandTree(window)}
	deps.newSwayClient = func(string) swayRequester { return client }
	for _, arguments := range [][]string{{"app", "pin", "Example"}, {"app", "archive", "Example"}, {"app", "activate", "Example"}, {"app", "unpin", "Example"}} {
		var stderr bytes.Buffer
		if code := runWith(arguments, strings.NewReader(""), os.Stdout, &stderr, deps); code != exitSuccess {
			t.Fatalf("%v failed code=%d stderr=%q", arguments, code, stderr.String())
		}
	}
	var stderr bytes.Buffer
	if code := runWith([]string{"app", "forget", "--yes", "--socket", "/run/user/1000/sway.sock", "Example"}, strings.NewReader(""), os.Stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("forget failed code=%d stderr=%q", code, stderr.String())
	}
	if len(loadRegistryOrEmpty(t, deps).Contexts) != 0 || len(client.commands) == 0 || !strings.Contains(client.commands[len(client.commands)-1], "unmark "+mark) {
		t.Fatalf("forget did not remove registry and live mark: registry=%+v commands=%q", loadRegistryOrEmpty(t, deps), client.commands)
	}
}

func TestAppRebindFocusedShowsAndAppliesExactNewIdentity(t *testing.T) {
	deps := testDependencies(t)
	catalog := testDesktopCatalog(t, map[string]string{
		"org.example.Old.desktop": "[Desktop Entry]\nType=Application\nName=Old App\nExec=/usr/bin/flatpak run org.example.Old\nX-Flatpak=org.example.Old\n",
		"org.example.New.desktop": "[Desktop Entry]\nType=Application\nName=New App\nExec=/usr/bin/flatpak run org.example.New\nX-Flatpak=org.example.New\n",
	}, true)
	deps.desktopCatalog = func() (sessionstate.DesktopCatalog, error) { return catalog, nil }
	registered := sessionstate.Context{
		ID: testContextID, Label: "Old App", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.Old", FlatpakInstallation: sessionstate.FlatpakUser},
		App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.Old", SandboxAppID: "org.example.Old"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestorePinned},
	}
	root, _ := deps.stateRoot()
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error { return sessionstate.AddContext(registry, registered) }); err != nil {
		t.Fatal(err)
	}
	oldAppID, newAppID := "org.example.Old", "org.example.New"
	oldSandbox, newSandbox := oldAppID, newAppID
	mark, _ := registered.ID.Mark()
	oldWindow := &swayipc.TreeNode{ID: 51, Type: "con", AppID: &oldAppID, SandboxAppID: &oldSandbox, Marks: []string{mark}}
	newWindow := &swayipc.TreeNode{ID: 52, Type: "con", Focused: true, AppID: &newAppID, SandboxAppID: &newSandbox}
	tree := applicationCommandTree(oldWindow)
	tree.Nodes[0].Nodes[0].Nodes = append(tree.Nodes[0].Nodes[0].Nodes, newWindow)
	client := &appCommandClient{tree: tree}
	deps.newSwayClient = func(string) swayRequester { return client }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"--json", "app", "rebind-focused", "--yes", "--socket", "/run/user/1000/sway.sock", "Old App"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("rebind failed code=%d stderr=%q", code, stderr.String())
	}
	registry := loadTestRegistry(t, deps)
	if len(registry.Contexts) != 1 || registry.Contexts[0].ID != registered.ID || registry.Contexts[0].Launcher.FlatpakID != newAppID || registry.Contexts[0].App.RestorePolicy != sessionstate.ApplicationRestorePinned {
		t.Fatalf("rebind did not preserve lifecycle policy and replace identity: %+v", registry)
	}
	if containsString(oldWindow.Marks, mark) || !containsString(newWindow.Marks, mark) {
		t.Fatalf("rebind did not transfer stable mark: old=%q new=%q commands=%q", oldWindow.Marks, newWindow.Marks, client.commands)
	}
}

func testDesktopCatalog(t *testing.T, files map[string]string, flatpak bool) sessionstate.DesktopCatalog {
	t.Helper()
	root := filepath.Join(t.TempDir(), "applications")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	installation := sessionstate.FlatpakInstallation("")
	if flatpak {
		installation = sessionstate.FlatpakUser
	}
	catalog, err := sessionstate.LoadDesktopCatalog([]sessionstate.DesktopSearchDirectory{{Path: root, Origin: sessionstate.DesktopEntryUser, FlatpakInstallation: installation}})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func applicationCommandTree(window *swayipc.TreeNode) *swayipc.TreeNode {
	return &swayipc.TreeNode{ID: 1, Type: "root", Nodes: []*swayipc.TreeNode{{
		ID: 2, Type: "output", Nodes: []*swayipc.TreeNode{{
			ID: 3, Type: "workspace", Name: "2:web", Nodes: []*swayipc.TreeNode{window},
		}},
	}}}
}

type appCommandClient struct {
	tree         *swayipc.TreeNode
	commands     []string
	getTreeCalls int
	afterGetTree func(int)
}

func (client *appCommandClient) Request(messageType swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	switch messageType {
	case swayipc.GetTree:
		data, err := json.Marshal(client.tree)
		client.getTreeCalls++
		if client.afterGetTree != nil {
			client.afterGetTree(client.getTreeCalls)
		}
		return swayipc.Message{Type: swayipc.GetTree, Payload: data}, err
	case swayipc.RunCommand:
		command := string(payload)
		client.commands = append(client.commands, command)
		var containerID int64
		if _, err := fmt.Sscanf(command, "[con_id=%d]", &containerID); err != nil {
			return swayipc.Message{}, err
		}
		node := findTestNode(client.tree, containerID)
		if node == nil {
			return swayipc.Message{Type: swayipc.RunCommand, Payload: []byte(`[{"success":false,"error":"missing"}]`)}, nil
		}
		fields := strings.Fields(command)
		mark := fields[len(fields)-1]
		if strings.Contains(command, "mark --add") && !containsString(node.Marks, mark) {
			node.Marks = append(node.Marks, mark)
		}
		if strings.Contains(command, " unmark ") {
			filtered := node.Marks[:0]
			for _, current := range node.Marks {
				if current != mark {
					filtered = append(filtered, current)
				}
			}
			node.Marks = filtered
		}
		return swayipc.Message{Type: swayipc.RunCommand, Payload: []byte(`[{"success":true}]`)}, nil
	default:
		return swayipc.Message{}, errors.New("unexpected Sway request")
	}
}

func (client *appCommandClient) RequestContext(ctx context.Context, messageType swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if err := ctx.Err(); err != nil {
		return swayipc.Message{}, err
	}
	return client.Request(messageType, payload)
}

func (*appCommandClient) Close() {}

func findTestNode(node *swayipc.TreeNode, id int64) *swayipc.TreeNode {
	if node == nil {
		return nil
	}
	if node.ID == id {
		return node
	}
	for _, children := range [][]*swayipc.TreeNode{node.Nodes, node.FloatingNodes} {
		for _, child := range children {
			if found := findTestNode(child, id); found != nil {
				return found
			}
		}
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func loadRegistryOrEmpty(t *testing.T, deps dependencies) sessionstate.Registry {
	t.Helper()
	root, _ := deps.stateRoot()
	registry, err := loadRegistry(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}
