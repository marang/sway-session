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
