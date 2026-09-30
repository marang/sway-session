package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"
)

// RestoreReportStore keeps an optional journal in the owner-only state database.
// It uses no registry foreign keys so missing contexts retain diagnostic outcomes.
type RestoreReportStore struct{ root string }

func RestoreReportStoreFor(root string) RestoreReportStore { return RestoreReportStore{root: root} }

func emptyRestoreReport() RestoreReport { return RestoreReport{Outcomes: []RestoreOutcome{}} }

func restoreReportTablesExist(ctx context.Context, queryer stateQueryer) (bool, error) {
	var count int
	if err := queryer.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name IN ('restore_report_meta', 'restore_outcomes')`).Scan(&count); err != nil {
		return false, err
	}
	if count != 0 && count != 2 {
		return false, errors.New("restore report journal is incomplete")
	}
	return count == 2, nil
}

func createRestoreReportTables(ctx context.Context, tx *stateWriteTransaction) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS restore_report_meta (id INTEGER PRIMARY KEY CHECK (id = 1), automatic_run_id TEXT NOT NULL, interrupted_run_id TEXT NOT NULL DEFAULT '', interrupted_at TEXT) STRICT`,
		`CREATE TABLE IF NOT EXISTS restore_outcomes (source TEXT NOT NULL CHECK (source IN ('automatic', 'explicit')), context_id TEXT NOT NULL, attempt_id TEXT NOT NULL, payload BLOB NOT NULL CHECK (length(payload) <= 16777216), PRIMARY KEY (source, context_id)) STRICT`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create restore journal: %w", err)
		}
	}
	return nil
}

// LoadContext never initializes missing state or journal tables. Its result is
// validated from one read snapshot; malformed or private payloads fail closed.
func (store RestoreReportStore) LoadContext(ctx context.Context) (RestoreReport, error) {
	report := emptyRestoreReport()
	if ctx == nil {
		return report, errors.New("restore report context is nil")
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	database, err := openStateDatabase(ctx, store.root, false)
	var legacy *LegacyStateError
	if errors.Is(err, os.ErrNotExist) || errors.As(err, &legacy) {
		// Legacy JSON has no restore journal. The shared opener deliberately
		// blocks ordinary database creation until explicit migration, but a
		// history read still reports its absent journal without changing state.
		return report, nil
	}
	if err != nil {
		return report, err
	}
	defer database.Close()
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	exists, err := restoreReportTablesExist(ctx, tx)
	if err != nil {
		return report, err
	}
	if exists {
		report, err = loadRestoreReport(ctx, tx)
		if err != nil {
			return emptyRestoreReport(), err
		}
	}
	if err := commitStateRead(ctx, tx); err != nil {
		return emptyRestoreReport(), err
	}
	return report, nil
}

func loadRestoreReport(ctx context.Context, queryer stateQueryer) (RestoreReport, error) {
	report := emptyRestoreReport()
	var interruptedAt sql.NullString
	err := queryer.QueryRowContext(ctx, `SELECT automatic_run_id, interrupted_run_id, interrupted_at FROM restore_report_meta WHERE id = 1`).Scan(&report.AutomaticRunID, &report.InterruptedRunID, &interruptedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return report, err
	}
	report.InterruptedAt, err = parseDatabaseTime("interrupted run time", interruptedAt)
	if err != nil {
		return report, err
	}
	rows, err := queryer.QueryContext(ctx, `SELECT source, context_id, attempt_id, payload FROM restore_outcomes ORDER BY source, context_id`)
	if err != nil {
		return report, err
	}
	defer rows.Close()
	for rows.Next() {
		var source, id, attempt string
		var payload []byte
		if err := rows.Scan(&source, &id, &attempt, &payload); err != nil {
			return report, err
		}
		outcome, err := decodeRestoreOutcome(source, id, attempt, payload)
		if err != nil {
			return report, err
		}
		report.Outcomes = append(report.Outcomes, outcome)
	}
	if err := rows.Err(); err != nil {
		return report, err
	}
	return report, report.Validate()
}

func decodeRestoreOutcome(source, id, attempt string, payload []byte) (RestoreOutcome, error) {
	var outcome RestoreOutcome
	if err := decodeDatabasePayload("restore outcome", payload, &outcome); err != nil {
		return outcome, err
	}
	if outcome.Source != source || string(outcome.ContextID) != id || outcome.AttemptID != attempt {
		return outcome, errors.New("restore outcome key does not match payload")
	}
	return outcome, outcome.Validate()
}

func loadRestoreOutcome(ctx context.Context, tx *stateWriteTransaction, source string, id ContextID) (RestoreOutcome, bool, error) {
	var attempt string
	var payload []byte
	err := tx.QueryRowContext(ctx, `SELECT attempt_id, payload FROM restore_outcomes WHERE source = ? AND context_id = ?`, source, string(id)).Scan(&attempt, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return RestoreOutcome{}, false, nil
	}
	if err != nil {
		return RestoreOutcome{}, false, err
	}
	outcome, err := decodeRestoreOutcome(source, string(id), attempt, payload)
	return outcome, true, err
}

func saveRestoreOutcome(ctx context.Context, tx *stateWriteTransaction, outcome RestoreOutcome) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	payload, err := marshalDatabasePayload("restore outcome", outcome)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO restore_outcomes (source, context_id, attempt_id, payload) VALUES (?, ?, ?, ?) ON CONFLICT (source, context_id) DO UPDATE SET attempt_id = excluded.attempt_id, payload = excluded.payload`, outcome.Source, string(outcome.ContextID), outcome.AttemptID, payload)
	return err
}

func recordAutomaticInterruption(ctx context.Context, tx *stateWriteTransaction, at time.Time) error {
	var runID, previousID string
	var previousAt sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT automatic_run_id, interrupted_run_id, interrupted_at FROM restore_report_meta WHERE id = 1`).Scan(&runID, &previousID, &previousAt); err != nil {
		return err
	}
	if err := validateRestoreUUID("automatic run", runID); err != nil {
		return err
	}
	previous, err := parseDatabaseTime("interrupted run time", previousAt)
	if err != nil {
		return err
	}
	if previousID == runID && previous != nil && previous.After(at) {
		at = *previous
	}
	_, err = tx.ExecContext(ctx, `UPDATE restore_report_meta SET interrupted_run_id = ?, interrupted_at = ? WHERE id = 1`, runID, at.Format(time.RFC3339Nano))
	return err
}

func validateRestoreRecords(source string, records []RestoreOutcome) error {
	seen := make(map[ContextID]struct{}, len(records))
	for _, record := range records {
		if err := record.Validate(); err != nil {
			return err
		}
		if record.Source != source {
			return errors.New("restore record source does not match begin operation")
		}
		if _, exists := seen[record.ContextID]; exists {
			return errors.New("duplicate restore context in begin operation")
		}
		seen[record.ContextID] = struct{}{}
	}
	return nil
}

// write serializes journal reads and effects with BEGIN IMMEDIATE. Mutations
// contain only SQLite and pure validation; all errors roll back optional DDL too.
func (store RestoreReportStore) write(ctx context.Context, create bool, mutate func(*stateWriteTransaction) error) error {
	if ctx == nil {
		return errors.New("restore report context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	database, err := openStateDatabase(ctx, store.root, create)
	if !create && errors.Is(err, os.ErrNotExist) {
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
	// Partial optional journals are corruption, not an invitation to repair.
	exists, err := restoreReportTablesExist(ctx, tx)
	if err != nil {
		return err
	}
	if !exists {
		if !create {
			return nil
		}
		if err := createRestoreReportTables(ctx, tx); err != nil {
			return err
		}
	}
	if err := mutate(tx); err != nil {
		return err
	}
	return commitStateWrite(ctx, tx)
}

// BeginAutomaticContext replaces only automatic rows on a new run. Retrying
// the same run ID preserves progress even if its original records are replayed.
func (store RestoreReportStore) BeginAutomaticContext(ctx context.Context, runID string, records []RestoreOutcome) error {
	if err := validateRestoreUUID("automatic run", runID); err != nil {
		return err
	}
	if err := validateRestoreRecords("automatic", records); err != nil {
		return err
	}
	return store.write(ctx, true, func(tx *stateWriteTransaction) error {
		var current string
		err := tx.QueryRowContext(ctx, `SELECT automatic_run_id FROM restore_report_meta WHERE id = 1`).Scan(&current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if current == runID {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM restore_outcomes WHERE source = 'automatic'`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO restore_report_meta (id, automatic_run_id) VALUES (1, ?) ON CONFLICT (id) DO UPDATE SET automatic_run_id = excluded.automatic_run_id`, runID); err != nil {
			return err
		}
		for _, record := range records {
			if err := saveRestoreOutcome(ctx, tx, record); err != nil {
				return err
			}
		}
		return nil
	})
}

// AppendAutomaticContext supports resumable seeding in caller-bounded chunks.
// Only the current run may append; existing source/context rows retain their
// tokens and progress. A missing journal or stale run returns false unchanged.
func (store RestoreReportStore) AppendAutomaticContext(ctx context.Context, runID string, records []RestoreOutcome) (bool, error) {
	if err := validateRestoreUUID("automatic run", runID); err != nil {
		return false, err
	}
	if err := validateRestoreRecords("automatic", records); err != nil {
		return false, err
	}
	matched := false
	err := store.write(ctx, false, func(tx *stateWriteTransaction) error {
		var current string
		err := tx.QueryRowContext(ctx, `SELECT automatic_run_id FROM restore_report_meta WHERE id = 1`).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if current != runID {
			return nil
		}
		for _, record := range records {
			_, exists, err := loadRestoreOutcome(ctx, tx, "automatic", record.ContextID)
			if err != nil {
				return err
			}
			if exists {
				continue
			}
			if err := saveRestoreOutcome(ctx, tx, record); err != nil {
				return err
			}
		}
		matched = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return matched, nil
}

// BeginExplicitContext preserves other contexts and replaces only strictly
// older explicit attempts. Replayed tokens never reset existing progress.
func (store RestoreReportStore) BeginExplicitContext(ctx context.Context, records []RestoreOutcome) error {
	if err := validateRestoreRecords("explicit", records); err != nil {
		return err
	}
	return store.write(ctx, true, func(tx *stateWriteTransaction) error {
		for _, record := range records {
			current, exists, err := loadRestoreOutcome(ctx, tx, "explicit", record.ContextID)
			if err != nil {
				return err
			}
			if exists && (current.AttemptID == record.AttemptID || !record.StartedAt.After(current.StartedAt)) {
				continue
			}
			if err := saveRestoreOutcome(ctx, tx, record); err != nil {
				return err
			}
		}
		return nil
	})
}

// UpdateContext ignores stale attempt tokens. Proofs accumulate by OR, update
// times never decrease, and terminal status/reason/ownership cannot be reset.
func (store RestoreReportStore) UpdateContext(ctx context.Context, source string, id ContextID, attemptID string, update RestoreOutcomeUpdate) (bool, error) {
	return store.updateContext(ctx, source, id, attemptID, update, false)
}

// UpdatePendingContext applies progress only while the matching latest attempt
// is pending. A stale token or finalized record returns false without changing
// proofs, timestamps, ownership or interruption summaries. Callers must check
// the match result before rearming explicit restore work outside this transaction.
func (store RestoreReportStore) UpdatePendingContext(ctx context.Context, source string, id ContextID, attemptID string, update RestoreOutcomeUpdate) (bool, error) {
	return store.updateContext(ctx, source, id, attemptID, update, true)
}

func (store RestoreReportStore) updateContext(ctx context.Context, source string, id ContextID, attemptID string, update RestoreOutcomeUpdate, requirePending bool) (bool, error) {
	if err := validateRestoreSource(source); err != nil {
		return false, err
	}
	if err := id.Validate(); err != nil {
		return false, err
	}
	if err := validateRestoreUUID("attempt", attemptID); err != nil {
		return false, err
	}
	if err := update.Validate(); err != nil {
		return false, err
	}
	matched := false
	err := store.write(ctx, false, func(tx *stateWriteTransaction) error {
		current, exists, err := loadRestoreOutcome(ctx, tx, source, id)
		if err != nil {
			return err
		}
		if !exists || current.AttemptID != attemptID || (requirePending && current.Status != "pending") {
			return nil
		}
		current.LaunchAccepted = current.LaunchAccepted || update.LaunchAccepted
		current.WindowMapped = current.WindowMapped || update.WindowMapped
		current.PlacementApplied = current.PlacementApplied || update.PlacementApplied
		current.LayoutApplied = current.LayoutApplied || update.LayoutApplied
		if update.UpdatedAt.After(current.UpdatedAt) {
			current.UpdatedAt = update.UpdatedAt
		}
		if current.Status == "pending" {
			current.Status, current.Reason = update.Status, update.Reason
			if update.OwnerRunID != "" {
				current.OwnerRunID = update.OwnerRunID
			}
			if current.Source == "automatic" && current.Status == "interrupted" {
				if err := recordAutomaticInterruption(ctx, tx, current.UpdatedAt); err != nil {
					return err
				}
			}
		}
		if err := saveRestoreOutcome(ctx, tx, current); err != nil {
			return err
		}
		matched = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return matched, nil
}

// InterruptPendingContext interrupts the current automatic run and explicit
// pending attempts owned by ownerRunID. Other owners and finalized rows survive.
func (store RestoreReportStore) InterruptPendingContext(ctx context.Context, ownerRunID string, now time.Time, reason string) error {
	if err := validateRestoreUUID("owner run", ownerRunID); err != nil {
		return err
	}
	if err := validateRestoreTime("interrupt time", now); err != nil {
		return err
	}
	if err := validateRestoreReason(reason); err != nil {
		return err
	}
	return store.interrupt(ctx, ownerRunID, now, reason, false)
}

// RecoverPendingContext is called before starting a new daemon automatic run.
// Fresh ownerless explicit requests survive startup for the daemon to adopt.
func (store RestoreReportStore) RecoverPendingContext(ctx context.Context, now time.Time) error {
	if err := validateRestoreTime("recovery time", now); err != nil {
		return err
	}
	return store.interrupt(ctx, "", now, "daemon_restarted", true)
}

func (store RestoreReportStore) interrupt(ctx context.Context, owner string, now time.Time, reason string, all bool) error {
	return store.write(ctx, false, func(tx *stateWriteTransaction) error {
		report, err := loadRestoreReport(ctx, tx)
		if err != nil {
			return err
		}
		var automaticInterruptedAt time.Time
		for _, outcome := range report.Outcomes {
			owned := (outcome.Source == "automatic" && report.AutomaticRunID == owner) || (outcome.Source == "explicit" && outcome.OwnerRunID == owner)
			if outcome.Status != "pending" || (!all && !owned) || (all && outcome.Source == "explicit" && outcome.OwnerRunID == "") {
				continue
			}
			outcome.Status, outcome.Reason = "interrupted", reason
			if now.After(outcome.UpdatedAt) {
				outcome.UpdatedAt = now
			}
			if err := saveRestoreOutcome(ctx, tx, outcome); err != nil {
				return err
			}
			if outcome.Source == "automatic" && outcome.UpdatedAt.After(automaticInterruptedAt) {
				automaticInterruptedAt = outcome.UpdatedAt
			}
		}
		if !automaticInterruptedAt.IsZero() {
			return recordAutomaticInterruption(ctx, tx, automaticInterruptedAt)
		}
		return nil
	})
}
