package session

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/marang/sway-session/internal/statefile"
	"golang.org/x/sys/unix"
	"modernc.org/sqlite"
)

// StateBackupResult describes a validated, self-contained runtime-state image.
// It contains session metadata, not terminal history or configuration files.
type StateBackupResult struct {
	Path          string `json:"path"`
	SizeBytes     int64  `json:"size_bytes"`
	SchemaVersion int    `json:"schema_version"`
	ContextCount  int    `json:"context_count"`
}

// BackupState copies a committed SQLite snapshot, including committed WAL
// frames, while ordinary writers continue. The output must be a new file in an
// existing owner-only directory. Only a complete, validated image is published.
func BackupState(ctx context.Context, root, output string) (StateBackupResult, error) {
	return backupStateWithCopy(ctx, root, output, copyStateBackup)
}

func backupStateWithCopy(ctx context.Context, root, output string, copySnapshot func(context.Context, *stateDatabase, string) error) (StateBackupResult, error) {
	return backupStateWithCopyAndSync(ctx, root, output, copySnapshot, (*os.File).Sync)
}

func backupStateWithCopyAndSync(ctx context.Context, root, output string, copySnapshot func(context.Context, *stateDatabase, string) error, syncFile func(*os.File) error) (StateBackupResult, error) {
	var result StateBackupResult
	if ctx == nil {
		return result, errors.New("state backup context is nil")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	directory, name, err := openStateBackupParent(output)
	if err != nil {
		return result, err
	}
	defer directory.Close()
	if err := requireAbsentStateBackupAt(directory, name); err != nil {
		return result, err
	}
	database, err := openStateDatabase(ctx, root, false)
	if err != nil {
		return result, err
	}
	defer database.Close()
	parentInfo, err := directory.Stat()
	if err != nil {
		return result, err
	}
	rootInfo, err := database.directory.Stat()
	if err != nil {
		return result, err
	}
	if os.SameFile(parentInfo, rootInfo) && reservedStateBackupName(name) {
		return result, errors.New("state backup output uses a reserved state-root name")
	}

	// An exclusive private staging directory also protects SQLite's temporary
	// journal names; no user-chosen destination name is opened by SQLite.
	stageName := ".state-backup-" + rand.Text()
	if err := unix.Mkdirat(int(directory.Fd()), stageName, 0o700); err != nil {
		return result, fmt.Errorf("create backup staging directory: %w", err)
	}
	defer func() { _ = unix.Unlinkat(int(directory.Fd()), stageName, unix.AT_REMOVEDIR) }()
	stageFD, err := unix.Openat(int(directory.Fd()), stageName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return result, err
	}
	stage := os.NewFile(uintptr(stageFD), stageName)
	defer stage.Close()
	defer func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			_ = unix.Unlinkat(stageFD, StateDatabaseFilename+suffix, 0)
		}
	}()
	if err := statefile.CreatePrivateFileAt(stage, StateDatabaseFilename, nil); err != nil {
		return result, err
	}
	values := url.Values{"mode": {"rw"}, "_busy_timeout": {"0"}, "_synchronous": {"full"}}
	destination := stateBackupURI(stage, StateDatabaseFilename, values)
	if err := copySnapshot(ctx, database, destination); err != nil {
		return result, fmt.Errorf("copy state backup: %w", err)
	}
	file, summary, err := openValidatedStateBackupAt(ctx, stage, StateDatabaseFilename)
	if err != nil {
		return result, fmt.Errorf("validate copied state backup: %w", err)
	}
	defer file.Close()
	if err := syncFile(file); err != nil {
		return result, fmt.Errorf("sync state backup: %w", err)
	}
	if err := requireAbsentStateBackupAt(directory, name); err != nil {
		return result, err
	}
	if err := verifyStateBackupFileAt(stage, StateDatabaseFilename, file); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := unix.Renameat2(stageFD, StateDatabaseFilename, int(directory.Fd()), name, unix.RENAME_NOREPLACE); err != nil {
		return result, fmt.Errorf("publish state backup without replacing destination: %w", err)
	}
	// Publication is the cancellation boundary. Once renamed, report a completed
	// image even if cancellation arrives during the durability sync.
	summary.Path = output
	// Persist both sides of the cross-directory rename. Remove the now-empty
	// stage before syncing its parent, so successful publication also persists
	// staging cleanup. Attempt the destination sync even if the source fails.
	stageErr := syncFile(stage)
	removeErr := unix.Unlinkat(int(directory.Fd()), stageName, unix.AT_REMOVEDIR)
	parentErr := syncFile(directory)
	if err := errors.Join(stageErr, removeErr, parentErr); err != nil {
		return summary, &statefile.CommitOutcomeUnknownError{Cause: fmt.Errorf("persist backup publication: %w", err)}
	}
	return summary, nil
}

func copyStateBackup(ctx context.Context, database *stateDatabase, destination string) error {
	return copyStateBackupWithStep(ctx, database, destination, (*sqlite.Backup).Step)
}

func copyStateBackupWithStep(ctx context.Context, database *stateDatabase, destination string, step func(*sqlite.Backup, int32) (bool, error)) error {
	connection, err := database.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	if _, err := connection.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	defer func() { _, _ = connection.ExecContext(context.Background(), "ROLLBACK") }()
	// A read transaction prevents endless backup restarts under sustained WAL
	// writes, and fixes the generation before the first page is copied.
	if _, err := validateStateBackupHeader(ctx, connection); err != nil {
		return err
	}
	return connection.Raw(func(raw any) (resultErr error) {
		backuper, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("SQLite driver does not support online backup")
		}
		backup, err := backuper.NewBackup(destination)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, backup.Finish()) }()
		busyDeadline := time.Now().Add(databaseBusyTimeout)
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := step(backup, 128)
			if err != nil {
				if !IsStateDatabaseBusy(err) || time.Now().After(busyDeadline) {
					return err
				}
				timer := time.NewTimer(5 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
				continue
			}
			if backup.PageCount() > stateDatabaseMaxPageCount {
				return errors.New("state backup exceeds the database page budget")
			}
			if !more {
				return ctx.Err()
			}
			busyDeadline = time.Now().Add(databaseBusyTimeout)
		}
	})
}

func reservedStateBackupName(name string) bool {
	switch name {
	case StateDatabaseFilename, stateAccessFilename, stateRecoveryFilename, stateRecoveryCompletedFilename,
		terminalLifecycleDirectory, desktopApprovalDirectory,
		legacyContextsFilename, legacyLayoutFilename, legacyApplicationSessionDirectory, legacyTerminalActivityDirectory:
		return true
	default:
		return isStateDatabaseSidecar(name)
	}
}

func openStateBackupParent(path string) (*os.File, string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, "", errors.New("state backup path must be a clean absolute file path")
	}
	directory, err := statefile.OpenPrivateDirectory(filepath.Dir(path), false)
	return directory, filepath.Base(path), err
}

func stateBackupURI(directory *os.File, name string, values url.Values) string {
	return (&url.URL{Scheme: "file", Path: fmt.Sprintf("/proc/self/fd/%d/%s", directory.Fd(), name), RawQuery: values.Encode()}).String()
}

func requireAbsentStateBackupAt(directory *os.File, name string) error {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		var stat unix.Stat_t
		err := unix.Fstatat(int(directory.Fd()), name+suffix, &stat, unix.AT_SYMLINK_NOFOLLOW)
		if err == nil {
			return fmt.Errorf("state backup destination %s already exists: %w", name+suffix, os.ErrExist)
		}
		if !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("inspect state backup destination: %w", err)
		}
	}
	return nil
}
