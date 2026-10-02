package session

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func backupTestMutate(t *testing.T, root string, statements ...string) {
	t.Helper()
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, statement := range statements {
		if _, err := database.db.Exec(statement); err != nil {
			t.Fatalf("mutate fixture: %v", err)
		}
	}
}

func backupTestFullRoot(t *testing.T) string {
	t.Helper()
	root := backupTestRoot(t)
	registry := validRegistry()
	app := desktopApplicationContext("org.example.Editor.desktop", "org.example.Editor")
	app.ID = restoreTestOtherRun
	registry.Contexts = append(registry.Contexts, app)
	if err := RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	if err := LayoutStoreFor(root).Save(LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	activity := TerminalActivityState{Version: TerminalActivitySchemaVersion, Terminals: []TerminalActivity{{ContextID: testContextID, CreatedAt: &now}}}
	if err := (TerminalActivityStore{root: root}).Save(activity); err != nil {
		t.Fatal(err)
	}
	application := ApplicationSessionState{Version: ApplicationSessionSchemaVersion, CompositorID: strings.Repeat("a", 64), Attempts: []ApplicationLaunchAttempt{{ContextID: app.ID, StartedAt: now}}}
	if err := ApplicationSessionStoreFor(root).Save(application); err != nil {
		t.Fatal(err)
	}
	if err := RestoreReportStoreFor(root).BeginExplicitContext(t.Context(), []RestoreOutcome{restoreTestOutcome("explicit", testContextID)}); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestValidateStateBackupAllDocumentsReadOnly(t *testing.T) {
	root, output := backupTestFullRoot(t), backupTestOutput(t)
	if _, err := BackupState(t.Context(), root, output); err != nil {
		t.Fatal(err)
	}
	before := backupTestRead(t, output)
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	file, summary, err := openValidatedStateBackup(t.Context(), output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if summary.Path != output || summary.ContextCount != 2 || summary.SizeBytes != int64(len(before)) {
		t.Fatalf("incorrect summary: %+v", summary)
	}
	// Recovery copies the pinned descriptor, not a reopened pathname.
	pinned, err := io.ReadAll(file)
	if err != nil || !bytes.Equal(pinned, before) {
		t.Fatalf("returned descriptor was not at offset zero: %v", err)
	}
	after, err := os.Stat(output)
	if err != nil || !after.ModTime().Equal(info.ModTime()) || !bytes.Equal(before, backupTestRead(t, output)) {
		t.Fatal("validation modified the image")
	}
	entries, err := os.ReadDir(filepath.Dir(output))
	if err != nil || len(entries) != 1 {
		t.Fatalf("validation created sidecars: %v %v", entries, err)
	}
}

func TestValidateStateBackupRejectsInvalidPersistedDocumentsUnchanged(t *testing.T) {
	cases := map[string][]string{
		"unknown table":                 {"CREATE TABLE future_document(payload BLOB) STRICT"},
		"missing table":                 {"DROP TABLE layout_state"},
		"newer schema":                  {"PRAGMA user_version=2"},
		"uninitialized schema":          {"PRAGMA user_version=0"},
		"wrong application":             {"PRAGMA application_id=1"},
		"context encoding":              {"UPDATE contexts SET encoding_version=999"},
		"context invalid":               {`UPDATE contexts SET payload=CAST('{}' AS BLOB)`},
		"context unknown field":         {`UPDATE contexts SET payload=CAST(json_set(CAST(payload AS TEXT), '$.unknown', 1) AS BLOB)`},
		"context trailing JSON":         {`UPDATE contexts SET payload=CAST(CAST(payload AS TEXT) || '{}' AS BLOB)`},
		"layout encoding":               {"UPDATE layout_state SET encoding_version=999"},
		"layout version":                {`UPDATE layout_state SET payload=CAST('{"version":999,"workspaces":[]}' AS BLOB)`},
		"layout invalid":                {`UPDATE layout_state SET payload=CAST('{}' AS BLOB)`},
		"layout unknown field":          {`UPDATE layout_state SET payload=CAST('{"version":1,"workspaces":[],"unknown":true}' AS BLOB)`},
		"activity invalid time":         {"UPDATE terminal_activity SET created_at='not-a-time'"},
		"activity wrong kind":           {"UPDATE terminal_activity SET context_id='" + restoreTestOtherRun + "'"},
		"application invalid ID":        {"UPDATE application_session SET compositor_id='bad'"},
		"application negative revision": {"UPDATE application_session SET revision=-1"},
		"application invalid time":      {"UPDATE application_launch_attempts SET started_at='bad'"},
		"application zero time":         {"UPDATE application_launch_attempts SET started_at='0001-01-01T00:00:00Z'"},
		"application wrong kind":        {"UPDATE application_launch_attempts SET context_id='" + string(testContextID) + "'"},
		"application missing session":   {"DELETE FROM application_session"},
		"hidden registry contexts":      {"UPDATE state_meta SET registry_present=0"},
		"missing preferences":           {"DELETE FROM registry_preferences"},
		"negative registry revision":    {"UPDATE state_meta SET registry_revision=-1"},
		"partial restore journal":       {"DROP TABLE restore_report_meta"},
		"restore invalid payload":       {`UPDATE restore_outcomes SET payload=CAST('{}' AS BLOB)`},
		"restore unknown field":         {`UPDATE restore_outcomes SET payload=CAST(json_set(CAST(payload AS TEXT), '$.unknown', 1) AS BLOB)`},
		"restore mismatched key":        {"UPDATE restore_outcomes SET attempt_id='" + restoreTestOtherAttempt + "'"},
		"restore invalid summary":       {"INSERT INTO restore_report_meta(id,automatic_run_id) VALUES(1,'invalid')"},
		"foreign key":                   {"PRAGMA foreign_keys=OFF", "UPDATE terminal_activity SET context_id='" + restoreTestRun + "'"},
	}
	for name, mutations := range cases {
		t.Run(name, func(t *testing.T) {
			root := backupTestFullRoot(t)
			backupTestMutate(t, root, mutations...)
			path := filepath.Join(root, StateDatabaseFilename)
			before := backupTestRead(t, path)
			if _, err := ValidateStateBackup(t.Context(), path); err == nil {
				t.Fatal("invalid persisted state passed validation")
			} else if name == "newer schema" || name == "context encoding" || name == "layout encoding" {
				var versionError *UnsupportedVersionError
				if !errors.As(err, &versionError) {
					t.Fatalf("schema error lost type: %v", err)
				}
			}
			if !bytes.Equal(before, backupTestRead(t, path)) {
				t.Fatal("rejected state was changed")
			}
		})
	}
}

func TestValidateStateBackupRejectsUnsafeFilesAndSidecars(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo", "directory", "public file", "special mode", "public parent", "wal", "shm", "journal", "empty", "oversized", "corrupt", "page size", "page budget", "database encoding", "btree corruption"} {
		t.Run(kind, func(t *testing.T) {
			root, output := backupTestRoot(t), backupTestOutput(t)
			if _, err := BackupState(t.Context(), root, output); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink", "fifo", "directory":
				if err := os.Remove(output); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink" {
					err = os.Symlink(filepath.Join(root, StateDatabaseFilename), output)
				} else if kind == "fifo" {
					err = unix.Mkfifo(output, 0o600)
				} else {
					err = os.Mkdir(output, 0o700)
				}
			case "hardlink":
				err = os.Link(output, output+".link")
			case "public file":
				err = os.Chmod(output, 0o644)
			case "special mode":
				err = os.Chmod(output, 0o600|os.ModeSetuid)
			case "public parent":
				err = os.Chmod(filepath.Dir(output), 0o755)
			case "wal", "shm", "journal":
				err = os.WriteFile(output+"-"+kind, nil, 0o600)
			case "empty":
				err = os.Truncate(output, 0)
			case "oversized":
				err = os.Truncate(output, maxStateDatabaseBytes+1)
			case "corrupt":
				err = os.WriteFile(output, []byte("not a sqlite database"), 0o600)
			case "page size", "page budget", "database encoding", "btree corruption":
				data := backupTestRead(t, output)
				switch kind {
				case "page size":
					binary.BigEndian.PutUint16(data[16:18], 8192)
				case "page budget":
					binary.BigEndian.PutUint32(data[28:32], stateDatabaseMaxPageCount+1)
				case "database encoding":
					binary.BigEndian.PutUint32(data[56:60], 2)
				case "btree corruption":
					data[stateDatabasePageSize*2] = 0xff
				}
				err = os.WriteFile(output, data, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ValidateStateBackup(t.Context(), output); err == nil {
				t.Fatal("unsafe state backup accepted")
			}
		})
	}
}

func TestValidateStateBackupSupportsOptionalDocuments(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "no registry", true: "empty registry"}[present], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			var err error
			if present {
				err = RegistryStoreFor(root).Save(emptyRegistry())
			} else {
				err = initializeStateDatabase(t.Context(), root)
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := BackupState(t.Context(), root, backupTestOutput(t))
			if err != nil || result.ContextCount != 0 {
				t.Fatalf("empty initialized state: %+v %v", result, err)
			}
		})
	}
}

func TestValidateStateBackupCanceledDoesNotModifyInput(t *testing.T) {
	root := backupTestRoot(t)
	path := filepath.Join(root, StateDatabaseFilename)
	before := backupTestRead(t, path)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ValidateStateBackup(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
	if !bytes.Equal(before, backupTestRead(t, path)) {
		t.Fatal("canceled validation changed input")
	}
}

func TestValidateStateBackupRejectsUnsupportedSchema(t *testing.T) {
	cases := map[string][]string{
		"unknown trigger": {`CREATE TRIGGER destroy_contexts AFTER UPDATE ON contexts BEGIN DELETE FROM contexts; END`},
		"known trigger changed body": {
			`DROP TRIGGER application_attempt_insert_revision`,
			`CREATE TRIGGER application_attempt_insert_revision AFTER INSERT ON application_launch_attempts
			WHEN EXISTS (SELECT 1 FROM application_session WHERE id = 1)
			BEGIN DELETE FROM contexts; END`,
		},
		"missing trigger":   {`DROP TRIGGER application_attempt_delete_revision`},
		"unknown index":     {`CREATE INDEX future_index ON contexts (encoding_version)`},
		"modified index":    {`DROP INDEX contexts_ordinal`, `CREATE INDEX contexts_ordinal ON contexts (id, ordinal)`},
		"missing index":     {`DROP INDEX contexts_ordinal`},
		"additional column": {`ALTER TABLE contexts ADD COLUMN unknown TEXT`},
		"missing check": {
			`DROP TABLE layout_state`,
			`CREATE TABLE layout_state (id INTEGER PRIMARY KEY CHECK (id = 1), encoding_version INTEGER NOT NULL, payload BLOB NOT NULL) STRICT`,
		},
		"changed check": {
			`DROP TABLE layout_state`,
			`CREATE TABLE layout_state (id INTEGER PRIMARY KEY CHECK (id = 1), encoding_version INTEGER NOT NULL, payload BLOB NOT NULL CHECK (length(payload) <= 16777217)) STRICT`,
		},
		"missing foreign key": {
			`DROP TABLE terminal_activity`,
			`CREATE TABLE terminal_activity (context_id TEXT PRIMARY KEY, created_at TEXT, last_focused_at TEXT, CHECK (created_at IS NOT NULL OR last_focused_at IS NOT NULL)) STRICT`,
		},
		"changed foreign key action": {
			`DROP TABLE terminal_activity`,
			`CREATE TABLE terminal_activity (context_id TEXT PRIMARY KEY REFERENCES contexts(id) ON DELETE RESTRICT, created_at TEXT, last_focused_at TEXT, CHECK (created_at IS NOT NULL OR last_focused_at IS NOT NULL)) STRICT`,
		},
		"missing strict": {
			`DROP TABLE layout_state`,
			`CREATE TABLE layout_state (id INTEGER PRIMARY KEY CHECK (id = 1), encoding_version INTEGER NOT NULL, payload BLOB NOT NULL CHECK (length(payload) <= 16777216))`,
		},
		"changed type": {
			`DROP TABLE layout_state`,
			`CREATE TABLE layout_state (id INTEGER PRIMARY KEY CHECK (id = 1), encoding_version TEXT NOT NULL, payload BLOB NOT NULL CHECK (length(payload) <= 16777216)) STRICT`,
		},
		"changed default": {
			`DROP TABLE application_session`,
			`CREATE TABLE application_session (id INTEGER PRIMARY KEY CHECK (id = 1), compositor_id TEXT NOT NULL, revision INTEGER NOT NULL DEFAULT 42) STRICT`,
		},
		"changed ledger literal": {
			`CREATE TABLE restore_report_meta (id INTEGER PRIMARY KEY CHECK (id = 1), automatic_run_id TEXT NOT NULL, interrupted_run_id TEXT NOT NULL DEFAULT ' ', interrupted_at TEXT) STRICT`,
			`CREATE TABLE restore_outcomes (source TEXT NOT NULL CHECK (source IN ('automatic', 'explicit')), context_id TEXT NOT NULL, attempt_id TEXT NOT NULL, payload BLOB NOT NULL CHECK (length(payload) <= 16777216), PRIMARY KEY (source, context_id)) STRICT`,
		},
		"changed ledger primary key": {
			`CREATE TABLE restore_report_meta (id INTEGER PRIMARY KEY CHECK (id = 1), automatic_run_id TEXT NOT NULL, interrupted_run_id TEXT NOT NULL DEFAULT '', interrupted_at TEXT) STRICT`,
			`CREATE TABLE restore_outcomes (source TEXT NOT NULL CHECK (source IN ('automatic', 'explicit')), context_id TEXT NOT NULL, attempt_id TEXT NOT NULL, payload BLOB NOT NULL CHECK (length(payload) <= 16777216), PRIMARY KEY (context_id)) STRICT`,
		},
	}
	for name, statements := range cases {
		t.Run(name, func(t *testing.T) {
			root := backupTestRoot(t)
			backupTestMutate(t, root, statements...)
			path := filepath.Join(root, StateDatabaseFilename)
			before := backupTestRead(t, path)
			if _, err := ValidateStateBackup(t.Context(), path); err == nil || !strings.Contains(err.Error(), "schema") {
				t.Fatalf("schema was not rejected explicitly: %v", err)
			}
			if !bytes.Equal(before, backupTestRead(t, path)) {
				t.Fatal("validation changed the unsupported schema")
			}
			output := backupTestOutput(t)
			if _, err := BackupState(t.Context(), root, output); err == nil {
				t.Fatal("backup published an unsupported schema")
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejected schema left a backup output")
			}
		})
	}
}

func TestValidateStateBackupSchemaCompatibility(t *testing.T) {
	for _, ledger := range []bool{false, true} {
		for _, maintenance := range []string{"none", "ANALYZE", "VACUUM", "REINDEX"} {
			name := maintenance
			if ledger {
				name += " with ledger"
			}
			t.Run(name, func(t *testing.T) {
				root := backupTestRoot(t)
				if ledger {
					if err := RestoreReportStoreFor(root).BeginExplicitContext(t.Context(), []RestoreOutcome{restoreTestOutcome("explicit", testContextID)}); err != nil {
						t.Fatal(err)
					}
				}
				if maintenance != "none" {
					backupTestMutate(t, root, maintenance)
				}
				// Real v1 initialization, optional-ledger initialization, and
				// SQLite maintenance must agree with the independent allowlist.
				if _, err := ValidateStateBackup(t.Context(), filepath.Join(root, StateDatabaseFilename)); err != nil {
					t.Fatal(err)
				}
				if _, err := BackupState(t.Context(), root, backupTestOutput(t)); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestValidateStateBackupRejectsTamperedSchemaCatalog(t *testing.T) {
	for name, statement := range map[string]string{
		"unknown internal object":     `UPDATE sqlite_schema SET name='sqlite_unrecognized', tbl_name='sqlite_unrecognized', sql='CREATE TABLE sqlite_unrecognized (x)' WHERE name='injected'`,
		"modified statistics columns": `UPDATE sqlite_schema SET sql='CREATE TABLE sqlite_stat1(tbl,idx,stat,hidden)' WHERE name='sqlite_stat1'`,
		"implicit index owner":        `UPDATE sqlite_schema SET tbl_name='terminal_activity' WHERE name='sqlite_autoindex_contexts_1'`,
		"missing implicit index":      `DELETE FROM sqlite_schema WHERE name='sqlite_autoindex_contexts_1'`,
		"trigger owner":               `UPDATE sqlite_schema SET tbl_name='contexts' WHERE name='application_attempt_insert_revision'`,
		"trigger root page":           `UPDATE sqlite_schema SET rootpage=5 WHERE name='application_attempt_insert_revision'`,
	} {
		t.Run(name, func(t *testing.T) {
			root := backupTestRoot(t)
			if name == "unknown internal object" {
				backupTestMutate(t, root, "CREATE TABLE injected(x)")
			} else if name == "modified statistics columns" {
				backupTestMutate(t, root, "ANALYZE")
			}
			path := filepath.Join(root, StateDatabaseFilename)
			// Only this disposable fixture uses writable_schema. Validation
			// must reject both parseable tampering and malformed catalogs.
			dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw&_defensive=0"}).String()
			db, err := openSQLiteStateDatabase(t.Context(), dsn)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("PRAGMA writable_schema=ON"); err != nil {
				_ = db.Close()
				t.Fatal(err)
			}
			_, updateErr := db.Exec(statement)
			if err := errors.Join(updateErr, db.Close()); err != nil {
				t.Fatal(err)
			}
			before := backupTestRead(t, path)
			if _, err := ValidateStateBackup(t.Context(), path); err == nil {
				t.Fatal("tampered catalog accepted")
			}
			if !bytes.Equal(before, backupTestRead(t, path)) {
				t.Fatal("validation changed the catalog")
			}
		})
	}
}
