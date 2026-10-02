package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const launcherHelperMode = "SWAY_SESSION_LAUNCHER_TEST_HELPER"

type launcherProcessProbe struct {
	PID     int
	Parent  int
	Session int
}

func launcherHelperSpec(t *testing.T, mode, directory, gate string, exitCode int) ProcessSpec {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return ProcessSpec{
		Name:      executable,
		Arguments: []string{"-test.run=^TestExecProcessStarterHelper$"},
		Environment: []string{
			launcherHelperMode + "=" + mode,
			"SWAY_SESSION_LAUNCHER_TEST_DIRECTORY=" + directory,
			"SWAY_SESSION_LAUNCHER_TEST_GATE=" + gate,
			"SWAY_SESSION_LAUNCHER_TEST_EXIT=" + strconv.Itoa(exitCode),
			"GORACE=atexit_sleep_ms=0",
		},
	}
}

func launcherHelperCommand(t *testing.T, spec ProcessSpec) *exec.Cmd {
	t.Helper()
	command := exec.Command(spec.Name, spec.Arguments...)
	environment, err := mergeEnvironment(os.Environ(), spec.Environment, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	command.Env = environment
	return command
}

func TestExecProcessStarterLifecycle(t *testing.T) {
	spec := launcherHelperSpec(t, "parent", t.TempDir(), "", 0)
	command := launcherHelperCommand(t, spec)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// CommandContext owns only the isolated test parent. Production children
	// are started by the real ExecProcessStarter inside that parent.
	bounded := exec.CommandContext(ctx, command.Path, command.Args[1:]...)
	bounded.Env = command.Env
	if output, err := bounded.CombinedOutput(); err != nil {
		t.Fatalf("isolated launcher lifecycle: %v\n%s", err, output)
	}
}

func TestExecProcessStarterHelper(t *testing.T) {
	mode := os.Getenv(launcherHelperMode)
	if mode == "" {
		return
	}
	directory := os.Getenv("SWAY_SESSION_LAUNCHER_TEST_DIRECTORY")
	gate := os.Getenv("SWAY_SESSION_LAUNCHER_TEST_GATE")
	exitCode, err := strconv.Atoi(os.Getenv("SWAY_SESSION_LAUNCHER_TEST_EXIT"))
	if err != nil {
		t.Fatal(err)
	}
	if mode == "child" {
		session, err := unix.Getsid(0)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(launcherProcessProbe{PID: os.Getpid(), Parent: os.Getppid(), Session: session})
		if err != nil {
			t.Fatal(err)
		}
		temporary := filepath.Join(directory, "probe.tmp")
		if err := os.WriteFile(temporary, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temporary, filepath.Join(directory, "probe.json")); err != nil {
			t.Fatal(err)
		}
		if gate != "" {
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(gate); err == nil {
					break
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if time.Now().After(deadline) {
					t.Fatal("test child gate timed out")
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		os.Exit(exitCode)
	}
	if mode == "cli" {
		if err := (ExecProcessStarter{}).Start(launcherHelperSpec(t, "child", directory, gate, exitCode)); err != nil {
			t.Fatal(err)
		}
		waitForLauncherProbe(t, directory)
		os.Exit(0)
	}
	if mode != "parent" {
		t.Fatalf("unknown launcher test helper %q", mode)
	}
	// Adopt the deliberately orphaned CLI child so this test can reap it
	// without depending on the host's init process. No global wait is used.
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	t.Run("reaps_multiple_exited_children", func(t *testing.T) {
		var children []launcherProcessProbe
		for index, exitCode := range []int{0, 7, 0, 19} {
			childDirectory := filepath.Join(directory, fmt.Sprintf("short-%d", index))
			if err := os.Mkdir(childDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := (ExecProcessStarter{}).Start(launcherHelperSpec(t, "child", childDirectory, "", exitCode)); err != nil {
				t.Fatal(err)
			}
			probe := waitForLauncherProbe(t, childDirectory)
			registerLauncherChildCleanup(t, probe.PID)
			if probe.Parent != os.Getpid() || probe.Session != probe.PID {
				t.Fatalf("child ownership/session changed: %+v", probe)
			}
			children = append(children, probe)
		}
		for _, child := range children {
			waitForLauncherChildReaped(t, child.PID)
		}
	})
	t.Run("running_child_does_not_block_start", func(t *testing.T) {
		childDirectory := t.TempDir()
		gate := filepath.Join(childDirectory, "exit")
		started := time.Now()
		if err := (ExecProcessStarter{}).Start(launcherHelperSpec(t, "child", childDirectory, gate, 0)); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("Start waited for a running child: %s", elapsed)
		}
		probe := waitForLauncherProbe(t, childDirectory)
		registerLauncherChildCleanup(t, probe.PID)
		if err := syscall.Kill(probe.PID, 0); err != nil {
			t.Fatalf("child did not stay alive after Start: %v", err)
		}
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		waitForLauncherChildReaped(t, probe.PID)
	})
	t.Run("start_errors_are_synchronous", func(t *testing.T) {
		for _, spec := range []ProcessSpec{
			{Name: filepath.Join(directory, "missing-executable")},
			{Name: "/usr/bin/true", Environment: []string{"INVALID"}},
		} {
			if err := (ExecProcessStarter{}).Start(spec); err == nil {
				t.Fatalf("invalid launch returned success: %+v", spec)
			}
		}
	})
	t.Run("does_not_reap_another_command", func(t *testing.T) {
		childDirectory := t.TempDir()
		foreign := launcherHelperCommand(t, launcherHelperSpec(t, "child", childDirectory, "", 23))
		if err := foreign.Start(); err != nil {
			t.Fatal(err)
		}
		waited := false
		t.Cleanup(func() {
			if !waited {
				_ = foreign.Process.Kill()
				_ = foreign.Wait()
			}
		})
		waitForLauncherProbe(t, childDirectory)
		ownDirectory := t.TempDir()
		if err := (ExecProcessStarter{}).Start(launcherHelperSpec(t, "child", ownDirectory, "", 0)); err != nil {
			t.Fatal(err)
		}
		own := waitForLauncherProbe(t, ownDirectory)
		registerLauncherChildCleanup(t, own.PID)
		waitForLauncherChildReaped(t, own.PID)
		var exit *exec.ExitError
		err := foreign.Wait()
		waited = true
		if !errors.As(err, &exit) || exit.ExitCode() != 23 {
			t.Fatalf("another command lost its exit status: %v", err)
		}
	})
	t.Run("child_survives_short_lived_cli", func(t *testing.T) {
		childDirectory := t.TempDir()
		gate := filepath.Join(childDirectory, "exit")
		cli := launcherHelperCommand(t, launcherHelperSpec(t, "cli", childDirectory, gate, 0))
		if output, err := cli.CombinedOutput(); err != nil {
			t.Fatalf("short-lived CLI: %v\n%s", err, output)
		}
		probe := waitForLauncherProbe(t, childDirectory)
		registerLauncherChildCleanup(t, probe.PID)
		if parent, err := launcherChildParent(probe.PID); err != nil || parent != os.Getpid() || probe.Session != probe.PID {
			t.Fatalf("detached child did not survive CLI exit: parent=%d probe=%+v err=%v", parent, probe, err)
		}
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		var status syscall.WaitStatus
		if waited, err := syscall.Wait4(probe.PID, &status, 0, nil); err != nil || waited != probe.PID || !status.Exited() || status.ExitStatus() != 0 {
			t.Fatalf("adopted child completion: pid=%d status=%v err=%v", waited, status, err)
		}
	})
}

func waitForLauncherProbe(t *testing.T, directory string) launcherProcessProbe {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(directory, "probe.json"))
		if err == nil {
			var probe launcherProcessProbe
			if err := json.Unmarshal(data, &probe); err != nil {
				t.Fatal(err)
			}
			return probe
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("launcher child did not publish its identity")
	return launcherProcessProbe{}
}

func launcherChildParent(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// comm may contain spaces and parentheses; fields after its final ')' are
	// state, ppid, and the remaining numeric /proc stat fields.
	closing := strings.LastIndexByte(string(data), ')')
	if closing < 0 {
		return 0, errors.New("process stat has no command boundary")
	}
	fields := strings.Fields(string(data[closing+1:]))
	if len(fields) < 2 {
		return 0, errors.New("process stat has no parent PID")
	}
	return strconv.Atoi(fields[1])
}

func registerLauncherChildCleanup(t *testing.T, pid int) {
	t.Helper()
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unix.PidfdSendSignal(fd, syscall.SIGKILL, nil, 0)
		_ = unix.Close(fd)
		// Only failure cleanup may race the starter's per-child Wait. Never
		// consume another component's children with waitpid(-1).
		_, _ = syscall.Wait4(pid, nil, 0, nil)
	})
}

func waitForLauncherChildReaped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	status, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	t.Fatalf("owned child %d was not reaped while parent %d remained alive:\n%s", pid, os.Getpid(), status)
}
