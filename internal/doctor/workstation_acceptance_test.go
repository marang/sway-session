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
			if !strings.Contains(strings.Join(check.Evidence, "\n"), label) {
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
