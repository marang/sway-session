package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

func TestRegistryCreationRefusesSchemaV4DaemonBeforeMutation(t *testing.T) {
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
	lockDirectory := filepath.Join(runtimeRoot, daemonLockDirectory)
	if err := os.MkdirAll(lockDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(lockDirectory, daemonLockFilename), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN) //nolint:errcheck // test cleanup
	start, err := processStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	marker, err := json.Marshal(daemonCompatibilityMarker{
		Version: daemonCompatibilityMarkerVersion, PID: os.Getpid(), ProcessStart: start, ContextsSchema: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	marker = append(marker, '\n')
	if err := lock.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := lock.WriteAt(marker, 0); err != nil {
		t.Fatal(err)
	}
	if err := lock.Sync(); err != nil {
		t.Fatal(err)
	}

	mutated := false
	_, err = UpdateRegistry(root, func(*Registry) error {
		mutated = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "incompatible sway-session daemon") {
		t.Fatalf("schema-v4 daemon did not block registry creation: %v", err)
	}
	if mutated {
		t.Fatal("registry mutation ran while schema-v4 daemon held the compatibility lock")
	}
	if _, err := os.Lstat(filepath.Join(root, ContextsFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked registry creation wrote state: %v", err)
	}
}

func TestConcurrentFirstRegistryCreationWaitsAndPreservesBothMutations(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	root, err := DefaultStateRoot()
	if err != nil {
		t.Fatal(err)
	}

	first := validRegistry().Contexts[0]
	second := first
	second.ID = ContextID("223e4567-e89b-42d3-a456-426614174000")
	second.Launcher.Session = "lab-108-second"
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, updateErr := UpdateRegistry(root, func(registry *Registry) error {
			close(firstEntered)
			<-releaseFirst
			return AddContext(registry, first)
		})
		firstDone <- updateErr
	}()
	<-firstEntered

	secondDone := make(chan error, 1)
	go func() {
		_, updateErr := UpdateRegistry(root, func(registry *Registry) error {
			return AddContext(registry, second)
		})
		secondDone <- updateErr
	}()
	select {
	case secondErr := <-secondDone:
		close(releaseFirst)
		<-firstDone
		t.Fatalf("concurrent first-use writer returned before the creator committed: %v", secondErr)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first registry creation failed: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second registry creation failed: %v", err)
	}
	var registry Registry
	if err := RegistryFile(root).LoadInto(&registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Contexts) != 2 {
		t.Fatalf("concurrent first-use mutation was lost: %+v", registry.Contexts)
	}
}

func TestDefaultRootConcurrentV4MigrationSerializesAllReaders(t *testing.T) {
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
	legacy := []byte(`{"version":4,"preferences":{"desktop_indicators":false},"contexts":[]}`)
	if err := os.WriteFile(filepath.Join(root, ContextsFilename), legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	releaseMigration, err := acquireRegistryMigrationLock(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	const readers = 8
	start := make(chan struct{})
	results := make(chan error, readers)
	var workers sync.WaitGroup
	workers.Add(readers)
	for range readers {
		go func() {
			defer workers.Done()
			<-start
			var registry Registry
			results <- RegistryFile(root).LoadInto(&registry)
		}()
	}
	close(start)
	select {
	case early := <-results:
		releaseMigration()
		workers.Wait()
		t.Fatalf("v4 migration bypassed its serialization lock: %v", early)
	case <-time.After(50 * time.Millisecond):
	}
	releaseMigration()
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent v4 migration failed: %v", err)
		}
	}
	backup, err := os.ReadFile(filepath.Join(root, ContextsV4BackupFilename))
	if err != nil || !bytes.Equal(backup, legacy) {
		t.Fatalf("concurrent v4 migration lost rollback evidence: data=%q err=%v", backup, err)
	}
}

func TestWaitingV4ReaderReobservesCurrentStateBeforeInspectingDaemon(t *testing.T) {
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
	registryPath := filepath.Join(root, ContextsFilename)
	if err := os.WriteFile(registryPath, []byte(`{"version":4,"preferences":{"desktop_indicators":false},"contexts":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	releaseMigration, err := acquireRegistryMigrationLock(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	migrationReleased := false
	t.Cleanup(func() {
		if !migrationReleased {
			releaseMigration()
		}
	})
	type loadResult struct {
		registry Registry
		err      error
	}
	loaded := make(chan loadResult, 1)
	go func() {
		var registry Registry
		loadErr := RegistryFile(root).LoadInto(&registry)
		loaded <- loadResult{registry: registry, err: loadErr}
	}()
	select {
	case result := <-loaded:
		t.Fatalf("legacy reader bypassed migration serialization: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	current := []byte(`{"version":5,"preferences":{"desktop_indicators":false},"contexts":[]}`)
	if err := os.WriteFile(registryPath, current, 0o600); err != nil {
		t.Fatal(err)
	}

	ready := filepath.Join(runtimeRoot, "reobserve-daemon-ready")
	releaseDaemon := filepath.Join(runtimeRoot, "reobserve-daemon-release")
	command := exec.Command(os.Args[0], "-test.run=^TestDaemonCompatibilityProcessHelper$")
	command.Env = append(os.Environ(),
		"DAEMON_COMPAT_HELPER=1",
		"DAEMON_COMPAT_RUNTIME_ROOT="+runtimeRoot,
		"DAEMON_COMPAT_READY="+ready,
		"DAEMON_COMPAT_RELEASE="+releaseDaemon,
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
	releaseMigration()
	migrationReleased = true

	select {
	case result := <-loaded:
		if result.err != nil || result.registry.Version != ContextsSchemaVersion {
			t.Fatalf("waiting reader treated current state as an unsafe migration: registry=%+v err=%v", result.registry, result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waiting reader did not finish after migration serialization was released")
	}
	if err := os.WriteFile(releaseDaemon, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("daemon helper: %v\n%s", err, output.String())
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
