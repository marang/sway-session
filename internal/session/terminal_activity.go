package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/marang/sway-title-animator/internal/statefile"
)

const (
	// TerminalActivitySchemaVersion independently versions terminal activity
	// rows so observations do not change a context's durable restore identity.
	TerminalActivitySchemaVersion = 1
)

// TerminalActivityState is the owner-only observation state for typed
// terminals. An absent database row set represents this valid empty state.
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

// TerminalActivityStoreFor returns the transactional terminal-activity view of
// the private sway-session state database.
func TerminalActivityStoreFor(root string) TerminalActivityStore {
	return TerminalActivityStore{root: root}
}

// ReadTerminalActivitySnapshot returns a validated snapshot without creating
// state. A missing database is the valid empty state.
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
	if err := TerminalActivityStoreFor(root).LoadIntoContext(ctx, &state); err != nil {
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
	return TerminalActivityStoreFor(root).UpdateContext(ctx, emptyTerminalActivityState(), mutate)
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
	activity := TerminalActivity{ContextID: id, LastFocusedAt: &canonical}
	state.Terminals = append(state.Terminals, activity)
	return activity, true, nil
}

// RecordTerminalCreationContext transactionally records one new terminal
// generation and reconciles an unknown commit outcome by observation.
func RecordTerminalCreationContext(ctx context.Context, root string, id ContextID, observedAt time.Time) error {
	if err := id.Validate(); err != nil {
		return fmt.Errorf("invalid context ID: %w", err)
	}
	if observedAt.IsZero() {
		return errors.New("terminal creation time must be non-zero")
	}
	canonical := observedAt.UTC()
	database, err := openStateDatabase(ctx, root, false)
	if err != nil {
		return err
	}
	defer database.Close()
	tx, err := beginStateWrite(ctx, database)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireStoredTerminalContext(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO terminal_activity (context_id, created_at, last_focused_at) VALUES (?, ?, NULL)
		ON CONFLICT(context_id) DO UPDATE SET created_at = excluded.created_at, last_focused_at = NULL`,
		id, canonical.Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("store terminal creation activity for %s: %w", id, err)
	}
	err = commitStateWrite(ctx, tx)
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

// RecordTerminalFocusBatchContext persists the newest confirmed Sway focus
// event for every exact terminal context in one bounded transaction.
func RecordTerminalFocusBatchContext(ctx context.Context, root string, observations map[ContextID]time.Time) error {
	if len(observations) == 0 {
		return nil
	}
	database, err := openStateDatabase(ctx, root, false)
	if err != nil {
		return err
	}
	defer database.Close()
	tx, err := beginStateWrite(ctx, database)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, observedAt := range observations {
		if err := id.Validate(); err != nil {
			return fmt.Errorf("invalid focused terminal context ID: %w", err)
		}
		if observedAt.IsZero() {
			return fmt.Errorf("terminal focus time for %s must be non-zero", id)
		}
		if err := requireStoredTerminalContext(ctx, tx, id); err != nil {
			return err
		}
		state := emptyTerminalActivityState()
		var createdValue, focusedValue sql.NullString
		err := tx.QueryRowContext(ctx, "SELECT created_at, last_focused_at FROM terminal_activity WHERE context_id = ?", id).Scan(&createdValue, &focusedValue)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("load terminal focus activity for %s: %w", id, err)
		}
		if err == nil {
			createdAt, parseErr := parseDatabaseTime("terminal creation time", createdValue)
			if parseErr != nil {
				return parseErr
			}
			focusedAt, parseErr := parseDatabaseTime("terminal focus time", focusedValue)
			if parseErr != nil {
				return parseErr
			}
			state.Terminals = append(state.Terminals, TerminalActivity{ContextID: id, CreatedAt: createdAt, LastFocusedAt: focusedAt})
		}
		activity, changed, mutationErr := RecordTerminalFocusedAt(&state, id, observedAt.UTC())
		if mutationErr != nil {
			return mutationErr
		}
		if !changed {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO terminal_activity (context_id, created_at, last_focused_at) VALUES (?, ?, ?)
			ON CONFLICT(context_id) DO UPDATE SET created_at = excluded.created_at, last_focused_at = excluded.last_focused_at`,
			id, canonicalDatabaseTime(activity.CreatedAt), canonicalDatabaseTime(activity.LastFocusedAt),
		); err != nil {
			return fmt.Errorf("store terminal focus activity for %s: %w", id, err)
		}
	}
	err = commitStateWrite(ctx, tx)
	if err == nil {
		return nil
	}
	var unknown *statefile.CommitOutcomeUnknownError
	if !errors.As(err, &unknown) {
		return err
	}
	visible, loadErr := ReadTerminalActivitySnapshotContext(ctx, root)
	if loadErr == nil && terminalFocusBatchVisible(visible, observations) {
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
	if err := id.Validate(); err != nil {
		return fmt.Errorf("invalid context ID: %w", err)
	}
	database, err := openStateDatabase(ctx, root, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer database.Close()
	tx, err := beginStateWrite(ctx, database)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM terminal_activity WHERE context_id = ?", id); err != nil {
		return fmt.Errorf("remove terminal activity for %s: %w", id, err)
	}
	err = commitStateWrite(ctx, tx)
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

func requireStoredTerminalContext(ctx context.Context, tx stateTransaction, id ContextID) error {
	contextValue, err := loadStoredContextReference(ctx, tx, id)
	if err != nil {
		return err
	}
	if contextValue.Launcher.Kind != LauncherHerdr || contextValue.Launcher.Terminal == nil {
		return fmt.Errorf("context %s is not a typed terminal context", id)
	}
	return nil
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
