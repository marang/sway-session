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
	secret := "set $private super-secret-value\n"
	if err := os.WriteFile(root, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root})

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
		"exec --no-startup-id /custom/sway-session daemon\n"+
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

func TestRepairRecoversMissingIncludedManagedSnippet(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "config")
	snippet := filepath.Join(directory, doctorSnippetName)
	includeLine, err := renderIncludeLine(snippet)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte(includeLine+"\n"), 0o600); err != nil {
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
