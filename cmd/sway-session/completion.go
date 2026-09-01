package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
)

type completionCandidate struct {
	Value       string `json:"value"`
	Description string `json:"description"`
}

func executeCompletion(arguments []string, deps dependencies) (commandResult, *commandFailure) {
	if len(arguments) != 2 || arguments[0] != "contexts" || !supportedCompletionContextCommand(arguments[1]) {
		return commandResult{}, usageFailure("completion", "completion contexts requires one supported command")
	}
	root, commandFailure := stateRoot(deps)
	if commandFailure != nil {
		return commandResult{}, commandFailure
	}
	registry, err := sessionstate.ReadRegistrySnapshot(root)
	if err != nil {
		return commandResult{}, classifyStateError("load completion contexts", err)
	}
	candidates := make([]completionCandidate, 0, len(registry.Contexts))
	for _, context := range registry.Contexts {
		if !completionContextEligible(context, arguments[1]) {
			continue
		}
		candidates = append(candidates, completionCandidate{
			Value:       string(context.ID),
			Description: completionContextDescription(context),
		})
	}
	sort.Slice(candidates, func(left int, right int) bool {
		return candidates[left].Value < candidates[right].Value
	})
	return commandResult{Command: "completion contexts", Contexts: []sessionstate.Context{}, CompletionCandidates: candidates}, nil
}

func supportedCompletionContextCommand(command string) bool {
	switch command {
	case "archive", "activate", "restore", "restore-active", "purge", "app-forget":
		return true
	default:
		return false
	}
}

func completionContextEligible(context sessionstate.Context, command string) bool {
	switch command {
	case "archive":
		return context.State == sessionstate.ContextActive
	case "activate":
		return context.State == sessionstate.ContextArchived
	case "restore":
		return context.Launcher.Kind == sessionstate.LauncherHerdr || context.State == sessionstate.ContextActive
	case "restore-active":
		return context.State == sessionstate.ContextActive
	case "purge":
		return context.Launcher.Kind == sessionstate.LauncherHerdr
	case "app-forget":
		return context.App != nil
	default:
		return false
	}
}

func completionContextDescription(context sessionstate.Context) string {
	parts := make([]string, 0, 5)
	if context.Label != "" {
		parts = append(parts, context.Label)
	}
	parts = append(parts, string(context.State))
	launcherName, launcherDetail := launcherOutput(context.Launcher)
	parts = append(parts, string(context.Launcher.Kind)+":"+launcherName)
	if context.Launcher.Kind == sessionstate.LauncherHerdr && launcherDetail != "" {
		parts = append(parts, completionDisplayPath(launcherDetail))
	}
	if context.Provider != "" {
		parts = append(parts, "provider:"+context.Provider)
	}
	return strings.Join(parts, " · ")
}

func completionDisplayPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !filepath.IsAbs(home) {
		return path
	}
	relative, err := filepath.Rel(filepath.Clean(home), path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return path
	}
	if relative == "." {
		return "~"
	}
	return "~" + string(os.PathSeparator) + relative
}
