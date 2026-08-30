package sessionrequest

import (
	"path/filepath"
	"strings"
	"testing"
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
