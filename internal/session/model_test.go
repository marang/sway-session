package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRegistryValidatesTypedHerdrContext(t *testing.T) {
	registry := validRegistry()
	if err := registry.Validate(); err != nil {
		t.Fatalf("expected valid registry: %v", err)
	}
}

func TestHerdrTerminalConfigurationIsTypedAndRequired(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Context)
	}{
		{name: "missing terminal", mutate: func(context *Context) { context.Launcher.Terminal = nil }},
		{name: "unsupported adapter", mutate: func(context *Context) { context.Launcher.Terminal.Adapter = "shell" }},
		{name: "default with project", mutate: func(context *Context) {
			context.Launcher.Terminal.Identity = &TerminalIdentity{Kind: TerminalIdentityDefault, Project: "unexpected"}
		}},
		{name: "project without name", mutate: func(context *Context) {
			context.Launcher.Terminal.Identity = &TerminalIdentity{Kind: TerminalIdentityProject}
		}},
		{name: "project with unstable name", mutate: func(context *Context) {
			context.Launcher.Terminal.Identity = &TerminalIdentity{Kind: TerminalIdentityProject, Project: "two words"}
		}},
		{name: "instance with reusable identity", mutate: func(context *Context) {
			context.Launcher.Terminal.Identity = &TerminalIdentity{Kind: TerminalIdentityDefault}
			context.Launcher.Terminal.Instance = true
		}},
		{name: "instance with unreserved provider", mutate: func(context *Context) {
			context.Launcher.Terminal.Instance = true
		}},
		{name: "instance with unrelated session", mutate: func(context *Context) {
			context.Provider = TerminalContextProvider
			context.Launcher.Terminal.Instance = true
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			context := validRegistry().Contexts[0]
			terminal := *context.Launcher.Terminal
			context.Launcher.Terminal = &terminal
			test.mutate(&context)
			if err := context.Validate(); err == nil {
				t.Fatal("expected invalid terminal configuration")
			}
		})
	}

	for _, identity := range []*TerminalIdentity{
		nil,
		{Kind: TerminalIdentityDefault},
		{Kind: TerminalIdentityProject, Project: "project-1.core"},
	} {
		context := validRegistry().Contexts[0]
		terminal := *context.Launcher.Terminal
		terminal.Identity = identity
		context.Launcher.Terminal = &terminal
		if err := context.Validate(); err != nil {
			t.Errorf("expected terminal identity %+v to validate: %v", identity, err)
		}
	}
}

func TestDesktopLaunchersRejectTerminalConfiguration(t *testing.T) {
	context := desktopApplicationContext("example.desktop", "example.app")
	context.Launcher.Terminal = &TerminalLauncher{Adapter: TerminalAdapterAlacritty}
	if err := context.Validate(); err == nil {
		t.Fatal("desktop launcher accepted terminal configuration")
	}
}

func TestRegistryRejectsDuplicateTerminalIdentity(t *testing.T) {
	first := validRegistry().Contexts[0]
	first.Launcher.Terminal.Identity = &TerminalIdentity{Kind: TerminalIdentityProject, Project: "shared"}
	second := first
	second.ID = ContextID("6ba7b810-9dad-41d1-80b4-00c04fd430c8")
	second.Launcher.Session = "different-session"
	secondTerminal := *first.Launcher.Terminal
	secondIdentity := *first.Launcher.Terminal.Identity
	secondTerminal.Identity = &secondIdentity
	second.Launcher.Terminal = &secondTerminal

	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{first, second}}
	if err := registry.Validate(); err == nil || !strings.Contains(err.Error(), "terminal identity") {
		t.Fatalf("expected duplicate terminal identity rejection, got %v", err)
	}
}

func TestContextArchivedAtIsOptionalCanonicalUTC(t *testing.T) {
	context := validRegistry().Contexts[0]
	context.State = ContextArchived
	if err := context.Validate(); err != nil {
		t.Fatalf("legacy archived context without timestamp was rejected: %v", err)
	}

	archivedAt := time.Date(2026, 9, 2, 10, 34, 56, 123456789, time.UTC)
	context.ArchivedAt = &archivedAt
	if err := context.Validate(); err != nil {
		t.Fatalf("UTC archived timestamp was rejected: %v", err)
	}
	encoded, err := json.Marshal(context)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"archived_at":"2026-09-02T10:34:56.123456789Z"`) {
		t.Fatalf("timestamp is not canonical UTC JSON: %s", encoded)
	}

	nonUTC := archivedAt.In(time.FixedZone("test", 60*60))
	context.ArchivedAt = &nonUTC
	if err := context.Validate(); err == nil {
		t.Fatal("non-UTC archived timestamp passed validation")
	}

	context.State = ContextActive
	context.ArchivedAt = &archivedAt
	if err := context.Validate(); err == nil {
		t.Fatal("active context retained archived timestamp")
	}
}

func TestRegistryJSONSchema(t *testing.T) {
	encoded, err := json.Marshal(validRegistry())
	if err != nil {
		t.Fatalf("encode registry: %v", err)
	}
	want := `{"version":5,"preferences":{"desktop_indicators":false},"contexts":[{"id":"123e4567-e89b-12d3-a456-426614174000","label":"LAB-80","provider":"linear","state":"active","launcher":{"kind":"herdr","session":"lab-80","cwd":"/home/example/work","terminal":{"adapter":"alacritty"}}}]}`
	if string(encoded) != want {
		t.Fatalf("unexpected registry schema:\n got: %s\nwant: %s", encoded, want)
	}
}

func TestDesktopApplicationJSONSchema(t *testing.T) {
	registry := Registry{
		Version:     ContextsSchemaVersion,
		Preferences: RegistryPreferences{DesktopIndicators: true},
		Contexts: []Context{{
			ID:    ContextID("6ba7b811-9dad-41d1-80b4-00c04fd430c8"),
			Label: "Slack",
			State: ContextActive,
			Launcher: Launcher{
				Kind:                LauncherFlatpak,
				FlatpakID:           "com.slack.Slack",
				FlatpakInstallation: FlatpakUser,
			},
			App: &Application{
				Identity: ApplicationIdentity{
					Protocol:     WindowXWayland,
					X11Class:     "Slack",
					X11Instance:  "slack",
					SandboxAppID: "com.slack.Slack",
				},
				DesiredOpen:   true,
				RestorePolicy: ApplicationRestorePinned,
			},
		}},
	}
	encoded, err := json.Marshal(registry)
	if err != nil {
		t.Fatalf("encode desktop registry: %v", err)
	}
	want := `{"version":5,"preferences":{"desktop_indicators":true},"contexts":[{"id":"6ba7b811-9dad-41d1-80b4-00c04fd430c8","label":"Slack","state":"active","launcher":{"kind":"flatpak","flatpak_id":"com.slack.Slack","flatpak_installation":"user"},"app":{"identity":{"protocol":"xwayland","x11_class":"Slack","x11_instance":"slack","sandbox_app_id":"com.slack.Slack"},"desired_open":true,"restore_policy":"pinned"}}]}`
	if string(encoded) != want {
		t.Fatalf("unexpected desktop registry schema:\n got: %s\nwant: %s", encoded, want)
	}
}

func TestRegistryRejectsUnsupportedVersion(t *testing.T) {
	registry := validRegistry()
	registry.Version = ContextsSchemaVersion + 1

	err := registry.Validate()
	var versionError *UnsupportedVersionError
	if !errors.As(err, &versionError) {
		t.Fatalf("expected UnsupportedVersionError, got %v", err)
	}
	if versionError.Got != ContextsSchemaVersion+1 || versionError.Want != ContextsSchemaVersion {
		t.Fatalf("unexpected version error: %+v", versionError)
	}
}

func TestRegistryBoundsContextCount(t *testing.T) {
	registry := Registry{Version: ContextsSchemaVersion, Contexts: make([]Context, MaxContexts+1)}
	if err := registry.Validate(); err == nil {
		t.Fatal("expected oversized context registry rejection")
	}
}

func TestRegistryRejectsDuplicateIdentityAndUntypedLaunchData(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Registry)
	}{
		{
			name: "duplicate identity",
			mutate: func(registry *Registry) {
				registry.Contexts = append(registry.Contexts, registry.Contexts[0])
			},
		},
		{
			name: "duplicate launcher identity",
			mutate: func(registry *Registry) {
				duplicate := registry.Contexts[0]
				duplicate.ID = ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
				duplicate.Label = "LAB-81"
				registry.Contexts = append(registry.Contexts, duplicate)
			},
		},
		{
			name: "unknown launcher",
			mutate: func(registry *Registry) {
				registry.Contexts[0].Launcher.Kind = "shell"
			},
		},
		{
			name: "command-like session",
			mutate: func(registry *Registry) {
				registry.Contexts[0].Launcher.Session = "lab-80; rm"
			},
		},
		{
			name: "option-like session",
			mutate: func(registry *Registry) {
				registry.Contexts[0].Launcher.Session = "--help"
			},
		},
		{
			name: "reserved default session",
			mutate: func(registry *Registry) {
				registry.Contexts[0].Launcher.Session = "default"
			},
		},
		{
			name: "session exceeds Herdr byte limit",
			mutate: func(registry *Registry) {
				registry.Contexts[0].Launcher.Session = "a" + strings.Repeat("b", 64)
			},
		},
		{
			name: "relative cwd",
			mutate: func(registry *Registry) {
				registry.Contexts[0].Launcher.Cwd = "work/project"
			},
		},
		{
			name: "unknown state",
			mutate: func(registry *Registry) {
				registry.Contexts[0].State = "closed"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := validRegistry()
			test.mutate(&registry)
			if err := registry.Validate(); err == nil {
				t.Fatal("expected registry to be rejected")
			}
		})
	}
}

func TestRegistryValidatesTypedDesktopAndFlatpakApplications(t *testing.T) {
	desktop := Context{
		ID:    ContextID("6ba7b810-9dad-41d1-80b4-00c04fd430c8"),
		Label: "Google Chrome",
		State: ContextActive,
		Launcher: Launcher{
			Kind:          LauncherDesktop,
			DesktopID:     "google-chrome.desktop",
			DesktopOrigin: DesktopEntrySystem,
			DesktopPath:   "/usr/share/applications/google-chrome.desktop",
		},
		App: &Application{
			Identity: ApplicationIdentity{
				Protocol:       WindowWayland,
				WaylandAppID:   "google-chrome",
				StartupWMClass: "Google-chrome",
			},
			DesiredOpen:   true,
			RestorePolicy: ApplicationRestoreFollow,
		},
	}
	flatpak := Context{
		ID:    ContextID("6ba7b811-9dad-41d1-80b4-00c04fd430c8"),
		Label: "Slack",
		State: ContextActive,
		Launcher: Launcher{
			Kind:                LauncherFlatpak,
			FlatpakID:           "com.slack.Slack",
			FlatpakInstallation: FlatpakUser,
		},
		App: &Application{
			Identity: ApplicationIdentity{
				Protocol:     WindowXWayland,
				X11Class:     "Slack",
				X11Instance:  "slack",
				SandboxAppID: "com.slack.Slack",
			},
			DesiredOpen:   true,
			RestorePolicy: ApplicationRestorePinned,
		},
	}
	digest := strings.Repeat("a", 64)
	local := Context{
		ID:    ContextID("6ba7b812-9dad-41d1-80b4-00c04fd430c8"),
		Label: "Local App",
		State: ContextActive,
		Launcher: Launcher{
			Kind:                     LauncherDesktop,
			DesktopID:                "local.example.desktop",
			DesktopOrigin:            DesktopEntryUser,
			DesktopPath:              "/home/example/.local/share/applications/local.example.desktop",
			DesktopEntrySHA256:       digest,
			ApprovedDesktopPath:      "/home/example/.local/state/sway-session/desktop-approvals/local.example.desktop",
			ApprovedExecutablePath:   "/home/example/.local/bin/example",
			ApprovedExecutableSHA256: digest,
		},
		App: &Application{
			Identity:      ApplicationIdentity{Protocol: WindowWayland, WaylandAppID: "local.example"},
			RestorePolicy: ApplicationRestoreFollow,
		},
	}
	xwayland := Context{
		ID:    ContextID("6ba7b813-9dad-41d1-80b4-00c04fd430c8"),
		Label: "Legacy X11 App",
		State: ContextActive,
		Launcher: Launcher{
			Kind:          LauncherDesktop,
			DesktopID:     "legacy-x11.desktop",
			DesktopOrigin: DesktopEntrySystem,
			DesktopPath:   "/usr/share/applications/legacy-x11.desktop",
		},
		App: &Application{
			Identity: ApplicationIdentity{
				Protocol:    WindowXWayland,
				X11Class:    "LegacyApp",
				X11Instance: "legacy-app",
			},
			RestorePolicy: ApplicationRestoreFollow,
		},
	}
	registry := Registry{
		Version:     ContextsSchemaVersion,
		Preferences: RegistryPreferences{DesktopIndicators: true},
		Contexts:    []Context{validRegistry().Contexts[0], desktop, flatpak, local, xwayland},
	}
	if err := registry.Validate(); err != nil {
		t.Fatalf("expected mixed typed registry: %v", err)
	}
}

func TestRegistryRejectsDuplicateTypedLauncherIdentities(t *testing.T) {
	tests := []struct {
		name   string
		first  Context
		second Context
	}{
		{
			name:   "desktop ID across origins",
			first:  desktopApplicationContext("shared.desktop", "first.app"),
			second: desktopApplicationContext("shared.desktop", "second.app"),
		},
		{
			name:   "Flatpak ID across installations",
			first:  flatpakApplicationContext("org.example.App", "first.app"),
			second: flatpakApplicationContext("org.example.App", "second.app"),
		},
	}
	tests[0].second.Launcher.DesktopOrigin = DesktopEntryUser
	tests[0].second.Launcher.DesktopPath = "/home/example/.local/share/applications/shared.desktop"
	tests[0].second.Launcher.DesktopEntrySHA256 = strings.Repeat("a", 64)
	tests[0].second.Launcher.ApprovedDesktopPath = "/home/example/.local/state/sway-session/desktop-approvals/shared.desktop"
	tests[1].first.Launcher.FlatpakInstallation = FlatpakSystem

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.first.ID = ContextID(fmt.Sprintf("6ba7b83%d-9dad-41d1-80b4-00c04fd430c8", index))
			test.second.ID = ContextID(fmt.Sprintf("6ba7b84%d-9dad-41d1-80b4-00c04fd430c8", index))
			registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{test.first, test.second}}
			if err := registry.Validate(); err == nil || !strings.Contains(err.Error(), "launcher") {
				t.Fatalf("expected duplicate typed launcher rejection, got %v", err)
			}
		})
	}
}

func TestRegistryRejectsInvalidDesktopApplicationUnion(t *testing.T) {
	digest := strings.Repeat("a", 64)
	base := Context{
		ID:    ContextID("6ba7b810-9dad-41d1-80b4-00c04fd430c8"),
		State: ContextActive,
		Launcher: Launcher{
			Kind:                     LauncherDesktop,
			DesktopID:                "local.example.desktop",
			DesktopOrigin:            DesktopEntryUser,
			DesktopPath:              "/home/example/.local/share/applications/local.example.desktop",
			DesktopEntrySHA256:       digest,
			ApprovedDesktopPath:      "/home/example/.local/state/sway-session/desktop-approvals/local.example.desktop",
			ApprovedExecutablePath:   "/home/example/.local/bin/example",
			ApprovedExecutableSHA256: digest,
		},
		App: &Application{
			Identity:      ApplicationIdentity{Protocol: WindowWayland, WaylandAppID: "local.example"},
			RestorePolicy: ApplicationRestoreFollow,
		},
	}
	tests := []struct {
		name   string
		mutate func(*Context)
	}{
		{name: "missing app state", mutate: func(context *Context) { context.App = nil }},
		{name: "Herdr fields in desktop launcher", mutate: func(context *Context) { context.Launcher.Session = "bad" }},
		{name: "path-like desktop ID", mutate: func(context *Context) { context.Launcher.DesktopID = "sub/app.desktop" }},
		{name: "relative desktop path", mutate: func(context *Context) { context.Launcher.DesktopPath = "app.desktop" }},
		{name: "uppercase checksum", mutate: func(context *Context) { context.Launcher.DesktopEntrySHA256 = strings.Repeat("A", 64) }},
		{name: "unpaired executable approval", mutate: func(context *Context) { context.Launcher.ApprovedExecutableSHA256 = "" }},
		{name: "mixed Wayland and X11", mutate: func(context *Context) { context.App.Identity.X11Class = "App" }},
		{name: "Flatpak sandbox on desktop launcher", mutate: func(context *Context) { context.App.Identity.SandboxAppID = "org.example.App" }},
		{name: "missing restore policy", mutate: func(context *Context) { context.App.RestorePolicy = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			application := *base.App
			candidate.App = &application
			test.mutate(&candidate)
			registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{candidate}}
			if err := registry.Validate(); err == nil {
				t.Fatal("expected invalid desktop application registry")
			}
		})
	}
}

func TestPinnedApplicationRequiresDesiredOpen(t *testing.T) {
	context := flatpakApplicationContext("org.example.App", "org.example.App")
	context.ID = testContextID
	context.App.RestorePolicy = ApplicationRestorePinned
	context.App.DesiredOpen = false
	if err := context.Validate(); err == nil {
		t.Fatal("pinned desired-closed application passed validation")
	}
}

func TestFlatpakApplicationIDValidation(t *testing.T) {
	for _, valid := range []string{"org.example.App", "io.github.example.my_app", "org.example.My-App"} {
		if !validFlatpakID(valid) {
			t.Errorf("expected valid Flatpak ID %q", valid)
		}
	}
	for _, invalid := range []string{"org.example", "Org.example.App", "org.example-site.App", "org.example.9App"} {
		if validFlatpakID(invalid) {
			t.Errorf("expected invalid Flatpak ID %q", invalid)
		}
	}
}

func TestRegistryRejectsDuplicateApplicationIdentity(t *testing.T) {
	tests := []struct {
		name   string
		first  Context
		second Context
		valid  bool
	}{
		{name: "exact desktop identity", first: desktopApplicationContext("one.desktop", "shared.app"), second: desktopApplicationContext("two.desktop", "shared.app")},
		{name: "StartupWMClass is only a resolver hint", first: desktopApplicationContext("one.desktop", "shared.app"), second: withStartupWMClass(desktopApplicationContext("two.desktop", "shared.app"), "DifferentHint")},
		{name: "desktop identity overlaps a sandbox-specific Flatpak identity", first: desktopApplicationContext("one.desktop", "shared.app"), second: flatpakApplicationContext("org.example.App", "shared.app")},
		{name: "different explicit sandbox identities disambiguate", first: flatpakApplicationContext("org.example.App", "shared.app"), second: flatpakApplicationContext("org.example.Other", "shared.app"), valid: true},
		{name: "different primary identity is distinct", first: desktopApplicationContext("one.desktop", "shared.app"), second: desktopApplicationContext("two.desktop", "other.app"), valid: true},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first := test.first
			second := test.second
			first.ID = ContextID(fmt.Sprintf("6ba7b81%d-9dad-41d1-80b4-00c04fd430c8", index))
			second.ID = ContextID(fmt.Sprintf("6ba7b82%d-9dad-41d1-80b4-00c04fd430c8", index))
			registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{first, second}}
			err := registry.Validate()
			if test.valid && err != nil {
				t.Fatalf("expected distinct application identities: %v", err)
			}
			if !test.valid && (err == nil || !strings.Contains(err.Error(), "application identity")) {
				t.Fatalf("expected overlapping application identity rejection, got %v", err)
			}
		})
	}
}

func desktopApplicationContext(id string, waylandAppID string) Context {
	return Context{
		State: ContextActive,
		Launcher: Launcher{Kind: LauncherDesktop, DesktopID: id, DesktopOrigin: DesktopEntrySystem,
			DesktopPath: "/usr/share/applications/" + id},
		App: &Application{Identity: ApplicationIdentity{Protocol: WindowWayland, WaylandAppID: waylandAppID}, RestorePolicy: ApplicationRestoreFollow},
	}
}

func flatpakApplicationContext(id string, waylandAppID string) Context {
	return Context{
		State:    ContextActive,
		Launcher: Launcher{Kind: LauncherFlatpak, FlatpakID: id, FlatpakInstallation: FlatpakUser},
		App:      &Application{Identity: ApplicationIdentity{Protocol: WindowWayland, WaylandAppID: waylandAppID, SandboxAppID: id}, RestorePolicy: ApplicationRestoreFollow},
	}
}

func withStartupWMClass(context Context, value string) Context {
	application := *context.App
	application.Identity.StartupWMClass = value
	context.App = &application
	return context
}

func TestLayoutSnapshotValidatesNestedAndFloatingState(t *testing.T) {
	secondID := ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	snapshot := LayoutSnapshot{
		Version: LayoutSchemaVersion,
		Workspaces: []WorkspaceLayout{
			{
				Name:        "2: work",
				RestoreMode: WorkspaceRestoreLayout,
				Tiling: &LayoutNode{
					Layout: LayoutSplitHorizontal,
					Children: []LayoutNode{
						{ContextID: contextIDPointer(testContextID), Proportion: 0.6},
						{
							Layout:     LayoutTabbed,
							Proportion: 0.4,
							Fullscreen: FullscreenWorkspace,
							Children: []LayoutNode{
								{ContextID: contextIDPointer(secondID), Proportion: 1},
							},
						},
					},
				},
				FocusedContext: contextIDPointer(secondID),
			},
			{
				Name:        "3",
				RestoreMode: WorkspaceRestoreLayout,
				Floating: []LayoutNode{
					{
						ContextID: contextIDPointer(ContextID("6ba7b811-9dad-11d1-80b4-00c04fd430c8")),
						Geometry:  &Geometry{X: 12, Y: 30, Width: 900, Height: 700},
					},
				},
			},
		},
	}

	if err := snapshot.Validate(); err != nil {
		t.Fatalf("expected valid layout snapshot: %v", err)
	}
}

func TestLayoutSnapshotValidatesNestedFloatingContainer(t *testing.T) {
	snapshot := LayoutSnapshot{
		Version: LayoutSchemaVersion,
		Workspaces: []WorkspaceLayout{
			{
				Name:        "4: floating group",
				RestoreMode: WorkspaceRestoreLayout,
				Floating: []LayoutNode{
					{
						Layout:   LayoutSplitVertical,
						Geometry: &Geometry{X: 20, Y: 40, Width: 800, Height: 600},
						Children: []LayoutNode{
							{ContextID: contextIDPointer(testContextID), Proportion: 0.5},
							{ContextID: contextIDPointer(ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")), Proportion: 0.5},
						},
					},
				},
			},
		},
	}

	if err := snapshot.Validate(); err != nil {
		t.Fatalf("expected nested floating container to be valid: %v", err)
	}
}

func TestLayoutSnapshotValidatesPlacementOnlyWorkspace(t *testing.T) {
	snapshot := LayoutSnapshot{
		Version: LayoutSchemaVersion,
		Workspaces: []WorkspaceLayout{
			{
				Name:              "2: mixed",
				RestoreMode:       WorkspaceRestorePlacementOnly,
				PlacementContexts: []ContextID{testContextID, ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")},
			},
		},
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("expected placement-only workspace to be valid: %v", err)
	}
}

func TestPlacementOnlyLayoutJSONSchema(t *testing.T) {
	snapshot := LayoutSnapshot{
		Version: LayoutSchemaVersion,
		Workspaces: []WorkspaceLayout{
			{
				Name:              "2: mixed",
				RestoreMode:       WorkspaceRestorePlacementOnly,
				PlacementContexts: []ContextID{testContextID},
			},
		},
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("encode placement-only layout: %v", err)
	}
	want := `{"version":1,"workspaces":[{"name":"2: mixed","restore_mode":"placement_only","placement_contexts":["123e4567-e89b-12d3-a456-426614174000"]}]}`
	if string(encoded) != want {
		t.Fatalf("unexpected placement-only schema:\n got: %s\nwant: %s", encoded, want)
	}
}

func TestLayoutSnapshotRejectsAmbiguousOrInvalidTrees(t *testing.T) {
	secondID := ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	tests := []struct {
		name      string
		workspace WorkspaceLayout
	}{
		{
			name:      "empty workspace",
			workspace: WorkspaceLayout{Name: "1", RestoreMode: WorkspaceRestoreLayout},
		},
		{
			name: "missing restore mode",
			workspace: WorkspaceLayout{Name: "1", Tiling: &LayoutNode{
				ContextID: contextIDPointer(testContextID),
			}},
		},
		{
			name: "placement-only workspace with layout state",
			workspace: WorkspaceLayout{
				Name:              "1",
				RestoreMode:       WorkspaceRestorePlacementOnly,
				PlacementContexts: []ContextID{testContextID},
				Tiling:            &LayoutNode{ContextID: contextIDPointer(testContextID)},
			},
		},
		{
			name: "placement-only workspace with duplicate context",
			workspace: WorkspaceLayout{
				Name:              "1",
				RestoreMode:       WorkspaceRestorePlacementOnly,
				PlacementContexts: []ContextID{testContextID, testContextID},
			},
		},
		{
			name: "layout workspace with placement contexts",
			workspace: WorkspaceLayout{
				Name:              "1",
				RestoreMode:       WorkspaceRestoreLayout,
				PlacementContexts: []ContextID{testContextID},
				Tiling:            &LayoutNode{ContextID: contextIDPointer(testContextID)},
			},
		},
		{
			name: "leaf with children",
			workspace: WorkspaceLayout{Name: "1", RestoreMode: WorkspaceRestoreLayout, Tiling: &LayoutNode{
				ContextID: contextIDPointer(testContextID),
				Children:  []LayoutNode{{ContextID: contextIDPointer(testContextID)}},
			}},
		},
		{
			name: "geometry on tiled leaf",
			workspace: WorkspaceLayout{Name: "1", RestoreMode: WorkspaceRestoreLayout, Tiling: &LayoutNode{
				ContextID: contextIDPointer(testContextID),
				Geometry:  &Geometry{Width: 10, Height: 10},
			}},
		},
		{
			name: "proportion above parent share",
			workspace: WorkspaceLayout{Name: "1", RestoreMode: WorkspaceRestoreLayout, Tiling: &LayoutNode{
				ContextID:  contextIDPointer(testContextID),
				Proportion: 1.01,
			}},
		},
		{
			name: "proportion on floating leaf",
			workspace: WorkspaceLayout{Name: "1", RestoreMode: WorkspaceRestoreLayout, Floating: []LayoutNode{
				{
					ContextID:  contextIDPointer(testContextID),
					Proportion: 0.5,
					Geometry:   &Geometry{Width: 10, Height: 10},
				},
			}},
		},
		{
			name: "floating leaf without geometry",
			workspace: WorkspaceLayout{Name: "1", RestoreMode: WorkspaceRestoreLayout, Floating: []LayoutNode{
				{ContextID: contextIDPointer(testContextID)},
			}},
		},
		{
			name: "floating parent without geometry",
			workspace: WorkspaceLayout{Name: "1", RestoreMode: WorkspaceRestoreLayout, Floating: []LayoutNode{
				{
					Layout:   LayoutSplitHorizontal,
					Children: []LayoutNode{{ContextID: contextIDPointer(testContextID)}},
				},
			}},
		},
		{
			name: "geometry on floating descendant",
			workspace: WorkspaceLayout{Name: "1", RestoreMode: WorkspaceRestoreLayout, Floating: []LayoutNode{
				{
					Layout:   LayoutSplitHorizontal,
					Geometry: &Geometry{Width: 20, Height: 20},
					Children: []LayoutNode{
						{ContextID: contextIDPointer(testContextID), Geometry: &Geometry{Width: 10, Height: 10}},
					},
				},
			}},
		},
		{
			name: "unknown focused context",
			workspace: WorkspaceLayout{
				Name:           "1",
				RestoreMode:    WorkspaceRestoreLayout,
				Tiling:         &LayoutNode{ContextID: contextIDPointer(testContextID)},
				FocusedContext: contextIDPointer(ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")),
			},
		},
		{
			name: "unknown fullscreen mode",
			workspace: WorkspaceLayout{Name: "1", RestoreMode: WorkspaceRestoreLayout, Tiling: &LayoutNode{
				ContextID:  contextIDPointer(testContextID),
				Fullscreen: "exclusive",
			}},
		},
		{
			name: "multiple fullscreen nodes in one workspace",
			workspace: WorkspaceLayout{
				Name:        "1",
				RestoreMode: WorkspaceRestoreLayout,
				Tiling: &LayoutNode{
					Layout: LayoutSplitHorizontal,
					Children: []LayoutNode{
						{ContextID: contextIDPointer(testContextID), Fullscreen: FullscreenWorkspace},
						{ContextID: contextIDPointer(secondID), Fullscreen: FullscreenGlobal},
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{test.workspace}}
			if err := snapshot.Validate(); err == nil {
				t.Fatal("expected layout snapshot to be rejected")
			}
		})
	}
}

func TestLayoutSnapshotRejectsMultipleGlobalFullscreenNodes(t *testing.T) {
	snapshot := LayoutSnapshot{
		Version: LayoutSchemaVersion,
		Workspaces: []WorkspaceLayout{
			{
				Name:        "1",
				RestoreMode: WorkspaceRestoreLayout,
				Tiling: &LayoutNode{
					ContextID:  contextIDPointer(testContextID),
					Fullscreen: FullscreenGlobal,
				},
			},
			{
				Name:        "2",
				RestoreMode: WorkspaceRestoreLayout,
				Tiling: &LayoutNode{
					ContextID:  contextIDPointer(ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")),
					Fullscreen: FullscreenGlobal,
				},
			},
		},
	}

	if err := snapshot.Validate(); err == nil {
		t.Fatal("expected multiple global fullscreen nodes to be rejected")
	}
}

func validRegistry() Registry {
	return Registry{
		Version:     ContextsSchemaVersion,
		Preferences: RegistryPreferences{},
		Contexts: []Context{
			{
				ID:       testContextID,
				Label:    "LAB-80",
				Provider: "linear",
				State:    ContextActive,
				Launcher: Launcher{
					Kind:    LauncherHerdr,
					Session: "lab-80",
					Cwd:     "/home/example/work",
					Terminal: &TerminalLauncher{
						Adapter: TerminalAdapterAlacritty,
					},
				},
			},
		},
	}
}

func contextIDPointer(id ContextID) *ContextID {
	return &id
}
