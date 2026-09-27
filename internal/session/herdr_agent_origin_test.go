package session

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This fake accepts only the adapter's fixed methods, and keeps association
// state independently of the report acknowledgement.
func fakeHerdrAgentEndpoint(t *testing.T, respond func(string, map[string]any) any) HerdrPaths {
	t.Helper()
	root, err := os.MkdirTemp("", "herdr-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	dir := filepath.Join(root, "sessions", "lab-138")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, herdrAgentSocketName)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { _ = listener.Close(); <-done })
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			var req struct {
				ID     string         `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			line, err := bufio.NewReader(conn).ReadBytes('\n')
			if err != nil {
				t.Error(err)
				_ = conn.Close()
				return
			}
			if err := json.Unmarshal(line, &req); err != nil {
				t.Error(err)
				_ = conn.Close()
				return
			}
			result := respond(req.Method, req.Params)
			data, err := json.Marshal(map[string]any{"id": req.ID, "result": result})
			if err == nil {
				_, err = conn.Write(append(data, '\n'))
			}
			_ = conn.Close()
			if err != nil {
				t.Error(err)
				return
			}
		}
	}()
	return HerdrPaths{Root: root}
}

func agentAssociationSnapshot(panes ...any) any {
	return map[string]any{"type": "session_snapshot", "snapshot": map[string]any{"panes": panes}}
}

func agentAssociationPane(source, agent, kind, value string) any {
	return map[string]any{"pane_id": "work:p1", "agent_session": map[string]any{"source": source, "agent": agent, "kind": kind, "value": value}}
}

func TestReportHerdrAgentSessionRejectsOKButIgnoredAssociation(t *testing.T) {
	paths := fakeHerdrAgentEndpoint(t, func(method string, params map[string]any) any {
		switch method {
		case "pane.process_info":
			return map[string]any{"type": "pane_process_info", "process_info": map[string]any{"pane_id": "work:p1", "shell_pid": os.Getpid()}}
		case "pane.report_agent_session":
			return map[string]any{"type": "ok"}
		case "session.snapshot":
			return agentAssociationSnapshot(agentAssociationPane("herdr:codex", "codex", "id", "session-A"))
		default:
			t.Errorf("unexpected method %s", method)
			return nil
		}
	})
	launcher := Launcher{Kind: LauncherHerdr, Session: "lab-138"}
	if err := ReportHerdrAgentSession(context.Background(), paths, launcher, "work:p1", "codex", "session-B", "", os.Getpid(), time.Unix(0, 100)); err == nil {
		t.Fatal("ok-but-ignored replacement falsely reported success")
	}
}

func TestReportHerdrAgentSessionPreservesReplacementOrigin(t *testing.T) {
	// A deliberately narrow stateful model of Herdr 0.9.1's authority decisions:
	// acknowledgements do not imply mutation, origins and sequences gate changes.
	// Codex replacement accepts startup/resume/clear/compact; other normalized
	// tokens remain unchanged on the wire and are ignored by this fake authority.
	for _, origin := range []string{"startup", "resume", "clear", "compact"} {
		t.Run(origin, func(t *testing.T) {
			current := ""
			expectedOrigins := make(chan string, 1)
			var previousSeq float64
			paths := fakeHerdrAgentEndpoint(t, func(method string, params map[string]any) any {
				switch method {
				case "pane.process_info":
					return map[string]any{"type": "pane_process_info", "process_info": map[string]any{"pane_id": "work:p1", "shell_pid": os.Getpid()}}
				case "pane.report_agent_session":
					event, supplied := params["session_start_source"].(string)
					expectedOrigin := <-expectedOrigins
					if event != expectedOrigin {
						t.Errorf("Herdr origin changed: got %q want %q", event, expectedOrigin)
					}
					if supplied && event == "" {
						t.Error("empty origin must be omitted")
					}
					seq := params["seq"].(float64)
					value := params["agent_session_id"].(string)
					if seq > previousSeq {
						previousSeq = seq
						if current == value || event == "startup" || event == "resume" || event == "clear" || event == "compact" {
							current = value
						}
					}
					return map[string]any{"type": "ok"}
				case "session.snapshot":
					if len(params) != 0 {
						t.Errorf("snapshot must use empty fixed params: %#v", params)
					}
					return agentAssociationSnapshot(agentAssociationPane("herdr:codex", "codex", "id", current))
				default:
					t.Errorf("unexpected method %s", method)
					return nil
				}
			})
			for _, step := range []struct {
				id, event string
				seq       int64
				success   bool
			}{
				{"session-A", "startup", 1, true},
				{"session-B", "", 2, false},
				{"session-B", "future_event-1", 3, false},
				// These tokens normalize in Herdr but cannot replace Codex IDs.
				{"session-B", "branch", 4, false},
				{"session-B", "new", 5, false},
				{"session-B", "fork", 6, false},
				{"session-B", "select", 7, false},
				{"session-B", origin, 2, false}, // Valid origin, stale sequence.
				{"session-B", origin, 8, true},
				{"session-B", origin, 9, true}, // Idempotent repeated association.
				{"session-A", "", 10, false},   // A late source-less A cannot displace B.
				{"session-B", "", 11, true},
			} {
				expectedOrigins <- step.event
				err := ReportHerdrAgentSession(context.Background(), paths, Launcher{Kind: LauncherHerdr, Session: "lab-138"}, "work:p1", "codex", step.id, step.event, os.Getpid(), time.Unix(0, step.seq))
				if (err == nil) != step.success {
					t.Fatalf("origin=%q seq=%d success=%v err=%v", step.event, step.seq, step.success, err)
				}
			}
		})
	}
}

func TestValidateAgentEventOrigin(t *testing.T) {
	for _, origin := range []string{"", "startup", "resume", "clear", "compact", "future_event-123", strings.Repeat("a", 64)} {
		if err := ValidateAgentEventOrigin(origin); err != nil {
			t.Errorf("valid origin %q: %v", origin, err)
		}
	}
	for _, origin := range []string{strings.Repeat("a", 65), "Startup", "1startup", "_startup", "-startup", " startup", "startup ", "start/up", "a\n", "a\x00", "ä", "aé"} {
		if err := ValidateAgentEventOrigin(origin); err == nil {
			t.Errorf("invalid origin %q accepted", origin)
		}
	}
}

func TestVerifyHerdrAgentAssociationRequiresExactUniqueSnapshot(t *testing.T) {
	good := agentAssociationPane("herdr:codex", "codex", "id", "private-session-B")
	for _, tc := range []struct {
		name   string
		result any
		valid  bool
	}{
		{"exact", agentAssociationSnapshot(good), true},
		{"duplicate pane", agentAssociationSnapshot(good, good), false},
		{"missing pane", agentAssociationSnapshot(), false},
		{"missing association", agentAssociationSnapshot(map[string]any{"pane_id": "work:p1"}), false},
		{"wrong pane", agentAssociationSnapshot(map[string]any{"pane_id": "work:p2", "agent_session": map[string]any{"source": "herdr:codex", "agent": "codex", "kind": "id", "value": "private-session-B"}}), false},
		{"wrong source", agentAssociationSnapshot(agentAssociationPane("untrusted:codex", "codex", "id", "private-session-B")), false},
		{"wrong agent", agentAssociationSnapshot(agentAssociationPane("herdr:codex", "claude", "id", "private-session-B")), false},
		{"wrong kind", agentAssociationSnapshot(agentAssociationPane("herdr:codex", "codex", "path", "private-session-B")), false},
		{"stale identity", agentAssociationSnapshot(agentAssociationPane("herdr:codex", "codex", "id", "private-session-A")), false},
		{"old schema", map[string]any{"type": "session_snapshot", "snapshot": map[string]any{}}, false},
		{"wrong result", map[string]any{"type": "ok"}, false},
		{"malformed panes", map[string]any{"type": "session_snapshot", "snapshot": map[string]any{"panes": true}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.result)
			if err != nil {
				t.Fatal(err)
			}
			err = verifyHerdrAgentAssociation(raw, "work:p1", "codex", "private-session-B")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "private-session-") {
				t.Fatalf("private identity leaked: %v", err)
			}
		})
	}
	if err := verifyHerdrAgentAssociation(json.RawMessage(`{"type":`), "work:p1", "codex", "private-session-B"); err == nil {
		t.Fatal("malformed snapshot accepted")
	}
}

func TestReportHerdrAgentSessionRejectsUnsupportedOrOversizedReadback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot any
		expected string
	}{
		{"unsupported", map[string]any{"type": "ok"}, "upgrade Herdr"},
		{"oversized", map[string]any{"type": "session_snapshot", "padding": strings.Repeat("x", maxHerdrAPIResponse)}, "exceeds"},
		{"identity authority rejects", agentAssociationSnapshot(agentAssociationPane("herdr:claude", "claude", "id", "private-session-A")), "did not confirm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := fakeHerdrAgentEndpoint(t, func(method string, params map[string]any) any {
				switch method {
				case "pane.process_info":
					return map[string]any{"type": "pane_process_info", "process_info": map[string]any{"pane_id": "work:p1", "shell_pid": os.Getpid()}}
				case "pane.report_agent_session":
					return map[string]any{"type": "ok"}
				case "session.snapshot":
					return tc.snapshot
				default:
					t.Errorf("unexpected method %s", method)
					return nil
				}
			})
			err := ReportHerdrAgentSession(context.Background(), paths, Launcher{Kind: LauncherHerdr, Session: "lab-138"}, "work:p1", "codex", "private-session-B", "startup", os.Getpid(), time.Unix(0, 1))
			if err == nil || !strings.Contains(err.Error(), tc.expected) {
				t.Fatalf("expected %q, got %v", tc.expected, err)
			}
			if strings.Contains(err.Error(), "private-session-") {
				t.Fatalf("private identity leaked: %v", err)
			}
		})
	}
}
