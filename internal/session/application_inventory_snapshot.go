package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// ApplicationInventorySnapshot contains durable application inventory evidence
// from one SQLite read snapshot. Missing optional documents remain nil; missing
// state has an empty valid registry and restore report.
type ApplicationInventorySnapshot struct {
	Registry           Registry
	Layout             *LayoutSnapshot
	ApplicationSession *ApplicationSessionState
	RestoreReport      RestoreReport
}

// ReadApplicationInventorySnapshotContext never initializes, migrates, or
// repairs state. It closes the database before returning so callers can observe
// the compositor and launcher validity outside the read transaction.
func ReadApplicationInventorySnapshotContext(ctx context.Context, root string) (ApplicationInventorySnapshot, error) {
	snapshot := emptyApplicationInventorySnapshot()
	if ctx == nil {
		return snapshot, errors.New("application inventory snapshot context is nil")
	}
	database, err := openStateDatabase(ctx, root, false)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, err
	}
	defer database.Close()
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return snapshot, fmt.Errorf("begin application inventory snapshot: %w", err)
	}
	defer tx.Rollback()
	candidate, err := loadApplicationInventorySnapshotQuery(ctx, tx)
	if err != nil {
		return snapshot, err
	}
	if err := commitStateRead(ctx, tx); err != nil {
		return snapshot, fmt.Errorf("finish application inventory snapshot: %w", err)
	}
	return candidate, nil
}

func emptyApplicationInventorySnapshot() ApplicationInventorySnapshot {
	return ApplicationInventorySnapshot{Registry: emptyRegistry(), RestoreReport: emptyRestoreReport()}
}

func loadApplicationInventorySnapshotQuery(ctx context.Context, queryer stateQueryer) (ApplicationInventorySnapshot, error) {
	snapshot := emptyApplicationInventorySnapshot()
	registry, _, err := loadRegistrySnapshotQuery(ctx, queryer)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return snapshot, err
	}
	snapshot.Registry = registry
	layout, err := loadLayoutSnapshotQuery(ctx, queryer)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return snapshot, err
	}
	if err == nil {
		snapshot.Layout = &layout
	}
	applicationSession, err := loadApplicationSessionQuery(ctx, queryer)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return snapshot, err
	}
	if err == nil {
		snapshot.ApplicationSession = &applicationSession
	}
	exists, err := restoreReportTablesExist(ctx, queryer)
	if err != nil {
		return snapshot, err
	}
	if exists {
		snapshot.RestoreReport, err = loadRestoreReport(ctx, queryer)
		if err != nil {
			return snapshot, err
		}
	}
	return snapshot, nil
}
