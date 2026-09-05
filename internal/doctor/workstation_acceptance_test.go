package doctor

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This is intentionally a whole ordinary config, not a minimal parser example.
// The public report must remain useful without changing a user's display setup.
func TestDoctorOrdinaryWorkstationConfiguration(t *testing.T) {
	root := copyWorkstationFixture(t)
	checks := New(Options{SwayConfigPath: filepath.Join(root, "config")}).Check(context.Background()).Checks
	var found bool
	for _, check := range checks {
		if check.ID != swayIntegrationFixID {
			continue
		}
		found = true
		if check.Status != OK || check.FixID != "" {
			t.Fatalf("normal Sway blocks must not disable integration diagnosis: %+v", check)
		}
		for _, label := range []string{"daemon startup", "restore startup", "persistent-terminal shortcut", "ephemeral-terminal shortcut"} {
			if !evidenceLineContains(check.Evidence, label, "present") {
				t.Errorf("missing independently useful %s evidence: %+v", label, check)
			}
		}
	}
	if !found {
		t.Fatal("public doctor report omitted Sway integration")
	}
	if _, err := os.Stat(filepath.Join(root, doctorSnippetName)); !os.IsNotExist(err) {
		t.Fatalf("read-only check created repair snippet: %v", err)
	}
}

func TestDoctorWorkstationPartialEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		extra   string
		known   []string
		unknown []string
	}{
		{
			name:    "keycode binding does not erase startup evidence",
			extra:   "bindcode Mod4+36 exec /usr/bin/foot\n",
			known:   []string{"daemon startup", "restore startup"},
			unknown: []string{"persistent-terminal shortcut", "ephemeral-terminal shortcut"},
		},
		{
			name:    "indirect startup does not erase shortcut evidence",
			extra:   "exec sh -c 'sway-session daemon'\n",
			known:   []string{"persistent-terminal shortcut", "ephemeral-terminal shortcut"},
			unknown: []string{"daemon startup", "restore startup"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := copyWorkstationFixture(t)
			path := filepath.Join(root, "config")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			content := append(original, []byte(tc.extra)...)
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			service := New(Options{SwayConfigPath: path})
			check := workstationIntegrationCheck(t, service)
			if check.Status != Warning || !strings.Contains(check.Detail, "partially checked") || check.FixID != "" {
				t.Fatalf("partial useful diagnosis must not look wholly unavailable or repairable: %+v", check)
			}
			for _, label := range tc.known {
				if !evidenceLineContains(check.Evidence, label, "present") {
					t.Errorf("lost independently confirmed %s: %+v", label, check)
				}
			}
			for _, label := range tc.unknown {
				if !evidenceLineContains(check.Evidence, label, "could not be fully checked") {
					t.Errorf("concealed uncertainty for %s: %+v", label, check)
				}
			}
			if _, err := service.Plan(context.Background(), swayIntegrationFixID); err == nil {
				t.Fatal("partial analysis allowed an automatic repair")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(content) {
				t.Fatalf("read-only check/failed preview modified config: %v", err)
			}
		})
	}
}

func TestDoctorWorkstationMissingShortcutOffersRepair(t *testing.T) {
	root := copyWorkstationFixture(t)
	path := filepath.Join(root, "conf.d", "20-terminals.conf")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content = []byte(strings.ReplaceAll(string(content), "bindsym $mod+Shift+Return exec --no-startup-id /usr/bin/sway-session terminal --ephemeral\n", ""))
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: filepath.Join(root, "config")})
	check := workstationIntegrationCheck(t, service)
	if check.Status != Warning || check.FixID != swayIntegrationFixID {
		t.Fatalf("a complete ordinary config with one missing binding should offer repair: %+v", check)
	}
	if !evidenceLineContains(check.Evidence, "ephemeral-terminal shortcut", "missing") {
		t.Fatalf("missing requirement was not identified: %+v", check)
	}
	if _, err := service.Plan(context.Background(), swayIntegrationFixID); err != nil {
		t.Fatalf("safe preview rejected a supported config: %v", err)
	}
}

func TestDoctorWorkstationOptionalUnmatchedInclude(t *testing.T) {
	root := copyWorkstationFixture(t)
	path := filepath.Join(root, "config")
	appendWorkstationConfig(t, path, "include missing.d/*.conf\n")
	check := workstationIntegrationCheck(t, New(Options{SwayConfigPath: path}))
	if check.Status != OK || check.FixID != "" {
		t.Fatalf("Sway's optional unmatched include must not disable a complete diagnosis: %+v", check)
	}
}

func TestDoctorWorkstationUnsafeIncludeRetainsLocatedFacts(t *testing.T) {
	root := copyWorkstationFixture(t)
	path := filepath.Join(root, "config")
	if err := os.Symlink(filepath.Join(root, "conf.d", "10-startup.conf"), filepath.Join(root, "uninspectable.conf")); err != nil {
		t.Fatal(err)
	}
	appendWorkstationConfig(t, path, "include uninspectable.conf\n")
	service := New(Options{SwayConfigPath: path})
	check := workstationIntegrationCheck(t, service)
	if check.Status != Warning || !strings.Contains(check.Detail, "partially checked") || check.FixID != "" {
		t.Fatalf("known declarations must survive incomplete graph inspection without enabling repair: %+v", check)
	}
	for _, label := range []string{"daemon startup", "restore startup", "persistent-terminal shortcut", "ephemeral-terminal shortcut"} {
		if !evidenceLineContains(check.Evidence, label, "could not be fully checked") {
			t.Errorf("incomplete include graph hid uncertainty for %s: %+v", label, check)
		}
	}
	for _, source := range []string{"10-startup.conf:", "20-terminals.conf:"} {
		if !strings.Contains(strings.Join(check.Evidence, "\n"), source) {
			t.Errorf("lost known source location %s: %+v", source, check)
		}
	}
	if _, err := service.Plan(context.Background(), swayIntegrationFixID); err == nil {
		t.Fatal("unsafe included path permitted automatic repair")
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

func evidenceLineContains(lines []string, first, second string) bool {
	for _, line := range lines {
		if strings.Contains(line, first) && strings.Contains(line, second) {
			return true
		}
	}
	return false
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
