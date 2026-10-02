package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// StateBackupValidation describes an existing validated runtime-state image.
type StateBackupValidation = StateBackupResult

// ValidateStateBackup accepts only standalone snapshots. In particular it must
// never use immutable=1 on a live WAL database, where it would miss committed
// frames. No opener here creates state, changes journal mode, or migrates data.
func ValidateStateBackup(ctx context.Context, path string) (StateBackupValidation, error) {
	file, result, err := openValidatedStateBackup(ctx, path)
	if err != nil {
		return StateBackupResult{}, err
	}
	return result, file.Close()
}

// openValidatedStateBackup returns the same pinned file that was validated,
// positioned at offset zero for recovery's staging copy. The caller closes it.
// As with the normal SQLite opener, concurrent changes by the owning UID are
// outside the filesystem trust boundary; callers must keep snapshots quiescent.
func openValidatedStateBackup(ctx context.Context, path string) (*os.File, StateBackupResult, error) {
	directory, name, err := openStateBackupParent(path)
	if err != nil {
		return nil, StateBackupResult{}, err
	}
	defer directory.Close()
	file, result, err := openValidatedStateBackupAt(ctx, directory, name)
	if err == nil {
		result.Path = path
	}
	return file, result, err
}

func validateStateBackupAt(ctx context.Context, directory *os.File, name string) (StateBackupResult, error) {
	file, result, err := openValidatedStateBackupAt(ctx, directory, name)
	if err != nil {
		return StateBackupResult{}, err
	}
	return result, file.Close()
}

func openValidatedStateBackupAt(ctx context.Context, directory *os.File, name string) (*os.File, StateBackupResult, error) {
	var result StateBackupResult
	if ctx == nil {
		return nil, result, errors.New("state backup validation context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, result, err
	}
	if err := validateStateBackupDirectory(directory); err != nil {
		return nil, result, err
	}
	if err := validateStateBackupName(name); err != nil {
		return nil, result, err
	}
	if err := requireAbsentStateBackupSidecarsAt(directory, name); err != nil {
		return nil, result, err
	}
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, result, fmt.Errorf("open state backup: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	keep := false
	defer func() {
		if !keep {
			_ = file.Close()
		}
	}()
	if err := verifyStateBackupFileAt(directory, name, file); err != nil {
		return nil, result, err
	}
	before, err := file.Stat()
	if err != nil {
		return nil, result, err
	}
	values := url.Values{
		"mode": {"ro"}, "immutable": {"1"}, "_defensive": {"1"}, "_dqs": {"0"},
		"_pragma": {"query_only(ON)", "trusted_schema(OFF)", "cell_size_check(ON)", "mmap_size(0)", "temp_store(MEMORY)"},
	}
	// SQLite's VFS canonicalizes /proc paths, so check the descriptor and its
	// directory entry both before and after all SQLite reads.
	db, err := openSQLiteStateDatabase(ctx, stateBackupURI(directory, name, values))
	if err != nil {
		return nil, result, err
	}
	result, validationErr := validateStateBackupDatabase(ctx, db)
	if err := errors.Join(validationErr, db.Close()); err != nil {
		return nil, StateBackupResult{}, err
	}
	if err := verifyStateBackupFileAt(directory, name, file); err != nil {
		return nil, StateBackupResult{}, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, StateBackupResult{}, err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, StateBackupResult{}, errors.New("state backup changed during validation")
	}
	if err := requireAbsentStateBackupSidecarsAt(directory, name); err != nil {
		return nil, StateBackupResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, StateBackupResult{}, err
	}
	result.Path, result.SizeBytes = name, after.Size()
	keep = true
	return file, result, nil
}

func validateStateBackupName(name string) error {
	if name == "" || name == "." || name == ".." {
		return errors.New("invalid state backup filename")
	}
	for _, ch := range name {
		if ch == '/' || ch == 0 {
			return errors.New("invalid state backup filename")
		}
	}
	return nil
}

func validateStateBackupDirectory(directory *os.File) error {
	if directory == nil {
		return errors.New("state backup directory is nil")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o7777 != 0o700 || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("state backup directory must be owner-only 0700")
	}
	return nil
}

func requireAbsentStateBackupSidecarsAt(directory *os.File, name string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		var stat unix.Stat_t
		err := unix.Fstatat(int(directory.Fd()), name+suffix, &stat, unix.AT_SYMLINK_NOFOLLOW)
		if err == nil {
			return errors.New("state backup must be standalone; SQLite sidecar exists")
		}
		if !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("inspect state backup sidecar: %w", err)
		}
	}
	return nil
}

func verifyStateBackupFileAt(directory *os.File, name string, file *os.File) error {
	var opened, current unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &opened); err != nil {
		return err
	}
	if opened.Mode&unix.S_IFMT != unix.S_IFREG || opened.Mode&0o7777 != 0o600 || opened.Uid != uint32(os.Geteuid()) || opened.Nlink != 1 {
		return errors.New("state backup must be a single-link owner-only 0600 regular file")
	}
	if opened.Size <= 0 || opened.Size > maxStateDatabaseBytes {
		return errors.New("state backup size is outside the database safety budget")
	}
	if err := unix.Fstatat(int(directory.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if opened.Dev != current.Dev || opened.Ino != current.Ino || current.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("state backup was replaced while being read")
	}
	return nil
}

func validateStateBackupHeader(ctx context.Context, queryer stateQueryer) (int, error) {
	var version, applicationID int
	if err := queryer.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, err
	}
	if version != StateDatabaseSchemaVersion {
		return 0, &UnsupportedVersionError{Document: "state database", Got: version, Want: StateDatabaseSchemaVersion}
	}
	if err := queryer.QueryRowContext(ctx, "PRAGMA application_id").Scan(&applicationID); err != nil {
		return 0, err
	}
	if applicationID != stateDatabaseApplicationID {
		return 0, errors.New("state backup application ID is invalid")
	}
	var pageSize, pageCount int64
	if err := queryer.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, err
	}
	if err := queryer.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return 0, err
	}
	if pageSize != stateDatabasePageSize || pageCount <= 0 || pageCount > stateDatabaseMaxPageCount {
		return 0, errors.New("state backup page budget is invalid")
	}
	var encoding string
	if err := queryer.QueryRowContext(ctx, "PRAGMA encoding").Scan(&encoding); err != nil {
		return 0, err
	}
	if encoding != "UTF-8" {
		return 0, errors.New("state backup database encoding is unsupported")
	}
	return version, nil
}

func validateStateBackupDatabase(ctx context.Context, db *sql.DB) (StateBackupResult, error) {
	var result StateBackupResult
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	result.SchemaVersion, err = validateStateBackupHeader(ctx, tx)
	if err != nil {
		return result, err
	}
	if err := validateStateBackupSchema(ctx, tx); err != nil {
		return result, err
	}
	var integrity string
	if err := tx.QueryRowContext(ctx, "PRAGMA integrity_check(1)").Scan(&integrity); err != nil {
		return result, err
	}
	if integrity != "ok" {
		return result, errors.New("state backup SQLite integrity check failed")
	}
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return result, err
	}
	violated := rows.Next()
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return result, err
	}
	if violated {
		return result, errors.New("state backup contains a foreign-key violation")
	}
	if err := validateStateBackupMetadata(ctx, tx); err != nil {
		return result, err
	}
	registry, _, err := loadRegistrySnapshotQuery(ctx, tx)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	result.ContextCount = len(registry.Contexts)
	contexts := make(map[ContextID]Context, len(registry.Contexts))
	for _, value := range registry.Contexts {
		contexts[value.ID] = value
	}
	activity, err := loadTerminalActivityQuery(ctx, tx)
	if err != nil {
		return result, err
	}
	for _, entry := range activity.Terminals {
		value, ok := contexts[entry.ContextID]
		if !ok || value.Launcher.Kind != LauncherHerdr || value.Launcher.Terminal == nil {
			return result, errors.New("state backup terminal activity references a nonterminal context")
		}
	}
	if err := validateStateBackupLayout(ctx, tx); err != nil {
		return result, err
	}
	if err := validateStateBackupApplications(ctx, tx, contexts); err != nil {
		return result, err
	}
	exists, err := restoreReportTablesExist(ctx, tx)
	if err != nil {
		return result, err
	}
	if exists {
		if err := validateStateBackupPayloadBudget(ctx, tx, "restore_outcomes"); err != nil {
			return result, err
		}
		if _, err := loadRestoreReport(ctx, tx); err != nil {
			return result, err
		}
	}
	if err := validateLifecycleOperationsQuery(ctx, tx, registry); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

type stateBackupSchemaObject struct {
	kind, table, sql string
	extension        string
	optional         bool
}

// stateBackupSchemaV1 is an allowlist, not a schema initializer. Keep this
// independent of untrusted input and compare it with real initialized databases
// in tests. A new supported schema must explicitly account for its executable
// objects as well as its document columns, constraints, and implicit indexes.
func stateBackupSchemaV1() map[string]stateBackupSchemaObject {
	return map[string]stateBackupSchemaObject{
		"state_meta": {kind: "table", table: "state_meta", sql: `CREATE TABLE state_meta (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			registry_revision INTEGER NOT NULL DEFAULT 0,
			registry_present INTEGER NOT NULL DEFAULT 0 CHECK (registry_present IN (0, 1))
		) STRICT`},
		"registry_preferences": {kind: "table", table: "registry_preferences", sql: `CREATE TABLE registry_preferences (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			desktop_indicators INTEGER NOT NULL CHECK (desktop_indicators IN (0, 1))
		) STRICT`},
		"contexts": {kind: "table", table: "contexts", sql: `CREATE TABLE contexts (
			id TEXT PRIMARY KEY,
			ordinal INTEGER NOT NULL,
			encoding_version INTEGER NOT NULL,
			payload BLOB NOT NULL CHECK (length(payload) <= 16777216)
		) STRICT`},
		"contexts_ordinal":            {kind: "index", table: "contexts", sql: `CREATE INDEX contexts_ordinal ON contexts (ordinal, id)`},
		"sqlite_autoindex_contexts_1": {kind: "index", table: "contexts"},
		"terminal_activity": {kind: "table", table: "terminal_activity", sql: `CREATE TABLE terminal_activity (
			context_id TEXT PRIMARY KEY REFERENCES contexts(id) ON DELETE CASCADE,
			created_at TEXT,
			last_focused_at TEXT,
			CHECK (created_at IS NOT NULL OR last_focused_at IS NOT NULL)
		) STRICT`},
		"sqlite_autoindex_terminal_activity_1": {kind: "index", table: "terminal_activity"},
		"layout_state": {kind: "table", table: "layout_state", sql: `CREATE TABLE layout_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			encoding_version INTEGER NOT NULL,
			payload BLOB NOT NULL CHECK (length(payload) <= 16777216)
		) STRICT`},
		"application_session": {kind: "table", table: "application_session", sql: `CREATE TABLE application_session (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			compositor_id TEXT NOT NULL,
			revision INTEGER NOT NULL DEFAULT 0
		) STRICT`},
		"application_launch_attempts": {kind: "table", table: "application_launch_attempts", sql: `CREATE TABLE application_launch_attempts (
			context_id TEXT PRIMARY KEY REFERENCES contexts(id) ON DELETE CASCADE,
			started_at TEXT NOT NULL
		) STRICT`},
		"sqlite_autoindex_application_launch_attempts_1": {kind: "index", table: "application_launch_attempts"},
		"application_attempt_insert_revision": {kind: "trigger", table: "application_launch_attempts", sql: `CREATE TRIGGER application_attempt_insert_revision AFTER INSERT ON application_launch_attempts
			WHEN EXISTS (SELECT 1 FROM application_session WHERE id = 1)
			BEGIN UPDATE application_session SET revision = revision + 1 WHERE id = 1; END`},
		"application_attempt_update_revision": {kind: "trigger", table: "application_launch_attempts", sql: `CREATE TRIGGER application_attempt_update_revision AFTER UPDATE ON application_launch_attempts
			WHEN EXISTS (SELECT 1 FROM application_session WHERE id = 1)
			BEGIN UPDATE application_session SET revision = revision + 1 WHERE id = 1; END`},
		"application_attempt_delete_revision": {kind: "trigger", table: "application_launch_attempts", sql: `CREATE TRIGGER application_attempt_delete_revision AFTER DELETE ON application_launch_attempts
			WHEN EXISTS (SELECT 1 FROM application_session WHERE id = 1)
			BEGIN UPDATE application_session SET revision = revision + 1 WHERE id = 1; END`},
		"restore_report_meta":                       {kind: "table", table: "restore_report_meta", extension: "restore", sql: `CREATE TABLE restore_report_meta (id INTEGER PRIMARY KEY CHECK (id = 1), automatic_run_id TEXT NOT NULL, interrupted_run_id TEXT NOT NULL DEFAULT '', interrupted_at TEXT) STRICT`},
		"restore_outcomes":                          {kind: "table", table: "restore_outcomes", extension: "restore", sql: `CREATE TABLE restore_outcomes (source TEXT NOT NULL CHECK (source IN ('automatic', 'explicit')), context_id TEXT NOT NULL, attempt_id TEXT NOT NULL, payload BLOB NOT NULL CHECK (length(payload) <= 16777216), PRIMARY KEY (source, context_id)) STRICT`},
		"sqlite_autoindex_restore_outcomes_1":       {kind: "index", table: "restore_outcomes", extension: "restore"},
		"lifecycle_operations":                      {kind: "table", table: "lifecycle_operations", extension: "lifecycle", sql: lifecycleOperationsSQL},
		"sqlite_autoindex_lifecycle_operations_1":   {kind: "index", table: "lifecycle_operations", extension: "lifecycle"},
		"lifecycle_reservations":                    {kind: "table", table: "lifecycle_reservations", extension: "lifecycle", sql: lifecycleReservationsSQL},
		"sqlite_autoindex_lifecycle_reservations_1": {kind: "index", table: "lifecycle_reservations", extension: "lifecycle"},
		"lifecycle_reservations_operation":          {kind: "index", table: "lifecycle_reservations", extension: "lifecycle", sql: lifecycleReservationIndexSQL},
		// ANALYZE may add these SQLite-owned statistics tables to an otherwise
		// unchanged v1 database. Do not exempt arbitrary sqlite_* objects.
		"sqlite_stat1": {kind: "table", table: "sqlite_stat1", optional: true, sql: `CREATE TABLE sqlite_stat1(tbl,idx,stat)`},
		"sqlite_stat4": {kind: "table", table: "sqlite_stat4", optional: true, sql: `CREATE TABLE sqlite_stat4(tbl,idx,neq,nlt,ndlt,sample)`},
	}
}

func validateStateBackupSchema(ctx context.Context, queryer stateQueryer) error {
	expected := stateBackupSchemaV1()
	rows, err := queryer.QueryContext(ctx, "SELECT type, name, tbl_name, rootpage, sql FROM main.sqlite_schema")
	if err != nil {
		return err
	}
	defer rows.Close()
	extensions := make(map[string]bool)
	for rows.Next() {
		var name, kind, table string
		var rootPage sql.NullInt64
		var definition sql.NullString
		if err := rows.Scan(&kind, &name, &table, &rootPage, &definition); err != nil {
			return err
		}
		want, known := expected[name]
		if !known || kind != want.kind || table != want.table || definition.Valid != (want.sql != "") || normalizeStateBackupSchemaSQL(definition.String) != normalizeStateBackupSchemaSQL(want.sql) {
			return fmt.Errorf("state backup contains an unsupported database schema object %q", name)
		}
		if kind == "trigger" {
			if rootPage.Int64 != 0 {
				return errors.New("state backup trigger has an invalid root page")
			}
		} else if !rootPage.Valid || rootPage.Int64 <= 1 || rootPage.Int64 > stateDatabaseMaxPageCount {
			return errors.New("state backup schema object has an invalid root page")
		}
		if want.extension != "" {
			extensions[want.extension] = true
		}
		delete(expected, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for name, want := range expected {
		if !want.optional && (want.extension == "" || extensions[want.extension]) {
			return fmt.Errorf("state backup is missing required schema object %q", name)
		}
	}
	return nil
}

func normalizeStateBackupSchemaSQL(statement string) string {
	// Ignore formatting whitespace only outside quoted SQL tokens. Do not
	// remove comments, rewrite operators, fold literal case, or collapse spaces
	// within literals: any of those could hide a changed constraint or trigger.
	var result strings.Builder
	var quote byte
	space := false
	for index := 0; index < len(statement); index++ {
		ch := statement[index]
		if quote != 0 {
			result.WriteByte(ch)
			if ch == quote {
				if index+1 < len(statement) && statement[index+1] == quote {
					index++
					result.WriteByte(quote)
				} else {
					quote = 0
				}
			}
			continue
		}
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '\f' {
			space = result.Len() != 0
			continue
		}
		if space {
			result.WriteByte(' ')
			space = false
		}
		result.WriteByte(ch)
		if ch == '\'' || ch == '"' || ch == '`' {
			quote = ch
		} else if ch == '[' {
			quote = ']'
		}
	}
	return result.String()
}

func validateStateBackupMetadata(ctx context.Context, queryer stateQueryer) error {
	// Do not let absent-document fast paths hide persisted rows or invalid
	// preferences. Check singleton cardinality as well as the selected row.
	var metaCount, preferenceCount, present, indicators, contextCount int
	var revision int64
	if err := queryer.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM state_meta), (SELECT count(*) FROM registry_preferences),
		registry_present, registry_revision, (SELECT desktop_indicators FROM registry_preferences WHERE id = 1),
		(SELECT count(*) FROM contexts) FROM state_meta WHERE id = 1`).Scan(
		&metaCount, &preferenceCount, &present, &revision, &indicators, &contextCount,
	); err != nil {
		return err
	}
	if metaCount != 1 || preferenceCount != 1 || revision < 0 || (present != 0 && present != 1) || (indicators != 0 && indicators != 1) || (present == 0 && contextCount != 0) {
		return errors.New("state backup registry metadata is invalid")
	}
	return nil
}

func validateStateBackupPayloadBudget(ctx context.Context, queryer stateQueryer, table string) error {
	// table is always an internal constant, never an input filename or payload.
	var largest, total int64
	if err := queryer.QueryRowContext(ctx, "SELECT COALESCE(MAX(length(payload)), 0), COALESCE(SUM(length(payload)), 0) FROM "+table).Scan(&largest, &total); err != nil {
		return err
	}
	if largest > maxDatabasePayloadBytes || total > maxStateDatabaseBytes {
		return errors.New("state backup payload exceeds the database safety budget")
	}
	return nil
}

func validateStateBackupLayout(ctx context.Context, queryer stateQueryer) error {
	if err := validateStateBackupPayloadBudget(ctx, queryer, "layout_state"); err != nil {
		return err
	}
	rows, err := queryer.QueryContext(ctx, "SELECT id, encoding_version, payload FROM layout_state")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, version int
		var payload []byte
		if err := rows.Scan(&id, &version, &payload); err != nil {
			return err
		}
		if id != 1 {
			return errors.New("state backup layout key is invalid")
		}
		if version != LayoutSchemaVersion {
			return &UnsupportedVersionError{Document: "layout", Got: version, Want: LayoutSchemaVersion}
		}
		var layout LayoutSnapshot
		if err := decodeDatabasePayload("layout", payload, &layout); err != nil {
			return err
		}
		if err := layout.Validate(); err != nil {
			return err
		}
	}
	return rows.Err()
}

func validateStateBackupApplications(ctx context.Context, queryer stateQueryer, contexts map[ContextID]Context) error {
	state := ApplicationSessionState{Version: ApplicationSessionSchemaVersion, Attempts: []ApplicationLaunchAttempt{}}
	var count int
	if err := queryer.QueryRowContext(ctx, "SELECT count(*) FROM application_session").Scan(&count); err != nil {
		return err
	}
	if count > 1 {
		return errors.New("state backup application session metadata is invalid")
	}
	if count == 1 {
		var revision int64
		if err := queryer.QueryRowContext(ctx, "SELECT compositor_id, revision FROM application_session WHERE id = 1").Scan(&state.CompositorID, &revision); err != nil {
			return err
		}
		if revision < 0 {
			return errors.New("state backup application session revision is invalid")
		}
	}
	rows, err := queryer.QueryContext(ctx, "SELECT context_id, started_at FROM application_launch_attempts")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var attempt ApplicationLaunchAttempt
		var started string
		if err := rows.Scan(&attempt.ContextID, &started); err != nil {
			return err
		}
		value, ok := contexts[attempt.ContextID]
		if count == 0 || !ok || value.App == nil || (value.Launcher.Kind != LauncherDesktop && value.Launcher.Kind != LauncherFlatpak) {
			return errors.New("state backup launch attempt has no application session or application context")
		}
		attempt.StartedAt, err = time.Parse(time.RFC3339Nano, started)
		if err != nil {
			return fmt.Errorf("decode backup launch attempt time: %w", err)
		}
		state.Attempts = append(state.Attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != 0 {
		return state.Validate()
	}
	return nil
}
