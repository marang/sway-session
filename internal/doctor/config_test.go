package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestInspectSwayConfigHealthyStaticConfiguration(t *testing.T) {
	path := writeSwayConfig(t, healthySwayConfig())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	checks := inspectSwayConfig(context.Background(), Options{SwayConfigPath: path})
	if len(checks) != 1 || checks[0].Status != OK || checks[0].FixID != "" {
		t.Fatalf("unexpected checks: %+v", checks)
	}
	if !strings.Contains(checks[0].Evidence[0], "on-disk") || !strings.Contains(checks[0].Hint, "does not claim") {
		t.Fatalf("static provenance not explicit: %+v", checks[0])
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("inspection changed the configuration")
	}
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*doctor*")); len(matches) != 0 {
		t.Fatalf("inspection created files: %v", matches)
	}
}

func TestInspectSwayConfigMissingOffersNarrowRepair(t *testing.T) {
	path := writeSwayConfig(t, "set $mod Mod4\n")
	check := inspectSwayConfig(context.Background(), Options{SwayConfigPath: path})[0]
	if check.Status != Warning || check.FixID != swayIntegrationFixID {
		t.Fatalf("missing integration did not offer repair: %+v", check)
	}
}

func TestInspectSwayConfigFollowsQuotedVariableGlobIncludes(t *testing.T) {
	directory := t.TempDir()
	includedDirectory := filepath.Join(directory, "parts with spaces")
	if err := os.Mkdir(includedDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(includedDirectory, "50-session.conf"), []byte(healthySwayConfig()), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(directory, "config")
	content := "set $parts \"" + includedDirectory + "\"\ninclude \"$parts/*.conf\"\n"
	if err := os.WriteFile(root, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	check := inspectSwayConfig(context.Background(), Options{SwayConfigPath: root})[0]
	if check.Status != OK || !containsEvidence(check.Evidence, "followed 2 safe") {
		t.Fatalf("include graph not recognized: %+v", check)
	}
}

func TestInspectSwayConfigRefusesDuplicateAndOverriddenShortcut(t *testing.T) {
	config := healthySwayConfig() + "bindsym $mod+Return exec foot\n"
	path := writeSwayConfig(t, config)
	check := inspectSwayConfig(context.Background(), Options{SwayConfigPath: path})[0]
	if check.Status != Warning || check.FixID != "" || !strings.Contains(check.Detail, "conflicting") {
		t.Fatalf("overridden shortcut was not reported conservatively: %+v", check)
	}
	if !containsEvidence(check.Evidence, "conflicting declaration") {
		t.Fatalf("conflicting location missing: %+v", check.Evidence)
	}
}

func TestInspectSwayConfigRefusesExecAlwaysStartup(t *testing.T) {
	config := strings.Replace(healthySwayConfig(), "exec --no-startup-id /usr/bin/sway-session daemon",
		"exec_always --no-startup-id /usr/bin/sway-session daemon", 1)
	path := writeSwayConfig(t, config)
	check := inspectSwayConfig(context.Background(), Options{SwayConfigPath: path})[0]
	if check.Status != Warning || check.FixID != "" || !strings.Contains(check.Detail, "conflicting") {
		t.Fatalf("exec_always was accepted as one-time: %+v", check)
	}
}

func TestInspectSwayConfigUnsupportedMalformedAndCycle(t *testing.T) {
	tests := map[string]func(*testing.T) string{
		"dynamic include": func(t *testing.T) string {
			return writeSwayConfig(t, "include $(find /tmp -name '*.conf')\n")
		},
		"malformed quote": func(t *testing.T) string {
			return writeSwayConfig(t, "set $broken \"unterminated\n")
		},
		"include cycle": func(t *testing.T) string {
			directory := t.TempDir()
			first := filepath.Join(directory, "first")
			second := filepath.Join(directory, "second")
			if err := os.WriteFile(first, []byte("include \""+second+"\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(second, []byte("include \""+first+"\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return first
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			check := inspectSwayConfig(context.Background(), Options{SwayConfigPath: setup(t)})[0]
			if check.Status != Unavailable || check.FixID != "" {
				t.Fatalf("unsafe syntax offered repair: %+v", check)
			}
		})
	}
}

func TestInspectSwayConfigRejectsUnsafeFilesystemObjectsWithoutBlocking(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		target := writeSwayConfig(t, healthySwayConfig())
		link := filepath.Join(t.TempDir(), "config")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		assertUnavailableQuickly(t, link)
	})
	t.Run("hardlink", func(t *testing.T) {
		target := writeSwayConfig(t, healthySwayConfig())
		link := filepath.Join(filepath.Dir(target), "other")
		if err := os.Link(target, link); err != nil {
			t.Fatal(err)
		}
		assertUnavailableQuickly(t, target)
	})
	t.Run("group writable", func(t *testing.T) {
		path := writeSwayConfig(t, healthySwayConfig())
		if err := os.Chmod(path, 0o620); err != nil {
			t.Fatal(err)
		}
		assertUnavailableQuickly(t, path)
	})
	t.Run("fifo", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config")
		if err := unix.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		assertUnavailableQuickly(t, path)
	})
}

func TestInspectSwayConfigHonorsCancellation(t *testing.T) {
	path := writeSwayConfig(t, healthySwayConfig())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	check := inspectSwayConfig(ctx, Options{SwayConfigPath: path})[0]
	if check.Status != Unavailable || !strings.Contains(check.Hint, "canceled") {
		t.Fatalf("cancellation not preserved: %+v", check)
	}
}

func assertUnavailableQuickly(t *testing.T, path string) {
	t.Helper()
	started := time.Now()
	check := inspectSwayConfig(context.Background(), Options{SwayConfigPath: path})[0]
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("unsafe object inspection blocked for %s", elapsed)
	}
	if check.Status != Unavailable || check.FixID != "" {
		t.Fatalf("unsafe object accepted: %+v", check)
	}
}

func writeSwayConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func healthySwayConfig() string {
	return "exec --no-startup-id /usr/bin/sway-session daemon\n" +
		"exec --no-startup-id /usr/bin/sway-session restore\n" +
		"bindsym $mod+Return exec --no-startup-id /usr/bin/sway-session terminal --new\n" +
		"bindsym $mod+Shift+Return exec --no-startup-id /usr/bin/sway-session terminal --ephemeral\n"
}

func containsEvidence(evidence []string, fragment string) bool {
	for _, item := range evidence {
		if strings.Contains(item, fragment) {
			return true
		}
	}
	return false
}
