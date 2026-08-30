package herdrinit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
	"github.com/marang/sway-title-animator/internal/statefile"
	"golang.org/x/sys/unix"
)

var ErrInitializationRunning = errors.New("herdr initialization is already running for this context")

type ContextLock struct {
	file      *os.File
	directory *os.File
}

// AcquireContextLock serializes the empty-session check and its dependent
// Herdr mutations for one registered context. The persistent lock file is
// owner-only and lives beside the narrow sway-session runtime sockets.
func AcquireContextLock(runtimeRoot string, id sessionstate.ContextID) (*ContextLock, error) {
	if runtimeRoot == "" || !filepath.IsAbs(runtimeRoot) || filepath.Clean(runtimeRoot) != runtimeRoot {
		return nil, errors.New("runtime root must be a clean absolute path")
	}
	if err := id.Validate(); err != nil {
		return nil, err
	}
	directory, err := statefile.OpenPrivateDirectory(filepath.Join(runtimeRoot, "sway-session"), true)
	if err != nil {
		return nil, fmt.Errorf("prepare Herdr initialization lock directory: %w", err)
	}
	name := "herdr-init-" + string(id) + ".lock"
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, statefile.RegularFileMode)
	if err != nil {
		_ = directory.Close()
		return nil, fmt.Errorf("open Herdr initialization lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		_ = directory.Close()
		return nil, errors.New("open Herdr initialization lock: invalid file descriptor")
	}
	closeFiles := func() {
		_ = file.Close()
		_ = directory.Close()
	}
	info, err := file.Stat()
	if err != nil {
		closeFiles()
		return nil, fmt.Errorf("inspect Herdr initialization lock: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || int(stat.Uid) != os.Geteuid() || info.Mode().Perm() != statefile.RegularFileMode || stat.Nlink != 1 {
		closeFiles()
		return nil, errors.New("herdr initialization lock must be an owner-only regular file with one link")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeFiles()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrInitializationRunning
		}
		return nil, fmt.Errorf("lock Herdr initialization: %w", err)
	}
	return &ContextLock{file: file, directory: directory}, nil
}

func (lock *ContextLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	fileErr := lock.file.Close()
	directoryErr := lock.directory.Close()
	lock.file = nil
	lock.directory = nil
	return errors.Join(unlockErr, fileErr, directoryErr)
}
