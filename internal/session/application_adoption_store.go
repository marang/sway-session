package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const applicationAdoptionsSQL = `CREATE TABLE application_adoptions (
	context_id TEXT PRIMARY KEY REFERENCES contexts(id) ON DELETE CASCADE,
	compositor_id TEXT NOT NULL,
	observed_at TEXT NOT NULL
) STRICT`

const applicationAdoptionInsertRevisionSQL = `CREATE TRIGGER application_adoption_insert_revision AFTER INSERT ON application_adoptions
	WHEN EXISTS (SELECT 1 FROM application_session WHERE id = 1)
	BEGIN UPDATE application_session SET revision = revision + 1 WHERE id = 1; END`

const applicationAdoptionUpdateRevisionSQL = `CREATE TRIGGER application_adoption_update_revision AFTER UPDATE ON application_adoptions
	WHEN EXISTS (SELECT 1 FROM application_session WHERE id = 1)
	BEGIN UPDATE application_session SET revision = revision + 1 WHERE id = 1; END`

const applicationAdoptionDeleteRevisionSQL = `CREATE TRIGGER application_adoption_delete_revision AFTER DELETE ON application_adoptions
	WHEN EXISTS (SELECT 1 FROM application_session WHERE id = 1)
	BEGIN UPDATE application_session SET revision = revision + 1 WHERE id = 1; END`

func applicationAdoptionSchemaV1() map[string]stateBackupSchemaObject {
	return map[string]stateBackupSchemaObject{
		"application_adoptions":                    {kind: "table", table: "application_adoptions", extension: "adoption", sql: applicationAdoptionsSQL},
		"sqlite_autoindex_application_adoptions_1": {kind: "index", table: "application_adoptions", extension: "adoption"},
		"application_adoption_insert_revision":     {kind: "trigger", table: "application_adoptions", extension: "adoption", sql: applicationAdoptionInsertRevisionSQL},
		"application_adoption_update_revision":     {kind: "trigger", table: "application_adoptions", extension: "adoption", sql: applicationAdoptionUpdateRevisionSQL},
		"application_adoption_delete_revision":     {kind: "trigger", table: "application_adoptions", extension: "adoption", sql: applicationAdoptionDeleteRevisionSQL},
	}
}

// The extension is optional in schema 1, but any part requires the complete
// table, foreign key and revision triggers. Reads never create or repair it.
func applicationAdoptionTablesExist(ctx context.Context, queryer stateQueryer) (bool, error) {
	expected := applicationAdoptionSchemaV1()
	rows, err := queryer.QueryContext(ctx, `SELECT type, name, tbl_name, rootpage, sql FROM main.sqlite_schema
		WHERE tbl_name = 'application_adoptions' OR name IN ('application_adoptions',
		'sqlite_autoindex_application_adoptions_1', 'application_adoption_insert_revision',
		'application_adoption_update_revision', 'application_adoption_delete_revision')`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		found = true
		var kind, name, table string
		var rootPage sql.NullInt64
		var definition sql.NullString
		if err := rows.Scan(&kind, &name, &table, &rootPage, &definition); err != nil {
			return false, err
		}
		want, known := expected[name]
		if !known || kind != want.kind || table != want.table || definition.Valid != (want.sql != "") || normalizeStateBackupSchemaSQL(definition.String) != normalizeStateBackupSchemaSQL(want.sql) {
			return false, fmt.Errorf("unsupported application adoption schema object %q", name)
		}
		if kind == "trigger" {
			if rootPage.Int64 != 0 {
				return false, errors.New("application adoption trigger has an invalid root page")
			}
		} else if !rootPage.Valid || rootPage.Int64 <= 1 || rootPage.Int64 > stateDatabaseMaxPageCount {
			return false, errors.New("application adoption schema object has an invalid root page")
		}
		delete(expected, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return false, err
	}
	if found && len(expected) != 0 {
		return false, errors.New("application adoption schema is incomplete")
	}
	return found, nil
}

func createApplicationAdoptionTables(ctx context.Context, tx stateTransaction) error {
	exists, err := applicationAdoptionTablesExist(ctx, tx)
	if err != nil || exists {
		return err
	}
	for _, statement := range []string{applicationAdoptionsSQL, applicationAdoptionInsertRevisionSQL, applicationAdoptionUpdateRevisionSQL, applicationAdoptionDeleteRevisionSQL} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create application adoption extension: %w", err)
		}
	}
	return nil
}

type storedApplicationAdoption struct {
	compositorID string
	observedAt   string
}

type applicationAdoptionWrite struct {
	contextID  ContextID
	observedAt string
}

// Validate every row, including evidence from an older compositor. The tag is
// independent of application_session so a legacy binary resetting that singleton
// cannot make old observations apply to a new compositor lifetime.
func loadStoredApplicationAdoptions(ctx context.Context, queryer stateQueryer, compositorID string) (map[ContextID]storedApplicationAdoption, []ApplicationAdoption, error) {
	stored := make(map[ContextID]storedApplicationAdoption)
	var current []ApplicationAdoption
	exists, err := applicationAdoptionTablesExist(ctx, queryer)
	if err != nil || !exists {
		return stored, current, err
	}
	rows, err := queryer.QueryContext(ctx, "SELECT context_id, compositor_id, observed_at FROM application_adoptions ORDER BY context_id")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var adoption ApplicationAdoption
		var row storedApplicationAdoption
		if err := rows.Scan(&adoption.ContextID, &row.compositorID, &row.observedAt); err != nil {
			return nil, nil, fmt.Errorf("scan application adoption: %w", err)
		}
		if compositorID == "" {
			return nil, nil, errors.New("application adoption has no application session")
		}
		adoption.ObservedAt, err = time.Parse(time.RFC3339Nano, row.observedAt)
		if err != nil {
			return nil, nil, fmt.Errorf("decode application adoption time for %s: %w", adoption.ContextID, err)
		}
		adoption.ObservedAt = adoption.ObservedAt.UTC()
		value := ApplicationSessionState{Version: ApplicationSessionSchemaVersion, CompositorID: row.compositorID, Attempts: []ApplicationLaunchAttempt{}, Adoptions: []ApplicationAdoption{adoption}}
		if err := value.Validate(); err != nil {
			return nil, nil, fmt.Errorf("validate application adoption for %s: %w", adoption.ContextID, err)
		}
		stored[adoption.ContextID] = row
		if row.compositorID == compositorID {
			current = append(current, adoption)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	for id := range stored {
		if err := requireStoredApplicationContext(ctx, queryer, id); err != nil {
			return nil, nil, fmt.Errorf("validate application adoption context: %w", err)
		}
	}
	return stored, current, nil
}

func applyApplicationAdoptionDeltaTx(ctx context.Context, tx stateTransaction, delta applicationSessionDelta) error {
	if len(delta.adoptionUpserts) != 0 {
		if err := createApplicationAdoptionTables(ctx, tx); err != nil {
			return err
		}
	}
	for _, write := range delta.adoptionUpserts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO application_adoptions (context_id, compositor_id, observed_at) VALUES (?, ?, ?)
			ON CONFLICT(context_id) DO UPDATE SET compositor_id = excluded.compositor_id, observed_at = excluded.observed_at`, write.contextID, delta.compositorID, write.observedAt); err != nil {
			return fmt.Errorf("store application adoption for %s: %w", write.contextID, err)
		}
	}
	for _, id := range delta.adoptionDeletes {
		if _, err := tx.ExecContext(ctx, "DELETE FROM application_adoptions WHERE context_id = ?", id); err != nil {
			return fmt.Errorf("remove application adoption for %s: %w", id, err)
		}
	}
	return nil
}
