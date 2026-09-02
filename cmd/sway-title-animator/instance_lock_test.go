package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestInstanceLockExcludesSecondProcessInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	first, err := acquireInstanceLock(path, false)
	if err != nil {
		t.Fatalf("acquire first instance lock: %v", err)
	}
	t.Cleanup(func() {
		_ = first.Close()
	})

	if _, err := acquireInstanceLock(path, false); !errors.Is(err, errInstanceRunning) {
		t.Fatalf("expected a second instance to be rejected, got %v", err)
	}

	record, err := readInstanceRecord(first.file)
	if err != nil {
		t.Fatalf("read instance record: %v", err)
	}
	if record.PID != os.Getpid() || record.StartTime == 0 || record.Executable == "" {
		t.Fatalf("expected complete process identity, got %+v", record)
	}
}

func TestProcessStartTimeIsStable(t *testing.T) {
	first, err := processStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("read first process start time: %v", err)
	}
	second, err := processStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("read second process start time: %v", err)
	}
	if first == 0 || first != second {
		t.Fatalf("expected a stable nonzero process start time, got %d and %d", first, second)
	}
}

func TestInstanceLockReplaceWaitsForVerifiedOwner(t *testing.T) {
	if os.Getenv("SWAY_TITLE_ANIMATOR_LOCK_HELPER") == "1" {
		path := os.Getenv("SWAY_TITLE_ANIMATOR_LOCK_PATH")
		lock, err := acquireInstanceLock(path, false)
		if err != nil {
			t.Fatalf("helper acquire lock: %v", err)
		}
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		fmt.Println("LOCK_READY")
		<-signals
		signal.Stop(signals)
		if err := lock.Close(); err != nil {
			t.Fatalf("helper release lock: %v", err)
		}
		return
	}

	path := filepath.Join(t.TempDir(), "daemon.lock")
	command := exec.Command(os.Args[0], "-test.run=^TestInstanceLockReplaceWaitsForVerifiedOwner$")
	command.Env = append(os.Environ(),
		"SWAY_TITLE_ANIMATOR_LOCK_HELPER=1",
		"SWAY_TITLE_ANIMATOR_LOCK_PATH="+path,
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout: %v", err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start lock helper: %v", err)
	}
	helperDone := false
	t.Cleanup(func() {
		if helperDone {
			return
		}
		_ = command.Process.Kill()
		_ = command.Wait()
	})

	scanner := bufio.NewScanner(stdout)
	ready := make(chan bool, 1)
	go func() {
		for scanner.Scan() {
			if scanner.Text() == "LOCK_READY" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("lock helper exited before becoming ready")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for lock helper")
	}

	replacement, err := acquireInstanceLock(path, true)
	if err != nil {
		t.Fatalf("replace verified lock owner: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatalf("release replacement lock: %v", err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("lock helper shutdown: %v", err)
	}
	helperDone = true
}

func TestInstanceLockReplaceNeverSignalsLegacyPIDOnlyOwner(t *testing.T) {
	if os.Getenv("SWAY_TITLE_ANIMATOR_LEGACY_LOCK_HELPER") == "1" {
		path := os.Getenv("SWAY_TITLE_ANIMATOR_LOCK_PATH")
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatalf("helper open lock: %v", err)
		}
		defer file.Close()
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatalf("helper lock: %v", err)
		}
		if err := file.Truncate(0); err != nil {
			t.Fatalf("helper truncate lock: %v", err)
		}
		if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
			t.Fatalf("helper write legacy PID: %v", err)
		}
		if err := file.Sync(); err != nil {
			t.Fatalf("helper sync legacy PID: %v", err)
		}
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		defer signal.Stop(signals)
		fmt.Println("LOCK_READY")
		<-signals
		return
	}

	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "daemon.lock")
	helperPath := filepath.Join(tempDir, "sway-title-animator")
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	testBinary, err := os.ReadFile(testExecutable)
	if err != nil {
		t.Fatalf("read test executable: %v", err)
	}
	if err := os.WriteFile(helperPath, testBinary, 0o700); err != nil {
		t.Fatalf("copy named test executable: %v", err)
	}

	command := exec.Command(helperPath, "-test.run=^TestInstanceLockReplaceNeverSignalsLegacyPIDOnlyOwner$")
	command.Env = append(os.Environ(),
		"SWAY_TITLE_ANIMATOR_LEGACY_LOCK_HELPER=1",
		"SWAY_TITLE_ANIMATOR_LOCK_PATH="+path,
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout: %v", err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start legacy lock helper: %v", err)
	}
	helperDone := false
	t.Cleanup(func() {
		if helperDone {
			return
		}
		_ = command.Process.Kill()
		_ = command.Wait()
	})

	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "LOCK_READY" {
		t.Fatal("legacy lock helper exited before becoming ready")
	}

	replacement, err := acquireInstanceLock(path, true)
	if err == nil {
		_ = replacement.Close()
		t.Fatal("replace accepted a held legacy PID-only record")
	}
	if err := syscall.Kill(command.Process.Pid, 0); err != nil {
		t.Fatalf("legacy lock owner was signaled: %v", err)
	}

	if err := command.Process.Kill(); err != nil {
		t.Fatalf("stop legacy lock helper: %v", err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("expected killed legacy lock helper to exit unsuccessfully")
	}
	helperDone = true
}

func TestInstanceLockOverwritesUnlockedLegacyPIDOnlyRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireInstanceLock(path, false)
	if err != nil {
		t.Fatalf("acquire unlocked legacy record: %v", err)
	}
	defer lock.Close()
	record, err := readInstanceRecord(lock.file)
	if err != nil {
		t.Fatal(err)
	}
	if record.PID != os.Getpid() || record.StartTime == 0 || record.Executable == "" {
		t.Fatalf("legacy record was not upgraded: %+v", record)
	}
}
