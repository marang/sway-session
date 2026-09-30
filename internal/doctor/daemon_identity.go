package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/marang/sway-session/internal/statefile"
	"golang.org/x/sys/unix"
)

// The start-time token belongs to a process incarnation, not just a reusable
// PID. Parse after the final ')' because comm may contain spaces or parentheses.
func daemonStartTime(pid int) (uint64, error) {
	data, err := readBounded(filepath.Join("/proc", strconv.Itoa(pid), "stat"), maxProcRead)
	if err != nil {
		return 0, err
	}
	end := strings.LastIndexByte(string(data), ')')
	start := strings.IndexByte(string(data), '(')
	if start < 0 || end < start || strings.TrimSpace(string(data[:start])) != strconv.Itoa(pid) {
		return 0, errors.New("invalid process stat identity")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return 0, errors.New("incomplete process stat")
	}
	token, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || token == 0 {
		return 0, errors.New("invalid process start time")
	}
	return token, nil
}

func daemonIdentityUnchanged(observation daemonObservation) bool {
	if !observation.valid || observation.pid <= 0 || observation.startTime == 0 || observation.runtimeDirectory == nil {
		return false
	}
	start, err := daemonStartTime(observation.pid)
	if err != nil || start != observation.startTime || !sameUID(observation.pid) || !daemonCommand(observation.pid) {
		return false
	}
	lock, err := inspectPrivateObjectAt(observation.runtimeDirectory, "daemon.lock", unix.S_IFREG, statefile.RegularFileMode)
	if err != nil || lock.Dev != observation.lockStat.Dev || lock.Ino != observation.lockStat.Ino {
		return false
	}
	pid, err := lockedByPID(&lock)
	if err != nil || pid != observation.pid {
		return false
	}
	end, err := daemonStartTime(observation.pid)
	return err == nil && end == start
}

// Reopening procfs only revalidates identity. Metadata and digest reads always
// use the original pinned executable descriptor, including deleted binaries.
func daemonExecutableUnchanged(observation daemonObservation, pinned os.FileInfo) bool {
	if !daemonIdentityUnchanged(observation) {
		return false
	}
	file, current, err := runtimeProbes.openBinary(filepath.Join("/proc", strconv.Itoa(observation.pid), "exe"))
	if err != nil {
		return false
	}
	defer file.Close()
	return sameInode(pinned, current) && daemonIdentityUnchanged(observation)
}
