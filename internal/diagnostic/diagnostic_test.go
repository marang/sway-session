package diagnostic

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestTextDiagnosticEscapesTerminalControls(t *testing.T) {
	var output bytes.Buffer
	item := Diagnostic{
		Level:   LevelError,
		Message: "unknown \x1b[31mcommand",
		Hint:    "try\nagain",
	}

	if err := Write(&output, "sway-session", item, false); err != nil {
		t.Fatalf("write diagnostic: %v", err)
	}
	got := output.String()
	if strings.ContainsRune(got, '\x1b') {
		t.Fatalf("diagnostic contains a terminal escape: %q", got)
	}
	if !strings.Contains(got, `unknown \x1b[31mcommand`) || !strings.Contains(got, `try\nagain`) {
		t.Fatalf("diagnostic does not visibly escape controls: %q", got)
	}
	if strings.Count(got, "\n") != 2 {
		t.Fatalf("control characters injected extra lines: %q", got)
	}
}

func TestWriteAllUsesOneJSONEnvelope(t *testing.T) {
	var output bytes.Buffer
	items := []Diagnostic{
		{Level: LevelError, Code: "first", Message: "one"},
		{Level: LevelError, Code: "second", Message: "two"},
	}
	if err := WriteAll(&output, "sway-session", items, true); err != nil {
		t.Fatal(err)
	}
	var decoded envelope
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if len(decoded.Diagnostics) != 2 || decoded.Diagnostics[1].Code != "second" {
		t.Fatalf("unexpected envelope: %+v", decoded)
	}
}
