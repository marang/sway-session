package sessionrequest

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	sessionstate "github.com/marang/sway-session/internal/session"
)

var requestDiagnosticCodes = []string{
	DiagnosticWorkspaceAmbiguous,
	DiagnosticWorkspaceConflict,
	DiagnosticWindowAmbiguous,
	DiagnosticMappingPending,
	DiagnosticContextChanged,
	DiagnosticInitializationFailed,
	DiagnosticRestoreFailed,
}

func TestRequestDiagnosticRetainsFullInternalCause(t *testing.T) {
	first := errors.New("private project /home/owner/project")
	second := errors.New("private window title and session metadata")
	diagnostic := &RequestDiagnostic{
		Code: DiagnosticInitializationFailed, ContextID: testContextID, Workspace: 98,
		Cause: errors.Join(first, second),
	}
	if !errors.Is(diagnostic, first) || !errors.Is(diagnostic, second) || diagnostic.Unwrap() != diagnostic.Cause {
		t.Fatalf("internal causes were lost: %v", diagnostic)
	}
	for _, want := range []string{diagnostic.Code, string(testContextID), "98", first.Error(), second.Error()} {
		if !strings.Contains(diagnostic.Error(), want) {
			t.Fatalf("internal log omitted %q: %v", want, diagnostic)
		}
	}
}

func TestRequestDiagnosticValidation(t *testing.T) {
	for _, code := range requestDiagnosticCodes {
		for _, workspace := range []int{1, 99} {
			diagnostic := &RequestDiagnostic{Code: code, ContextID: testContextID, Workspace: workspace}
			if err := diagnostic.Validate(); err != nil {
				t.Fatalf("valid diagnostic rejected: %+v: %v", diagnostic, err)
			}
			diagnostic.ContextID = ""
			if got := diagnostic.Validate() == nil; got != (code == DiagnosticWorkspaceAmbiguous) {
				t.Fatalf("unexpected optional context for %s: valid=%v", code, got)
			}
		}
	}
	for name, diagnostic := range map[string]*RequestDiagnostic{
		"nil":                nil,
		"unknown code":       {Code: "private failure", ContextID: testContextID, Workspace: 98},
		"invalid UUID":       {Code: DiagnosticRestoreFailed, ContextID: "private session name", Workspace: 98},
		"nil UUID":           {Code: DiagnosticRestoreFailed, ContextID: "00000000-0000-0000-0000-000000000000", Workspace: 98},
		"uppercase UUID":     {Code: DiagnosticRestoreFailed, ContextID: "AAAAAAAA-1111-4111-8111-111111111111", Workspace: 98},
		"zero workspace":     {Code: DiagnosticRestoreFailed, ContextID: testContextID},
		"negative workspace": {Code: DiagnosticRestoreFailed, ContextID: testContextID, Workspace: -1},
		"large workspace":    {Code: DiagnosticRestoreFailed, ContextID: testContextID, Workspace: 100},
	} {
		t.Run(name, func(t *testing.T) {
			if diagnostic.Validate() == nil {
				t.Fatal("invalid diagnostic accepted")
			}
		})
	}
}

type unrenderableDiagnosticCause struct{}

func (unrenderableDiagnosticCause) Error() string { panic("wire encoder must not render Cause") }

func TestRequestDiagnosticWireUsesOnlyKnownFields(t *testing.T) {
	for _, code := range requestDiagnosticCodes {
		t.Run(code, func(t *testing.T) {
			original := &RequestDiagnostic{Code: code, ContextID: testContextID, Workspace: 98}
			wrapped := fmt.Errorf("private outer wrapper: %w", original)
			original.Cause = unrenderableDiagnosticCause{}
			encoded := encodeRequestError(wrapped)
			decoded, ok := decodeRequestDiagnostic(encoded)
			if !ok || decoded.Code != code || decoded.ContextID != original.ContextID || decoded.Workspace != 98 || decoded.Cause != nil {
				t.Fatalf("diagnostic lost across wire: encoded=%s decoded=%+v", encoded, decoded)
			}
			if decoded.Message() == "" || decoded.Hint() == "" || !strings.Contains(decoded.Hint(), "exact original request-start") {
				t.Fatalf("diagnostic has no fixed recovery guidance: %+v", decoded)
			}
			if strings.Contains(encoded, "private") || strings.Contains(encoded, "cause") || strings.Contains(encoded, "detail") || strings.Contains(encoded, "hint") {
				t.Fatalf("wire included non-allowlisted fields: %s", encoded)
			}
			if _, recognized := decodeProtocolMismatch(encoded); recognized {
				t.Fatal("older CLI would interpret new reason as a Herdr mismatch")
			}
		})
	}
	diagnostic := &RequestDiagnostic{Code: DiagnosticWorkspaceAmbiguous, Workspace: 98}
	decoded, ok := decodeRequestDiagnostic(encodeRequestError(diagnostic))
	if !ok || decoded.ContextID != "" {
		t.Fatalf("pre-registration ambiguity was lost: %+v", decoded)
	}
}

func TestRequestErrorDoesNotSelectOneJoinedFailure(t *testing.T) {
	diagnostic := &RequestDiagnostic{Code: DiagnosticRestoreFailed, ContextID: testContextID, Workspace: 98}
	mismatch := &ProtocolMismatchDiagnostic{ContextID: testContextID, Detail: "unsupported Herdr protocol 21"}
	for _, err := range []error{
		errors.Join(diagnostic, errors.New("private rollback error")),
		fmt.Errorf("wrapped: %w", errors.Join(errors.New("private split error"), mismatch)),
		&RequestDiagnostic{Code: "unknown", ContextID: testContextID, Workspace: 98, Cause: diagnostic},
		(*RequestDiagnostic)(nil),
	} {
		if got := encodeRequestError(err); got != "request rejected" {
			t.Fatalf("unsafe compound or invalid rejection encoded: %s", got)
		}
	}
	// A service-selected diagnostic can deliberately retain all internal causes.
	diagnostic.Cause = errors.Join(errors.New("private restore error"), mismatch)
	decoded, ok := decodeRequestDiagnostic(encodeRequestError(diagnostic))
	if !ok || decoded.Code != DiagnosticRestoreFailed {
		t.Fatalf("explicit diagnostic with joined causes was lost: %+v", decoded)
	}
}

func invalidRequestDiagnosticWires() map[string]string {
	id := string(testContextID)
	valid := fmt.Sprintf(`{"code":"restore_failed","context_id":%q,"workspace":98}`, id)
	return map[string]string{
		"plain legacy rejection": "request rejected",
		"untyped private error":  "private path /home/owner and window title",
		"malformed":              protocolMismatchWirePrefix + "{",
		"null":                   protocolMismatchWirePrefix + "null",
		"unknown code":           protocolMismatchWirePrefix + fmt.Sprintf(`{"code":"private_failure","context_id":%q,"workspace":98}`, id),
		"invalid UUID":           protocolMismatchWirePrefix + `{"code":"restore_failed","context_id":"private session","workspace":98}`,
		"missing context":        protocolMismatchWirePrefix + `{"code":"restore_failed","workspace":98}`,
		"empty context":          protocolMismatchWirePrefix + `{"code":"workspace_ambiguous","context_id":"","workspace":98}`,
		"missing workspace":      protocolMismatchWirePrefix + fmt.Sprintf(`{"code":"restore_failed","context_id":%q}`, id),
		"zero workspace":         protocolMismatchWirePrefix + strings.Replace(valid, "98", "0", 1),
		"negative workspace":     protocolMismatchWirePrefix + strings.Replace(valid, "98", "-1", 1),
		"large workspace":        protocolMismatchWirePrefix + strings.Replace(valid, "98", "100", 1),
		"string workspace":       protocolMismatchWirePrefix + strings.Replace(valid, "98", `"98"`, 1),
		"fractional workspace":   protocolMismatchWirePrefix + strings.Replace(valid, "98", "98.5", 1),
		"private detail":         protocolMismatchWirePrefix + strings.TrimSuffix(valid, "}") + `,"detail":"private path /home/owner"}`,
		"private cause":          protocolMismatchWirePrefix + strings.TrimSuffix(valid, "}") + `,"cause":"private session and title"}`,
		"private hint":           protocolMismatchWirePrefix + strings.TrimSuffix(valid, "}") + `,"hint":"private metadata"}`,
		"trailing document":      protocolMismatchWirePrefix + valid + ` {}`,
		"oversized":              protocolMismatchWirePrefix + strings.Repeat(" ", maxProtocolMessage) + valid,
	}
}

func TestRequestDiagnosticRejectsUnknownMalformedAndInvalidWires(t *testing.T) {
	for name, wire := range invalidRequestDiagnosticWires() {
		t.Run(name, func(t *testing.T) {
			if diagnostic, ok := decodeRequestDiagnostic(wire); ok {
				t.Fatalf("invalid wire accepted: %+v", diagnostic)
			}
		})
	}
}

func TestProtocolMismatchEnvelopeRemainsCompatible(t *testing.T) {
	mismatch := &ProtocolMismatchDiagnostic{ContextID: sessionstate.ContextID(testContextID), Detail: `unsupported Herdr snapshot version "0.9.0" protocol 21`}
	encoded := encodeRequestError(fmt.Errorf("internal wrapper: %w", mismatch))
	if encoded != encodeProtocolMismatch(mismatch) {
		t.Fatalf("legacy mismatch encoding changed: %s", encoded)
	}
	if _, ok := decodeRequestDiagnostic(encoded); ok {
		t.Fatal("legacy mismatch decoded as a new request diagnostic")
	}
	decoded, ok := decodeProtocolMismatch(encoded)
	if !ok || *decoded != *mismatch {
		t.Fatalf("legacy mismatch lost: %+v", decoded)
	}
}
