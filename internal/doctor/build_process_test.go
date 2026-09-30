//go:build linux

package doctor

import (
	"context"
	"debug/buildinfo"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/buildmetadata"
)

// This helper uses only the standard library and disposable files. Its argv
// deliberately matches the real daemon; the runtime directory is passed only
// through the environment, so doctor verifies the actual process command line.
const doctorBuildProcessSource = `package main
import (
 "fmt"
 "os"
 "syscall"
 "time"
)
var stamp string
func main() {
 if len(os.Args) != 2 || os.Args[1] != "daemon" || stamp == "" { os.Exit(2) }
 lockPath := os.Getenv("SWAY_SESSION_DOCTOR_TEST_LOCK")
 if lockPath == "" { os.Exit(2) }
 lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
 if err != nil { panic(err) }
 defer lock.Close()
 if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { panic(err) }
 // Referencing the linker variable retains the immutable stamp under -s -w.
 fmt.Fprintln(os.Stderr, stamp)
 fmt.Println("ready")
 for { time.Sleep(time.Hour) }
}
`

func buildDoctorProcess(t *testing.T, source, output, version, commit string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	stamp := "sway-session-build-v1|" + version + "|" + commit + "|false|end-sway-session-build-v1"
	command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-ldflags", "-s -w -buildid= -X main.stamp="+stamp, "-o", output, source)
	command.WaitDelay = time.Second
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build disposable helper: %v\n%s", err, data)
	}
	info, err := buildinfo.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range info.Settings {
		if setting.Key == "-ldflags" {
			t.Fatal("trimpath fixture unexpectedly retains recorded linker flags")
		}
	}
}

func TestDoctorBuildIdentifiesRealHeldLockProcessAfterExecutableReplacement(t *testing.T) {
	previous := runtimeProbes
	t.Cleanup(func() { runtimeProbes = previous })
	// These are production observation boundaries, not simulated procfs records.
	runtimeProbes.readFile = readFileBounded
	runtimeProbes.readlink = os.Readlink
	runtimeProbes.openBinary = openBoundedBinary
	runtimeProbes.readBuild = buildmetadata.ReadExecutable

	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "helper.go")
	if err := os.WriteFile(source, []byte(doctorBuildProcessSource), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "sway-session")
	replacement := filepath.Join(root, "replacement")
	oldBuild := buildmetadata.Metadata{Version: "1.2.3", Commit: strings.Repeat("a", 40)}
	newBuild := buildmetadata.Metadata{Version: "1.2.4", Commit: strings.Repeat("b", 40)}
	buildDoctorProcess(t, source, executable, oldBuild.Version, oldBuild.Commit)
	buildDoctorProcess(t, source, replacement, newBuild.Version, newBuild.Commit)

	stderr, err := os.OpenFile(filepath.Join(root, "helper.stderr"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stderr.Close() })
	command := exec.Command(executable, "daemon")
	command.Env = append(os.Environ(), "SWAY_SESSION_DOCTOR_TEST_LOCK="+filepath.Join(root, "daemon.lock"))
	command.Stderr = stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		_ = stdout.Close()
		t.Fatal(err)
	}
	// Every exit path kills and reaps this exact disposable process. No daemon
	// stop command or user runtime directory is involved.
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = stdout.Close()
	})
	ready := make(chan error, 1)
	go func() {
		data := make([]byte, len("ready\n"))
		_, err := io.ReadFull(stdout, data)
		if err == nil && string(data) != "ready\n" {
			err = fmt.Errorf("unexpected helper readiness %q", data)
		}
		ready <- err
	}()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("helper readiness: %v", err)
		}
	case <-deadline.C:
		t.Fatal("helper did not acquire its disposable lock within three seconds")
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}

	runtime := observePrivateDirectory(root, nil)
	if runtime.err != nil {
		t.Fatal(runtime.err)
	}
	t.Cleanup(runtime.Close)
	observation := inspectDaemonLock(runtime)
	if !observation.valid || observation.check.Status != OK || observation.pid != command.Process.Pid || observation.startTime == 0 {
		t.Fatalf("real held-lock process not verified: %+v", observation)
	}
	options := Options{Executable: executable, CLIBuild: &newBuild}
	before := inspectDaemonBinary(t.Context(), options, observation)
	if before.Status != OK {
		t.Fatalf("original live executable: %+v", before)
	}
	requireDoctorEvidence(t, before, "inode match", "Executing CLI build: version=1.2.4", "Daemon build: version=1.2.3 commit="+oldBuild.Commit+" modified=false")

	// Atomic replacement unlinks the executable held by the live process. The
	// procfs descriptor still reads the old stamp, rather than the installed one.
	if err := os.Rename(replacement, executable); err != nil {
		t.Fatal(err)
	}
	livePath, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", command.Process.Pid))
	if err != nil || !strings.HasSuffix(livePath, " (deleted)") {
		t.Fatalf("old live executable was not deleted: %q %v", livePath, err)
	}
	installed, err := buildmetadata.ReadFile(executable)
	if err != nil || installed != newBuild {
		t.Fatalf("replacement build: %+v %v", installed, err)
	}
	after := inspectDaemonBinary(t.Context(), options, observation)
	if after.Status != Warning || !strings.Contains(after.Detail, "deleted binary") || !strings.Contains(after.Detail, "restart is needed") {
		t.Fatalf("replaced live executable did not require restart: %+v", after)
	}
	requireDoctorEvidence(t, after, "Executing CLI build: version=1.2.4", "Daemon build: version=1.2.3 commit="+oldBuild.Commit+" modified=false")
	if strings.Contains(strings.Join(after.Evidence, "\n"), "Daemon build: version=1.2.4") {
		t.Fatalf("installed metadata substituted for the live daemon: %+v", after)
	}
	if !daemonIdentityUnchanged(observation) || !strings.Contains(after.Hint, fmt.Sprintf("kill -TERM %d", command.Process.Pid)) {
		t.Fatalf("original live process identity or restart procedure lost: %+v", after)
	}
}
