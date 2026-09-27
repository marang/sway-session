// Package agentreport implements a narrow, provider-neutral agent-session
// reporting boundary. It intentionally exposes no pane-control operation.
package agentreport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	sessionstate "github.com/marang/sway-session/internal/session"
)

const (
	ProtocolVersion        = 3
	LegacyProtocolVersion  = 2
	SocketFilename         = "agent-report.sock"
	ContextIDEnvironment   = "SWAY_SESSION_CONTEXT_ID"
	HerdrPaneEnvironment   = "HERDR_PANE_ID"
	HerdrActiveEnvironment = "HERDR_ENV"
	maxHookPayload         = 16 * 1024
	maxProtocolMessage     = 8 * 1024
)

var ErrNotManagedSession = errors.New("agent session did not start in a managed Herdr context")

// Report is the generic broker request. Version 3 carries an optional native
// event origin; version 2 is accepted only without that field. Context and pane
// identity come exclusively from the managed terminal environment.
type Report struct {
	Version        int                    `json:"version"`
	ContextID      sessionstate.ContextID `json:"context_id"`
	PaneID         string                 `json:"pane_id"`
	Agent          string                 `json:"agent"`
	AgentSessionID string                 `json:"agent_session_id"`
	EventOrigin    string                 `json:"event_origin,omitempty"`
	PeerPID        int                    `json:"-"`
}

type response struct {
	Version int    `json:"version"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

func (report Report) Validate() error {
	if report.Version != ProtocolVersion && report.Version != LegacyProtocolVersion {
		return fmt.Errorf("unsupported agent report protocol version %d", report.Version)
	}
	if report.Version == LegacyProtocolVersion && report.EventOrigin != "" {
		return errors.New("agent report protocol v2 cannot carry event_origin; upgrade the client and restart the sway-session daemon")
	}
	if err := sessionstate.ValidateAgentEventOrigin(report.EventOrigin); err != nil {
		return err
	}
	if err := report.ContextID.Validate(); err != nil {
		return fmt.Errorf("invalid context ID: %w", err)
	}
	if err := validateIdentity("Herdr pane ID", report.PaneID, 256); err != nil {
		return err
	}
	if !sessionstate.ValidHerdrAgentKind(report.Agent) {
		return fmt.Errorf("unsupported Herdr agent kind %q", report.Agent)
	}
	if err := sessionstate.ValidateHerdrAgentSessionID(report.AgentSessionID); err != nil {
		return err
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

// ParseAgentReport converts a provider-neutral hook payload into the fixed
// v3 report contract. It deliberately accepts no paths, commands, sockets, or
// context/pane identifiers from provider input.
func ParseAgentReport(input io.Reader, getenv func(string) string) (Report, error) {
	if input == nil || getenv == nil {
		return Report{}, errors.New("agent report input and environment are required")
	}
	data, err := io.ReadAll(io.LimitReader(input, maxHookPayload+1))
	if err != nil {
		return Report{}, fmt.Errorf("read agent report payload: %w", err)
	}
	if len(data) > maxHookPayload {
		return Report{}, fmt.Errorf("agent report payload exceeds %d bytes", maxHookPayload)
	}
	var payload struct {
		Agent          string `json:"agent"`
		AgentSessionID string `json:"agent_session_id"`
		EventOrigin    string `json:"event_origin,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return Report{}, fmt.Errorf("decode agent report payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Report{}, errors.New("agent report payload contains multiple JSON values")
		}
		return Report{}, fmt.Errorf("decode trailing agent report data: %w", err)
	}
	if getenv(HerdrActiveEnvironment) != "1" {
		return Report{}, ErrNotManagedSession
	}
	contextID, err := sessionstate.ParseContextID(getenv(ContextIDEnvironment))
	if err != nil {
		return Report{}, fmt.Errorf("invalid %s: %w", ContextIDEnvironment, err)
	}
	report := Report{Version: ProtocolVersion, ContextID: contextID, PaneID: getenv(HerdrPaneEnvironment), Agent: payload.Agent, AgentSessionID: payload.AgentSessionID, EventOrigin: payload.EventOrigin}
	if err := report.Validate(); err != nil {
		return Report{}, err
	}
	return report, nil
}

func validateIdentity(name string, value string, maximum int) error {
	if value == "" || len(value) > maximum || value != string(bytes.TrimSpace([]byte(value))) {
		return fmt.Errorf("%s must contain between 1 and %d bytes without surrounding whitespace", name, maximum)
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("%s must not contain control characters", name)
		}
	}
	return nil
}
