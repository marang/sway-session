package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/sessionrequest"
	"github.com/marang/sway-session/internal/statefile"
	"github.com/marang/sway-session/internal/swayipc"
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

func TestNoColorDoesNotChangeNonTerminalCommandOutput(t *testing.T) {
	previous, wasSet := os.LookupEnv("NO_COLOR")
	t.Cleanup(func() {
		if wasSet {
			if err := os.Setenv("NO_COLOR", previous); err != nil {
				t.Errorf("restore NO_COLOR: %v", err)
			}
			return
		}
		if err := os.Unsetenv("NO_COLOR"); err != nil {
			t.Errorf("unset NO_COLOR: %v", err)
		}
	})
	deps := testDependencies(t)
	runList := func() (int, string, string) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		code := runWith([]string{"list"}, strings.NewReader(""), &stdout, &stderr, deps)
		return code, stdout.String(), stderr.String()
	}
	if err := os.Unsetenv("NO_COLOR"); err != nil {
		t.Fatal(err)
	}
	wantCode, wantStdout, wantStderr := runList()
	if err := os.Setenv("NO_COLOR", "1"); err != nil {
		t.Fatal(err)
	}
	gotCode, gotStdout, gotStderr := runList()

	if gotCode != wantCode || gotStdout != wantStdout || gotStderr != wantStderr {
		t.Fatalf("NO_COLOR changed list output: got=(%d,%q,%q) want=(%d,%q,%q)", gotCode, gotStdout, gotStderr, wantCode, wantStdout, wantStderr)
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
		"archive, activate, restore, restore-active, purge, terminal-status, app-forget",
		"canonical UUID",
		"tab-separated line",
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("completion help missing %q:\n%s", expected, stdout.String())
		}
	}
}

func TestPurgeHelpDocumentsHerdrOnlyContract(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"purge", "--help"}, &stdout, &stderr)

	if exitCode != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("purge help failed code=%d stderr=%q", exitCode, stderr.String())
	}
	for _, expected := range []string{"Herdr terminal context", "sway-session app forget --yes <context>", "live marks", "launcher approval"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("purge help missing %q:\n%s", expected, stdout.String())
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

func TestJSONResultsUseAnEmptyArrayInsteadOfNullContexts(t *testing.T) {
	var output bytes.Buffer
	if err := writeResult(&output, true, commandResult{Command: "terminal"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"contexts":[]`) || strings.Contains(output.String(), `"contexts":null`) {
		t.Fatalf("agent-facing result has an unstable empty contexts shape: %s", output.String())
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

func TestTerminalSubcommandHelpIsSpecificAndAgentAddressable(t *testing.T) {
	for _, test := range []struct {
		subcommand string
		want       []string
		reject     string
	}{
		{subcommand: "status", want: []string{"terminal status [context] [--project NAME]"}, reject: "--ephemeral"},
		{subcommand: "cleanup", want: []string{"terminal cleanup [--archived-before YYYY-MM-DD]", "Preview"}, reject: "--cwd"},
		{subcommand: "manage", want: []string{"terminal manage [--socket PATH]", "Interactively"}, reject: "--json"},
		{subcommand: "reconfigure", want: []string{"terminal reconfigure [--project NAME] [--socket PATH]", "archived and stopped"}, reject: "--label"},
		{subcommand: "rename", want: []string{"terminal rename --label NAME <context>", "presentation title"}, reject: "--cwd"},
	} {
		t.Run(test.subcommand, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := run([]string{"terminal", test.subcommand, "--help"}, &stdout, &stderr)
			if code != exitSuccess || stderr.Len() != 0 {
				t.Fatalf("help failed code=%d stderr=%q", code, stderr.String())
			}
			for _, want := range test.want {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("help missing %q: %s", want, stdout.String())
				}
			}
			if strings.Contains(stdout.String(), test.reject) {
				t.Fatalf("help included unrelated option %q: %s", test.reject, stdout.String())
			}
		})
	}
}

func TestTerminalHelpDocumentsExplicitFreshAndReusableModes(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"terminal", "--help"}, &stdout, &stderr)
	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("terminal help failed code=%d stderr=%q", code, stderr.String())
	}
	for _, expected := range []string{"--new | --context UUID | --project NAME | --ephemeral", "--cwd PATH", "--role LEFT --role RIGHT"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("terminal help missing %q: %s", expected, stdout.String())
		}
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

func TestPurgeJSONReturnsMachineReadablePreviewBeforeConfirmation(t *testing.T) {
	deps := testDependencies(t)
	registered := registerTestContext(t, deps)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"--json", "purge", string(registered.ID)}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitOperation || !strings.Contains(stderr.String(), `"code":"confirmation_required"`) {
		t.Fatalf("JSON purge confirmation code=%d stderr=%q", code, stderr.String())
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("purge preview is not JSON: %v\n%s", err, stdout.String())
	}
	if !result.Preview || result.Command != "purge" || len(result.Contexts) != 1 || result.Contexts[0].ID != registered.ID {
		t.Fatalf("purge preview is incomplete: %+v", result)
	}
	if len(loadTestRegistry(t, deps).Contexts) != 1 {
		t.Fatal("purge preview modified registry")
	}
}

func TestLifecycleJSONReportsStableIdempotentActions(t *testing.T) {
	deps := testDependencies(t)
	registered := registerTestContext(t, deps)

	archived := runTerminalJSON(t, deps, "--json", "archive", string(registered.ID))
	if !reflect.DeepEqual(archived.Actions, []string{"archived"}) {
		t.Fatalf("first archive action=%v", archived.Actions)
	}
	repeated := runTerminalJSON(t, deps, "--json", "archive", string(registered.ID))
	if !reflect.DeepEqual(repeated.Actions, []string{"no_change"}) {
		t.Fatalf("repeated archive action=%v", repeated.Actions)
	}
	activated := runTerminalJSON(t, deps, "--json", "activate", string(registered.ID))
	if !reflect.DeepEqual(activated.Actions, []string{"activated"}) {
		t.Fatalf("activate action=%v", activated.Actions)
	}
}

func TestUnknownArchiveCommitRejectsAConcurrentArchiveGeneration(t *testing.T) {
	deps := testDependencies(t)
	expected := registerTestContext(t, deps)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	expected.State = sessionstate.ContextArchived
	first := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	expected.ArchivedAt = &first
	visible := expected
	second := first.Add(time.Minute)
	visible.ArchivedAt = &second
	saveTestRegistry(t, root, sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{visible},
	})
	unknown := &statefile.CommitOutcomeUnknownError{Cause: errors.New("directory sync failed")}
	if committedContext(root, expected.ID, func(context sessionstate.Context) bool {
		return contextsEqual(context, expected)
	}, unknown) {
		t.Fatal("unknown commit reported a stale archive timestamp after concurrent lifecycle changes")
	}
}

func TestPurgeStopsDeletesAndThenRemovesRegistryEntry(t *testing.T) {
	deps := testDependencies(t)
	registered := registerTestContext(t, deps)
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

	code := runWith([]string{"purge", "--yes", string(registered.ID), "--json"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("purge failed code=%d stderr=%q", code, stderr.String())
	}
	if len(loadTestRegistry(t, deps).Contexts) != 0 {
		t.Fatal("purged context remains in registry")
	}
	root, _ := deps.stateRoot()
	activity, err := sessionstate.ReadTerminalActivitySnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := sessionstate.FindTerminalActivity(activity, registered.ID); exists {
		t.Fatalf("purged context retained terminal activity: %+v", activity)
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

func TestPurgeRefusesWhenActiveAgentPreventsSessionStop(t *testing.T) {
	deps := testDependencies(t)
	registered := registerTestContext(t, deps)
	paths, _ := deps.herdrPaths()
	sessionPath := filepath.Join(paths.Root, "sessions", "lab-80")
	if err := os.MkdirAll(sessionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &recordingHerdrRunner{
		responses: []string{herdrList(paths.Root, true), ""},
		errors:    []error{nil, errors.New("active agent refused shutdown")},
	}
	deps.herdrRunner = runner
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"--json", "purge", "--yes", string(registered.ID)}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitOperation || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"code":"herdr"`) ||
		!strings.Contains(stderr.String(), "active agent refused shutdown") {
		t.Fatalf("active-agent purge code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !reflect.DeepEqual(runner.calls, [][]string{
		{"session", "list", "--json"}, {"session", "stop", "lab-80", "--json"},
	}) {
		t.Fatalf("purge continued after refused stop: %v", runner.calls)
	}
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("refused purge removed Herdr session state: %v", err)
	}
	registry := loadTestRegistry(t, deps)
	if len(registry.Contexts) != 1 || registry.Contexts[0].ID != registered.ID {
		t.Fatalf("refused purge changed registry: %+v", registry)
	}
	root, _ := deps.stateRoot()
	activity, err := sessionstate.ReadTerminalActivitySnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := sessionstate.FindTerminalActivity(activity, registered.ID); !exists {
		t.Fatalf("refused purge removed terminal activity: %+v", activity)
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
	registered := registerTestContext(t, deps)
	paths, _ := deps.herdrPaths()
	if err := os.Remove(paths.Root); err != nil {
		t.Fatal(err)
	}
	deps.resolveProgram = func(string) (string, error) { return "", errors.New("must not resolve Herdr") }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"purge", "--yes", string(registered.ID)}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitSuccess || len(loadTestRegistry(t, deps).Contexts) != 0 {
		t.Fatalf("purge without Herdr state failed code=%d stderr=%q", code, stderr.String())
	}
}

func TestPurgeYesRejectsLabelWithCanonicalUUIDPreview(t *testing.T) {
	deps := testDependencies(t)
	registered := registerTestContext(t, deps)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"--json", "purge", "--yes", "LAB-80"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitOperation || !strings.Contains(stderr.String(), `"code":"confirmation_required"`) {
		t.Fatalf("label purge code=%d stderr=%q", code, stderr.String())
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("label purge did not return a JSON preview: %v\n%s", err, stdout.String())
	}
	if !result.Preview || len(result.Contexts) != 1 || result.Contexts[0].ID != registered.ID {
		t.Fatalf("label purge preview omitted canonical UUID: %+v", result)
	}
	if len(loadTestRegistry(t, deps).Contexts) != 1 {
		t.Fatal("rejected label purge modified registry")
	}
}

func TestPurgeRejectsApplicationContextsBeforeConfirmationOrHerdrAccess(t *testing.T) {
	applicationContexts := []sessionstate.Context{
		{
			ID: "22222222-2222-4222-8222-222222222222", Label: "Desktop App", Provider: "desktop", State: sessionstate.ContextActive,
			Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherDesktop, DesktopID: "org.example.App.desktop", DesktopOrigin: sessionstate.DesktopEntrySystem, DesktopPath: "/usr/share/applications/org.example.App.desktop"},
			App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow},
		},
		{
			ID: "33333333-3333-4333-8333-333333333333", Label: "Flatpak App", Provider: "flatpak", State: sessionstate.ContextActive,
			Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.Flatpak", FlatpakInstallation: sessionstate.FlatpakUser},
			App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.Flatpak", SandboxAppID: "org.example.Flatpak"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestorePinned},
		},
	}
	for _, contextValue := range applicationContexts {
		for _, herdrRootExists := range []bool{true, false} {
			t.Run(string(contextValue.Launcher.Kind)+"/herdr-root-exists="+strconv.FormatBool(herdrRootExists), func(t *testing.T) {
				deps := testDependencies(t)
				paths, pathErr := deps.herdrPaths()
				if pathErr != nil {
					t.Fatal(pathErr)
				}
				root, err := deps.stateRoot()
				if err != nil {
					t.Fatal(err)
				}
				saveTestRegistry(t, root, sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue}})
				if !herdrRootExists {
					if err := os.Remove(paths.Root); err != nil {
						t.Fatal(err)
					}
				}
				deps.herdrPaths = func() (sessionstate.HerdrPaths, error) {
					t.Fatal("application purge must not resolve Herdr paths")
					return sessionstate.HerdrPaths{}, errors.New("unreachable")
				}
				deps.stdinTerminal = func() bool { t.Fatal("application purge must not request confirmation"); return false }
				var stdout bytes.Buffer
				var stderr bytes.Buffer

				code := runWith([]string{"--json", "purge", "--yes", string(contextValue.ID)}, strings.NewReader(""), &stdout, &stderr, deps)

				if code != exitOperation || stdout.Len() != 0 {
					t.Fatalf("application purge code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				for _, want := range []string{`"code":"context_kind"`, "purge accepts only Herdr terminal contexts", "sway-session app forget --yes " + string(contextValue.ID)} {
					if !strings.Contains(stderr.String(), want) {
						t.Fatalf("application purge diagnostic missing %q: %s", want, stderr.String())
					}
				}
				registry := loadTestRegistry(t, deps)
				if len(registry.Contexts) != 1 || !reflect.DeepEqual(registry.Contexts[0], contextValue) {
					t.Fatalf("rejected application purge modified registry: %+v", registry)
				}
			})
		}
	}
}

func TestPurgeRejectsApplicationContextBeforeConfirmationPreview(t *testing.T) {
	deps := testDependencies(t)
	contextValue := sessionstate.Context{
		ID: "22222222-2222-4222-8222-222222222222", Label: "Desktop App", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherDesktop, DesktopID: "org.example.App.desktop", DesktopOrigin: sessionstate.DesktopEntrySystem, DesktopPath: "/usr/share/applications/org.example.App.desktop"},
		App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow},
	}
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	saveTestRegistry(t, root, sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue}})
	deps.herdrPaths = func() (sessionstate.HerdrPaths, error) {
		t.Fatal("application purge must not resolve Herdr paths")
		return sessionstate.HerdrPaths{}, errors.New("unreachable")
	}
	deps.stdinTerminal = func() bool { t.Fatal("application purge must not request confirmation"); return false }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"--json", "purge", "Desktop App"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitOperation || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"code":"context_kind"`) {
		t.Fatalf("application purge was not rejected before preview: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if registry := loadTestRegistry(t, deps); len(registry.Contexts) != 1 || !reflect.DeepEqual(registry.Contexts[0], contextValue) {
		t.Fatalf("rejected application purge modified registry: %+v", registry)
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
	herdrPaths, err := deps.herdrPaths()
	if err != nil {
		t.Fatal(err)
	}
	want := processCall{
		name: "/trusted/alacritty",
		arguments: []string{
			"--class=sway-session." + string(testContextID),
			"--working-directory=" + registered.Launcher.Cwd, "--title=LAB-80",
			"-e", "/trusted/herdr", "--session", "lab-80",
		},
		environment: []string{
			"SWAY_SESSION_CONTEXT_ID=" + string(testContextID),
			"HERDR_CONFIG_PATH=" + herdrPaths.ConfigFile,
		},
	}
	if !reflect.DeepEqual(starter.calls[0], want) {
		t.Fatalf("launcher argv differs:\ngot  %q\nwant %q", starter.calls[0], want)
	}
}

func TestAutomaticRestoreBatchesMissingContextsInsteadOfLaunchingAnUnboundedBurst(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 7)
	harness := &restoreBatchHarness{}
	clientCalls := 0
	deps.newSwayClient = func(string) swayRequester {
		clientCalls++
		return harness
	}
	deps.processStarter = harness
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"--json", "restore", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("batched restore failed code=%d stderr=%q", code, stderr.String())
	}
	if harness.startCount() != len(registry.Contexts) {
		t.Fatalf("restored %d of %d missing contexts", harness.startCount(), len(registry.Contexts))
	}
	if harness.maximumPending() > maxConcurrentTerminalRestores {
		t.Fatalf("restore launched %d terminals without re-observation; bound=%d", harness.maximumPending(), maxConcurrentTerminalRestores)
	}
	if want := (len(registry.Contexts) + maxConcurrentTerminalRestores - 1) / maxConcurrentTerminalRestores; clientCalls != want {
		t.Fatalf("automatic restore used %d lifecycle waves, want %d", clientCalls, want)
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Contexts) != len(registry.Contexts) {
		t.Fatalf("batched restore reported %d of %d contexts", len(result.Contexts), len(registry.Contexts))
	}
}

func TestAutomaticRestoreDoesNotExceedPendingBoundAfterMappingTimeout(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 5)
	deps.newSwayClient = func(string) swayRequester {
		return &fakeSwayClient{trees: []*swayipc.TreeNode{treeWithContexts()}}
	}
	starter := &recordingStarter{}
	deps.processStarter = starter
	now := time.Unix(0, 0)
	deps.now = func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	deps.settleTimeout = time.Second
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"--json", "restore", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitOperation || len(starter.calls) != maxConcurrentTerminalRestores {
		t.Fatalf("timed-out restore code=%d starts=%d, want bound %d; stderr=%q", code, len(starter.calls), maxConcurrentTerminalRestores, stderr.String())
	}
	if strings.Count(stderr.String(), `"code":"mapping_timeout"`) != maxConcurrentTerminalRestores ||
		!strings.Contains(stderr.String(), `"code":"restore_deferred"`) || !strings.Contains(stderr.String(), fmt.Sprintf("%d additional", len(registry.Contexts)-maxConcurrentTerminalRestores)) {
		t.Fatalf("timed-out restore did not explain deferred contexts: %s", stderr.String())
	}
}

func TestExplicitRestoreSelectsOneContextFromLargeRegistryImmediately(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 300)
	target := registry.Contexts[len(registry.Contexts)-1]
	harness := &restoreBatchHarness{}
	clientCalls := 0
	deps.newSwayClient = func(string) swayRequester {
		clientCalls++
		return harness
	}
	deps.processStarter = harness
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"--json", "restore", "--socket", "/run/user/1000/sway.sock", string(target.ID)}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitSuccess || stderr.Len() != 0 || harness.startCount() != 1 || clientCalls != 1 {
		t.Fatalf("explicit restore was not immediate: code=%d starts=%d clients=%d stderr=%q", code, harness.startCount(), clientCalls, stderr.String())
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Contexts) != 1 || result.Contexts[0].ID != target.ID {
		t.Fatalf("explicit restore returned wrong context: %+v", result.Contexts)
	}
}

func TestAutomaticRestorePreservesEarlierSuccessWhenLaterWaveFails(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 3)
	harness := &restoreBatchHarness{}
	clientCalls := 0
	deps.newSwayClient = func(string) swayRequester {
		clientCalls++
		if clientCalls == 1 {
			return harness
		}
		return &fakeSwayClient{err: errors.New("second restore wave unavailable")}
	}
	deps.processStarter = harness
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"--json", "restore", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitOperation || clientCalls != 2 || harness.startCount() != maxConcurrentTerminalRestores {
		t.Fatalf("later-wave failure code=%d clients=%d starts=%d stderr=%q", code, clientCalls, harness.startCount(), stderr.String())
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode partial restore result: %v; output=%q", err, stdout.String())
	}
	if len(result.Contexts) != maxConcurrentTerminalRestores {
		t.Fatalf("later failure discarded earlier successful contexts: %+v", result.Contexts)
	}
	for index := range maxConcurrentTerminalRestores {
		if result.Contexts[index].ID != registry.Contexts[index].ID {
			t.Fatalf("partial restore context %d = %s, want %s", index, result.Contexts[index].ID, registry.Contexts[index].ID)
		}
	}
	if !strings.Contains(stderr.String(), "second restore wave unavailable") {
		t.Fatalf("later-wave error was not reported: %s", stderr.String())
	}
}

func TestRestoreRejectsTerminalThatDisappearsAfterInitialMapping(t *testing.T) {
	deps := testDependencies(t)
	registered := registerTestContext(t, deps)
	client := &fakeSwayClient{trees: []*swayipc.TreeNode{
		treeWithContexts(),
		treeWithContexts(registered.ID),
		treeWithContexts(),
	}}
	deps.newSwayClient = func(string) swayRequester { return client }
	starter := &recordingStarter{}
	deps.processStarter = starter
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"--json", "restore", "--socket", "/run/user/1000/sway.sock", string(registered.ID)}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitOperation || len(starter.calls) != 1 || !strings.Contains(stderr.String(), `"code":"mapping_unstable"`) {
		t.Fatalf("transient mapping code=%d starts=%v stdout=%q stderr=%q", code, starter.calls, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("transient mapping was reported as restored: %q", stdout.String())
	}
	registry := loadTestRegistry(t, deps)
	if len(registry.Contexts) != 1 || registry.Contexts[0].ID != registered.ID || registry.Contexts[0].State != sessionstate.ContextActive {
		t.Fatalf("restore failure changed desired registry state: %+v", registry)
	}
}

func TestRestoreRejectsMissingTerminalCwdBeforeProcessStart(t *testing.T) {
	deps := testDependencies(t)
	registered := registerTestContext(t, deps)
	if err := os.RemoveAll(registered.Launcher.Cwd); err != nil {
		t.Fatal(err)
	}
	deps.newSwayClient = func(string) swayRequester {
		return &fakeSwayClient{trees: []*swayipc.TreeNode{treeWithContexts()}}
	}
	starter := &recordingStarter{}
	deps.processStarter = starter
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"--json", "restore", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitOperation || len(starter.calls) != 0 || !strings.Contains(stderr.String(), `"code":"project_path"`) {
		t.Fatalf("missing cwd restore code=%d starts=%v stderr=%q", code, starter.calls, stderr.String())
	}
}

func TestRestoreRejectsCurrentInstanceWithOverlongHerdrSocketBeforeStart(t *testing.T) {
	deps := testDependencies(t)
	cwd := t.TempDir()
	sessionName, err := sessionstate.DeriveTerminalInstanceSessionName(testContextID)
	if err != nil {
		t.Fatal(err)
	}
	registered := sessionstate.Context{
		ID:       testContextID,
		Label:    "Terminal",
		Provider: sessionstate.TerminalContextProvider,
		State:    sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{
			Kind:    sessionstate.LauncherHerdr,
			Session: sessionName,
			Cwd:     cwd,
			Terminal: &sessionstate.TerminalLauncher{
				Adapter:  sessionstate.TerminalAdapterAlacritty,
				Instance: true,
			},
		},
	}
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	saveTestRegistry(t, root, sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{registered},
	})
	deps.herdrPaths = func() (sessionstate.HerdrPaths, error) {
		return sessionstate.HerdrPaths{
			Root: "/home/example/configuration-root/herdr", ConfigFile: "/home/example/configuration-root/herdr/config.toml",
		}, nil
	}
	deps.newSwayClient = func(string) swayRequester {
		return &fakeSwayClient{trees: []*swayipc.TreeNode{treeWithContexts()}}
	}
	starter := &recordingStarter{}
	deps.processStarter = starter
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runWith([]string{"--json", "restore", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitOperation || len(starter.calls) != 0 || !strings.Contains(stderr.String(), `"code":"herdr_path"`) ||
		!strings.Contains(stderr.String(), "herdr-client.sock") || !strings.Contains(stderr.String(), "terminal --new") {
		t.Fatalf("overlong restore code=%d starts=%v stdout=%q stderr=%q", code, starter.calls, stdout.String(), stderr.String())
	}
}

func TestRestorePendingProcessPreventsDuplicateLaunch(t *testing.T) {
	deps := testDependencies(t)
	registered := registerTestContext(t, deps)
	deps.findPendingProcess = func(string, sessionstate.ProcessSpec) ([]int, error) { return []int{1234}, nil }
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
	if err := sessionstate.RegistryStoreFor(root).LoadInto(&registry); err != nil {
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

func TestRestoreWaitsForTerminalLifecycleLock(t *testing.T) {
	deps := testDependencies(t)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- sessionstate.WithTerminalLifecycleLockContext(context.Background(), root, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	defer func() {
		close(release)
		if lockErr := <-lockDone; lockErr != nil {
			t.Errorf("release terminal lifecycle lock: %v", lockErr)
		}
	}()
	clientCalls := 0
	deps.newSwayClient = func(string) swayRequester {
		clientCalls++
		return &fakeSwayClient{trees: []*swayipc.TreeNode{treeWithContexts()}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, commandFailure := executeRestore(ctx, []string{"--socket", "/run/user/1000/sway.sock"}, deps)
	if commandFailure == nil || len(commandFailure.diagnostics) != 1 || !strings.Contains(commandFailure.diagnostics[0].Hint, context.DeadlineExceeded.Error()) {
		t.Fatalf("locked restore did not return context deadline: %+v", commandFailure)
	}
	if clientCalls != 0 {
		t.Fatalf("restore contacted Sway before acquiring lifecycle lock: %d", clientCalls)
	}
}

func TestRestoreDuplicateContextDoesNotPreventIndependentLaunch(t *testing.T) {
	deps := testDependencies(t)
	first := registerTestContext(t, deps)
	second := sessionstate.Context{
		ID: "22222222-2222-4222-8222-222222222222", Label: "LAB-81", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{
			Kind: sessionstate.LauncherHerdr, Session: "lab-81", Cwd: first.Launcher.Cwd,
			Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty},
		},
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

func TestUnknownCommandUsesUsageExitCode(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"launch"}, &stdout, &stderr)

	if exitCode != exitUsage || !strings.Contains(stderr.String(), "unknown command \"launch\"") {
		t.Fatalf("unexpected result code=%d stderr=%q", exitCode, stderr.String())
	}
}

func TestLegacyCodexReportCommandIsNotAvailable(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWith([]string{"report-codex-session"}, strings.NewReader(`{}`), &stdout, &stderr, testDependencies(t))

	if exitCode != exitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), `unknown command "report-codex-session"`) {
		t.Fatalf("legacy command result code=%d stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if exitCode := runWith([]string{"--help"}, strings.NewReader(""), &stdout, &stderr, testDependencies(t)); exitCode != exitSuccess || strings.Contains(stdout.String(), "report-codex-session") {
		t.Fatalf("legacy command remained in help: code=%d help=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
}

func testDependencies(t *testing.T) dependencies {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "state", "sway-session")
	project := filepath.Join(base, "project")
	herdrRoot, err := os.MkdirTemp("", "herdr-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(herdrRoot); err != nil {
			t.Errorf("remove Herdr test root: %v", err)
		}
	})
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	return dependencies{
		stateRoot:    func() (string, error) { return root, nil },
		workingDir:   func() (string, error) { return project, nil },
		homeDir:      func() (string, error) { return base, nil },
		newContextID: func() (sessionstate.ContextID, error) { return testContextID, nil },
		loadSessionConfig: func(path string) (sessionstate.SessionConfig, string, error) {
			if path == "" {
				return sessionstate.DefaultSessionConfig(), filepath.Join(base, "config", "sway-session", "config.toml"), nil
			}
			return sessionstate.LoadSessionConfig(path)
		},
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
		presentApproval:    func(string, []sessionstate.ApprovalChoice) error { return nil },
		verifyFlatpak:      func(sessionstate.Launcher) error { return nil },
		newSwayClient:      func(string) swayRequester { return &fakeSwayClient{trees: []*swayipc.TreeNode{treeWithContexts()}} },
		processStarter:     &recordingStarter{},
		herdrRunner:        &recordingHerdrRunner{},
		findPendingProcess: func(string, sessionstate.ProcessSpec) ([]int, error) { return nil, nil },
		now:                time.Now,
		sleep:              func(time.Duration) {},
		settleTimeout:      time.Second,
		stabilityDelay:     time.Millisecond,
		stdinTerminal:      func() bool { return false },
	}
}

func registerTestContext(t *testing.T, deps dependencies) sessionstate.Context {
	t.Helper()
	result, commandFailure := executeRegister(t.Context(), []string{"--session", "lab-80", "--label", "LAB-80"}, deps)
	if commandFailure != nil {
		t.Fatalf("register fixture: %+v", commandFailure)
	}
	return result.Contexts[0]
}

func storeRestoreTestContexts(t *testing.T, deps dependencies, count int) sessionstate.Registry {
	t.Helper()
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := deps.workingDir()
	if err != nil {
		t.Fatal(err)
	}
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{}}
	for index := range count {
		id := sessionstate.ContextID(fmt.Sprintf("%08x-0000-4000-8000-%012x", index+1, index+1))
		registry.Contexts = append(registry.Contexts, sessionstate.Context{
			ID: id, Label: fmt.Sprintf("Restore %d", index+1), State: sessionstate.ContextActive,
			Launcher: sessionstate.Launcher{
				Kind: sessionstate.LauncherHerdr, Session: fmt.Sprintf("restore-%d", index+1), Cwd: cwd,
				Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty},
			},
		})
	}
	saveTestRegistry(t, root, registry)
	return registry
}

func saveTestRegistry(t *testing.T, root string, registry sessionstate.Registry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	for {
		err := sessionstate.RegistryStoreFor(root).SaveContext(ctx, registry)
		if err == nil {
			return
		}
		if !sessionstate.IsStateDatabaseBusy(err) {
			t.Fatal(err)
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("save registry fixture after busy result: %v", err)
		case <-timer.C:
		}
	}
}

func loadTestRegistry(t *testing.T, deps dependencies) sessionstate.Registry {
	t.Helper()
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	var registry sessionstate.Registry
	if err := sessionstate.RegistryStoreFor(root).LoadInto(&registry); err != nil {
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

func (client *fakeSwayClient) RequestContext(ctx context.Context, messageType swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if err := ctx.Err(); err != nil {
		return swayipc.Message{}, err
	}
	return client.Request(messageType, payload)
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

func (client *sharedTreeClient) RequestContext(ctx context.Context, messageType swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if err := ctx.Err(); err != nil {
		return swayipc.Message{}, err
	}
	return client.Request(messageType, payload)
}

func (*sharedTreeClient) Close() {}

type mappingStarter struct {
	mapped *atomic.Bool
	starts atomic.Int32
}

type restoreBatchHarness struct {
	mu         sync.Mutex
	mapped     []sessionstate.ContextID
	pending    []sessionstate.ContextID
	starts     int
	maxPending int
}

func (harness *restoreBatchHarness) Start(spec sessionstate.ProcessSpec) error {
	var id sessionstate.ContextID
	for _, argument := range spec.Arguments {
		const prefix = "--class=sway-session."
		if strings.HasPrefix(argument, prefix) {
			id = sessionstate.ContextID(strings.TrimPrefix(argument, prefix))
			break
		}
	}
	if id == "" {
		return errors.New("terminal process spec has no managed application ID")
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	harness.starts++
	harness.pending = append(harness.pending, id)
	harness.maxPending = max(harness.maxPending, len(harness.pending))
	return nil
}

func (harness *restoreBatchHarness) Request(messageType swayipc.MessageType, _ []byte) (swayipc.Message, error) {
	if messageType != swayipc.GetTree {
		return swayipc.Message{}, errors.New("unexpected Sway request")
	}
	harness.mu.Lock()
	harness.mapped = append(harness.mapped, harness.pending...)
	harness.pending = nil
	tree := treeWithContexts(harness.mapped...)
	harness.mu.Unlock()
	payload, err := json.Marshal(tree)
	return swayipc.Message{Type: swayipc.GetTree, Payload: payload}, err
}

func (harness *restoreBatchHarness) RequestContext(ctx context.Context, messageType swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if err := ctx.Err(); err != nil {
		return swayipc.Message{}, err
	}
	return harness.Request(messageType, payload)
}

func (*restoreBatchHarness) Close() {}

func (harness *restoreBatchHarness) startCount() int {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	return harness.starts
}

func (harness *restoreBatchHarness) maximumPending() int {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	return harness.maxPending
}

func (starter *mappingStarter) Start(sessionstate.ProcessSpec) error {
	starter.starts.Add(1)
	starter.mapped.Store(true)
	return nil
}

func treeWithContexts(ids ...sessionstate.ContextID) *swayipc.TreeNode {
	workspace := &swayipc.TreeNode{ID: 3, Type: "workspace", Name: "98", Nodes: []*swayipc.TreeNode{}, FloatingNodes: []*swayipc.TreeNode{}}
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
	errors     []error
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
	var responseErr error
	if len(runner.errors) != 0 {
		responseErr = runner.errors[0]
		runner.errors = runner.errors[1:]
	}
	if responseErr != nil {
		return []byte(response), responseErr
	}
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
