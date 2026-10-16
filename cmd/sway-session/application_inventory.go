package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func executeApplicationInventory(ctx context.Context, command, socket, selection string, focused bool, deps dependencies) (commandResult, *commandFailure) {
	root, failed := stateRoot(deps)
	if failed != nil {
		return commandResult{}, failed
	}
	snapshot, err := sessionstate.ReadApplicationInventorySnapshotContext(ctx, root)
	if err != nil {
		return commandResult{}, classifyStateError("read application inventory", err)
	}
	var selected sessionstate.ContextID
	if selection != "" {
		index, err := sessionstate.ResolveContext(snapshot.Registry, selection)
		if err == nil && snapshot.Registry.Contexts[index].App == nil {
			err = errors.New("selected context is not a desktop application")
		}
		if err != nil {
			return commandResult{}, classifyStateError("select application inventory", err)
		}
		selected = snapshot.Registry.Contexts[index].ID
	}
	items := sessionstate.BuildApplicationInventory(snapshot)
	if socket == "" {
		socket = os.Getenv("SWAYSOCK")
	}
	observationCtx, cancel := context.WithTimeout(ctx, terminalObservationTimeout)
	defer cancel()
	var tree *swayipc.TreeNode
	reason := "compositor_unavailable"
	if (len(items) > 0 || focused) && filepath.IsAbs(socket) && deps.newSwayClient != nil {
		client := deps.newSwayClient(socket)
		if client != nil {
			tree, err = requestTree(observationCtx, client)
			client.Close()
			if err == nil && tree != nil && tree.ID > 0 && tree.Type == "root" {
				reason = ""
			} else {
				tree = nil
				reason = "window_observation_unavailable"
			}
		}
	}
	if ctx.Err() != nil {
		return commandResult{}, failure("canceled", "read application inventory", "The request was canceled.")
	}
	if focused {
		if tree == nil {
			return commandResult{}, failure("sway_ipc", "resolve focused application", "Focused inventory requires a running Sway compositor. Use app status --context UUID_OR_LABEL for saved inventory offline.")
		}
		resolution, err := sessionstate.ResolveFocusedApplication(tree, sessionstate.DesktopCatalog{}, snapshot.Registry)
		if err != nil {
			return commandResult{}, failure("app_resolution", "resolve focused application", err.Error())
		}
		if resolution.Registered == nil {
			return commandResult{Command: command, Message: "Focused application is not registered."}, nil
		}
		selected = resolution.Registered.ID
	}
	groups := map[sessionstate.ContextID]sessionstate.ApplicationGroup{}
	if tree != nil {
		groups, err = sessionstate.ObserveRegisteredApplicationGroups(tree, snapshot.Registry)
		if err != nil {
			reason = "window_observation_unavailable"
		}
	}
	if selected != "" {
		filtered := make([]sessionstate.ApplicationInventoryItem, 0, 1)
		for _, item := range items {
			if item.ContextID == selected {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	at := time.Now().UTC()
	if deps.now != nil {
		at = deps.now().UTC()
	}
	currentCompositor := ""
	if tree != nil && reason == "" {
		currentCompositor, _ = compositorIdentity(socket)
	}
	for index := range items {
		item := &items[index]
		if reason == "" {
			item.Presence = sessionstate.ApplicationGroupPresence(groups[item.ContextID], at)
		} else {
			item.Presence.Reason = reason
		}
		if item.LaunchGuard != nil && currentCompositor != "" && snapshot.ApplicationSession != nil {
			matches := snapshot.ApplicationSession.CompositorID == currentCompositor
			item.LaunchGuard.CompositorMatches = &matches
		}
	}
	observeApplicationLaunchers(observationCtx, root, deps, snapshot.Registry, items)
	if ctx.Err() != nil {
		return commandResult{}, failure("canceled", "read application inventory", "The request was canceled.")
	}
	return commandResult{Command: command, Contexts: []sessionstate.Context{}, Applications: &items}, nil
}

// Four workers share the command's single observation deadline. Saved contexts
// have no count limit; contexts queued after expiry retain explicit unknowns.
func observeApplicationLaunchers(ctx context.Context, root string, deps dependencies, registry sessionstate.Registry, items []sessionstate.ApplicationInventoryItem) {
	registered := make(map[sessionstate.ContextID]sessionstate.Context)
	for _, item := range registry.Contexts {
		if item.App != nil {
			registered[item.ID] = item
		}
	}
	observe := deps.observeApplicationLauncher
	if observe == nil {
		launcher := sessionstate.DesktopApplicationLauncher{StateRoot: root, GIO: "/usr/bin/gio", Flatpak: "/usr/bin/flatpak", ObservationResolveSystem: deps.resolveSystem, ObservationRunner: deps.herdrRunner}
		observe = launcher.ObserveContext
	}
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(4, len(items)) {
		workers.Go(func() {
			for index := range jobs {
				if ctx.Err() != nil {
					items[index].LauncherValidity = sessionstate.ApplicationLauncherObservation{State: sessionstate.ApplicationLauncherUnknown, Reason: "observation_timeout"}
					continue
				}
				items[index].LauncherValidity = observe(ctx, registered[items[index].ContextID])
				at := time.Now().UTC()
				if deps.now != nil {
					at = deps.now().UTC()
				}
				items[index].LauncherObservedAt = &at
			}
		})
	}
	for index := range items {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
}

func writeApplicationInventory(writer io.Writer, items []sessionstate.ApplicationInventoryItem) error {
	if len(items) == 0 {
		_, err := fmt.Fprintln(writer, "No registered desktop applications.")
		return err
	}
	for _, item := range items {
		label := item.Label
		if label == "" {
			label = item.Launcher.DesktopID
			if label == "" {
				label = item.Launcher.FlatpakID
			}
		}
		intent := "closed"
		if item.DesiredOpen {
			intent = "desired open"
		}
		if _, err := fmt.Fprintf(writer, "%s (%s)\n  state: %s; intent: %s; policy: %s\n  next login: %s\n  window: %s; launcher: %s (%s)\n", label, item.ContextID, item.State, intent, item.Policy, sessionstate.RestorePolicyExplanation(item.RestorePolicy.Reason), item.Presence.State, item.LauncherValidity.State, item.LauncherValidity.Reason); err != nil {
			return err
		}
		if item.Presence.Reason != "" {
			if _, err := fmt.Fprintf(writer, "  window evidence: %s\n", applicationPresenceExplanation(item.Presence.Reason)); err != nil {
				return err
			}
		}
		if item.Launcher.Administrative {
			if _, err := fmt.Fprintln(writer, "  system authentication may be requested during restore"); err != nil {
				return err
			}
		}
		if item.LauncherValidity.State == sessionstate.ApplicationLauncherMissing {
			if _, err := fmt.Fprintln(writer, "  launcher missing; restore cannot start it until it is available again"); err != nil {
				return err
			}
		}
		if item.LauncherValidity.State == sessionstate.ApplicationLauncherReapprovalRequired {
			hint := "  launcher trust changed; inspect it before app reapprove CONTEXT"
			if item.Launcher.Administrative {
				hint = "  administrative launcher trust changed; inspect it before app reapprove CONTEXT"
			}
			if _, err := fmt.Fprintln(writer, hint); err != nil {
				return err
			}
		}
		if anchor := item.Presence.Anchor; anchor != nil {
			if _, err := fmt.Fprintf(writer, "  placement: workspace=%s; scratchpad=%t; visible=%t; container=%d\n", anchor.Workspace, anchor.Scratchpad, anchor.Visible, anchor.ContainerID); err != nil {
				return err
			}
		}
		placement := item.SavedPlacement
		if _, err := fmt.Fprintf(writer, "  saved placement: %s", placement.State); err != nil {
			return err
		}
		if placement.Workspace != "" {
			if _, err := fmt.Fprintf(writer, "; workspace=%s; mode=%s", placement.Workspace, placement.RestoreMode); err != nil {
				return err
			}
		}
		if placement.Scratchpad != nil {
			if _, err := fmt.Fprintf(writer, "; scratchpad; visible=%t; workspace=%s", placement.Scratchpad.Visible, placement.Scratchpad.Workspace); err != nil {
				return err
			}
		}
		if placement.Reason != "" {
			explanation := placement.Reason
			if explanation == "saved_placement_only" {
				explanation = "degraded: saved placement only; exact layout unavailable"
			}
			if explanation == "saved_layout_unavailable" {
				explanation = "saved layout evidence unavailable"
			}
			if _, err := fmt.Fprintf(writer, "; %s", explanation); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(writer); err != nil {
			return err
		}
		if item.RestoreOutcome == nil {
			if _, err := fmt.Fprintln(writer, "  restore: no recorded outcome"); err != nil {
				return err
			}
		} else {
			if err := writeApplicationRestoreEvidence(writer, "last restore", *item.RestoreOutcome); err != nil {
				return err
			}
		}
		for _, pending := range item.PendingRestore {
			if err := writeApplicationRestoreEvidence(writer, "pending restore", pending); err != nil {
				return err
			}
		}
		if item.LaunchGuard != nil {
			if _, err := fmt.Fprintf(writer, "  launch guard: recorded %s; current process not established\n", item.LaunchGuard.StartedAt.Format(time.RFC3339)); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeApplicationRestoreEvidence(writer io.Writer, label string, evidence sessionstate.ApplicationRestoreEvidence) error {
	_, err := fmt.Fprintf(writer, "  %s: %s; source=%s; stage=%s; reason=%s; identity=%s\n", label, evidence.Outcome.Status, evidence.Outcome.Source, evidence.Stage, evidence.Outcome.Reason, evidence.Identity)
	if err == nil && (evidence.Outcome.Reason == "layout_failed" || evidence.Outcome.Reason == "layout_timeout") {
		_, err = fmt.Fprintln(writer, "  degraded restore: that attempt did not prove the requested exact layout")
	}
	return err
}

func applicationPresenceExplanation(reason string) string {
	switch reason {
	case "anchor_ambiguous":
		return "matching windows found; placement anchor is ambiguous"
	case "compositor_unavailable":
		return "Sway unavailable; current windows are unknown"
	default:
		return "current window observation unavailable"
	}
}
