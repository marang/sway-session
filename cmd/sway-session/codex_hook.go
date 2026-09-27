package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/marang/sway-session/internal/agentreport"
	sessionstate "github.com/marang/sway-session/internal/session"
)

const maxCodexHookPayload = 16 * 1024

var canonicalCodexSessionID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// codexHookReport translates Codex's SessionStart event at the CLI edge. The
// generic reporter and its protocol remain independent of provider events.
func codexHookReport(input io.Reader, getenv func(string) string) (io.Reader, error) {
	if input == nil || getenv == nil {
		return nil, errors.New("codex hook input and environment are required")
	}
	data, err := io.ReadAll(io.LimitReader(input, maxCodexHookPayload+1))
	if err != nil {
		return nil, fmt.Errorf("read Codex hook payload: %w", err)
	}
	if len(data) > maxCodexHookPayload {
		return nil, fmt.Errorf("codex hook payload exceeds %d bytes", maxCodexHookPayload)
	}
	var payload map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode Codex hook payload: %w", err)
	}
	if payload == nil {
		return nil, errors.New("codex hook payload must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("codex hook payload contains multiple JSON values")
		}
		return nil, fmt.Errorf("decode trailing Codex hook data: %w", err)
	}
	event, err := codexHookString(payload, "hook_event_name")
	if err != nil {
		return nil, err
	}
	sessionID, err := codexHookString(payload, "session_id")
	if err != nil {
		return nil, err
	}
	if event != "SessionStart" || getenv(agentreport.HerdrActiveEnvironment) != "1" {
		return nil, agentreport.ErrNotManagedSession
	}
	if socketPath := getenv("HERDR_SOCKET_PATH"); socketPath != "" {
		info, err := os.Stat(socketPath)
		// The optional AppArmor profile hides Herdr's private directory even
		// from stat(2). In that case the broker still verifies pane ancestry.
		if err != nil && !errors.Is(err, os.ErrPermission) {
			return nil, agentreport.ErrNotManagedSession
		}
		if err == nil && info.Mode()&os.ModeSocket == 0 {
			return nil, agentreport.ErrNotManagedSession
		}
	}
	if !canonicalCodexSessionID.MatchString(sessionID) || sessionID == "00000000-0000-0000-0000-000000000000" {
		return nil, errors.New("codex session_id must be a canonical lowercase, non-nil UUID")
	}
	if threadID := getenv("CODEX_THREAD_ID"); threadID != "" && threadID != sessionID {
		return nil, errors.New("codex session_id does not match CODEX_THREAD_ID")
	}
	eventOrigin, err := codexHookString(payload, "source")
	if err != nil {
		return nil, err
	}
	if err := sessionstate.ValidateAgentEventOrigin(eventOrigin); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(struct {
		Agent          string `json:"agent"`
		AgentSessionID string `json:"agent_session_id"`
		EventOrigin    string `json:"event_origin,omitempty"`
	}{Agent: "codex", AgentSessionID: sessionID, EventOrigin: eventOrigin})
	if err != nil {
		return nil, fmt.Errorf("encode Codex agent report: %w", err)
	}
	return bytes.NewReader(encoded), nil
}

func codexHookString(payload map[string]json.RawMessage, name string) (string, error) {
	value := payload[name]
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return "", nil
	}
	var decoded string
	if err := json.Unmarshal(value, &decoded); err != nil {
		return "", fmt.Errorf("codex %s must be a string: %w", name, err)
	}
	return decoded, nil
}
