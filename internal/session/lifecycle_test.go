package session

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"
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

func TestSetContextStateAtRecordsAndClearsArchiveTime(t *testing.T) {
	context := testValidContext(testContextID)
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{context}}
	local := time.Date(2026, 9, 2, 12, 34, 56, 123456789, time.FixedZone("CEST", 2*60*60))

	archived, err := SetContextStateAt(&registry, string(context.ID), ContextArchived, local)
	if err != nil {
		t.Fatalf("archive context: %v", err)
	}
	want := time.Date(2026, 9, 2, 10, 34, 56, 123456789, time.UTC)
	if archived.ArchivedAt == nil || !archived.ArchivedAt.Equal(want) || archived.ArchivedAt.Location() != time.UTC {
		t.Fatalf("unexpected archive timestamp: %+v", archived.ArchivedAt)
	}
	repeated, err := SetContextStateAt(&registry, string(context.ID), ContextArchived, local.Add(24*time.Hour))
	if err != nil || repeated.ArchivedAt == nil || !repeated.ArchivedAt.Equal(want) {
		t.Fatalf("repeated archive changed its original timestamp: context=%+v err=%v", repeated, err)
	}

	active, err := SetContextStateAt(&registry, string(context.ID), ContextActive, time.Time{})
	if err != nil {
		t.Fatalf("activate context: %v", err)
	}
	if active.ArchivedAt != nil {
		t.Fatalf("activation retained archive timestamp: %+v", active.ArchivedAt)
	}
}

func TestSetContextStateAtRejectsMissingArchiveTimeWithoutMutation(t *testing.T) {
	context := testValidContext(testContextID)
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{context}}
	if _, err := SetContextStateAt(&registry, string(context.ID), ContextArchived, time.Time{}); err == nil {
		t.Fatal("archive accepted a missing time")
	}
	if registry.Contexts[0].State != ContextActive || registry.Contexts[0].ArchivedAt != nil {
		t.Fatalf("failed archive mutated registry: %+v", registry.Contexts[0])
	}
}

func TestAddContextRejectsDuplicateTerminalIdentityWithoutMutation(t *testing.T) {
	first := testValidContext(testContextID)
	first.Launcher.Terminal.Identity = &TerminalIdentity{Kind: TerminalIdentityDefault}
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{first}}
	second := testValidContext(ContextID("6ba7b810-9dad-41d1-80b4-00c04fd430c8"))
	second.Launcher.Terminal.Identity = &TerminalIdentity{Kind: TerminalIdentityDefault}

	if err := AddContext(&registry, second); err == nil {
		t.Fatal("duplicate terminal identity was accepted")
	}
	if len(registry.Contexts) != 1 || registry.Contexts[0].ID != first.ID {
		t.Fatalf("failed add mutated registry: %+v", registry.Contexts)
	}
}

func TestAddContextAllowsInventoryToGrowBeyondLegacyLimit(t *testing.T) {
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{}}
	for index := range 300 {
		context := Context{
			ID:    ContextID(fmt.Sprintf("00000000-0000-4000-8000-%012x", index)),
			State: ContextArchived,
			Launcher: Launcher{
				Kind:     LauncherHerdr,
				Session:  fmt.Sprintf("stored-session-%d", index),
				Cwd:      "/tmp",
				Terminal: &TerminalLauncher{Adapter: TerminalAdapterAlacritty},
			},
		}
		if err := AddContext(&registry, context); err != nil {
			t.Fatalf("add stored context %d: %v", index, err)
		}
	}
	if got := len(registry.Contexts); got != 300 {
		t.Fatalf("stored context count = %d, want 300", got)
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
		Launcher: Launcher{
			Kind:     LauncherHerdr,
			Session:  "test-" + string(id[:8]),
			Cwd:      "/work",
			Terminal: &TerminalLauncher{Adapter: TerminalAdapterAlacritty},
		},
	}
}
