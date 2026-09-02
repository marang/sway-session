package session

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRegistryMigrationRefusesIncompatibleRunningDaemon(t *testing.T) {
	root, lock := prepareLegacyRegistryWithDaemonLock(t)
	defer lock.Close()

	var registry Registry
	err := RegistryFile(root).LoadInto(&registry)
	if err == nil || !strings.Contains(err.Error(), "incompatible sway-session daemon") {
		t.Fatalf("migration was not blocked by legacy daemon: %v", err)
	}
	data, readErr := os.ReadFile(filepath.Join(root, ContextsFilename))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(data), `"version":2`) {
		t.Fatalf("blocked migration modified the registry: %s", data)
	}
	if _, statErr := os.Lstat(filepath.Join(root, ContextsV2BackupFilename)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("blocked migration created rollback state: %v", statErr)
	}
}

func TestRegistryMigrationAllowsCompatibleRunningDaemon(t *testing.T) {
	root, lock := prepareLegacyRegistryWithDaemonLock(t)
	defer lock.Close()
	if err := MarkDaemonRegistryCompatibility(lock); err != nil {
		t.Fatal(err)
	}

	var registry Registry
	if err := RegistryFile(root).LoadInto(&registry); err != nil {
		t.Fatalf("compatible daemon blocked migration: %v", err)
	}
	if registry.Version != ContextsSchemaVersion {
		t.Fatalf("registry was not migrated: %+v", registry)
	}
}

func TestRegistryMigrationRefusesForeignCompatibleDaemonWithoutLease(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	root, err := DefaultStateRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"version":2,"preferences":{"desktop_indicators":false},"contexts":[]}`)
	if err := os.WriteFile(filepath.Join(root, ContextsFilename), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(runtimeRoot, "daemon-ready")
	release := filepath.Join(runtimeRoot, "daemon-release")
	command := exec.Command(os.Args[0], "-test.run=^TestDaemonCompatibilityProcessHelper$")
	command.Env = append(os.Environ(),
		"DAEMON_COMPAT_HELPER=1",
		"DAEMON_COMPAT_RUNTIME_ROOT="+runtimeRoot,
		"DAEMON_COMPAT_READY="+ready,
		"DAEMON_COMPAT_RELEASE="+release,
	)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	waitForDaemonCompatPath(t, ready)

	var registry Registry
	err = RegistryFile(root).LoadInto(&registry)
	if err == nil || !strings.Contains(err.Error(), "separate running daemon") {
		t.Fatalf("foreign compatible daemon did not retain migration exclusion: %v", err)
	}
	data, readErr := os.ReadFile(filepath.Join(root, ContextsFilename))
	if readErr != nil || !bytes.Equal(data, legacy) {
		t.Fatalf("blocked foreign-daemon migration modified registry: data=%q err=%v", data, readErr)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("daemon helper: %v\n%s", err, output.String())
	}
}

func TestDaemonCompatibilityProcessHelper(t *testing.T) {
	if os.Getenv("DAEMON_COMPAT_HELPER") != "1" {
		return
	}
	runtimeRoot := os.Getenv("DAEMON_COMPAT_RUNTIME_ROOT")
	directory := filepath.Join(runtimeRoot, daemonLockDirectory)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(directory, daemonLockFilename), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN) //nolint:errcheck // subprocess is exiting
	if err := MarkDaemonRegistryCompatibility(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("DAEMON_COMPAT_READY"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(os.Getenv("DAEMON_COMPAT_RELEASE")); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for daemon compatibility helper release")
}

func waitForDaemonCompatPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func prepareLegacyRegistryWithDaemonLock(t *testing.T) (string, *os.File) {
	t.Helper()
	stateHome := filepath.Join(t.TempDir(), "state")
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	root, err := DefaultStateRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"version":2,"preferences":{"desktop_indicators":false},"contexts":[]}`)
	if err := os.WriteFile(filepath.Join(root, ContextsFilename), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	lockDirectory := filepath.Join(runtimeRoot, daemonLockDirectory)
	if err := os.MkdirAll(lockDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(lockDirectory, daemonLockFilename), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	})
	return root, lock
}
