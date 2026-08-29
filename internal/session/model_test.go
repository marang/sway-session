package session

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRegistryValidatesTypedHerdrContext(t *testing.T) {
	registry := validRegistry()
	if err := registry.Validate(); err != nil {
		t.Fatalf("expected valid registry: %v", err)
	}
}

func TestRegistryJSONSchema(t *testing.T) {
	encoded, err := json.Marshal(validRegistry())
	if err != nil {
		t.Fatalf("encode registry: %v", err)
	}
	want := `{"version":1,"contexts":[{"id":"123e4567-e89b-12d3-a456-426614174000","label":"LAB-80","provider":"linear","state":"active","launcher":{"kind":"herdr","session":"lab-80","cwd":"/home/example/work"}}]}`
	if string(encoded) != want {
		t.Fatalf("unexpected registry schema:\n got: %s\nwant: %s", encoded, want)
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
	if versionError.Got != 2 || versionError.Want != ContextsSchemaVersion {
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
		Version: ContextsSchemaVersion,
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
				},
			},
		},
	}
}

func contextIDPointer(id ContextID) *ContextID {
	return &id
}
