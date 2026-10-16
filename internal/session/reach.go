package session

import (
	"context"
	"errors"

	"github.com/marang/sway-session/internal/swayipc"
)

// ReachStatus distinguishes a proposed action from an observed result. A
// missing window never becomes launch permission from an unavailable observation.
type ReachStatus string

const (
	ReachWouldFocus     ReachStatus = "would_focus"
	ReachWouldOpen      ReachStatus = "would_open"
	ReachAlreadyPresent ReachStatus = "already_present"
	ReachSkipped        ReachStatus = "skipped"
	ReachUncertain      ReachStatus = "uncertain"
	ReachBlocked        ReachStatus = "blocked"
	ReachFocused        ReachStatus = "focused"
	ReachOpening        ReachStatus = "opening"
	ReachOpened         ReachStatus = "opened"
	ReachFailed         ReachStatus = "failed"
	ReachCanceled       ReachStatus = "canceled"
)

type ReachMemberResult struct {
	ContextID ContextID   `json:"context_id"`
	Primary   bool        `json:"primary"`
	Status    ReachStatus `json:"status"`
	Reason    string      `json:"reason"`
}

type ReachPreview struct {
	EnvironmentID EnvironmentID       `json:"environment_id"`
	Revision      int64               `json:"revision"`
	Primary       ContextID           `json:"primary,omitempty"`
	Members       []ReachMemberResult `json:"members"`
	Ready         bool                `json:"ready"`
}

type ReachResult struct {
	EnvironmentID EnvironmentID       `json:"environment_id"`
	Primary       ContextID           `json:"primary,omitempty"`
	Members       []ReachMemberResult `json:"members"`
}

// ReachPresence is fresh canonical window evidence, never a saved placement.
// Unknown is deliberately its zero value. ContainerID is private execution
// evidence; callers cannot submit one as a Reach intent.
type ReachPresence struct {
	Known       bool
	Ambiguous   bool
	ContainerID int64
	Hidden      bool
	Restoring   bool
}

// ObserveReachPresence uses the same terminal identity and application group
// authorities as inventory/restore. An unmarked but unique app anchor can be
// focused without changing its registration or adding a presentation mark.
func ObserveReachPresence(root *swayipc.TreeNode, registry Registry) (map[ContextID]ReachPresence, error) {
	if root == nil || root.Type != "root" || root.ID <= 0 {
		return nil, errors.New("reach requires a valid fresh Sway tree")
	}
	windows, issues, err := ObserveManagedWindowsIsolated(root, registry)
	if err != nil {
		return nil, err
	}
	groups, err := ObserveApplicationGroups(root, registry)
	if err != nil {
		return nil, err
	}
	result := make(map[ContextID]ReachPresence, len(registry.Contexts))
	for _, item := range registry.Contexts {
		presence := ReachPresence{Known: true}
		if item.App != nil {
			group := groups[item.ID]
			presence.Ambiguous = group.Ambiguous || len(group.Windows) != 0 && group.Anchor == nil
			if group.Anchor != nil {
				presence.ContainerID = group.Anchor.ContainerID
				presence.Hidden = group.Anchor.Scratchpad && group.Anchor.Workspace == "__i3_scratch"
				presence.Restoring = group.Anchor.Workspace == RestoreStagingWorkspace
			}
		} else if window, found := windows[item.ID]; found {
			presence.ContainerID = window.ContainerID
			presence.Hidden = window.Workspace == "__i3_scratch"
			presence.Restoring = window.Workspace == RestoreStagingWorkspace
		}
		result[item.ID] = presence
	}
	for _, issue := range issues {
		result[issue.ContextID] = ReachPresence{Known: true, Ambiguous: true}
	}
	return result, nil
}

// PlanWorkEnvironmentReach is shared by preview and execution. It never owns
// launch, layout or lifecycle authority. Existing supported adapters provide no
// generic launch-scoped focus veto: missing windows are explicitly deferred.
func PlanWorkEnvironmentReach(ctx context.Context, environment WorkEnvironment, registry Registry, presence map[ContextID]ReachPresence, primaryOverride ContextID) (ReachPreview, error) {
	if ctx == nil {
		return ReachPreview{}, errors.New("reach context is nil")
	}
	if err := environment.Validate(); err != nil {
		return ReachPreview{}, err
	}
	if err := registry.Validate(); err != nil {
		return ReachPreview{}, err
	}
	primary := environment.Primary
	if primaryOverride != "" {
		found := false
		for _, id := range environment.Members {
			if id == primaryOverride {
				found = true
				break
			}
		}
		if !found {
			return ReachPreview{}, errors.New("explicit primary must belong to this environment")
		}
		primary = primaryOverride
	}
	preview := ReachPreview{EnvironmentID: environment.ID, Revision: environment.Revision, Primary: primary, Members: make([]ReachMemberResult, 0, len(environment.Members))}
	contexts := make(map[ContextID]Context, len(registry.Contexts))
	for _, item := range registry.Contexts {
		contexts[item.ID] = item
	}
	for _, id := range environment.Members {
		if err := ctx.Err(); err != nil {
			return ReachPreview{}, err
		}
		member := ReachMemberResult{ContextID: id, Primary: id == primary}
		item, found := contexts[id]
		observed := presence[id]
		switch {
		case !found:
			member.Status, member.Reason = ReachUncertain, "Context was removed; choose a replacement or remove this reference."
		case item.State != ContextActive:
			member.Status, member.Reason = ReachSkipped, "Archived; activate separately before reaching this member."
		case !observed.Known:
			member.Status, member.Reason = ReachUncertain, "Window observation unavailable; refresh when Sway is reachable."
		case observed.Ambiguous:
			member.Status, member.Reason = ReachUncertain, "Window identity is ambiguous; resolve it before reaching this member."
		case observed.Restoring:
			member.Status, member.Reason = ReachBlocked, "Window restore is in progress; wait before reaching this member."
		case observed.Hidden:
			member.Status, member.Reason = ReachBlocked, "Window is hidden in the scratchpad; show it explicitly first."
		case observed.ContainerID > 0 && member.Primary:
			member.Status, member.Reason = ReachWouldFocus, "Focus the existing window in place."
			preview.Ready = true
		case observed.ContainerID > 0:
			member.Status, member.Reason = ReachAlreadyPresent, "Leave the existing window in place."
		case item.App != nil && !item.App.DesiredOpen:
			member.Status, member.Reason = ReachSkipped, "Desired closed; Reach does not change saved lifecycle intent."
		default:
			member.Status, member.Reason = ReachBlocked, "Deferred: Sway cannot guarantee that a late window will preserve your focus. Open this member separately."
		}
		preview.Members = append(preview.Members, member)
	}
	if primary == "" {
		preview.Ready = false
	}
	return preview, nil
}
