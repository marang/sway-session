package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestApplicationInventoryKeepsPolicyPresenceAndProofsIndependent(t *testing.T) {
	app := applicationInventoryTestRegistry("Editor").Contexts[0]
	app.App.RestorePolicy = ApplicationRestorePinned
	app.App.DesiredOpen = true
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	record := RestoreOutcome{ContextID: app.ID, Source: "automatic", StartedAt: at, UpdatedAt: at, Status: "pending", Reason: "window_mapped", Requested: RestoreWork{Window: true, Placement: true, Layout: true}, LaunchAccepted: true, WindowMapped: true, IdentityDigest: RestoreContextDigest(app)}
	explicit := record
	explicit.Source = "explicit"
	explicit.Status = "failed"
	explicit.Reason = "layout_timeout"
	snapshot := ApplicationInventorySnapshot{Registry: Registry{Version: ContextsSchemaVersion, Contexts: []Context{app}}, Layout: &LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{{Name: "98", RestoreMode: WorkspaceRestorePlacementOnly, PlacementContexts: []ContextID{app.ID}}}}, RestoreReport: RestoreReport{Outcomes: []RestoreOutcome{explicit, record}}, ApplicationSession: &ApplicationSessionState{Attempts: []ApplicationLaunchAttempt{{ContextID: app.ID, StartedAt: at}}}}
	items := BuildApplicationInventory(snapshot)
	item := items[0]
	if !item.RestorePolicy.Eligible || item.RestorePolicy.Reason != "desired_open_pinned" || item.Presence.State != "unknown" || item.Presence.WindowCount != nil {
		t.Fatalf("invented observation or changed policy: %+v", item)
	}
	if item.RestoreOutcome.Outcome.Source != "explicit" || item.RestoreOutcome.Outcome.Reason != "layout_timeout" || len(item.PendingRestore) != 1 || item.PendingRestore[0].Stage != "window_mapped" || item.PendingRestore[0].Outcome.Status != "pending" {
		t.Fatalf("lost source or inferred completion: %+v", item)
	}
	if item.SavedPlacement.Reason != "saved_placement_only" || item.LaunchGuard == nil || item.LaunchGuard.CompositorMatches != nil {
		t.Fatalf("missing degradation or invented guard lifetime: %+v", item)
	}
	app.App.DesiredOpen = false
	snapshot.Registry.Contexts[0] = app
	closed := BuildApplicationInventory(snapshot)[0]
	closed.Presence = ApplicationGroupPresence(ApplicationGroup{}, at)
	if closed.RestorePolicy.Eligible || closed.RestorePolicy.Reason != "desired_closed" || closed.Presence.State != "absent" || *closed.Presence.WindowCount != 0 {
		t.Fatalf("closed and observed absent collapsed: %+v", closed)
	}
	// Desired-open changes do not change the launch identity; identity changes do.
	snapshot.Registry.Contexts[0].App.Identity.WaylandAppID = "org.example.Other"
	changed := BuildApplicationInventory(snapshot)[0]
	if changed.RestoreOutcome.Identity != "changed" || changed.PendingRestore[0].Identity != "changed" || changed.PendingRestore[0].Outcome.Status != "pending" {
		t.Fatalf("historical outcome rewritten: %+v", changed)
	}
	if snapshot.Registry.Contexts[0].App.DesiredOpen {
		t.Fatal("projection mutated registry")
	}
}

func TestApplicationInventoryPresenceNeverGuessesAmbiguousAnchor(t *testing.T) {
	at := time.Now().UTC()
	group := ApplicationGroup{Windows: []WindowApplication{{ContainerID: 42}, {ContainerID: 43}}, Ambiguous: true, Anchor: &WindowApplication{ContainerID: 42}}
	ambiguous := ApplicationGroupPresence(group, at)
	if ambiguous.State != "ambiguous" || ambiguous.Anchor != nil || *ambiguous.WindowCount != 2 {
		t.Fatalf("guessed anchor: %+v", ambiguous)
	}
	group.Ambiguous = false
	group.Anchor = &WindowApplication{ContainerID: 43, Workspace: "__i3_scratch", Scratchpad: true}
	present := ApplicationGroupPresence(group, at)
	if present.State != "present" || present.Anchor.ContainerID != 43 || !present.Anchor.Scratchpad || present.Anchor.Visible || present.Anchor.Workspace != "" {
		t.Fatalf("hidden scratchpad lost: %+v", present)
	}
	group.Anchor.Workspace = "98"
	if shown := ApplicationGroupPresence(group, at); !shown.Anchor.Visible || shown.Anchor.Workspace != "98" {
		t.Fatalf("shown scratchpad lost: %+v", shown)
	}
}

func TestApplicationInventoryProjectionOmitsPrivateLauncherPaths(t *testing.T) {
	registry := applicationInventoryTestRegistry("Editor")
	registry.Contexts[0].Launcher.DesktopPath = "/private/source.desktop"
	registry.Contexts[0].Launcher.ApprovedDesktopPath = "/private/snapshot.desktop"
	registry.Contexts[0].Launcher.ApprovedExecutablePath = "/private/executable"
	items := BuildApplicationInventory(ApplicationInventorySnapshot{Registry: registry})
	data, err := json.Marshal(items)
	if err != nil || strings.Contains(string(data), "/private/") {
		t.Fatalf("private launcher data exported: %s, %v", data, err)
	}
	if items[0].SavedPlacement.State != "unknown" || len(items[0].PendingRestore) != 0 || items[0].RestoreOutcome != nil {
		t.Fatalf("invented optional evidence: %+v", items[0])
	}
}
