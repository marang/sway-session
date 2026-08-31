package statefile

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type legacyTestDocument struct {
	Version int    `json:"version"`
	Legacy  string `json:"legacy"`
}

func decodeTestMigration(data []byte) (testDocument, bool, error) {
	var envelope struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return testDocument{}, false, err
	}
	switch envelope.Version {
	case 0:
		var legacy legacyTestDocument
		if err := decodeStrict(data, &legacy); err != nil {
			return testDocument{}, false, err
		}
		if legacy.Legacy == "" {
			return testDocument{}, false, errors.New("legacy value is required")
		}
		return testDocument{Version: 1, Value: legacy.Legacy}, true, nil
	case 1:
		var current testDocument
		if err := decodeStrict(data, &current); err != nil {
			return testDocument{}, false, err
		}
		return current, false, nil
	default:
		return testDocument{}, false, errors.New("unsupported version")
	}
}

func TestMigratePreservesExactOwnerOnlyBackupBeforeReplacement(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, DirectoryMode); err != nil {
		t.Fatal(err)
	}
	legacy := []byte("{\n  \"version\": 0,\n  \"legacy\": \"preserve me exactly\"\n}\n")
	if err := os.WriteFile(filepath.Join(root, "state.json"), legacy, RegularFileMode); err != nil {
		t.Fatal(err)
	}

	file := NewJSONFile(root, "state.json", validateTestDocument)
	got, migrated, err := file.Migrate(decodeTestMigration, "state.v0.json")
	if err != nil {
		t.Fatalf("migrate state: %v", err)
	}
	want := testDocument{Version: 1, Value: "preserve me exactly"}
	if !migrated || got != want {
		t.Fatalf("unexpected migration result: migrated=%t got=%+v", migrated, got)
	}
	backup, err := os.ReadFile(filepath.Join(root, "state.v0.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backup, legacy) {
		t.Fatalf("migration backup changed source bytes:\n got=%q\nwant=%q", backup, legacy)
	}
	assertMode(t, filepath.Join(root, "state.v0.json"), RegularFileMode)
	var loaded testDocument
	if err := file.LoadInto(&loaded); err != nil || loaded != want {
		t.Fatalf("load migrated state: got=%+v err=%v", loaded, err)
	}

	second, migratedAgain, err := file.Migrate(decodeTestMigration, "state.v0.json")
	if err != nil || migratedAgain || second != want {
		t.Fatalf("migration was not idempotent: migrated=%t got=%+v err=%v", migratedAgain, second, err)
	}
}

func TestMigrateRejectsConflictingOrUnsafeBackup(t *testing.T) {
	for name, prepare := range map[string]func(string) error{
		"different regular file": func(path string) error {
			return os.WriteFile(path, []byte("different"), RegularFileMode)
		},
		"symlink": func(path string) error {
			return os.Symlink("state.json", path)
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(root, DirectoryMode); err != nil {
				t.Fatal(err)
			}
			legacy := []byte(`{"version":0,"legacy":"unchanged"}`)
			statePath := filepath.Join(root, "state.json")
			if err := os.WriteFile(statePath, legacy, RegularFileMode); err != nil {
				t.Fatal(err)
			}
			if err := prepare(filepath.Join(root, "state.v0.json")); err != nil {
				t.Fatal(err)
			}
			file := NewJSONFile(root, "state.json", validateTestDocument)
			if _, _, err := file.Migrate(decodeTestMigration, "state.v0.json"); err == nil {
				t.Fatal("expected unsafe migration backup rejection")
			}
			after, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, legacy) {
				t.Fatalf("legacy source changed after rejected backup: got=%q want=%q", after, legacy)
			}
		})
	}
}

func TestMigrateRejectsMalformedAndUnsupportedSourcesWithoutBackup(t *testing.T) {
	for _, source := range []string{
		`{"version":0,"legacy":"ok","command":"sh"}`,
		`{"version":7,"legacy":"unknown"}`,
		`{"version":`,
	} {
		t.Run(source, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(root, DirectoryMode); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(source), RegularFileMode); err != nil {
				t.Fatal(err)
			}
			file := NewJSONFile(root, "state.json", validateTestDocument)
			if _, _, err := file.Migrate(decodeTestMigration, "state.v0.json"); err == nil {
				t.Fatal("expected invalid migration source rejection")
			}
			if _, err := os.Lstat(filepath.Join(root, "state.v0.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid source created a backup: %v", err)
			}
		})
	}
}

func TestMigrateSerializesConcurrentCallers(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, DirectoryMode); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"version":0,"legacy":"shared"}`)
	if err := os.WriteFile(filepath.Join(root, "state.json"), legacy, RegularFileMode); err != nil {
		t.Fatal(err)
	}
	file := NewJSONFile(root, "state.json", validateTestDocument)
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _, err := file.Migrate(decodeTestMigration, "state.v0.json")
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent migration failed: %v", err)
		}
	}
	backup, err := os.ReadFile(filepath.Join(root, "state.v0.json"))
	if err != nil || !bytes.Equal(backup, legacy) {
		t.Fatalf("concurrent migration backup mismatch: got=%q err=%v", backup, err)
	}
}
