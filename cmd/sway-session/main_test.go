package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marang/sway-title-animator/internal/codexreport"
	sessionstate "github.com/marang/sway-title-animator/internal/session"
	"github.com/marang/sway-title-animator/internal/sessionrequest"
	"github.com/marang/sway-title-animator/internal/swayipc"
)

const testContextID = sessionstate.ContextID("11111111-1111-4111-8111-111111111111")

func TestHelpListsImplementedCommandContract(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"--help"}, &stdout, &stderr)

	if exitCode != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("unexpected help result code=%d stderr=%q", exitCode, stderr.String())
	}
	for _, expected := range []string{"register --session <name>", "restore [--socket <path>] [context]", "app <subcommand> [options]", "daemon [--socket <path>]", "broker [--socket <path>]", "request-start --session <name> --workspace <number>", "purge [--yes] <context>", "completion contexts <command>", "3  Operational failure"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("help does not contain %q:\n%s", expected, stdout.String())
		}
	}
}

func TestAppHelpDocumentsMachineReadableInventoryAndIndicatorStates(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"app", "--help"}, &stdout, &stderr)

	if exitCode != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("app help failed code=%d stderr=%q", exitCode, stderr.String())
	}
	for _, expected := range []string{
		"sway-session --json app list",
		"○ unregistered",
		"◔ pending",
		"● registered/follow",
		"▲ pinned/autostart",
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("app help missing %q:\n%s", expected, stdout.String())
		}
	}
}

func TestCompletionHelpDocumentsStableReadOnlyRecordContract(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"completion", "--help"}, &stdout, &stderr)

	if exitCode != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("completion help failed code=%d stderr=%q", exitCode, stderr.String())
	}
	for _, expected := range []string{
		"archive, activate, restore, restore-active, purge, app-forget",
		"canonical UUID",
		"tab-separated line",
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("completion help missing %q:\n%s", expected, stdout.String())
		}
	}
}

func TestDaemonRunsIndependentlyOfTitleAnimator(t *testing.T) {
	deps := testDependencies(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := 0
	deps.runDaemon = func(got context.Context, socket string, report func(error)) error {
		called++
		if socket != "/run/user/1000/sway.sock" {
			t.Fatalf("unexpected Sway socket %q", socket)
		}
		if !errors.Is(got.Err(), context.Canceled) {
			t.Fatalf("daemon did not receive canceled context: %v", got.Err())
		}
		if report == nil {
			t.Fatal("daemon error reporter is nil")
		}
		return nil
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWithContext(ctx, []string{"--json", "daemon", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitSuccess || called != 1 || stderr.Len() != 0 {
		t.Fatalf("daemon failed code=%d called=%d stderr=%q", code, called, stderr.String())
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Command != "daemon" || len(result.Contexts) != 0 {
		t.Fatalf("unexpected daemon result: %+v", result)
	}
}

func TestBrokerRunsUntilContextCancellation(t *testing.T) {
	deps := testDependencies(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := 0
	deps.runBroker = func(got context.Context, socket string, report func(error)) error {
		called++
		if socket != "/run/user/1000/sway.sock" {
			t.Fatalf("unexpected Sway socket %q", socket)
		}
		if !errors.Is(got.Err(), context.Canceled) {
			t.Fatalf("broker did not receive canceled context: %v", got.Err())
		}
		if report == nil {
			t.Fatal("broker error reporter is nil")
		}
		return nil
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWithContext(ctx, []string{"--json", "broker", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitSuccess || called != 1 || stderr.Len() != 0 {
		t.Fatalf("broker failed code=%d called=%d stderr=%q", code, called, stderr.String())
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Command != "broker" || len(result.Contexts) != 0 {
		t.Fatalf("unexpected broker result: %+v", result)
	}
}

func TestBrokerRejectsRelativeSwaySocketBeforeRunner(t *testing.T) {
	deps := testDependencies(t)
	deps.runBroker = func(context.Context, string, func(error)) error {
		t.Fatal("broker runner called with relative Sway socket")
		return nil
	}
	var stderr bytes.Buffer
	code := runWith([]string{"broker", "--socket", "relative.sock"}, strings.NewReader(""), io.Discard, &stderr, deps)
	if code != exitOperation || !strings.Contains(stderr.String(), "absolute Sway IPC socket") {
		t.Fatalf("unexpected result code=%d stderr=%q", code, stderr.String())
	}
}

func TestRequestStartUsesOnlyTypedBrokerDependency(t *testing.T) {
	deps := testDependencies(t)
	project, err := deps.workingDir()
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	deps.requestStart = func(_ context.Context, request sessionrequest.Request) (sessionrequest.Response, error) {
		called++
		want := sessionrequest.Request{Version: sessionrequest.ProtocolVersion, Session: "reboot-e2e", Cwd: project, Label: "REBOOT-E2E", Provider: "local", Workspace: 7}
		if request != want {
			t.Fatalf("unexpected request: got=%+v want=%+v", request, want)
		}
		contextValue := sessionstate.Context{ID: testContextID, Label: request.Label, Provider: request.Provider, State: sessionstate.ContextActive, Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: request.Session, Cwd: request.Cwd}}
		return sessionrequest.Response{Version: sessionrequest.ProtocolVersion, OK: true, Context: &contextValue, Workspace: 7, Created: true}, nil
	}
	deps.stateRoot = func() (string, error) { return "", errors.New("request-start must not read state directly") }
	deps.newSwayClient = func(string) swayRequester { t.Fatal("request-start must not open Sway IPC"); return nil }
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWith([]string{"--json", "request-start", "--session", "reboot-e2e", "--cwd", project, "--label", "REBOOT-E2E", "--provider", "local", "--workspace", "7"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitSuccess || stderr.Len() != 0 || called != 1 {
		t.Fatalf("request-start failed code=%d called=%d stderr=%q", code, called, stderr.String())
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Command != "request-start" || result.Workspace != 7 || !result.Created || len(result.Contexts) != 1 || result.Contexts[0].ID != testContextID {
		t.Fatalf("unexpected request-start result: %+v", result)
	}
}

func TestCommandHelpWorksAfterCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"archive", "--help"}, &stdout, &stderr)

	if exitCode != exitSuccess || stderr.Len() != 0 || !strings.Contains(stdout.String(), "archive <context>") {
		t.Fatalf("unexpected help result code=%d stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
}

func TestInvalidArityHasActionableTextDiagnostic(t *testing.T) {
	deps := testDependencies(t)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWith([]string{"purge"}, strings.NewReader(""), &stdout, &stderr, deps)

	if exitCode != exitUsage || stdout.Len() != 0 {
		t.Fatalf("unexpected result code=%d stdout=%q", exitCode, stdout.String())
	}
	for _, expected := range []string{"sway-session: error:", "purge requires exactly one context", "Usage: sway-session purge [--yes] <context>"} {
		if !strings.Contains(stderr.String(), expected) {
			t.Fatalf("diagnostic does not contain %q: %s", expected, stderr.String())
		}
	}
}

func TestRegisterListArchiveAndActivateLifecycle(t *testing.T) {
	deps := testDependencies(t)
	commands := [][]string{
		{"register", "--session", "lab-80", "--label", "LAB-80", "--json"},
		{"archive", "LAB-80", "--json"},
		{"activate", string(testContextID), "--json"},
		{"list", "--json"},
	}
	for _, arguments := range commands {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if code := runWith(arguments, strings.NewReader(""), &stdout, &stderr, deps); code != exitSuccess {
			t.Fatalf("%v failed code=%d stderr=%s", arguments, code, stderr.String())
		}
		var result commandResult
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatalf("decode %v result: %v\n%s", arguments, err, stdout.String())
		}
		if len(result.Contexts) != 1 || result.Contexts[0].ID != testContextID {
			t.Fatalf("unexpected %v result: %+v", arguments, result)
		}
	}

	registry := loadTestRegistry(t, deps)
	if registry.Contexts[0].State != sessionstate.ContextActive || registry.Contexts[0].Launcher.Session != "lab-80" {
		t.Fatalf("unexpected final registry: %+v", registry)
	}
}

func TestRegisterRejectsDuplicateHerdrSession(t *testing.T) {
	deps := testDependencies(t)
	registerTestContext(t, deps)
	deps.newContextID = func() (sessionstate.ContextID, error) {
		return "22222222-2222-4222-8222-222222222222", nil
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"register", "--session", "lab-80"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitOperation || !strings.Contains(stderr.String(), "already registered") {
		t.Fatalf("duplicate launcher was not rejected: code=%d stderr=%q", code, stderr.String())
	}
}

func TestPurgeRequiresExplicitNonInteractiveConfirmation(t *testing.T) {
	deps := testDependencies(t)
	registerTestContext(t, deps)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"purge", "LAB-80"}, strings.NewReader(string(testContextID)+"\n"), &stdout, &stderr, deps)

	if code != exitOperation || !strings.Contains(stderr.String(), "--yes") {
		t.Fatalf("non-interactive purge was not rejected: code=%d stderr=%q", code, stderr.String())
	}
	if len(loadTestRegistry(t, deps).Contexts) != 1 {
		t.Fatal("rejected purge modified registry")
	}
}

func TestPurgeStopsDeletesAndThenRemovesRegistryEntry(t *testing.T) {
	deps := testDependencies(t)
	registerTestContext(t, deps)
	paths, _ := deps.herdrPaths()
	sessionPath := filepath.Join(paths.Root, "sessions", "lab-80")
	if err := os.MkdirAll(sessionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &recordingHerdrRunner{responses: []string{
		herdrList(paths.Root, true), `{}`, herdrList(paths.Root, false), `{}`, `{"sessions":[]}`,
	}, deletePath: sessionPath}
	deps.herdrRunner = runner
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"purge", "--yes", "LAB-80", "--json"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("purge failed code=%d stderr=%q", code, stderr.String())
	}
	if len(loadTestRegistry(t, deps).Contexts) != 0 {
		t.Fatal("purged context remains in registry")
	}
	wantCalls := [][]string{
		{"session", "list", "--json"},
		{"session", "stop", "lab-80", "--json"},
		{"session", "list", "--json"},
		{"session", "delete", "lab-80", "--json"},
		{"session", "list", "--json"},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("unexpected Herdr calls: got=%v want=%v", runner.calls, wantCalls)
	}
}

func TestPurgeAcceptsFullIDFromInteractiveTerminal(t *testing.T) {
	deps := testDependencies(t)
	registerTestContext(t, deps)
	deps.stdinTerminal = func() bool { return true }
	deps.herdrRunner = &recordingHerdrRunner{responses: []string{`{"sessions":[]}`}}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"purge", "LAB-80"}, strings.NewReader(string(testContextID)+"\n"), &stdout, &stderr, deps)

	if code != exitSuccess || len(loadTestRegistry(t, deps).Contexts) != 0 {
		t.Fatalf("interactive purge failed code=%d stderr=%q", code, stderr.String())
	}
}

func TestPurgeNeverStartedContextWithoutHerdrInstallation(t *testing.T) {
	deps := testDependencies(t)
	registerTestContext(t, deps)
	paths, _ := deps.herdrPaths()
	if err := os.Remove(paths.Root); err != nil {
		t.Fatal(err)
	}
	deps.resolveProgram = func(string) (string, error) { return "", errors.New("must not resolve Herdr") }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"purge", "--yes", "LAB-80"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitSuccess || len(loadTestRegistry(t, deps).Contexts) != 0 {
		t.Fatalf("purge without Herdr state failed code=%d stderr=%q", code, stderr.String())
	}
}

func TestRestoreReusesExistingManagedWindowWithoutLauncherChecks(t *testing.T) {
	deps := testDependencies(t)
	registered := registerTestContext(t, deps)
	deps.validateHistory = func(sessionstate.HerdrPaths) error { return errors.New("must not be called") }
	deps.resolveProgram = func(string) (string, error) { return "", errors.New("must not be called") }
	deps.newSwayClient = func(string) swayRequester {
		return &fakeSwayClient{trees: []*swayipc.TreeNode{treeWithContexts(registered.ID)}}
	}
	starter := &recordingStarter{}
	deps.processStarter = starter
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"restore", "--socket", "/run/user/1000/sway.sock", "--json"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitSuccess || stderr.Len() != 0 || len(starter.calls) != 0 {
		t.Fatalf("idempotent restore failed code=%d stderr=%q starts=%v", code, stderr.String(), starter.calls)
	}
}

func TestRestoreLaunchesMissingContextWithTypedArgumentsAndWaitsForMapping(t *testing.T) {
	deps := testDependencies(t)
	registered := registerTestContext(t, deps)
	client := &fakeSwayClient{trees: []*swayipc.TreeNode{treeWithContexts(), treeWithContexts(registered.ID)}}
	deps.newSwayClient = func(string) swayRequester { return client }
	starter := &recordingStarter{}
	deps.processStarter = starter
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"restore", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("restore failed code=%d stderr=%q", code, stderr.String())
	}
	if len(starter.calls) != 1 {
		t.Fatalf("expected one launch, got %v", starter.calls)
	}
	want := processCall{
		name: "/trusted/alacritty",
		arguments: []string{
			"--class=sway-session." + string(testContextID),
			"--working-directory=" + registered.Launcher.Cwd, "--title=LAB-80",
			"-e", "/trusted/herdr", "--session", "lab-80",
		},
		environment: []string{"SWAY_SESSION_CONTEXT_ID=" + string(testContextID)},
	}
	if !reflect.DeepEqual(starter.calls[0], want) {
		t.Fatalf("launcher argv differs:\ngot  %q\nwant %q", starter.calls[0], want)
	}
}

func TestRestorePendingProcessPreventsDuplicateLaunch(t *testing.T) {
	deps := testDependencies(t)
	registered := registerTestContext(t, deps)
	deps.findPending = func(string, sessionstate.Context, string, string) ([]int, error) { return []int{1234}, nil }
	deps.newSwayClient = func(string) swayRequester {
		return &fakeSwayClient{trees: []*swayipc.TreeNode{treeWithContexts(), treeWithContexts(registered.ID)}}
	}
	starter := &recordingStarter{}
	deps.processStarter = starter
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"restore", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitSuccess || len(starter.calls) != 0 {
		t.Fatalf("pending launch was duplicated: code=%d starts=%v stderr=%q", code, starter.calls, stderr.String())
	}
}

func TestBrokerRestoreRequiresContextToRemainActive(t *testing.T) {
	deps := testDependencies(t)
	registered := registerTestContext(t, deps)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
		_, err := sessionstate.SetContextState(registry, string(registered.ID), sessionstate.ContextArchived)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	deps.newSwayClient = func(string) swayRequester {
		t.Fatal("archived broker target reached Sway")
		return nil
	}
	var stderr bytes.Buffer

	code := runWith([]string{"restore", "--require-active", "--socket", "/run/user/1000/sway.sock", string(registered.ID)}, strings.NewReader(""), io.Discard, &stderr, deps)

	if code != exitOperation || !strings.Contains(stderr.String(), "archived") {
		t.Fatalf("archived broker target was not rejected: code=%d stderr=%q", code, stderr.String())
	}
}

func TestExplicitManualRestoreStillAllowsArchivedContext(t *testing.T) {
	contextValue := sessionstate.Context{ID: testContextID, State: sessionstate.ContextArchived, Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr}}
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue}}

	targets, err := restoreTargets(registry, string(contextValue.ID), false)

	if err != nil || len(targets) != 1 || targets[0].ID != contextValue.ID {
		t.Fatalf("manual archived restore changed semantics: targets=%+v err=%v", targets, err)
	}
}

func TestAutomaticOneShotRestoreLeavesDesktopContextsToDaemon(t *testing.T) {
	herdr := sessionstate.Context{ID: testContextID, State: sessionstate.ContextActive, Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr}}
	desktop := sessionstate.Context{ID: "22222222-2222-4222-8222-222222222222", State: sessionstate.ContextActive, Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherDesktop}}
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{herdr, desktop}}
	targets, err := restoreTargets(registry, "", false)
	if err != nil || len(targets) != 1 || targets[0].ID != herdr.ID {
		t.Fatalf("automatic restore sent desktop context through Herdr: targets=%+v err=%v", targets, err)
	}
	if _, err := restoreTargets(registry, string(desktop.ID), false); err == nil || !strings.Contains(err.Error(), "session daemon") {
		t.Fatalf("explicit desktop restore did not preserve daemon ownership: %v", err)
	}
}

func TestExplicitDesktopRestoreQueuesDesiredOpenForDaemon(t *testing.T) {
	deps := testDependencies(t)
	context := sessionstate.Context{
		ID: testContextID, State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.App", FlatpakInstallation: sessionstate.FlatpakUser},
		App: &sessionstate.Application{
			Identity:    sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App", SandboxAppID: "org.example.App"},
			DesiredOpen: false, RestorePolicy: sessionstate.ApplicationRestoreFollow,
		},
	}
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
		return sessionstate.AddContext(registry, context)
	}); err != nil {
		t.Fatal(err)
	}
	deps.newSwayClient = func(string) swayRequester {
		t.Fatal("queued desktop restore bypassed daemon ownership")
		return nil
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"restore", "--socket", "/run/user/1000/sway.sock", string(context.ID)}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitSuccess || !strings.Contains(stdout.String(), "queued") || stderr.Len() != 0 {
		t.Fatalf("desktop restore was not queued: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var registry sessionstate.Registry
	if err := sessionstate.RegistryFile(root).LoadInto(&registry); err != nil {
		t.Fatal(err)
	}
	if !registry.Contexts[0].App.DesiredOpen {
		t.Fatal("queued desktop restore did not persist desired-open state")
	}
}

func TestConcurrentRestoreInvocationsLaunchContextOnce(t *testing.T) {
	deps := testDependencies(t)
	registerTestContext(t, deps)
	mapped := &atomic.Bool{}
	starter := &mappingStarter{mapped: mapped}
	deps.processStarter = starter
	deps.newSwayClient = func(string) swayRequester { return &sharedTreeClient{mapped: mapped} }
	start := make(chan struct{})
	results := make(chan int, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			results <- runWith([]string{"restore", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	for code := range results {
		if code != exitSuccess {
			t.Fatalf("concurrent restore failed with exit %d", code)
		}
	}
	if starter.starts.Load() != 1 {
		t.Fatalf("concurrent restores launched %d processes", starter.starts.Load())
	}
}

func TestRestoreDuplicateContextDoesNotPreventIndependentLaunch(t *testing.T) {
	deps := testDependencies(t)
	first := registerTestContext(t, deps)
	second := sessionstate.Context{
		ID: "22222222-2222-4222-8222-222222222222", Label: "LAB-81", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: "lab-81", Cwd: first.Launcher.Cwd},
	}
	root, _ := deps.stateRoot()
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
		return sessionstate.AddContext(registry, second)
	}); err != nil {
		t.Fatal(err)
	}
	client := &fakeSwayClient{trees: []*swayipc.TreeNode{
		treeWithContexts(first.ID, first.ID),
		treeWithContexts(first.ID, first.ID, second.ID),
	}}
	deps.newSwayClient = func(string) swayRequester { return client }
	starter := &recordingStarter{}
	deps.processStarter = starter
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"restore", "--socket", "/run/user/1000/sway.sock", "--json"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitOperation || len(starter.calls) != 1 {
		t.Fatalf("independent launch did not continue: code=%d starts=%v stderr=%q", code, starter.calls, stderr.String())
	}
	if !strings.Contains(strings.Join(starter.calls[0].arguments, "\x00"), "lab-81") || !strings.Contains(stderr.String(), "LAB-80") {
		t.Fatalf("wrong context launched or diagnosed: starts=%v stderr=%q", starter.calls, stderr.String())
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || len(result.Contexts) != 1 || result.Contexts[0].ID != second.ID {
		t.Fatalf("partial success was not reported: result=%+v err=%v output=%q", result, err, stdout.String())
	}
}

func TestJSONRestoreFailuresUseOneDiagnosticEnvelope(t *testing.T) {
	deps := testDependencies(t)
	registered := registerTestContext(t, deps)
	deps.newSwayClient = func(string) swayRequester {
		return &fakeSwayClient{trees: []*swayipc.TreeNode{treeWithContexts()}}
	}
	deps.validateHistory = func(sessionstate.HerdrPaths) error { return errors.New("pane history disabled") }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"restore", "--socket", "/run/user/1000/sway.sock", string(registered.ID), "--json"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitOperation || stdout.Len() != 0 {
		t.Fatalf("unexpected failure result code=%d stdout=%q", code, stdout.String())
	}
	var envelope struct {
		Diagnostics []struct {
			Code string `json:"code"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &envelope); err != nil {
		t.Fatalf("decode diagnostic envelope: %v\n%s", err, stderr.String())
	}
	if len(envelope.Diagnostics) != 1 || envelope.Diagnostics[0].Code != "pane_history" {
		t.Fatalf("unexpected diagnostics: %+v", envelope.Diagnostics)
	}
}

func TestReportCodexSessionUsesOnlyHookBoundary(t *testing.T) {
	deps := testDependencies(t)
	called := false
	deps.reportCodexHook = func(_ context.Context, input io.Reader, getenv func(string) string) error {
		called = true
		data, err := io.ReadAll(input)
		if err != nil || string(data) != `{"hook_event_name":"SessionStart"}` || getenv("PATH") != os.Getenv("PATH") {
			t.Fatalf("unexpected hook boundary input=%q err=%v", data, err)
		}
		return nil
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWith([]string{"report-codex-session"}, strings.NewReader(`{"hook_event_name":"SessionStart"}`), &stdout, &stderr, deps)
	if code != exitSuccess || !called || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("Codex report command failed code=%d called=%v stdout=%q stderr=%q", code, called, stdout.String(), stderr.String())
	}
}

func TestReportCodexSessionDoesNotFailOutsideManagedHerdr(t *testing.T) {
	deps := testDependencies(t)
	deps.reportCodexHook = func(context.Context, io.Reader, func(string) string) error {
		return codexreport.ErrNotManagedSession
	}
	var stderr bytes.Buffer
	code := runWith([]string{"report-codex-session"}, strings.NewReader(`{}`), io.Discard, &stderr, deps)
	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("unmanaged hook should be a silent no-op: code=%d stderr=%q", code, stderr.String())
	}
}

func TestUnknownCommandUsesUsageExitCode(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"launch"}, &stdout, &stderr)

	if exitCode != exitUsage || !strings.Contains(stderr.String(), "unknown command \"launch\"") {
		t.Fatalf("unexpected result code=%d stderr=%q", exitCode, stderr.String())
	}
}

func testDependencies(t *testing.T) dependencies {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "state", "sway-session")
	project := filepath.Join(base, "project")
	herdrRoot := filepath.Join(base, "config", "herdr")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(herdrRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	return dependencies{
		stateRoot:    func() (string, error) { return root, nil },
		workingDir:   func() (string, error) { return project, nil },
		newContextID: func() (sessionstate.ContextID, error) { return testContextID, nil },
		herdrPaths: func() (sessionstate.HerdrPaths, error) {
			return sessionstate.HerdrPaths{Root: herdrRoot, ConfigFile: filepath.Join(herdrRoot, "config.toml")}, nil
		},
		validateHistory: func(sessionstate.HerdrPaths) error { return nil },
		resolveProgram: func(name string) (string, error) {
			return "/trusted/" + name, nil
		},
		resolveSystem: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		desktopCatalog: func() (sessionstate.DesktopCatalog, error) {
			return sessionstate.LoadDesktopCatalog(nil)
		},
		operationStore: func() (sessionstate.ApplicationOperationStore, error) {
			return sessionstate.ApplicationOperationStore{RuntimeRoot: filepath.Join(base, "runtime")}, nil
		},
		presentApproval: func(string, []sessionstate.ApprovalChoice) error { return nil },
		verifyFlatpak:   func(sessionstate.Launcher) error { return nil },
		newSwayClient:   func(string) swayRequester { return &fakeSwayClient{trees: []*swayipc.TreeNode{treeWithContexts()}} },
		processStarter:  &recordingStarter{},
		herdrRunner:     &recordingHerdrRunner{},
		findPending:     func(string, sessionstate.Context, string, string) ([]int, error) { return nil, nil },
		now:             time.Now,
		sleep:           func(time.Duration) {},
		settleTimeout:   time.Second,
		stdinTerminal:   func() bool { return false },
	}
}

func registerTestContext(t *testing.T, deps dependencies) sessionstate.Context {
	t.Helper()
	result, commandFailure := executeRegister([]string{"--session", "lab-80", "--label", "LAB-80"}, deps)
	if commandFailure != nil {
		t.Fatalf("register fixture: %+v", commandFailure)
	}
	return result.Contexts[0]
}

func loadTestRegistry(t *testing.T, deps dependencies) sessionstate.Registry {
	t.Helper()
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	var registry sessionstate.Registry
	if err := sessionstate.RegistryFile(root).LoadInto(&registry); err != nil {
		t.Fatal(err)
	}
	return registry
}

type processCall struct {
	name        string
	arguments   []string
	environment []string
}

type recordingStarter struct {
	calls []processCall
	err   error
}

func (starter *recordingStarter) Start(spec sessionstate.ProcessSpec) error {
	starter.calls = append(starter.calls, processCall{
		name:        spec.Name,
		arguments:   append([]string(nil), spec.Arguments...),
		environment: append([]string(nil), spec.Environment...),
	})
	return starter.err
}

type fakeSwayClient struct {
	trees  []*swayipc.TreeNode
	index  int
	err    error
	closed bool
}

func (client *fakeSwayClient) Request(messageType swayipc.MessageType, _ []byte) (swayipc.Message, error) {
	if client.err != nil {
		return swayipc.Message{}, client.err
	}
	if messageType != swayipc.GetTree || len(client.trees) == 0 {
		return swayipc.Message{}, errors.New("unexpected Sway request")
	}
	index := client.index
	if index >= len(client.trees) {
		index = len(client.trees) - 1
	}
	client.index++
	payload, err := json.Marshal(client.trees[index])
	return swayipc.Message{Type: swayipc.GetTree, Payload: payload}, err
}

func (client *fakeSwayClient) Close() { client.closed = true }

type sharedTreeClient struct {
	mapped *atomic.Bool
}

func (client *sharedTreeClient) Request(messageType swayipc.MessageType, _ []byte) (swayipc.Message, error) {
	if messageType != swayipc.GetTree {
		return swayipc.Message{}, errors.New("unexpected Sway request")
	}
	var tree *swayipc.TreeNode
	if client.mapped.Load() {
		tree = treeWithContexts(testContextID)
	} else {
		tree = treeWithContexts()
	}
	payload, err := json.Marshal(tree)
	return swayipc.Message{Type: swayipc.GetTree, Payload: payload}, err
}

func (*sharedTreeClient) Close() {}

type mappingStarter struct {
	mapped *atomic.Bool
	starts atomic.Int32
}

func (starter *mappingStarter) Start(sessionstate.ProcessSpec) error {
	starter.starts.Add(1)
	starter.mapped.Store(true)
	return nil
}

func treeWithContexts(ids ...sessionstate.ContextID) *swayipc.TreeNode {
	workspace := &swayipc.TreeNode{ID: 3, Type: "workspace", Name: "1", Nodes: []*swayipc.TreeNode{}, FloatingNodes: []*swayipc.TreeNode{}}
	for index, id := range ids {
		appID, _ := id.AppID()
		workspace.Nodes = append(workspace.Nodes, &swayipc.TreeNode{ID: int64(10 + index), Type: "con", AppID: &appID, Nodes: []*swayipc.TreeNode{}, FloatingNodes: []*swayipc.TreeNode{}})
	}
	return &swayipc.TreeNode{
		ID: 1, Type: "root", Nodes: []*swayipc.TreeNode{{
			ID: 2, Type: "output", Nodes: []*swayipc.TreeNode{workspace}, FloatingNodes: []*swayipc.TreeNode{},
		}}, FloatingNodes: []*swayipc.TreeNode{},
	}
}

type recordingHerdrRunner struct {
	responses  []string
	calls      [][]string
	deletePath string
}

func (runner *recordingHerdrRunner) CombinedOutput(_ context.Context, _ string, arguments ...string) ([]byte, error) {
	runner.calls = append(runner.calls, append([]string(nil), arguments...))
	if len(runner.responses) == 0 {
		return nil, errors.New("unexpected Herdr command")
	}
	response := runner.responses[0]
	runner.responses = runner.responses[1:]
	if len(arguments) >= 2 && arguments[0] == "session" && arguments[1] == "delete" && runner.deletePath != "" {
		if err := os.RemoveAll(runner.deletePath); err != nil {
			return nil, err
		}
	}
	return []byte(response), nil
}

func herdrList(root string, running bool) string {
	data, _ := json.Marshal(map[string]any{"sessions": []map[string]any{{
		"name": "lab-80", "default": false, "running": running,
		"socket_path": filepath.Join(root, "sessions", "lab-80", "herdr.sock"),
		"session_dir": filepath.Join(root, "sessions", "lab-80"),
	}}})
	return string(data)
}
