package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/marang/sway-session/internal/buildmetadata"
)

// Assert the public Check rather than the internal identity result: all doctor
// callers must retain the rejection boundary without exposing probe contents.
func requireIdentityBoundary(t *testing.T, check Check, id, stage, reason string, qualifiers ...string) {
	t.Helper()
	if check.ID != id || check.Status != Unavailable {
		t.Fatalf("identity rejection changed check contract: %+v", check)
	}
	prefix := "identity stage=" + stage + " reason=" + reason
	var diagnostic string
	for _, evidence := range check.Evidence {
		if strings.HasPrefix(evidence, prefix) && (len(evidence) == len(prefix) || evidence[len(prefix)] == ' ') {
			diagnostic = evidence
			break
		}
	}
	if diagnostic == "" {
		t.Fatalf("missing identity boundary %q: %+v", prefix, check)
	}
	if len(diagnostic) > 256 || strings.ContainsAny(diagnostic, "\n\r\x00") {
		t.Fatalf("identity diagnostic is not bounded single-line evidence: %q", diagnostic)
	}
	for _, qualifier := range qualifiers {
		if !strings.Contains(diagnostic, qualifier) {
			t.Errorf("missing bounded qualifier %q in %q", qualifier, diagnostic)
		}
	}
}

func requireIdentityPrivacy(t *testing.T, check Check, private ...string) {
	t.Helper()
	data, err := json.Marshal(check)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range private {
		if strings.Contains(string(data), value) {
			t.Errorf("private probe content %q reached public Check: %s", value, data)
		}
	}
}

func TestDoctorIdentityBoundariesInitialProcessAndHeldLockFailures(t *testing.T) {
	const privateValue = "private-probe-marker-472ff8"
	for _, test := range []struct {
		name, path, reason string
		data               []byte
		err                error
		qualifiers         []string
	}{
		{"token-read", "/proc/32100/stat", "process-token-read", nil, &os.PathError{Op: privateValue, Path: "/" + privateValue, Err: syscall.EACCES}, []string{"errno=EACCES"}},
		{"token-invalid", "/proc/32100/stat", "process-token-invalid", []byte(privateValue), nil, nil},
		{"uid-read", "/proc/32100/status", "process-uid-read", nil, fmt.Errorf("%s: %w", privateValue, syscall.ENOENT), []string{"errno=ENOENT"}},
		{"uid-mismatch", "/proc/32100/status", "process-uid-mismatch", []byte("Uid:\t" + strconv.Itoa(os.Geteuid()+1) + "\n" + privateValue), nil, nil},
		{"command-read", "/proc/32100/cmdline", "process-command-read", nil, fmt.Errorf("%s: %w", privateValue, syscall.EIO), []string{"errno=EIO"}},
		{"command-mismatch", "/proc/32100/cmdline", "process-command-mismatch", []byte("/usr/bin/sway-session\x00" + privateValue + "\x00"), nil, nil},
		{"held-lock-read", "/proc/locks", "held-lock-read", nil, fmt.Errorf("%s: %w", privateValue, syscall.EACCES), []string{"outcome=unreadable", "errno=EACCES"}},
		{"held-lock-missing", "/proc/locks", "held-lock-missing", []byte(privateValue), nil, []string{"outcome=missing-holder"}},
		{"held-lock-arbitrary-error", "/proc/locks", "held-lock-read", nil, errors.New(privateValue + "\nidentity stage=final reason=forged"), []string{"outcome=unreadable"}},
	} {
		for _, caller := range []string{"lock", "binary"} {
			t.Run(test.name+"/"+caller, func(t *testing.T) {
				observation, options, _, _, _ := doctorBinaryFixture(t, "same", "same")
				read := runtimeProbes.readFile
				runtimeProbes.readFile = func(path string, limit int) ([]byte, error) {
					if path == test.path {
						return test.data, test.err
					}
					return read(path, limit)
				}
				var check Check
				if caller == "lock" {
					runtime := privateDirectoryObservation{path: observation.runtimeDirectory.Name(), directory: observation.runtimeDirectory}
					result := inspectDaemonLock(runtime)
					if result.valid {
						t.Fatal("rejected lock observation remains valid")
					}
					check = result.check
				} else {
					check = inspectDaemonBinary(t.Context(), options, observation)
				}
				requireIdentityBoundary(t, check, "daemon."+caller, "initial", test.reason, test.qualifiers...)
				requireIdentityPrivacy(t, check, privateValue, "reason=forged")
			})
		}
	}
}

func TestDoctorIdentityBoundariesInitialRetainedIdentityFailures(t *testing.T) {
	for _, change := range []string{"process-token-mismatch", "private-lock-read", "private-lock-identity-mismatch", "held-lock-holder-mismatch", "observation-invalid"} {
		t.Run(change, func(t *testing.T) {
			observation, options, _, setStart, _ := doctorBinaryFixture(t, "same", "same")
			lockPath := fmt.Sprintf("/proc/self/fd/%d/daemon.lock", observation.runtimeDirectory.Fd())
			switch change {
			case "process-token-mismatch":
				setStart(101)
			case "private-lock-read":
				if err := os.Remove(lockPath); err != nil {
					t.Fatal(err)
				}
			case "private-lock-identity-mismatch":
				if err := os.Rename(lockPath, lockPath+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case "held-lock-holder-mismatch":
				read := runtimeProbes.readFile
				runtimeProbes.readFile = func(path string, limit int) ([]byte, error) {
					data, err := read(path, limit)
					if path == "/proc/locks" {
						data = []byte(strings.ReplaceAll(string(data), "32100", "32101"))
					}
					return data, err
				}
			case "observation-invalid":
				observation.valid = false
			}
			check := inspectDaemonBinary(t.Context(), options, observation)
			requireIdentityBoundary(t, check, "daemon.binary", "initial", change)
			requireDoctorEvidence(t, check, "Executing CLI build: version=dev", "Daemon build: unknown")
		})
	}
}

func TestDoctorIdentityBoundariesTransientPinningFailureCannotBecomeTrusted(t *testing.T) {
	observation, options, _, _, _ := doctorBinaryFixture(t, "same", "same")
	read, open, readBuild := runtimeProbes.readFile, runtimeProbes.openBinary, runtimeProbes.readBuild
	armed, failed, buildReads := false, false, 0
	runtimeProbes.openBinary = func(path string) (*os.File, os.FileInfo, error) {
		file, info, err := open(path)
		if path == "/proc/32100/exe" && !failed {
			armed = true
		}
		return file, info, err
	}
	runtimeProbes.readFile = func(path string, limit int) ([]byte, error) {
		if armed && !failed && path == "/proc/32100/stat" {
			failed = true
			return nil, fmt.Errorf("private-pinning-read: %w", syscall.EACCES)
		}
		return read(path, limit)
	}
	runtimeProbes.readBuild = func(ctx context.Context, file *os.File) (buildmetadata.Metadata, error) {
		buildReads++
		return readBuild(ctx, file)
	}
	check := inspectDaemonBinary(t.Context(), options, observation)
	if !failed || buildReads != 0 {
		t.Fatalf("pinning failure did not reject before metadata reads: failed=%t buildReads=%d", failed, buildReads)
	}
	requireIdentityBoundary(t, check, "daemon.binary", "pinning", "process-token-read", "errno=EACCES")
	requireDoctorEvidence(t, check, "Daemon build: unknown", "Executing CLI build: version=dev", "identity stage=final outcome=verified")
	requireIdentityPrivacy(t, check, "private-pinning-read")
}

func TestDoctorIdentityBoundariesFirstExecutableOpenIsPinning(t *testing.T) {
	observation, options, _, _, _ := doctorBinaryFixture(t, "same", "same")
	open := runtimeProbes.openBinary
	failed := false
	runtimeProbes.openBinary = func(path string) (*os.File, os.FileInfo, error) {
		if path == "/proc/32100/exe" && !failed {
			failed = true
			return nil, nil, &os.PathError{Op: "private-executable-op", Path: "/private-executable-path", Err: syscall.EACCES}
		}
		return open(path)
	}
	check := inspectDaemonBinary(t.Context(), options, observation)
	requireIdentityBoundary(t, check, "daemon.binary", "pinning", "executable-open", "errno=EACCES")
	requireDoctorEvidence(t, check, "Daemon build: unknown", "Executing CLI build: version=dev", "identity stage=final outcome=verified")
	requireIdentityPrivacy(t, check, "private-executable-op", "private-executable-path")
}

func TestDoctorIdentityBoundariesExecutableReplacement(t *testing.T) {
	for _, stage := range []string{"pinning", "final"} {
		t.Run(stage, func(t *testing.T) {
			observation, options, livePath, _, _ := doctorBinaryFixture(t, "same", "same")
			open, readBuild := runtimeProbes.openBinary, runtimeProbes.readBuild
			replaced := false
			replace := func() {
				if err := os.Rename(livePath, livePath+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(livePath, []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
				replaced = true
			}
			runtimeProbes.openBinary = func(path string) (*os.File, os.FileInfo, error) {
				file, info, err := open(path)
				if stage == "pinning" && !replaced && path == "/proc/32100/exe" && err == nil {
					replace()
				}
				return file, info, err
			}
			runtimeProbes.readBuild = func(ctx context.Context, file *os.File) (buildmetadata.Metadata, error) {
				metadata, err := readBuild(ctx, file)
				if stage == "final" {
					replace()
				}
				return metadata, err
			}
			check := inspectDaemonBinary(t.Context(), options, observation)
			if !replaced {
				t.Fatal("executable replacement boundary was not reached")
			}
			requireIdentityBoundary(t, check, "daemon.binary", stage, "executable-inode-mismatch")
			requireDoctorEvidence(t, check, "Daemon build: unknown", "Executing CLI build: version=dev")
			requireIdentityPrivacy(t, check, "Daemon build: version=1.2.3", doctorTestDaemonBuild.Commit, "digest match", "inode match")
		})
	}
}

func TestDoctorIdentityBoundariesFinalFailureDiscardsMetadata(t *testing.T) {
	for _, failure := range []struct{ path, reason string }{
		{"/proc/32100/stat", "process-token-read"},
		{"/proc/32100/status", "process-uid-read"},
		{"/proc/32100/cmdline", "process-command-read"},
		{"/proc/locks", "held-lock-read"},
		{"/proc/32100/exe", "executable-open"},
	} {
		t.Run(failure.reason, func(t *testing.T) {
			observation, options, _, _, _ := doctorBinaryFixture(t, "same", "same")
			read, open, readBuild := runtimeProbes.readFile, runtimeProbes.openBinary, runtimeProbes.readBuild
			finishedMetadata := false
			runtimeProbes.readFile = func(path string, limit int) ([]byte, error) {
				if finishedMetadata && path == failure.path {
					return nil, fmt.Errorf("private-final-probe: %w", syscall.EACCES)
				}
				return read(path, limit)
			}
			runtimeProbes.openBinary = func(path string) (*os.File, os.FileInfo, error) {
				if finishedMetadata && path == failure.path {
					return nil, nil, &os.PathError{Op: "open", Path: "/private-final-path", Err: syscall.EACCES}
				}
				return open(path)
			}
			runtimeProbes.readBuild = func(ctx context.Context, file *os.File) (buildmetadata.Metadata, error) {
				metadata, err := readBuild(ctx, file)
				finishedMetadata = true
				return metadata, err
			}
			check := inspectDaemonBinary(t.Context(), options, observation)
			if !finishedMetadata {
				t.Fatal("final failure fixture never completed its metadata read")
			}
			requireIdentityBoundary(t, check, "daemon.binary", "final", failure.reason, "errno=EACCES")
			requireDoctorEvidence(t, check, "Daemon build: unknown", "Executing CLI build: version=dev")
			requireIdentityPrivacy(t, check, "Daemon build: version=1.2.3", doctorTestDaemonBuild.Commit, "digest match", "inode match", "private-final-probe", "private-final-path")
		})
	}
}

func TestDoctorIdentityBoundariesRealReadBudget(t *testing.T) {
	// Keep this literal independent of maxProcRead so a raised production budget
	// cannot silently weaken the acceptance boundary.
	const budget = 65536
	for _, path := range []string{"/proc/32100/stat", "/proc/32100/status", "/proc/32100/cmdline", "/proc/locks"} {
		for _, over := range []bool{false, true} {
			t.Run(path+"/over="+strconv.FormatBool(over), func(t *testing.T) {
				observation, options, _, _, _ := doctorBinaryFixture(t, "same", "same")
				read := runtimeProbes.readFile
				data, err := read(path, budget)
				if err != nil {
					t.Fatal(err)
				}
				size := budget
				if over {
					size++
				}
				if strings.HasSuffix(path, "/cmdline") {
					// A long argv[0] remains valid by basename; repeated NULs
					// would instead introduce invalid empty daemon arguments.
					const tail = "/sway-session\x00daemon\x00"
					data = []byte("/" + strings.Repeat("a", size-len(tail)-1) + tail)
				} else {
					data = append(data, []byte(strings.Repeat(" ", size-len(data)))...)
				}
				fixture := filepath.Join(t.TempDir(), "bounded-proc-record")
				if err := os.WriteFile(fixture, data, 0o600); err != nil {
					t.Fatal(err)
				}
				seen := false
				runtimeProbes.readFile = func(request string, limit int) ([]byte, error) {
					if request == path {
						seen = true
						if limit != budget {
							t.Errorf("process read budget changed: got=%d want=%d", limit, budget)
						}
						return readFileBounded(fixture, limit)
					}
					return read(request, limit)
				}
				check := inspectDaemonBinary(t.Context(), options, observation)
				if !seen {
					t.Fatal("real bounded reader was never reached")
				}
				if over {
					reasons := map[string]string{"/proc/32100/stat": "process-token-read", "/proc/32100/status": "process-uid-read", "/proc/32100/cmdline": "process-command-read", "/proc/locks": "held-lock-read"}
					requireIdentityBoundary(t, check, "daemon.binary", "initial", reasons[path], "outcome=over-bound", "limit=65536")
				} else if check.Status != OK {
					t.Fatalf("record exactly at the read budget rejected: %+v", check)
				}
			})
		}
	}
}
