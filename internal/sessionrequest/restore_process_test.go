package sessionrequest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"golang.org/x/sys/unix"
)

const restoreProcessHelperMode = "SWAY_SESSION_RESTORE_PROCESS_TEST_HELPER"

const restoreProcessContextID sessionstate.ContextID = "3e7dfb0a-5e6e-4c95-a3b9-a68479fb2152"

type restoreProcessProbe struct {
	Parent     int
	Descendant int
}

type restoreInvocationProbe struct {
	Arguments []string
	Path      string
	Loader    []string
	Retained  string
}

func TestExecRestoreRunnerProcess(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"cancel", "deadline", "successful-parent-held-pipes", "success", "failed-exit", "trusted-invocation"} {
		t.Run(scenario, func(t *testing.T) {
			command := exec.Command(executable, "-test.run=^TestExecRestoreRunnerProcessHelper$")
			command.Env = append(os.Environ(),
				restoreProcessHelperMode+"=supervisor",
				"SWAY_SESSION_RESTORE_PROCESS_TEST_SCENARIO="+scenario,
				"GORACE=atexit_sleep_ms=0",
			)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("isolated restore scenario: %v\n%s", err, output)
			}
		})
	}
}

func TestExecRestoreRunnerProcessHelper(t *testing.T) {
	mode := os.Getenv(restoreProcessHelperMode)
	if mode == "" {
		return
	}
	directory := os.Getenv("SWAY_SESSION_RESTORE_PROCESS_TEST_DIRECTORY")
	scenario := os.Getenv("SWAY_SESSION_RESTORE_PROCESS_TEST_SCENARIO")
	switch mode {
	case "descendant":
		if err := os.WriteFile(filepath.Join(directory, "ready"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		// Keep both inherited output descriptors open until the supervisor
		// kills and reaps this child. A fallback bounds a broken test harness.
		time.Sleep(10 * time.Second)
		os.Exit(0)
	case "restore":
		runRestoreProcessHelper(t, scenario, directory)
	case "supervisor":
		// This private test process owns only this scenario's restore parent
		// and adopts its descendant for exact-PID cleanup. The main package
		// test process never changes its child-reaping behavior.
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// Normally the exact-PID cleanup below has already reaped the
			// descendant. If fixture setup fails before its PID is published,
			// reap all remaining children of this isolated helper; every
			// fixture process has a finite fallback lifetime. This never
			// runs in the normal package test process.
			for {
				_, err := syscall.Wait4(-1, nil, 0, nil)
				if errors.Is(err, syscall.ECHILD) {
					return
				}
				if errors.Is(err, syscall.EINTR) {
					continue
				}
				if err != nil {
					t.Errorf("reap isolated restore helper children: %v", err)
					return
				}
			}
		})
		testRestoreProcessScenario(t, scenario)
	default:
		t.Fatalf("unknown restore process helper %q", mode)
	}
}

func runRestoreProcessHelper(t *testing.T, scenario, directory string) {
	t.Helper()
	if scenario == "trusted-invocation" {
		separator := -1
		for index, argument := range os.Args {
			if argument == "--" {
				separator = index
				break
			}
		}
		if separator < 0 {
			t.Fatal("restore arguments were not passed through the wrapper")
		}
		probe := restoreInvocationProbe{
			Arguments: os.Args[separator+1:],
			Path:      os.Getenv("PATH"),
			Retained:  os.Getenv("SWAY_SESSION_RESTORE_PROCESS_TEST_RETAINED"),
		}
		for _, value := range os.Environ() {
			if strings.HasPrefix(value, "LD_") {
				probe.Loader = append(probe.Loader, value)
			}
		}
		writeRestoreProcessProbe(t, filepath.Join(directory, "invocation.json"), probe)
		os.Exit(0)
	}
	if scenario == "success" {
		_, _ = fmt.Fprint(os.Stdout, "discarded restore output")
		_, _ = fmt.Fprint(os.Stderr, "successful restore detail")
		os.Exit(0)
	}
	if scenario == "failed-exit" {
		_, _ = fmt.Fprint(os.Stderr, strings.Repeat("x", 8192)+"discarded tail")
		os.Exit(23)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestExecRestoreRunnerProcessHelper$")
	child.Env = append(os.Environ(), restoreProcessHelperMode+"=descendant")
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	// Publish both identities before waiting for readiness, so failure
	// cleanup can always identify the deliberately orphaned descendant.
	err = publishRestoreProcessProbe(filepath.Join(directory, "processes.json"), restoreProcessProbe{Parent: os.Getpid(), Descendant: child.Process.Pid})
	if err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatal(err)
	}
	if scenario == "successful-parent-held-pipes" {
		os.Exit(0)
	}
	// CommandContext must kill this parent on cancellation. The descendant
	// stays alive with both pipes, reproducing the real os/exec drain wait.
	time.Sleep(10 * time.Second)
	os.Exit(0)
}

func testRestoreProcessScenario(t *testing.T, scenario string) {
	t.Helper()
	directory := t.TempDir()
	t.Setenv(restoreProcessHelperMode, "restore")
	t.Setenv("SWAY_SESSION_RESTORE_PROCESS_TEST_DIRECTORY", directory)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(directory, "restore")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestExecRestoreRunnerProcessHelper$ -- \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(directory, "unused-sway.sock")
	runner := ExecRestoreRunner{Executable: wrapper, SwaySocket: socket}
	if scenario == "success" {
		if err := runner.Restore(context.Background(), restoreProcessContextID); err != nil {
			t.Fatalf("normal restore: %v", err)
		}
		return
	}
	if scenario == "failed-exit" {
		err := runner.Restore(context.Background(), restoreProcessContextID)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 23 {
			t.Fatalf("restore lost the failed exit: %v", err)
		}
		want := "trusted restore failed: " + exit.Error() + ": " + strings.Repeat("x", 4096)
		if err.Error() != want {
			t.Fatalf("restore stderr was not capped at 4096 bytes: error length=%d want=%d", len(err.Error()), len(want))
		}
		return
	}
	if scenario == "trusted-invocation" {
		t.Setenv("PATH", "/untrusted/bin")
		t.Setenv("LD_PRELOAD", filepath.Join(directory, "untrusted.so"))
		t.Setenv("LD_LIBRARY_PATH", filepath.Join(directory, "untrusted-libraries"))
		t.Setenv("SWAY_SESSION_RESTORE_PROCESS_TEST_RETAINED", "retained")
		if err := runner.Restore(context.Background(), restoreProcessContextID); err != nil {
			t.Fatal(err)
		}
		var probe restoreInvocationProbe
		readRestoreProcessProbe(t, filepath.Join(directory, "invocation.json"), &probe)
		want := []string{"--json", "restore", "--require-active", "--socket", socket, string(restoreProcessContextID)}
		if !reflect.DeepEqual(probe.Arguments, want) {
			t.Fatalf("restore arguments changed: got=%q want=%q", probe.Arguments, want)
		}
		if probe.Path != "/usr/local/sbin:/usr/local/bin:/usr/bin" || len(probe.Loader) != 0 || probe.Retained != "retained" {
			t.Fatalf("restore environment changed: %+v", probe)
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	if scenario == "deadline" {
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	}
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- runner.Restore(ctx, restoreProcessContextID) }()
	var probe restoreProcessProbe
	readRestoreProcessProbe(t, filepath.Join(directory, "processes.json"), &probe)
	fd, err := unix.PidfdOpen(probe.Descendant, 0)
	if err != nil {
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		cancel()
		_ = unix.PidfdSendSignal(fd, syscall.SIGKILL, nil, 0)
		_ = unix.Close(fd)
		if !finished {
			select {
			case <-result:
			case <-time.After(2 * time.Second):
				t.Error("restore did not finish after owned descendant cleanup")
			}
		}
		var status syscall.WaitStatus
		if waited, err := syscall.Wait4(probe.Descendant, &status, 0, nil); err != nil || waited != probe.Descendant {
			t.Errorf("reap owned restore descendant: pid=%d err=%v", waited, err)
		}
	})
	readRestoreProcessProbe(t, filepath.Join(directory, "ready"), nil)
	if scenario == "cancel" {
		cancel()
	} else if scenario == "deadline" {
		<-ctx.Done()
	}
	started := time.Now()
	select {
	case err := <-result:
		finished = true
		if scenario == "successful-parent-held-pipes" {
			if !errors.Is(err, exec.ErrWaitDelay) {
				t.Fatalf("successful parent with inherited pipes did not preserve ErrWaitDelay: %v", err)
			}
		} else {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("canceled restore lost its direct-process exit error: %v", err)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("restore still waited for inherited stdout/stderr after %s (%s)", time.Since(started), scenario)
	}
	if err := unix.PidfdSendSignal(fd, 0, nil, 0); err != nil {
		t.Fatalf("restore unexpectedly killed its inherited-pipe descendant: %v", err)
	}
}

func writeRestoreProcessProbe(t *testing.T, path string, value any) {
	t.Helper()
	if err := publishRestoreProcessProbe(path, value); err != nil {
		t.Fatal(err)
	}
}

func publishRestoreProcessProbe(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func readRestoreProcessProbe(t *testing.T, path string, value any) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			if value != nil {
				if err := json.Unmarshal(data, value); err != nil {
					t.Fatal(err)
				}
			}
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("restore process helper did not publish %s", filepath.Base(path))
}
