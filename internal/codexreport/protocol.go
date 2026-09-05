// Package codexreport implements the narrow Codex-to-Herdr session reporting
// boundary. It intentionally has no general pane-control operation.
package codexreport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	sessionstate "github.com/marang/sway-session/internal/session"
)

const (
	ProtocolVersion        = 1
	SocketFilename         = "codex-report.sock"
	ContextIDEnvironment   = "SWAY_SESSION_CONTEXT_ID"
	HerdrPaneEnvironment   = "HERDR_PANE_ID"
	HerdrActiveEnvironment = "HERDR_ENV"
	CodexThreadEnvironment = "CODEX_THREAD_ID"
	maxHookPayload         = 16 * 1024
	maxProtocolMessage     = 8 * 1024
)

var ErrNotManagedSession = errors.New("codex session did not start in a managed Herdr context")

type Report struct {
	Version        int                    `json:"version"`
	ContextID      sessionstate.ContextID `json:"context_id"`
	PaneID         string                 `json:"pane_id"`
	CodexSessionID string                 `json:"codex_session_id"`
	PeerPID        int                    `json:"-"`
}

type response struct {
	Version int    `json:"version"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

func (report Report) Validate() error {
	if report.Version != ProtocolVersion {
		return fmt.Errorf("unsupported Codex report protocol version %d", report.Version)
	}
	if err := report.ContextID.Validate(); err != nil {
		return fmt.Errorf("invalid context ID: %w", err)
	}
	if err := validateIdentity("Herdr pane ID", report.PaneID, 256); err != nil {
		return err
	}
	if _, err := sessionstate.ParseContextID(report.CodexSessionID); err != nil {
		return fmt.Errorf("invalid Codex session ID: %w", err)
	}
	return nil
}

func DefaultSocketPath() (string, error) {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" || !filepath.IsAbs(runtimeDir) {
		return "", errors.New("XDG_RUNTIME_DIR must be an absolute path")
	}
	runtimeDir = filepath.Clean(runtimeDir)
	return filepath.Join(runtimeDir, "sway-session", SocketFilename), nil
}

// ParseCodexHook converts Codex's extensible SessionStart JSON into the fixed
// report contract. Transcript paths, source command lines, and unknown fields
// are deliberately ignored.
func ParseCodexHook(input io.Reader, getenv func(string) string) (Report, error) {
	if input == nil || getenv == nil {
		return Report{}, errors.New("codex hook input and environment are required")
	}
	data, err := io.ReadAll(io.LimitReader(input, maxHookPayload+1))
	if err != nil {
		return Report{}, fmt.Errorf("read Codex hook payload: %w", err)
	}
	if len(data) > maxHookPayload {
		return Report{}, fmt.Errorf("codex hook payload exceeds %d bytes", maxHookPayload)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var payload struct {
		HookEventName string `json:"hook_event_name"`
		SessionID     string `json:"session_id"`
	}
	if err := decoder.Decode(&payload); err != nil {
		return Report{}, fmt.Errorf("decode Codex hook payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Report{}, errors.New("codex hook payload contains multiple JSON values")
		}
		return Report{}, fmt.Errorf("decode trailing Codex hook data: %w", err)
	}
	if payload.HookEventName != "SessionStart" || getenv(HerdrActiveEnvironment) != "1" {
		return Report{}, ErrNotManagedSession
	}
	contextID, err := sessionstate.ParseContextID(getenv(ContextIDEnvironment))
	if err != nil {
		return Report{}, fmt.Errorf("invalid %s: %w", ContextIDEnvironment, err)
	}
	report := Report{
		Version:        ProtocolVersion,
		ContextID:      contextID,
		PaneID:         getenv(HerdrPaneEnvironment),
		CodexSessionID: payload.SessionID,
	}
	if inherited := getenv(CodexThreadEnvironment); inherited != "" && inherited != report.CodexSessionID {
		return Report{}, errors.New("codex hook session ID does not match CODEX_THREAD_ID")
	}
	if err := report.Validate(); err != nil {
		return Report{}, err
	}
	return report, nil
}

func validateIdentity(name string, value string, maximum int) error {
	if value == "" || len(value) > maximum || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must contain between 1 and %d bytes without surrounding whitespace", name, maximum)
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("%s must not contain control characters", name)
		}
	}
	return nil
}
