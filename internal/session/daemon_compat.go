package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/marang/sway-title-animator/internal/statefile"
	"golang.org/x/sys/unix"
)

const (
	daemonLockDirectory              = "sway-session"
	daemonLockFilename               = "daemon.lock"
	registryMigrationLockFilename    = "registry-migration.lock"
	daemonCompatibilityMarkerVersion = 1
	maxDaemonCompatibilityMarkerSize = 1024
	registryMigrationLockRetryDelay  = 10 * time.Millisecond
)

type daemonCompatibilityMarker struct {
	Version        int    `json:"version"`
	PID            int    `json:"pid"`
	ProcessStart   uint64 `json:"process_start"`
	ContextsSchema int    `json:"contexts_schema"`
}

// MarkDaemonRegistryCompatibility binds the current process and supported
// contexts schema to the already-held daemon lock. Upgrade-time commands use
// this evidence to distinguish a current daemon from a pre-current-schema daemon.
func MarkDaemonRegistryCompatibility(lock *os.File) error {
	if lock == nil {
		return errors.New("daemon lock is nil")
	}
	start, err := processStartTime(os.Getpid())
	if err != nil {
		return fmt.Errorf("identify daemon process start: %w", err)
	}
	encoded, err := json.Marshal(daemonCompatibilityMarker{
		Version: daemonCompatibilityMarkerVersion, PID: os.Getpid(), ProcessStart: start, ContextsSchema: ContextsSchemaVersion,
	})
	if err != nil {
		return fmt.Errorf("encode daemon compatibility marker: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := lock.Truncate(0); err != nil {
		return fmt.Errorf("reset daemon compatibility marker: %w", err)
	}
	if _, err := lock.WriteAt(encoded, 0); err != nil {
		return fmt.Errorf("write daemon compatibility marker: %w", err)
	}
	if err := lock.Sync(); err != nil {
		return fmt.Errorf("sync daemon compatibility marker: %w", err)
	}
	return nil
}

// ClearDaemonRegistryCompatibility removes clean-shutdown evidence before the
// daemon releases its lock. Crash leftovers are rejected through PID/start
// validation when another process later owns the lock.
func ClearDaemonRegistryCompatibility(lock *os.File) error {
	if lock == nil {
		return nil
	}
	if err := lock.Truncate(0); err != nil {
		return fmt.Errorf("clear daemon compatibility marker: %w", err)
	}
	if err := lock.Sync(); err != nil {
		return fmt.Errorf("sync cleared daemon compatibility marker: %w", err)
	}
	return nil
}

func acquireDaemonMigrationGuard(root string) (func(), error) {
	noRelease := func() {}
	defaultRoot, err := DefaultStateRoot()
	if err != nil || root != defaultRoot {
		return noRelease, nil
	}
	runtimeRoot := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeRoot == "" || !filepath.IsAbs(runtimeRoot) || filepath.Clean(runtimeRoot) != runtimeRoot {
		return nil, migrationDaemonCompatibilityError("XDG_RUNTIME_DIR is unavailable")
	}
	directory, err := statefile.OpenPrivateDirectory(filepath.Join(runtimeRoot, daemonLockDirectory), true)
	if err != nil {
		return nil, fmt.Errorf("inspect sway-session daemon compatibility: %w", err)
	}
	fd, err := unix.Openat(
		int(directory.Fd()),
		daemonLockFilename,
		unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		statefile.RegularFileMode,
	)
	if err != nil {
		_ = directory.Close()
		return nil, fmt.Errorf("open sway-session daemon compatibility lock: %w", err)
	}
	lock := os.NewFile(uintptr(fd), daemonLockFilename)
	if lock == nil {
		_ = unix.Close(fd)
		_ = directory.Close()
		return nil, errors.New("open sway-session daemon compatibility lock: invalid file descriptor")
	}
	closeFiles := func() {
		_ = lock.Close()
		_ = directory.Close()
	}
	if err := validateDaemonLockFile(lock); err != nil {
		closeFiles()
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
		return func() {
			_ = unix.Flock(fd, unix.LOCK_UN)
			closeFiles()
		}, nil
	} else if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
		closeFiles()
		return nil, fmt.Errorf("inspect sway-session daemon lock owner: %w", err)
	}
	marker, err := readDaemonCompatibilityMarker(lock)
	if err != nil {
		closeFiles()
		return nil, migrationDaemonCompatibilityError(err.Error())
	}
	// A marker owned by another process is evidence about that instant, not a
	// lease. Once these descriptors are closed, that daemon could exit and an
	// older daemon could acquire the lock before migration commits. Only the
	// current daemon may migrate while its independently held lock excludes an
	// owner-generation change for the full transaction.
	if marker.PID != os.Getpid() {
		closeFiles()
		return nil, migrationDaemonCompatibilityError("a separate running daemon owns the compatibility lock and must complete migration")
	}
	start, err := processStartTime(marker.PID)
	if err != nil || start != marker.ProcessStart || marker.Version != daemonCompatibilityMarkerVersion || marker.ContextsSchema != ContextsSchemaVersion {
		closeFiles()
		return nil, migrationDaemonCompatibilityError("the running daemon did not provide current schema evidence")
	}
	closeFiles()
	return noRelease, nil
}

// acquireRegistryMigrationLock serializes default-root writers while they
// establish or migrate the current contexts schema. This is separate from the
// daemon lock: a concurrent CLI writer must wait and re-observe state, whereas
// a running daemon can make a schema transition unsafe and must be rejected.
func acquireRegistryMigrationLock(ctx context.Context, root string) (func(), error) {
	noRelease := func() {}
	defaultRoot, err := DefaultStateRoot()
	if err != nil || root != defaultRoot {
		return noRelease, nil
	}
	if ctx == nil {
		return nil, errors.New("registry migration lock context is nil")
	}
	runtimeRoot := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeRoot == "" || !filepath.IsAbs(runtimeRoot) || filepath.Clean(runtimeRoot) != runtimeRoot {
		return nil, migrationDaemonCompatibilityError("XDG_RUNTIME_DIR is unavailable")
	}
	directory, err := statefile.OpenPrivateDirectory(filepath.Join(runtimeRoot, daemonLockDirectory), true)
	if err != nil {
		return nil, fmt.Errorf("prepare registry migration lock: %w", err)
	}
	fd, err := unix.Openat(
		int(directory.Fd()),
		registryMigrationLockFilename,
		unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		statefile.RegularFileMode,
	)
	if err != nil {
		_ = directory.Close()
		return nil, fmt.Errorf("open registry migration lock: %w", err)
	}
	lock := os.NewFile(uintptr(fd), registryMigrationLockFilename)
	if lock == nil {
		_ = unix.Close(fd)
		_ = directory.Close()
		return nil, errors.New("open registry migration lock: invalid file descriptor")
	}
	closeFiles := func() {
		_ = lock.Close()
		_ = directory.Close()
	}
	if err := validateCompatibilityLockFile(lock, "registry migration"); err != nil {
		closeFiles()
		return nil, err
	}
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() {
				_ = unix.Flock(fd, unix.LOCK_UN)
				closeFiles()
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			closeFiles()
			return nil, fmt.Errorf("lock registry migration: %w", err)
		}
		timer := time.NewTimer(registryMigrationLockRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			closeFiles()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func validateDaemonLockFile(lock *os.File) error {
	return validateCompatibilityLockFile(lock, "session daemon")
}

func validateCompatibilityLockFile(lock *os.File, purpose string) error {
	info, err := lock.Stat()
	if err != nil {
		return fmt.Errorf("inspect %s lock: %w", purpose, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || int(stat.Uid) != os.Geteuid() || info.Mode().Perm() != statefile.RegularFileMode || stat.Nlink != 1 {
		return fmt.Errorf("%s lock must be an owner-only regular file with one link", purpose)
	}
	return nil
}

func readDaemonCompatibilityMarker(lock *os.File) (daemonCompatibilityMarker, error) {
	info, err := lock.Stat()
	if err != nil {
		return daemonCompatibilityMarker{}, err
	}
	if info.Size() <= 0 || info.Size() > maxDaemonCompatibilityMarkerSize {
		return daemonCompatibilityMarker{}, errors.New("the running daemon has no bounded compatibility marker")
	}
	data := make([]byte, info.Size())
	if _, err := lock.ReadAt(data, 0); err != nil && !errors.Is(err, io.EOF) {
		return daemonCompatibilityMarker{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var marker daemonCompatibilityMarker
	if err := decoder.Decode(&marker); err != nil {
		return daemonCompatibilityMarker{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return daemonCompatibilityMarker{}, errors.New("daemon compatibility marker contains trailing data")
	}
	if marker.PID <= 0 || marker.ProcessStart == 0 {
		return daemonCompatibilityMarker{}, errors.New("daemon compatibility marker has invalid process identity")
	}
	return marker, nil
}

func processStartTime(pid int) (uint64, error) {
	if pid <= 0 {
		return 0, errors.New("process ID must be positive")
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	closing := bytes.LastIndexByte(data, ')')
	if closing < 0 {
		return 0, errors.New("process stat has no command boundary")
	}
	fields := strings.Fields(string(data[closing+1:]))
	const startTimeIndexAfterCommand = 19
	if len(fields) <= startTimeIndexAfterCommand {
		return 0, errors.New("process stat has too few fields")
	}
	start, err := strconv.ParseUint(fields[startTimeIndexAfterCommand], 10, 64)
	if err != nil || start == 0 {
		return 0, errors.New("process stat has invalid start time")
	}
	return start, nil
}

func migrationDaemonCompatibilityError(detail string) error {
	return fmt.Errorf("cannot migrate %s while an incompatible sway-session daemon may be running (%s); restart the Sway session with the upgraded sway-session daemon, then retry", ContextsFilename, detail)
}
