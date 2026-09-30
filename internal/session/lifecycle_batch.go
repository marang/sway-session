package session

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// ArchiveObservedTerminalContextsAt atomically archives an exact-ID batch with
// one full registry validation. Callers bound their batches; registry inventory
// has no total context limit. Archived contexts retain all existing metadata,
// including unknown legacy history. No external effects occur in this helper.
func ArchiveObservedTerminalContextsAt(registry *Registry, ids []ContextID, now time.Time) error {
	if registry == nil {
		return errors.New("context registry is nil")
	}
	requested := make(map[ContextID]struct{}, len(ids))
	for _, id := range ids {
		if err := id.Validate(); err != nil {
			return fmt.Errorf("invalid observed-close context ID: %w", err)
		}
		if _, exists := requested[id]; exists {
			return fmt.Errorf("duplicate observed-close context ID %q", id)
		}
		requested[id] = struct{}{}
	}
	indexes := make(map[ContextID]int, len(registry.Contexts))
	for index, current := range registry.Contexts {
		if _, exists := indexes[current.ID]; exists {
			return fmt.Errorf("duplicate registry context ID %q", current.ID)
		}
		indexes[current.ID] = index
	}
	candidate := *registry
	candidate.Contexts = slices.Clone(registry.Contexts)
	transitionAt := now.UTC()
	for _, id := range ids {
		index, exists := indexes[id]
		if !exists {
			return fmt.Errorf("%w: %q", ErrContextNotFound, id)
		}
		current := &candidate.Contexts[index]
		if current.Launcher.Kind != LauncherHerdr || current.Launcher.Terminal == nil {
			return fmt.Errorf("observed terminal close requires a terminal context: %q", id)
		}
		if current.State == ContextArchived {
			continue
		}
		if current.State != ContextActive {
			return fmt.Errorf("invalid context state %q", current.State)
		}
		if now.IsZero() {
			return errors.New("lifecycle transition time must be non-zero")
		}
		archivedAt := transitionAt
		current.State = ContextArchived
		current.ArchivedAt = &archivedAt
		current.Lifecycle = &LifecycleTransition{Reason: LifecycleReasonObservedTerminalClose, At: transitionAt}
	}
	if err := candidate.Validate(); err != nil {
		return err
	}
	*registry = candidate
	return nil
}
