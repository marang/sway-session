package session

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEnsureTerminalContextCreatesAndReusesTypedIdentity(t *testing.T) {
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{}}
	request := TerminalContextRequest{
		Identity: TerminalIdentity{Kind: TerminalIdentityProject, Project: "sway-title-animator"},
		Adapter:  TerminalAdapterFoot,
		Cwd:      "/work/sway-title-animator",
		Label:    "Project terminal",
	}

	created, wasCreated, err := EnsureTerminalContext(&registry, request, fixedTerminalContextID(testContextID))
	if err != nil || !wasCreated {
		t.Fatalf("create terminal context: context=%+v created=%t err=%v", created, wasCreated, err)
	}
	if created.Launcher.Session != "sway-terminal-project-14e5c8d81d5e834d3d5c2c06" ||
		created.Launcher.Terminal == nil || created.Launcher.Terminal.Adapter != TerminalAdapterFoot ||
		created.Launcher.Terminal.Identity == nil || *created.Launcher.Terminal.Identity != request.Identity {
		t.Fatalf("unexpected created context: %+v", created)
	}

	reused, wasCreated, err := EnsureTerminalContext(&registry, request, func() (ContextID, error) {
		return "", errors.New("must not generate an ID while reusing a context")
	})
	if err != nil || wasCreated || reused.ID != created.ID || len(registry.Contexts) != 1 {
		t.Fatalf("reuse terminal context: context=%+v created=%t registry=%+v err=%v", reused, wasCreated, registry, err)
	}
}

func TestCreateTerminalInstanceContextAlwaysCreatesUniqueWindowAndSession(t *testing.T) {
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{}}
	request := TerminalInstanceRequest{
		Adapter: TerminalAdapterAlacritty,
		Cwd:     "/work/new-terminal",
		Label:   "Terminal",
	}
	ids := []ContextID{
		testContextID,
		"6ba7b810-9dad-41d1-80b4-00c04fd430c8",
	}
	index := 0
	generate := func() (ContextID, error) {
		id := ids[index]
		index++
		return id, nil
	}

	first, err := CreateTerminalInstanceContext(&registry, request, generate)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CreateTerminalInstanceContext(&registry, request, generate)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.Launcher.Session == second.Launcher.Session || len(registry.Contexts) != 2 {
		t.Fatalf("fresh terminals were reused: first=%+v second=%+v registry=%+v", first, second, registry)
	}
	for _, created := range []Context{first, second} {
		if created.Provider != TerminalContextProvider || created.Launcher.Terminal == nil ||
			!created.Launcher.Terminal.Instance || created.Launcher.Terminal.Identity != nil ||
			created.State != ContextActive || !IsTerminalInstanceContext(created) {
			t.Fatalf("fresh terminal is not an independent typed context: %+v", created)
		}
		wantSession := "sway-terminal-" + strings.ReplaceAll(string(created.ID), "-", "")
		if created.Launcher.Session != wantSession {
			t.Fatalf("fresh terminal session=%q want=%q", created.Launcher.Session, wantSession)
		}
		clientSocket := filepath.Join("/home/example/.config/herdr", "sessions", created.Launcher.Session, "herdr-client.sock")
		if len(clientSocket) > 107 {
			t.Fatalf("fresh terminal client socket path has %d bytes, exceeds Linux limit: %q", len(clientSocket), clientSocket)
		}
	}
	spoofed := first
	spoofed.Launcher.Session = "manually-named"
	if IsTerminalInstanceContext(spoofed) {
		t.Fatal("provider metadata alone classified a manual context as a fresh terminal instance")
	}
	lookalike := first
	lookalike.Launcher.Terminal = &TerminalLauncher{Adapter: first.Launcher.Terminal.Adapter}
	if IsTerminalInstanceContext(lookalike) {
		t.Fatal("pre-v4 lookalike without explicit discriminator was classified as a fresh terminal instance")
	}
	legacy := first
	legacy.Launcher.Session = "sway-terminal-instance-" + string(first.ID)
	if !IsTerminalInstanceContext(legacy) {
		t.Fatal("v4 terminal instance became invalid after shortening new session names")
	}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("v4 terminal instance no longer validates: %v", err)
	}
}

func TestCreateTerminalInstanceContextRejectsGeneratedIDCollisionWithoutMutation(t *testing.T) {
	existing := testValidContext(testContextID)
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{existing}}
	before := registry

	_, err := CreateTerminalInstanceContext(&registry, TerminalInstanceRequest{
		Adapter: TerminalAdapterAlacritty,
		Cwd:     "/work/new-terminal",
	}, fixedTerminalContextID(testContextID))
	if err == nil || !reflect.DeepEqual(registry, before) {
		t.Fatalf("generated ID collision mutated registry: registry=%+v err=%v", registry, err)
	}
}

func TestEnsureTerminalContextRejectsConflictingExplicitCwdAndSessionCollision(t *testing.T) {
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{}}
	request := TerminalContextRequest{
		Identity:    TerminalIdentity{Kind: TerminalIdentityDefault},
		Adapter:     TerminalAdapterAlacritty,
		Cwd:         "/home/example",
		CwdExplicit: true,
	}
	created, _, err := EnsureTerminalContext(&registry, request, fixedTerminalContextID(testContextID))
	if err != nil {
		t.Fatal(err)
	}
	request.Cwd = "/tmp/other"
	if _, _, err := EnsureTerminalContext(&registry, request, fixedTerminalContextID(testContextID)); !errors.Is(err, ErrTerminalIdentityConflict) {
		t.Fatalf("conflicting cwd was accepted: %v", err)
	}

	registry = Registry{Version: ContextsSchemaVersion, Contexts: []Context{{
		ID:    testContextID,
		State: ContextActive,
		Launcher: Launcher{
			Kind: LauncherHerdr, Session: created.Launcher.Session, Cwd: "/work",
			Terminal: &TerminalLauncher{Adapter: TerminalAdapterAlacritty},
		},
	}}}
	if _, _, err := EnsureTerminalContext(&registry, TerminalContextRequest{
		Identity: TerminalIdentity{Kind: TerminalIdentityDefault}, Adapter: TerminalAdapterAlacritty, Cwd: "/home/example",
	}, fixedTerminalContextID("6ba7b810-9dad-41d1-80b4-00c04fd430c8")); !errors.Is(err, ErrTerminalSessionCollision) {
		t.Fatalf("manual session collision was adopted: %v", err)
	}
}

func TestEnsureTerminalContextRequiresExplicitActivationOfArchivedIdentity(t *testing.T) {
	identity := TerminalIdentity{Kind: TerminalIdentityDefault}
	archivedAt := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	existing := testValidContext(testContextID)
	existing.State = ContextArchived
	existing.ArchivedAt = &archivedAt
	existing.Launcher.Terminal.Identity = &identity
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{existing}}

	_, _, err := EnsureTerminalContext(&registry, TerminalContextRequest{
		Identity: identity, Adapter: TerminalAdapterAlacritty, Cwd: existing.Launcher.Cwd,
	}, func() (ContextID, error) {
		t.Fatal("archived identity must not generate a replacement context")
		return "", nil
	})
	if !errors.Is(err, ErrTerminalIdentityArchived) || !strings.Contains(err.Error(), string(existing.ID)) {
		t.Fatalf("archived identity did not return actionable activation guidance: %v", err)
	}
}

func TestTerminalAdapterChangeRequiresArchivedNonDestructiveReconfigure(t *testing.T) {
	identity := TerminalIdentity{Kind: TerminalIdentityProject, Project: "LAB-105"}
	existing := testValidContext(testContextID)
	existing.Launcher.Terminal.Identity = &identity
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{existing}}

	_, _, err := EnsureTerminalContext(&registry, TerminalContextRequest{
		Identity: identity, Adapter: TerminalAdapterFoot, Cwd: existing.Launcher.Cwd,
	}, fixedTerminalContextID("22222222-2222-4222-8222-222222222222"))
	if !errors.Is(err, ErrTerminalAdapterConflict) {
		t.Fatalf("adapter mismatch was silently ignored: %v", err)
	}
	if _, _, err := reconfigureTerminalAdapter(&registry, identity, TerminalAdapterFoot); !errors.Is(err, ErrTerminalAdapterActive) {
		t.Fatalf("active adapter was changed without archive: %v", err)
	}
	archivedAt := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	registry.Contexts[0].State = ContextArchived
	registry.Contexts[0].ArchivedAt = &archivedAt
	changed, reconfigured, err := reconfigureTerminalAdapter(&registry, identity, TerminalAdapterFoot)
	if err != nil || !reconfigured || changed.Launcher.Terminal.Adapter != TerminalAdapterFoot {
		t.Fatalf("archived adapter reconfigure: context=%+v changed=%t err=%v", changed, reconfigured, err)
	}
	if changed.ID != existing.ID || changed.Launcher.Session != existing.Launcher.Session || changed.Launcher.Cwd != existing.Launcher.Cwd || changed.ArchivedAt == nil {
		t.Fatalf("adapter reconfigure changed persistent identity or history metadata: before=%+v after=%+v", existing, changed)
	}
	unchanged, reconfigured, err := reconfigureTerminalAdapter(&registry, identity, TerminalAdapterFoot)
	if err != nil || reconfigured || !reflect.DeepEqual(unchanged, changed) {
		t.Fatalf("repeated adapter reconfigure was not idempotent: context=%+v changed=%t err=%v", unchanged, reconfigured, err)
	}
}

func fixedTerminalContextID(id ContextID) func() (ContextID, error) {
	return func() (ContextID, error) { return id, nil }
}

func TestTerminalProjectIdentityAndSessionNameAreBoundedAgentIdentifiers(t *testing.T) {
	identity, err := ParseTerminalIdentity("LAB-105.project_1")
	if err != nil || identity != (TerminalIdentity{Kind: TerminalIdentityProject, Project: "LAB-105.project_1"}) {
		t.Fatalf("parse project identity: %+v err=%v", identity, err)
	}
	if _, err := ParseTerminalIdentity("bad project; rm"); err == nil {
		t.Fatal("unsafe project identity was accepted")
	}
	name, err := DeriveTerminalSessionName(TerminalIdentity{Kind: TerminalIdentityDefault})
	if err != nil || name != "sway-terminal-default" {
		t.Fatalf("unexpected default session %q: %v", name, err)
	}
}
