package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marang/sway-title-animator/internal/statefile"
)

const (
	// TerminalActivitySchemaVersion is the independently versioned schema for
	// terminal-activity.json. It is deliberately separate from contexts.json so
	// activity observations do not change a context's durable restore identity.
	TerminalActivitySchemaVersion = 1
	// TerminalActivityDirectory isolates activity mutations from the registry
	// directory lock, so recording focus cannot block terminal lifecycle work.
	TerminalActivityDirectory = "terminal-runtime"
	TerminalActivityFilename  = "terminal-activity.json"
)

// TerminalActivityState is the owner-only, bounded observation state for
// typed terminals. An absent activity file represents this valid empty state.
type TerminalActivityState struct {
	Version   int                `json:"version"`
	Terminals []TerminalActivity `json:"terminals"`
}

// TerminalActivity contains presentation-only timestamps. ContextID remains
// the only identity; neither timestamp participates in terminal launching,
// restore, or lifecycle selection.
type TerminalActivity struct {
	ContextID     ContextID  `json:"context_id"`
	CreatedAt     *time.Time `json:"created_at,omitempty"`
	LastFocusedAt *time.Time `json:"last_focused_at,omitempty"`
}

// Validate rejects malformed, ambiguous, or non-canonical activity state
// before it can be observed or atomically persisted.
func (state *TerminalActivityState) Validate() error {
	if state == nil {
		return errors.New("terminal activity state is nil")
	}
	if err := validateVersion("terminal activity", state.Version, TerminalActivitySchemaVersion); err != nil {
		return err
	}
	if state.Terminals == nil {
		return errors.New("terminal activity state must contain a terminals array")
	}
	if len(state.Terminals) > MaxContexts {
		return fmt.Errorf("terminal activity state contains %d terminals; maximum is %d", len(state.Terminals), MaxContexts)
	}
	seen := make(map[ContextID]struct{}, len(state.Terminals))
	for index := range state.Terminals {
		if err := state.Terminals[index].validate(); err != nil {
			return fmt.Errorf("terminals[%d]: %w", index, err)
		}
		if _, exists := seen[state.Terminals[index].ContextID]; exists {
			return fmt.Errorf("terminals[%d]: duplicate context ID %q", index, state.Terminals[index].ContextID)
		}
		seen[state.Terminals[index].ContextID] = struct{}{}
	}
	return nil
}

func (activity *TerminalActivity) validate() error {
	if err := activity.ContextID.Validate(); err != nil {
		return fmt.Errorf("invalid context ID: %w", err)
	}
	if err := validateTerminalActivityTimestamp("created_at", activity.CreatedAt); err != nil {
		return err
	}
	if err := validateTerminalActivityTimestamp("last_focused_at", activity.LastFocusedAt); err != nil {
		return err
	}
	if activity.CreatedAt == nil && activity.LastFocusedAt == nil {
		return errors.New("terminal activity must contain created_at or last_focused_at")
	}
	if activity.CreatedAt != nil && activity.LastFocusedAt != nil && activity.LastFocusedAt.Before(*activity.CreatedAt) {
		return errors.New("last_focused_at must not precede created_at")
	}
	return nil
}

func validateTerminalActivityTimestamp(name string, value *time.Time) error {
	if value == nil {
		return nil
	}
	if value.IsZero() || value.Location() != time.UTC {
		return fmt.Errorf("%s must be a non-zero canonical UTC timestamp", name)
	}
	return nil
}

// TerminalActivityFile returns the private transactional state file. statefile
// verifies owner-only paths, files, permissions, and bounded strict JSON.
func TerminalActivityFile(root string) statefile.JSONFile[TerminalActivityState] {
	return statefile.NewJSONFile(filepath.Join(root, TerminalActivityDirectory), TerminalActivityFilename, (*TerminalActivityState).Validate)
}

// ReadTerminalActivitySnapshot returns a bounded, validated snapshot without
// creating state. A missing activity file is the valid empty state.
func ReadTerminalActivitySnapshot(root string) (TerminalActivityState, error) {
	return ReadTerminalActivitySnapshotContext(context.Background(), root)
}

// ReadTerminalActivitySnapshotContext is ReadTerminalActivitySnapshot with an
// early cancellation check.
func ReadTerminalActivitySnapshotContext(ctx context.Context, root string) (TerminalActivityState, error) {
	state := emptyTerminalActivityState()
	if ctx == nil {
		return state, errors.New("terminal activity snapshot context is nil")
	}
	if err := ctx.Err(); err != nil {
		return state, err
	}
	if err := TerminalActivityFile(root).LoadSnapshotInto(&state); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return state, err
	}
	return state, nil
}

// UpdateTerminalActivity serializes one full load-mutate-save transaction.
// Missing state starts as the valid empty v1 document.
func UpdateTerminalActivity(root string, mutate func(*TerminalActivityState) error) (TerminalActivityState, error) {
	return UpdateTerminalActivityContext(context.Background(), root, mutate)
}

// UpdateTerminalActivityContext is UpdateTerminalActivity with cancelable
// private-directory lock acquisition.
func UpdateTerminalActivityContext(ctx context.Context, root string, mutate func(*TerminalActivityState) error) (TerminalActivityState, error) {
	return TerminalActivityFile(root).UpdateContext(ctx, emptyTerminalActivityState(), mutate)
}

// FindTerminalActivity finds one exact context observation without treating a
// label or another presentation value as an identity.
func FindTerminalActivity(state TerminalActivityState, id ContextID) (TerminalActivity, bool) {
	for _, activity := range state.Terminals {
		if activity.ContextID == id {
			return activity, true
		}
	}
	return TerminalActivity{}, false
}

// RecordTerminalCreatedAt records a newly created context generation. If an
// explicitly reused UUID still has orphaned activity, creation replaces that
// row and clears stale focus instead of inheriting another terminal's history.
func RecordTerminalCreatedAt(state *TerminalActivityState, id ContextID, observedAt time.Time) (TerminalActivity, bool, error) {
	if err := state.Validate(); err != nil {
		return TerminalActivity{}, false, err
	}
	if err := id.Validate(); err != nil {
		return TerminalActivity{}, false, fmt.Errorf("invalid context ID: %w", err)
	}
	if observedAt.IsZero() {
		return TerminalActivity{}, false, errors.New("terminal creation time must be non-zero")
	}
	canonical := observedAt.UTC()
	if index, exists := terminalActivityIndex(*state, id); exists {
		current := state.Terminals[index]
		if current.CreatedAt != nil && current.CreatedAt.Equal(canonical) && current.LastFocusedAt == nil {
			return current, false, nil
		}
		state.Terminals[index] = TerminalActivity{ContextID: id, CreatedAt: &canonical}
		return state.Terminals[index], true, nil
	}
	if len(state.Terminals) >= MaxContexts {
		return TerminalActivity{}, false, fmt.Errorf("terminal activity state already contains the maximum of %d terminals", MaxContexts)
	}
	activity := TerminalActivity{ContextID: id, CreatedAt: &canonical}
	state.Terminals = append(state.Terminals, activity)
	return activity, true, nil
}

// RecordTerminalFocusedAt records a monotonic confirmed-focus observation.
// Repeated or stale observations leave the state unchanged.
func RecordTerminalFocusedAt(state *TerminalActivityState, id ContextID, observedAt time.Time) (TerminalActivity, bool, error) {
	if err := state.Validate(); err != nil {
		return TerminalActivity{}, false, err
	}
	if err := id.Validate(); err != nil {
		return TerminalActivity{}, false, fmt.Errorf("invalid context ID: %w", err)
	}
	if observedAt.IsZero() {
		return TerminalActivity{}, false, errors.New("terminal focus time must be non-zero")
	}
	canonical := observedAt.UTC()
	if index, exists := terminalActivityIndex(*state, id); exists {
		current := state.Terminals[index]
		if current.CreatedAt != nil && canonical.Before(*current.CreatedAt) {
			return current, false, nil
		}
		if current.LastFocusedAt != nil && !canonical.After(*current.LastFocusedAt) {
			return current, false, nil
		}
		previous := current
		state.Terminals[index].LastFocusedAt = &canonical
		if err := state.Validate(); err != nil {
			state.Terminals[index] = previous
			return TerminalActivity{}, false, err
		}
		return state.Terminals[index], true, nil
	}
	if len(state.Terminals) >= MaxContexts {
		return TerminalActivity{}, false, fmt.Errorf("terminal activity state already contains the maximum of %d terminals", MaxContexts)
	}
	activity := TerminalActivity{ContextID: id, LastFocusedAt: &canonical}
	state.Terminals = append(state.Terminals, activity)
	return activity, true, nil
}

// RecordTerminalCreationContext transactionally records one new terminal
// generation and reconciles an unknown commit outcome by observation.
func RecordTerminalCreationContext(ctx context.Context, root string, id ContextID, observedAt time.Time) error {
	terminalIDs, err := authoritativeTerminalIDsContext(ctx, root)
	if err != nil {
		return err
	}
	if _, exists := terminalIDs[id]; !exists {
		return fmt.Errorf("terminal context %s is absent from the authoritative registry", id)
	}

	canonical := observedAt.UTC()
	_, err = UpdateTerminalActivityContext(ctx, root, func(state *TerminalActivityState) error {
		pruneTerminalActivity(state, terminalIDs)
		_, _, mutationErr := RecordTerminalCreatedAt(state, id, canonical)
		return mutationErr
	})
	if err == nil {
		return nil
	}
	var unknown *statefile.CommitOutcomeUnknownError
	if !errors.As(err, &unknown) {
		return err
	}
	visible, loadErr := ReadTerminalActivitySnapshotContext(ctx, root)
	if loadErr == nil {
		activity, exists := FindTerminalActivity(visible, id)
		if exists && activity.CreatedAt != nil && activity.CreatedAt.Equal(canonical) {
			return nil
		}
	}
	return errors.Join(err, loadErr)
}

func authoritativeTerminalIDsContext(ctx context.Context, root string) (map[ContextID]struct{}, error) {
	registry, err := ReadRegistrySnapshotContext(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("load authoritative terminal registry: %w", err)
	}
	terminalIDs := make(map[ContextID]struct{})
	for _, contextValue := range registry.Contexts {
		if contextValue.Launcher.Kind == LauncherHerdr && contextValue.Launcher.Terminal != nil {
			terminalIDs[contextValue.ID] = struct{}{}
		}
	}
	return terminalIDs, nil
}

func pruneTerminalActivity(state *TerminalActivityState, terminalIDs map[ContextID]struct{}) {
	retained := state.Terminals[:0]
	for _, activity := range state.Terminals {
		if _, exists := terminalIDs[activity.ContextID]; exists {
			retained = append(retained, activity)
		}
	}
	state.Terminals = retained
}

// RecordTerminalFocusBatchContext persists the newest confirmed Sway focus
// event for every exact terminal context in one bounded transaction.
func RecordTerminalFocusBatchContext(ctx context.Context, root string, observations map[ContextID]time.Time) error {
	if len(observations) == 0 {
		return nil
	}
	terminalIDs, err := authoritativeTerminalIDsContext(ctx, root)
	if err != nil {
		return err
	}
	eligible := make(map[ContextID]time.Time, len(observations))
	for id, observedAt := range observations {
		if _, exists := terminalIDs[id]; exists {
			eligible[id] = observedAt
		}
	}
	_, err = UpdateTerminalActivityContext(ctx, root, func(state *TerminalActivityState) error {
		pruneTerminalActivity(state, terminalIDs)
		for id, observedAt := range eligible {
			if _, _, mutationErr := RecordTerminalFocusedAt(state, id, observedAt); mutationErr != nil {
				return mutationErr
			}
		}
		return nil
	})
	if err == nil {
		return nil
	}
	var unknown *statefile.CommitOutcomeUnknownError
	if !errors.As(err, &unknown) {
		return err
	}
	visible, loadErr := ReadTerminalActivitySnapshotContext(ctx, root)
	if loadErr == nil && terminalFocusBatchVisible(visible, eligible) {
		return nil
	}
	return errors.Join(err, loadErr)
}

func terminalFocusBatchVisible(state TerminalActivityState, observations map[ContextID]time.Time) bool {
	for id, observedAt := range observations {
		activity, exists := FindTerminalActivity(state, id)
		if !exists || activity.LastFocusedAt == nil || activity.LastFocusedAt.Before(observedAt.UTC()) {
			return false
		}
	}
	return true
}

// RemoveTerminalActivityContext transactionally removes presentation activity.
// Missing files and rows are already absent and are not created as a side effect.
func RemoveTerminalActivityContext(ctx context.Context, root string, id ContextID) error {
	visible, err := ReadTerminalActivitySnapshotContext(ctx, root)
	if err != nil {
		return err
	}
	if _, exists := FindTerminalActivity(visible, id); !exists {
		return nil
	}
	_, err = UpdateTerminalActivityContext(ctx, root, func(state *TerminalActivityState) error {
		_, _, mutationErr := RemoveTerminalActivity(state, id)
		return mutationErr
	})
	if err == nil {
		return nil
	}
	var unknown *statefile.CommitOutcomeUnknownError
	if !errors.As(err, &unknown) {
		return err
	}
	visible, loadErr := ReadTerminalActivitySnapshotContext(ctx, root)
	if loadErr == nil {
		if _, exists := FindTerminalActivity(visible, id); !exists {
			return nil
		}
	}
	return errors.Join(err, loadErr)
}

// RemoveTerminalActivity removes the exact context observation. Missing
// observations are idempotent and report removed=false.
func RemoveTerminalActivity(state *TerminalActivityState, id ContextID) (TerminalActivity, bool, error) {
	if err := state.Validate(); err != nil {
		return TerminalActivity{}, false, err
	}
	if err := id.Validate(); err != nil {
		return TerminalActivity{}, false, fmt.Errorf("invalid context ID: %w", err)
	}
	index, exists := terminalActivityIndex(*state, id)
	if !exists {
		return TerminalActivity{}, false, nil
	}
	removed := state.Terminals[index]
	copy(state.Terminals[index:], state.Terminals[index+1:])
	state.Terminals = state.Terminals[:len(state.Terminals)-1]
	if err := state.Validate(); err != nil {
		return TerminalActivity{}, false, err
	}
	return removed, true, nil
}

func terminalActivityIndex(state TerminalActivityState, id ContextID) (int, bool) {
	for index := range state.Terminals {
		if state.Terminals[index].ContextID == id {
			return index, true
		}
	}
	return 0, false
}

func emptyTerminalActivityState() TerminalActivityState {
	return TerminalActivityState{Version: TerminalActivitySchemaVersion, Terminals: []TerminalActivity{}}
}
