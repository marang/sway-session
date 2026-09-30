package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marang/sway-session/internal/buildmetadata"
)

var doctorTestCLIBuild = buildmetadata.Metadata{Version: "dev", Commit: strings.Repeat("a", 40)}
var doctorTestDaemonBuild = buildmetadata.Metadata{Version: "1.2.3", Commit: strings.Repeat("b", 40), Modified: true}

func doctorBinaryFixture(t *testing.T, live, installed string) (daemonObservation, Options, string, func(uint64), func(bool)) {
	t.Helper()
	observation, setStart, setHeld := doctorDaemonFixture(t)
	root := t.TempDir()
	livePath, installedPath := filepath.Join(root, "live"), filepath.Join(root, "installed")
	for path, contents := range map[string]string{livePath: live, installedPath: installed} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runtimeProbes.openBinary = func(path string) (*os.File, os.FileInfo, error) {
		if path == "/proc/32100/exe" {
			path = livePath
		}
		return openBoundedBinary(path)
	}
	runtimeProbes.readlink = func(string) (string, error) { return livePath, nil }
	runtimeProbes.readBuild = func(_ context.Context, file *os.File) (buildmetadata.Metadata, error) {
		// Metadata must be read from the actual live file, never the newer path.
		data, err := io.ReadAll(file)
		if err != nil || string(data) != live {
			t.Fatalf("metadata read from wrong executable: %q %v", data, err)
		}
		return doctorTestDaemonBuild, nil
	}
	return observation, Options{Executable: installedPath, CLIBuild: &doctorTestCLIBuild}, livePath, setStart, setHeld
}

func requireDoctorEvidence(t *testing.T, check Check, fragments ...string) {
	t.Helper()
	text := strings.Join(check.Evidence, "\n")
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			t.Fatalf("missing %q: %+v", fragment, check)
		}
	}
}

func TestDoctorBuildIdentifiesCLIAndLiveDaemonSeparately(t *testing.T) {
	observation, options, _, _, _ := doctorBinaryFixture(t, "old executable", "new executable")
	check := inspectDaemonBinary(context.Background(), options, observation)
	if check.Status != Warning {
		t.Fatalf("changed executable: %+v", check)
	}
	requireDoctorEvidence(t, check, "Executing CLI build: version=dev", "Daemon build: version=1.2.3", "commit="+doctorTestDaemonBuild.Commit, "modified=true")
	for _, fragment := range []string{"kill -TERM 32100", "release the lock", "sway-session daemon", "--socket", "Reloading Sway does not restart"} {
		if !strings.Contains(check.Hint, fragment) {
			t.Fatalf("missing restart procedure %q: %+v", fragment, check)
		}
	}
}

func TestDoctorBuildMatchingInodeAndMatchingContents(t *testing.T) {
	for _, inodeMatch := range []bool{true, false} {
		t.Run(fmt.Sprint(inodeMatch), func(t *testing.T) {
			observation, options, livePath, _, _ := doctorBinaryFixture(t, "identical", "identical")
			if inodeMatch {
				options.Executable = livePath
			}
			check := inspectDaemonBinary(context.Background(), options, observation)
			if check.Status != OK {
				t.Fatalf("matching executable: %+v", check)
			}
			match := "digest match"
			if inodeMatch {
				match = "inode match"
			}
			requireDoctorEvidence(t, check, match, "Executing CLI build: version=dev", "Daemon build: version=1.2.3")
		})
	}
}

func TestDoctorBuildSameVersionDifferentContentsStillWarns(t *testing.T) {
	observation, options, _, _, _ := doctorBinaryFixture(t, "contents A", "contents B")
	options.CLIBuild = &doctorTestDaemonBuild
	check := inspectDaemonBinary(context.Background(), options, observation)
	if check.Status != Warning {
		t.Fatalf("equal version masked content mismatch: %+v", check)
	}
	requireDoctorEvidence(t, check, "Executing CLI build: version=1.2.3", "Daemon build: version=1.2.3")
}

func TestDoctorBuildReadsPinnedDeletedExecutable(t *testing.T) {
	observation, options, livePath, _, _ := doctorBinaryFixture(t, "old executable", "new executable")
	pinned, _, err := openBoundedBinary(livePath)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	if err := os.Remove(livePath); err != nil {
		t.Fatal(err)
	}
	open := runtimeProbes.openBinary
	runtimeProbes.openBinary = func(path string) (*os.File, os.FileInfo, error) {
		if path == "/proc/32100/exe" {
			return openBoundedBinary(fmt.Sprintf("/proc/self/fd/%d", pinned.Fd()))
		}
		return open(path)
	}
	runtimeProbes.readlink = func(string) (string, error) { return livePath + " (deleted)", nil }
	check := inspectDaemonBinary(context.Background(), options, observation)
	if check.Status != Warning || !strings.Contains(check.Detail, "deleted binary") {
		t.Fatalf("deleted executable: %+v", check)
	}
	requireDoctorEvidence(t, check, "Daemon build: version=1.2.3")
}

func TestDoctorBuildDiscardsResultsAfterIdentityChanges(t *testing.T) {
	for _, change := range []string{"pid", "lock", "exec"} {
		t.Run(change, func(t *testing.T) {
			observation, options, livePath, setStart, setHeld := doctorBinaryFixture(t, "old executable", "new executable")
			read := runtimeProbes.readBuild
			runtimeProbes.readBuild = func(ctx context.Context, file *os.File) (buildmetadata.Metadata, error) {
				value, err := read(ctx, file)
				switch change {
				case "pid":
					setStart(101)
				case "lock":
					setHeld(false)
				case "exec":
					if err := os.Rename(livePath, livePath+".old"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(livePath, []byte("replacement"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return value, err
			}
			check := inspectDaemonBinary(context.Background(), options, observation)
			if check.Status != Unavailable {
				t.Fatalf("changed identity trusted: %+v", check)
			}
			requireDoctorEvidence(t, check, "Executing CLI build: version=dev", "Daemon build: unknown", "changed during observation")
			if strings.Contains(strings.Join(check.Evidence, "\n"), "Daemon build: version=1.2.3") {
				t.Fatalf("stale metadata retained: %+v", check)
			}
		})
	}
}

func TestDoctorBuildUnreadableAndLegacyMetadataExplainUnknown(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			observation, options, _, _, _ := doctorBinaryFixture(t, "same", "same")
			runtimeProbes.readBuild = func(context.Context, *os.File) (buildmetadata.Metadata, error) {
				if legacy {
					return buildmetadata.Metadata{Version: "unknown", Commit: "unknown"}, nil
				}
				return buildmetadata.Metadata{}, errors.New("unreadable")
			}
			check := inspectDaemonBinary(context.Background(), options, observation)
			if check.Status != OK {
				t.Fatalf("metadata unknown should preserve digest evidence: %+v", check)
			}
			requireDoctorEvidence(t, check, "digest match", "Executing CLI build: version=dev", "older")
			if legacy {
				requireDoctorEvidence(t, check, "Daemon build: version=unknown commit=unknown modified=unknown")
			} else {
				requireDoctorEvidence(t, check, "Daemon build: unknown")
			}
		})
	}
}

func TestDoctorBuildUnavailableDaemonKeepsExecutingCLIIdentity(t *testing.T) {
	previous := runtimeProbes
	t.Cleanup(func() { runtimeProbes = previous })
	runtimeProbes.openBinary = func(string) (*os.File, os.FileInfo, error) {
		t.Fatal("unverified daemon executable opened")
		return nil, nil, os.ErrPermission
	}
	metadata := buildmetadata.Executing("legacy-version", "unknown", "unknown")
	check := inspectDaemonBinary(context.Background(), Options{CLIBuild: &metadata}, daemonObservation{})
	requireDoctorEvidence(t, check, "Executing CLI build: version=legacy-version", "Daemon build: unknown", "no verified live executable")
}

func TestDoctorBuildUnreadableExecutableAndCancelledMetadata(t *testing.T) {
	observation, options, _, _, _ := doctorBinaryFixture(t, "same", "same")
	open := runtimeProbes.openBinary
	runtimeProbes.openBinary = func(path string) (*os.File, os.FileInfo, error) {
		if path == "/proc/32100/exe" {
			return nil, nil, os.ErrPermission
		}
		return open(path)
	}
	check := inspectDaemonBinary(context.Background(), options, observation)
	if check.Status != Unavailable {
		t.Fatalf("unreadable executable: %+v", check)
	}
	requireDoctorEvidence(t, check, "Executing CLI build: version=dev", "Daemon build: unknown", "unreadable")
	runtimeProbes.openBinary = open
	runtimeProbes.readBuild = func(ctx context.Context, _ *os.File) (buildmetadata.Metadata, error) {
		return buildmetadata.Metadata{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	check = inspectDaemonBinary(ctx, options, observation)
	if check.Status != Unavailable {
		t.Fatalf("cancelled digest should remain unavailable: %+v", check)
	}
	requireDoctorEvidence(t, check, "Daemon build: unknown")
}

// Exercise the real reader against an actual running executable, while the
// comparison path and lock/process records remain disposable/injected.
func TestDoctorBuildReadsRealProcExecutable(t *testing.T) {
	observation, options, _, _, _ := doctorBinaryFixture(t, "ignored", "installed replacement")
	expected, err := buildmetadata.ReadFile("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	open := runtimeProbes.openBinary
	runtimeProbes.openBinary = func(path string) (*os.File, os.FileInfo, error) {
		if path == "/proc/32100/exe" {
			return openBoundedBinary("/proc/self/exe")
		}
		return open(path)
	}
	runtimeProbes.readBuild = buildmetadata.ReadExecutable
	check := inspectDaemonBinary(t.Context(), options, observation)
	if check.Status != Warning {
		t.Fatalf("live proc executable: %+v", check)
	}
	for _, evidence := range buildEvidence("Daemon", expected) {
		requireDoctorEvidence(t, check, evidence)
	}
	requireDoctorEvidence(t, check, "Executing CLI build: version=dev")
}
