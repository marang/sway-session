package session

import (
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

	unsupported := []byte(`{"version":2,"contexts":[]}`)
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
