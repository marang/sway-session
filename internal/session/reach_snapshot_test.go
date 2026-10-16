package session

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func reachSnapshotFixture(t *testing.T) (string, ReachSnapshot) {
	t.Helper()
	root := backupTestRoot(t)
	store := WorkEnvironmentStoreFor(root)
	environment := environmentTestAdd(t, store, environmentTestCreate(t, store, "Development"), testContextID)
	var err error
	environment, err = store.SetPrimaryContext(t.Context(), environment.ID, environment.Revision, testContextID)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	activity := TerminalActivityState{Version: TerminalActivitySchemaVersion, Terminals: []TerminalActivity{{ContextID: testContextID, CreatedAt: &created}}}
	if err := TerminalActivityStoreFor(root).Save(activity); err != nil {
		t.Fatal(err)
	}
	inventory := emptyApplicationInventorySnapshot()
	inventory.Registry = validRegistry()
	return root, ReachSnapshot{Environments: []WorkEnvironment{environment}, Inventory: inventory, Activity: activity}
}

func TestReachSnapshotMissingStateAndOldSchemaNeverInitialize(t *testing.T) {
	for _, kind := range []string{"missing root", "missing database", "old schema"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if kind == "missing database" {
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if kind == "old schema" {
				if err := initializeStateDatabase(t.Context(), root); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(root, stateAccessFilename)); err != nil {
					t.Fatal(err)
				}
			}
			var before []byte
			var entriesBefore []os.DirEntry
			if kind == "old schema" {
				before = backupTestRead(t, filepath.Join(root, StateDatabaseFilename))
			}
			if kind != "missing root" {
				var err error
				entriesBefore, err = os.ReadDir(root)
				if err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := ReadReachSnapshotContext(t.Context(), root)
			if err != nil || !reflect.DeepEqual(snapshot, emptyReachSnapshot()) {
				t.Fatalf("empty snapshot = %+v %v", snapshot, err)
			}
			if err := snapshot.Inventory.Registry.Validate(); err != nil {
				t.Fatal(err)
			}
			if err := snapshot.Activity.Validate(); err != nil {
				t.Fatal(err)
			}
			selected, err := ReadReachEnvironmentSnapshotContext(t.Context(), root, EnvironmentID(testContextID))
			if !errors.Is(err, ErrWorkEnvironmentNotFound) || !reflect.DeepEqual(selected, emptyReachSnapshot()) {
				t.Fatalf("exact absent = %+v %v", selected, err)
			}
			if kind == "missing root" {
				if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("read created root: %v", err)
				}
			} else {
				entriesAfter, err := os.ReadDir(root)
				if err != nil || !reflect.DeepEqual(entriesAfter, entriesBefore) {
					t.Fatalf("read created metadata: %v %v", entriesAfter, err)
				}
			}
			if kind == "old schema" && !bytes.Equal(before, backupTestRead(t, filepath.Join(root, StateDatabaseFilename))) {
				t.Fatal("read changed old database")
			}
		})
	}
}

func TestReachSnapshotReadsAllEvidenceAndSelectedDefinition(t *testing.T) {
	root, want := reachSnapshotFixture(t)
	store := WorkEnvironmentStoreFor(root)
	other := environmentTestCreate(t, store, "Review")
	got, err := ReadReachSnapshotContext(t.Context(), root)
	wantAll := want
	wantAll.Environments = append(append([]WorkEnvironment{}, want.Environments...), other)
	if err != nil || !reflect.DeepEqual(got, wantAll) {
		t.Fatalf("all evidence = %+v %v; want %+v", got, err, wantAll)
	}
	got, err = ReadReachEnvironmentSnapshotContext(t.Context(), root, want.Environments[0].ID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("selected evidence = %+v %v; want %+v", got, err, want)
	}
	// Exclusive recovery can begin before any caller performs external probes.
	directory := accessTestDirectory(t, root)
	gate, err := acquireStateAccessAt(t.Context(), directory, true)
	if err != nil {
		t.Fatalf("snapshot retained recovery gate: %v", err)
	}
	if _, err := ReadReachSnapshotContext(t.Context(), root); !errors.Is(err, ErrStateDatabaseBusy) {
		t.Fatalf("read bypassed recovery gate: %v", err)
	}
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReachSnapshotContext(t.Context(), root); err != nil {
		t.Fatalf("read after recovery gate release: %v", err)
	}
}

type reachSnapshotInterleavedQueryer struct {
	stateQueryer
	beforeInventory func()
}

func (queryer *reachSnapshotInterleavedQueryer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if query == "SELECT registry_present, registry_revision FROM state_meta WHERE id = 1" && queryer.beforeInventory != nil {
		hook := queryer.beforeInventory
		queryer.beforeInventory = nil
		hook()
	}
	return queryer.stateQueryer.QueryRowContext(ctx, query, args...)
}

func TestReachSnapshotStaysCoherentAcrossDefinitionEditAndContextPurge(t *testing.T) {
	root, before := reachSnapshotFixture(t)
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
	after := emptyReachSnapshot()
	after.Environments = append([]WorkEnvironment{}, before.Environments...)
	after.Environments[0].Name = "Renamed"
	after.Environments[0].Revision++
	queryer := &reachSnapshotInterleavedQueryer{stateQueryer: tx, beforeInventory: func() {
		writeTx, err := beginStateWrite(t.Context(), writer)
		if err != nil {
			t.Fatal(err)
		}
		defer writeTx.Rollback()
		if _, err := writeTx.ExecContext(t.Context(), "UPDATE work_environments SET name = ?, revision = revision + 1 WHERE id = ?", after.Environments[0].Name, after.Environments[0].ID); err != nil {
			t.Fatal(err)
		}
		if _, err := writeTx.ExecContext(t.Context(), "DELETE FROM contexts WHERE id = ?", testContextID); err != nil {
			t.Fatal(err)
		}
		if _, err := writeTx.ExecContext(t.Context(), "UPDATE state_meta SET registry_revision = registry_revision + 1 WHERE id = 1"); err != nil {
			t.Fatal(err)
		}
		if err := commitStateWrite(t.Context(), writeTx); err != nil {
			t.Fatal(err)
		}
	}}
	got, err := loadReachSnapshotQuery(t.Context(), queryer, "")
	if err != nil || !reflect.DeepEqual(got, before) {
		t.Fatalf("torn snapshot = %+v %v; want %+v", got, err, before)
	}
	if queryer.beforeInventory != nil {
		t.Fatal("writer interleaving did not run")
	}
	if err := commitStateRead(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	got, err = ReadReachSnapshotContext(t.Context(), root)
	if err != nil || !reflect.DeepEqual(got, after) {
		t.Fatalf("next snapshot = %+v %v; want %+v", got, err, after)
	}
}

func TestReachSnapshotCancellationAndInvalidEvidenceExposeNoPartialState(t *testing.T) {
	root, _ := reachSnapshotFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ReadReachSnapshotContext(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read = %v", err)
	}
	//lint:ignore SA1012 Exercise the explicit nil-context rejection boundary.
	if _, err := ReadReachSnapshotContext(nil, root); err == nil {
		t.Fatal("accepted nil context")
	}
	if _, err := ReadReachEnvironmentSnapshotContext(t.Context(), root, "invalid"); err == nil {
		t.Fatal("accepted invalid exact ID")
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	queryer := &reachSnapshotInterleavedQueryer{stateQueryer: tx, beforeInventory: cancel}
	got, err := loadReachSnapshotQuery(ctx, queryer, "")
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, emptyReachSnapshot()) {
		t.Fatalf("cancel between documents = %+v %v", got, err)
	}
	_ = tx.Rollback()
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	backupTestMutate(t, root, "UPDATE terminal_activity SET created_at = 'invalid'")
	got, err = ReadReachSnapshotContext(t.Context(), root)
	if err == nil || !reflect.DeepEqual(got, emptyReachSnapshot()) {
		t.Fatalf("malformed late evidence exposed partial state: %+v %v", got, err)
	}
	directory := accessTestDirectory(t, root)
	gate, err := acquireStateAccessAt(t.Context(), directory, true)
	if err != nil {
		t.Fatalf("failed snapshot retained recovery gate: %v", err)
	}
	_ = gate.Close()
}
