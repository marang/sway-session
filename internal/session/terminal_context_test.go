package session

import (
	"errors"
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
