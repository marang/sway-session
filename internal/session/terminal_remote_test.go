package session

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRemoteBindingValidation(t *testing.T) {
	for _, target := range []string{"workbox", "user@workbox", "ssh://user@host:2222", "ssh://[::1]:2222"} {
		if err := (TerminalRemoteBinding{Target: target, Session: "default"}).Validate(); err != nil {
			t.Errorf("valid %q: %v", target, err)
		}
	}
	for _, target := range []string{"", "-oProxyCommand=bad", "work box", "host\nnext", "host;bad", "ssh://user:secret@host", "https://host", "ssh://host/path", "ssh://host?x=y", "host/command"} {
		if err := (TerminalRemoteBinding{Target: target, Session: "work"}).Validate(); err == nil {
			t.Errorf("accepted %q", target)
		}
	}
}

func TestRemoteContextPinsRouteAcrossRoundTripAndIdentityReuse(t *testing.T) {
	registry := Registry{Version: ContextsSchemaVersion}
	identity := TerminalIdentity{Kind: TerminalIdentityProject, Project: "remote-project"}
	binding := TerminalRemoteBinding{Target: "alpha", Session: "same-name"}
	request := TerminalContextRequest{Identity: identity, Adapter: TerminalAdapterAlacritty, Cwd: "/", Remote: &binding}
	created, fresh, err := EnsureTerminalContext(&registry, request, func() (ContextID, error) { return ContextID("37600000-0000-4000-8000-000000000001"), nil })
	if err != nil || !fresh {
		t.Fatalf("create: %v %v", fresh, err)
	}
	binding.Target = "beta"
	if created.Launcher.Terminal.Remote.Target != "alpha" {
		t.Fatal("request alias mutated durable binding")
	}
	if _, _, err := EnsureTerminalContext(&registry, request, nil); !errors.Is(err, ErrTerminalIdentityConflict) {
		t.Fatalf("retarget reused identity: %v", err)
	}
	request.Remote = nil
	if _, _, err := EnsureTerminalContext(&registry, request, nil); !errors.Is(err, ErrTerminalIdentityConflict) {
		t.Fatalf("local route reused remote identity: %v", err)
	}
	root := filepath.Join(t.TempDir(), "state")
	if err := RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	var loaded Registry
	if err := RegistryStoreFor(root).LoadInto(&loaded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(registry, loaded) {
		t.Fatalf("route changed on round trip: %+v", loaded)
	}
	data, _ := json.Marshal(created.Launcher.Terminal)
	// This reproduces the previous strict terminal payload reader's unknown-field
	// rejection, instead of silently treating a remote context as local.
	var old struct {
		Adapter  TerminalAdapter   `json:"adapter"`
		Identity *TerminalIdentity `json:"identity,omitempty"`
		Instance bool              `json:"instance,omitempty"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&old); err == nil {
		t.Fatal("old reader accepted remote binding")
	}
}

func TestRemoteLaunchUsesExactRouteWithLocalWindowIdentity(t *testing.T) {
	for _, adapter := range []TerminalAdapter{TerminalAdapterAlacritty, TerminalAdapterFoot} {
		registry := Registry{Version: ContextsSchemaVersion}
		c, err := CreateTerminalInstanceContext(&registry, TerminalInstanceRequest{Adapter: adapter, Cwd: "/", Remote: &TerminalRemoteBinding{Target: "ssh://user@host:2222", Session: "default"}}, func() (ContextID, error) { return ContextID("37600000-0000-4000-8000-000000000002"), nil })
		if err != nil {
			t.Fatal(err)
		}
		spec, err := BuildTerminalProcessSpec(c, "/usr/bin/terminal", "/usr/bin/herdr", "/private/config.toml")
		if err != nil {
			t.Fatal(err)
		}
		tail := []string{"/usr/bin/herdr", "--remote", "ssh://user@host:2222", "--session", "default"}
		if !reflect.DeepEqual(spec.Arguments[len(spec.Arguments)-len(tail):], tail) {
			t.Fatalf("wrong remote argv: %q", spec.Arguments)
		}
		if strings.Contains(strings.Join(spec.Arguments, " "), c.Launcher.Session) {
			t.Fatal("local session leaked into remote launch")
		}
		if !strings.Contains(strings.Join(spec.Arguments, " "), string(c.ID)) {
			t.Fatal("lost stable local app ID")
		}
	}
}
