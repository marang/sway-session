package session

import (
	"errors"
	"fmt"
	"time"
)

// LifecycleReason is a bounded reason for the latest state transition,
// rather than a history or a source of inferred lifecycle events.
type LifecycleReason string

const (
	LifecycleReasonExplicitArchive       LifecycleReason = "explicit_archive"
	LifecycleReasonExplicitActivate      LifecycleReason = "explicit_activate"
	LifecycleReasonObservedTerminalClose LifecycleReason = "observed_terminal_close"
)

// LifecycleTransition is optional durable metadata, not an event history.
type LifecycleTransition struct {
	Reason LifecycleReason `json:"reason"`
	At     time.Time       `json:"at"`
}

func validateLifecycleReason(state ContextState, reason LifecycleReason) error {
	switch reason {
	case LifecycleReasonExplicitArchive, LifecycleReasonObservedTerminalClose:
		if state != ContextArchived {
			return fmt.Errorf("lifecycle reason %q requires archived state", reason)
		}
	case LifecycleReasonExplicitActivate:
		if state != ContextActive {
			return fmt.Errorf("lifecycle reason %q requires active state", reason)
		}
	default:
		return fmt.Errorf("unsupported lifecycle reason %q", reason)
	}
	return nil
}

func (context *Context) validateLifecycle() error {
	if context.Lifecycle == nil {
		return nil
	}
	if err := validateLifecycleReason(context.State, context.Lifecycle.Reason); err != nil {
		return err
	}
	if context.Lifecycle.At.IsZero() || context.Lifecycle.At.Location() != time.UTC {
		return errors.New("lifecycle.at must be a non-zero canonical UTC timestamp")
	}
	if context.Lifecycle.Reason == LifecycleReasonObservedTerminalClose && context.Launcher.Kind != LauncherHerdr {
		return errors.New("observed_terminal_close requires a terminal context")
	}
	if context.State == ContextArchived && (context.ArchivedAt == nil || !context.ArchivedAt.Equal(context.Lifecycle.At)) {
		return errors.New("archived lifecycle.at must match archived_at")
	}
	return nil
}
