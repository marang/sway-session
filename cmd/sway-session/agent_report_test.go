package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
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
