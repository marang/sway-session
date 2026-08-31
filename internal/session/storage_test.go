package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDefaultStateRootUsesAbsoluteXDGStateHome(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)

	root, err := DefaultStateRoot()
	if err != nil {
		t.Fatalf("resolve state root: %v", err)
	}
	if root != filepath.Join(stateHome, "sway-session") {
		t.Fatalf("unexpected state root %q", root)
	}
}

func TestDefaultStateRootRejectsRelativeXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative/state")
	if _, err := DefaultStateRoot(); err == nil {
		t.Fatal("expected relative XDG_STATE_HOME to be rejected")
	}
}

func TestRegistryFileRoundTripAndVersionRejectionPreserveCurrentState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	file := RegistryFile(root)
	want := validRegistry()
	if err := file.Save(want); err != nil {
		t.Fatalf("save registry: %v", err)
	}

	var loaded Registry
	if err := file.LoadInto(&loaded); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if !reflect.DeepEqual(loaded, want) {
		t.Fatalf("unexpected registry: got=%+v want=%+v", loaded, want)
	}

	unsupported := []byte(`{"version":3,"preferences":{"desktop_indicators":false},"contexts":[]}`)
	if err := os.WriteFile(filepath.Join(root, ContextsFilename), unsupported, 0o600); err != nil {
		t.Fatalf("write unsupported registry: %v", err)
	}
	current := loaded
	err := file.LoadInto(&current)
	var versionError *UnsupportedVersionError
	if !errors.As(err, &versionError) {
		t.Fatalf("expected unsupported version error, got %v", err)
	}
	if !reflect.DeepEqual(current, loaded) {
		t.Fatalf("unsupported registry replaced current value: got=%+v want=%+v", current, loaded)
	}
}

func TestVersionedDocumentsRequireTheirTopLevelArrays(t *testing.T) {
	registry := Registry{Version: ContextsSchemaVersion}
	if err := registry.Validate(); err == nil {
		t.Fatal("expected a missing contexts array to be rejected")
	}

	snapshot := LayoutSnapshot{Version: LayoutSchemaVersion}
	if err := snapshot.Validate(); err == nil {
		t.Fatal("expected a missing workspaces array to be rejected")
	}
}

func TestUpdateRegistryCreatesAndModifiesValidState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	updated, err := UpdateRegistry(root, func(registry *Registry) error {
		registry.Contexts = append(registry.Contexts, validRegistry().Contexts[0])
		return nil
	})
	if err != nil {
		t.Fatalf("create registry transactionally: %v", err)
	}
	if !reflect.DeepEqual(updated, validRegistry()) {
		t.Fatalf("unexpected created registry: %+v", updated)
	}

	updated, err = UpdateRegistry(root, func(registry *Registry) error {
		registry.Contexts[0].State = ContextArchived
		return nil
	})
	if err != nil {
		t.Fatalf("modify registry transactionally: %v", err)
	}
	if updated.Contexts[0].State != ContextArchived {
		t.Fatalf("registry mutation was not persisted: %+v", updated)
	}
}

func TestRegistryStoreMigratesV1AndPreservesExactRollbackCopy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte("{\n" +
		"  \"version\": 1,\n" +
		"  \"contexts\": [{\n" +
		"    \"id\": \"123e4567-e89b-12d3-a456-426614174000\",\n" +
		"    \"label\": \"LAB-80\",\n" +
		"    \"provider\": \"linear\",\n" +
		"    \"state\": \"active\",\n" +
		"    \"launcher\": {\"kind\": \"herdr\", \"session\": \"lab-80\", \"cwd\": \"/home/example/work\"}\n" +
		"  }]\n" +
		"}\n")
	if err := os.WriteFile(filepath.Join(root, ContextsFilename), legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	current := Registry{Version: ContextsSchemaVersion, Contexts: []Context{}}
	if err := RegistryFile(root).LoadInto(&current); err != nil {
		t.Fatalf("migrate registry on load: %v", err)
	}
	if !reflect.DeepEqual(current, validRegistry()) {
		t.Fatalf("unexpected migrated registry: got=%+v want=%+v", current, validRegistry())
	}
	backup, err := os.ReadFile(filepath.Join(root, ContextsV1BackupFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backup, legacy) {
		t.Fatalf("v1 rollback copy changed bytes:\n got=%q\nwant=%q", backup, legacy)
	}
	backupInfo, err := os.Stat(filepath.Join(root, ContextsV1BackupFilename))
	if err != nil || backupInfo.Mode().Perm() != 0o600 {
		t.Fatalf("rollback copy is not owner-only: info=%v err=%v", backupInfo, err)
	}

	var loadedAgain Registry
	if err := RegistryFile(root).LoadInto(&loadedAgain); err != nil || !reflect.DeepEqual(loadedAgain, current) {
		t.Fatalf("load migrated registry again: got=%+v err=%v", loadedAgain, err)
	}
	after, err := os.ReadFile(filepath.Join(root, ContextsV1BackupFilename))
	if err != nil || !bytes.Equal(after, legacy) {
		t.Fatalf("idempotent load changed rollback copy: got=%q err=%v", after, err)
	}
}

func TestUpdateRegistryMigratesBeforeMutation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"version":1,"contexts":[]}`)
	if err := os.WriteFile(filepath.Join(root, ContextsFilename), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := UpdateRegistry(root, func(registry *Registry) error {
		registry.Contexts = append(registry.Contexts, validRegistry().Contexts[0])
		return nil
	})
	if err != nil {
		t.Fatalf("migrate and update registry: %v", err)
	}
	if !reflect.DeepEqual(updated, validRegistry()) {
		t.Fatalf("unexpected updated registry: %+v", updated)
	}
	backup, err := os.ReadFile(filepath.Join(root, ContextsV1BackupFilename))
	if err != nil || !bytes.Equal(backup, legacy) {
		t.Fatalf("update did not preserve v1 source: got=%q err=%v", backup, err)
	}
}

func TestRegistryMigrationRejectsUnknownOrMalformedV1WithoutBackup(t *testing.T) {
	for _, contents := range []string{
		`{"version":1,"contexts":[],"command":"sh"}`,
		`{"version":1,"contexts":null}`,
		`{"version":0,"contexts":[]}`,
		`{"version":1,"contexts":`,
	} {
		t.Run(contents, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "sway-session")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, ContextsFilename)
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var registry Registry
			if err := RegistryFile(root).LoadInto(&registry); err == nil {
				t.Fatal("expected invalid migration source rejection")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, before) {
				t.Fatalf("invalid migration changed source: got=%q err=%v", after, err)
			}
			if _, err := os.Lstat(filepath.Join(root, ContextsV1BackupFilename)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid migration created rollback copy: %v", err)
			}
		})
	}
}

func TestRegistryMigrationRejectsConflictingRollbackCopy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"version":1,"contexts":[]}`)
	if err := os.WriteFile(filepath.Join(root, ContextsFilename), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ContextsV1BackupFilename), []byte("different"), 0o600); err != nil {
		t.Fatal(err)
	}
	var registry Registry
	if err := RegistryFile(root).LoadInto(&registry); err == nil {
		t.Fatal("expected conflicting rollback copy rejection")
	}
	after, err := os.ReadFile(filepath.Join(root, ContextsFilename))
	if err != nil || !bytes.Equal(after, legacy) {
		t.Fatalf("conflicting rollback copy changed legacy source: got=%q err=%v", after, err)
	}
}

func TestRegistryWritesRefuseUnknownExistingSchemaWithoutMutation(t *testing.T) {
	unknown := []byte(`{"version":3,"preferences":{"desktop_indicators":false},"contexts":[]}`)
	for name, operation := range map[string]func(string, *bool) error{
		"save": func(root string, _ *bool) error {
			return RegistryFile(root).Save(validRegistry())
		},
		"update": func(root string, called *bool) error {
			_, err := UpdateRegistry(root, func(*Registry) error {
				*called = true
				return nil
			})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "sway-session")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, ContextsFilename)
			if err := os.WriteFile(path, unknown, 0o600); err != nil {
				t.Fatal(err)
			}
			called := false
			if err := operation(root, &called); err == nil {
				t.Fatal("unknown registry schema was overwritten")
			}
			if called {
				t.Fatal("registry mutation ran against an unknown schema")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, unknown) {
				t.Fatalf("unknown registry changed: got=%q err=%v", after, err)
			}
			if _, err := os.Lstat(filepath.Join(root, ContextsV1BackupFilename)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unknown registry created rollback evidence: %v", err)
			}
		})
	}
}
