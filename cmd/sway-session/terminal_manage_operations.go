package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	sessionstate "github.com/marang/sway-session/internal/session"
)

type terminalManageOperations interface {
	Load(context.Context, string) (terminalManageSnapshot, error)
	Open(context.Context, sessionstate.ContextID, string) error
	SetState(context.Context, sessionstate.ContextID, sessionstate.ContextState) error
	Rename(context.Context, sessionstate.ContextID, string) error
	Purge(context.Context, sessionstate.ContextID) (string, error)
	Migrate(context.Context) (string, error)
}

type terminalManageSnapshot struct {
	items       []terminalInventoryResult
	windows     map[sessionstate.ContextID]terminalWindowPresence
	windowError error
}

type commandTerminalManageOperations struct {
	configPath string
	deps       dependencies
}

func (operations commandTerminalManageOperations) Load(ctx context.Context, socket string) (terminalManageSnapshot, error) {
	inventory, commandFailure := loadTerminalInventory(ctx, operations.deps)
	if commandFailure != nil {
		return terminalManageSnapshot{}, terminalManageFailure(commandFailure)
	}
	items := terminalInventory(inventory.Registry.Contexts, inventory.Activity)
	result := terminalManageSnapshot{
		items:   items,
		windows: terminalManageUnknownWindows(items),
	}
	if len(items) == 0 {
		return result, nil
	}
	if operations.deps.newSwayClient == nil {
		result.windowError = errors.New("sway window observation dependency is unavailable")
		return result, nil
	}
	client := operations.deps.newSwayClient(socket)
	if client == nil {
		result.windowError = errors.New("sway window observation client is unavailable")
		return result, nil
	}
	defer client.Close()
	tree, err := requestTree(ctx, client)
	if err != nil {
		result.windowError = err
		return result, nil
	}
	if tree.ID <= 0 || tree.Type != "root" {
		result.windowError = errors.New("sway tree response has no valid root")
		return result, nil
	}
	windows, issues, err := sessionstate.ObserveManagedWindowsIsolated(tree, inventory.Registry)
	if err != nil {
		result.windowError = fmt.Errorf("observe managed windows: %w", err)
		return result, nil
	}
	for _, item := range items {
		result.windows[item.ContextID] = terminalWindowClosed
	}
	for id := range windows {
		if _, terminal := result.windows[id]; terminal {
			result.windows[id] = terminalWindowOpen
		}
	}
	terminalIssues := 0
	for _, issue := range issues {
		if _, terminal := result.windows[issue.ContextID]; terminal {
			result.windows[issue.ContextID] = terminalWindowUnknown
			terminalIssues++
		}
	}
	if terminalIssues != 0 {
		identity := "identity is"
		if terminalIssues != 1 {
			identity = "identities are"
		}
		result.windowError = fmt.Errorf("%d terminal window %s ambiguous", terminalIssues, identity)
	}
	return result, nil
}

func (operations commandTerminalManageOperations) Open(ctx context.Context, id sessionstate.ContextID, socket string) error {
	arguments := []string{"--context", string(id)}
	if socket != "" {
		arguments = append(arguments, "--socket", socket)
	}
	_, commandFailure := executeTerminal(ctx, arguments, nil, io.Discard, false, operations.configPath, operations.deps)
	return terminalManageFailure(commandFailure)
}

func (operations commandTerminalManageOperations) SetState(ctx context.Context, id sessionstate.ContextID, state sessionstate.ContextState) error {
	name := "activate"
	if state == sessionstate.ContextArchived {
		name = "archive"
	} else if state != sessionstate.ContextActive {
		return fmt.Errorf("unsupported terminal state %q", state)
	}
	_, commandFailure := executeStateChange(ctx, name, []string{string(id)}, state, operations.deps)
	return terminalManageFailure(commandFailure)
}

func (operations commandTerminalManageOperations) Rename(ctx context.Context, id sessionstate.ContextID, label string) error {
	_, commandFailure := executeTerminalRename(ctx, []string{"--label", label, string(id)}, operations.deps)
	return terminalManageFailure(commandFailure)
}

func (operations commandTerminalManageOperations) Purge(ctx context.Context, id sessionstate.ContextID) (string, error) {
	result, commandFailure := executePurge(ctx, []string{"--yes", string(id)}, strings.NewReader(""), io.Discard, false, operations.deps)
	return result.Message, terminalManageFailure(commandFailure)
}

func (operations commandTerminalManageOperations) Migrate(ctx context.Context) (string, error) {
	root, err := operations.deps.stateRoot()
	if err != nil {
		return "", err
	}
	result, err := sessionstate.MigrateLegacyState(ctx, root)
	if err != nil {
		return "", err
	}
	if !result.Migrated {
		return "SQLite state is already current", nil
	}
	message := fmt.Sprintf(
		"Migrated %d contexts, %d terminal activity records, layout=%t, application session=%t; legacy JSON kept",
		result.Contexts, result.TerminalActivity, result.Layout, result.ApplicationSession,
	)
	skipped := result.SkippedApplicationAttempts + result.SkippedTerminalActivity
	if skipped != 0 {
		message += fmt.Sprintf("; skipped %d stale runtime records", skipped)
	}
	if result.CommitReconciled {
		message += "; commit acknowledgement was lost, migrated state verified"
	}
	return message, nil
}

func terminalManageFailure(commandFailure *commandFailure) error {
	if commandFailure == nil {
		return nil
	}
	if len(commandFailure.diagnostics) == 0 {
		return errors.New("terminal operation failed")
	}
	item := commandFailure.diagnostics[0]
	if item.Hint != "" {
		return fmt.Errorf("%s: %s", item.Message, item.Hint)
	}
	return errors.New(item.Message)
}
