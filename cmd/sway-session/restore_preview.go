package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

type restorePreviewResult struct {
	ObservedAt time.Time            `json:"observed_at"`
	Contexts   []restorePreviewItem `json:"contexts"`
}

type restorePreviewItem struct {
	ContextID sessionstate.ContextID             `json:"context_id"`
	Label     string                             `json:"label"`
	Kind      sessionstate.LauncherKind          `json:"kind"`
	Status    string                             `json:"status"`
	Policy    sessionstate.RestorePolicyDecision `json:"policy"`
	Window    string                             `json:"window"`
	Reason    string                             `json:"reason,omitempty"`
	Lifecycle *sessionstate.LifecycleTransition  `json:"lifecycle,omitempty"`
}

// Preview only reads saved policy and a bounded compositor observation. It does
// not initialize managers, evaluate launch commands, or queue daemon work.
func executeRestorePreview(ctx context.Context, socket string, deps dependencies) (commandResult, *commandFailure) {
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	registry, err := sessionstate.ReadRegistrySnapshotContext(ctx, root)
	if err != nil {
		return commandResult{}, classifyStateError("load restore preview", err)
	}
	if socket == "" {
		socket = os.Getenv("SWAYSOCK")
	}
	observationCtx, cancel := context.WithTimeout(ctx, terminalObservationTimeout)
	defer cancel()
	windows := map[sessionstate.ContextID]sessionstate.ManagedWindow{}
	issues := map[sessionstate.ContextID]bool{}
	groups := map[sessionstate.ContextID]sessionstate.ApplicationGroup{}
	observationReason := "compositor_unavailable"
	if filepath.IsAbs(socket) && deps.newSwayClient != nil {
		client := deps.newSwayClient(socket)
		if client != nil {
			defer client.Close()
			tree, treeErr := requestTree(observationCtx, client)
			if treeErr == nil && tree != nil && tree.ID > 0 && tree.Type == "root" {
				var windowIssues []sessionstate.ManagedWindowIssue
				windows, windowIssues, err = sessionstate.ObserveManagedWindowsIsolated(tree, registry)
				if err == nil {
					groups, err = sessionstate.ObserveApplicationGroups(tree, registry)
				}
				if err == nil {
					observationReason = ""
					for _, issue := range windowIssues {
						issues[issue.ContextID] = true
					}
				} else {
					observationReason = "window_observation_unavailable"
				}
			} else if treeErr == nil {
				observationReason = "window_observation_unavailable"
			}
		}
	}
	preview := restorePreviewResult{ObservedAt: deps.now().UTC(), Contexts: []restorePreviewItem{}}
	for _, value := range registry.Contexts {
		policy := sessionstate.EvaluateRestorePolicy(value)
		item := restorePreviewItem{ContextID: value.ID, Label: value.Label, Kind: value.Launcher.Kind, Policy: policy, Window: "unknown", Lifecycle: value.Lifecycle}
		if item.Label == "" {
			item.Label = value.Launcher.Session
			if item.Label == "" {
				item.Label = string(value.ID)
			}
		}
		item.Status = "skipped"
		if policy.Eligible {
			item.Status = "eligible"
		}
		if observationReason != "" {
			item.Reason = observationReason
			if policy.Eligible {
				item.Status = "uncertain"
			}
		} else {
			item.Window = "closed"
			if value.App != nil && value.State == sessionstate.ContextActive {
				group := groups[value.ID]
				if len(group.Windows) != 0 {
					item.Window = "open"
				}
				if group.Ambiguous {
					item.Reason = "ambiguous_application_windows"
				}
			} else {
				if _, found := windows[value.ID]; found {
					item.Window = "open"
				}
			}
			if issues[value.ID] {
				item.Reason = "ambiguous_managed_window"
				item.Window = "unknown"
			}
			if policy.Eligible && item.Reason != "" {
				item.Status = "uncertain"
			}
		}
		preview.Contexts = append(preview.Contexts, item)
	}
	sort.Slice(preview.Contexts, func(i, j int) bool { return preview.Contexts[i].ContextID < preview.Contexts[j].ContextID })
	return commandResult{Command: "restore", Preview: true, Contexts: []sessionstate.Context{}, RestorePreview: &preview}, nil
}

func writeRestorePreview(writer io.Writer, preview restorePreviewResult) error {
	if _, err := fmt.Fprintf(writer, "Next-login restore preview · observed %s\nEligibility follows saved policy; successful launch and internal session recovery are not guaranteed.\n", preview.ObservedAt.Format(time.RFC3339)); err != nil {
		return err
	}
	for _, item := range preview.Contexts {
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\twindow=%s", item.Label, item.ContextID, item.Status, sessionstate.RestorePolicyExplanation(item.Policy.Reason), item.Window); err != nil {
			return err
		}
		if item.Reason != "" {
			if _, err := fmt.Fprintf(writer, "\t%s", restorePreviewObservationExplanation(item.Reason)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(writer); err != nil {
			return err
		}
	}
	return nil
}

func restorePreviewObservationExplanation(reason string) string {
	switch reason {
	case "compositor_unavailable":
		return "current windows could not be checked"
	case "window_observation_unavailable":
		return "current window evidence could not be interpreted"
	case "ambiguous_application_windows":
		return "application windows cannot be uniquely placed"
	case "ambiguous_managed_window":
		return "managed window identity is ambiguous"
	default:
		return "current window evidence is uncertain"
	}
}
