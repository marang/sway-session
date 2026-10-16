package session

import (
	"context"
	"errors"
)

// ForgetRemote removes only an archived remote registration after the same
// fresh window/process proof used by terminal adapter reconfiguration. It never
// calls Herdr, removes profiles or touches remote panes. The lifecycle lock
// serializes this proof with Open/restore; SQL transactions contain no effects.
func (reconfigurer TerminalAdapterReconfigurer) ForgetRemote(ctx context.Context, id ContextID) (Context, error) {
	if ctx == nil {
		return Context{}, errors.New("remote forget context is nil")
	}
	if err := id.Validate(); err != nil {
		return Context{}, err
	}
	if reconfigurer.Client == nil {
		return Context{}, errors.New("remote forget requires a Sway observer")
	}
	if reconfigurer.ProcRoot == "" {
		reconfigurer.ProcRoot = "/proc"
	}
	if reconfigurer.FindProcesses == nil {
		reconfigurer.FindProcesses = FindTerminalAdapterProcesses
	}
	var removed Context
	err := WithTerminalLifecycleLockContext(ctx, reconfigurer.StateRoot, func() error {
		_, err := UpdateRegistryContext(ctx, reconfigurer.StateRoot, func(registry *Registry) error {
			index, err := ResolveContext(*registry, string(id))
			if err != nil {
				return err
			}
			current := registry.Contexts[index]
			if !IsRemoteTerminal(current) {
				return errors.New("terminal forget accepts only remote contexts; local terminal deletion uses purge")
			}
			if current.State != ContextArchived {
				return errors.New("archive the remote context before forgetting it")
			}
			if err := reconfigurer.requireTerminalClosed(ctx, *registry, current); err != nil {
				return err
			}
			removed, err = RemoveContext(registry, string(id))
			return err
		})
		return err
	})
	return removed, err
}
