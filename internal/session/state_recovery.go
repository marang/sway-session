package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/marang/sway-session/internal/statefile"
	"golang.org/x/sys/unix"
	"modernc.org/sqlite"
)

// StateRecoveryResult contains locations and validation status, never private
// context payloads. Previous names a standalone backup, or a preserved raw
// SQLite bundle when the previous database could not be validated.
type StateRecoveryResult struct {
	Source        string `json:"source"`
	Database      string `json:"database"`
	Applied       bool   `json:"applied"`
	Previous      string `json:"previous,omitempty"`
	PreviousValid bool   `json:"previous_valid"`
	Resumed       bool   `json:"resumed"`
}

type recoveryFile struct {
	Name   string `json:"name"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Size   int64  `json:"size"`
	Digest string `json:"sha256"`
}

type recoveryDirectory struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

const stateRecoveryCompletedFilename = "state-recovery-completed.json"

type recoveryManifest struct {
	Version           int               `json:"version"`
	ID                ContextID         `json:"id"`
	SourceDigest      string            `json:"source_sha256"`
	Replacement       recoveryFile      `json:"replacement"`
	Previous          []recoveryFile    `json:"previous"`
	PreviousValid     bool              `json:"previous_valid"`
	StageDirectory    recoveryDirectory `json:"stage_directory"`
	PreviousDirectory recoveryDirectory `json:"previous_directory"`
}

// recoveryBoundary and recoverySyncDirectory are internal fault-injection
// seams. Subprocess tests can terminate between rename and sync as well as at
// durable boundaries, without deferred cleanup disguising a crash.
var recoveryBoundary = func(string) error { return nil }
var recoverySyncDirectory = func(directory *os.File) error { return directory.Sync() }

// RecoverState validates a standalone metadata snapshot. Apply requires the
// caller to hold the daemon's runtime lock; all participating state handles and
// lifecycle operations are independently excluded by the permanent state gate.
// Preview never creates the state root or opens the current database.
func RecoverState(ctx context.Context, root, source string, apply bool) (StateRecoveryResult, error) {
	result := StateRecoveryResult{Source: source, Database: filepath.Join(root, StateDatabaseFilename)}
	if ctx == nil || root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return result, errors.New("recovery requires a context and a clean absolute state root")
	}
	input, _, err := openValidatedStateBackup(ctx, source)
	if err != nil {
		return result, fmt.Errorf("validate recovery input: %w", err)
	}
	defer input.Close()
	inputIdentity, err := describeRecoveryFile(ctx, input, filepath.Base(source))
	if err != nil {
		return result, err
	}
	if !apply {
		return result, rejectRecoveryAlias(root, inputIdentity)
	}
	directory, err := statefile.OpenPrivateDirectory(root, true)
	if err != nil {
		return result, err
	}
	defer directory.Close()
	gate, err := acquireStateAccessAt(ctx, directory, true)
	if err != nil {
		return result, err
	}
	defer gate.Close()
	// Retain the existing lock domains as well, including older lifecycle and
	// registry clients which cooperate with them. Never wait while recovering.
	if err := recoveryTryLock(directory); err != nil {
		return result, err
	}
	defer unix.Flock(int(directory.Fd()), unix.LOCK_UN)
	lifecycle, err := statefile.OpenPrivateDirectory(filepath.Join(root, terminalLifecycleDirectory), true)
	if err != nil {
		return result, err
	}
	defer lifecycle.Close()
	if err := recoveryTryLock(lifecycle); err != nil {
		return result, err
	}
	defer unix.Flock(int(lifecycle.Fd()), unix.LOCK_UN)

	manifest, found, err := loadRecoveryManifest(directory)
	if err != nil {
		return result, fmt.Errorf("inspect pending recovery: %w", err)
	}
	completed, receiptFound, err := loadRecoveryRecord(directory, stateRecoveryCompletedFilename)
	if err != nil {
		return result, fmt.Errorf("inspect completed recovery: %w", err)
	}
	if found {
		result.Resumed = true
		if manifest.SourceDigest != inputIdentity.Digest {
			return result, fmt.Errorf("%w: recovery input differs from the pending operation", ErrStateRecoveryPending)
		}
		if receiptFound && completed.ID == manifest.ID {
			if !sameRecoveryOperation(completed, manifest) {
				return result, fmt.Errorf("%w: pending recovery and completion receipt disagree", ErrStateRecoveryPending)
			}
			setRecoveryPrevious(&result, root, manifest)
			if err := validateCompletedInstallation(ctx, directory, root, manifest); err != nil {
				return result, fmt.Errorf("%w: inspect completed installation: %w", ErrStateRecoveryPending, err)
			}
			// Receipt publication is durable before marker retirement. If an
			// unsynced unlink reappears after power loss, ordinary users may
			// already have advanced this installed database. Retire the marker
			// without replaying installation or discarding their new WAL.
			result.Applied = true
			return result, retireRecoveryMarker(directory)
		}
	} else {
		if receiptFound && completed.SourceDigest == inputIdentity.Digest && matchRecoveryFile(ctx, directory, StateDatabaseFilename, completed.Replacement) == nil && requireAbsentStateBackupSidecarsAt(directory, StateDatabaseFilename) == nil {
			setRecoveryPrevious(&result, root, completed)
			result.Applied, result.Resumed = true, true
			if err := validateCompletedRecovery(ctx, root, completed); err != nil {
				return result, fmt.Errorf("completed recovery archive is unavailable: %w", err)
			}
			if err := recoverySyncDirectory(directory); err != nil {
				return result, &statefile.CommitOutcomeUnknownError{Cause: fmt.Errorf("confirm completed recovery: %w", err)}
			}
			return result, nil
		}
		if err := rejectRecoveryAliasAt(directory, inputIdentity); err != nil {
			return result, err
		}
		manifest, err = prepareStateRecovery(ctx, directory, root, input, inputIdentity.Digest)
		if err != nil {
			if stateRecoveryPendingAt(directory) != nil {
				setRecoveryPrevious(&result, root, manifest)
				return result, fmt.Errorf("%w: %w", ErrStateRecoveryPending, err)
			}
			return result, err
		}
	}
	setRecoveryPrevious(&result, root, manifest)
	if err := completeStateRecovery(ctx, directory, root, manifest); err != nil {
		if stateRecoveryPendingAt(directory) == nil {
			result.Applied = true
			return result, &statefile.CommitOutcomeUnknownError{Cause: err}
		}
		return result, fmt.Errorf("%w: resume with the same state recover --from PATH --yes command: %w", ErrStateRecoveryPending, err)
	}
	result.Applied = true
	return result, nil
}

func setRecoveryPrevious(result *StateRecoveryResult, root string, manifest recoveryManifest) {
	stagePath := filepath.Join(root, recoveryDirectoryName(manifest.ID))
	if len(manifest.Previous) != 0 {
		result.Previous = filepath.Join(stagePath, "previous")
		if manifest.PreviousValid {
			result.Previous = filepath.Join(stagePath, "previous.sqlite3")
			result.PreviousValid = true
		}
	}
}

func recoveryDirectoryName(id ContextID) string { return "recovery-" + string(id) }

func recoveryTryLock(file *os.File) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("%w: state lifecycle or registry writer is active", ErrStateDatabaseBusy)
		}
		return err
	}
	return nil
}

func rejectRecoveryAlias(root string, input recoveryFile) error {
	directory, err := statefile.OpenPrivateDirectory(root, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer directory.Close()
	return rejectRecoveryAliasAt(directory, input)
}

func rejectRecoveryAliasAt(directory *os.File, input recoveryFile) error {
	for _, name := range append(recoveryDatabaseNames(), stateAccessFilename, stateRecoveryFilename, stateRecoveryCompletedFilename) {
		stat, exists, err := inspectPrivateDatabaseObjectAt(directory, name)
		if err != nil {
			return err
		}
		if exists && stat.Dev == input.Device && stat.Ino == input.Inode {
			return errors.New("recovery input aliases the current database or its sidecars")
		}
	}
	return nil
}

func recoveryDatabaseNames() []string {
	return []string{StateDatabaseFilename, StateDatabaseFilename + "-wal", StateDatabaseFilename + "-shm", StateDatabaseFilename + "-journal"}
}

func prepareStateRecovery(ctx context.Context, directory *os.File, root string, input *os.File, digest string) (manifest recoveryManifest, err error) {
	manifest.Version, manifest.SourceDigest = 1, digest
	manifest.ID, err = NewContextID()
	if err != nil {
		return manifest, err
	}
	stageName := recoveryDirectoryName(manifest.ID)
	if err := unix.Mkdirat(int(directory.Fd()), stageName, 0o700); err != nil {
		return manifest, err
	}
	stage, err := statefile.OpenPrivateDirectory(filepath.Join(root, stageName), false)
	if err != nil {
		return manifest, err
	}
	defer stage.Close()
	// Until the journal is published, every file in this private staging
	// directory belongs solely to this attempt; live names are still untouched.
	published := false
	defer func() {
		if !published {
			cleanupRecoveryPreparation(directory, stage, stageName)
		}
	}()
	if err := unix.Mkdirat(int(stage.Fd()), "previous", 0o700); err != nil {
		return manifest, err
	}
	previous, err := statefile.OpenPrivateDirectory(filepath.Join(root, stageName, "previous"), false)
	if err != nil {
		return manifest, err
	}
	defer previous.Close()
	manifest.StageDirectory, err = describeRecoveryDirectory(stage)
	if err != nil {
		return manifest, err
	}
	manifest.PreviousDirectory, err = describeRecoveryDirectory(previous)
	if err != nil {
		return manifest, err
	}
	if err := copyRecoveryFile(ctx, input, stage, "replacement.sqlite3"); err != nil {
		return manifest, err
	}
	if _, err := validateStateBackupAt(ctx, stage, "replacement.sqlite3"); err != nil {
		return manifest, fmt.Errorf("validate staged recovery input: %w", err)
	}
	staged, err := describeRecoveryFileAt(ctx, stage, "replacement.sqlite3")
	if err != nil || staged.Digest != digest {
		return manifest, errors.Join(err, errors.New("recovery input changed during staging"))
	}
	// The portable backup may use rollback journaling. Only its staged copy is
	// converted to the runtime's required WAL mode, with all handles closed
	// before its identity is recorded and it can become the live database.
	if err := configureRecoveredDatabase(ctx, stage); err != nil {
		return manifest, err
	}
	manifest.Replacement, err = describeRecoveryFileAt(ctx, stage, "replacement.sqlite3")
	if err != nil {
		return manifest, err
	}
	if manifest.PreviousValid, err = preparePreviousBackup(ctx, directory, stage, filepath.Join(root, stageName)); err != nil {
		return manifest, err
	}
	for _, name := range recoveryDatabaseNames() {
		_, exists, err := inspectPrivateDatabaseObjectAt(directory, name)
		if err != nil {
			return manifest, err
		}
		if exists {
			file, err := openRecoveryRegularAt(directory, name)
			if err != nil {
				return manifest, err
			}
			// Preserve the raw bundle durably too, including a corrupt image
			// for which no normalized previous.sqlite3 can be produced.
			syncErr := file.Sync()
			previous, describeErr := describeRecoveryFile(ctx, file, name)
			err = errors.Join(syncErr, describeErr, file.Close())
			if err != nil {
				return manifest, err
			}
			manifest.Previous = append(manifest.Previous, previous)
		}
	}
	if err := recoverySyncDirectory(previous); err != nil {
		return manifest, err
	}
	if err := recoverySyncDirectory(stage); err != nil {
		return manifest, err
	}
	if err := recoverySyncDirectory(directory); err != nil {
		return manifest, err
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return manifest, err
	}
	published, err = publishRecoveryRecord(directory, stateRecoveryFilename, data, false)
	if err != nil {
		// Publication reports whether rename succeeded directly; an I/O error
		// inspecting the marker must never trigger deletion of its dependencies.
		return manifest, err
	}
	published = true
	return manifest, recoveryBoundary("journal-published")
}

func configureRecoveredDatabase(ctx context.Context, stage *os.File) error {
	db, err := openSQLiteStateDatabase(ctx, stateBackupURI(stage, "replacement.sqlite3", url.Values{"mode": {"rw"}, "_busy_timeout": {"0"}, "_synchronous": {"full"}}))
	if err != nil {
		return err
	}
	var mode string
	err = db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode)
	if err == nil && mode != "wal" {
		err = errors.New("recovered database refused WAL mode")
	}
	err = errors.Join(err, db.Close())
	if err != nil {
		return err
	}
	file, _, err := openValidatedStateBackupAt(ctx, stage, "replacement.sqlite3")
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func preparePreviousBackup(ctx context.Context, current, stage *os.File, stagePath string) (bool, error) {
	_, exists, err := inspectDatabaseAt(current)
	if err != nil || !exists {
		return false, err
	}
	// Even a read-only SQLite open can recover a journal or update shared WAL
	// metadata. Normalize only a disposable copy; the original bundle remains
	// byte-for-byte intact until it is moved into the previous-state archive.
	if err := unix.Mkdirat(int(stage.Fd()), "previous-work", 0o700); err != nil {
		return false, err
	}
	work, err := statefile.OpenPrivateDirectory(filepath.Join(stagePath, "previous-work"), false)
	if err != nil {
		return false, err
	}
	defer func() {
		for _, name := range recoveryDatabaseNames() {
			_ = unix.Unlinkat(int(work.Fd()), name, 0)
		}
		_ = work.Close()
		_ = unix.Unlinkat(int(stage.Fd()), "previous-work", unix.AT_REMOVEDIR)
	}()
	for _, name := range recoveryDatabaseNames() {
		_, present, err := inspectRecoveryRegularAt(current, name)
		if err != nil {
			return false, err
		}
		if !present {
			continue
		}
		file, err := openRecoveryRegularAt(current, name)
		if err != nil {
			return false, err
		}
		copyErr := copyRecoveryFile(ctx, file, work, name)
		if err := errors.Join(copyErr, file.Close()); err != nil {
			return false, err
		}
	}
	values := url.Values{"mode": {"rw"}, "_busy_timeout": {"0"}, "_defensive": {"1"}, "_pragma": {"trusted_schema(OFF)", "mmap_size(0)"}}
	db, err := openSQLiteStateDatabase(ctx, stateBackupURI(work, StateDatabaseFilename, values))
	if err != nil {
		return false, recoveryOldValidationFailure(ctx, err)
	}
	defer db.Close()
	if _, err := validateStateBackupDatabase(ctx, db); err != nil {
		return false, recoveryOldValidationFailure(ctx, err)
	}
	if err := statefile.CreatePrivateFileAt(stage, "previous.sqlite3", nil); err != nil {
		return false, err
	}
	destination := stateBackupURI(stage, "previous.sqlite3", url.Values{"mode": {"rw"}, "_busy_timeout": {"0"}, "_synchronous": {"full"}})
	if err := copyStateBackup(ctx, &stateDatabase{db: db}, destination); err != nil {
		return false, fmt.Errorf("preserve previous database: %w", err)
	}
	file, _, err := openValidatedStateBackupAt(ctx, stage, "previous.sqlite3")
	if err != nil {
		return false, err
	}
	defer file.Close()
	return true, file.Sync()
}

// Invalid or unsupported old metadata is retained verbatim in the raw bundle.
// Cancellation, contention and storage errors must not be called corruption.
func recoveryOldValidationFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if IsStateDatabaseBusy(err) || errors.Is(err, os.ErrPermission) {
		return err
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return err
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() & 0xff {
		case 7, 8, 10, 13, 14: // NOMEM, READONLY, IOERR, FULL, CANTOPEN.
			return err
		}
	}
	return nil
}

func openRecoveryDirectories(root string, manifest recoveryManifest) (*os.File, *os.File, error) {
	stage, err := statefile.OpenPrivateDirectory(filepath.Join(root, recoveryDirectoryName(manifest.ID)), false)
	if err != nil {
		return nil, nil, err
	}
	previous, err := statefile.OpenPrivateDirectory(filepath.Join(root, recoveryDirectoryName(manifest.ID), "previous"), false)
	if err != nil {
		_ = stage.Close()
		return nil, nil, err
	}
	if err := errors.Join(matchRecoveryDirectory(stage, manifest.StageDirectory), matchRecoveryDirectory(previous, manifest.PreviousDirectory)); err != nil {
		_ = previous.Close()
		_ = stage.Close()
		return nil, nil, err
	}
	return stage, previous, nil
}

func validateCompletedRecovery(ctx context.Context, root string, manifest recoveryManifest) error {
	stage, previous, err := openRecoveryDirectories(root, manifest)
	if err != nil {
		return err
	}
	defer stage.Close()
	defer previous.Close()
	for _, file := range manifest.Previous {
		if err := matchRecoveryFile(ctx, previous, file.Name, file); err != nil {
			return err
		}
	}
	if manifest.PreviousValid {
		_, err := validateStateBackupAt(ctx, stage, "previous.sqlite3")
		return err
	}
	return ctx.Err()
}

func sameRecoveryOperation(a, b recoveryManifest) bool {
	return a.Version == b.Version && a.ID == b.ID && a.SourceDigest == b.SourceDigest && a.Replacement == b.Replacement && a.PreviousValid == b.PreviousValid && a.StageDirectory == b.StageDirectory && a.PreviousDirectory == b.PreviousDirectory && slices.Equal(a.Previous, b.Previous)
}

func validateCompletedInstallation(ctx context.Context, directory *os.File, root string, manifest recoveryManifest) error {
	if err := validateCompletedRecovery(ctx, root, manifest); err != nil {
		return err
	}
	stage, previous, err := openRecoveryDirectories(root, manifest)
	if err != nil {
		return err
	}
	defer stage.Close()
	defer previous.Close()
	if _, present, err := inspectRecoveryRegularAt(stage, "replacement.sqlite3"); err != nil || present {
		return errors.Join(err, errors.New("completed recovery still has a staged replacement"))
	}
	stat, present, err := inspectPrivateDatabaseObjectAt(directory, StateDatabaseFilename)
	if err != nil {
		return err
	}
	if !present || stat.Dev != manifest.Replacement.Device || stat.Ino != manifest.Replacement.Inode {
		return errors.New("completed recovery database identity changed")
	}
	return inspectDatabaseSidecarsAt(directory)
}

func completeStateRecovery(ctx context.Context, directory *os.File, root string, manifest recoveryManifest) error {
	stage, previous, err := openRecoveryDirectories(root, manifest)
	if err != nil {
		return err
	}
	defer stage.Close()
	defer previous.Close()
	// Decide where the replacement is before moving anything. A missing stage
	// is accepted only when the live entry is this exact validated replacement.
	installed := false
	_, replacementPresent, err := inspectRecoveryRegularAt(stage, "replacement.sqlite3")
	if err != nil {
		return err
	}
	if replacementPresent {
		if err := matchRecoveryFile(ctx, stage, "replacement.sqlite3", manifest.Replacement); err != nil {
			return err
		}
	} else {
		if err := matchRecoveryFile(ctx, directory, StateDatabaseFilename, manifest.Replacement); err != nil {
			return fmt.Errorf("replacement absent from staging and target: %w", err)
		}
		installed = true
	}
	for _, old := range manifest.Previous {
		_, archived, err := inspectRecoveryRegularAt(previous, old.Name)
		if err != nil {
			return err
		}
		if archived {
			if err := matchRecoveryFile(ctx, previous, old.Name, old); err != nil {
				return err
			}
			if !(installed && old.Name == StateDatabaseFilename) {
				if _, present, err := inspectRecoveryRegularAt(directory, old.Name); err != nil || present {
					return errors.Join(err, errors.New("both old live and archived database entries exist"))
				}
			}
			continue
		}
		if installed {
			return errors.New("installed replacement has an incomplete previous-state archive")
		}
		if err := matchRecoveryFile(ctx, directory, old.Name, old); err != nil {
			return err
		}
		if err := unix.Renameat2(int(directory.Fd()), old.Name, int(previous.Fd()), old.Name, unix.RENAME_NOREPLACE); err != nil {
			return err
		}
		if err := recoveryBoundary("archive-renamed-" + old.Name); err != nil {
			return err
		}
		if err := syncRecoveryRename(previous, directory); err != nil {
			return err
		}
		if err := recoveryBoundary("archived-" + old.Name); err != nil {
			return err
		}
	}
	// A prior attempt may have returned after rename but before either sync.
	// Seeing the archived entry on retry does not prove it is durable yet.
	if err := syncRecoveryRename(previous, directory); err != nil {
		return err
	}
	// Reject unexpected sidecars, including an old file not listed in the
	// durable manifest. SQLite must never see a mixture of generations.
	for _, name := range recoveryDatabaseNames() {
		if installed && name == StateDatabaseFilename {
			continue
		}
		if _, exists, err := inspectRecoveryRegularAt(directory, name); err != nil || exists {
			return errors.Join(err, errors.New("unexpected database or sidecar blocks recovery installation"))
		}
	}
	if !installed {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := unix.Renameat2(int(stage.Fd()), "replacement.sqlite3", int(directory.Fd()), StateDatabaseFilename, unix.RENAME_NOREPLACE); err != nil {
			return err
		}
		if err := syncRecoveryRename(directory, stage); err != nil {
			return err
		}
		if err := recoveryBoundary("replacement-installed"); err != nil {
			return err
		}
	}
	// Also persist the source-directory removal when resuming an installed
	// replacement whose original stage sync may have failed.
	if err := recoverySyncDirectory(stage); err != nil {
		return err
	}
	if _, err := validateStateBackupAt(ctx, directory, StateDatabaseFilename); err != nil {
		return err
	}
	if manifest.PreviousValid {
		if _, err := validateStateBackupAt(ctx, stage, "previous.sqlite3"); err != nil {
			return fmt.Errorf("previous backup is no longer valid: %w", err)
		}
	}
	if err := recoverySyncDirectory(directory); err != nil {
		return err
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if _, err := publishRecoveryRecord(directory, stateRecoveryCompletedFilename, data, true); err != nil {
		return err
	}
	if err := recoveryBoundary("receipt-published"); err != nil {
		return err
	}
	if err := recoveryBoundary("before-journal-removal"); err != nil {
		return err
	}
	return retireRecoveryMarker(directory)
}

func syncRecoveryRename(destination, source *os.File) error {
	if err := recoverySyncDirectory(destination); err != nil {
		return err
	}
	return recoverySyncDirectory(source)
}

func retireRecoveryMarker(directory *os.File) error {
	if err := unix.Unlinkat(int(directory.Fd()), stateRecoveryFilename, 0); err != nil {
		return fmt.Errorf("%w: retire completed recovery marker: %w", ErrStateRecoveryPending, err)
	}
	if err := recoverySyncDirectory(directory); err != nil {
		return &statefile.CommitOutcomeUnknownError{Cause: fmt.Errorf("sync completed recovery: %w", err)}
	}
	return recoveryBoundary("journal-removed")
}

func loadRecoveryManifest(directory *os.File) (recoveryManifest, bool, error) {
	return loadRecoveryRecord(directory, stateRecoveryFilename)
}

func loadRecoveryRecord(directory *os.File, name string) (recoveryManifest, bool, error) {
	var manifest recoveryManifest
	_, present, err := inspectRecoveryRegularAt(directory, name)
	if err != nil || !present {
		return manifest, present, err
	}
	file, err := openRecoveryRegularAt(directory, name)
	if err != nil {
		return manifest, true, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil || len(data) > 64*1024 {
		return manifest, true, errors.Join(err, errors.New("recovery manifest exceeds safety budget"))
	}
	if err := decodeDatabasePayload("recovery manifest", data, &manifest); err != nil {
		return manifest, true, err
	}
	if manifest.Version != 1 || manifest.ID.Validate() != nil || !validRecoveryDigest(manifest.SourceDigest) || manifest.Replacement.Name != "replacement.sqlite3" || !validRecoveryFile(manifest.Replacement) || manifest.StageDirectory.Inode == 0 || manifest.PreviousDirectory.Inode == 0 {
		return manifest, true, errors.New("invalid or unsupported recovery manifest")
	}
	seen := make(map[string]bool)
	for _, old := range manifest.Previous {
		if (old.Name != StateDatabaseFilename && !isStateDatabaseSidecar(old.Name)) || seen[old.Name] || !validRecoveryFile(old) {
			return manifest, true, errors.New("invalid previous recovery file identity")
		}
		seen[old.Name] = true
	}
	if len(manifest.Previous) > 4 || (manifest.PreviousValid && !seen[StateDatabaseFilename]) {
		return manifest, true, errors.New("invalid previous recovery bundle")
	}
	return manifest, true, nil
}

func validRecoveryDigest(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

func validRecoveryFile(file recoveryFile) bool {
	return file.Inode != 0 && file.Size >= 0 && file.Size <= maxStateDatabaseBytes*2 && validRecoveryDigest(file.Digest)
}

func inspectRecoveryRegularAt(directory *os.File, name string) (unix.Stat_t, bool, error) {
	stat, present, err := inspectPrivateDatabaseObjectAt(directory, name)
	if err == nil && present && (stat.Size < 0 || stat.Size > maxStateDatabaseBytes*2) {
		err = errors.New("recovery file exceeds safety budget")
	}
	return stat, present, err
}

func openRecoveryRegularAt(directory *os.File, name string) (*os.File, error) {
	before, present, err := inspectRecoveryRegularAt(directory, name)
	if err != nil || !present {
		if err == nil {
			err = os.ErrNotExist
		}
		return nil, err
	}
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	var current unix.Stat_t
	if err := unix.Fstat(fd, &current); err != nil || current.Dev != before.Dev || current.Ino != before.Ino || current.Nlink != 1 {
		_ = file.Close()
		return nil, errors.Join(err, errors.New("recovery file changed while opening"))
	}
	return file, nil
}

func describeRecoveryFileAt(ctx context.Context, directory *os.File, name string) (recoveryFile, error) {
	file, err := openRecoveryRegularAt(directory, name)
	if err != nil {
		return recoveryFile{}, err
	}
	defer file.Close()
	return describeRecoveryFile(ctx, file, name)
}

func describeRecoveryFile(ctx context.Context, file *os.File, name string) (recoveryFile, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return recoveryFile{}, err
	}
	if stat.Size < 0 || stat.Size > maxStateDatabaseBytes*2 {
		return recoveryFile{}, errors.New("recovery file exceeds safety budget")
	}
	hash := sha256.New()
	if err := copyRecoveryBytes(ctx, hash, io.NewSectionReader(file, 0, stat.Size)); err != nil {
		return recoveryFile{}, err
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &after); err != nil {
		return recoveryFile{}, err
	}
	if stat.Size != after.Size || stat.Mtim != after.Mtim || stat.Ctim != after.Ctim {
		return recoveryFile{}, errors.New("recovery file changed during inspection")
	}
	return recoveryFile{Name: name, Device: stat.Dev, Inode: stat.Ino, Size: stat.Size, Digest: hex.EncodeToString(hash.Sum(nil))}, nil
}

func matchRecoveryFile(ctx context.Context, directory *os.File, name string, want recoveryFile) error {
	actual, err := describeRecoveryFileAt(ctx, directory, name)
	if err != nil {
		return err
	}
	actual.Name = want.Name
	if actual != want {
		return fmt.Errorf("recovery file identity changed: %s", name)
	}
	return nil
}

func copyRecoveryFile(ctx context.Context, input *os.File, directory *os.File, name string) error {
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if err := copyRecoveryBytes(ctx, file, io.NewSectionReader(input, 0, info.Size())); err != nil {
		return err
	}
	return file.Sync()
}

func copyRecoveryBytes(ctx context.Context, destination io.Writer, source io.Reader) error {
	buffer := make([]byte, 128*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := source.Read(buffer)
		if n > 0 {
			written, writeErr := destination.Write(buffer[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func publishRecoveryRecord(directory *os.File, target string, data []byte, replace bool) (bool, error) {
	if _, _, err := inspectRecoveryRegularAt(directory, target); err != nil {
		return false, err
	}
	id, err := NewContextID()
	if err != nil {
		return false, err
	}
	name := target + ".preparing-" + string(id)
	if err := statefile.CreatePrivateFileAt(directory, name, data); err != nil {
		return false, err
	}
	defer unix.Unlinkat(int(directory.Fd()), name, 0)
	flags := uint(unix.RENAME_NOREPLACE)
	if replace {
		flags = 0
	}
	if err := unix.Renameat2(int(directory.Fd()), name, int(directory.Fd()), target, flags); err != nil {
		return false, err
	}
	return true, recoverySyncDirectory(directory)
}

func describeRecoveryDirectory(directory *os.File) (recoveryDirectory, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		return recoveryDirectory{}, err
	}
	return recoveryDirectory{Device: stat.Dev, Inode: stat.Ino}, nil
}

func matchRecoveryDirectory(directory *os.File, want recoveryDirectory) error {
	got, err := describeRecoveryDirectory(directory)
	if err != nil {
		return err
	}
	if got != want {
		return errors.New("recovery directory identity changed")
	}
	return nil
}

func cleanupRecoveryPreparation(parent, stage *os.File, name string) {
	for _, base := range []string{"replacement.sqlite3", "previous.sqlite3"} {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			_ = unix.Unlinkat(int(stage.Fd()), base+suffix, 0)
		}
	}
	_ = unix.Unlinkat(int(stage.Fd()), "previous", unix.AT_REMOVEDIR)
	_ = unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
}
