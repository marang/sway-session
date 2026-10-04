package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/marang/sway-session/internal/diagnostic"
	"github.com/marang/sway-session/internal/sessionrequest"
)

func TestRequestStartSafeDiagnosticsJSON(t *testing.T) {
	for _, code := range []string{
		sessionrequest.DiagnosticWorkspaceAmbiguous,
		sessionrequest.DiagnosticWorkspaceConflict,
		sessionrequest.DiagnosticWindowAmbiguous,
		sessionrequest.DiagnosticMappingPending,
		sessionrequest.DiagnosticContextChanged,
		sessionrequest.DiagnosticInitializationFailed,
		sessionrequest.DiagnosticRestoreFailed,
	} {
		t.Run(code, func(t *testing.T) {
			deps := testDependencies(t)
			problem := &sessionrequest.RequestDiagnostic{
				Code: code, ContextID: testContextID, Workspace: 98,
				Cause: errors.Join(errors.New("private /home/owner/project and window-title-secret"), errors.New("private rollback session-secret")),
			}
			if code == sessionrequest.DiagnosticWorkspaceAmbiguous {
				problem.ContextID = ""
			}
			deps.requestStart = func(context.Context, sessionrequest.Request) (sessionrequest.Response, error) {
				return sessionrequest.Response{}, fmt.Errorf("private transport wrapper: %w", problem)
			}
			var stdout, stderr bytes.Buffer
			status := runWith([]string{"--json", "request-start", "--session", "session-secret", "--label", "label-secret", "--provider", "provider-secret", "--workspace", "98"}, strings.NewReader(""), &stdout, &stderr, deps)
			if status != exitOperation || stdout.Len() != 0 {
				t.Fatalf("unexpected CLI status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
			}
			var envelope struct {
				Diagnostics []diagnostic.Diagnostic `json:"diagnostics"`
			}
			if err := json.Unmarshal(stderr.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if len(envelope.Diagnostics) != 1 {
				t.Fatalf("expected one diagnostic: %+v", envelope.Diagnostics)
			}
			got := envelope.Diagnostics[0]
			if got.Code != code || got.Level != diagnostic.LevelError || got.Details["workspace"] != float64(98) || !strings.Contains(got.Message, problem.Message()) || !strings.Contains(got.Message, "workspace 98") {
				t.Fatalf("typed fields not rendered: %+v", got)
			}
			if problem.ContextID == "" {
				if _, present := got.Details["context_id"]; present || len(got.Details) != 1 {
					t.Fatalf("pre-registration diagnostic exposed a context: %+v", got)
				}
			} else if got.Details["context_id"] != string(testContextID) || len(got.Details) != 2 || !strings.Contains(got.Message, string(testContextID)) {
				t.Fatalf("context UUID not rendered: %+v", got)
			}
			for _, expected := range []string{"exact original request-start", "--session", "--cwd", "--label", "--provider", "--workspace", "original working directory if --cwd was omitted"} {
				if !strings.Contains(got.Hint, expected) {
					t.Fatalf("retry hint omitted %q: %+v", expected, got)
				}
			}
			for _, forbidden := range []string{"private", "/home/owner", "window-title-secret", "session-secret", "label-secret", "provider-secret", "rollback"} {
				if strings.Contains(stderr.String(), forbidden) {
					t.Fatalf("CLI exposed %q: %s", forbidden, stderr.String())
				}
			}
		})
	}
}

func TestRequestStartPartialInitializationDiagnosticText(t *testing.T) {
	deps := testDependencies(t)
	deps.requestStart = func(context.Context, sessionrequest.Request) (sessionrequest.Response, error) {
		return sessionrequest.Response{}, &sessionrequest.RequestDiagnostic{
			Code: sessionrequest.DiagnosticInitializationFailed, ContextID: testContextID, Workspace: 98,
			Cause: errors.New("private path /home/owner and title-secret"),
		}
	}
	var stdout, stderr bytes.Buffer
	status := runWith([]string{"request-start", "--session", "session-secret", "--workspace", "98"}, strings.NewReader(""), &stdout, &stderr, deps)
	if status != exitOperation || stdout.Len() != 0 {
		t.Fatalf("unexpected CLI status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	for _, expected := range []string{"Terminal initialization failed", string(testContextID), "workspace 98", "context remains registered", "exact original request-start", "--cwd"} {
		if !strings.Contains(stderr.String(), expected) {
			t.Fatalf("recovery text omitted %q: %s", expected, stderr.String())
		}
	}
	for _, forbidden := range []string{"private", "/home/owner", "title-secret", "session-secret"} {
		if strings.Contains(stderr.String(), forbidden) {
			t.Fatalf("recovery text exposed %q: %s", forbidden, stderr.String())
		}
	}
}

func TestRequestStartInvalidTypedDiagnosticsJSONStaysGeneric(t *testing.T) {
	for name, problem := range map[string]*sessionrequest.RequestDiagnostic{
		"unknown code":    {Code: "private unknown code", ContextID: testContextID, Workspace: 98},
		"invalid UUID":    {Code: sessionrequest.DiagnosticRestoreFailed, ContextID: "private context", Workspace: 98},
		"missing UUID":    {Code: sessionrequest.DiagnosticRestoreFailed, Workspace: 98},
		"zero workspace":  {Code: sessionrequest.DiagnosticRestoreFailed, ContextID: testContextID},
		"large workspace": {Code: sessionrequest.DiagnosticRestoreFailed, ContextID: testContextID, Workspace: 100},
		"nil diagnostic":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			deps := testDependencies(t)
			if problem != nil {
				problem.Cause = errors.New("private path /home/owner and title-secret")
			}
			deps.requestStart = func(context.Context, sessionrequest.Request) (sessionrequest.Response, error) {
				return sessionrequest.Response{}, problem
			}
			var stdout, stderr bytes.Buffer
			status := runWith([]string{"--json", "request-start", "--session", "session-secret", "--workspace", "98"}, strings.NewReader(""), &stdout, &stderr, deps)
			if status != exitOperation || stdout.Len() != 0 {
				t.Fatalf("unexpected CLI status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
			}
			var envelope struct {
				Diagnostics []diagnostic.Diagnostic `json:"diagnostics"`
			}
			if err := json.Unmarshal(stderr.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if len(envelope.Diagnostics) != 1 || envelope.Diagnostics[0].Code != "session_request" || len(envelope.Diagnostics[0].Details) != 0 || !strings.Contains(envelope.Diagnostics[0].Hint, "exact original request-start") {
				t.Fatalf("invalid diagnostic did not fall back: %+v", envelope.Diagnostics)
			}
			for _, forbidden := range []string{"private", "/home/owner", "title-secret", "session-secret", string(testContextID), "workspace 100"} {
				if strings.Contains(stderr.String(), forbidden) {
					t.Fatalf("invalid diagnostic exposed %q: %s", forbidden, stderr.String())
				}
			}
		})
	}
}
