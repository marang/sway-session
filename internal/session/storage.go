package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/marang/sway-title-animator/internal/statefile"
)

const (
	ContextsFilename            = "contexts.json"
	ContextsV1BackupFilename    = "contexts.v1.json"
	LayoutFilename              = "layout.json"
	ApplicationSessionFilename  = "application-session.json"
	ApplicationSessionDirectory = "application-runtime"
)

// DefaultStateRoot resolves the private sway-session XDG state directory.
func DefaultStateRoot() (string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(stateHome) {
		return "", errors.New("XDG_STATE_HOME must be an absolute path")
	}
	return filepath.Join(filepath.Clean(stateHome), "sway-session"), nil
}

// RegistryStore wraps the current JSON document with the one supported
// contexts.json migration. The v1 backup is rollback evidence only and is
// never used as an automatic fallback.
type RegistryStore struct {
	current statefile.JSONFile[Registry]
}

func RegistryFile(root string) RegistryStore {
	return RegistryStore{current: statefile.NewJSONFile(root, ContextsFilename, (*Registry).Validate)}
}

func (store RegistryStore) Save(value Registry) error {
	if err := value.Validate(); err != nil {
		return fmt.Errorf("validate %s: %w", ContextsFilename, err)
	}
	if err := store.ensureCurrent(context.Background()); err != nil {
		return err
	}
	return store.current.Save(value)
}

func (store RegistryStore) LoadInto(target *Registry) error {
	return store.LoadIntoContext(context.Background(), target)
}

func (store RegistryStore) LoadIntoContext(ctx context.Context, target *Registry) error {
	if ctx == nil {
		return errors.New("registry load context is nil")
	}
	if target == nil {
		return errors.New("registry target is nil")
	}
	if err := store.current.LoadIntoContext(ctx, target); !isLegacyRegistryVersion(err) {
		return err
	}
	candidate, _, err := store.current.MigrateContext(ctx, decodeRegistryMigration, ContextsV1BackupFilename)
	if err != nil {
		return err
	}
	*target = candidate
	return nil
}

func (store RegistryStore) LoadSnapshotInto(target *Registry) error {
	if target == nil {
		return errors.New("registry target is nil")
	}
	if err := store.current.LoadSnapshotInto(target); !isLegacyRegistryVersion(err) {
		return err
	}
	candidate, _, err := store.current.Migrate(decodeRegistryMigration, ContextsV1BackupFilename)
	if err != nil {
		return err
	}
	*target = candidate
	return nil
}

// ReadRegistrySnapshot returns one validated current-schema snapshot without
// waiting for the registry lock, creating state, or performing migration.
// It is intended for stale-safe read-only presentation such as completion.
func ReadRegistrySnapshot(root string) (Registry, error) {
	registry := emptyRegistry()
	err := RegistryFile(root).current.LoadSnapshotInto(&registry)
	if errors.Is(err, os.ErrNotExist) {
		return registry, nil
	}
	return registry, err
}

func (store RegistryStore) ensureCurrent(ctx context.Context) error {
	var current Registry
	err := store.current.LoadSnapshotInto(&current)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if !isLegacyRegistryVersion(err) {
		return err
	}
	_, _, err = store.current.MigrateContext(ctx, decodeRegistryMigration, ContextsV1BackupFilename)
	return err
}

// UpdateRegistry serializes the complete registry load-modify-save operation
// across concurrent sway-session processes.
func UpdateRegistry(root string, mutate func(*Registry) error) (Registry, error) {
	return UpdateRegistryContext(context.Background(), root, mutate)
}

// UpdateRegistryContext is UpdateRegistry with cancelable lock acquisition.
func UpdateRegistryContext(ctx context.Context, root string, mutate func(*Registry) error) (Registry, error) {
	store := RegistryFile(root)
	initial := emptyRegistry()
	if err := store.ensureCurrent(ctx); err != nil {
		return initial, err
	}
	return store.current.UpdateContext(ctx, initial, mutate)
}

// InspectRegistryLocked serializes an observation and its external effects
// with all registry mutations without rewriting contexts.json.
func InspectRegistryLocked(root string, inspect func(Registry) error) error {
	return InspectRegistryLockedContext(context.Background(), root, inspect)
}

// InspectRegistryLockedContext is InspectRegistryLocked with cancelable lock acquisition.
func InspectRegistryLockedContext(ctx context.Context, root string, inspect func(Registry) error) error {
	store := RegistryFile(root)
	if err := store.ensureCurrent(ctx); err != nil {
		return err
	}
	return store.current.InspectLockedContext(ctx, emptyRegistry(), inspect)
}

func LayoutFile(root string) statefile.JSONFile[LayoutSnapshot] {
	return statefile.NewJSONFile(root, LayoutFilename, (*LayoutSnapshot).Validate)
}

func ApplicationSessionFile(root string) statefile.JSONFile[ApplicationSessionState] {
	return statefile.NewJSONFile(filepath.Join(root, ApplicationSessionDirectory), ApplicationSessionFilename, (*ApplicationSessionState).Validate)
}

func emptyRegistry() Registry {
	return Registry{Version: ContextsSchemaVersion, Preferences: RegistryPreferences{}, Contexts: []Context{}}
}

type registryV1 struct {
	Version  int         `json:"version"`
	Contexts []contextV1 `json:"contexts"`
}

type contextV1 struct {
	ID       ContextID    `json:"id"`
	Label    string       `json:"label,omitempty"`
	Provider string       `json:"provider,omitempty"`
	State    ContextState `json:"state"`
	Launcher launcherV1   `json:"launcher"`
}

type launcherV1 struct {
	Kind    LauncherKind `json:"kind"`
	Session string       `json:"session"`
	Cwd     string       `json:"cwd"`
}

func decodeRegistryMigration(data []byte) (Registry, bool, error) {
	var envelope struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return Registry{}, false, err
	}
	switch envelope.Version {
	case ContextsSchemaVersion:
		var current Registry
		if err := decodeRegistryStrict(data, &current); err != nil {
			return Registry{}, false, err
		}
		return current, false, nil
	case 1:
		var legacy registryV1
		if err := decodeRegistryStrict(data, &legacy); err != nil {
			return Registry{}, false, err
		}
		if legacy.Contexts == nil {
			return Registry{}, false, errors.New("legacy context registry must contain a contexts array")
		}
		contexts := make([]Context, len(legacy.Contexts))
		for index, old := range legacy.Contexts {
			contexts[index] = Context{
				ID:       old.ID,
				Label:    old.Label,
				Provider: old.Provider,
				State:    old.State,
				Launcher: Launcher{Kind: old.Launcher.Kind, Session: old.Launcher.Session, Cwd: old.Launcher.Cwd},
			}
		}
		return Registry{
			Version:     ContextsSchemaVersion,
			Preferences: RegistryPreferences{},
			Contexts:    contexts,
		}, true, nil
	default:
		return Registry{}, false, &UnsupportedVersionError{
			Document: "context registry",
			Got:      envelope.Version,
			Want:     ContextsSchemaVersion,
		}
	}
}

func decodeRegistryStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func isLegacyRegistryVersion(err error) bool {
	var versionError *UnsupportedVersionError
	return errors.As(err, &versionError) && versionError.Got == 1 && versionError.Want == ContextsSchemaVersion
}
