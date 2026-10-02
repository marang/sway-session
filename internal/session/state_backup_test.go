package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/statefile"
	"golang.org/x/sys/unix"
	"modernc.org/sqlite"
)

func backupTestRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	if err := RegistryStoreFor(root).Save(validRegistry()); err != nil {
		t.Fatal(err)
	}
	return root
}

func backupTestOutput(t *testing.T) string {
	t.Helper()
	parent := filepath.Join(t.TempDir(), "backups")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, "backup.sqlite3")
}

func backupTestRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBackupStateIncludesCommittedWALAndAllowsConcurrentWriter(t *testing.T) {
	root := backupTestRoot(t)
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.db.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec("UPDATE registry_preferences SET desktop_indicators=1"); err != nil {
		t.Fatal(err)
	}
	walInfo, err := os.Stat(filepath.Join(root, StateDatabaseFilename+"-wal"))
	if err != nil || walInfo.Size() <= 32 {
		t.Fatalf("expected committed WAL frames: %v %v", walInfo, err)
	}
	output := backupTestOutput(t)
	steps := 0
	result, err := backupStateWithCopy(t.Context(), root, output, func(ctx context.Context, source *stateDatabase, destination string) error {
		return copyStateBackupWithStep(ctx, source, destination, func(backup *sqlite.Backup, pages int32) (bool, error) {
			steps++
			// This commit runs after the source read transaction starts. It
			// must complete while the snapshot continues to expose the old value.
			if steps == 1 {
				writerCtx, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				if _, err := database.db.ExecContext(writerCtx, "UPDATE registry_preferences SET desktop_indicators=0"); err != nil {
					return false, fmt.Errorf("concurrent writer: %w", err)
				}
			}
			return backup.Step(pages)
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if steps == 0 || result.Path != output || result.ContextCount != 1 || result.SchemaVersion != StateDatabaseSchemaVersion || result.SizeBytes <= 0 {
		t.Fatalf("bad summary: %+v steps=%d", result, steps)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != result.SizeBytes {
		t.Fatalf("output stat: %v %v", info, err)
	}
	// Read the standalone image through its ordinary store after moving it
	// into a disposable root; this also proves the image is directly usable.
	readRoot := filepath.Join(t.TempDir(), "restored")
	if err := os.Mkdir(readRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readRoot, StateDatabaseFilename), backupTestRead(t, output), 0o600); err != nil {
		t.Fatal(err)
	}
	var registry Registry
	if err := RegistryStoreFor(readRoot).LoadInto(&registry); err != nil {
		t.Fatal(err)
	}
	if !registry.Preferences.DesktopIndicators {
		t.Fatal("backup omitted committed WAL state or crossed snapshot generations")
	}
	entries, err := os.ReadDir(filepath.Dir(output))
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(output) {
		t.Fatalf("backup left sidecars/staging files: %v %v", entries, err)
	}
}

func TestBackupStateFailuresLeaveNoOutput(t *testing.T) {
	for _, kind := range []string{"canceled before start", "canceled mid copy", "disk full mid copy", "invalid document"} {
		t.Run(kind, func(t *testing.T) {
			root, output := backupTestRoot(t), backupTestOutput(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			copySnapshot := copyStateBackup
			switch kind {
			case "canceled before start":
				cancel()
			case "canceled mid copy", "disk full mid copy":
				copySnapshot = func(ctx context.Context, source *stateDatabase, destination string) error {
					return copyStateBackupWithStep(ctx, source, destination, func(backup *sqlite.Backup, _ int32) (bool, error) {
						more, err := backup.Step(1)
						if err != nil || !more {
							t.Fatalf("expected partial copy, got more=%v err=%v", more, err)
						}
						if kind == "disk full mid copy" {
							return false, unix.ENOSPC
						}
						cancel()
						return more, nil
					})
				}
			case "invalid document":
				backupTestMutate(t, root, `UPDATE contexts SET payload = CAST('{}' AS BLOB)`)
			}
			_, err := backupStateWithCopy(ctx, root, output, copySnapshot)
			if err == nil {
				t.Fatal("failure unexpectedly published a backup")
			}
			if strings.HasPrefix(kind, "canceled") && !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation, got %v", err)
			}
			if kind == "disk full mid copy" && !errors.Is(err, unix.ENOSPC) {
				t.Fatalf("lost I/O failure: %v", err)
			}
			entries, err := os.ReadDir(filepath.Dir(output))
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed backup left output: %v %v", entries, err)
			}
		})
	}
}

func TestBackupStateRefusesExistingAndUnsafeDestinations(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "dangling symlink", "hardlink", "fifo", "directory", "sidecar", "public parent", "symlink parent", "live database", "live sidecar", "relative"} {
		t.Run(kind, func(t *testing.T) {
			root, output := backupTestRoot(t), backupTestOutput(t)
			original := backupTestRead(t, filepath.Join(root, StateDatabaseFilename))
			var err error
			switch kind {
			case "file":
				err = os.WriteFile(output, []byte("keep me"), 0o600)
			case "symlink":
				err = os.Symlink(filepath.Join(root, StateDatabaseFilename), output)
			case "dangling symlink":
				err = os.Symlink(filepath.Join(root, "missing"), output)
			case "hardlink":
				err = os.Link(filepath.Join(root, StateDatabaseFilename), output)
			case "fifo":
				err = unix.Mkfifo(output, 0o600)
			case "directory":
				err = os.Mkdir(output, 0o700)
			case "sidecar":
				err = os.WriteFile(output+"-wal", []byte("keep me"), 0o600)
			case "public parent":
				err = os.Chmod(filepath.Dir(output), 0o755)
			case "symlink parent":
				link := filepath.Join(t.TempDir(), "linked")
				err = os.Symlink(filepath.Dir(output), link)
				output = filepath.Join(link, "backup.sqlite3")
			case "live database":
				output = filepath.Join(root, StateDatabaseFilename)
			case "live sidecar":
				output = filepath.Join(root, StateDatabaseFilename+"-wal")
			case "relative":
				output = "backup.sqlite3"
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := BackupState(t.Context(), root, output); err == nil {
				t.Fatal("unsafe destination accepted")
			}
			if !bytes.Equal(original, backupTestRead(t, filepath.Join(root, StateDatabaseFilename))) {
				t.Fatal("source changed")
			}
			if kind == "file" && string(backupTestRead(t, output)) != "keep me" {
				t.Fatal("existing output was overwritten")
			}
		})
	}
}

func TestBackupStatePublicationDoesNotReplaceRacingDestination(t *testing.T) {
	root, output := backupTestRoot(t), backupTestOutput(t)
	_, err := backupStateWithCopy(t.Context(), root, output, func(ctx context.Context, source *stateDatabase, destination string) error {
		if err := copyStateBackup(ctx, source, destination); err != nil {
			return err
		}
		return os.WriteFile(output, []byte("racing writer"), 0o600)
	})
	if !errors.Is(err, os.ErrExist) || string(backupTestRead(t, output)) != "racing writer" {
		t.Fatalf("destination was replaced or error lost: %v", err)
	}
}

func TestBackupStateReservesStateRootMetadata(t *testing.T) {
	for _, name := range []string{stateRecoveryFilename, stateRecoveryCompletedFilename, stateAccessFilename, StateDatabaseFilename, StateDatabaseFilename + "-wal", StateDatabaseFilename + "-shm", StateDatabaseFilename + "-journal", terminalLifecycleDirectory, desktopApprovalDirectory, legacyContextsFilename, legacyLayoutFilename, legacyApplicationSessionDirectory, legacyTerminalActivityDirectory} {
		t.Run(name, func(t *testing.T) {
			root := backupTestRoot(t)
			path := filepath.Join(root, name)
			before, beforeErr := os.ReadFile(path)
			if _, err := BackupState(t.Context(), root, path); err == nil {
				t.Fatal("backup accepted an internal state metadata path")
			}
			after, afterErr := os.ReadFile(path)
			if errors.Is(beforeErr, os.ErrNotExist) {
				if !errors.Is(afterErr, os.ErrNotExist) {
					t.Fatal("backup created an internal metadata file")
				}
			} else if beforeErr != nil || afterErr != nil || !bytes.Equal(before, after) {
				t.Fatal("backup changed an internal metadata file")
			}
			// The restriction belongs to the live root, not the basename.
			elsewhere := filepath.Join(filepath.Dir(backupTestOutput(t)), name)
			if _, err := BackupState(t.Context(), root, elsewhere); err != nil {
				t.Fatalf("reserved basename rejected outside state root: %v", err)
			}
		})
	}
}

func TestBackupStateCannotBlockFutureTerminalLifecycle(t *testing.T) {
	root := backupTestRoot(t)
	path := filepath.Join(root, terminalLifecycleDirectory)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fixture already has a lifecycle directory")
	}
	if _, err := BackupState(t.Context(), root, path); err == nil {
		t.Fatal("backup accepted an absent lifecycle directory as output")
	}
	called := false
	if err := WithTerminalLifecycleLockContext(t.Context(), root, func() error {
		called = true
		return nil
	}); err != nil || !called {
		t.Fatalf("rejected backup blocked subsequent lifecycle work: %v", err)
	}
}

func TestBackupStatePublicationSyncFailures(t *testing.T) {
	for _, fail := range []string{"none", "image", "stage", "parent"} {
		t.Run(fail, func(t *testing.T) {
			root, output := backupTestRoot(t), backupTestOutput(t)
			parentInfo, err := os.Stat(filepath.Dir(output))
			if err != nil {
				t.Fatal(err)
			}
			var calls []string
			result, err := backupStateWithCopyAndSync(t.Context(), root, output, copyStateBackup, func(file *os.File) error {
				info, err := file.Stat()
				if err != nil {
					return err
				}
				kind := "image"
				if info.IsDir() {
					kind = "stage"
					if os.SameFile(info, parentInfo) {
						kind = "parent"
					}
				}
				calls = append(calls, kind)
				if kind == "image" {
					if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("output was published before image sync")
					}
				} else if _, err := ValidateStateBackup(t.Context(), output); err != nil {
					t.Fatalf("directory sync did not follow complete publication: %v", err)
				}
				if kind == fail {
					return unix.EIO
				}
				return file.Sync()
			})
			if fail == "image" {
				if !errors.Is(err, unix.EIO) || strings.Join(calls, ",") != "image" || result.Path != "" {
					t.Fatalf("prepublication sync failure: calls=%v result=%+v err=%v", calls, result, err)
				}
				if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("image sync failure left a published output")
				}
				return
			}
			if strings.Join(calls, ",") != "image,stage,parent" {
				t.Fatalf("missing or out-of-order durability sync: %v", calls)
			}
			if fail == "none" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var uncertain *statefile.CommitOutcomeUnknownError
				if !errors.As(err, &uncertain) || !errors.Is(err, unix.EIO) {
					t.Fatalf("directory sync failure lost publication uncertainty: %v", err)
				}
			}
			if result.Path != output {
				t.Fatalf("published output missing from result: %+v", result)
			}
			if _, err := ValidateStateBackup(t.Context(), output); err != nil {
				t.Fatalf("published output was removed or damaged: %v", err)
			}
			entries, err := os.ReadDir(filepath.Dir(output))
			if err != nil || len(entries) != 1 {
				t.Fatalf("staging cleanup failed: %v %v", entries, err)
			}
		})
	}
}
