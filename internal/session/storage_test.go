package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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

func TestRegistryFileRoundTrip(t *testing.T) {
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

func TestRegistryRejectsEveryUnsupportedSchemaWithoutMutation(t *testing.T) {
	for _, version := range []int{1, 2, 3, 4, 6} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "sway-session")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			contents := []byte(`{"version":` + strconv.Itoa(version) + `,"preferences":{"desktop_indicators":false},"contexts":[]}`)
			path := filepath.Join(root, ContextsFilename)
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			originalTarget := validRegistry()
			target := originalTarget
			err := RegistryFile(root).LoadInto(&target)
			var versionError *UnsupportedVersionError
			if !errors.As(err, &versionError) || versionError.Got != version {
				t.Fatalf("schema %d returned %v", version, err)
			}
			if !reflect.DeepEqual(target, originalTarget) {
				t.Fatalf("schema %d changed the load target: %+v", version, target)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(after, contents) {
				t.Fatalf("schema %d changed on disk: data=%q err=%v", version, after, readErr)
			}
		})
	}
}

func TestRegistryWritesRefuseUnsupportedExistingSchemaWithoutMutation(t *testing.T) {
	for _, version := range []int{1, 4, 6} {
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
			t.Run(strconv.Itoa(version)+"_"+name, func(t *testing.T) {
				root := filepath.Join(t.TempDir(), "sway-session")
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				contents := []byte(`{"version":` + strconv.Itoa(version) + `,"preferences":{"desktop_indicators":false},"contexts":[]}`)
				path := filepath.Join(root, ContextsFilename)
				if err := os.WriteFile(path, contents, 0o600); err != nil {
					t.Fatal(err)
				}
				called := false
				if err := operation(root, &called); err == nil {
					t.Fatalf("schema %d was overwritten", version)
				}
				if called {
					t.Fatalf("mutation ran against schema %d", version)
				}
				after, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(after, contents) {
					t.Fatalf("schema %d changed on disk: data=%q err=%v", version, after, readErr)
				}
			})
		}
	}
}

func TestRegistryStrictlyRejectsUnknownCurrentFieldsWithoutMutation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := []byte(`{"version":5,"preferences":{"desktop_indicators":false},"contexts":[],"command":"sh"}`)
	path := filepath.Join(root, ContextsFilename)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	var registry Registry
	if err := RegistryFile(root).LoadInto(&registry); err == nil {
		t.Fatal("unknown field passed strict registry load")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, contents) {
		t.Fatalf("rejected state changed: got=%q err=%v", after, err)
	}
}
