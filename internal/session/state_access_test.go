package session

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/statefile"
	"golang.org/x/sys/unix"
)

func accessTestDirectory(t *testing.T, root string) *os.File {
	t.Helper()
	directory, err := statefile.OpenPrivateDirectory(root, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })
	return directory
}

func requireRecoveryBusy(t *testing.T, directory *os.File) {
	t.Helper()
	gate, err := acquireStateAccessAt(t.Context(), directory, true)
	if gate != nil {
		_ = gate.Close()
	}
	if !errors.Is(err, ErrStateDatabaseBusy) {
		t.Fatalf("recovery acquired access during an operation: %v", err)
	}
}

func TestStateAccessDatabaseHandleExcludesRecoveryUntilClose(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	database, err := openStateDatabase(t.Context(), root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	directory := accessTestDirectory(t, root)
	requireRecoveryBusy(t, directory)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	gate, err := acquireStateAccessAt(t.Context(), directory, true)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	for _, create := range []bool{false, true} {
		opened, err := openStateDatabase(t.Context(), root, create)
		if opened != nil {
			_ = opened.Close()
		}
		if !errors.Is(err, ErrStateDatabaseBusy) {
			t.Fatalf("create=%v opened during recovery: %v", create, err)
		}
	}
}

func TestStateAccessOldDatabaseReadDoesNotPublishGate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := initializeStateDatabase(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	// Simulate a database made by a release predating the access gate.
	if err := os.Remove(filepath.Join(root, stateAccessFilename)); err != nil {
		t.Fatal(err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	directory := accessTestDirectory(t, root)
	requireRecoveryBusy(t, directory)
	writer, err := AcquireStateAccess(t.Context(), root, true)
	if writer != nil {
		_ = writer.Close()
	}
	if !errors.Is(err, ErrStateDatabaseBusy) {
		t.Fatalf("first writer bypassed old-database reader: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, stateAccessFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read or rejected recovery created a gate: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	gate, err := acquireStateAccessAt(t.Context(), directory, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	access, err := AcquireStateAccess(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer access.Close()
	// Once published, readers use the independent gate and leave the registry
	// lock free, preserving completion while a registry writer is active.
	if err := unix.Flock(int(directory.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("reader retained fallback root lock after gate publication: %v", err)
	}
	defer unix.Flock(int(directory.Fd()), unix.LOCK_UN)
	requireRecoveryBusy(t, directory)
}

func TestStateAccessMissingReadsCreateNothing(t *testing.T) {
	for _, existingRoot := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-root", true: "empty-root"}[existingRoot], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if existingRoot {
				accessTestDirectory(t, root)
			}
			if _, err := openStateDatabase(t.Context(), root, false); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing read: %v", err)
			}
			entries, err := os.ReadDir(root)
			if existingRoot {
				if err != nil || len(entries) != 0 {
					t.Fatalf("read changed empty root: entries=%v err=%v", entries, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("read created missing root: %v", err)
			}
		})
	}
}

func TestStateAccessNoncreatingActivityWriterUsesFallback(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := RegistryStoreFor(root).SaveContext(t.Context(), validRegistry()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, stateAccessFilename)); err != nil {
		t.Fatal(err)
	}
	directory := accessTestDirectory(t, root)
	original := executeStateCommit
	checked := false
	executeStateCommit = func(tx *stateWriteTransaction) error {
		requireRecoveryBusy(t, directory)
		checked = true
		return original(tx)
	}
	t.Cleanup(func() { executeStateCommit = original })
	if err := RecordTerminalCreationContext(t.Context(), root, testContextID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("activity commit was not exercised")
	}
	if _, err := os.Lstat(filepath.Join(root, stateAccessFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("noncreating activity writer published the gate: %v", err)
	}
}

func TestStateAccessPendingMarkerFailsClosedBeforeCreation(t *testing.T) {
	for _, marker := range []string{"malformed", "symlink", "directory"} {
		t.Run(marker, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			directory := accessTestDirectory(t, root)
			path := filepath.Join(root, stateRecoveryFilename)
			var err error
			switch marker {
			case "malformed":
				err = os.WriteFile(path, []byte("{"), 0o600)
			case "symlink":
				err = os.Symlink("missing", path)
			case "directory":
				err = os.Mkdir(path, 0o700)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, create := range []bool{false, true} {
				if _, err := openStateDatabase(t.Context(), root, create); !errors.Is(err, ErrStateRecoveryPending) {
					t.Fatalf("database create=%v ignored marker: %v", create, err)
				}
				if _, err := AcquireStateAccess(t.Context(), root, create); !errors.Is(err, ErrStateRecoveryPending) {
					t.Fatalf("operation create=%v ignored marker: %v", create, err)
				}
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 {
				t.Fatalf("pending access created files: entries=%v err=%v", entries, err)
			}
			gate, err := acquireStateAccessAt(t.Context(), directory, true)
			if err != nil {
				t.Fatalf("recovery itself cannot resume: %v", err)
			}
			_ = gate.Close()
		})
	}
}

func TestStateAccessRejectsUnsafeGate(t *testing.T) {
	for _, kind := range []string{"mode", "symlink", "hardlink", "fifo", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			directory := accessTestDirectory(t, root)
			path := filepath.Join(root, stateAccessFilename)
			var err error
			switch kind {
			case "mode":
				err = os.WriteFile(path, nil, 0o600)
				if err == nil {
					err = os.Chmod(path, 0o644)
				}
			case "symlink":
				err = os.Symlink("missing", path)
			case "hardlink":
				err = os.WriteFile(path, nil, 0o600)
				if err == nil {
					err = os.Link(path, filepath.Join(root, "alias"))
				}
			case "fifo":
				err = unix.Mkfifo(path, 0o600)
			case "directory":
				err = os.Mkdir(path, 0o700)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, exclusive := range []bool{false, true} {
				gate, err := acquireStateAccessAt(t.Context(), directory, exclusive)
				if gate != nil {
					_ = gate.Close()
				}
				if err == nil {
					t.Fatalf("accepted unsafe %s gate", kind)
				}
			}
		})
	}
}

func TestStateAccessNestedSharedLocksAndPermanentInode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	first, err := AcquireStateAccess(t.Context(), root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := AcquireStateAccess(t.Context(), root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	before, err := os.Stat(filepath.Join(root, stateAccessFilename))
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	directory := accessTestDirectory(t, root)
	requireRecoveryBusy(t, directory)
	_ = second.Close()
	gate, err := acquireStateAccessAt(t.Context(), directory, true)
	if err != nil {
		t.Fatal(err)
	}
	_ = gate.Close()
	after, err := os.Stat(filepath.Join(root, stateAccessFilename))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("gate inode was not permanent: %v", err)
	}
}

func TestStateAccessRejectsReplacedGateInode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	directory := accessTestDirectory(t, root)
	gate, err := acquireStateAccessAt(t.Context(), directory, false)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	path := filepath.Join(root, stateAccessFilename)
	if err := os.Rename(path, filepath.Join(root, "detached-gate")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyStateAccessAt(directory, gate); err == nil {
		t.Fatal("accepted an old descriptor after the gate inode was replaced")
	}
}

func TestStateAccessLifecycleAndRegistryCallbacksExcludeRecovery(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	directory := accessTestDirectory(t, root)
	err := WithTerminalLifecycleLockContext(t.Context(), root, func() error {
		requireRecoveryBusy(t, directory)
		return WithRegistryLockContext(t.Context(), root, func(*statefile.LockedPrivateDirectory) error {
			requireRecoveryBusy(t, directory)
			return initializeStateDatabase(t.Context(), root)
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStateAccessCoversBootstrapConnectionGap(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	original := acquireStateDatabaseInitializationLock
	called := false
	acquireStateDatabaseInitializationLock = func(ctx context.Context, directory *os.File) (*os.File, error) {
		called = true
		requireRecoveryBusy(t, directory)
		return original(ctx, directory)
	}
	t.Cleanup(func() { acquireStateDatabaseInitializationLock = original })
	if err := initializeStateDatabase(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("bootstrap lock was not exercised")
	}
}

func TestStateAccessCanceledAcquisitionDoesNotCreateRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := AcquireStateAccess(ctx, root, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled access: %v", err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled access created state: %v", err)
	}
}

func TestStateAccessExcludesAnotherProcess(t *testing.T) {
	const childRoot = "SWAY_SESSION_TEST_ACCESS_ROOT"
	if root := os.Getenv(childRoot); root != "" {
		directory := accessTestDirectory(t, root)
		requireRecoveryBusy(t, directory)
		return
	}
	root := filepath.Join(t.TempDir(), "state")
	database, err := openStateDatabase(t.Context(), root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestStateAccessExcludesAnotherProcess$")
	command.Env = append(os.Environ(), childRoot+"="+root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("child bypassed parent's database handle: %v\n%s", err, output)
	}
}
