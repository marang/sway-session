package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const herdrProcessHelperTest = "-test.run=^TestExecCommandRunnerProcessHelper$"

func TestExecCommandRunnerProcess(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, herdrProcessHelperTest, "--", "supervisor", t.TempDir())
	command.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0")
	command.WaitDelay = time.Second
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("isolated Herdr subprocess checks: %v\n%s", err, output)
	}
}

func TestExecCommandRunnerProcessHelper(t *testing.T) {
	if len(os.Args) < 5 || os.Args[2] != "--" {
		return
	}
	mode, directory := os.Args[3], os.Args[4]
	switch mode {
	case "success", "failure":
		fmt.Fprintln(os.Stdout, "parent stdout")
		fmt.Fprintln(os.Stderr, "parent stderr")
		if mode == "failure" {
			os.Exit(7)
		}
		os.Exit(0)
	case "overflow":
		if _, err := os.Stdout.Write(bytes.Repeat([]byte("x"), maxHerdrOutputSize+128)); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	case "descendant":
		publishHerdrProcessFile(t, directory, "descendant-ready", nil)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(directory, "release")); err == nil {
				os.Exit(0)
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		os.Exit(0)
	case "held-parent", "exited-parent":
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		child := exec.Command(executable, herdrProcessHelperTest, "--", "descendant", directory)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		publishHerdrProcessFile(t, directory, "descendant-pid", []byte(strconv.Itoa(child.Process.Pid)))
		waitForHerdrProcessFile(t, directory, "descendant-ready")
		fmt.Fprintln(os.Stdout, "parent stdout")
		fmt.Fprintln(os.Stderr, "parent stderr")
		publishHerdrProcessFile(t, directory, "parent-ready", nil)
		if mode == "exited-parent" {
			os.Exit(0)
		}
		time.Sleep(15 * time.Second)
		os.Exit(0)
	case "supervisor":
		// Only this isolated process adopts deliberately orphaned descendants.
		// The package's main test process never changes its child ownership.
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GORACE", "atexit_sleep_ms=0")
		t.Run("ordinary_completion", testHerdrOrdinaryCompletion)
		for _, trigger := range []string{"cancel", "deadline", "parent-exit"} {
			t.Run(trigger, func(t *testing.T) { testHerdrInheritedPipes(t, trigger) })
		}
	default:
		t.Fatalf("unknown Herdr process helper mode %q", mode)
	}
}

func herdrProcessArguments(mode, directory string) []string {
	return []string{herdrProcessHelperTest, "--", mode, directory}
}

func testHerdrOrdinaryCompletion(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"success", "failure", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			output, err := (ExecCommandRunner{}).CombinedOutput(ctx, executable, herdrProcessArguments(mode, t.TempDir())...)
			if mode == "failure" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 7 {
					t.Fatalf("exit status changed: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if mode == "overflow" {
				if len(output) != maxHerdrOutputSize+1 || !bytes.Equal(output, bytes.Repeat([]byte("x"), maxHerdrOutputSize+1)) {
					t.Fatalf("output overflow sentinel changed: got %d bytes", len(output))
				}
			} else {
				assertHerdrProcessOutput(t, output)
			}
		})
	}
}

func testHerdrInheritedPipes(t *testing.T, trigger string) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	if trigger == "deadline" {
		cancel()
		ctx, cancel = context.WithTimeout(t.Context(), 2*time.Second)
	}
	defer cancel()
	mode := "held-parent"
	if trigger == "parent-exit" {
		mode = "exited-parent"
	}
	type result struct {
		output   []byte
		err      error
		returned time.Time
	}
	done := make(chan result, 1)
	go func() {
		output, err := (ExecCommandRunner{}).CombinedOutput(ctx, executable, herdrProcessArguments(mode, directory)...)
		done <- result{output: output, err: err, returned: time.Now()}
	}()
	pidBytes := waitForHerdrProcessFile(t, directory, "descendant-pid")
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid descendant PID %q: %v", pidBytes, err)
	}
	reaped := false
	registerHerdrDescendantCleanup(t, directory, pid, &reaped)
	waitForHerdrProcessFile(t, directory, "parent-ready")
	triggered := time.Now()
	switch trigger {
	case "cancel":
		cancel()
	case "deadline":
		triggered, _ = ctx.Deadline()
	}
	var completed result
	select {
	case completed = <-done:
	case <-time.After(7 * time.Second):
		t.Fatal("Herdr runner did not return after its inherited pipes' bounded hold")
	}
	// The descendant holds both inherited streams for five seconds. Allow
	// scheduling slack beyond the 250ms drain budget, but catch that full hold.
	if elapsed := completed.returned.Sub(triggered); elapsed > 1500*time.Millisecond {
		t.Errorf("Herdr runner waited for descendant-held stdout/stderr after %s: %s", trigger, elapsed)
	}
	if trigger == "parent-exit" {
		if !errors.Is(completed.err, exec.ErrWaitDelay) {
			t.Errorf("successful parent with inherited pipes: got %v, want exec.ErrWaitDelay", completed.err)
		}
	} else {
		var exit *exec.ExitError
		if !errors.As(completed.err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() {
			t.Errorf("canceled direct process lost its exit failure: %v", completed.err)
		}
		expected := context.Canceled
		if trigger == "deadline" {
			expected = context.DeadlineExceeded
		}
		if !errors.Is(ctx.Err(), expected) {
			t.Errorf("context cause: got %v, want %v", ctx.Err(), expected)
		}
	}
	assertHerdrProcessOutput(t, completed.output)
	// A drain budget closes our read ends; it must not kill an intentional
	// persistent descendant. The supervisor releases and reaps it afterward.
	var status syscall.WaitStatus
	waited, waitErr := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	reaped = waited == pid
	if waitErr != nil || waited != 0 {
		t.Errorf("descendant did not remain running after runner return: pid=%d status=%v err=%v", waited, status, waitErr)
	}
}

func assertHerdrProcessOutput(t *testing.T, output []byte) {
	t.Helper()
	for _, text := range []string{"parent stdout\n", "parent stderr\n"} {
		if !bytes.Contains(output, []byte(text)) {
			t.Errorf("captured output lost %q: %q", text, output)
		}
	}
}

func publishHerdrProcessFile(t *testing.T, directory, name string, data []byte) {
	t.Helper()
	temporary := filepath.Join(directory, name+".tmp")
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, filepath.Join(directory, name)); err != nil {
		t.Fatal(err)
	}
}

func waitForHerdrProcessFile(t *testing.T, directory, name string) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err == nil {
			return data
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("Herdr helper did not publish %s", name)
	return nil
}

func registerHerdrDescendantCleanup(t *testing.T, directory string, pid int, reaped *bool) {
	t.Helper()
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer unix.Close(fd)
		if *reaped {
			return
		}
		if err := os.WriteFile(filepath.Join(directory, "release"), nil, 0o600); err != nil {
			t.Error(err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			waited, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
			if waited == pid {
				return
			}
			// Failure cleanup may begin while the direct parent is still
			// being killed. Wait until adoption instead of abandoning ECHILD.
			if err != nil && !errors.Is(err, syscall.EINTR) && !errors.Is(err, syscall.ECHILD) {
				t.Error(err)
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		// Kill only this owned PID on failed cleanup; never signal a process
		// group or consume another component's children with waitpid(-1).
		t.Errorf("descendant %d did not exit and become reapable after release", pid)
		_ = unix.PidfdSendSignal(fd, syscall.SIGKILL, nil, 0)
		if waited, err := syscall.Wait4(pid, nil, 0, nil); err != nil && !errors.Is(err, syscall.ECHILD) {
			t.Errorf("reap descendant %d: waited=%d err=%v", pid, waited, err)
		}
	})
}
