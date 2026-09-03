package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/marang/sway-title-animator/internal/statefile"
)

const (
	ContextsFilename            = "contexts.json"
	LayoutFilename              = "layout.json"
	ApplicationSessionFilename  = "application-session.json"
	ApplicationSessionDirectory = "application-runtime"
	terminalLifecycleDirectory  = "terminal-lifecycle"
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

// RegistryStore accepts only the current contexts.json schema. Older and
// unknown versions fail closed and are never rewritten implicitly.
type RegistryStore struct {
	current statefile.JSONFile[Registry]
}

func RegistryFile(root string) RegistryStore {
	return RegistryStore{current: statefile.NewJSONFile(root, ContextsFilename, (*Registry).Validate)}
}

func (store RegistryStore) Save(value Registry) error {
	return store.SaveContext(context.Background(), value)
}

// SaveContext is Save with cancelable registry-lock acquisition.
func (store RegistryStore) SaveContext(ctx context.Context, value Registry) error {
	if ctx == nil {
		return errors.New("registry save context is nil")
	}
	if err := value.Validate(); err != nil {
		return fmt.Errorf("validate %s: %w", ContextsFilename, err)
	}
	_, err := store.current.UpdateContext(ctx, emptyRegistry(), func(current *Registry) error {
		*current = value
		return nil
	})
	return err
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
	return store.current.LoadIntoContext(ctx, target)
}

func (store RegistryStore) LoadSnapshotInto(target *Registry) error {
	if target == nil {
		return errors.New("registry target is nil")
	}
	return store.current.LoadSnapshotInto(target)
}

// ReadRegistrySnapshot returns one validated current-schema snapshot without
// waiting for the registry lock or creating state.
// It is intended for stale-safe read-only presentation such as completion.
func ReadRegistrySnapshot(root string) (Registry, error) {
	return ReadRegistrySnapshotContext(context.Background(), root)
}

// ReadRegistrySnapshotContext is ReadRegistrySnapshot with cancellation
// observed before the bounded snapshot read begins.
func ReadRegistrySnapshotContext(ctx context.Context, root string) (Registry, error) {
	registry := emptyRegistry()
	if ctx == nil {
		return registry, errors.New("registry snapshot context is nil")
	}
	if err := ctx.Err(); err != nil {
		return registry, err
	}
	err := RegistryFile(root).current.LoadSnapshotInto(&registry)
	if errors.Is(err, os.ErrNotExist) {
		return registry, nil
	}
	return registry, err
}

// UpdateRegistry serializes the complete registry load-modify-save operation
// across concurrent sway-session processes.
func UpdateRegistry(root string, mutate func(*Registry) error) (Registry, error) {
	return UpdateRegistryContext(context.Background(), root, mutate)
}

// UpdateRegistryContext is UpdateRegistry with cancelable lock acquisition.
func UpdateRegistryContext(ctx context.Context, root string, mutate func(*Registry) error) (Registry, error) {
	return RegistryFile(root).current.UpdateContext(ctx, emptyRegistry(), mutate)
}

// InspectRegistryLocked serializes an observation and its external effects
// with all registry mutations without rewriting contexts.json.
func InspectRegistryLocked(root string, inspect func(Registry) error) error {
	return InspectRegistryLockedContext(context.Background(), root, inspect)
}

// InspectRegistryLockedContext is InspectRegistryLocked with cancelable lock acquisition.
func InspectRegistryLockedContext(ctx context.Context, root string, inspect func(Registry) error) error {
	return RegistryFile(root).current.InspectLockedContext(ctx, emptyRegistry(), inspect)
}

// WithTerminalLifecycleLockContext serializes manager-backed terminal
// creation, restore, and initialization across sway-session processes. The
// lifecycle lock is always acquired before the registry lock.
func WithTerminalLifecycleLockContext(ctx context.Context, root string, action func() error) error {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return errors.New("terminal lifecycle state root must be a clean absolute path")
	}
	if action == nil {
		return errors.New("terminal lifecycle action is nil")
	}
	return statefile.WithPrivateDirectoryLockContext(ctx, filepath.Join(root, terminalLifecycleDirectory), func(*statefile.LockedPrivateDirectory) error {
		return action()
	})
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
