package sessionrequest

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
)

func TestRequestValidationAcceptsOnlyTypedStartFields(t *testing.T) {
	request := Request{Version: ProtocolVersion, Session: "reboot-e2e", Cwd: filepath.Join(t.TempDir(), "project"), Label: "REBOOT-E2E", Provider: "local", Workspace: 7}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Request){
		"version":   func(value *Request) { value.Version++ },
		"session":   func(value *Request) { value.Session = "bad session" },
		"cwd":       func(value *Request) { value.Cwd = "relative" },
		"label":     func(value *Request) { value.Label = strings.Repeat("x", 257) },
		"workspace": func(value *Request) { value.Workspace = MaximumWorkspace + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := request
			mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatalf("invalid %s was accepted: %+v", name, candidate)
			}
		})
	}
}

func TestResponseProtocolV1OmitsRegistryV3Fields(t *testing.T) {
	archived := testTime(t, "2026-09-01T12:00:00Z")
	contextValue := sessionstate.Context{
		ID:         testContextID,
		Label:      "LAB-105",
		Provider:   "linear",
		State:      sessionstate.ContextActive,
		ArchivedAt: &archived,
		Launcher: sessionstate.Launcher{
			Kind:    sessionstate.LauncherHerdr,
			Session: "lab-105",
			Cwd:     "/home/example/project",
			Terminal: &sessionstate.TerminalLauncher{
				Adapter: sessionstate.TerminalAdapterFoot,
				Identity: &sessionstate.TerminalIdentity{
					Kind:    sessionstate.TerminalIdentityProject,
					Project: "LAB-105",
				},
			},
		},
	}
	encoded, err := encodeResponseV1(Response{
		Version: ProtocolVersion, OK: true, Context: &contextValue, Workspace: 98, Created: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"terminal"`, `"archived_at"`, `"identity"`} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("protocol v1 leaked registry-v3 field %s: %s", forbidden, encoded)
		}
	}
	if !bytes.Contains(encoded, []byte(`"launcher":{"kind":"herdr","session":"lab-105","cwd":"/home/example/project"}`)) {
		t.Fatalf("protocol v1 omitted legacy launcher fields: %s", encoded)
	}
}

func TestDecodeResponseV1UpgradesLegacyHerdrContextLocally(t *testing.T) {
	encoded := []byte(`{"version":1,"ok":true,"context":{"id":"11111111-1111-4111-8111-111111111111","label":"LAB-105","provider":"linear","state":"active","launcher":{"kind":"herdr","session":"lab-105","cwd":"/home/example/project"}},"workspace":98,"created":true}`)
	response, err := decodeResponseV1(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if response.Context == nil || response.Context.Launcher.Terminal == nil {
		t.Fatalf("legacy response did not receive local terminal compatibility data: %+v", response)
	}
	terminal := response.Context.Launcher.Terminal
	if terminal.Adapter != sessionstate.TerminalAdapterAlacritty || terminal.Identity != nil {
		t.Fatalf("unexpected legacy terminal compatibility data: %+v", terminal)
	}
}

func TestDecodeResponseV1RemainsStrict(t *testing.T) {
	for name, encoded := range map[string]string{
		"new registry field": `{"version":1,"ok":true,"context":{"id":"11111111-1111-4111-8111-111111111111","state":"active","archived_at":"2026-09-01T12:00:00Z","launcher":{"kind":"herdr","session":"lab-105","cwd":"/tmp"}},"workspace":98}`,
		"trailing data":      `{"version":1,"ok":false,"error":"request rejected"} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeResponseV1([]byte(encoded)); err == nil {
				t.Fatalf("unsafe response was accepted: %s", encoded)
			}
		})
	}
}

func testTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
