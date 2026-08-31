package session

import (
	"bytes"
	"errors"
	"testing"
)

func TestNewContextIDSetsRFC4122VersionAndVariant(t *testing.T) {
	id, err := newContextIDFrom(bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if id != "00000000-0000-4000-8000-000000000000" {
		t.Fatalf("unexpected generated UUID %q", id)
	}
	if err := id.Validate(); err != nil {
		t.Fatalf("generated invalid UUID: %v", err)
	}
}

func TestResolveContextRequiresUnambiguousExactSelector(t *testing.T) {
	secondID := ContextID("6ba7b810-9dad-41d1-80b4-00c04fd430c8")
	registry := registryWithContexts(testContextID, secondID)
	registry.Contexts[0].Label = "duplicate"
	registry.Contexts[1].Label = "duplicate"

	index, err := ResolveContext(registry, string(secondID))
	if err != nil || index != 1 {
		t.Fatalf("exact ID selection failed: index=%d err=%v", index, err)
	}
	if _, err := ResolveContext(registry, "duplicate"); !errors.Is(err, ErrContextAmbiguous) {
		t.Fatalf("expected ambiguous label, got %v", err)
	}
	if _, err := ResolveContext(registry, "missing"); !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("expected missing selector, got %v", err)
	}
}

func TestLifecycleMutationsValidateStateAndLauncherIdentity(t *testing.T) {
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{}}
	first := testValidContext(testContextID)
	if err := AddContext(&registry, first); err != nil {
		t.Fatalf("add context: %v", err)
	}
	duplicateLauncher := testValidContext(ContextID("6ba7b810-9dad-41d1-80b4-00c04fd430c8"))
	duplicateLauncher.Launcher = first.Launcher
	if err := AddContext(&registry, duplicateLauncher); err == nil {
		t.Fatal("expected duplicate launcher identity rejection")
	}
	changed, err := SetContextState(&registry, string(first.ID), ContextArchived)
	if err != nil || changed.State != ContextArchived {
		t.Fatalf("archive context: changed=%+v err=%v", changed, err)
	}
	removed, err := RemoveContext(&registry, string(first.ID))
	if err != nil || removed.ID != first.ID || len(registry.Contexts) != 0 {
		t.Fatalf("remove context: removed=%+v registry=%+v err=%v", removed, registry, err)
	}
}

func TestAddContextRejectsOverlappingApplicationIdentityWithoutMutation(t *testing.T) {
	first := Context{
		ID:    testContextID,
		State: ContextActive,
		Launcher: Launcher{
			Kind:          LauncherDesktop,
			DesktopID:     "first.desktop",
			DesktopOrigin: DesktopEntrySystem,
			DesktopPath:   "/usr/share/applications/first.desktop",
		},
		App: &Application{
			Identity: ApplicationIdentity{
				Protocol:     WindowWayland,
				WaylandAppID: "shared.app",
			},
			RestorePolicy: ApplicationRestoreFollow,
		},
	}
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{first}}
	second := first
	second.ID = ContextID("6ba7b810-9dad-41d1-80b4-00c04fd430c8")
	second.Launcher.DesktopID = "second.desktop"
	second.Launcher.DesktopPath = "/usr/share/applications/second.desktop"
	secondApp := *first.App
	secondApp.Identity.StartupWMClass = "ResolverHintDoesNotDisambiguate"
	second.App = &secondApp

	if err := AddContext(&registry, second); err == nil {
		t.Fatal("expected overlapping application identity rejection")
	}
	if len(registry.Contexts) != 1 || registry.Contexts[0].ID != first.ID {
		t.Fatalf("failed AddContext mutated registry: %+v", registry.Contexts)
	}
}

func testValidContext(id ContextID) Context {
	return Context{
		ID: id, State: ContextActive,
		Launcher: Launcher{Kind: LauncherHerdr, Session: "test-" + string(id[:8]), Cwd: "/work"},
	}
}
