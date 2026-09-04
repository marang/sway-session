package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
)

func TestCompletionContextsHonorsCanceledCommandContext(t *testing.T) {
	deps := testDependencies(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, failure := executeCompletion(ctx, []string{"contexts", "archive"}, deps)
	if failure == nil || len(failure.diagnostics) == 0 ||
		!strings.Contains(failure.diagnostics[0].Message+failure.diagnostics[0].Hint, context.Canceled.Error()) {
		t.Fatalf("canceled completion returned failure %+v", failure)
	}
}

func TestCompletionContextsArchiveEmitsOnlyActiveCanonicalIDs(t *testing.T) {
	deps := testDependencies(t)
	active := registerTestContext(t, deps)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	archived := sessionstate.Context{
		ID:       "22222222-2222-4222-8222-222222222222",
		Label:    "Archived Work",
		Provider: "linear",
		State:    sessionstate.ContextArchived,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: "archived-work", Cwd: active.Launcher.Cwd, Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}},
	}
	if _, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
		return sessionstate.AddContext(registry, archived)
	}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWith([]string{"completion", "contexts", "archive"}, strings.NewReader(""), &stdout, &stderr, deps)

	want := string(active.ID) + "\tLAB-80 · active · herdr:lab-80 · " + active.Launcher.Cwd + "\n"
	if code != exitSuccess || stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("unexpected completion result code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestCompletionContextReadRejectsUnsupportedSchemaWithoutMutation(t *testing.T) {
	deps := testDependencies(t)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := sessionstate.RegistryStoreFor(root).Save(sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{}}); err != nil {
		t.Fatal(err)
	}
	execStateDatabaseForTest(t, root, "PRAGMA user_version = 2")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWith([]string{"completion", "contexts", "restore"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitOperation || stdout.Len() != 0 || !strings.Contains(stderr.String(), "unsupported state database schema version 2") {
		t.Fatalf("unsupported database completion did not fail read-only: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestCompletionContextsMissingRegistryIsSilentAndReadOnly(t *testing.T) {
	deps := testDependencies(t)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWith([]string{"completion", "contexts", "archive"}, strings.NewReader(""), &stdout, &stderr, deps)

	if code != exitSuccess || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("missing registry was not silent: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completion created missing state root: %v", err)
	}
}

func TestCompletionContextsDoesNotWaitForRegistryMutationLock(t *testing.T) {
	deps := testDependencies(t)
	registerTestContext(t, deps)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}

	locked := make(chan struct{})
	release := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		_, updateErr := sessionstate.UpdateRegistry(root, func(*sessionstate.Registry) error {
			close(locked)
			<-release
			return nil
		})
		mutationDone <- updateErr
	}()
	<-locked
	released := false
	mutationFinished := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		if !mutationFinished {
			<-mutationDone
		}
	})

	type completionResult struct {
		code   int
		stdout string
		stderr string
	}
	result := make(chan completionResult, 1)
	go func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		code := runWith([]string{"completion", "contexts", "archive"}, strings.NewReader(""), &stdout, &stderr, deps)
		result <- completionResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}()

	select {
	case got := <-result:
		if got.code != exitSuccess || got.stdout == "" || got.stderr != "" {
			t.Fatalf("unexpected completion during mutation: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("completion waited for the registry mutation lock")
	}

	close(release)
	released = true
	err = <-mutationDone
	mutationFinished = true
	if err != nil {
		t.Fatal(err)
	}
}

func TestCompletionContextsRejectsUnsupportedScopeWithoutReadingState(t *testing.T) {
	deps := testDependencies(t)
	deps.stateRoot = func() (string, error) {
		t.Fatal("unsupported completion scope read session state")
		return "", nil
	}
	var stderr bytes.Buffer
	code := runWith([]string{"completion", "contexts", "unknown"}, strings.NewReader(""), &bytes.Buffer{}, &stderr, deps)
	if code != exitUsage || !strings.Contains(stderr.String(), "Usage: sway-session completion contexts <command>") {
		t.Fatalf("unexpected unsupported-scope result code=%d stderr=%q", code, stderr.String())
	}
}

func TestCompletionContextDescriptionShowsUsefulMetadataWithoutPrivateLauncherPaths(t *testing.T) {
	deps := testDependencies(t)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Dir(filepath.Dir(root))
	t.Setenv("HOME", home)
	contexts := []sessionstate.Context{
		{
			ID: "11111111-1111-4111-8111-111111111111", Label: "Sway Title Animator", Provider: "linear", State: sessionstate.ContextActive,
			Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: "sway-title-animator", Cwd: filepath.Join(home, "Dev", "sway-title-animator"), Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}},
		},
		{
			ID: "22222222-2222-4222-8222-222222222222", Label: "Calculator", Provider: "desktop", State: sessionstate.ContextActive,
			Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherDesktop, DesktopID: "org.example.Calculator.desktop", DesktopOrigin: sessionstate.DesktopEntrySystem, DesktopPath: "/usr/share/applications/org.example.Calculator.desktop"},
			App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.Calculator"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow},
		},
	}
	if err := sessionstate.RegistryStoreFor(root).Save(sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: contexts}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWith([]string{"completion", "contexts", "archive"}, strings.NewReader(""), &stdout, &stderr, deps)
	want := "11111111-1111-4111-8111-111111111111\tSway Title Animator · active · herdr:sway-title-animator · ~/Dev/sway-title-animator · provider:linear\n" +
		"22222222-2222-4222-8222-222222222222\tCalculator · active · desktop:org.example.Calculator.desktop · provider:desktop\n"
	if code != exitSuccess || stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("unexpected completion metadata code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, private := range []string{"/usr/share/applications", "org.example.Calculator\t"} {
		if strings.Contains(stdout.String(), private) {
			t.Fatalf("completion exposed private launcher detail %q: %q", private, stdout.String())
		}
	}
}

func TestCompletionContextsFollowCommandEligibility(t *testing.T) {
	deps := testDependencies(t)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	project, err := deps.workingDir()
	if err != nil {
		t.Fatal(err)
	}
	contexts := []sessionstate.Context{
		{
			ID: "11111111-1111-4111-8111-111111111111", Label: "Active Herdr", State: sessionstate.ContextActive,
			Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: "active-herdr", Cwd: project, Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}},
		},
		{
			ID: "22222222-2222-4222-8222-222222222222", Label: "Archived Herdr", State: sessionstate.ContextArchived,
			Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: "archived-herdr", Cwd: project, Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}},
		},
		{
			ID: "33333333-3333-4333-8333-333333333333", Label: "Active Desktop", State: sessionstate.ContextActive,
			Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherDesktop, DesktopID: "org.example.Active.desktop", DesktopOrigin: sessionstate.DesktopEntrySystem, DesktopPath: "/usr/share/applications/org.example.Active.desktop"},
			App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.Active"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow},
		},
		{
			ID: "44444444-4444-4444-8444-444444444444", Label: "Archived Flatpak", State: sessionstate.ContextArchived,
			Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.Archived", FlatpakInstallation: sessionstate.FlatpakUser},
			App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.Archived", SandboxAppID: "org.example.Archived"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestorePinned},
		},
	}
	if err := sessionstate.RegistryStoreFor(root).Save(sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: contexts}); err != nil {
		t.Fatal(err)
	}

	tests := map[string][]string{
		"archive":         {string(contexts[0].ID), string(contexts[2].ID)},
		"activate":        {string(contexts[1].ID), string(contexts[3].ID)},
		"restore":         {string(contexts[0].ID), string(contexts[1].ID), string(contexts[2].ID)},
		"restore-active":  {string(contexts[0].ID), string(contexts[2].ID)},
		"purge":           {string(contexts[0].ID), string(contexts[1].ID)},
		"terminal-status": {string(contexts[0].ID), string(contexts[1].ID)},
		"app-forget":      {string(contexts[2].ID), string(contexts[3].ID)},
	}
	for command, want := range tests {
		t.Run(command, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := runWith([]string{"completion", "contexts", command}, strings.NewReader(""), &stdout, &stderr, deps)
			got := completionValues(stdout.String())
			if code != exitSuccess || stderr.Len() != 0 || !reflect.DeepEqual(got, want) {
				t.Fatalf("unexpected completion result code=%d values=%v stderr=%q", code, got, stderr.String())
			}
		})
	}
}

func completionValues(output string) []string {
	if output == "" {
		return []string{}
	}
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	values := make([]string, 0, len(lines))
	for _, line := range lines {
		value, _, _ := strings.Cut(line, "\t")
		values = append(values, value)
	}
	return values
}
