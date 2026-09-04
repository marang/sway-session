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
	legacyContextsFilename            = "contexts.json"
	legacyLayoutFilename              = "layout.json"
	legacyApplicationSessionFilename  = "application-session.json"
	legacyApplicationSessionDirectory = "application-runtime"
	legacyTerminalActivityFilename    = "terminal-activity.json"
	legacyTerminalActivityDirectory   = "terminal-runtime"
	terminalLifecycleDirectory        = "terminal-lifecycle"
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

func emptyRegistry() Registry {
	return Registry{Version: ContextsSchemaVersion, Preferences: RegistryPreferences{}, Contexts: []Context{}}
}
