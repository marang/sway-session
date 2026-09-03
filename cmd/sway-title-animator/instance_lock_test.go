package main

import (
	"bufio"
	"encoding/json"
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
	if record.Version != instanceRecordVersion || record.PID != os.Getpid() || record.StartTime == 0 || record.Executable == "" || record.ExecutableIdentity == nil {
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

func TestProcessExecutableMatchesProcfsDeletedSuffixExactly(t *testing.T) {
	tests := []struct {
		name                  string
		observed              string
		recorded              string
		deletedMarkerVerified bool
		wantMatch             bool
	}{
		{name: "exact path", observed: "/usr/bin/sway-title-animator", recorded: "sway-title-animator", wantMatch: true},
		{name: "deleted executable", observed: "/usr/bin/sway-title-animator (deleted)", recorded: "sway-title-animator", deletedMarkerVerified: true, wantMatch: true},
		{name: "live literal deleted suffix", observed: "/usr/bin/sway-title-animator (deleted)", recorded: "sway-title-animator", wantMatch: false},
		{name: "exact live literal deleted suffix", observed: "/usr/bin/sway-title-animator (deleted)", recorded: "sway-title-animator (deleted)", wantMatch: true},
		{name: "different executable deleted", observed: "/usr/bin/not-the-animator (deleted)", recorded: "sway-title-animator", deletedMarkerVerified: true, wantMatch: false},
		{name: "arbitrary suffix", observed: "/usr/bin/sway-title-animator.old", recorded: "sway-title-animator", wantMatch: false},
		{name: "deleted marker not final", observed: "/usr/bin/sway-title-animator (deleted).old", recorded: "sway-title-animator", wantMatch: false},
		{name: "repeated deleted marker", observed: "/usr/bin/sway-title-animator (deleted) (deleted)", recorded: "sway-title-animator", deletedMarkerVerified: true, wantMatch: false},
		{name: "deleted literal-suffix executable", observed: "/usr/bin/sway-title-animator (deleted) (deleted)", recorded: "sway-title-animator (deleted)", deletedMarkerVerified: true, wantMatch: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := processExecutableMatches(test.observed, test.recorded, test.deletedMarkerVerified); got != test.wantMatch {
				t.Fatalf("processExecutableMatches(%q, %q, %t)=%t, want %t", test.observed, test.recorded, test.deletedMarkerVerified, got, test.wantMatch)
			}
		})
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

func TestInstanceLockReplaceAcceptsUpgradedDeletedExecutable(t *testing.T) {
	if os.Getenv("SWAY_TITLE_ANIMATOR_DELETED_LOCK_HELPER") == "1" {
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

	tests := []struct {
		name           string
		legacyRecord   bool
		retainHardlink bool
		expectedLinks  uint64
	}{
		{name: "legacy record with unlinked inode", legacyRecord: true},
		{name: "versioned record with surviving hardlink", retainHardlink: true, expectedLinks: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
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
			if test.retainHardlink {
				if err := os.Link(helperPath, helperPath+".survivor"); err != nil {
					t.Fatalf("retain helper hardlink: %v", err)
				}
			}

			command := exec.Command(helperPath, "-test.run=^TestInstanceLockReplaceAcceptsUpgradedDeletedExecutable$")
			command.Env = append(os.Environ(),
				"SWAY_TITLE_ANIMATOR_DELETED_LOCK_HELPER=1",
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

			ready := make(chan bool, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				ready <- scanner.Scan() && scanner.Text() == "LOCK_READY"
			}()
			select {
			case ok := <-ready:
				if !ok {
					t.Fatal("lock helper exited before becoming ready")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for lock helper")
			}
			if test.legacyRecord {
				rewriteInstanceRecord(t, path, func(record *instanceRecord) {
					record.Version = 0
					record.ExecutableIdentity = nil
				})
			}
			if err := os.Remove(helperPath); err != nil {
				t.Fatalf("remove upgraded helper executable: %v", err)
			}
			executableLink := filepath.Join("/proc", fmt.Sprint(command.Process.Pid), "exe")
			procExecutable, err := os.Readlink(executableLink)
			if err != nil {
				t.Fatalf("read deleted helper executable: %v", err)
			}
			if procExecutable != helperPath+procfsDeletedExecutableSuffix {
				t.Fatalf("unexpected deleted executable target %q", procExecutable)
			}
			info, err := os.Stat(executableLink)
			if err != nil {
				t.Fatalf("stat deleted helper executable: %v", err)
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || uint64(stat.Nlink) != test.expectedLinks {
				t.Fatalf("deleted helper link count=%v, want %d", stat, test.expectedLinks)
			}

			replacement, err := acquireInstanceLock(path, true)
			if err != nil {
				t.Fatalf("replace upgraded lock owner: %v", err)
			}
			if err := replacement.Close(); err != nil {
				t.Fatalf("release replacement lock: %v", err)
			}
			if err := command.Wait(); err != nil {
				t.Fatalf("upgraded lock helper shutdown: %v", err)
			}
			helperDone = true
		})
	}
}

func TestInstanceLockReplaceRejectsMismatchedOwnerMetadata(t *testing.T) {
	if os.Getenv("SWAY_TITLE_ANIMATOR_MISMATCHED_LOCK_HELPER") == "1" {
		path := os.Getenv("SWAY_TITLE_ANIMATOR_LOCK_PATH")
		lock, err := acquireInstanceLock(path, false)
		if err != nil {
			t.Fatalf("helper acquire lock: %v", err)
		}
		defer lock.Close()
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		defer signal.Stop(signals)
		fmt.Println("LOCK_READY")
		<-signals
		return
	}

	tests := []struct {
		name              string
		literalExecutable bool
		mutate            func(*instanceRecord)
	}{
		{name: "stale start time", mutate: func(record *instanceRecord) { record.StartTime++ }},
		{name: "mismatched executable", mutate: func(record *instanceRecord) { record.Executable = "not-sway-title-animator" }},
		{name: "mismatched executable inode", mutate: func(record *instanceRecord) { record.ExecutableIdentity.Inode++ }},
		{name: "missing versioned executable identity", mutate: func(record *instanceRecord) { record.ExecutableIdentity = nil }},
		{name: "live literal deleted suffix", literalExecutable: true, mutate: func(record *instanceRecord) {
			record.Version = 0
			record.Executable = "sway-title-animator"
			record.ExecutableIdentity = nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tempDir := t.TempDir()
			path := filepath.Join(tempDir, "daemon.lock")
			helperExecutable := os.Args[0]
			if test.literalExecutable {
				helperExecutable = filepath.Join(tempDir, "sway-title-animator (deleted)")
				testBinary, err := os.ReadFile(os.Args[0])
				if err != nil {
					t.Fatalf("read test executable: %v", err)
				}
				if err := os.WriteFile(helperExecutable, testBinary, 0o700); err != nil {
					t.Fatalf("copy literal-suffix test executable: %v", err)
				}
			}
			command := exec.Command(helperExecutable, "-test.run=^TestInstanceLockReplaceRejectsMismatchedOwnerMetadata$")
			command.Env = append(os.Environ(),
				"SWAY_TITLE_ANIMATOR_MISMATCHED_LOCK_HELPER=1",
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

			ready := make(chan bool, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				ready <- scanner.Scan() && scanner.Text() == "LOCK_READY"
			}()
			select {
			case ok := <-ready:
				if !ok {
					t.Fatal("lock helper exited before becoming ready")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for lock helper")
			}

			rewriteInstanceRecord(t, path, test.mutate)
			if test.literalExecutable {
				renamed := helperExecutable + ".renamed"
				if err := os.Rename(helperExecutable, renamed); err != nil {
					t.Fatalf("rename literal-suffix executable away: %v", err)
				}
				if err := os.Rename(renamed, helperExecutable); err != nil {
					t.Fatalf("rename literal-suffix executable back: %v", err)
				}
			}

			replacement, err := acquireInstanceLock(path, true)
			if err == nil {
				_ = replacement.Close()
				t.Fatal("replace accepted mismatched lock owner metadata")
			}
			if err := syscall.Kill(command.Process.Pid, 0); err != nil {
				t.Fatalf("mismatched lock owner was signaled: %v", err)
			}
			if err := command.Process.Kill(); err != nil {
				t.Fatalf("stop mismatched lock helper: %v", err)
			}
			if err := command.Wait(); err == nil {
				t.Fatal("expected killed mismatched lock helper to exit unsuccessfully")
			}
			helperDone = true
		})
	}
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
	if record.Version != instanceRecordVersion || record.PID != os.Getpid() || record.StartTime == 0 || record.Executable == "" || record.ExecutableIdentity == nil {
		t.Fatalf("legacy record was not upgraded: %+v", record)
	}
}

func rewriteInstanceRecord(t *testing.T, path string, mutate func(*instanceRecord)) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	record, err := readInstanceRecord(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	mutate(&record)
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
