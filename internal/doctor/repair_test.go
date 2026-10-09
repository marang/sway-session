package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepairPlanPreviewApplyBackupAndIdempotence(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "config")
	secret := "set $mod Mod4\nset $private super-secret-value\n"
	if err := os.WriteFile(root, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(directory, "sway-session")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n: > \"$0.started\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(directory, "unrelated.conf")
	if err := os.WriteFile(unrelated, []byte("set $private untouched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root, Executable: executable})

	plan, err := service.Plan(context.Background(), swayIntegrationFixID)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 2 || len(plan.edits) != 2 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	for _, change := range plan.Changes {
		if strings.Contains(change.Preview, "super-secret") {
			t.Fatalf("preview leaked surrounding configuration: %+v", change)
		}
	}
	if got := plan.Changes[1].Preview; !strings.HasPrefix(got, "+ include ") || strings.Contains(got, "set $private") {
		t.Fatalf("root preview is not narrow: %q", got)
	}

	result, err := service.Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != swayIntegrationFixID || len(result.Backups) != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	backup, err := os.ReadFile(result.Backups[0])
	if err != nil || string(backup) != secret {
		t.Fatalf("backup mismatch: %q, %v", backup, err)
	}
	assertMode(t, result.Backups[0], 0o600)
	snippet := filepath.Join(directory, doctorSnippetName)
	assertMode(t, snippet, 0o600)
	rootAfter, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(rootAfter), secret) || strings.Count(string(rootAfter), "include ") != 1 {
		t.Fatalf("root content not narrowly appended: %q", rootAfter)
	}
	if _, err := os.Stat(executable + ".started"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repair executed the pinned command: %v", err)
	}
	unrelatedAfter, err := os.ReadFile(unrelated)
	if err != nil || string(unrelatedAfter) != "set $private untouched\n" {
		t.Fatalf("repair changed unrelated configuration: %q, %v", unrelatedAfter, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 5 {
		t.Fatalf("repair wrote beyond planned files and backup: %v, %v", entries, err)
	}
	managed, err := os.ReadFile(snippet)
	if err != nil || !strings.Contains(string(managed), "exec --no-startup-id "+executable+" daemon\n") ||
		!strings.Contains(string(managed), "exec --no-startup-id "+executable+" restore\n") || strings.Contains(string(managed), "exec_always") {
		t.Fatalf("repair changed pinned startup commands: %q, %v", managed, err)
	}
	check := inspectSwayConfig(context.Background(), service.options)[0]
	if check.Status != OK {
		t.Fatalf("repair did not converge: %+v", check)
	}
	if _, err := service.Plan(context.Background(), swayIntegrationFixID); err == nil || !strings.Contains(err.Error(), "not needed") {
		t.Fatalf("idempotent plan unexpectedly available: %v", err)
	}
}

func TestRepairAddsOnlyMissingDirectives(t *testing.T) {
	root := writeSwayConfig(t,
		"set $mod Mod4\nexec --no-startup-id /custom/sway-session daemon\n"+
			"bindsym $mod+Return exec --no-startup-id sway-session terminal --new\n")
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(context.Background(), swayIntegrationFixID)
	if err != nil {
		t.Fatal(err)
	}
	preview := plan.Changes[0].Preview
	if strings.Contains(preview, " daemon\n") || strings.Contains(preview, " terminal --new\n") ||
		!strings.Contains(preview, " restore\n") || !strings.Contains(preview, " terminal --ephemeral\n") {
		t.Fatalf("managed snippet was not limited to missing directives: %q", preview)
	}
}

func TestRepairApplyExplainsWhenMissingIntegrationTakesEffect(t *testing.T) {
	const (
		daemon     = "exec --no-startup-id /usr/bin/sway-session daemon\n"
		restore    = "exec --no-startup-id /usr/bin/sway-session restore\n"
		persistent = "bindsym $mod+Return exec --no-startup-id /usr/bin/sway-session terminal --new\n"
		ephemeral  = "bindsym $mod+Shift+Return exec --no-startup-id /usr/bin/sway-session terminal --ephemeral\n"
		applied    = "Applied the managed Sway integration files."
		bindings   = " Reload Sway to load the new bindings."
		startup    = " New startup commands run in your next Sway session; reload does not run them."
		daemonNow  = " An existing daemon keeps running; if none is running, start it with sway-session daemon."
		restoreNow = " Run sway-session restore only if you want saved windows restored now."
	)
	for _, test := range []struct {
		name, existing, message string
	}{
		{"bindings only", daemon + restore, applied + bindings},
		{"persistent binding only", daemon + restore + ephemeral, applied + bindings},
		{"ephemeral binding only", daemon + restore + persistent, applied + bindings},
		{"daemon only", restore + persistent + ephemeral, applied + startup + daemonNow},
		{"restore only", daemon + persistent + ephemeral, applied + startup + restoreNow},
		{"startup only", persistent + ephemeral, applied + startup + daemonNow + restoreNow},
		{"all integration", "", applied + bindings + startup + daemonNow + restoreNow},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := "set $mod Mod4\n" + test.existing
			root := writeSwayConfig(t, original)
			service := New(Options{SwayConfigPath: root})
			plan, err := service.Plan(t.Context(), swayIntegrationFixID)
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.Apply(t.Context(), plan)
			if err != nil {
				t.Fatal(err)
			}
			if result.Message != test.message {
				t.Errorf("repair guidance = %q, want %q", result.Message, test.message)
			}
			if len(result.Backups) != 1 {
				t.Fatalf("repair backups = %v, want original root backup", result.Backups)
			}
			backup, err := os.ReadFile(result.Backups[0])
			if err != nil || string(backup) != original {
				t.Fatalf("backup did not preserve original configuration: %q, %v", backup, err)
			}
			check := inspectSwayConfig(t.Context(), service.options)[0]
			if check.Status != OK {
				t.Fatalf("repair did not converge: %+v", check)
			}
		})
	}
}

func TestRepairApplyGuidanceDistinguishesExistingManagedStartup(t *testing.T) {
	root := writeSwayConfig(t, "set $mod Mod4\ninclude 50-sway-session-doctor.conf\n"+
		"bindsym $mod+Return exec --no-startup-id /usr/bin/sway-session terminal --new\n"+
		"bindsym $mod+Shift+Return exec --no-startup-id /usr/bin/sway-session terminal --ephemeral\n")
	snippet := filepath.Join(filepath.Dir(root), doctorSnippetName)
	if err := os.WriteFile(snippet, []byte(doctorHeader+"exec --no-startup-id /usr/bin/sway-session daemon\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(t.Context(), swayIntegrationFixID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Apply(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Message, "reload does not run them") || !strings.Contains(result.Message, "restore only if") ||
		strings.Contains(result.Message, "daemon") || strings.Contains(result.Message, "new bindings") {
		t.Fatalf("guidance confused existing and new managed directives: %q", result.Message)
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Path != snippet || len(result.Backups) != 1 {
		t.Fatalf("repair expanded beyond the existing managed snippet: plan=%+v result=%+v", plan, result)
	}
}

func TestRepairRecoversMissingIncludedManagedSnippet(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "config")
	snippet := filepath.Join(directory, doctorSnippetName)
	includeLine, err := renderIncludeLine(snippet)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("set $mod Mod4\n"+includeLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(context.Background(), swayIntegrationFixID)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Path != snippet {
		t.Fatalf("repair should only recover included snippet: %+v", plan.Changes)
	}
	if _, err := service.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
}

func TestRepairRefusesUnknownManagedSnippetEdits(t *testing.T) {
	root := writeSwayConfig(t, "set $mod Mod4\n")
	snippet := filepath.Join(filepath.Dir(root), doctorSnippetName)
	if err := os.WriteFile(snippet, []byte(doctorHeader+"set $manual yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root})
	check := inspectSwayConfig(context.Background(), service.options)[0]
	if check.FixID != "" || !strings.Contains(check.Hint, "unrecognized") {
		t.Fatalf("inspection offered unsafe overwrite: %+v", check)
	}
	if _, err := service.Plan(context.Background(), swayIntegrationFixID); err == nil || !strings.Contains(err.Error(), "manual edits") {
		t.Fatalf("manual edits were not refused: %v", err)
	}
}

func TestRepairRejectsStaleFileAndDirectoryPlans(t *testing.T) {
	t.Run("file changed", func(t *testing.T) {
		root := writeSwayConfig(t, "set $mod Mod4\n")
		service := New(Options{SwayConfigPath: root})
		plan, err := service.Plan(context.Background(), swayIntegrationFixID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(root, []byte("set $mod Mod1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Apply(context.Background(), plan); err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("stale file plan accepted: %v", err)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(root), doctorSnippetName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale plan wrote snippet: %v", err)
		}
	})

	t.Run("directory replaced", func(t *testing.T) {
		parent := t.TempDir()
		directory := filepath.Join(parent, "sway")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(directory, "config")
		if err := os.WriteFile(root, []byte("set $mod Mod4\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		service := New(Options{SwayConfigPath: root})
		plan, err := service.Plan(context.Background(), swayIntegrationFixID)
		if err != nil {
			t.Fatal(err)
		}
		old := directory + "-old"
		if err := os.Rename(directory, old); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(root, []byte("set $mod Mod4\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Apply(context.Background(), plan); err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("replaced directory accepted: %v", err)
		}
	})
}

func TestRepairRefusesUnsafeExecutableAndCancellation(t *testing.T) {
	root := writeSwayConfig(t, "set $mod Mod4\n")
	service := New(Options{SwayConfigPath: root, Executable: "/usr/bin/sway-session;touch-pwned"})
	if _, err := service.Plan(context.Background(), swayIntegrationFixID); err == nil {
		t.Fatalf("unsafe executable accepted: %v", err)
	}

	service = New(Options{SwayConfigPath: root})
	plan, err := service.Plan(context.Background(), swayIntegrationFixID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Apply(ctx, plan); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled apply returned %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), doctorSnippetName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled apply wrote snippet: %v", err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode %04o, want %04o", path, got, want)
	}
}
