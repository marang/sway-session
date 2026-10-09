package doctor

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This is a whole ordinary configuration with prior foreign integration. The
// owned standard is reported as source evidence without an effective-state claim.
func TestDoctorOrdinaryWorkstationConfiguration(t *testing.T) {
	root := copyWorkstationFixture(t)
	main := filepath.Join(root, "config")
	before := snapshotWorkstationFiles(t, root)
	service := New(Options{SwayConfigPath: main})
	check := workstationIntegrationCheck(t, service)
	if check.Status != OK || check.FixID != swayIntegrationFixID || check.AdoptionRequired {
		t.Fatalf("ordinary workstation lost supported owned profile: %+v", check)
	}
	if !containsEvidence(check.Evidence, "standard profile: default shortcuts") || !containsEvidence(check.Evidence, "literal direct include: present") {
		t.Fatalf("report lost bounded source evidence: %+v", check)
	}
	if !strings.Contains(check.Hint, "does not establish effective load order or live binding activity") {
		t.Fatalf("source diagnosis claimed effective state: %+v", check)
	}
	assertWorkstationFiles(t, root, before)
}

func TestDoctorWorkstationForeignSourcesCannotSubstituteForStandard(t *testing.T) {
	root := copyWorkstationFixture(t)
	if err := os.Remove(filepath.Join(root, "config.d", doctorSnippetName)); err != nil {
		t.Fatal(err)
	}
	before := snapshotWorkstationFiles(t, root)
	service := New(Options{SwayConfigPath: filepath.Join(root, "config")})
	check := workstationIntegrationCheck(t, service)
	if check.Status != Warning || !check.AdoptionRequired || !containsEvidence(check.Evidence, "standard file: missing") {
		t.Fatalf("prior foreign integration substituted for owned source: %+v", check)
	}
	if _, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{}); err == nil {
		t.Fatal("foreign configuration granted adoption")
	}
	assertWorkstationFiles(t, root, before)
	plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Path != filepath.Join(root, "config.d", doctorSnippetName) {
		t.Fatalf("recovery escaped standard sibling: %+v", plan)
	}
	if _, err := service.Apply(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	assertWorkstationFiles(t, root, before)
	if check := workstationIntegrationCheck(t, service); check.Status != OK || !containsEvidence(check.Evidence, "standard profile: startup-only") {
		t.Fatalf("recovery did not default to startup-only: %+v", check)
	}
}

func TestDoctorWorkstationManualOwnedShortcutRemovalIsProtected(t *testing.T) {
	root := copyWorkstationFixture(t)
	snippet := filepath.Join(root, "config.d", doctorSnippetName)
	original, err := os.ReadFile(snippet)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.ReplaceAll(string(original), "bindsym $mod+Shift+Return exec --no-startup-id /usr/bin/sway-session terminal --ephemeral\n", "")
	if err := os.WriteFile(snippet, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotWorkstationFiles(t, root)
	service := New(Options{SwayConfigPath: filepath.Join(root, "config")})
	check := workstationIntegrationCheck(t, service)
	if check.Status != Unavailable || check.FixID != "" {
		t.Fatalf("partial owned profile offered regeneration: %+v", check)
	}
	if _, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true, Shortcuts: ShortcutsDefault}); err == nil {
		t.Fatal("adoption overwrote protected partial profile")
	}
	assertWorkstationFiles(t, root, before)
}

func TestDoctorWorkstationForeignShellBindingsAndUnsafeIncludesStayOpaque(t *testing.T) {
	root := copyWorkstationFixture(t)
	main := filepath.Join(root, "config")
	if err := os.Symlink(filepath.Join(root, "conf.d", "10-startup.conf"), filepath.Join(root, "uninspectable.conf")); err != nil {
		t.Fatal(err)
	}
	appendWorkstationConfig(t, main, "bindcode Mod4+36 exec foot\nunbindsym Mod4+Return\nexec sh -c 'sway-session daemon'\ninclude missing.d/*.conf\ninclude uninspectable.conf\ninclude $unknown\n")
	service := New(Options{SwayConfigPath: main})
	check := workstationIntegrationCheck(t, service)
	if check.Status != OK || check.AdoptionRequired {
		t.Fatalf("foreign source altered owned-source diagnosis: %+v", check)
	}
	for _, forbidden := range []string{"matching declaration", "conflicting declaration", "followed", "could not be fully checked"} {
		if containsEvidence(check.Evidence, forbidden) {
			t.Fatalf("report reintroduced foreign source interpretation: %+v", check)
		}
	}
}

func snapshotWorkstationFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[path] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertWorkstationFiles(t *testing.T, root string, before map[string]string) {
	t.Helper()
	for path, want := range before {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("workstation file changed: %s: %q, %v", path, got, err)
		}
	}
}

func appendWorkstationConfig(t *testing.T, path, extra string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(content, []byte(extra)...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func workstationIntegrationCheck(t *testing.T, service *Service) Check {
	t.Helper()
	for _, check := range service.Check(context.Background()).Checks {
		if check.ID == swayIntegrationFixID {
			return check
		}
	}
	t.Fatal("public doctor report omitted Sway integration")
	return Check{}
}

func copyWorkstationFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR"} {
		dir := filepath.Join(root, key)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, dir)
	}
	t.Setenv("SWAYSOCK", "")
	t.Setenv("I3SOCK", "")
	t.Setenv("HERDR_CONFIG_PATH", filepath.Join(root, "missing-herdr.toml"))
	fixture := filepath.Join("testdata", "workstation")
	err := filepath.WalkDir(fixture, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(fixture, path)
		if err != nil {
			return err
		}
		target := filepath.Join(root, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}
