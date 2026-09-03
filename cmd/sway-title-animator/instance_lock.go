package main

import (
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

	"golang.org/x/sys/unix"
)

var errInstanceRunning = errors.New("sway-title-animator is already running")

const (
	instanceRecordVersion         = 1
	procfsDeletedExecutableSuffix = " (deleted)"
)

type executableIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type instanceRecord struct {
	Version            int                 `json:"version,omitempty"`
	PID                int                 `json:"pid"`
	StartTime          uint64              `json:"start_time"`
	Executable         string              `json:"executable"`
	ExecutableIdentity *executableIdentity `json:"executable_identity,omitempty"`
}

type instanceLock struct {
	file *os.File
}

func runtimeFile() (string, error) {
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		return filepath.Join(runtimeDir, "sway-tab-title-daemon.pid"), nil
	}

	runtimeDir := filepath.Join(os.TempDir(), fmt.Sprintf("sway-title-animator-%d", os.Getuid()))
	if err := os.Mkdir(runtimeDir, 0o700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("create private runtime directory: %w", err)
	}
	info, err := os.Stat(runtimeDir)
	if err != nil {
		return "", fmt.Errorf("inspect private runtime directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() || info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("runtime directory %s is not private and owned by the current user", runtimeDir)
	}
	return filepath.Join(runtimeDir, "sway-tab-title-daemon.pid"), nil
}

func acquireInstanceLock(path string, replace bool) (*instanceLock, error) {
	for range 3 {
		lock, retry, err := acquireInstanceLockOnce(path, replace)
		if err != nil {
			return nil, err
		}
		if !retry {
			return lock, nil
		}
	}
	return nil, fmt.Errorf("instance lock %s changed repeatedly while acquiring it", path)
}

func acquireInstanceLockOnce(path string, replace bool) (*instanceLock, bool, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open instance lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, false, errors.New("open instance lock: invalid file descriptor")
	}
	closeWithUnlock := func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = file.Close()
	}

	info, err := file.Stat()
	if err != nil {
		closeWithUnlock()
		return nil, false, fmt.Errorf("inspect instance lock: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || int(stat.Uid) != os.Getuid() {
		closeWithUnlock()
		return nil, false, fmt.Errorf("instance lock %s is not a regular file owned by the current user", path)
	}

	lockErr := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	record, recordErr := readInstanceRecord(file)
	if lockErr == nil {
		if recordErr == nil && record.PID != os.Getpid() && processMatchesRecord(record) {
			if !replace {
				closeWithUnlock()
				return nil, false, errInstanceRunning
			}
			if err := terminateRecordedProcess(record); err != nil {
				closeWithUnlock()
				return nil, false, err
			}
			if err := waitForRecordedProcessExit(record, 2*time.Second); err != nil {
				closeWithUnlock()
				return nil, false, err
			}
			if same, err := pathStillReferencesFile(path, file); err != nil || !same {
				closeWithUnlock()
				return nil, true, err
			}
		}
		if err := writeInstanceRecord(file); err != nil {
			closeWithUnlock()
			return nil, false, err
		}
		return &instanceLock{file: file}, false, nil
	}
	if !errors.Is(lockErr, unix.EWOULDBLOCK) && !errors.Is(lockErr, unix.EAGAIN) {
		closeWithUnlock()
		return nil, false, fmt.Errorf("lock instance file: %w", lockErr)
	}
	if !replace {
		closeWithUnlock()
		return nil, false, errInstanceRunning
	}
	if recordErr != nil {
		closeWithUnlock()
		return nil, false, fmt.Errorf("cannot safely replace running instance: %w", recordErr)
	}
	if !processMatchesRecord(record) {
		closeWithUnlock()
		return nil, false, errors.New("cannot safely replace running instance: PID metadata does not match the lock owner")
	}
	if err := terminateRecordedProcess(record); err != nil {
		closeWithUnlock()
		return nil, false, err
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
			if same, err := pathStillReferencesFile(path, file); err != nil || !same {
				closeWithUnlock()
				return nil, true, err
			}
			if err := writeInstanceRecord(file); err != nil {
				closeWithUnlock()
				return nil, false, err
			}
			return &instanceLock{file: file}, false, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	closeWithUnlock()
	return nil, false, errors.New("previous sway-title-animator did not exit within 2 seconds")
}

func (lock *instanceLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	fd := int(lock.file.Fd())
	unlockErr := unix.Flock(fd, unix.LOCK_UN)
	closeErr := lock.file.Close()
	lock.file = nil
	return errors.Join(unlockErr, closeErr)
}

func readInstanceRecord(file *os.File) (instanceRecord, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return instanceRecord{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return instanceRecord{}, err
	}
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 {
		return instanceRecord{}, errors.New("instance lock has no PID metadata")
	}

	var record instanceRecord
	if err := json.Unmarshal(data, &record); err == nil && record.PID > 0 {
		return record, nil
	}

	legacyPID, err := strconv.Atoi(string(data))
	if err != nil || legacyPID <= 0 {
		return instanceRecord{}, errors.New("instance lock contains invalid PID metadata")
	}
	return instanceRecord{PID: legacyPID}, nil
}

func writeInstanceRecord(file *os.File) error {
	startTime, err := processStartTime(os.Getpid())
	if err != nil {
		return fmt.Errorf("read current process identity: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("read current executable: %w", err)
	}
	executableInfo, err := os.Stat("/proc/self/exe")
	if err != nil {
		return fmt.Errorf("inspect current executable identity: %w", err)
	}
	executableStat, ok := executableInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("inspect current executable identity: unsupported stat data")
	}
	record := instanceRecord{
		Version:    instanceRecordVersion,
		PID:        os.Getpid(),
		StartTime:  startTime,
		Executable: filepath.Base(executable),
		ExecutableIdentity: &executableIdentity{
			Device: uint64(executableStat.Dev),
			Inode:  executableStat.Ino,
		},
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("truncate instance lock: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind instance lock: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write instance lock: %w", err)
	}
	return file.Sync()
}

func processMatchesRecord(record instanceRecord) bool {
	if record.PID <= 0 || record.PID == os.Getpid() || record.StartTime == 0 || record.Executable == "" {
		return false
	}
	if record.Version != 0 && record.Version != instanceRecordVersion {
		return false
	}
	if record.Version == instanceRecordVersion && record.ExecutableIdentity == nil {
		return false
	}
	if record.Version == 0 && record.ExecutableIdentity != nil {
		return false
	}
	if err := syscall.Kill(record.PID, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	executableLink := filepath.Join("/proc", strconv.Itoa(record.PID), "exe")
	executable, err := os.Readlink(executableLink)
	if err != nil {
		return false
	}
	deletedMarkerVerified := false
	if strings.HasSuffix(executable, procfsDeletedExecutableSuffix) {
		info, err := os.Stat(executableLink)
		if err != nil {
			return false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return false
		}
		if record.ExecutableIdentity != nil {
			if uint64(stat.Dev) != record.ExecutableIdentity.Device || stat.Ino != record.ExecutableIdentity.Inode {
				return false
			}
		} else if stat.Nlink != 0 {
			return false
		}
		confirmed, err := os.Readlink(executableLink)
		if err != nil || confirmed != executable {
			return false
		}
		deletedMarkerVerified = true
	} else if record.ExecutableIdentity != nil {
		info, err := os.Stat(executableLink)
		if err != nil {
			return false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || uint64(stat.Dev) != record.ExecutableIdentity.Device || stat.Ino != record.ExecutableIdentity.Inode {
			return false
		}
	}
	if !processExecutableMatches(executable, record.Executable, deletedMarkerVerified) {
		return false
	}
	startTime, err := processStartTime(record.PID)
	return err == nil && startTime == record.StartTime
}

func processExecutableMatches(observed string, recorded string, deletedMarkerVerified bool) bool {
	if filepath.Base(observed) == filepath.Base(recorded) {
		return true
	}
	if strings.HasSuffix(observed, procfsDeletedExecutableSuffix) {
		// A live executable may literally end in " (deleted)". Only an
		// independently verified inode permits treating the suffix as metadata.
		if !deletedMarkerVerified {
			return false
		}
		observed = strings.TrimSuffix(observed, procfsDeletedExecutableSuffix)
	}
	return filepath.Base(observed) == filepath.Base(recorded)
}

func processStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	raw := string(data)
	end := strings.LastIndex(raw, ")")
	if end < 0 || end+2 >= len(raw) {
		return 0, errors.New("invalid process stat")
	}
	fields := strings.Fields(raw[end+2:])
	if len(fields) <= 19 {
		return 0, errors.New("process stat is missing start time")
	}
	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse process start time: %w", err)
	}
	return startTime, nil
}

func terminateRecordedProcess(record instanceRecord) error {
	if !processMatchesRecord(record) {
		return errors.New("refusing to terminate process because its identity changed")
	}
	if err := syscall.Kill(record.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("terminate previous sway-title-animator: %w", err)
	}
	return nil
}

func waitForRecordedProcessExit(record instanceRecord, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processMatchesRecord(record) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("previous sway-title-animator did not exit within 2 seconds")
}

func pathStillReferencesFile(path string, file *os.File) (bool, error) {
	pathInfo, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	fileInfo, err := file.Stat()
	if err != nil {
		return false, err
	}
	return os.SameFile(pathInfo, fileInfo), nil
}
