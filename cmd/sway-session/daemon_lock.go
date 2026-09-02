package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/marang/sway-title-animator/internal/statefile"
	"golang.org/x/sys/unix"
)

var errSessionDaemonRunning = errors.New("sway-session daemon is already running")

type sessionDaemonLock struct {
	file      *os.File
	directory *os.File
}

func acquireSessionDaemonLock() (*sessionDaemonLock, error) {
	runtimeRoot := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeRoot == "" || !filepath.IsAbs(runtimeRoot) || filepath.Clean(runtimeRoot) != runtimeRoot {
		return nil, errors.New("XDG_RUNTIME_DIR must be a clean absolute path")
	}
	directory, err := statefile.OpenPrivateDirectory(filepath.Join(runtimeRoot, "sway-session"), true)
	if err != nil {
		return nil, fmt.Errorf("prepare session daemon lock directory: %w", err)
	}
	fd, err := unix.Openat(
		int(directory.Fd()),
		"daemon.lock",
		unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		statefile.RegularFileMode,
	)
	if err != nil {
		_ = directory.Close()
		return nil, fmt.Errorf("open session daemon lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), "daemon.lock")
	if file == nil {
		_ = unix.Close(fd)
		_ = directory.Close()
		return nil, errors.New("open session daemon lock: invalid file descriptor")
	}
	closeFiles := func() {
		_ = file.Close()
		_ = directory.Close()
	}
	info, err := file.Stat()
	if err != nil {
		closeFiles()
		return nil, fmt.Errorf("inspect session daemon lock: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || int(stat.Uid) != os.Geteuid() ||
		info.Mode().Perm() != statefile.RegularFileMode || stat.Nlink != 1 {
		closeFiles()
		return nil, errors.New("session daemon lock must be an owner-only regular file with one link")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeFiles()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errSessionDaemonRunning
		}
		return nil, fmt.Errorf("lock session daemon: %w", err)
	}
	return &sessionDaemonLock{file: file, directory: directory}, nil
}

func (lock *sessionDaemonLock) Close() error {
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
