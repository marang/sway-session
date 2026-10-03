package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func migrationTestWriteRow(t *testing.T, root string, version int, payload []byte) {
	t.Helper()
	database, err := openStateDatabase(t.Context(), root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.db.ExecContext(t.Context(), `
		INSERT INTO layout_state (id, encoding_version, payload) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET encoding_version = excluded.encoding_version, payload = excluded.payload`, version, payload); err != nil {
		t.Fatal(err)
	}
}

func migrationTestReadRow(t *testing.T, root string) (int, []byte) {
	t.Helper()
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var version int
	var payload []byte
	if err := database.db.QueryRowContext(t.Context(), "SELECT encoding_version, payload FROM layout_state WHERE id = 1").Scan(&version, &payload); err != nil {
		t.Fatal(err)
	}
	var schemaVersion int
	if err := database.db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&schemaVersion); err != nil {
		t.Fatal(err)
	}
	if schemaVersion != 1 {
		t.Fatalf("layout migration changed the physical database schema: %d", schemaVersion)
	}
	return version, payload
}

func TestLayoutStoreReadsV1WithoutRewritingAndSavesV2(t *testing.T) {
	for _, old := range []LayoutSnapshot{
		migrationTestLayout(1),
		{Version: 1, Workspaces: []WorkspaceLayout{}},
	} {
		t.Run(map[bool]string{true: "empty", false: "tree"}[len(old.Workspaces) == 0], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			payload := migrationTestPayload(t, old)
			migrationTestWriteRow(t, root, 1, payload)
			before := backupTestRead(t, filepath.Join(root, StateDatabaseFilename))
			var loaded LayoutSnapshot
			if err := LayoutStoreFor(root).LoadIntoContext(t.Context(), &loaded); err != nil {
				t.Fatal(err)
			}
			want := old
			want.Version = 2
			if !reflect.DeepEqual(loaded, want) {
				t.Fatalf("load changed the saved tree: got %+v, want %+v", loaded, want)
			}
			version, stored := migrationTestReadRow(t, root)
			if version != 1 || !bytes.Equal(stored, payload) || !bytes.Equal(before, backupTestRead(t, filepath.Join(root, StateDatabaseFilename))) {
				t.Fatal("reading v1 rewrote the database")
			}
			if err := LayoutStoreFor(root).Save(loaded); err != nil {
				t.Fatal(err)
			}
			version, stored = migrationTestReadRow(t, root)
			if version != 2 || !bytes.Equal(stored, migrationTestPayload(t, want)) {
				t.Fatalf("save did not write v2: version=%d payload=%s", version, stored)
			}
		})
	}
}

func TestLayoutStoreV2ScratchpadRoundtrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	want := migrationTestScratchpadLayout()
	if err := LayoutStoreFor(root).Save(want); err != nil {
		t.Fatal(err)
	}
	var got LayoutSnapshot
	if err := LayoutStoreFor(root).LoadInto(&got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("scratchpad roundtrip = %+v, err=%v", got, err)
	}
	version, payload := migrationTestReadRow(t, root)
	if version != 2 || !bytes.Equal(payload, migrationTestPayload(t, want)) {
		t.Fatalf("stored scratchpad layout is not v2: %d %s", version, payload)
	}
}

func TestLayoutStoreRejectsInvalidVersionsWithoutMutation(t *testing.T) {
	for _, test := range migrationInvalidCases() {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			payload := []byte(test.payload)
			migrationTestWriteRow(t, root, test.encodingVersion, payload)
			before := backupTestRead(t, filepath.Join(root, StateDatabaseFilename))
			got := migrationTestScratchpadLayout()
			want := migrationTestScratchpadLayout()
			err := LayoutStoreFor(root).LoadInto(&got)
			assertMigrationError(t, test, err, "layout")
			version, stored := migrationTestReadRow(t, root)
			if !reflect.DeepEqual(got, want) || version != test.encodingVersion || !bytes.Equal(stored, payload) || !bytes.Equal(before, backupTestRead(t, filepath.Join(root, StateDatabaseFilename))) {
				t.Fatal("failed load mutated the target or database")
			}
		})
	}
}

func TestLayoutLegacyMigrationUpgradesV1AndPreservesV2(t *testing.T) {
	for _, source := range []LayoutSnapshot{
		migrationTestLayout(1),
		{Version: 1, Workspaces: []WorkspaceLayout{}},
		migrationTestScratchpadLayout(),
	} {
		name := "v1 tree"
		if len(source.Workspaces) == 0 {
			name = "v1 empty"
		} else if source.Version == 2 {
			name = "v2 scratchpad"
		}
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, legacyLayoutFilename)
			payload := migrationTestPayload(t, source)
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := MigrateLegacyState(t.Context(), root)
			if err != nil || !result.Migrated || !result.Layout {
				t.Fatalf("legacy layout migration = %+v, err=%v", result, err)
			}
			want := source
			want.Version = 2
			var got LayoutSnapshot
			if err := LayoutStoreFor(root).LoadInto(&got); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("legacy layout changed: %+v, err=%v", got, err)
			}
			version, stored := migrationTestReadRow(t, root)
			if version != 2 || !bytes.Equal(stored, migrationTestPayload(t, want)) || !bytes.Equal(backupTestRead(t, path), payload) {
				t.Fatal("legacy migration changed the source or failed to write current data")
			}
			again, err := MigrateLegacyState(t.Context(), root)
			if err != nil || again.Migrated {
				t.Fatalf("legacy migration is not idempotent: %+v, err=%v", again, err)
			}
		})
	}
}

func TestLayoutLegacyMigrationRejectsInvalidDocumentsUnchanged(t *testing.T) {
	for _, test := range migrationInvalidCases() {
		// JSON documents have no separate encoding_version to mismatch.
		if test.encodingVersion == 0 || test.name == "unknown encoding" || test.name == "v1 encoding v2 payload" || test.name == "v2 encoding v1 payload" {
			continue
		}
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, legacyLayoutFilename)
			payload := []byte(test.payload)
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := MigrateLegacyState(t.Context(), root)
			assertMigrationError(t, test, err, "legacy layout")
			if !bytes.Equal(payload, backupTestRead(t, path)) {
				t.Fatal("rejected migration changed legacy source")
			}
			if _, err := os.Stat(filepath.Join(root, StateDatabaseFilename)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected legacy layout initialized the database: %v", err)
			}
		})
	}
}

func TestLayoutBackupValidatesV1AndV2WithoutMigration(t *testing.T) {
	for _, source := range []LayoutSnapshot{
		migrationTestLayout(1),
		{Version: 1, Workspaces: []WorkspaceLayout{}},
		migrationTestScratchpadLayout(),
	} {
		name := "v1 tree"
		if len(source.Workspaces) == 0 {
			name = "v1 empty"
		} else if source.Version == 2 {
			name = "v2 scratchpad"
		}
		t.Run(name, func(t *testing.T) {
			root, output := backupTestRoot(t), backupTestOutput(t)
			payload := migrationTestPayload(t, source)
			migrationTestWriteRow(t, root, source.Version, payload)
			if _, err := BackupState(t.Context(), root, output); err != nil {
				t.Fatal(err)
			}
			before := backupTestRead(t, output)
			info, err := os.Stat(output)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ValidateStateBackup(t.Context(), output); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(output)
			if err != nil || !bytes.Equal(before, backupTestRead(t, output)) || !after.ModTime().Equal(info.ModTime()) || after.Mode().Perm() != 0o600 {
				t.Fatalf("validation changed the backup: %v", err)
			}
			entries, err := os.ReadDir(filepath.Dir(output))
			if err != nil || len(entries) != 1 {
				t.Fatalf("validation left sidecars: %v, err=%v", entries, err)
			}
			readRoot := filepath.Join(t.TempDir(), "restored")
			if err := os.Mkdir(readRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(readRoot, StateDatabaseFilename), before, 0o600); err != nil {
				t.Fatal(err)
			}
			version, stored := migrationTestReadRow(t, readRoot)
			if version != source.Version || !bytes.Equal(stored, payload) {
				t.Fatal("backup rewrote the stored layout")
			}
			var got LayoutSnapshot
			want := source
			want.Version = 2
			if err := LayoutStoreFor(readRoot).LoadInto(&got); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("backed-up layout cannot be read: %+v, err=%v", got, err)
			}
		})
	}
}

func TestLayoutBackupRejectsInvalidVersionsUnchanged(t *testing.T) {
	for _, test := range migrationInvalidCases() {
		t.Run(test.name, func(t *testing.T) {
			root := backupTestRoot(t)
			migrationTestWriteRow(t, root, test.encodingVersion, []byte(test.payload))
			path := filepath.Join(root, StateDatabaseFilename)
			before := backupTestRead(t, path)
			_, err := ValidateStateBackup(t.Context(), path)
			assertMigrationError(t, test, err, "state backup layout")
			if !bytes.Equal(before, backupTestRead(t, path)) {
				t.Fatal("rejected backup validation changed the image")
			}
			output := backupTestOutput(t)
			if _, err := BackupState(t.Context(), root, output); err == nil {
				t.Fatal("backup creation accepted invalid layout")
			}
			entries, err := os.ReadDir(filepath.Dir(output))
			if err != nil || len(entries) != 0 {
				t.Fatalf("invalid backup left output: %v, err=%v", entries, err)
			}
		})
	}
}
