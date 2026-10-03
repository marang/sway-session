package main

import (
	"context"
	"fmt"
	"io"
	"slices"

	sessionstate "github.com/marang/sway-session/internal/session"
)

// Placement is known only when the shared application observer finds an exact
// anchor. Null booleans distinguish unknown placement from an observed false.
type applicationPlacementResult struct {
	ContextID   sessionstate.ContextID `json:"context_id"`
	Presence    string                 `json:"presence"`
	WindowCount int                    `json:"window_count"`
	ContainerID int64                  `json:"container_id,omitempty"`
	Workspace   string                 `json:"workspace,omitempty"`
	Scratchpad  *bool                  `json:"scratchpad"`
	Visible     *bool                  `json:"visible"`
}

func executeStatus(ctx context.Context, arguments []string, deps dependencies) (commandResult, *commandFailure) {
	flags := newFlagSet("status")
	socket := flags.String("socket", "", "Sway IPC socket")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return commandResult{}, usageFailure("status", "status accepts only --socket PATH and no positional arguments")
	}
	if deps.newSwayClient == nil {
		return commandResult{}, failure("sway", "observe live application placement", "Sway client is unavailable. Run inside Sway or pass --socket PATH to a running compositor.")
	}
	environment, commandFailure := loadApplicationEnvironment(ctx, *socket, deps, true, false)
	if commandFailure != nil {
		for index := range commandFailure.diagnostics {
			item := &commandFailure.diagnostics[index]
			if item.Code == "sway" || item.Code == "sway_tree" {
				item.Hint += ". Check that Sway is running and --socket PATH or SWAYSOCK selects its accessible IPC socket; live placement could not be observed."
			}
		}
		return commandResult{}, commandFailure
	}
	defer environment.close()
	if environment.tree == nil || environment.tree.Type != "root" {
		return commandResult{}, failure("sway_tree", "observe live application placement", "Sway returned an invalid tree root. Check that --socket PATH or SWAYSOCK selects a running Sway compositor; live placement could not be observed.")
	}

	// Restore observes only active applications. Status includes archived
	// registrations too, without changing their stored lifecycle or policy.
	observationRegistry := environment.registry
	observationRegistry.Contexts = slices.Clone(environment.registry.Contexts)
	for index := range observationRegistry.Contexts {
		observationRegistry.Contexts[index].State = sessionstate.ContextActive
		observationRegistry.Contexts[index].ArchivedAt = nil
		observationRegistry.Contexts[index].Lifecycle = nil
	}
	groups, err := sessionstate.ObserveApplicationGroups(environment.tree, observationRegistry)
	if err != nil {
		return commandResult{}, failure("application_observation", "observe live application placement", err.Error()+". Check Sway application identities and context marks; live placement could not be determined.")
	}
	placements := make([]applicationPlacementResult, 0, len(groups))
	for _, registered := range environment.registry.Contexts {
		if registered.App == nil {
			continue
		}
		group := groups[registered.ID]
		placement := applicationPlacementResult{ContextID: registered.ID, Presence: "absent", WindowCount: len(group.Windows)}
		switch {
		case group.Ambiguous || len(group.Windows) > 0 && group.Anchor == nil:
			placement.Presence = "ambiguous"
		case group.Anchor != nil:
			placement.Presence = "present"
			placement.ContainerID = group.Anchor.ContainerID
			scratchpad := group.Anchor.Scratchpad
			// Visible describes a shown scratchpad window, regardless of focus.
			visible := group.Anchor.Workspace != "__i3_scratch"
			placement.Scratchpad = &scratchpad
			placement.Visible = &visible
			if visible {
				placement.Workspace = group.Anchor.Workspace
			}
		}
		placements = append(placements, placement)
	}
	return commandResult{Command: "status", Contexts: environment.registry.Contexts, ApplicationPlacements: &placements}, nil
}

func writeStatus(writer io.Writer, result commandResult) error {
	placements := make(map[sessionstate.ContextID]applicationPlacementResult, len(*result.ApplicationPlacements))
	for _, placement := range *result.ApplicationPlacements {
		placements[placement.ContextID] = placement
	}
	for _, registered := range result.Contexts {
		label := registered.Label
		if label == "" {
			label, _ = launcherOutput(registered.Launcher)
		}
		detail := string(registered.Launcher.Kind)
		if placement, exists := placements[registered.ID]; exists {
			detail = "application=" + placement.Presence
			if placement.Scratchpad != nil && placement.Visible != nil {
				detail += fmt.Sprintf("\tscratchpad=%t\tvisible=%t", *placement.Scratchpad, *placement.Visible)
				if placement.Workspace != "" {
					detail += "\tworkspace=" + placement.Workspace
				}
				detail += fmt.Sprintf("\tcontainer=%d", placement.ContainerID)
			}
		}
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", label, registered.ID, registered.State, detail); err != nil {
			return err
		}
	}
	return nil
}
