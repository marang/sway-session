package doctor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAnalyzeSwayConfigNeverFollowsForeignIncludes(t *testing.T) {
	root := writeStandardSwayConfig(t, ShortcutsNone, true)
	directory := filepath.Dir(root)
	fifo := filepath.Join(directory, "blocked.conf")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "linked.conf")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(directory, "private.conf")
	if err := os.WriteFile(foreign, []byte("private-foreign-source-value\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	content := "include " + doctorSnippetName + "\ninclude blocked.conf\ninclude linked.conf\ninclude private.conf\ninclude missing.conf\ninclude $unknown\ninclude *.conf\ninclude config\n"
	if err := os.WriteFile(root, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	analysis, err := analyzeSwayConfig(t.Context(), Options{SwayConfigPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(analysis.observed) != 2 || analysis.observed[0].path != root || analysis.observed[1].path != filepath.Join(directory, doctorSnippetName) {
		t.Fatalf("inspection escaped two-file boundary: %+v", analysis.observed)
	}
	check := inspectSwayConfig(t.Context(), Options{SwayConfigPath: root})[0]
	if check.Status != OK || strings.Contains(strings.Join(check.Evidence, "\n")+check.Hint, "private-foreign-source-value") {
		t.Fatalf("foreign source affected report: %+v", check)
	}
}

func TestInspectSwayConfigRejectsUnsafeStandardSibling(t *testing.T) {
	for _, name := range []string{"symlink", "hardlink", "group writable", "fifo", "oversized"} {
		t.Run(name, func(t *testing.T) {
			root := writeSwayConfig(t, "# root\n")
			snippet := filepath.Join(filepath.Dir(root), doctorSnippetName)
			switch name {
			case "symlink":
				if err := os.Symlink(root, snippet); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(root, snippet); err != nil {
					t.Fatal(err)
				}
			case "group writable":
				if err := os.WriteFile(snippet, renderManagedSnippet("/usr/bin/sway-session", integrationOrder), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(snippet, 0o620); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(snippet, 0o600); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.WriteFile(snippet, bytes.Repeat([]byte("x"), maxSwayConfigBytes+1), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			assertUnavailableQuickly(t, root)
			if _, err := New(Options{SwayConfigPath: root}).Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true}); err == nil {
				t.Fatal("unsafe sibling offered repair")
			}
		})
	}
}

func TestInspectSwayConfigRejectsOversizedMainAndAbsentMain(t *testing.T) {
	root := writeSwayConfig(t, strings.Repeat("x", maxSwayConfigBytes+1))
	assertUnavailableQuickly(t, root)
	missing := filepath.Join(t.TempDir(), "config")
	assertUnavailableQuickly(t, missing)
	if _, err := New(Options{SwayConfigPath: missing}).Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true}); err == nil {
		t.Fatal("adoption created absent main")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("absent main created: %v", err)
	}
}

func TestInspectSwayConfigRejectsSelectedManagedSibling(t *testing.T) {
	root := writeStandardSwayConfig(t, ShortcutsNone, true)
	snippet := filepath.Join(filepath.Dir(root), doctorSnippetName)
	check := inspectSwayConfig(t.Context(), Options{SwayConfigPath: snippet})[0]
	if check.Status != Unavailable || check.FixID != "" || !strings.Contains(check.Hint, "cannot be the doctor-managed snippet") {
		t.Fatalf("selected sibling became main: %+v", check)
	}
}

func TestRepairRequiresSafeDirectory(t *testing.T) {
	root := writeSwayConfig(t, "# root\n")
	if err := os.Chmod(filepath.Dir(root), 0o722); err != nil {
		t.Fatal(err)
	}
	check := inspectSwayConfig(t.Context(), Options{SwayConfigPath: root})[0]
	if check.FixID != "" {
		t.Fatalf("unsafe repair directory offered fix: %+v", check)
	}
	if _, err := New(Options{SwayConfigPath: root}).Plan(context.Background(), swayIntegrationFixID, RepairOptions{AdoptStandard: true}); err == nil {
		t.Fatal("unsafe directory offered repair")
	}
}

func TestRepairUnavailableFixIDsMatchExecutableAndAppendSafety(t *testing.T) {
	for _, setup := range []func(*testing.T) Options{
		func(t *testing.T) Options {
			return Options{SwayConfigPath: writeSwayConfig(t, "# root\n"), Executable: "/usr/bin/sway-session;true"}
		},
		func(t *testing.T) Options {
			return Options{SwayConfigPath: writeSwayConfig(t, strings.Repeat("#\n", maxSwayConfigBytes/2))}
		},
		func(t *testing.T) Options {
			return Options{SwayConfigPath: writeSwayConfig(t, "exec notify-send \\\n")}
		},
	} {
		options := setup(t)
		check := inspectSwayConfig(t.Context(), options)[0]
		if check.FixID != "" || check.AdoptionRequired {
			t.Fatalf("impossible or unsafe repair offered actionable check: %+v", check)
		}
		if _, err := New(options).Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true}); err == nil {
			t.Fatal("unsafe repair boundary offered plan")
		}
	}
}
