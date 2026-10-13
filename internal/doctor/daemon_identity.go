package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/marang/sway-session/internal/statefile"
	"golang.org/x/sys/unix"
)

var errProcessStatInvalid = errors.New("invalid process stat identity")

// Retain the OS error for callers without serializing its path or payload.
// Reasons are fixed internal labels; evidence never includes arbitrary errors.
type daemonIdentityRejection struct {
	reason string
	cause  error
}

func (rejection *daemonIdentityRejection) Error() string { return rejection.reason }

func (rejection *daemonIdentityRejection) Unwrap() error { return rejection.cause }

func rejectDaemonIdentity(reason string, cause error) error {
	return &daemonIdentityRejection{reason: reason, cause: cause}
}

func identityRejectionEvidence(stage string, err error) string {
	if err == nil {
		return "identity stage=" + stage + " outcome=verified"
	}
	var rejection *daemonIdentityRejection
	if !errors.As(err, &rejection) {
		return "identity stage=" + stage + " reason=observation-invalid outcome=rejected"
	}
	evidence := "identity stage=" + stage + " reason=" + rejection.reason
	switch {
	case errors.Is(err, errProcReadBound):
		evidence += fmt.Sprintf(" outcome=over-bound limit=%d", maxProcRead)
	case errors.Is(err, errLockHolderAbsent):
		evidence += " outcome=missing-holder"
	case rejection.cause != nil:
		evidence += " outcome=unreadable"
	default:
		evidence += " outcome=rejected"
	}
	var errno unix.Errno
	if errors.As(err, &errno) {
		evidence += " errno=" + identityErrnoName(errno)
	}
	return evidence
}

func rejectedIdentityCheck(check Check, stage string, err error) Check {
	check.Evidence = append(check.Evidence, identityRejectionEvidence(stage, err))
	return check
}

func identityErrnoName(errno unix.Errno) string {
	switch errno {
	case unix.EACCES:
		return "EACCES"
	case unix.EPERM:
		return "EPERM"
	case unix.ENOENT:
		return "ENOENT"
	case unix.ESRCH:
		return "ESRCH"
	case unix.EIO:
		return "EIO"
	case unix.EINTR:
		return "EINTR"
	case unix.EAGAIN:
		return "EAGAIN"
	case unix.EINVAL:
		return "EINVAL"
	case unix.EBADF:
		return "EBADF"
	case unix.ENOTDIR:
		return "ENOTDIR"
	case unix.ELOOP:
		return "ELOOP"
	default:
		return strconv.Itoa(int(errno))
	}
}

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
		return 0, errProcessStatInvalid
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return 0, fmt.Errorf("%w: incomplete process stat", errProcessStatInvalid)
	}
	token, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || token == 0 {
		return 0, fmt.Errorf("%w: invalid process start time", errProcessStatInvalid)
	}
	return token, nil
}

func daemonIdentityUnchanged(observation daemonObservation) bool {
	return verifyDaemonIdentity(observation) == nil
}

func verifyDaemonIdentity(observation daemonObservation) error {
	if !observation.valid || observation.pid <= 0 || observation.startTime == 0 || observation.runtimeDirectory == nil {
		return rejectDaemonIdentity("observation-invalid", nil)
	}
	start, err := daemonStartTime(observation.pid)
	if err != nil {
		return rejectProcessToken(err)
	}
	if start != observation.startTime {
		return rejectDaemonIdentity("process-token-mismatch", nil)
	}
	if err := verifyDaemonUID(observation.pid); err != nil {
		return err
	}
	if err := verifyDaemonCommand(observation.pid); err != nil {
		return err
	}
	lock, err := inspectPrivateObjectAt(observation.runtimeDirectory, "daemon.lock", unix.S_IFREG, statefile.RegularFileMode)
	if err != nil {
		return rejectDaemonIdentity("private-lock-read", err)
	}
	if lock.Dev != observation.lockStat.Dev || lock.Ino != observation.lockStat.Ino {
		return rejectDaemonIdentity("private-lock-identity-mismatch", nil)
	}
	pid, err := lockedByPID(&lock)
	if err != nil {
		return rejectHeldLock(err)
	}
	if pid != observation.pid {
		return rejectDaemonIdentity("held-lock-holder-mismatch", nil)
	}
	end, err := daemonStartTime(observation.pid)
	if err != nil {
		return rejectProcessToken(err)
	}
	if end != start {
		return rejectDaemonIdentity("process-token-mismatch", nil)
	}
	return nil
}

func rejectProcessToken(err error) error {
	if errors.Is(err, errProcessStatInvalid) {
		return rejectDaemonIdentity("process-token-invalid", nil)
	}
	return rejectDaemonIdentity("process-token-read", err)
}

func rejectHeldLock(err error) error {
	if errors.Is(err, errLockHolderAbsent) {
		return rejectDaemonIdentity("held-lock-missing", err)
	}
	return rejectDaemonIdentity("held-lock-read", err)
}

// Reopening procfs only revalidates identity. Metadata and digest reads always
// use the original pinned executable descriptor, including deleted binaries.
func verifyDaemonExecutable(observation daemonObservation, pinned os.FileInfo) error {
	if err := verifyDaemonIdentity(observation); err != nil {
		return err
	}
	file, current, err := runtimeProbes.openBinary(filepath.Join("/proc", strconv.Itoa(observation.pid), "exe"))
	if err != nil {
		return rejectDaemonIdentity("executable-open", err)
	}
	defer file.Close()
	if !sameInode(pinned, current) {
		return rejectDaemonIdentity("executable-inode-mismatch", nil)
	}
	return verifyDaemonIdentity(observation)
}
