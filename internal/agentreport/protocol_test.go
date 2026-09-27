package agentreport

import (
	"errors"
	"strings"
	"testing"
)

const (
	testContextID = "123e4567-e89b-12d3-a456-426614174000"
	testSessionID = "claude:thread-01"
)

func TestParseAgentReportUsesOnlyFixedPayloadAndManagedEnvironment(t *testing.T) {
	environment := map[string]string{
		HerdrActiveEnvironment: "1",
		ContextIDEnvironment:   testContextID,
		HerdrPaneEnvironment:   "work:p1",
		"HERDR_SOCKET_PATH":    "/tmp/attacker.sock",
	}
	report, err := ParseAgentReport(strings.NewReader(`{"agent":"claude","agent_session_id":"`+testSessionID+`"}`), func(name string) string { return environment[name] })
	if err != nil {
		t.Fatal(err)
	}
	if report.ContextID != testContextID || report.PaneID != "work:p1" || report.Agent != "claude" || report.AgentSessionID != testSessionID {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestParseAgentReportRejectsExtraFieldsAndUnsafeIdentity(t *testing.T) {
	environment := map[string]string{HerdrActiveEnvironment: "1", ContextIDEnvironment: testContextID, HerdrPaneEnvironment: "work:p1"}
	for name, payload := range map[string]string{
		"extra":  `{"agent":"claude","agent_session_id":"safe","command":["sh","-c","danger"]}`,
		"kind":   `{"agent":"future-agent","agent_session_id":"safe"}`,
		"token":  `{"agent":"claude","agent_session_id":"-unsafe"}`,
		"spaces": `{"agent":"claude","agent_session_id":"run command"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseAgentReport(strings.NewReader(payload), func(name string) string { return environment[name] }); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestParseAgentReportIgnoresUnmanagedSession(t *testing.T) {
	_, err := ParseAgentReport(strings.NewReader(`{"agent":"claude","agent_session_id":"safe"}`), func(string) string { return "" })
	if !errors.Is(err, ErrNotManagedSession) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseAgentReportAcceptsNativeEventOrigin(t *testing.T) {
	environment := map[string]string{HerdrActiveEnvironment: "1", ContextIDEnvironment: testContextID, HerdrPaneEnvironment: "work:p1"}
	_, err := ParseAgentReport(strings.NewReader(`{"agent":"codex","agent_session_id":"thread-b","event_origin":"resume"}`), func(key string) string { return environment[key] })
	if err != nil {
		t.Fatalf("event origin cannot reach broker: %v", err)
	}
}

func TestAgentEventOriginValidationAndUnknownValues(t *testing.T) {
	env := map[string]string{HerdrActiveEnvironment: "1", ContextIDEnvironment: testContextID, HerdrPaneEnvironment: "work:p1"}
	for _, origin := range []string{"", "startup", "resume", "clear", "compact", "future_origin"} {
		payload := `{"agent":"codex","agent_session_id":"thread-b","event_origin":"` + origin + `"}`
		r, err := ParseAgentReport(strings.NewReader(payload), func(k string) string { return env[k] })
		if err != nil || r.EventOrigin != origin || r.Version != 3 {
			t.Fatalf("origin %q: report=%+v err=%v", origin, r, err)
		}
	}
	for _, origin := range []string{" resume", "RESUME", "resume;exec", "évent", strings.Repeat("a", 65)} {
		payload := `{"agent":"codex","agent_session_id":"thread-b","event_origin":"` + origin + `"}`
		if _, err := ParseAgentReport(strings.NewReader(payload), func(k string) string { return env[k] }); err == nil {
			t.Fatalf("invalid origin accepted: %q", origin)
		}
	}
}
