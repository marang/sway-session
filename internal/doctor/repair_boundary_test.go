package doctor

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Inject an editor save during staging, after the last pre-write validation.
// These tests are deliberately serial because crypto/rand.Reader is global.
type repairMutationReader struct {
	count  int
	at     int
	mutate func()
}

func (reader *repairMutationReader) Read(content []byte) (int, error) {
	reader.count++
	if reader.count == reader.at {
		reader.mutate()
	}
	for index := range content {
		content[index] = byte(reader.count)
	}
	return len(content), nil
}

func mutateRepairStaging(t *testing.T, at int, mutate func()) {
	t.Helper()
	original := rand.Reader
	rand.Reader = &repairMutationReader{at: at, mutate: mutate}
	t.Cleanup(func() { rand.Reader = original })
}

func TestRepairPreservesConcurrentRootSave(t *testing.T) {
	for _, atomic := range []bool{false, true} {
		name := "in place"
		if atomic {
			name = "atomic replacement"
		}
		t.Run(name, func(t *testing.T) {
			root := writeSwayConfig(t, "set $mod Mod4\n")
			service := New(Options{SwayConfigPath: root})
			plan, err := service.Plan(context.Background(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
			if err != nil {
				t.Fatal(err)
			}
			concurrent := []byte("set $mod Mod4\n# concurrent user edit\n")
			mutateRepairStaging(t, 3, func() {
				path := root
				if atomic {
					path += ".editor-save"
				}
				if err := os.WriteFile(path, concurrent, 0o600); err != nil {
					t.Fatal(err)
				}
				if atomic {
					if err := os.Rename(path, root); err != nil {
						t.Fatal(err)
					}
				}
			})
			if _, err := service.Apply(context.Background(), plan); err == nil || !strings.Contains(err.Error(), "concurrent configuration was restored") {
				t.Fatalf("concurrent save was not detected/restored: %v", err)
			}
			content, err := os.ReadFile(root)
			if err != nil || !bytes.Equal(content, concurrent) {
				t.Fatalf("concurrent save lost: %q, %v", content, err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("earlier snippet was not rolled back: %v", err)
			}
		})
	}
}

func TestRepairDoesNotReplaceConcurrentSnippetCreation(t *testing.T) {
	root := writeSwayConfig(t, "set $mod Mod4\n")
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(context.Background(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
	if err != nil {
		t.Fatal(err)
	}
	snippet := filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)
	mutateRepairStaging(t, 2, func() {
		if err := os.WriteFile(snippet, []byte("# user created this file\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := service.Apply(context.Background(), plan); err == nil {
		t.Fatal("concurrent creation was replaced")
	}
	content, err := os.ReadFile(snippet)
	if err != nil || string(content) != "# user created this file\n" {
		t.Fatalf("concurrent creation lost: %q, %v", content, err)
	}
	content, err = os.ReadFile(root)
	if err != nil || string(content) != "set $mod Mod4\n" {
		t.Fatalf("root changed despite failed first write: %q, %v", content, err)
	}
}

func TestRollbackDoesNotRemoveEqualContentFromAnotherWriter(t *testing.T) {
	root := writeSwayConfig(t, "set $mod Mod4\n")
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(context.Background(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := atomicWriteEdit(plan.edits[0])
	if err != nil {
		t.Fatal(err)
	}
	editorPath := receipt.edit.path + ".editor-save"
	if err := os.WriteFile(editorPath, receipt.edit.newContent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(editorPath, receipt.edit.path); err != nil {
		t.Fatal(err)
	}
	_, before, err := readSafeConfigFile(receipt.edit.path)
	if err != nil {
		t.Fatal(err)
	}
	err = service.rollbackFailure([]appliedFileEdit{receipt}, nil, errors.New("later write failed"))
	if !strings.Contains(err.Error(), "rollback could not be proven complete") {
		t.Fatalf("rollback claimed ownership of replacement: %v", err)
	}
	_, after, err := readSafeConfigFile(receipt.edit.path)
	if err != nil || before != after {
		t.Fatalf("rollback removed/replaced another writer's file: %v", err)
	}
}

func TestFailedCompensationPreservesBothConcurrentFiles(t *testing.T) {
	root := writeSwayConfig(t, "set $mod Mod4\n")
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(context.Background(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := atomicWriteEdit(plan.edits[0])
	if err != nil {
		t.Fatal(err)
	}
	directory, err := openVerifiedDirectory(filepath.Dir(receipt.edit.path), receipt.edit.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	displacedName := ".displaced-concurrent-config"
	displacedPath := filepath.Join(filepath.Dir(receipt.edit.path), displacedName)
	if err := os.WriteFile(displacedPath, []byte("# first concurrent save\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receipt.edit.path, []byte("# second concurrent save\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cause := restoreDisplacedFile(directory, displacedName, filepath.Base(receipt.edit.path), receipt)
	err = service.rollbackFailure(nil, nil, cause)
	if !strings.Contains(err.Error(), displacedPath) || !strings.Contains(err.Error(), "rollback could not be proven complete") {
		t.Fatalf("missing recovery guidance: %v", err)
	}
	current, err := os.ReadFile(receipt.edit.path)
	if err != nil || string(current) != "# second concurrent save\n" {
		t.Fatalf("second save lost: %q, %v", current, err)
	}
	displaced, err := os.ReadFile(displacedPath)
	if err != nil || string(displaced) != "# first concurrent save\n" {
		t.Fatalf("first save lost: %q, %v", displaced, err)
	}
}

func TestRollbackPreservesReplacementDuringRemoval(t *testing.T) {
	root := writeSwayConfig(t, "set $mod Mod4\n")
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(context.Background(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := atomicWriteEdit(plan.edits[0])
	if err != nil {
		t.Fatal(err)
	}
	mutateRepairStaging(t, 1, func() {
		if err := os.WriteFile(receipt.edit.path, []byte("# concurrent save during removal\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	err = service.rollbackFailure([]appliedFileEdit{receipt}, nil, errors.New("later edit failed"))
	if !strings.Contains(err.Error(), "rollback could not be proven complete") {
		t.Fatalf("unexpected rollback outcome: %v", err)
	}
	content, err := os.ReadFile(receipt.edit.path)
	if err != nil || string(content) != "# concurrent save during removal\n" {
		t.Fatalf("concurrent save lost: %q, %v", content, err)
	}
}

func TestRepairIgnoresChangedForeignInclude(t *testing.T) {
	root := writeSwayConfig(t, "include settings.conf\ninclude config.d/50-sway-session-doctor.conf\n")
	settings := filepath.Join(filepath.Dir(root), "settings.conf")
	if err := os.WriteFile(settings, []byte("set $mod Mod4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(context.Background(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.edits) != 1 {
		t.Fatal("test requires root to remain unedited")
	}
	if err := os.WriteFile(settings, []byte("set $mod Mod1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Apply(context.Background(), plan); err != nil {
		t.Fatalf("foreign include unexpectedly invalidated bounded preview: %v", err)
	}
	if after, err := os.ReadFile(settings); err != nil || string(after) != "set $mod Mod1\n" {
		t.Fatalf("foreign include changed: %q, %v", after, err)
	}
}

func TestRollbackRestoresExistingManagedSnippet(t *testing.T) {
	root := writeSwayConfig(t, "set $mod Mod4\ninclude config.d/50-sway-session-doctor.conf\n")
	snippet := filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)
	original := renderManagedSnippet("/usr/bin/sway-session", []integrationKind{integrationDaemon, integrationRestore})
	if err := os.WriteFile(snippet, original, 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(context.Background(), swayIntegrationFixID, RepairOptions{Shortcuts: ShortcutsDefault})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := atomicWriteEdit(plan.edits[0])
	if err != nil {
		t.Fatal(err)
	}
	err = service.rollbackFailure([]appliedFileEdit{receipt}, nil, errors.New("later operation failed"))
	if !strings.Contains(err.Error(), "applied files were rolled back") {
		t.Fatalf("rollback failed: %v", err)
	}
	content, err := os.ReadFile(snippet)
	if err != nil || !bytes.Equal(content, original) {
		t.Fatalf("original snippet was not restored: %q, %v", content, err)
	}
}

func TestRepairPreservesManualManagedSnippetText(t *testing.T) {
	for name, example := range map[string]struct {
		line      string
		wantError string
	}{
		"trailing hash argument": {"exec --no-startup-id /usr/bin/sway-session daemon # deliberate user note", "manual edits"},
		"added whitespace":       {"exec  --no-startup-id /usr/bin/sway-session daemon", "manual edits"},
		"quoted executable":      {"exec --no-startup-id \"/usr/bin/sway-session\" daemon", "manual edits"},
	} {
		t.Run(name, func(t *testing.T) {
			root := writeSwayConfig(t, "set $mod Mod4\ninclude config.d/50-sway-session-doctor.conf\n")
			snippet := filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)
			original := []byte(doctorHeader + example.line + "\n")
			if err := os.WriteFile(snippet, original, 0o600); err != nil {
				t.Fatal(err)
			}
			service := New(Options{SwayConfigPath: root})
			if _, err := service.Plan(context.Background(), swayIntegrationFixID, RepairOptions{AdoptStandard: true}); err == nil || !strings.Contains(err.Error(), example.wantError) {
				t.Fatalf("manual text was accepted for regeneration: %v", err)
			}
			if check := inspectSwayConfig(context.Background(), service.options)[0]; check.FixID != "" {
				t.Fatalf("inspection offered unsafe repair: %+v", check)
			}
			content, err := os.ReadFile(snippet)
			if err != nil || !bytes.Equal(content, original) {
				t.Fatalf("manual snippet was modified: %q, %v", content, err)
			}
		})
	}
}

func TestRepairRejectsChangedUneditedMainAndManagedSnippet(t *testing.T) {
	for _, changed := range []string{"main", "snippet"} {
		t.Run(changed, func(t *testing.T) {
			root := writeStandardSwayConfig(t, ShortcutsNone, true)
			snippet := filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)
			service := New(Options{SwayConfigPath: root})
			plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{Shortcuts: ShortcutsDefault})
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.edits) != 1 || plan.edits[0].path != snippet {
				t.Fatalf("expected root to remain unedited: %+v", plan)
			}
			path := root
			data := []byte("# concurrent root save\ninclude config.d/" + doctorSnippetName + "\n")
			if changed == "snippet" {
				path = snippet
				data = append(renderManagedSnippet("/usr/bin/sway-session", []integrationKind{integrationDaemon, integrationRestore}), []byte("# concurrent manual edit\n")...)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			beforeRoot, _ := os.ReadFile(root)
			beforeSnippet, _ := os.ReadFile(snippet)
			if _, err := service.Apply(t.Context(), plan); err == nil || !strings.Contains(err.Error(), "stale") {
				t.Fatalf("changed %s accepted stale plan: %v", changed, err)
			}
			if after, err := os.ReadFile(root); err != nil || !bytes.Equal(after, beforeRoot) {
				t.Fatalf("stale apply changed root: %q, %v", after, err)
			}
			if after, err := os.ReadFile(snippet); err != nil || !bytes.Equal(after, beforeSnippet) {
				t.Fatalf("stale apply changed snippet: %q, %v", after, err)
			}
			entries, err := os.ReadDir(filepath.Dir(root))
			if err != nil || len(entries) != 2 {
				t.Fatalf("stale apply created extra files: %v, %v", entries, err)
			}
		})
	}
}

func TestRepairManualOwnershipAndDirectiveEditsStayProtected(t *testing.T) {
	original := renderManagedSnippet("/usr/bin/sway-session", integrationOrder)
	for name, edited := range map[string][]byte{
		"header removed":      bytes.TrimPrefix(original, []byte(doctorHeader)),
		"format changed":      bytes.Replace(original, []byte("format: 1"), []byte("format: 2"), 1),
		"comment appended":    append(bytes.Clone(original), []byte("# manual note\n")...),
		"missing newline":     bytes.TrimSuffix(original, []byte("\n")),
		"extra whitespace":    bytes.Replace(original, []byte("exec --"), []byte("exec  --"), 1),
		"reordered startup":   []byte(doctorHeader + integrationDirective("/usr/bin/sway-session", integrationRestore) + "\n" + integrationDirective("/usr/bin/sway-session", integrationDaemon) + "\n"),
		"duplicate directive": append(bytes.Clone(original), []byte(integrationDirective("/usr/bin/sway-session", integrationDaemon)+"\n")...),
		"mixed executables":   bytes.Replace(original, []byte("/usr/bin/sway-session restore"), []byte("/custom/sway-session restore"), 1),
		"exec always":         bytes.Replace(original, []byte("exec --"), []byte("exec_always --"), 1),
	} {
		t.Run(name, func(t *testing.T) {
			root := writeSwayConfig(t, "include config.d/"+doctorSnippetName+"\n")
			snippet := filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)
			if err := os.WriteFile(snippet, edited, 0o600); err != nil {
				t.Fatal(err)
			}
			service := New(Options{SwayConfigPath: root})
			check := inspectSwayConfig(t.Context(), service.options)[0]
			if check.Status != Unavailable || check.FixID != "" {
				t.Fatalf("manual ownership edit gained repair authority: %+v", check)
			}
			if _, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true, Shortcuts: ShortcutsNone}); err == nil {
				t.Fatal("adoption overwrote manual file")
			}
			if after, err := os.ReadFile(snippet); err != nil || !bytes.Equal(after, edited) {
				t.Fatalf("manual file changed: %q, %v", after, err)
			}
		})
	}
}

func TestRepairPreservesConcurrentSnippetReplacementBeforeMainSwap(t *testing.T) {
	root := writeSwayConfig(t, "# selected main\n")
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
	if err != nil {
		t.Fatal(err)
	}
	snippet := filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)
	concurrent := []byte("# concurrent user-owned integration\n")
	// Root backup, snippet staging, then main staging.
	mutateRepairStaging(t, 3, func() {
		path := snippet + ".editor-save"
		if err := os.WriteFile(path, concurrent, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path, snippet); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := service.Apply(t.Context(), plan); err == nil || !strings.Contains(err.Error(), "observed configuration changed") || !strings.Contains(err.Error(), "rollback could not be proven complete") {
		t.Fatalf("concurrent replacement not preserved=%v", err)
	}
	content, _ := os.ReadFile(snippet)
	if !bytes.Equal(content, concurrent) {
		t.Fatalf("concurrent snippet lost=%q", content)
	}
	content, _ = os.ReadFile(root)
	if string(content) != "# selected main\n" {
		t.Fatalf("main include appended despite changed snippet=%q", content)
	}
}
