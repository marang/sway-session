package main

import (
	"context"
	sessionstate "github.com/marang/sway-session/internal/session"
	"os"
	"path/filepath"
)

func executeTerminalForget(ctx context.Context, arguments []string, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("terminal forget")
	yes := set.Bool("yes", false, "remove only the archived remote registration")
	socket := set.String("socket", "", "Sway IPC socket")
	if err := set.Parse(arguments); err != nil || set.NArg() != 1 || !*yes {
		return commandResult{}, usageFailure("terminal", "terminal forget requires --yes, an exact remote context UUID, and an archived/closed context")
	}
	id := sessionstate.ContextID(set.Arg(0))
	if err := id.Validate(); err != nil {
		return commandResult{}, usageFailure("terminal", err.Error())
	}
	if *socket == "" {
		*socket = os.Getenv("SWAYSOCK")
	}
	if !filepath.IsAbs(*socket) || filepath.Clean(*socket) != *socket {
		return commandResult{}, failure("sway_socket", "a valid Sway IPC socket is required", "Run inside Sway or pass --socket PATH.")
	}
	root, problem := stateRoot(deps)
	if problem != nil {
		return commandResult{}, problem
	}
	client := deps.newSwayClient(*socket)
	if client == nil {
		return commandResult{}, failure("sway_socket", "Sway client unavailable", "")
	}
	defer client.Close()
	observed, ok := client.(sessionstate.ContextSwayRequestClient)
	if !ok {
		return commandResult{}, failure("sway_socket", "cancelable Sway client unavailable", "")
	}
	removed, err := (sessionstate.TerminalAdapterReconfigurer{StateRoot: root, Client: observed}).ForgetRemote(ctx, id)
	if err != nil {
		return commandResult{}, classifyStateError("forget remote registration", err)
	}
	return commandResult{Command: "terminal forget", Contexts: []sessionstate.Context{removed}, Message: "Removed the remote registration; Herdr work and saved profiles are preserved."}, nil
}
