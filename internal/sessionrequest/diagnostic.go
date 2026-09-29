package sessionrequest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	sessionstate "github.com/marang/sway-session/internal/session"
)

const protocolMismatchWirePrefix = "session-start-diagnostic-v1:"

// ProtocolMismatchDiagnostic identifies a registered context whose terminal
// initialization was stopped by a known Herdr protocol mismatch. Detail is
// deliberately limited to a diagnostic supplied by the typed initializer.
type ProtocolMismatchDiagnostic struct {
	ContextID sessionstate.ContextID
	Detail    string
}

func (diagnostic *ProtocolMismatchDiagnostic) Error() string {
	return fmt.Sprintf("context %s: %s", diagnostic.ContextID, diagnostic.Detail)
}

// SafeDiagnostic is implemented by a typed initializer error whose text is
// safe to show to the owner of a session-start request.
type SafeDiagnostic interface {
	SafeDiagnostic() string
}

func protocolMismatchAfterRegistration(contextID sessionstate.ContextID, err error) error {
	if contextID.Validate() != nil {
		return err
	}
	// A mismatch joined with a split or rollback failure is not a clean
	// pre-mutation rejection. Preserve every cause for the broker log and keep
	// the client-facing rejection generic in that case.
	var safe SafeDiagnostic
	for current := err; current != nil; current = errors.Unwrap(current) {
		if _, joined := current.(interface{ Unwrap() []error }); joined {
			return err
		}
		if candidate, ok := current.(SafeDiagnostic); ok {
			safe = candidate
		}
	}
	if safe == nil {
		return err
	}
	detail := safe.SafeDiagnostic()
	if !validSafeDetail(detail) {
		return err
	}
	return &ProtocolMismatchDiagnostic{ContextID: contextID, Detail: detail}
}

func validSafeDetail(detail string) bool {
	if detail == "" || len(detail) > 256 || strings.TrimSpace(detail) != detail {
		return false
	}
	for _, character := range detail {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

type protocolMismatchWire struct {
	Code      string                 `json:"code"`
	ContextID sessionstate.ContextID `json:"context_id"`
	Detail    string                 `json:"detail"`
}

func encodeProtocolMismatch(err error) string {
	var diagnostic *ProtocolMismatchDiagnostic
	if !errors.As(err, &diagnostic) || diagnostic == nil || diagnostic.ContextID.Validate() != nil || !validSafeDetail(diagnostic.Detail) {
		return "request rejected"
	}
	encoded, marshalErr := json.Marshal(protocolMismatchWire{
		Code: "herdr_protocol_mismatch", ContextID: diagnostic.ContextID, Detail: diagnostic.Detail,
	})
	if marshalErr != nil {
		return "request rejected"
	}
	return protocolMismatchWirePrefix + string(encoded)
}

func decodeProtocolMismatch(value string) (*ProtocolMismatchDiagnostic, bool) {
	if !strings.HasPrefix(value, protocolMismatchWirePrefix) {
		return nil, false
	}
	var wire protocolMismatchWire
	if err := json.Unmarshal([]byte(strings.TrimPrefix(value, protocolMismatchWirePrefix)), &wire); err != nil ||
		wire.Code != "herdr_protocol_mismatch" || wire.ContextID.Validate() != nil || !validSafeDetail(wire.Detail) {
		return nil, false
	}
	return &ProtocolMismatchDiagnostic{ContextID: wire.ContextID, Detail: wire.Detail}, true
}
