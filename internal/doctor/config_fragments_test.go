package doctor

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newFragmentConfig(t *testing.T, content string) (string, string) {
	t.Helper()
	root := writeSwayConfig(t, content)
	return root, filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)
}

func TestFragmentAdoptionAndProfileBackupsStayOutsideGlob(t *testing.T) {
	root, snippet := newFragmentConfig(t, "set $mod Mod4\ninclude config.d/*\n")
	original, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 2 || plan.Changes[0].Path != snippet {
		t.Fatalf("new setup must select the existing config.d: %+v", plan)
	}
	if _, err := os.Stat(snippet); !os.IsNotExist(err) {
		t.Fatalf("preview created its target: %v", err)
	}
	result, err := service.Apply(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	assertFragmentBackup(t, result, filepath.Dir(root), original)
	main, err := os.ReadFile(root)
	if err != nil || !bytes.HasPrefix(main, original) || literalDirectInclude(main, root, snippet) != 3 {
		t.Fatalf("glob was replaced or literal include was not appended: %q, %v", main, err)
	}
	for _, selection := range []ShortcutSelection{ShortcutsDefault, ShortcutsNone, ShortcutsDefault} {
		before, err := os.ReadFile(snippet)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{Shortcuts: selection})
		if err != nil || len(plan.Changes) != 1 || plan.Changes[0].Path != snippet {
			t.Fatalf("profile switch escaped the owned fragment: %+v, %v", plan, err)
		}
		result, err := service.Apply(t.Context(), plan)
		if err != nil {
			t.Fatal(err)
		}
		assertFragmentBackup(t, result, filepath.Dir(root), before)
		if after, err := os.ReadFile(root); err != nil || !bytes.Equal(after, main) {
			t.Fatalf("profile switch touched main: %q, %v", after, err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(snippet))
	if err != nil || len(entries) != 1 || entries[0].Name() != doctorSnippetName {
		t.Fatalf("wildcard directory contains backups or temporary files: %+v, %v", entries, err)
	}
	check := inspectSwayConfig(t.Context(), Options{SwayConfigPath: root})[0]
	if check.Status != OK || check.AdoptionRequired || check.FixID == "" {
		t.Fatalf("fragment not recognized after repeated edits: %+v", check)
	}
	assertMode(t, snippet, 0o600)
}

func assertFragmentBackup(t *testing.T, result FixResult, directory string, original []byte) {
	t.Helper()
	if len(result.Backups) != 1 || filepath.Dir(result.Backups[0]) != directory {
		t.Fatalf("backup must be next to main, outside config.d: %+v", result)
	}
	content, err := os.ReadFile(result.Backups[0])
	if err != nil || !bytes.Equal(content, original) {
		t.Fatalf("backup lost original bytes: %q, %v", content, err)
	}
	assertMode(t, result.Backups[0], 0o600)
}

func TestFragmentSiblingIsOutsideInspectionBoundary(t *testing.T) {
	root, fragment := newFragmentConfig(t, "set $mod Mod4\n")
	sibling := filepath.Join(filepath.Dir(root), doctorSnippetName)
	original := []byte("# foreign file, never managed\n")
	if err := os.WriteFile(sibling, original, 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
	if err != nil || plan.Changes[0].Path != fragment {
		t.Fatalf("wrong fixed path: %+v, %v", plan, err)
	}
	if _, err := service.Apply(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(sibling); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("foreign sibling changed: %q, %v", got, err)
	}
	analysis, err := analyzeSwayConfig(t.Context(), Options{SwayConfigPath: root})
	if err != nil || len(analysis.observed) != 2 {
		t.Fatalf("unexpected source boundary: %+v, %v", analysis.observed, err)
	}
	for _, observed := range analysis.observed {
		if observed.path == sibling {
			t.Fatal("foreign sibling was inspected")
		}
	}
}

func TestFragmentUnsafeDirectoryCannotRedirectAdoption(t *testing.T) {
	for _, name := range []string{"symlink", "group writable", "file", "absent explicitly included directory"} {
		t.Run(name, func(t *testing.T) {
			root := writeSwayConfig(t, "include config.d/"+doctorSnippetName+"\n")
			directory := filepath.Join(filepath.Dir(root), "config.d")
			if err := os.Remove(directory); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "symlink":
				if err := os.Symlink(t.TempDir(), directory); err != nil {
					t.Fatal(err)
				}
			case "group writable":
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(directory, 0o720); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(directory, []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			check := inspectSwayConfig(t.Context(), Options{SwayConfigPath: root})[0]
			if check.FixID != "" {
				t.Fatalf("unsafe or absent fragment directory offered repair: %+v", check)
			}
			if _, err := New(Options{SwayConfigPath: root}).Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true}); err == nil {
				t.Fatal("unsafe fragment directory yielded plan")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(root), doctorSnippetName)); !os.IsNotExist(err) {
				t.Fatalf("repair silently redirected to sibling: %v", err)
			}
		})
	}
}

func TestFragmentRollsBackAcrossDirectoriesAfterConcurrentMainSave(t *testing.T) {
	root, fragment := newFragmentConfig(t, "set $mod Mod4\n")
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
	if err != nil {
		t.Fatal(err)
	}
	concurrent := []byte("set $mod Mod4\n# concurrent edit\n")
	mutateRepairStaging(t, 3, func() {
		if err := os.WriteFile(root, concurrent, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := service.Apply(t.Context(), plan); err == nil || !strings.Contains(err.Error(), "concurrent configuration was restored") {
		t.Fatalf("main save not preserved: %v", err)
	}
	if got, err := os.ReadFile(root); err != nil || !bytes.Equal(got, concurrent) {
		t.Fatalf("concurrent main lost: %q, %v", got, err)
	}
	if _, err := os.Stat(fragment); !os.IsNotExist(err) {
		t.Fatalf("fragment not rolled back: %v", err)
	}
}

func TestFragmentDirectoryReplacementDuringStagingIsRejected(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing fragment", true: "profile switch"}[existing], func(t *testing.T) {
			root, fragment := newFragmentConfig(t, "include config.d/"+doctorSnippetName+"\n")
			original := renderManagedSnippet("/usr/bin/sway-session", []integrationKind{integrationDaemon, integrationRestore})
			if existing {
				if err := os.WriteFile(fragment, original, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			service := New(Options{SwayConfigPath: root})
			plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true, Shortcuts: ShortcutsDefault})
			if err != nil || len(plan.edits) != 1 {
				t.Fatalf("expected one fragment edit: %+v, %v", plan, err)
			}
			directory := filepath.Dir(fragment)
			stagingCall := 1
			if existing {
				stagingCall = 2
			}
			concurrent := []byte("# user replacement file\n")
			mutateRepairStaging(t, stagingCall, func() {
				if err := os.Rename(directory, directory+".previous"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
				if existing {
					if err := os.WriteFile(fragment, concurrent, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			})
			if _, err := service.Apply(t.Context(), plan); err == nil || !strings.Contains(err.Error(), "parent directory was replaced") {
				t.Fatalf("directory replacement was not rejected: %v", err)
			}
			displaced := filepath.Join(directory+".previous", doctorSnippetName)
			if existing {
				if got, err := os.ReadFile(fragment); err != nil || !bytes.Equal(got, concurrent) {
					t.Fatalf("user replacement changed: %q, %v", got, err)
				}
				if got, err := os.ReadFile(displaced); err != nil || !bytes.Equal(got, original) {
					t.Fatalf("retired profile changed: %q, %v", got, err)
				}
			} else {
				for _, path := range []string{fragment, displaced} {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("repair installed into a replaced directory: %s, %v", path, err)
					}
				}
			}
		})
	}
}

func TestFragmentDirectoryReplacementPreventsProvenRollback(t *testing.T) {
	root, fragment := newFragmentConfig(t, "include config.d/"+doctorSnippetName+"\n")
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := atomicWriteEdit(plan.edits[0])
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(fragment)
	if err := os.Rename(directory, directory+".previous"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	err = service.rollbackFailure([]appliedFileEdit{receipt}, nil, os.ErrNotExist)
	if err == nil || !strings.Contains(err.Error(), "rollback could not be proven complete") {
		t.Fatalf("rollback reported false completion: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory+".previous", doctorSnippetName)); err != nil {
		t.Fatalf("displaced installed file was lost: %v", err)
	}
}
