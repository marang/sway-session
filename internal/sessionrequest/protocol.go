// Package sessionrequest implements the narrow client boundary for
// idempotently ensuring and starting one typed Sway/Herdr work context.
package sessionrequest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	sessionstate "github.com/marang/sway-session/internal/session"
)

const (
	ProtocolVersion    = 1
	SocketFilename     = "session-start.sock"
	MaximumWorkspace   = 99
	maxProtocolMessage = 8 * 1024
)

var validationContextID = sessionstate.ContextID("00000000-0000-4000-8000-000000000000")

type Request struct {
	Version   int    `json:"version"`
	Session   string `json:"session"`
	Cwd       string `json:"cwd"`
	Label     string `json:"label,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Workspace int    `json:"workspace"`
}

type Response struct {
	Version   int                   `json:"version"`
	OK        bool                  `json:"ok"`
	Context   *sessionstate.Context `json:"context,omitempty"`
	Workspace int                   `json:"workspace,omitempty"`
	Created   bool                  `json:"created,omitempty"`
	Error     string                `json:"error,omitempty"`
}

func (request Request) Validate() error {
	if request.Version != ProtocolVersion {
		return fmt.Errorf("unsupported session request protocol version %d", request.Version)
	}
	if request.Workspace < 1 || request.Workspace > MaximumWorkspace {
		return fmt.Errorf("workspace must be between 1 and %d", MaximumWorkspace)
	}
	candidate := sessionstate.Context{
		ID:       validationContextID,
		Label:    request.Label,
		Provider: request.Provider,
		State:    sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{
			Kind:     sessionstate.LauncherHerdr,
			Session:  request.Session,
			Cwd:      request.Cwd,
			Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty},
		},
	}
	if err := candidate.Validate(); err != nil {
		return fmt.Errorf("invalid requested context: %w", err)
	}
	return nil
}

func DefaultSocketPath() (string, error) {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" || !filepath.IsAbs(runtimeDir) {
		return "", errors.New("XDG_RUNTIME_DIR must be an absolute path")
	}
	return filepath.Join(filepath.Clean(runtimeDir), "sway-session", SocketFilename), nil
}
