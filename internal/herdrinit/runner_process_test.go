package herdrinit

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

	"golang.org/x/sys/unix"
)

const initializationProcessHelperMode = "SWAY_SESSION_INITIALIZATION_PROCESS_TEST_HELPER"

type initializationProcessProbe struct {
	Parent     int
	Descendant int
}

type initializationInvocationProbe struct {
	Arguments  []string
	Directory  string
	ConfigFile string
	Unsafe     []string
	Retained   string
}

func TestExecRunnerProcess(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{
		"cancel", "deadline", "successful-parent-held-pipes", "canceled-before-start",
		"success", "failed-exit", "stdout-limit", "stdout-overflow", "stderr-overflow", "trusted-invocation",
	} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable, "-test.run=^TestExecRunnerProcessHelper$")
			command.Env = append(os.Environ(),
				initializationProcessHelperMode+"=supervisor",
				"SWAY_SESSION_INITIALIZATION_PROCESS_TEST_SCENARIO="+scenario,
				"GORACE=atexit_sleep_ms=0",
			)
			command.WaitDelay = time.Second
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("isolated initialization scenario: %v\n%s", err, output)
			}
		})
	}
}

func TestExecRunnerProcessHelper(t *testing.T) {
	mode := os.Getenv(initializationProcessHelperMode)
	if mode == "" {
		return
	}
	directory := os.Getenv("SWAY_SESSION_INITIALIZATION_PROCESS_TEST_DIRECTORY")
	scenario := os.Getenv("SWAY_SESSION_INITIALIZATION_PROCESS_TEST_SCENARIO")
	switch mode {
	case "descendant":
		writeInitializationProcessProbe(t, filepath.Join(directory, "descendant-ready"), nil)
		// Keep both inherited streams open until exact-PID supervisor cleanup.
		// A finite fallback also bounds a broken fixture.
		time.Sleep(10 * time.Second)
		os.Exit(0)
	case "runner":
		runInitializationProcessHelper(t, scenario, directory)
	case "supervisor":
		// Only this private process adopts its deliberately orphaned child.
		// The package test process does not change child-reaping behavior.
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		testInitializationProcessScenario(t, scenario)
	default:
		t.Fatalf("unknown initialization process helper %q", mode)
	}
}

func runInitializationProcessHelper(t *testing.T, scenario, directory string) {
	t.Helper()
	switch scenario {
	case "success", "failed-exit":
		fmt.Fprintln(os.Stdout, "parent stdout")
		fmt.Fprintln(os.Stderr, "parent stderr")
		if scenario == "failed-exit" {
			os.Exit(7)
		}
		os.Exit(0)
	case "stdout-limit", "stdout-overflow":
		size := outputLimit
		if scenario == "stdout-overflow" {
			size += 128
		}
		if _, err := fmt.Fprint(os.Stdout, strings.Repeat("x", size)); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	case "stderr-overflow":
		fmt.Fprint(os.Stderr, strings.Repeat("x", outputLimit+128)+"discarded tail")
		os.Exit(23)
	case "trusted-invocation":
		separator := -1
		for index, argument := range os.Args {
			if argument == "--" {
				separator = index
				break
			}
		}
		if separator < 0 {
			t.Fatal("initialization arguments were not passed through the wrapper")
		}
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		probe := initializationInvocationProbe{
			Arguments: os.Args[separator+1:], Directory: cwd,
			ConfigFile: os.Getenv("HERDR_CONFIG_PATH"),
			Retained:   os.Getenv("SWAY_SESSION_INITIALIZATION_PROCESS_TEST_RETAINED"),
		}
		for _, value := range os.Environ() {
			name, _, _ := strings.Cut(value, "=")
			if name == "CODEX_THREAD_ID" || strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "HERDR_") && name != "HERDR_CONFIG_PATH" {
				probe.Unsafe = append(probe.Unsafe, name)
			}
		}
		if err := json.NewEncoder(os.Stdout).Encode(probe); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	case "cancel", "deadline", "successful-parent-held-pipes":
	default:
		t.Fatalf("unknown initialization scenario %q", scenario)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestExecRunnerProcessHelper$")
	child.Env = append(os.Environ(), initializationProcessHelperMode+"=descendant")
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	// Publish ownership before readiness, and clean up directly if publication
	// fails before the supervisor can identify this child.
	probe := initializationProcessProbe{Parent: os.Getpid(), Descendant: child.Process.Pid}
	if err := publishInitializationProcessProbe(filepath.Join(directory, "processes.json"), probe); err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatal(err)
	}
	readInitializationProcessProbe(t, filepath.Join(directory, "descendant-ready"), nil)
	fmt.Fprintln(os.Stdout, "parent stdout")
	fmt.Fprintln(os.Stderr, "parent stderr")
	writeInitializationProcessProbe(t, filepath.Join(directory, "parent-ready"), nil)
	if scenario == "successful-parent-held-pipes" {
		os.Exit(0)
	}
	// CommandContext must kill the direct parent without killing the child.
	time.Sleep(10 * time.Second)
	os.Exit(0)
}

func testInitializationProcessScenario(t *testing.T, scenario string) {
	t.Helper()
	directory := t.TempDir()
	t.Setenv(initializationProcessHelperMode, "runner")
	t.Setenv("SWAY_SESSION_INITIALIZATION_PROCESS_TEST_DIRECTORY", directory)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(directory, "herdr")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestExecRunnerProcessHelper$ -- \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := ExecRunner{Executable: wrapper, ConfigFile: filepath.Join(directory, "config.toml")}
	if scenario == "canceled-before-start" {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		output, err := runner.Run(ctx, "checkpoint-session", directory, "api", "snapshot")
		if !errors.Is(err, context.Canceled) || output != nil {
			t.Fatalf("already canceled initialization caller: output=%q err=%v", output, err)
		}
		return
	}
	if scenario == "trusted-invocation" {
		t.Setenv("HERDR_CONFIG_PATH", filepath.Join(directory, "untrusted.toml"))
		t.Setenv("HERDR_PANE_ID", "untrusted-pane")
		t.Setenv("CODEX_THREAD_ID", "untrusted-thread")
		t.Setenv("LD_PRELOAD", filepath.Join(directory, "untrusted.so"))
		t.Setenv("LD_LIBRARY_PATH", filepath.Join(directory, "untrusted-libraries"))
		t.Setenv("SWAY_SESSION_INITIALIZATION_PROCESS_TEST_RETAINED", "retained")
		output, err := runner.Run(t.Context(), "checkpoint-session", directory, "api", "snapshot")
		if err != nil {
			t.Fatal(err)
		}
		var probe initializationInvocationProbe
		if err := json.Unmarshal(output, &probe); err != nil {
			t.Fatal(err)
		}
		want := []string{"--session", "checkpoint-session", "api", "snapshot"}
		if !reflect.DeepEqual(probe.Arguments, want) || probe.Directory != directory || probe.ConfigFile != runner.ConfigFile || len(probe.Unsafe) != 0 || probe.Retained != "retained" {
			t.Fatalf("initialization invocation changed: %+v", probe)
		}
		return
	}
	if scenario == "success" || scenario == "failed-exit" || scenario == "stdout-limit" || scenario == "stdout-overflow" || scenario == "stderr-overflow" {
		output, err := runner.Run(t.Context(), "checkpoint-session", directory, "api", "snapshot")
		switch scenario {
		case "success":
			if err != nil || string(output) != "parent stdout\n" {
				t.Fatalf("normal initialization output: output=%q err=%v", output, err)
			}
		case "failed-exit", "stderr-overflow":
			var exit *exec.ExitError
			code, detail := 7, "parent stderr"
			if scenario == "stderr-overflow" {
				code, detail = 23, strings.Repeat("x", 64*1024)
			}
			if !errors.As(err, &exit) || exit.ExitCode() != code || output != nil {
				t.Fatalf("initialization lost its failed exit: output length=%d err=%v", len(output), err)
			}
			if want := detail + ": " + exit.Error(); err.Error() != want {
				t.Fatalf("initialization stderr wrapping or limit changed: error length=%d want=%d", len(err.Error()), len(want))
			}
		case "stdout-limit":
			if err != nil || string(output) != strings.Repeat("x", 64*1024) {
				t.Fatalf("initialization rejected output at its limit: output length=%d err=%v", len(output), err)
			}
		case "stdout-overflow":
			if err == nil || err.Error() != "herdr output exceeds 65536 bytes" || output != nil {
				t.Fatalf("initialization stdout limit changed: output length=%d err=%v", len(output), err)
			}
		}
		return
	}
	ctx, cancel := context.WithCancel(t.Context())
	if scenario == "deadline" {
		cancel()
		ctx, cancel = context.WithTimeout(t.Context(), 3*time.Second)
	}
	defer cancel()
	type result struct {
		output []byte
		err    error
	}
	results := make(chan result, 1)
	go func() {
		output, err := runner.Run(ctx, "checkpoint-session", directory, "api", "snapshot")
		results <- result{output: output, err: err}
	}()
	var probe initializationProcessProbe
	readInitializationProcessProbe(t, filepath.Join(directory, "processes.json"), &probe)
	if probe.Parent <= 0 || probe.Descendant <= 0 || probe.Parent == probe.Descendant {
		t.Fatalf("invalid process ownership: %+v", probe)
	}
	fd, err := unix.PidfdOpen(probe.Descendant, 0)
	if err != nil {
		t.Fatal(err)
	}
	finished, reaped := false, false
	t.Cleanup(func() {
		cancel()
		// Pin signals to this exact owned child, never its process group.
		_ = unix.PidfdSendSignal(fd, syscall.SIGKILL, nil, 0)
		defer unix.Close(fd)
		if !finished {
			select {
			case <-results:
			case <-time.After(2 * time.Second):
				t.Error("initialization runner did not finish after owned child cleanup")
			}
		}
		if !reaped {
			if waited, err := syscall.Wait4(probe.Descendant, nil, 0, nil); err != nil || waited != probe.Descendant {
				t.Errorf("reap owned initialization child: pid=%d err=%v", waited, err)
			}
		}
	})
	readInitializationProcessProbe(t, filepath.Join(directory, "parent-ready"), nil)
	if scenario == "cancel" {
		cancel()
	} else if scenario == "deadline" {
		<-ctx.Done()
	}
	select {
	case completed := <-results:
		finished = true
		if scenario == "successful-parent-held-pipes" {
			if !errors.Is(completed.err, exec.ErrWaitDelay) {
				t.Fatalf("successful parent with inherited pipes lost ErrWaitDelay: %v", completed.err)
			}
		} else {
			var exit *exec.ExitError
			if !errors.As(completed.err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("canceled initialization lost its direct-process exit error: %v", completed.err)
			}
			cause := context.Canceled
			if scenario == "deadline" {
				cause = context.DeadlineExceeded
			}
			if !errors.Is(ctx.Err(), cause) {
				t.Fatalf("initialization caller cause changed: got=%v want=%v", ctx.Err(), cause)
			}
		}
		if completed.output != nil || !strings.HasPrefix(completed.err.Error(), "parent stderr: ") {
			t.Fatalf("initialization error wrapping changed: output=%q err=%v", completed.output, completed.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("initialization runner still waited for inherited stdout/stderr after %s", scenario)
	}
	// The drain budget must return without terminating the persistent child.
	var status syscall.WaitStatus
	waited, waitErr := syscall.Wait4(probe.Descendant, &status, syscall.WNOHANG, nil)
	reaped = waited == probe.Descendant
	if waitErr != nil || waited != 0 {
		t.Fatalf("initialization runner terminated its descendant: pid=%d status=%v err=%v", waited, status, waitErr)
	}
}

func writeInitializationProcessProbe(t *testing.T, path string, value any) {
	t.Helper()
	if err := publishInitializationProcessProbe(path, value); err != nil {
		t.Fatal(err)
	}
}

func publishInitializationProcessProbe(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func readInitializationProcessProbe(t *testing.T, path string, value any) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
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
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("initialization helper did not publish %s", filepath.Base(path))
}
