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

func TestInspectSwayConfigSupportedProfiles(t *testing.T) {
	for _, shortcuts := range []ShortcutSelection{ShortcutsNone, ShortcutsDefault} {
		t.Run(string(shortcuts), func(t *testing.T) {
			root := writeStandardSwayConfig(t, shortcuts, true)
			check := inspectSwayConfig(t.Context(), Options{SwayConfigPath: root})[0]
			if check.Status != OK || check.FixID != swayIntegrationFixID || check.AdoptionRequired {
				t.Fatalf("supported owned profile not recognized: %+v", check)
			}
			if !containsEvidence(check.Evidence, standardProfileEvidence(shortcuts)) || !containsEvidence(check.Evidence, "literal direct include: present at "+root+":1") {
				t.Fatalf("missing located source evidence: %+v", check)
			}
			if !strings.Contains(check.Hint, "does not establish effective load order or live binding activity") {
				t.Fatalf("report overstated source evidence: %+v", check)
			}
		})
	}
}

func TestInspectSwayConfigMissingOwnedIntegration(t *testing.T) {
	for _, test := range []struct {
		name, content string
		status        Status
	}{
		{"no direct include", healthySwayConfig(), Unavailable},
		{"missing included sibling", "include config.d/" + doctorSnippetName + "\n", Warning},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := writeSwayConfig(t, test.content)
			check := inspectSwayConfig(t.Context(), Options{SwayConfigPath: root})[0]
			if check.Status != test.status || check.FixID != swayIntegrationFixID || !check.AdoptionRequired {
				t.Fatalf("missing owned source must require adoption: %+v", check)
			}
			if !containsEvidence(check.Evidence, "standard file: missing") {
				t.Fatalf("foreign directives substituted for standard ownership: %+v", check)
			}
			if after, err := os.ReadFile(root); err != nil || string(after) != test.content {
				t.Fatalf("inspection changed root: %q, %v", after, err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)); !os.IsNotExist(err) {
				t.Fatalf("inspection created snippet: %v", err)
			}
		})
	}
}

func TestInspectSwayConfigSupportedSiblingWithoutDirectInclude(t *testing.T) {
	root := writeStandardSwayConfig(t, ShortcutsDefault, false)
	check := inspectSwayConfig(t.Context(), Options{SwayConfigPath: root})[0]
	if check.Status != Warning || check.FixID != swayIntegrationFixID || !check.AdoptionRequired || !containsEvidence(check.Evidence, "standard profile: default shortcuts") {
		t.Fatalf("disconnected standard source was not reported: %+v", check)
	}
}

func writeStandardSwayConfig(t *testing.T, shortcuts ShortcutSelection, include bool) string {
	t.Helper()
	content := "# unrelated main configuration\n"
	if include {
		content = "include config.d/" + doctorSnippetName + "\n"
	}
	root := writeSwayConfig(t, content)
	kinds := []integrationKind{integrationDaemon, integrationRestore}
	if shortcuts == ShortcutsDefault {
		kinds = integrationOrder
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName), renderManagedSnippet("/usr/bin/sway-session", kinds), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
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
	if err := os.Mkdir(filepath.Join(filepath.Dir(path), "config.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func healthySwayConfig() string {
	return "set $mod Mod4\n" +
		"exec --no-startup-id /usr/bin/sway-session daemon\n" +
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
