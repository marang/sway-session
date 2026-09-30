package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func doctorProcessStat(pid int, start uint64) []byte {
	fields := append([]string{"S"}, strings.Fields(strings.Repeat("0 ", 18))...)
	fields = append(fields, strconv.FormatUint(start, 10))
	return []byte(fmt.Sprintf("%d (sway-session (daemon)) %s\n", pid, strings.Join(fields, " ")))
}

// This fixture reads only disposable files; procfs identity and held-lock
// records are injected so no real user daemon or runtime directory is touched.
func doctorDaemonFixture(t *testing.T) (daemonObservation, func(uint64), func(bool)) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "daemon.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := observePrivateDirectory(root, nil)
	if runtime.err != nil {
		t.Fatal(runtime.err)
	}
	t.Cleanup(runtime.Close)
	previous := runtimeProbes
	t.Cleanup(func() { runtimeProbes = previous })
	start, held := uint64(100), true
	const pid = 32100
	info, err := os.Stat(filepath.Join(root, "daemon.lock"))
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	major, minor := unixMajorMinor(uint64(stat.Dev))
	runtimeProbes.readFile = func(path string, _ int) ([]byte, error) {
		switch path {
		case "/proc/32100/stat":
			return doctorProcessStat(pid, start), nil
		case "/proc/32100/status":
			return []byte(fmt.Sprintf("Uid:\t%d\t%d\t%d\t%d\n", os.Geteuid(), os.Geteuid(), os.Geteuid(), os.Geteuid())), nil
		case "/proc/32100/cmdline":
			return []byte("/usr/bin/sway-session\x00daemon\x00"), nil
		case "/proc/locks":
			if !held {
				return nil, nil
			}
			return []byte(fmt.Sprintf("1: FLOCK ADVISORY WRITE %d %02x:%02x:%d 0 EOF\n", pid, major, minor, stat.Ino)), nil
		default:
			return nil, errors.New("unexpected process read")
		}
	}
	observation := inspectDaemonLock(runtime)
	if !observation.valid {
		t.Fatalf("fixture identity not verified: %+v", observation)
	}
	return observation, func(value uint64) { start = value }, func(value bool) { held = value }
}

func TestDoctorDaemonIdentityRejectsPIDReuseAndReleasedLock(t *testing.T) {
	observation, setStart, setHeld := doctorDaemonFixture(t)
	if !daemonIdentityUnchanged(observation) {
		t.Fatal("matching process identity rejected")
	}
	setStart(101)
	if daemonIdentityUnchanged(observation) {
		t.Fatal("reused PID trusted")
	}
	setStart(100)
	setHeld(false)
	if daemonIdentityUnchanged(observation) {
		t.Fatal("released daemon lock trusted")
	}
}

func TestDoctorDaemonIdentityRejectsReplacedLockAndUnreadableProcess(t *testing.T) {
	observation, _, _ := doctorDaemonFixture(t)
	path := fmt.Sprintf("/proc/self/fd/%d/daemon.lock", observation.runtimeDirectory.Fd())
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if daemonIdentityUnchanged(observation) {
		t.Fatal("replacement daemon lock trusted")
	}
	runtimeProbes.readFile = func(string, int) ([]byte, error) { return nil, os.ErrPermission }
	if daemonIdentityUnchanged(observation) {
		t.Fatal("unreadable process identity trusted")
	}
}

func TestDoctorDaemonStartTimeParsesBoundedIdentity(t *testing.T) {
	previous := runtimeProbes.readFile
	t.Cleanup(func() { runtimeProbes.readFile = previous })
	for _, test := range []struct {
		data  []byte
		valid bool
	}{
		{doctorProcessStat(32100, 100), true},
		{doctorProcessStat(32101, 100), false},
		{doctorProcessStat(32100, 0), false},
		{[]byte("32100 (incomplete) S 0"), false},
		{[]byte("invalid process data"), false},
	} {
		runtimeProbes.readFile = func(string, int) ([]byte, error) { return test.data, nil }
		_, err := daemonStartTime(32100)
		if (err == nil) != test.valid {
			t.Fatalf("process stat validity=%t err=%v", test.valid, err)
		}
	}
}
