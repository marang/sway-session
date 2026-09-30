package session

import (
	"context"
	"time"
)

// TerminalSessionObservation is ephemeral evidence, never restore policy or
// permission to terminate a session. Unknown differs from observed absence.
type TerminalSessionObservation struct {
	SessionState string                    `json:"session_state"`
	AgentState   string                    `json:"agent_state"`
	ObservedAt   *time.Time                `json:"observed_at,omitempty"`
	Reason       string                    `json:"reason,omitempty"`
	Directory    HerdrDirectoryObservation `json:"-"`
}

// TerminalSessionObserver shares discovery across a refresh and never starts
// a manager or changes its state. Keys are exact manager session names.
type TerminalSessionObserver interface {
	ObserveSessions(context.Context, []string) map[string]TerminalSessionObservation
}

func UnknownTerminalSessionObservation(reason string) TerminalSessionObservation {
	return TerminalSessionObservation{SessionState: "unknown", AgentState: "unknown", Reason: reason}
}
