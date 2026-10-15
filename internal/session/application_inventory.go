package session

import (
	"slices"
	"time"

	"github.com/marang/sway-session/internal/swayipc"
)

// ApplicationInventoryItem keeps policy, observations and historical proofs
// independent. It omits approval paths, process specifications and tree data.
type ApplicationInventoryItem struct {
	ContextID          ContextID                      `json:"context_id"`
	Label              string                         `json:"label,omitempty"`
	State              ContextState                   `json:"state"`
	ArchivedAt         *time.Time                     `json:"archived_at,omitempty"`
	Lifecycle          *LifecycleTransition           `json:"lifecycle,omitempty"`
	Launcher           ApplicationLauncherIdentity    `json:"launcher"`
	DesiredOpen        bool                           `json:"desired_open"`
	Policy             ApplicationRestorePolicy       `json:"policy"`
	RestorePolicy      RestorePolicyDecision          `json:"restore_policy"`
	Presence           ApplicationPresence            `json:"presence"`
	LauncherValidity   ApplicationLauncherObservation `json:"launcher_validity"`
	LauncherObservedAt *time.Time                     `json:"launcher_observed_at,omitempty"`
	SavedPlacement     ApplicationSavedPlacement      `json:"saved_placement"`
	RestoreOutcome     *ApplicationRestoreEvidence    `json:"restore_outcome,omitempty"`
	PendingRestore     []ApplicationRestoreEvidence   `json:"pending_restore"`
	LaunchGuard        *ApplicationLaunchGuard        `json:"launch_guard,omitempty"`
}

type ApplicationLauncherIdentity struct {
	Kind         LauncherKind        `json:"kind"`
	DesktopID    string              `json:"desktop_id,omitempty"`
	FlatpakID    string              `json:"flatpak_id,omitempty"`
	Installation FlatpakInstallation `json:"installation,omitempty"`
}

type ApplicationPresence struct {
	State       string                     `json:"state"`
	Reason      string                     `json:"reason,omitempty"`
	ObservedAt  *time.Time                 `json:"observed_at,omitempty"`
	WindowCount *int                       `json:"window_count"`
	Anchor      *ApplicationPresenceAnchor `json:"anchor,omitempty"`
}

type ApplicationPresenceAnchor struct {
	ContainerID int64  `json:"container_id"`
	Workspace   string `json:"workspace,omitempty"`
	Scratchpad  bool   `json:"scratchpad"`
	Visible     bool   `json:"visible"`
}

type ApplicationSavedPlacement struct {
	State       string                 `json:"state"`
	Workspace   string                 `json:"workspace,omitempty"`
	RestoreMode WorkspaceRestoreMode   `json:"restore_mode,omitempty"`
	Scratchpad  *ScratchpadRestoreWork `json:"scratchpad,omitempty"`
	Reason      string                 `json:"reason,omitempty"`
}

type ApplicationRestoreEvidence struct {
	Outcome  RestoreOutcome `json:"outcome"`
	Identity string         `json:"identity"`
	Stage    string         `json:"stage"`
}

type ApplicationLaunchGuard struct {
	StartedAt         time.Time `json:"started_at"`
	CompositorMatches *bool     `json:"compositor_matches"`
}

// LatestRestoreOutcomes uses the established CLI selection: newest attempt,
// with explicit attempts winning equal start times.
func LatestRestoreOutcomes(report RestoreReport) map[ContextID]RestoreOutcome {
	latest := make(map[ContextID]RestoreOutcome)
	for _, record := range report.Outcomes {
		previous, exists := latest[record.ContextID]
		if !exists || record.StartedAt.After(previous.StartedAt) || record.StartedAt.Equal(previous.StartedAt) && record.Source == "explicit" {
			latest[record.ContextID] = record
		}
	}
	return latest
}

// BuildApplicationInventory projects a validated consistent read snapshot.
// It does not execute launcher, compositor or restore-coordinator operations.
func BuildApplicationInventory(snapshot ApplicationInventorySnapshot) []ApplicationInventoryItem {
	items := make([]ApplicationInventoryItem, 0)
	latest := LatestRestoreOutcomes(snapshot.RestoreReport)
	byContext := make(map[ContextID][]RestoreOutcome)
	for _, record := range snapshot.RestoreReport.Outcomes {
		if record.Status == "pending" {
			byContext[record.ContextID] = append(byContext[record.ContextID], record)
		}
	}
	guards := make(map[ContextID]ApplicationLaunchAttempt)
	if snapshot.ApplicationSession != nil {
		for _, attempt := range snapshot.ApplicationSession.Attempts {
			guards[attempt.ContextID] = attempt
		}
	}
	for _, registered := range snapshot.Registry.Contexts {
		if registered.App == nil {
			continue
		}
		item := ApplicationInventoryItem{
			ContextID: registered.ID, Label: registered.Label, State: registered.State,
			ArchivedAt: registered.ArchivedAt, Lifecycle: registered.Lifecycle,
			Launcher:    ApplicationLauncherIdentity{Kind: registered.Launcher.Kind, DesktopID: registered.Launcher.DesktopID, FlatpakID: registered.Launcher.FlatpakID, Installation: registered.Launcher.FlatpakInstallation},
			DesiredOpen: registered.App.DesiredOpen, Policy: registered.App.RestorePolicy,
			RestorePolicy:    EvaluateRestorePolicy(registered),
			Presence:         ApplicationPresence{State: "unknown", Reason: "compositor_unavailable"},
			LauncherValidity: ApplicationLauncherObservation{State: ApplicationLauncherUnknown, Reason: "observation_unavailable"},
			SavedPlacement:   savedApplicationPlacement(snapshot.Layout, registered.ID),
			PendingRestore:   []ApplicationRestoreEvidence{},
		}
		if record, exists := latest[registered.ID]; exists {
			evidence := applicationRestoreEvidence(registered, record)
			item.RestoreOutcome = &evidence
		}
		for _, record := range byContext[registered.ID] {
			item.PendingRestore = append(item.PendingRestore, applicationRestoreEvidence(registered, record))
		}
		slices.SortFunc(item.PendingRestore, func(a, b ApplicationRestoreEvidence) int {
			if a.Outcome.Source < b.Outcome.Source {
				return -1
			}
			if a.Outcome.Source > b.Outcome.Source {
				return 1
			}
			return 0
		})
		if guard, exists := guards[registered.ID]; exists {
			item.LaunchGuard = &ApplicationLaunchGuard{StartedAt: guard.StartedAt.UTC()}
		}
		items = append(items, item)
	}
	slices.SortFunc(items, func(a, b ApplicationInventoryItem) int {
		if a.ContextID < b.ContextID {
			return -1
		}
		if a.ContextID > b.ContextID {
			return 1
		}
		return 0
	})
	return items
}

func applicationRestoreEvidence(item Context, record RestoreOutcome) ApplicationRestoreEvidence {
	identity := "unknown"
	if record.IdentityDigest != "" {
		identity = "changed"
		if record.IdentityDigest == RestoreContextDigest(item) {
			identity = "current"
		}
	}
	return ApplicationRestoreEvidence{Outcome: record, Identity: identity, Stage: record.Stage()}
}

func savedApplicationPlacement(layout *LayoutSnapshot, id ContextID) ApplicationSavedPlacement {
	if layout == nil {
		return ApplicationSavedPlacement{State: "unknown", Reason: "saved_layout_unavailable"}
	}
	for _, placement := range layout.Scratchpad {
		if placement.ContextID == id {
			return ApplicationSavedPlacement{State: "known", Scratchpad: &ScratchpadRestoreWork{Visible: placement.Visible, Workspace: placement.Workspace}}
		}
	}
	for _, workspace := range layout.Workspaces {
		if containsContextID(workspaceContextIDs(workspace), id) {
			placement := ApplicationSavedPlacement{State: "known", Workspace: workspace.Name, RestoreMode: workspace.RestoreMode}
			if workspace.RestoreMode == WorkspaceRestorePlacementOnly {
				placement.Reason = "saved_placement_only"
			}
			return placement
		}
	}
	return ApplicationSavedPlacement{State: "none"}
}

// ObserveRegisteredApplicationGroups includes archived registrations without
// changing their saved state; status and inventory share the identity observer.
func ObserveRegisteredApplicationGroups(tree *swayipc.TreeNode, registry Registry) (map[ContextID]ApplicationGroup, error) {
	registry.Contexts = slices.Clone(registry.Contexts)
	for index := range registry.Contexts {
		registry.Contexts[index].State = ContextActive
		registry.Contexts[index].ArchivedAt = nil
		registry.Contexts[index].Lifecycle = nil
	}
	return ObserveApplicationGroups(tree, registry)
}

func ApplicationGroupPresence(group ApplicationGroup, at time.Time) ApplicationPresence {
	count := len(group.Windows)
	at = at.UTC()
	presence := ApplicationPresence{State: "absent", WindowCount: &count, ObservedAt: &at}
	switch {
	case group.Ambiguous || count > 0 && group.Anchor == nil:
		presence.State = "ambiguous"
		presence.Reason = "anchor_ambiguous"
	case group.Anchor != nil:
		presence.State = "present"
		visible := group.Anchor.Workspace != "__i3_scratch"
		presence.Anchor = &ApplicationPresenceAnchor{ContainerID: group.Anchor.ContainerID, Scratchpad: group.Anchor.Scratchpad, Visible: visible}
		if visible {
			presence.Anchor.Workspace = group.Anchor.Workspace
		}
	}
	return presence
}
