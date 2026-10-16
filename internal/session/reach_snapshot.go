package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// ReachSnapshot contains durable definitions, policy and presentation evidence
// from one SQLite read snapshot. External observations belong after this read.
type ReachSnapshot struct {
	Environments []WorkEnvironment
	Inventory    ApplicationInventorySnapshot
	Activity     TerminalActivityState
}

// ReadReachSnapshotContext never initializes or repairs state and closes its
// transaction and database access before callers can observe external systems.
func ReadReachSnapshotContext(ctx context.Context, root string) (ReachSnapshot, error) {
	return readReachSnapshotContext(ctx, root, "")
}

// ReadReachEnvironmentSnapshotContext reads only the selected definition with
// its coherent context evidence. An absent exact ID is an explicit error.
func ReadReachEnvironmentSnapshotContext(ctx context.Context, root string, id EnvironmentID) (ReachSnapshot, error) {
	if err := id.Validate(); err != nil {
		return emptyReachSnapshot(), err
	}
	return readReachSnapshotContext(ctx, root, id)
}

func emptyReachSnapshot() ReachSnapshot {
	return ReachSnapshot{
		Environments: []WorkEnvironment{},
		Inventory:    emptyApplicationInventorySnapshot(),
		Activity:     emptyTerminalActivityState(),
	}
}

func readReachSnapshotContext(ctx context.Context, root string, id EnvironmentID) (ReachSnapshot, error) {
	empty := emptyReachSnapshot()
	if ctx == nil {
		return empty, errors.New("reach snapshot context is nil")
	}
	database, err := openStateDatabase(ctx, root, false)
	if errors.Is(err, os.ErrNotExist) {
		if id != "" {
			return empty, ErrWorkEnvironmentNotFound
		}
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	defer database.Close()
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return empty, fmt.Errorf("begin reach snapshot: %w", err)
	}
	defer tx.Rollback()
	snapshot, err := loadReachSnapshotQuery(ctx, tx, id)
	if err != nil {
		return empty, err
	}
	if err := commitStateRead(ctx, tx); err != nil {
		return empty, fmt.Errorf("finish reach snapshot: %w", err)
	}
	return snapshot, nil
}

func loadReachSnapshotQuery(ctx context.Context, queryer stateQueryer, id EnvironmentID) (ReachSnapshot, error) {
	empty := emptyReachSnapshot()
	environments, err := loadWorkEnvironments(ctx, queryer, id)
	if err != nil {
		return empty, err
	}
	if id != "" && len(environments) == 0 {
		return empty, ErrWorkEnvironmentNotFound
	}
	inventory, err := loadApplicationInventorySnapshotQuery(ctx, queryer)
	if err != nil {
		return empty, err
	}
	activity, err := loadTerminalActivityQuery(ctx, queryer)
	if err != nil {
		return empty, err
	}
	return ReachSnapshot{Environments: environments, Inventory: inventory, Activity: activity}, nil
}
