package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/marang/sway-title-animator/internal/statefile"
)

const (
	ContextsFilename = "contexts.json"
	LayoutFilename   = "layout.json"
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

func RegistryFile(root string) statefile.JSONFile[Registry] {
	return statefile.NewJSONFile(root, ContextsFilename, (*Registry).Validate)
}

// UpdateRegistry serializes the complete registry load-modify-save operation
// across concurrent sway-session processes.
func UpdateRegistry(root string, mutate func(*Registry) error) (Registry, error) {
	initial := Registry{Version: ContextsSchemaVersion, Contexts: []Context{}}
	return RegistryFile(root).Update(initial, mutate)
}

// InspectRegistryLocked serializes an observation and its external effects
// with all registry mutations without rewriting contexts.json.
func InspectRegistryLocked(root string, inspect func(Registry) error) error {
	initial := Registry{Version: ContextsSchemaVersion, Contexts: []Context{}}
	return RegistryFile(root).InspectLocked(initial, inspect)
}

func LayoutFile(root string) statefile.JSONFile[LayoutSnapshot] {
	return statefile.NewJSONFile(root, LayoutFilename, (*LayoutSnapshot).Validate)
}
