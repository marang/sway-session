package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWorkEnvironmentBackupRecoveryRetainsSharedAndUnresolvedDefinitions(t *testing.T) {
	root := backupTestRoot(t)
	store := WorkEnvironmentStoreFor(root)
	first := environmentTestAdd(t, store, environmentTestCreate(t, store, "Development"), testContextID)
	var err error
	first, err = store.SetPrimaryContext(t.Context(), first.ID, first.Revision, testContextID)
	if err != nil {
		t.Fatal(err)
	}
	second := environmentTestAdd(t, store, environmentTestCreate(t, store, "Review"), testContextID)
	empty := environmentTestCreate(t, store, "Empty")
	_, err = UpdateRegistry(root, func(registry *Registry) error { _, err := RemoveContext(registry, string(testContextID)); return err })
	if err != nil {
		t.Fatal(err)
	}
	want, err := store.ListContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 3 {
		t.Fatalf("definitions = %+v", want)
	}
	output := backupTestOutput(t)
	if _, err := BackupState(t.Context(), root, output); err != nil {
		t.Fatal(err)
	}
	before := backupTestRead(t, output)
	if _, err := ValidateStateBackup(t.Context(), output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, backupTestRead(t, output)) {
		t.Fatal("validation changed backup")
	}
	target := filepath.Join(t.TempDir(), "recovered")
	if _, err := RecoverState(t.Context(), target, output, true); err != nil {
		t.Fatal(err)
	}
	got, err := WorkEnvironmentStoreFor(target).ListContext(t.Context())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("recovery = %+v %v; want %+v", got, err, want)
	}
	for _, value := range []WorkEnvironment{first, second, empty} {
		loaded, err := WorkEnvironmentStoreFor(target).LoadContext(t.Context(), value.ID)
		if err != nil || !reflect.DeepEqual(loaded, value) {
			t.Fatalf("definition after recovery = %+v %v; want %+v", loaded, err, value)
		}
	}
}

func TestWorkEnvironmentMalformedStorageAndBackupFailClosed(t *testing.T) {
	cases := map[string][]string{
		"partial schema":          {"DROP TABLE work_environment_members"},
		"extra executable object": {`CREATE TRIGGER environment_destroy AFTER DELETE ON work_environments BEGIN DELETE FROM contexts; END`},
		"additional column":       {"ALTER TABLE work_environments ADD COLUMN unexpected TEXT"},
		"empty name":              {"PRAGMA ignore_check_constraints = ON", "UPDATE work_environments SET name = ''"},
		"invalid identity":        {"UPDATE work_environments SET id = 'zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz'"},
		"invalid primary":         {"UPDATE work_environments SET primary_context_id = '22222222-2222-4222-8222-222222222222'"},
		"invalid member":          {"INSERT INTO work_environment_members(environment_id, context_id, ordinal) SELECT id, 'zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz', 0 FROM work_environments"},
	}
	for name, statements := range cases {
		t.Run(name, func(t *testing.T) {
			root := backupTestRoot(t)
			store := WorkEnvironmentStoreFor(root)
			value := environmentTestCreate(t, store, "Original")
			backupTestMutate(t, root, statements...)
			path := filepath.Join(root, StateDatabaseFilename)
			before := backupTestRead(t, path)
			if _, err := store.ListContext(t.Context()); err == nil {
				t.Fatal("read accepted malformed extension")
			}
			if _, err := store.RenameContext(t.Context(), value.ID, value.Revision, "Repair"); err == nil {
				t.Fatal("write repaired malformed extension")
			}
			if !bytes.Equal(before, backupTestRead(t, path)) {
				t.Fatal("read/write repaired invalid state")
			}
			output := backupTestOutput(t)
			if _, err := BackupState(t.Context(), root, output); err == nil {
				t.Fatal("backup accepted malformed extension")
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid backup published: %v", err)
			}
		})
	}
}

func TestWorkEnvironmentBackupRejectsWrongOwnershipForeignKey(t *testing.T) {
	root := backupTestRoot(t)
	store := WorkEnvironmentStoreFor(root)
	environmentTestCreate(t, store, "Original")
	backupTestMutate(t, root, "DROP TABLE work_environment_members", `CREATE TABLE work_environment_members (
		environment_id TEXT NOT NULL REFERENCES work_environments(id) ON DELETE CASCADE,
		context_id TEXT NOT NULL REFERENCES contexts(id) ON DELETE CASCADE,
		ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
		PRIMARY KEY (environment_id, context_id), UNIQUE (environment_id, ordinal)
	) STRICT`)
	if _, err := BackupState(t.Context(), root, backupTestOutput(t)); err == nil {
		t.Fatal("accepted destructive context ownership")
	}
}

func TestWorkEnvironmentDoesNotIntroduceSmallCollectionLimits(t *testing.T) {
	root := backupTestRoot(t)
	registry := validRegistry()
	for index := range 70 {
		value := registry.Contexts[0]
		id, err := NewContextID()
		if err != nil {
			t.Fatal(err)
		}
		value.ID, value.Launcher.Session = id, fmt.Sprintf("worktree-%d", index)
		registry.Contexts = append(registry.Contexts, value)
	}
	if err := RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	store := WorkEnvironmentStoreFor(root)
	value := environmentTestCreate(t, store, "Many members")
	for _, member := range registry.Contexts {
		value = environmentTestAdd(t, store, value, member.ID)
	}
	loaded, err := store.LoadContext(t.Context(), value.ID)
	if err != nil || !reflect.DeepEqual(loaded, value) {
		t.Fatalf("large group = %+v %v", loaded, err)
	}
	for index := range 70 {
		environmentTestCreate(t, store, fmt.Sprintf("group-%d", index))
	}
	values, err := store.ListContext(t.Context())
	if err != nil || len(values) != 71 {
		t.Fatalf("large collection length=%d err=%v", len(values), err)
	}
	if _, err := BackupState(t.Context(), root, backupTestOutput(t)); err != nil {
		t.Fatal(err)
	}
}
