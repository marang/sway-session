package sessionrequest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	sessionstate "github.com/marang/sway-session/internal/session"
)

const protocolMismatchWirePrefix = "session-start-diagnostic-v1:"

const (
	DiagnosticWorkspaceAmbiguous   = "workspace_ambiguous"
	DiagnosticWorkspaceConflict    = "workspace_conflict"
	DiagnosticWindowAmbiguous      = "window_ambiguous"
	DiagnosticMappingPending       = "mapping_pending"
	DiagnosticContextChanged       = "context_changed"
	DiagnosticInitializationFailed = "initialization_failed"
	DiagnosticRestoreFailed        = "restore_failed"
)

// RequestDiagnostic retains the complete internal cause for broker logging.
// Only its validated code, context ID and requested workspace cross the wire.
// ContextID may be absent only for workspace ambiguity before registration.
type RequestDiagnostic struct {
	Code      string
	ContextID sessionstate.ContextID
	Workspace int
	Cause     error
}

func (diagnostic *RequestDiagnostic) Error() string {
	message := fmt.Sprintf("session start %s: context %s, workspace %d", diagnostic.Code, diagnostic.ContextID, diagnostic.Workspace)
	if diagnostic.Cause != nil {
		return fmt.Sprintf("%s: %v", message, diagnostic.Cause)
	}
	return message
}

func (diagnostic *RequestDiagnostic) Unwrap() error { return diagnostic.Cause }

// Validate checks the complete public diagnostic, without inspecting Cause.
func (diagnostic *RequestDiagnostic) Validate() error {
	if diagnostic == nil || diagnostic.Message() == "" {
		return errors.New("unknown session start diagnostic")
	}
	if diagnostic.Workspace < 1 || diagnostic.Workspace > MaximumWorkspace {
		return errors.New("invalid session start diagnostic workspace")
	}
	if diagnostic.ContextID == "" && diagnostic.Code == DiagnosticWorkspaceAmbiguous {
		return nil
	}
	if diagnostic.ContextID.Validate() != nil {
		return errors.New("invalid session start diagnostic context ID")
	}
	return nil
}

// Message and Hint are fixed text selected solely from the allowlisted code.
// Call Validate before rendering context and workspace fields alongside them.
func (diagnostic *RequestDiagnostic) Message() string {
	switch diagnostic.Code {
	case DiagnosticWorkspaceAmbiguous:
		return "Requested workspace is ambiguous"
	case DiagnosticWorkspaceConflict:
		return "Context placement conflicts with the requested workspace"
	case DiagnosticWindowAmbiguous:
		return "Context has ambiguous managed windows"
	case DiagnosticMappingPending:
		return "Context window mapping is still pending"
	case DiagnosticContextChanged:
		return "Context changed during session start"
	case DiagnosticInitializationFailed:
		return "Terminal initialization failed"
	case DiagnosticRestoreFailed:
		return "Context restore failed"
	default:
		return ""
	}
}

func (diagnostic *RequestDiagnostic) Hint() string {
	const retry = " Retry the exact original request-start request, keeping --session, --cwd, --label, --provider and --workspace unchanged; use the original working directory if --cwd was omitted."
	switch diagnostic.Code {
	case DiagnosticWorkspaceAmbiguous:
		return "Resolve duplicate numbered workspace names; other windows on the requested workspace are allowed." + retry
	case DiagnosticWorkspaceConflict:
		return "Inspect the context's current and saved workspace placement; resolve the conflict." + retry
	case DiagnosticWindowAmbiguous:
		return "Resolve duplicate or invalid managed window identities for this context." + retry
	case DiagnosticMappingPending:
		return "Wait for the existing terminal window to map or its pending process to exit; inspect this context before retrying." + retry
	case DiagnosticContextChanged:
		return "Inspect whether this context was archived, removed or replaced before retrying." + retry
	case DiagnosticInitializationFailed:
		return "The context remains registered. Inspect its terminal and the broker log; repair initialization." + retry
	case DiagnosticRestoreFailed:
		return "The context remains registered. Inspect its windows, pending processes and the broker log; repair restore." + retry
	default:
		return ""
	}
}

type requestDiagnosticWire struct {
	Code      string                 `json:"code"`
	ContextID sessionstate.ContextID `json:"context_id,omitempty"`
	Workspace int                    `json:"workspace"`
}

func encodeRequestError(err error) string {
	// A diagnostic must describe the whole rejection, not one branch of a
	// joined failure. An explicit RequestDiagnostic may retain joined causes.
	for current := err; current != nil; current = errors.Unwrap(current) {
		if _, joined := current.(interface{ Unwrap() []error }); joined {
			return "request rejected"
		}
		if diagnostic, ok := current.(*RequestDiagnostic); ok {
			if diagnostic.Validate() != nil {
				return "request rejected"
			}
			encoded, marshalErr := json.Marshal(requestDiagnosticWire{
				Code: diagnostic.Code, ContextID: diagnostic.ContextID, Workspace: diagnostic.Workspace,
			})
			if marshalErr != nil {
				return "request rejected"
			}
			return protocolMismatchWirePrefix + string(encoded)
		}
	}
	return encodeProtocolMismatch(err)
}

func decodeRequestDiagnostic(value string) (*RequestDiagnostic, bool) {
	if !strings.HasPrefix(value, protocolMismatchWirePrefix) || len(value) > maxProtocolMessage {
		return nil, false
	}
	decoder := json.NewDecoder(strings.NewReader(strings.TrimPrefix(value, protocolMismatchWirePrefix)))
	decoder.DisallowUnknownFields()
	var wire requestDiagnosticWire
	if err := decoder.Decode(&wire); err != nil {
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, false
	}
	diagnostic := &RequestDiagnostic{Code: wire.Code, ContextID: wire.ContextID, Workspace: wire.Workspace}
	if diagnostic.Validate() != nil {
		return nil, false
	}
	return diagnostic, true
}

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
