// Package codexreport preserves the version-1 Codex SessionStart compatibility
// boundary. New provider-neutral reporting lives in internal/agentreport.
package codexreport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/marang/sway-session/internal/agentreport"
	sessionstate "github.com/marang/sway-session/internal/session"
)

const (
	ProtocolVersion        = 1
	SocketFilename         = "codex-report.sock"
	ContextIDEnvironment   = agentreport.ContextIDEnvironment
	HerdrPaneEnvironment   = agentreport.HerdrPaneEnvironment
	HerdrActiveEnvironment = agentreport.HerdrActiveEnvironment
	CodexThreadEnvironment = "CODEX_THREAD_ID"
	maxHookPayload         = 16 * 1024
)

var ErrNotManagedSession = agentreport.ErrNotManagedSession

type Report = agentreport.LegacyCodexReport

type response struct {
	Version int    `json:"version"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

func DefaultSocketPath() (string, error) {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" || !filepath.IsAbs(runtimeDir) {
		return "", errors.New("XDG_RUNTIME_DIR must be an absolute path")
	}
	return filepath.Join(filepath.Clean(runtimeDir), "sway-session", SocketFilename), nil
}

// ParseCodexHook converts Codex's extensible SessionStart JSON into the stable
// v1 report. Paths, source commands, and unknown fields remain ignored.
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
	var payload struct {
		HookEventName string `json:"hook_event_name"`
		SessionID     string `json:"session_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
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
	report := Report{Version: ProtocolVersion, ContextID: contextID, PaneID: getenv(HerdrPaneEnvironment), CodexSessionID: payload.SessionID}
	if inherited := getenv(CodexThreadEnvironment); inherited != "" && inherited != report.CodexSessionID {
		return Report{}, errors.New("codex hook session ID does not match CODEX_THREAD_ID")
	}
	if err := report.Validate(); err != nil {
		return Report{}, err
	}
	return report, nil
}
