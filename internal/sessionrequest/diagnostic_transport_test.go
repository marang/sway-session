package sessionrequest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func diagnosticTestSocket(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "sr-diagnostic-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return filepath.Join(root, SocketFilename)
}

func readDiagnosticRejection(t *testing.T, socketPath string, request Request) Response {
	t.Helper()
	connection, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(connection).Decode(&fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 || fields["version"] == nil || fields["ok"] == nil || fields["error"] == nil {
		t.Fatalf("rejection changed top-level v1 fields: %v", fields)
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	response, err := decodeResponseV1(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if response.Version != 1 || response.OK || response.Context != nil || response.Workspace != 0 || response.Created {
		t.Fatalf("rejection exposed partial response metadata: %+v", response)
	}
	return response
}

func TestServerRequestDiagnosticRoundTripAndRedaction(t *testing.T) {
	for _, code := range requestDiagnosticCodes {
		t.Run(code, func(t *testing.T) {
			request := testRequest(t)
			request.Workspace = 98
			cause := errors.New("private /home/owner/project, herdr-session-secret, window-title-secret, provider-secret")
			rollback := errors.New("private rollback /home/owner/.local/state")
			original := &RequestDiagnostic{Code: code, ContextID: testContextID, Workspace: 98, Cause: errors.Join(cause, rollback)}
			if code == DiagnosticWorkspaceAmbiguous {
				original.ContextID = ""
			}
			reported := make(chan error, 2)
			socketPath := diagnosticTestSocket(t)
			server, err := StartServer(socketPath, func(_ context.Context, got Request) (Response, error) {
				return testResponse(got, true), fmt.Errorf("private handler wrapper: %w", original)
			}, func(err error) { reported <- err })
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err = Send(ctx, socketPath, request)
			var decoded *RequestDiagnostic
			if !errors.As(err, &decoded) || decoded.Code != code || decoded.ContextID != original.ContextID || decoded.Workspace != request.Workspace || decoded.Cause != nil {
				t.Fatalf("safe diagnostic lost: %v", err)
			}
			response := readDiagnosticRejection(t, socketPath, request)
			for _, forbidden := range []string{cause.Error(), rollback.Error(), "private", request.Cwd, request.Session, request.Label, request.Provider, "window-title", "provider-secret", "cause", "detail", "hint"} {
				if strings.Contains(response.Error, forbidden) || strings.Contains(err.Error(), forbidden) || strings.Contains(decoded.Hint(), forbidden) {
					t.Fatalf("broker exposed %q: response=%+v err=%v", forbidden, response, err)
				}
			}
			for range 2 {
				select {
				case logged := <-reported:
					if !errors.Is(logged, cause) || !errors.Is(logged, rollback) || !strings.Contains(logged.Error(), cause.Error()) || !strings.Contains(logged.Error(), rollback.Error()) {
						t.Fatalf("internal broker log lost causes: %v", logged)
					}
				case <-time.After(time.Second):
					t.Fatal("broker did not log the complete rejection")
				}
			}
		})
	}
}

func TestServerPartialInitializationRecoveryKeepsContextID(t *testing.T) {
	request := testRequest(t)
	request.Workspace = 98
	socketPath := diagnosticTestSocket(t)
	var attempt atomic.Int32
	server, err := StartServer(socketPath, func(_ context.Context, got Request) (Response, error) {
		if got != request {
			return Response{}, errors.New("retry changed the original request")
		}
		if attempt.Add(1) == 1 {
			return testResponse(got, true), &RequestDiagnostic{
				Code: DiagnosticInitializationFailed, ContextID: testContextID, Workspace: got.Workspace,
				Cause: errors.New("private initialized pane history and /home/owner/project"),
			}
		}
		return testResponse(got, false), nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = Send(ctx, socketPath, request)
	var partial *RequestDiagnostic
	if !errors.As(err, &partial) || partial.ContextID != testContextID || partial.Code != DiagnosticInitializationFailed {
		t.Fatalf("registered recovery UUID was lost: %v", err)
	}
	response, err := Send(ctx, socketPath, request)
	if err != nil || response.Context == nil || response.Context.ID != partial.ContextID || response.Created {
		t.Fatalf("exact retry did not recover the original context: response=%+v err=%v", response, err)
	}
}

func TestSendRejectsDiagnosticForAnotherRequestedWorkspace(t *testing.T) {
	request := testRequest(t)
	socketPath := diagnosticTestSocket(t)
	server, err := StartServer(socketPath, func(context.Context, Request) (Response, error) {
		return Response{}, &RequestDiagnostic{Code: DiagnosticWorkspaceConflict, ContextID: testContextID, Workspace: 98}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	_, err = Send(t.Context(), socketPath, request)
	var diagnostic *RequestDiagnostic
	if err == nil || errors.As(err, &diagnostic) || err.Error() != "session start request rejected" {
		t.Fatalf("diagnostic for a different request was accepted: %v", err)
	}
}

func TestServerInvalidRequestDiagnosticsStayGeneric(t *testing.T) {
	for name, rejection := range map[string]error{
		"unknown":         &RequestDiagnostic{Code: "private unknown reason", ContextID: testContextID, Workspace: 98},
		"invalid context": &RequestDiagnostic{Code: DiagnosticRestoreFailed, ContextID: "private context title", Workspace: 98},
		"missing context": &RequestDiagnostic{Code: DiagnosticRestoreFailed, Workspace: 98},
		"zero workspace":  &RequestDiagnostic{Code: DiagnosticRestoreFailed, ContextID: testContextID},
		"large workspace": &RequestDiagnostic{Code: DiagnosticRestoreFailed, ContextID: testContextID, Workspace: 100},
		"joined mismatch": errors.Join(errors.New("private split failure"), &ProtocolMismatchDiagnostic{ContextID: testContextID, Detail: "unsupported Herdr protocol 21"}),
	} {
		t.Run(name, func(t *testing.T) {
			request := testRequest(t)
			socketPath := diagnosticTestSocket(t)
			server, err := StartServer(socketPath, func(context.Context, Request) (Response, error) {
				return Response{}, rejection
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err = Send(ctx, socketPath, request)
			if err == nil || err.Error() != "session start request rejected" {
				t.Fatalf("invalid diagnostic was not generic: %v", err)
			}
			if response := readDiagnosticRejection(t, socketPath, request); response.Error != "request rejected" {
				t.Fatalf("invalid diagnostic exposed on wire: %+v", response)
			}
		})
	}
}

func TestSendUnknownMalformedAndInvalidDiagnosticsStayGeneric(t *testing.T) {
	for name, rejection := range invalidRequestDiagnosticWires() {
		if name == "oversized" {
			continue // The existing transport size bound rejects before decoding.
		}
		t.Run(name, func(t *testing.T) {
			socketPath := diagnosticTestSocket(t)
			listener, err := net.Listen("unix", socketPath)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				connection, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer connection.Close()
				if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
					done <- err
					return
				}
				if _, err := bufio.NewReader(connection).ReadBytes('\n'); err != nil {
					done <- err
					return
				}
				done <- json.NewEncoder(connection).Encode(Response{Version: ProtocolVersion, Error: rejection})
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err = Send(ctx, socketPath, testRequest(t))
			if err == nil || err.Error() != "session start request rejected" {
				t.Fatalf("untrusted diagnostic escaped generic fallback: %v", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("synthetic broker did not complete")
			}
		})
	}
}
