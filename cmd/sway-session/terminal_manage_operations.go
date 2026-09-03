package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
)

type terminalManageOperations interface {
	List(context.Context) ([]terminalInventoryResult, error)
	Open(context.Context, sessionstate.ContextID, string) error
	SetState(context.Context, sessionstate.ContextID, sessionstate.ContextState) error
	Rename(context.Context, sessionstate.ContextID, string) error
	Purge(context.Context, sessionstate.ContextID) (string, error)
}

type commandTerminalManageOperations struct {
	configPath string
	deps       dependencies
}

func (operations commandTerminalManageOperations) List(_ context.Context) ([]terminalInventoryResult, error) {
	result, commandFailure := executeTerminalList(nil, operations.deps)
	if commandFailure != nil {
		return nil, terminalManageFailure(commandFailure)
	}
	if result.Terminals == nil {
		return []terminalInventoryResult{}, nil
	}
	return append([]terminalInventoryResult(nil), (*result.Terminals)...), nil
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
