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
			plan, err := service.Plan(context.Background(), swayIntegrationFixID)
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
			if _, err := os.Stat(filepath.Join(filepath.Dir(root), doctorSnippetName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("earlier snippet was not rolled back: %v", err)
			}
		})
	}
}

func TestRepairDoesNotReplaceConcurrentSnippetCreation(t *testing.T) {
	root := writeSwayConfig(t, "set $mod Mod4\n")
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(context.Background(), swayIntegrationFixID)
	if err != nil {
		t.Fatal(err)
	}
	snippet := filepath.Join(filepath.Dir(root), doctorSnippetName)
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
	plan, err := service.Plan(context.Background(), swayIntegrationFixID)
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
	plan, err := service.Plan(context.Background(), swayIntegrationFixID)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := atomicWriteEdit(plan.edits[0])
	if err != nil {
		t.Fatal(err)
	}
	directory, err := openVerifiedDirectory(filepath.Dir(root), receipt.edit.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	displacedName := ".displaced-concurrent-config"
	displacedPath := filepath.Join(filepath.Dir(root), displacedName)
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
	plan, err := service.Plan(context.Background(), swayIntegrationFixID)
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

func TestRepairRejectsChangedUneditedInclude(t *testing.T) {
	root := writeSwayConfig(t, "include settings.conf\ninclude 50-sway-session-doctor.conf\n")
	settings := filepath.Join(filepath.Dir(root), "settings.conf")
	if err := os.WriteFile(settings, []byte("set $mod Mod4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(context.Background(), swayIntegrationFixID)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.edits) != 1 {
		t.Fatal("test requires root to remain unedited")
	}
	if err := os.WriteFile(settings, []byte("set $mod Mod1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Apply(context.Background(), plan); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("changed include did not invalidate preview: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), doctorSnippetName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale repair wrote snippet: %v", err)
	}
}

func TestRollbackRestoresExistingManagedSnippet(t *testing.T) {
	root := writeSwayConfig(t, "set $mod Mod4\ninclude 50-sway-session-doctor.conf\n")
	snippet := filepath.Join(filepath.Dir(root), doctorSnippetName)
	original := renderManagedSnippet("/usr/bin/sway-session", []integrationKind{integrationDaemon})
	if err := os.WriteFile(snippet, original, 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(context.Background(), swayIntegrationFixID)
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
