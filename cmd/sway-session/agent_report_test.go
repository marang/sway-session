package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marang/sway-session/internal/agentreport"
)

func TestReportAgentSessionUsesOnlyReportBoundary(t *testing.T) {
	deps := testDependencies(t)
	deps.stateRoot = func() (string, error) { t.Fatal("report CLI must not access registry"); return "", nil }
	called := false
	payload := `{"agent":"claude","agent_session_id":"session-123"}`
	deps.reportAgentSession = func(_ context.Context, input io.Reader, getenv func(string) string) error {
		called = true
		data, err := io.ReadAll(input)
		if err != nil || string(data) != payload || getenv("PATH") != os.Getenv("PATH") {
			t.Fatalf("unexpected report boundary input=%q err=%v", data, err)
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	code := runWith([]string{"report-agent-session"}, strings.NewReader(payload), &stdout, &stderr, deps)
	if code != exitSuccess || !called || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("report failed code=%d called=%v stdout=%q stderr=%q", code, called, stdout.String(), stderr.String())
	}
}

func TestCodexSessionStartReportsThroughGenericBoundary(t *testing.T) {
	const sessionID = "01a04a4b-7fb9-7a90-8ace-51f7ae68e0ee"
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("CODEX_THREAD_ID", sessionID)
	t.Setenv("HERDR_SOCKET_PATH", "")
	deps := testDependencies(t)
	deps.stateRoot = func() (string, error) { t.Fatal("Codex hook must not access registry"); return "", nil }
	called := false
	deps.reportAgentSession = func(_ context.Context, input io.Reader, _ func(string) string) error {
		called = true
		data, err := io.ReadAll(input)
		if err != nil || string(data) != `{"agent":"codex","agent_session_id":"`+sessionID+`"}` {
			t.Fatalf("unexpected generic report %q: %v", data, err)
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	payload := `{"hook_event_name":"SessionStart","session_id":"` + sessionID + `","transcript_path":"/private/history","command":["ignored"]}`
	code := runWith([]string{"report-agent-session", "--codex-hook"}, strings.NewReader(payload), &stdout, &stderr, deps)
	if code != exitSuccess || !called || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d called=%v stdout=%q stderr=%q", code, called, stdout.String(), stderr.String())
	}
}

func TestCodexHookValidationAndNoop(t *testing.T) {
	const sessionID = "01a04a4b-7fb9-7a90-8ace-51f7ae68e0ee"
	valid := `{"hook_event_name":"SessionStart","session_id":"` + sessionID + `"}`
	for _, tc := range []struct {
		name, payload string
		env           map[string]string
		wantNoop      bool
	}{
		{"unmanaged", valid, map[string]string{}, true},
		{"unrelated event", `{"hook_event_name":"AfterAgent","session_id":"` + sessionID + `"}`, map[string]string{"HERDR_ENV": "1"}, true},
		{"stale socket", valid, map[string]string{"HERDR_ENV": "1", "HERDR_SOCKET_PATH": "/path/that/does/not/exist"}, true},
		{"nil UUID", `{"hook_event_name":"SessionStart","session_id":"00000000-0000-0000-0000-000000000000"}`, map[string]string{"HERDR_ENV": "1"}, false},
		{"uppercase UUID", `{"hook_event_name":"SessionStart","session_id":"01A04A4B-7FB9-7A90-8ACE-51F7AE68E0EE"}`, map[string]string{"HERDR_ENV": "1"}, false},
		{"thread mismatch", valid, map[string]string{"HERDR_ENV": "1", "CODEX_THREAD_ID": "223e4567-e89b-42d3-a456-426614174000"}, false},
		{"multiple objects", valid + `{}`, map[string]string{"HERDR_ENV": "1"}, false},
		{"non-object", `[]`, map[string]string{"HERDR_ENV": "1"}, false},
		{"bad type", `{"hook_event_name":5,"session_id":"` + sessionID + `"}`, map[string]string{"HERDR_ENV": "1"}, false},
		{"oversize", valid + strings.Repeat(" ", 16*1024), map[string]string{"HERDR_ENV": "1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string { return tc.env[key] }
			_, err := codexHookReport(strings.NewReader(tc.payload), getenv)
			if tc.wantNoop && !errors.Is(err, agentreport.ErrNotManagedSession) || !tc.wantNoop && (err == nil || errors.Is(err, agentreport.ErrNotManagedSession)) {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
	t.Run("live socket", func(t *testing.T) {
		listener, err := net.Listen("unix", t.TempDir()+"/herdr.sock")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		_, err = codexHookReport(strings.NewReader(valid), func(key string) string {
			switch key {
			case "HERDR_ENV":
				return "1"
			case "HERDR_SOCKET_PATH":
				return listener.Addr().String()
			}
			return ""
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("socket hidden by permissions", func(t *testing.T) {
		private := filepath.Join(t.TempDir(), "private")
		if err := os.Mkdir(private, 0700); err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("unix", filepath.Join(private, "herdr.sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		if err := os.Chmod(private, 0000); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(private, 0700)
		_, err = codexHookReport(strings.NewReader(valid), func(key string) string {
			switch key {
			case "HERDR_ENV":
				return "1"
			case "HERDR_SOCKET_PATH":
				return listener.Addr().String()
			}
			return ""
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestReportAgentSessionFailuresAndUnmanaged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		err    error
		code   int
		called bool
	}{
		{"unmanaged", nil, agentreport.ErrNotManagedSession, exitSuccess, true},
		{"rejected", nil, errors.New("report rejected"), exitOperation, true},
		{"arguments", []string{"--socket", "/tmp/other.sock"}, nil, exitUsage, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDependencies(t)
			called := false
			deps.reportAgentSession = func(context.Context, io.Reader, func(string) string) error { called = true; return tc.err }
			var stderr bytes.Buffer
			code := runWith(append([]string{"report-agent-session"}, tc.args...), strings.NewReader(`{}`), io.Discard, &stderr, deps)
			if code != tc.code || called != tc.called || (tc.code == exitSuccess && stderr.Len() != 0) {
				t.Fatalf("code=%d called=%v stderr=%q", code, called, stderr.String())
			}
		})
	}
}
