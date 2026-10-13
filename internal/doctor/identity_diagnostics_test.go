package doctor

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestDoctorRejectedIdentityRetainsBoundedReason(t *testing.T) {
	observation, options, _, _, _ := doctorBinaryFixture(t, "same", "same")
	read := runtimeProbes.readFile
	runtimeProbes.readFile = func(path string, limit int) ([]byte, error) {
		if path == "/proc/32100/stat" {
			return nil, &os.PathError{Op: "read", Path: "/private/sensitive-process-record", Err: syscall.EACCES}
		}
		return read(path, limit)
	}
	check := inspectDaemonBinary(context.Background(), options, observation)
	if check.Status != Unavailable {
		t.Fatalf("rejected identity trusted: %+v", check)
	}
	requireDoctorEvidence(t, check, "identity stage=initial", "reason=process-token-read", "errno=EACCES")
	if strings.Contains(strings.Join(check.Evidence, "\n"), "sensitive-process-record") {
		t.Fatalf("private read path leaked: %+v", check)
	}
}

func TestDoctorIdentityRejectionRetainsUnderlyingReadErrors(t *testing.T) {
	for _, path := range []string{"/proc/32100/stat", "/proc/32100/status", "/proc/32100/cmdline", "/proc/locks"} {
		t.Run(path, func(t *testing.T) {
			observation, _, _ := doctorDaemonFixture(t)
			injected := &os.PathError{Op: "read", Path: "/private/sensitive-process-record", Err: syscall.EIO}
			read := runtimeProbes.readFile
			runtimeProbes.readFile = func(candidate string, limit int) ([]byte, error) {
				if candidate == path {
					return nil, injected
				}
				return read(candidate, limit)
			}
			err := verifyDaemonIdentity(observation)
			if !errors.Is(err, injected) || !errors.Is(err, syscall.EIO) {
				t.Fatalf("underlying read error discarded: %v", err)
			}
		})
	}
}

func TestDoctorIdentityRejectionRetainsUnderlyingExecutableError(t *testing.T) {
	observation, options, _, _, _ := doctorBinaryFixture(t, "same", "same")
	pinned, err := os.Stat(options.Executable)
	if err != nil {
		t.Fatal(err)
	}
	injected := &os.PathError{Op: "open", Path: "/private/sensitive-executable", Err: syscall.EACCES}
	runtimeProbes.openBinary = func(string) (*os.File, os.FileInfo, error) { return nil, nil, injected }
	err = verifyDaemonExecutable(observation, pinned)
	if !errors.Is(err, injected) || !errors.Is(err, syscall.EACCES) {
		t.Fatalf("underlying executable error discarded: %v", err)
	}
}
