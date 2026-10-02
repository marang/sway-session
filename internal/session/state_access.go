package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/marang/sway-session/internal/statefile"
	"golang.org/x/sys/unix"
)

const (
	stateAccessFilename   = ".state-access.lock"
	stateRecoveryFilename = "state-recovery.json"
)

// ErrStateRecoveryPending prevents ordinary access to an interrupted recovery.
var ErrStateRecoveryPending = errors.New("state database recovery is pending")

var errStateAccessBootstrapBusy = errors.New("state access gate publication is busy")

// StateAccess pins the state root and holds its permanent shared recovery gate.
// Keep it until all database handles and external effects of an operation end.
type StateAccess struct {
	directory *os.File
	gate      *os.File
}

// AcquireStateAccess excludes recovery for one complete operation. The gate is
// synchronization metadata and is never removed, including when Close is called.
// With create=false, neither the state root nor the gate is created. Old roots
// without a gate are protected by a shared root-directory lock until first use
// by a writer publishes the gate under an exclusive root-directory lock.
func AcquireStateAccess(ctx context.Context, root string, create bool) (*StateAccess, error) {
	if ctx == nil {
		return nil, errors.New("state access context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, err := statefile.OpenPrivateDirectory(root, create)
	if err != nil {
		return nil, err
	}
	gate, err := acquireSharedStateAccessAt(ctx, directory, create)
	if err != nil {
		_ = directory.Close()
		return nil, err
	}
	return &StateAccess{directory: directory, gate: gate}, nil
}

// Close releases the gate after the operation's database handles have closed.
func (access *StateAccess) Close() error {
	if access == nil {
		return nil
	}
	var gateErr, directoryErr error
	if access.gate != nil {
		gateErr = access.gate.Close()
		access.gate = nil
	}
	if access.directory != nil {
		directoryErr = access.directory.Close()
		access.directory = nil
	}
	return errors.Join(gateErr, directoryErr)
}

func acquireSharedStateAccessAt(ctx context.Context, directory *os.File, create bool) (*os.File, error) {
	if ctx == nil {
		return nil, errors.New("state access context is nil")
	}
	deadline := time.Now().Add(databaseBusyTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := stateRecoveryPendingAt(directory); err != nil {
			return nil, err
		}
		gate, err := acquireStateAccess(ctx, directory, false, create)
		if create && errors.Is(err, errStateAccessBootstrapBusy) && time.Now().Before(deadline) {
			// Concurrent first writers can race before publication. Retry only
			// that transition, without holding any lock; recovery never retries.
			timer := time.NewTimer(min(5*time.Millisecond, time.Until(deadline)))
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
				continue
			}
		}
		if err != nil {
			return nil, err
		}
		if err := stateRecoveryPendingAt(directory); err != nil {
			_ = gate.Close()
			return nil, err
		}
		return gate, nil
	}
}

// acquireStateAccessAt takes a nonblocking shared or exclusive lock on a
// permanent inode. The caller owns the returned descriptor and releases the
// flock by closing it. Recovery may acquire EX despite a pending marker.
func acquireStateAccessAt(ctx context.Context, directory *os.File, exclusive bool) (*os.File, error) {
	return acquireStateAccess(ctx, directory, exclusive, true)
}

func acquireStateAccess(ctx context.Context, directory *os.File, exclusive, create bool) (*os.File, error) {
	if ctx == nil {
		return nil, errors.New("state access context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if directory == nil {
		return nil, errors.New("state access directory is nil")
	}
	var directoryStat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &directoryStat); err != nil {
		return nil, fmt.Errorf("inspect state access directory: %w", err)
	}
	if directoryStat.Mode&unix.S_IFMT != unix.S_IFDIR || directoryStat.Mode&0o7777 != statefile.DirectoryMode || directoryStat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("state access directory must be an owner-only directory")
	}
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	fd, err := unix.Openat(int(directory.Fd()), stateAccessFilename, flags, 0)
	if errors.Is(err, unix.ENOENT) {
		// An independent open description is essential: a dup would share the
		// caller's flock and could change or release an existing registry lock.
		rootFD, openErr := unix.Openat(int(directory.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			return nil, fmt.Errorf("open state access bootstrap lock: %w", openErr)
		}
		rootLock := os.NewFile(uintptr(rootFD), "state access bootstrap")
		operation := unix.LOCK_SH
		if create {
			operation = unix.LOCK_EX
		}
		if lockErr := flockStateAccess(rootFD, operation); lockErr != nil {
			_ = rootLock.Close()
			if create && errors.Is(lockErr, ErrStateDatabaseBusy) {
				return nil, errors.Join(errStateAccessBootstrapBusy, lockErr)
			}
			return nil, lockErr
		}
		// Recheck after taking the bootstrap lock. A reader must not wait on
		// the permanent gate while retaining its fallback root SH lock.
		fd, err = unix.Openat(int(directory.Fd()), stateAccessFilename, flags, 0)
		if errors.Is(err, unix.ENOENT) && !create {
			if err := ctx.Err(); err != nil {
				_ = rootLock.Close()
				return nil, err
			}
			return rootLock, nil
		}
		if errors.Is(err, unix.ENOENT) && create {
			fd, err = unix.Openat(int(directory.Fd()), stateAccessFilename, flags|unix.O_CREAT|unix.O_EXCL, statefile.RegularFileMode)
		}
		_ = rootLock.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("open state access gate: %w", err)
	}
	gate := os.NewFile(uintptr(fd), stateAccessFilename)
	keepGate := false
	defer func() {
		if !keepGate {
			_ = gate.Close()
		}
	}()
	if err := verifyStateAccessAt(directory, gate); err != nil {
		return nil, err
	}
	operation := unix.LOCK_SH
	if exclusive {
		operation = unix.LOCK_EX
	}
	if err := flockStateAccess(fd, operation); err != nil {
		return nil, err
	}
	if err := verifyStateAccessAt(directory, gate); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keepGate = true
	return gate, nil
}

func flockStateAccess(fd, operation int) error {
	if err := unix.Flock(fd, operation|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("%w: state access gate is held", ErrStateDatabaseBusy)
		}
		return fmt.Errorf("lock state access gate: %w", err)
	}
	return nil
}

func verifyStateAccessAt(directory, gate *os.File) error {
	var opened, current unix.Stat_t
	if err := unix.Fstat(int(gate.Fd()), &opened); err != nil {
		return fmt.Errorf("inspect state access gate: %w", err)
	}
	if opened.Mode&unix.S_IFMT != unix.S_IFREG || opened.Mode&0o7777 != statefile.RegularFileMode || opened.Uid != uint32(os.Geteuid()) || opened.Nlink != 1 {
		return errors.New("state access gate must be an owner-only regular file with one link")
	}
	if err := unix.Fstatat(int(directory.Fd()), stateAccessFilename, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("inspect state access gate path: %w", err)
	}
	if opened.Dev != current.Dev || opened.Ino != current.Ino || current.Mode != opened.Mode || current.Uid != opened.Uid || current.Nlink != 1 {
		return errors.New("state access gate changed while acquiring its lock")
	}
	return nil
}

// Any directory entry at the marker path blocks access. Ordinary operations
// never parse or repair it: malformed markers must also fail closed.
func stateRecoveryPendingAt(directory *os.File) error {
	if directory == nil {
		return errors.New("state recovery directory is nil")
	}
	var stat unix.Stat_t
	err := unix.Fstatat(int(directory.Fd()), stateRecoveryFilename, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect state recovery marker: %w", err)
	}
	return ErrStateRecoveryPending
}
