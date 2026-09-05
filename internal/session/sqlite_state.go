package session

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/marang/sway-session/internal/statefile"
	"golang.org/x/sys/unix"
	"modernc.org/sqlite"
)

const (
	StateDatabaseFilename         = "state.sqlite3"
	StateDatabaseSchemaVersion    = 1
	databaseBusyTimeout           = 250 * time.Millisecond
	databaseInitializationTimeout = 2 * time.Second
	maxDatabasePayloadBytes       = 16 * 1024 * 1024
	maxStateDatabaseBytes         = 1 << 30
	stateDatabasePageSize         = 4096
	stateDatabaseMaxPageCount     = maxStateDatabaseBytes / stateDatabasePageSize
	stateDatabaseApplicationID    = 0x53574159
)

var sqliteOFDLockingError error
var sqliteOFDLockingEnabled = sqlite.OFDLockingEnabled
var executeStateCommit = func(tx *stateWriteTransaction) error { return tx.Commit() }
var acquireStateDatabaseInitializationLock = lockStateDatabaseInitialization
var statStateDatabaseObjectAt = unix.Fstatat
var ErrUninitializedStateDatabase = errors.New("state database schema is uninitialized")
var ErrStateDatabaseBusy = errors.New("state database is busy")

func init() {
	_, sqliteOFDLockingError = sqlite.OFDLocking(true)
}

var legacyStateRelativePaths = []string{
	legacyContextsFilename,
	legacyLayoutFilename,
	filepath.Join(legacyApplicationSessionDirectory, legacyApplicationSessionFilename),
	filepath.Join(legacyTerminalActivityDirectory, legacyTerminalActivityFilename),
}

// LegacyStateError reports pre-SQLite runtime state which must be migrated
// explicitly from terminal manage. Ordinary commands never import it.
type LegacyStateError struct {
	Paths []string
}

func (err *LegacyStateError) Error() string {
	return fmt.Sprintf("legacy sway-session JSON state exists (%s)", strings.Join(err.Paths, ", "))
}

type stateDatabase struct {
	db          *sql.DB
	directory   *os.File
	dsn         string
	initialized bool
}

func (database *stateDatabase) Close() error {
	if database == nil {
		return nil
	}
	var databaseErr error
	if database.db != nil {
		databaseErr = database.db.Close()
	}
	return errors.Join(databaseErr, database.directory.Close())
}

func openStateDatabase(ctx context.Context, root string, create bool) (*stateDatabase, error) {
	return openStateDatabaseWithInitializer(ctx, root, create, false, nil)
}

func openStateDatabaseWithInitializer(
	ctx context.Context,
	root string,
	create bool,
	allowLegacy bool,
	initializer func(context.Context, stateTransaction) error,
) (*stateDatabase, error) {
	if ctx == nil {
		return nil, errors.New("state database context is nil")
	}
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, errors.New("state database root must be a clean absolute path")
	}
	if sqliteOFDLockingError != nil {
		return nil, fmt.Errorf("enable SQLite OFD locking: %w", sqliteOFDLockingError)
	}
	directory, err := statefile.OpenPrivateDirectory(root, create)
	if err != nil {
		return nil, err
	}
	keepDirectory := false
	defer func() {
		if !keepDirectory {
			_ = directory.Close()
		}
	}()

	before, exists, err := inspectDatabaseAt(directory)
	if err != nil {
		return nil, err
	}
	if !exists {
		if !allowLegacy {
			legacy, legacyErr := findLegacyState(directory)
			if legacyErr != nil {
				return nil, legacyErr
			}
			if len(legacy) != 0 {
				return nil, &LegacyStateError{Paths: legacy}
			}
		}
		if !create {
			return nil, os.ErrNotExist
		}
		if err := statefile.CreatePrivateFileAt(directory, StateDatabaseFilename, nil); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create state database: %w", err)
		}
		before, exists, err = inspectDatabaseAt(directory)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, errors.New("created state database is not visible")
		}
	}
	if err := inspectDatabaseSidecarsAt(directory); err != nil {
		return nil, err
	}

	values := url.Values{}
	busyTimeout := databaseBusyTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < busyTimeout {
			busyTimeout = max(remaining, time.Millisecond)
		}
	}
	values.Set("mode", "rw")
	values.Set("_busy_timeout", strconv.FormatInt(busyTimeout.Milliseconds(), 10))
	values.Set("_foreign_keys", "on")
	values.Set("_synchronous", "full")
	values.Set("_defensive", "1")
	values.Set("_dqs", "0")
	values.Set("_error_rc", "1")
	values.Add("_pragma", "cell_size_check(ON)")
	values.Add("_pragma", "journal_size_limit(4194304)")
	values.Add("_pragma", "mmap_size(0)")
	values.Add("_pragma", "secure_delete(ON)")
	values.Add("_pragma", "temp_store(MEMORY)")
	values.Add("_pragma", "trusted_schema(OFF)")
	values.Add("_pragma", "wal_autocheckpoint(256)")
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     fmt.Sprintf("/proc/self/fd/%d/%s", directory.Fd(), StateDatabaseFilename),
		RawQuery: values.Encode(),
	}).String()
	// The descriptor-relative spelling makes preflight follow the already
	// validated directory, but SQLite's Unix VFS canonicalizes it to a normal
	// path. The default state-root confinement rules therefore also prevent
	// ancestor-directory replacement. Unconfined processes of the same UID are
	// trusted state owners and must not relocate the root while operations run.
	db, err := openSQLiteStateDatabase(ctx, dsn)
	if err != nil {
		return nil, err
	}
	var schemaVersion int
	if err := db.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&schemaVersion); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("probe state database locking: %w", err)
	}
	if !sqliteOFDLockingEnabled() {
		_ = db.Close()
		return nil, errors.New("state database filesystem does not support required OFD locking")
	}
	after, exists, err := inspectDatabaseAt(directory)
	if err != nil || !exists || before.Dev != after.Dev || before.Ino != after.Ino {
		_ = db.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("state database changed while it was being opened")
	}
	database := &stateDatabase{db: db, directory: directory, dsn: dsn}
	initialized, err := database.ensureSchemaBounded(ctx, create, allowLegacy, initializer)
	if err != nil {
		_ = database.Close()
		return nil, err
	}
	database.initialized = initialized
	if err := database.verifyConnection(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	keepDirectory = true
	return database, nil
}

func openSQLiteStateDatabase(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open state database: %w", err)
	}
	return db, nil
}

func (database *stateDatabase) ensureSchemaBounded(
	ctx context.Context,
	create bool,
	allowLegacy bool,
	initializer func(context.Context, stateTransaction) error,
) (bool, error) {
	deadline := time.Now().Add(databaseInitializationTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	for {
		initialized, err := database.ensureSchema(ctx, create, allowLegacy, initializer)
		if err == nil || !create || !IsStateDatabaseBusy(err) {
			return initialized, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return false, ctxErr
			}
			return false, err
		}
		pause := min(5*time.Millisecond, remaining)
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
}

func (database *stateDatabase) verifyConnection(ctx context.Context) error {
	var pageSize, pageCount int64
	if err := database.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return fmt.Errorf("verify state database page size: %w", err)
	}
	if err := database.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return fmt.Errorf("verify state database page count: %w", err)
	}
	if pageSize != stateDatabasePageSize || pageCount > stateDatabaseMaxPageCount {
		return fmt.Errorf(
			"state database page budget is invalid: page_size=%d page_count=%d",
			pageSize, pageCount,
		)
	}
	var journalMode string
	if err := database.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return fmt.Errorf("verify state database journal mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("state database journal mode is %q; expected WAL", journalMode)
	}
	var foreignKeys int
	if err := database.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return fmt.Errorf("verify state database foreign keys: %w", err)
	}
	if foreignKeys != 1 {
		return errors.New("state database foreign keys are disabled")
	}
	var applicationID int
	if err := database.db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&applicationID); err != nil {
		return fmt.Errorf("verify state database application ID: %w", err)
	}
	if applicationID != stateDatabaseApplicationID {
		return fmt.Errorf("state database application ID is %d; expected %d", applicationID, stateDatabaseApplicationID)
	}
	return nil
}

func (database *stateDatabase) verifyIntegrity(ctx context.Context) error {
	foreignKeyRows, err := database.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("check state database foreign keys: %w", err)
	}
	if foreignKeyRows.Next() {
		_ = foreignKeyRows.Close()
		return errors.New("state database contains a foreign-key violation")
	}
	if err := foreignKeyRows.Err(); err != nil {
		_ = foreignKeyRows.Close()
		return fmt.Errorf("iterate state database foreign-key check: %w", err)
	}
	if err := foreignKeyRows.Close(); err != nil {
		return fmt.Errorf("close state database foreign-key check: %w", err)
	}
	var quickCheck string
	if err := database.db.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&quickCheck); err != nil {
		return fmt.Errorf("check state database integrity: %w", err)
	}
	if quickCheck != "ok" {
		return fmt.Errorf("state database integrity check failed: %s", quickCheck)
	}
	return nil
}

// VerifyStateDatabaseContext runs the database-wide integrity checks used at
// daemon startup. Ordinary short-lived store operations still validate all
// connection invariants and every row they consume without rescanning the
// complete database on each open.
func VerifyStateDatabaseContext(ctx context.Context, root string) error {
	database, err := openStateDatabase(ctx, root, false)
	if err != nil {
		return err
	}
	defer database.Close()
	return database.verifyIntegrity(ctx)
}

func inspectDatabaseAt(directory *os.File) (unix.Stat_t, bool, error) {
	return inspectPrivateDatabaseObjectAt(directory, StateDatabaseFilename)
}

func inspectDatabaseSidecarsAt(directory *os.File) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		name := StateDatabaseFilename + suffix
		if _, _, err := inspectPrivateDatabaseObjectAt(directory, name); err != nil {
			return err
		}
	}
	return nil
}

func inspectPrivateDatabaseObjectAt(directory *os.File, name string) (unix.Stat_t, bool, error) {
	var stat unix.Stat_t
	err := statStateDatabaseObjectAt(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return stat, false, nil
	}
	if err != nil {
		return stat, false, fmt.Errorf("inspect state database object %s: %w", name, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return stat, false, fmt.Errorf("state database object %s is not a regular file", name)
	}
	if stat.Mode&0o7777 != statefile.RegularFileMode {
		return stat, false, fmt.Errorf("state database object %s permissions are %04o; expected %04o", name, stat.Mode&0o7777, statefile.RegularFileMode)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return stat, false, fmt.Errorf("state database object %s owner is UID %d; expected %d", name, stat.Uid, os.Geteuid())
	}
	// SQLite creates and removes WAL, SHM, and rollback-journal sidecars as
	// connections change modes or close. fstatat may resolve one immediately
	// before SQLite unlinks it and then report the detached inode with no links.
	// Treat that transient sidecar as absent; the persistent main database must
	// always remain linked, and every reachable object must still be single-link.
	if stat.Nlink == 0 && isStateDatabaseSidecar(name) {
		return stat, false, nil
	}
	if stat.Nlink != 1 {
		return stat, false, fmt.Errorf("state database object %s link count is %d; expected 1", name, stat.Nlink)
	}
	// A valid WAL or rollback journal may temporarily exceed the main database
	// while a reader pins old frames or recovery is pending after a crash. Do
	// not reject those sidecars before SQLite has a chance to recover and
	// checkpoint them. The main database remains bounded by max_page_count.
	if name == StateDatabaseFilename && (stat.Size < 0 || stat.Size > maxStateDatabaseBytes) {
		return stat, false, fmt.Errorf("state database object %s is too large: %d bytes exceeds %d", name, stat.Size, maxStateDatabaseBytes)
	}
	return stat, true, nil
}

func isStateDatabaseSidecar(name string) bool {
	switch name {
	case StateDatabaseFilename + "-wal", StateDatabaseFilename + "-shm", StateDatabaseFilename + "-journal":
		return true
	default:
		return false
	}
}

func findLegacyState(directory *os.File) ([]string, error) {
	found := make([]string, 0)
	for _, relative := range legacyStateRelativePaths {
		exists, err := legacyPathExistsAt(directory, relative)
		switch {
		case err == nil && exists:
			found = append(found, relative)
		case err == nil:
			continue
		default:
			return nil, fmt.Errorf("inspect legacy state %s: %w", relative, err)
		}
	}
	return found, nil
}

func legacyPathExistsAt(directory *os.File, relative string) (bool, error) {
	components := strings.Split(relative, string(os.PathSeparator))
	currentFD, err := unix.FcntlInt(directory.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return false, fmt.Errorf("duplicate state directory descriptor: %w", err)
	}
	defer func() { _ = unix.Close(currentFD) }()
	for index, component := range components {
		if component == "" || component == "." || component == ".." {
			return false, errors.New("legacy state path is invalid")
		}
		if index == len(components)-1 {
			var stat unix.Stat_t
			err := unix.Fstatat(currentFD, component, &stat, unix.AT_SYMLINK_NOFOLLOW)
			if errors.Is(err, unix.ENOENT) {
				return false, nil
			}
			return err == nil, err
		}
		nextFD, err := unix.Openat(currentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("open legacy state directory %s: %w", component, err)
		}
		_ = unix.Close(currentFD)
		currentFD = nextFD
	}
	return false, nil
}

func (database *stateDatabase) ensureSchema(
	ctx context.Context,
	create bool,
	allowLegacy bool,
	initializer func(context.Context, stateTransaction) error,
) (bool, error) {
	var version int
	if err := database.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return false, fmt.Errorf("read state database schema version: %w", err)
	}
	if version == StateDatabaseSchemaVersion {
		return false, nil
	}
	if version != 0 {
		return false, &UnsupportedVersionError{Document: "state database", Got: version, Want: StateDatabaseSchemaVersion}
	}
	if !allowLegacy {
		legacy, err := findLegacyState(database.directory)
		if err != nil {
			return false, err
		}
		if len(legacy) != 0 {
			return false, &LegacyStateError{Paths: legacy}
		}
	}
	if !create {
		return false, ErrUninitializedStateDatabase
	}
	// Persistent bootstrap PRAGMAs must run before the schema transaction, so
	// SQLite cannot serialize them with BEGIN IMMEDIATE. Drop this opener's
	// provisional connection before waiting on the cross-process file lock;
	// otherwise a waiter could retain a SQLite lock needed by the initializer.
	if err := database.db.Close(); err != nil {
		return false, fmt.Errorf("close state database before initialization lock: %w", err)
	}
	database.db = nil
	initializationLock, err := acquireStateDatabaseInitializationLock(ctx, database.directory)
	if err != nil {
		return false, err
	}
	defer unlockStateDatabaseInitialization(initializationLock)
	database.db, err = openSQLiteStateDatabase(ctx, database.dsn)
	if err != nil {
		return false, err
	}
	if err := verifyStateDatabaseInitializationLock(database.directory, initializationLock, "while reopening after initialization lock"); err != nil {
		return false, err
	}
	if err := database.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return false, fmt.Errorf("recheck state database schema version: %w", err)
	}
	if version == StateDatabaseSchemaVersion {
		return false, nil
	}
	if version != 0 {
		return false, &UnsupportedVersionError{Document: "state database", Got: version, Want: StateDatabaseSchemaVersion}
	}
	if _, err := database.db.ExecContext(ctx, fmt.Sprintf("PRAGMA page_size = %d", stateDatabasePageSize)); err != nil {
		return false, fmt.Errorf("configure state database page size: %w", err)
	}
	var maxPageCount int64
	if err := database.db.QueryRowContext(ctx, fmt.Sprintf("PRAGMA max_page_count = %d", stateDatabaseMaxPageCount)).Scan(&maxPageCount); err != nil {
		return false, fmt.Errorf("configure state database page limit: %w", err)
	}
	if maxPageCount != stateDatabaseMaxPageCount {
		return false, fmt.Errorf("state database refused page limit: got %d, want %d", maxPageCount, stateDatabaseMaxPageCount)
	}
	if _, err := database.db.ExecContext(ctx, "PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
		return false, fmt.Errorf("configure state database auto vacuum: %w", err)
	}
	var journalMode string
	if err := database.db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		return false, fmt.Errorf("configure state database journal mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return false, fmt.Errorf("state database refused WAL journal mode: %q", journalMode)
	}
	tx, err := beginStateWrite(ctx, database)
	if err != nil {
		return false, fmt.Errorf("begin state database initialization: %w", err)
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return false, fmt.Errorf("recheck state database schema version: %w", err)
	}
	if version == StateDatabaseSchemaVersion {
		return false, nil
	}
	if version != 0 {
		return false, &UnsupportedVersionError{Document: "state database", Got: version, Want: StateDatabaseSchemaVersion}
	}
	statements := []string{
		`CREATE TABLE state_meta (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			registry_revision INTEGER NOT NULL DEFAULT 0,
			registry_present INTEGER NOT NULL DEFAULT 0 CHECK (registry_present IN (0, 1))
		) STRICT`,
		`CREATE TABLE registry_preferences (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			desktop_indicators INTEGER NOT NULL CHECK (desktop_indicators IN (0, 1))
		) STRICT`,
		`CREATE TABLE contexts (
			id TEXT PRIMARY KEY,
			ordinal INTEGER NOT NULL,
			encoding_version INTEGER NOT NULL,
			payload BLOB NOT NULL CHECK (length(payload) <= 16777216)
		) STRICT`,
		`CREATE INDEX contexts_ordinal ON contexts (ordinal, id)`,
		`CREATE TABLE terminal_activity (
			context_id TEXT PRIMARY KEY REFERENCES contexts(id) ON DELETE CASCADE,
			created_at TEXT,
			last_focused_at TEXT,
			CHECK (created_at IS NOT NULL OR last_focused_at IS NOT NULL)
		) STRICT`,
		`CREATE TABLE layout_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			encoding_version INTEGER NOT NULL,
			payload BLOB NOT NULL CHECK (length(payload) <= 16777216)
		) STRICT`,
		`CREATE TABLE application_session (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			compositor_id TEXT NOT NULL,
			revision INTEGER NOT NULL DEFAULT 0
		) STRICT`,
		`CREATE TABLE application_launch_attempts (
			context_id TEXT PRIMARY KEY REFERENCES contexts(id) ON DELETE CASCADE,
			started_at TEXT NOT NULL
		) STRICT`,
		`CREATE TRIGGER application_attempt_insert_revision AFTER INSERT ON application_launch_attempts
			WHEN EXISTS (SELECT 1 FROM application_session WHERE id = 1)
			BEGIN UPDATE application_session SET revision = revision + 1 WHERE id = 1; END`,
		`CREATE TRIGGER application_attempt_update_revision AFTER UPDATE ON application_launch_attempts
			WHEN EXISTS (SELECT 1 FROM application_session WHERE id = 1)
			BEGIN UPDATE application_session SET revision = revision + 1 WHERE id = 1; END`,
		`CREATE TRIGGER application_attempt_delete_revision AFTER DELETE ON application_launch_attempts
			WHEN EXISTS (SELECT 1 FROM application_session WHERE id = 1)
			BEGIN UPDATE application_session SET revision = revision + 1 WHERE id = 1; END`,
		`INSERT INTO state_meta (id) VALUES (1)`,
		`INSERT INTO registry_preferences (id, desktop_indicators) VALUES (1, 0)`,
		fmt.Sprintf("PRAGMA application_id = %d", stateDatabaseApplicationID),
		fmt.Sprintf("PRAGMA user_version = %d", StateDatabaseSchemaVersion),
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return false, fmt.Errorf("initialize state database schema: %w", err)
		}
	}
	if initializer != nil {
		if err := initializer(ctx, tx); err != nil {
			return false, fmt.Errorf("initialize migrated state database: %w", err)
		}
	}
	if err := commitStateWrite(ctx, tx); err != nil {
		return false, fmt.Errorf("commit state database initialization: %w", err)
	}
	return true, nil
}

func lockStateDatabaseInitialization(ctx context.Context, directory *os.File) (*os.File, error) {
	fd, err := unix.Openat(
		int(directory.Fd()),
		StateDatabaseFilename,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open state database initialization lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), StateDatabaseFilename)
	keepFile := false
	defer func() {
		if !keepFile {
			_ = file.Close()
		}
	}()

	if err := verifyStateDatabaseInitializationLock(directory, file, "before initialization lock acquisition"); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(databaseInitializationTimeout)
	contextBounded := false
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
		contextBounded = true
	}
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("lock state database initialization: %w", err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if contextBounded {
				return nil, context.DeadlineExceeded
			}
			return nil, fmt.Errorf("%w: wait for state database initialization lock", ErrStateDatabaseBusy)
		}
		pause := min(5*time.Millisecond, remaining)
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	if err := verifyStateDatabaseInitializationLock(directory, file, "while acquiring initialization lock"); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, err
	}
	keepFile = true
	return file, nil
}

func verifyStateDatabaseInitializationLock(directory, file *os.File, stage string) error {
	var opened unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &opened); err != nil {
		return fmt.Errorf("inspect state database initialization lock: %w", err)
	}
	current, exists, err := inspectDatabaseAt(directory)
	if err != nil {
		return err
	}
	if !exists || opened.Dev != current.Dev || opened.Ino != current.Ino {
		return fmt.Errorf("state database changed %s", stage)
	}
	return nil
}

func unlockStateDatabaseInitialization(file *os.File) {
	if file == nil {
		return
	}
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	_ = file.Close()
}

type stateTransaction interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type stateWriteTransaction struct {
	connection *sql.Conn
	done       bool
}

func beginStateWrite(ctx context.Context, database *stateDatabase) (*stateWriteTransaction, error) {
	connection, err := database.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("reserve state database writer connection: %w", err)
	}
	keepConnection := false
	defer func() {
		if !keepConnection {
			_ = connection.Close()
		}
	}()
	var maxPageCount int64
	if err := connection.QueryRowContext(ctx, fmt.Sprintf("PRAGMA max_page_count = %d", stateDatabaseMaxPageCount)).Scan(&maxPageCount); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("configure state database write page limit: %w", err)
	}
	if maxPageCount != stateDatabaseMaxPageCount {
		return nil, fmt.Errorf("state database refused write page limit: got %d, want %d", maxPageCount, stateDatabaseMaxPageCount)
	}
	if _, err := connection.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("begin state database transaction: %w", err)
	}
	keepConnection = true
	return &stateWriteTransaction{connection: connection}, nil
}

func (tx *stateWriteTransaction) ExecContext(ctx context.Context, query string, arguments ...any) (sql.Result, error) {
	return tx.connection.ExecContext(ctx, query, arguments...)
}

func (tx *stateWriteTransaction) QueryContext(ctx context.Context, query string, arguments ...any) (*sql.Rows, error) {
	return tx.connection.QueryContext(ctx, query, arguments...)
}

func (tx *stateWriteTransaction) QueryRowContext(ctx context.Context, query string, arguments ...any) *sql.Row {
	return tx.connection.QueryRowContext(ctx, query, arguments...)
}

func (tx *stateWriteTransaction) Commit() error {
	if tx == nil || tx.done {
		return sql.ErrTxDone
	}
	_, err := tx.connection.ExecContext(context.Background(), "COMMIT")
	if err != nil {
		return err
	}
	tx.done = true
	return tx.connection.Close()
}

func (tx *stateWriteTransaction) Rollback() error {
	if tx == nil || tx.done {
		return sql.ErrTxDone
	}
	tx.done = true
	_, rollbackErr := tx.connection.ExecContext(context.Background(), "ROLLBACK")
	return errors.Join(rollbackErr, tx.connection.Close())
}

func commitStateWrite(ctx context.Context, tx *stateWriteTransaction) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := executeStateCommit(tx); err != nil {
		if errors.Is(err, sql.ErrTxDone) || IsStateDatabaseBusy(err) {
			return err
		}
		return &statefile.CommitOutcomeUnknownError{Cause: fmt.Errorf("commit state database transaction: %w", err)}
	}
	return nil
}

func marshalDatabasePayload(name string, value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", name, err)
	}
	if len(data) > maxDatabasePayloadBytes {
		return nil, fmt.Errorf("%s payload is too large: %d bytes exceeds %d", name, len(data), maxDatabasePayloadBytes)
	}
	return data, nil
}

func decodeDatabasePayload(name string, data []byte, target any) error {
	if len(data) > maxDatabasePayloadBytes {
		return fmt.Errorf("%s payload is too large: %d bytes exceeds %d", name, len(data), maxDatabasePayloadBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode %s: trailing JSON value", name)
		}
		return fmt.Errorf("decode %s trailing data: %w", name, err)
	}
	return nil
}

func boolInteger(value bool) int {
	if value {
		return 1
	}
	return 0
}

func canonicalDatabaseTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func parseDatabaseTime(name string, value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

// IsStateDatabaseBusy reports bounded, retryable SQLite writer contention.
func IsStateDatabaseBusy(err error) bool {
	if errors.Is(err, ErrStateDatabaseBusy) {
		return true
	}
	var sqliteError *sqlite.Error
	if !errors.As(err, &sqliteError) {
		return false
	}
	primaryCode := sqliteError.Code() & 0xff
	return primaryCode == 5 || primaryCode == 6
}
