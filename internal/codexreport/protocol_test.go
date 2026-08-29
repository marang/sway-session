package codexreport

import (
	"errors"
	"strings"
	"testing"
)

const (
	testContextID = "123e4567-e89b-12d3-a456-426614174000"
	testSessionID = "01a04a4b-7fb9-7a90-8ace-51f7ae68e0ee"
)

func TestParseCodexHookIgnoresTranscriptCommandsAndSocketOverrides(t *testing.T) {
	environment := map[string]string{
		HerdrActiveEnvironment: "1",
		ContextIDEnvironment:   testContextID,
		HerdrPaneEnvironment:   "work:p1",
		CodexThreadEnvironment: testSessionID,
		"HERDR_SOCKET_PATH":    "/tmp/attacker.sock",
		"SWAYSOCK":             "/tmp/sway.sock",
	}
	payload := `{"hook_event_name":"SessionStart","session_id":"` + testSessionID + `","transcript_path":"/secret/history","source":"resume","command":["sh","-c","danger"]}`
	report, err := ParseCodexHook(strings.NewReader(payload), func(name string) string { return environment[name] })
	if err != nil {
		t.Fatal(err)
	}
	if report.ContextID != testContextID || report.PaneID != "work:p1" || report.CodexSessionID != testSessionID {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestParseCodexHookRejectsMismatchedOrInvalidSessionIDs(t *testing.T) {
	base := map[string]string{
		HerdrActiveEnvironment: "1", ContextIDEnvironment: testContextID,
		HerdrPaneEnvironment: "work:p1", CodexThreadEnvironment: testSessionID,
	}
	for name, payload := range map[string]string{
		"mismatch": `{"hook_event_name":"SessionStart","session_id":"223e4567-e89b-42d3-a456-426614174000"}`,
		"not uuid": `{"hook_event_name":"SessionStart","session_id":"run arbitrary command"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCodexHook(strings.NewReader(payload), func(name string) string { return base[name] }); err == nil {
				t.Fatal("expected invalid session identity to fail")
			}
		})
	}
}

func TestParseCodexHookIgnoresUnmanagedEvents(t *testing.T) {
	_, err := ParseCodexHook(strings.NewReader(`{"hook_event_name":"AfterAgent","session_id":"`+testSessionID+`"}`), func(string) string { return "" })
	if !errors.Is(err, ErrNotManagedSession) {
		t.Fatalf("unexpected error: %v", err)
	}
}
