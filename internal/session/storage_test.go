package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
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

	unsupported := []byte(`{"version":6,"preferences":{"desktop_indicators":false},"contexts":[]}`)
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

func TestRegistryStoreMigratesV2ConcurrentlyWithExactRollbackCopy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte("{\n" +
		"  \"version\": 2,\n" +
		"  \"preferences\": {\"desktop_indicators\": true},\n" +
		"  \"contexts\": [{\n" +
		"    \"id\": \"123e4567-e89b-12d3-a456-426614174000\",\n" +
		"    \"state\": \"archived\",\n" +
		"    \"launcher\": {\"kind\": \"herdr\", \"session\": \"lab-105\", \"cwd\": \"/home/example/work\"}\n" +
		"  }]\n" +
		"}\n")
	if err := os.WriteFile(filepath.Join(root, ContextsFilename), legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	const readers = 8
	results := make(chan Registry, readers)
	errorsFound := make(chan error, readers)
	var start sync.WaitGroup
	start.Add(1)
	var workers sync.WaitGroup
	workers.Add(readers)
	for range readers {
		go func() {
			defer workers.Done()
			start.Wait()
			var registry Registry
			if err := RegistryFile(root).LoadInto(&registry); err != nil {
				errorsFound <- err
				return
			}
			results <- registry
		}()
	}
	start.Done()
	workers.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Fatalf("concurrent migration failed: %v", err)
	}
	for registry := range results {
		if registry.Version != ContextsSchemaVersion || len(registry.Contexts) != 1 {
			t.Fatalf("unexpected migrated registry: %+v", registry)
		}
		context := registry.Contexts[0]
		if context.ArchivedAt != nil {
			t.Fatalf("migration invented archive time: %+v", context.ArchivedAt)
		}
		if context.Launcher.Terminal == nil || context.Launcher.Terminal.Adapter != TerminalAdapterAlacritty || context.Launcher.Terminal.Identity != nil {
			t.Fatalf("migration did not pin legacy terminal adapter: %+v", context.Launcher.Terminal)
		}
	}

	backup, err := os.ReadFile(filepath.Join(root, ContextsV2BackupFilename))
	if err != nil || !bytes.Equal(backup, legacy) {
		t.Fatalf("v2 rollback copy differs: got=%q err=%v", backup, err)
	}
	if _, err := os.Lstat(filepath.Join(root, ContextsV1BackupFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("v2 migration created v1 rollback copy: %v", err)
	}
}

func TestRegistryStoreMigratesV3LookalikeAsManualWithExactRollbackCopy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"version":3,"preferences":{"desktop_indicators":false},"contexts":[{` +
		`"id":"123e4567-e89b-12d3-a456-426614174000","label":"Terminal","provider":"sway-session-terminal","state":"active",` +
		`"launcher":{"kind":"herdr","session":"sway-terminal-instance-123e4567-e89b-12d3-a456-426614174000",` +
		`"cwd":"/home/example/work","terminal":{"adapter":"alacritty"}}}]}`)
	if err := os.WriteFile(filepath.Join(root, ContextsFilename), legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	var current Registry
	if err := RegistryFile(root).LoadInto(&current); err != nil {
		t.Fatalf("migrate v3 registry: %v", err)
	}
	if current.Version != ContextsSchemaVersion || len(current.Contexts) != 1 ||
		current.Contexts[0].Launcher.Terminal == nil || current.Contexts[0].Launcher.Terminal.Instance ||
		IsTerminalInstanceContext(current.Contexts[0]) {
		t.Fatalf("v3 manual lookalike changed meaning during migration: %+v", current)
	}
	backup, err := os.ReadFile(filepath.Join(root, ContextsV3BackupFilename))
	if err != nil || !bytes.Equal(backup, legacy) {
		t.Fatalf("v3 rollback copy differs: got=%q err=%v", backup, err)
	}
}

func TestRegistryStoreMigratesV4WithoutRenamingInstanceAndPreservesExactRollbackCopy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"version":4,"preferences":{"desktop_indicators":false},"contexts":[{` +
		`"id":"123e4567-e89b-12d3-a456-426614174000","label":"Terminal","provider":"sway-session-terminal","state":"active",` +
		`"launcher":{"kind":"herdr","session":"sway-terminal-instance-123e4567-e89b-12d3-a456-426614174000",` +
		`"cwd":"/home/example/work","terminal":{"adapter":"alacritty","instance":true}}}]}`)
	if err := os.WriteFile(filepath.Join(root, ContextsFilename), legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	var current Registry
	if err := RegistryFile(root).LoadInto(&current); err != nil {
		t.Fatalf("migrate v4 registry: %v", err)
	}
	if current.Version != ContextsSchemaVersion || len(current.Contexts) != 1 ||
		current.Contexts[0].Launcher.Session != "sway-terminal-instance-123e4567-e89b-12d3-a456-426614174000" ||
		!IsTerminalInstanceContext(current.Contexts[0]) {
		t.Fatalf("v4 instance changed identity during migration: %+v", current)
	}
	backup, err := os.ReadFile(filepath.Join(root, ContextsV4BackupFilename))
	if err != nil || !bytes.Equal(backup, legacy) {
		t.Fatalf("v4 rollback copy differs: got=%q err=%v", backup, err)
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

func TestRegistryStrictlyRejectsInvalidCurrentAndLegacyTerminalState(t *testing.T) {
	tests := []struct {
		name       string
		contents   string
		backupName string
	}{
		{
			name: "current terminal",
			contents: `{"version":5,"preferences":{"desktop_indicators":false},"contexts":[{` +
				`"id":"123e4567-e89b-12d3-a456-426614174000","state":"active",` +
				`"launcher":{"kind":"herdr","session":"work","cwd":"/work",` +
				`"terminal":{"adapter":"alacritty","command":"sh"}}}]}`,
		},
		{
			name: "v4 new instance spelling",
			contents: `{"version":4,"preferences":{"desktop_indicators":false},"contexts":[{` +
				`"id":"123e4567-e89b-12d3-a456-426614174000","label":"Terminal","provider":"sway-session-terminal","state":"active",` +
				`"launcher":{"kind":"herdr","session":"sway-terminal-123e4567e89b12d3a456426614174000","cwd":"/work",` +
				`"terminal":{"adapter":"alacritty","instance":true}}}]}`,
			backupName: ContextsV4BackupFilename,
		},
		{
			name: "v3 instance discriminator",
			contents: `{"version":3,"preferences":{"desktop_indicators":false},"contexts":[{` +
				`"id":"123e4567-e89b-12d3-a456-426614174000","state":"active",` +
				`"launcher":{"kind":"herdr","session":"work","cwd":"/work",` +
				`"terminal":{"adapter":"alacritty","instance":true}}}]}`,
			backupName: ContextsV3BackupFilename,
		},
		{
			name: "v2 launcher",
			contents: `{"version":2,"preferences":{"desktop_indicators":false},"contexts":[{` +
				`"id":"123e4567-e89b-12d3-a456-426614174000","state":"active",` +
				`"launcher":{"kind":"herdr","session":"work","cwd":"/work","terminal":"alacritty"}}]}`,
			backupName: ContextsV2BackupFilename,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "sway-session")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, ContextsFilename)
			before := []byte(test.contents)
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			var registry Registry
			if err := RegistryFile(root).LoadInto(&registry); err == nil {
				t.Fatal("unknown field passed strict registry load")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, before) {
				t.Fatalf("rejected state changed: got=%q err=%v", after, err)
			}
			if test.backupName != "" {
				if _, err := os.Lstat(filepath.Join(root, test.backupName)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("rejected state created backup: %v", err)
				}
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
	unknown := []byte(`{"version":6,"preferences":{"desktop_indicators":false},"contexts":[]}`)
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
