package session

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareRegistryDeltaIncludesOnlyChangedRows(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	first := registryDeltaTestContext("00000000-0000-4000-8000-000000000001", "first", "first")
	changed := registryDeltaTestContext("00000000-0000-4000-8000-000000000002", "changed", "before")
	unchanged := registryDeltaTestContext("00000000-0000-4000-8000-000000000003", "unchanged", "unchanged")
	stored := Registry{
		Version:  ContextsSchemaVersion,
		Contexts: []Context{first, changed, unchanged},
	}
	if err := RegistryStoreFor(root).Save(stored); err != nil {
		t.Fatal(err)
	}

	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	_, revision, err := loadRegistrySnapshotDatabase(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	changed.Label = "after"
	appended := registryDeltaTestContext("00000000-0000-4000-8000-000000000004", "appended", "appended")
	candidate := Registry{
		Version:     ContextsSchemaVersion,
		Preferences: RegistryPreferences{DesktopIndicators: true},
		Contexts:    []Context{changed, unchanged, appended},
	}
	delta, err := prepareRegistryDelta(t.Context(), database, candidate, revision)
	if err != nil {
		t.Fatal(err)
	}

	upsertIDs := make([]ContextID, 0, len(delta.upserts))
	for _, write := range delta.upserts {
		upsertIDs = append(upsertIDs, write.id)
	}
	if want := []ContextID{changed.ID, appended.ID}; !reflect.DeepEqual(upsertIDs, want) {
		t.Fatalf("upsert IDs = %v, want %v", upsertIDs, want)
	}
	if len(delta.reorders) != 0 {
		t.Fatalf("stable removal and append produced reorder writes: %+v", delta.reorders)
	}
	if want := []ContextID{first.ID}; !reflect.DeepEqual(delta.deletes, want) {
		t.Fatalf("delete IDs = %v, want %v", delta.deletes, want)
	}
	if !delta.desktopIndicatorsChanged || !delta.desktopIndicators {
		t.Fatalf("preference delta was not retained: %+v", delta)
	}
}

func TestApplyRegistryDeltaChecksRevisionBeforeWrites(t *testing.T) {
	tx := &revisionConflictTransaction{}
	delta := registryDelta{
		expectedRevision:         7,
		desktopIndicators:        true,
		desktopIndicatorsChanged: true,
		upserts: []registryContextWrite{{
			id:      "00000000-0000-4000-8000-000000000001",
			ordinal: 1,
			payload: []byte(`{"version":1}`),
		}},
		reorders: []registryOrdinalWrite{{
			id:      "00000000-0000-4000-8000-000000000002",
			ordinal: 2,
		}},
		deletes: []ContextID{"00000000-0000-4000-8000-000000000003"},
	}

	err := applyRegistryDeltaTx(t.Context(), tx, delta)
	if !errors.Is(err, ErrRegistryConflict) {
		t.Fatalf("apply stale delta error = %v, want %v", err, ErrRegistryConflict)
	}
	if len(tx.statements) != 1 || !strings.HasPrefix(tx.statements[0], "UPDATE state_meta") {
		t.Fatalf("stale delta executed statements after revision CAS: %v", tx.statements)
	}
}

func registryDeltaTestContext(id ContextID, session, label string) Context {
	contextValue := validRegistry().Contexts[0]
	contextValue.ID = id
	contextValue.Label = label
	contextValue.Launcher.Session = session
	return contextValue
}

type revisionConflictTransaction struct {
	statements []string
}

func (transaction *revisionConflictTransaction) ExecContext(_ context.Context, query string, _ ...any) (sql.Result, error) {
	transaction.statements = append(transaction.statements, query)
	return fixedRowsAffected(0), nil
}

func (*revisionConflictTransaction) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	panic("applyRegistryDeltaTx must not query inside the write transaction")
}

func (*revisionConflictTransaction) QueryRowContext(context.Context, string, ...any) *sql.Row {
	panic("applyRegistryDeltaTx must not query inside the write transaction")
}

type fixedRowsAffected int64

func (fixedRowsAffected) LastInsertId() (int64, error) { return 0, nil }
func (result fixedRowsAffected) RowsAffected() (int64, error) {
	return int64(result), nil
}
