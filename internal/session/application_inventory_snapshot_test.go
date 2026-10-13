package session

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestApplicationInventorySnapshotMissingStateCreatesNothing(t *testing.T) {
	for _, existingRoot := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing root", true: "missing database"}[existingRoot], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if existingRoot {
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := ReadApplicationInventorySnapshotContext(t.Context(), root)
			if err != nil || !reflect.DeepEqual(snapshot, emptyApplicationInventorySnapshot()) {
				t.Fatalf("missing state snapshot = %+v, %v", snapshot, err)
			}
			if err := snapshot.Registry.Validate(); err != nil {
				t.Fatal(err)
			}
			if existingRoot {
				entries, err := os.ReadDir(root)
				if err != nil || len(entries) != 0 {
					t.Fatalf("read created state metadata: %v, %v", entries, err)
				}
			} else if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("read created root: %v", err)
			}
		})
	}
}

func TestApplicationInventorySnapshotAbsentOptionalDocumentsIsReadOnly(t *testing.T) {
	for _, registryPresent := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent registry", true: "registered application"}[registryPresent], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			registry := emptyRegistry()
			if registryPresent {
				registry = applicationInventoryTestRegistry("98")
				if err := RegistryStoreFor(root).Save(registry); err != nil {
					t.Fatal(err)
				}
			} else if err := initializeStateDatabase(t.Context(), root); err != nil {
				t.Fatal(err)
			}
			// A release predating the access gate must remain readable without
			// publishing a gate, optional journal, or document on its behalf.
			if err := os.Remove(filepath.Join(root, stateAccessFilename)); err != nil {
				t.Fatal(err)
			}
			before := backupTestRead(t, filepath.Join(root, StateDatabaseFilename))
			entriesBefore, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := ReadApplicationInventorySnapshotContext(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			want := emptyApplicationInventorySnapshot()
			want.Registry = registry
			if !reflect.DeepEqual(snapshot, want) {
				t.Fatalf("optional evidence was invented: %+v", snapshot)
			}
			if !bytes.Equal(before, backupTestRead(t, filepath.Join(root, StateDatabaseFilename))) {
				t.Fatal("inventory changed the database")
			}
			entriesAfter, err := os.ReadDir(root)
			if err != nil || !reflect.DeepEqual(entriesBefore, entriesAfter) {
				t.Fatalf("inventory created metadata: before=%v after=%v err=%v", entriesBefore, entriesAfter, err)
			}
		})
	}
}

func applicationInventoryTestRegistry(label string) Registry {
	app := desktopApplicationContext("org.example.Editor.desktop", "org.example.Editor")
	app.ID, app.Label = testContextID, label
	return Registry{Version: ContextsSchemaVersion, Contexts: []Context{app}}
}

func applicationInventoryTestFixture(t *testing.T) (string, ApplicationInventorySnapshot) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	registry := applicationInventoryTestRegistry("98")
	layout := placementSnapshot("98", testContextID)
	outcome := restoreTestOutcome("explicit", testContextID)
	application := ApplicationSessionState{
		Version: ApplicationSessionSchemaVersion, CompositorID: strings.Repeat("a", 64),
		Attempts:  []ApplicationLaunchAttempt{{ContextID: testContextID, StartedAt: outcome.StartedAt}},
		Adoptions: []ApplicationAdoption{{ContextID: testContextID, ObservedAt: outcome.StartedAt}},
	}
	if err := RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	if err := LayoutStoreFor(root).Save(layout); err != nil {
		t.Fatal(err)
	}
	if err := ApplicationSessionStoreFor(root).Save(application); err != nil {
		t.Fatal(err)
	}
	if err := RestoreReportStoreFor(root).BeginExplicitContext(t.Context(), []RestoreOutcome{outcome}); err != nil {
		t.Fatal(err)
	}
	return root, ApplicationInventorySnapshot{
		Registry: registry, Layout: &layout, ApplicationSession: &application,
		RestoreReport: RestoreReport{Outcomes: []RestoreOutcome{outcome}},
	}
}

func TestApplicationInventorySnapshotReadsAllDocuments(t *testing.T) {
	root, want := applicationInventoryTestFixture(t)
	snapshot, err := ReadApplicationInventorySnapshotContext(t.Context(), root)
	if err != nil || !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("snapshot = %+v, %v; want %+v", snapshot, err, want)
	}
	// The reader must release its database access before external observers run.
	directory := accessTestDirectory(t, root)
	gate, err := acquireStateAccessAt(t.Context(), directory, true)
	if err != nil {
		t.Fatalf("snapshot retained its state access lock: %v", err)
	}
	defer gate.Close()
}

func TestApplicationInventorySnapshotRejectsMalformedOptionalEvidence(t *testing.T) {
	cases := map[string][]string{
		"layout payload":       {`UPDATE layout_state SET payload=CAST('{}' AS BLOB)`},
		"layout version":       {"UPDATE layout_state SET encoding_version=999"},
		"session compositor":   {"UPDATE application_session SET compositor_id='invalid'"},
		"session attempt time": {"UPDATE application_launch_attempts SET started_at='invalid'"},
		"adoption time":        {"UPDATE application_adoptions SET observed_at='invalid'"},
		"adoption no session":  {"DELETE FROM application_session"},
		"adoption incomplete":  {"DROP TRIGGER application_adoption_insert_revision"},
		"journal incomplete":   {"DROP TABLE restore_report_meta"},
		"journal payload":      {`UPDATE restore_outcomes SET payload=CAST('{}' AS BLOB)`},
		"journal private data": {`UPDATE restore_outcomes SET payload=CAST(json_set(CAST(payload AS TEXT), '$.reason', 'private launcher output') AS BLOB)`},
		"journal wrong key":    {"UPDATE restore_outcomes SET attempt_id='" + restoreTestOtherAttempt + "'"},
	}
	for name, statements := range cases {
		t.Run(name, func(t *testing.T) {
			root, _ := applicationInventoryTestFixture(t)
			backupTestMutate(t, root, statements...)
			before := backupTestRead(t, filepath.Join(root, StateDatabaseFilename))
			snapshot, err := ReadApplicationInventorySnapshotContext(t.Context(), root)
			if err == nil {
				t.Fatal("malformed evidence accepted")
			}
			if !reflect.DeepEqual(snapshot, emptyApplicationInventorySnapshot()) {
				t.Fatalf("rejected snapshot exposed partial evidence: %+v", snapshot)
			}
			if name == "layout version" {
				var versionError *UnsupportedVersionError
				if !errors.As(err, &versionError) {
					t.Fatalf("version error lost classification: %v", err)
				}
			}
			if !bytes.Equal(before, backupTestRead(t, filepath.Join(root, StateDatabaseFilename))) {
				t.Fatal("rejected evidence was repaired")
			}
		})
	}
}

func TestApplicationInventorySnapshotRejectsLegacyStateWithoutMigration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, legacyContextsFilename)
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadApplicationInventorySnapshotContext(t.Context(), root)
	var legacy *LegacyStateError
	if !errors.As(err, &legacy) {
		t.Fatalf("legacy error lost classification: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != legacyContextsFilename {
		t.Fatalf("legacy read created metadata: %v %v", entries, err)
	}
}

func TestApplicationInventorySnapshotRejectsUninitializedAndCorruptDatabase(t *testing.T) {
	for _, payload := range [][]byte{nil, []byte("invalid SQLite database")} {
		t.Run(map[bool]string{true: "uninitialized", false: "corrupt"}[len(payload) == 0], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, StateDatabaseFilename)
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := ReadApplicationInventorySnapshotContext(t.Context(), root)
			if err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid database treated as empty inventory: %v", err)
			}
			if len(payload) == 0 && !errors.Is(err, ErrUninitializedStateDatabase) {
				t.Fatalf("uninitialized error lost classification: %v", err)
			}
			if !bytes.Equal(payload, backupTestRead(t, path)) {
				t.Fatal("reader repaired or initialized invalid database")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 || entries[0].Name() != StateDatabaseFilename {
				t.Fatalf("invalid database read created metadata: %v, %v", entries, err)
			}
		})
	}
}

func TestApplicationInventorySnapshotHonorsCancellation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ReadApplicationInventorySnapshotContext(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot: %v", err)
	}
	if _, err := ReadApplicationInventorySnapshotContext(nil, root); err == nil {
		t.Fatal("nil context accepted")
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid request created root: %v", err)
	}
	root, _ = applicationInventoryTestFixture(t)
	ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := ReadApplicationInventorySnapshotContext(ctx, root); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired snapshot: %v", err)
	}
}

func TestApplicationInventorySnapshotCancellationBetweenDocuments(t *testing.T) {
	root, _ := applicationInventoryTestFixture(t)
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	queryer := &applicationInventoryInterleavedQueryer{stateQueryer: tx, beforeLayout: cancel}
	if _, err := loadApplicationInventorySnapshotQuery(ctx, queryer); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation between document reads lost its cause: %v", err)
	}
	if queryer.beforeLayout != nil {
		t.Fatal("read did not reach the inter-document cancellation")
	}
}

func TestApplicationInventorySnapshotReadsLegacyLayoutEncodingWithoutRewriting(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	legacy := migrationTestLayout(1)
	payload := migrationTestPayload(t, legacy)
	migrationTestWriteRow(t, root, 1, payload)
	before := backupTestRead(t, filepath.Join(root, StateDatabaseFilename))
	snapshot, err := ReadApplicationInventorySnapshotContext(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Version = LayoutSchemaVersion
	if snapshot.Layout == nil || !reflect.DeepEqual(*snapshot.Layout, legacy) {
		t.Fatalf("legacy layout differs from the established store projection: %+v", snapshot.Layout)
	}
	version, stored := migrationTestReadRow(t, root)
	if version != 1 || !bytes.Equal(stored, payload) || !bytes.Equal(before, backupTestRead(t, filepath.Join(root, StateDatabaseFilename))) {
		t.Fatal("inventory rewrote the legacy layout encoding")
	}
}

func TestApplicationInventorySnapshotPreservesNativeOwnerPolicy(t *testing.T) {
	for _, unsafe := range []string{"public root", "public database", "database symlink", "public sidecar"} {
		t.Run(unsafe, func(t *testing.T) {
			root, _ := applicationInventoryTestFixture(t)
			switch unsafe {
			case "public root":
				if err := os.Chmod(root, 0o755); err != nil {
					t.Fatal(err)
				}
			case "public database":
				if err := os.Chmod(filepath.Join(root, StateDatabaseFilename), 0o644); err != nil {
					t.Fatal(err)
				}
			case "database symlink":
				path := filepath.Join(root, StateDatabaseFilename)
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".original", path); err != nil {
					t.Fatal(err)
				}
			case "public sidecar":
				if err := os.WriteFile(filepath.Join(root, StateDatabaseFilename+"-wal"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ReadApplicationInventorySnapshotContext(t.Context(), root); err == nil {
				t.Fatal("unsafe owner-only boundary accepted")
			}
		})
	}
	root, _ := applicationInventoryTestFixture(t)
	var object unix.Stat_t
	if err := unix.Lstat(filepath.Join(root, StateDatabaseFilename), &object); err != nil || object.Uid != uint32(os.Geteuid()) || object.Mode&0o777 != 0o600 {
		t.Fatalf("fixture is not native owner-only SQLite state: %+v, %v", object, err)
	}
}

// The hook runs between completed SQL calls, after registry rows have closed.
// It creates a deterministic writer interleaving without a driver callback or
// a global seam that could hold SQLite or database/sql locks while reentering.
type applicationInventoryInterleavedQueryer struct {
	stateQueryer
	beforeLayout func()
}

func (queryer *applicationInventoryInterleavedQueryer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if query == "SELECT length(payload) FROM layout_state WHERE id = 1" && queryer.beforeLayout != nil {
		beforeLayout := queryer.beforeLayout
		queryer.beforeLayout = nil
		beforeLayout()
	}
	return queryer.stateQueryer.QueryRowContext(ctx, query, args...)
}

func TestApplicationInventorySnapshotRemainsCoherentAcrossDurableWriter(t *testing.T) {
	root, before := applicationInventoryTestFixture(t)
	reader, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	writer, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tx, err := reader.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	after := before
	after.Registry = applicationInventoryTestRegistry("99")
	layout := placementSnapshot("99", testContextID)
	after.Layout = &layout
	outcome := restoreTestOutcome("explicit", testContextID)
	outcome.AttemptID = restoreTestOtherAttempt
	outcome.StartedAt, outcome.UpdatedAt = outcome.StartedAt.Add(time.Minute), outcome.UpdatedAt.Add(time.Minute)
	after.RestoreReport = RestoreReport{Outcomes: []RestoreOutcome{outcome}}
	application := ApplicationSessionState{
		Version: ApplicationSessionSchemaVersion, CompositorID: strings.Repeat("b", 64),
		Attempts:  []ApplicationLaunchAttempt{{ContextID: testContextID, StartedAt: outcome.StartedAt}},
		Adoptions: []ApplicationAdoption{{ContextID: testContextID, ObservedAt: outcome.StartedAt}},
	}
	after.ApplicationSession = &application
	queryer := &applicationInventoryInterleavedQueryer{stateQueryer: tx, beforeLayout: func() {
		writeTx, err := beginStateWrite(t.Context(), writer)
		if err != nil {
			t.Fatal(err)
		}
		defer writeTx.Rollback()
		contextPayload, err := marshalDatabasePayload("context row", after.Registry.Contexts[0])
		if err != nil {
			t.Fatal(err)
		}
		layoutPayload, err := marshalDatabasePayload("layout", layout)
		if err != nil {
			t.Fatal(err)
		}
		statements := []struct {
			sql  string
			args []any
		}{
			{"UPDATE contexts SET payload=? WHERE id=?", []any{contextPayload, testContextID}},
			{"UPDATE state_meta SET registry_revision=registry_revision+1 WHERE id=1", nil},
			{"UPDATE layout_state SET payload=? WHERE id=1", []any{layoutPayload}},
			{"UPDATE application_session SET compositor_id=?, revision=revision+1 WHERE id=1", []any{application.CompositorID}},
			{"UPDATE application_launch_attempts SET started_at=? WHERE context_id=?", []any{outcome.StartedAt.Format(time.RFC3339Nano), testContextID}},
			{"UPDATE application_adoptions SET compositor_id=?, observed_at=? WHERE context_id=?", []any{application.CompositorID, outcome.StartedAt.Format(time.RFC3339Nano), testContextID}},
		}
		for _, statement := range statements {
			if _, err := writeTx.ExecContext(t.Context(), statement.sql, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
		if err := saveRestoreOutcome(t.Context(), writeTx, outcome); err != nil {
			t.Fatal(err)
		}
		if err := commitStateWrite(t.Context(), writeTx); err != nil {
			t.Fatal(err)
		}
	}}
	snapshot, err := loadApplicationInventorySnapshotQuery(t.Context(), queryer)
	if err != nil || !reflect.DeepEqual(snapshot, before) {
		t.Fatalf("torn snapshot = %+v, %v; want original %+v", snapshot, err, before)
	}
	if queryer.beforeLayout != nil {
		t.Fatal("durable writer did not run between registry and optional reads")
	}
	if err := commitStateRead(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	snapshot, err = ReadApplicationInventorySnapshotContext(t.Context(), root)
	if err != nil || !reflect.DeepEqual(snapshot, after) {
		t.Fatalf("next snapshot = %+v, %v; want committed %+v", snapshot, err, after)
	}
}
