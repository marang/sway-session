package session

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marang/sway-title-animator/internal/statefile"
	"golang.org/x/sys/unix"
)

func TestDefaultStateRootUsesAbsoluteXDGStateHome(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)

	root, err := DefaultStateRoot()
	if err != nil {
		t.Fatalf("resolve state root: %v", err)
	}
	if root != filepath.Join(stateHome, "sway-session") {
		t.Fatalf("unexpected state root %q", root)
	}
}

func TestRegistryMutationBoundsDirectoryLockAcquisition(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := RegistryStoreFor(root).Save(emptyRegistry()); err != nil {
		t.Fatal(err)
	}
	directory, err := statefile.OpenPrivateDirectory(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := unix.Flock(int(directory.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(directory.Fd()), unix.LOCK_UN) //nolint:errcheck -- best-effort test cleanup

	started := time.Now()
	_, err = UpdateRegistryContext(context.Background(), root, func(*Registry) error { return nil })
	if !IsStateDatabaseBusy(err) {
		t.Fatalf("blocked registry mutation returned %v, want retryable busy error", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("registry lock acquisition was not bounded: %v", elapsed)
	}
}

func TestTerminalContextAndCreationActivityCommitAtomically(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := initializeStateDatabase(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`CREATE TRIGGER reject_terminal_activity BEFORE INSERT ON terminal_activity BEGIN SELECT RAISE(ABORT, 'injected activity failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	created := validRegistry().Contexts[0]
	_, err = UpdateRegistryWithTerminalCreationContext(t.Context(), root, func(registry *Registry) error {
		return AddContext(registry, created)
	}, func() (ContextID, time.Time, bool) {
		return created.ID, time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC), true
	})
	if err == nil {
		t.Fatal("injected terminal activity failure was accepted")
	}
	registry, err := ReadRegistrySnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Contexts) != 0 {
		t.Fatalf("failed activity insert committed registry context: %+v", registry)
	}
}

func TestDefaultStateRootRejectsRelativeXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative/state")
	if _, err := DefaultStateRoot(); err == nil {
		t.Fatal("expected relative XDG_STATE_HOME to be rejected")
	}
}

func TestRegistryStoreRoundTrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	file := RegistryStoreFor(root)
	want := validRegistry()
	if err := file.Save(want); err != nil {
		t.Fatalf("save registry: %v", err)
	}

	var loaded Registry
	if err := file.LoadInto(&loaded); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if !reflect.DeepEqual(loaded, want) {
		t.Fatalf("unexpected registry: got=%+v want=%+v", loaded, want)
	}
	info, err := os.Stat(filepath.Join(root, StateDatabaseFilename))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("state database is not an owner-only regular file: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(root, legacyContextsFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registry still wrote legacy JSON: %v", err)
	}
}

func TestRegistryRevisionSnapshotSkipsUnchangedContextDecode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	store := RegistryStoreFor(root)
	if err := store.Save(validRegistry()); err != nil {
		t.Fatal(err)
	}
	registry, revision, present, changed, err := store.LoadIfChangedContext(t.Context(), -1, false)
	if err != nil || !present || !changed || len(registry.Contexts) != 1 {
		t.Fatalf("initial revision snapshot = registry:%+v revision:%d present:%t changed:%t err:%v", registry, revision, present, changed, err)
	}
	unchanged, sameRevision, present, changed, err := store.LoadIfChangedContext(t.Context(), revision, true)
	if err != nil || !present || changed || sameRevision != revision || len(unchanged.Contexts) != 0 {
		t.Fatalf("unchanged revision decoded contexts: registry:%+v revision:%d present:%t changed:%t err:%v", unchanged, sameRevision, present, changed, err)
	}
}

func TestRegistryPreferenceUpdateDoesNotRewriteUnchangedContextRows(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := RegistryStoreFor(root).Save(validRegistry()); err != nil {
		t.Fatal(err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`CREATE TRIGGER reject_context_rewrite BEFORE UPDATE ON contexts BEGIN SELECT RAISE(FAIL, 'unchanged context rewritten'); END`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	updated, err := UpdateRegistry(root, func(registry *Registry) error {
		registry.Preferences.DesktopIndicators = true
		return nil
	})
	if err != nil || !updated.Preferences.DesktopIndicators {
		t.Fatalf("preference-only update rewrote context rows: registry=%+v err=%v", updated, err)
	}
}

func TestRegistryRemovalDoesNotRewriteSurvivingContextRows(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	registry := Registry{Version: ContextsSchemaVersion, Contexts: make([]Context, 0, 32)}
	for index := range 32 {
		registry.Contexts = append(registry.Contexts, Context{
			ID:    ContextID(fmt.Sprintf("00000000-0000-4000-8000-%012x", index+1)),
			State: ContextArchived,
			Launcher: Launcher{
				Kind:     LauncherHerdr,
				Session:  fmt.Sprintf("survivor-%d", index),
				Cwd:      "/tmp",
				Terminal: &TerminalLauncher{Adapter: TerminalAdapterAlacritty},
			},
		})
	}
	if err := RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`CREATE TRIGGER reject_survivor_rewrite BEFORE UPDATE ON contexts BEGIN SELECT RAISE(FAIL, 'surviving context rewritten'); END`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	removed := registry.Contexts[0].ID
	updated, err := UpdateRegistry(root, func(registry *Registry) error {
		_, err := RemoveContext(registry, string(removed))
		return err
	})
	if err != nil {
		t.Fatalf("remove first context without rewriting survivors: %v", err)
	}
	if len(updated.Contexts) != len(registry.Contexts)-1 || updated.Contexts[0].ID != registry.Contexts[1].ID {
		t.Fatalf("registry order changed after removal: %+v", updated.Contexts)
	}
	appended := registry.Contexts[0]
	appended.ID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	appended.Launcher.Session = "appended-after-removal"
	updated, err = UpdateRegistry(root, func(registry *Registry) error {
		return AddContext(registry, appended)
	})
	if err != nil {
		t.Fatalf("append after removal rewrote survivors: %v", err)
	}
	if updated.Contexts[len(updated.Contexts)-1].ID != appended.ID {
		t.Fatalf("new context was not appended after stable survivors: %+v", updated.Contexts)
	}
}

func TestVersionedDocumentsRequireTheirTopLevelArrays(t *testing.T) {
	registry := Registry{Version: ContextsSchemaVersion}
	if err := registry.Validate(); err == nil {
		t.Fatal("expected a missing contexts array to be rejected")
	}

	snapshot := LayoutSnapshot{Version: LayoutSchemaVersion}
	if err := snapshot.Validate(); err == nil {
		t.Fatal("expected a missing workspaces array to be rejected")
	}
}

func TestUpdateRegistryCreatesAndModifiesValidState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	updated, err := UpdateRegistry(root, func(registry *Registry) error {
		registry.Contexts = append(registry.Contexts, validRegistry().Contexts[0])
		return nil
	})
	if err != nil {
		t.Fatalf("create registry transactionally: %v", err)
	}
	if !reflect.DeepEqual(updated, validRegistry()) {
		t.Fatalf("unexpected created registry: %+v", updated)
	}

	updated, err = UpdateRegistry(root, func(registry *Registry) error {
		registry.Contexts[0].State = ContextArchived
		return nil
	})
	if err != nil {
		t.Fatalf("modify registry transactionally: %v", err)
	}
	if updated.Contexts[0].State != ContextArchived {
		t.Fatalf("registry mutation was not persisted: %+v", updated)
	}
}

func TestStateDatabasePersistsMoreThanLegacyContextLimit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state with ? # % &", "sway-session")
	registry := Registry{Version: ContextsSchemaVersion, Contexts: make([]Context, 0, 5000)}
	for index := range 5000 {
		registry.Contexts = append(registry.Contexts, Context{
			ID:    ContextID(fmt.Sprintf("00000000-0000-4000-8000-%012x", index)),
			State: ContextArchived,
			Launcher: Launcher{
				Kind:     LauncherHerdr,
				Session:  fmt.Sprintf("stored-session-%d", index),
				Cwd:      "/tmp",
				Terminal: &TerminalLauncher{Adapter: TerminalAdapterAlacritty},
			},
		})
	}
	if err := RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatalf("save large registry: %v", err)
	}
	var loaded Registry
	if err := RegistryStoreFor(root).LoadInto(&loaded); err != nil {
		t.Fatalf("load large registry: %v", err)
	}
	if !reflect.DeepEqual(loaded, registry) {
		t.Fatalf("large registry changed across SQLite round trip: got %d contexts, want %d", len(loaded.Contexts), len(registry.Contexts))
	}
}

func TestLegacyJSONStateFailsClosedWithoutMutation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := []byte(`{"version":5,"preferences":{"desktop_indicators":false},"contexts":[]}`)
	path := filepath.Join(root, legacyContextsFilename)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := UpdateRegistry(root, func(*Registry) error {
		called = true
		return nil
	})
	var legacyError *LegacyStateError
	if !errors.As(err, &legacyError) {
		t.Fatalf("legacy state returned %v", err)
	}
	if called {
		t.Fatal("registry mutation ran against legacy state")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(after, contents) {
		t.Fatalf("legacy state changed: got=%q err=%v", after, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, StateDatabaseFilename)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("legacy-state rejection created a database: %v", statErr)
	}
}

func TestUninitializedDatabaseCannotHideLegacyJSONState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyContents := []byte(`{"version":5,"preferences":{"desktop_indicators":false},"contexts":[]}`)
	legacyPath := filepath.Join(root, legacyContextsFilename)
	if err := os.WriteFile(legacyPath, legacyContents, 0o600); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, StateDatabaseFilename)
	if err := os.WriteFile(databasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	called := false
	_, err := UpdateRegistry(root, func(*Registry) error {
		called = true
		return nil
	})
	var legacyError *LegacyStateError
	if !errors.As(err, &legacyError) || called {
		t.Fatalf("uninitialized database hid legacy state: called=%t err=%v", called, err)
	}
	info, statErr := os.Stat(databasePath)
	if statErr != nil || info.Size() != 0 {
		t.Fatalf("legacy rejection initialized the database: info=%v err=%v", info, statErr)
	}
	if after, readErr := os.ReadFile(legacyPath); readErr != nil || !bytes.Equal(after, legacyContents) {
		t.Fatalf("legacy source changed: data=%q err=%v", after, readErr)
	}

	result, err := MigrateLegacyState(t.Context(), root)
	if err != nil || !result.Migrated {
		t.Fatalf("explicit migration did not recover the interrupted database: result=%+v err=%v", result, err)
	}
}

func TestLegacyNestedPathProbeDoesNotLeakFileDescriptors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := os.MkdirAll(filepath.Join(root, legacyApplicationSessionDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, legacyTerminalActivityDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := statefile.OpenPrivateDirectory(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	countDescriptors := func() int {
		entries, readErr := os.ReadDir("/proc/self/fd")
		if readErr != nil {
			t.Skipf("count process file descriptors: %v", readErr)
		}
		return len(entries)
	}
	before := countDescriptors()
	for range 256 {
		if _, err := legacyPathExistsAt(directory, filepath.Join(legacyApplicationSessionDirectory, "missing.json")); err != nil {
			t.Fatal(err)
		}
	}
	after := countDescriptors()
	if after > before+2 {
		t.Fatalf("nested legacy probes leaked file descriptors: before=%d after=%d", before, after)
	}
}

func TestMigrateLegacyStateCopiesAllRuntimeDocumentsAndLeavesSourcesUntouched(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := os.MkdirAll(filepath.Join(root, legacyApplicationSessionDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, legacyTerminalActivityDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	registry := validRegistry()
	createdAt := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	staleID := ContextID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	layout := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{{
		Name: "98", RestoreMode: WorkspaceRestorePlacementOnly, PlacementContexts: []ContextID{testContextID},
	}}}
	application := ApplicationSessionState{Version: ApplicationSessionSchemaVersion, CompositorID: strings.Repeat("a", 64), Attempts: []ApplicationLaunchAttempt{{ContextID: staleID, StartedAt: createdAt}}}
	activity := TerminalActivityState{Version: TerminalActivitySchemaVersion, Terminals: []TerminalActivity{
		{ContextID: testContextID, CreatedAt: &createdAt},
		{ContextID: staleID, CreatedAt: &createdAt},
	}}
	written := map[string][]byte{}
	for path, value := range map[string]any{
		legacyContextsFilename: registry,
		legacyLayoutFilename:   layout,
		filepath.Join(legacyApplicationSessionDirectory, legacyApplicationSessionFilename): application,
		filepath.Join(legacyTerminalActivityDirectory, legacyTerminalActivityFilename):     activity,
	} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		written[path] = data
		if err := os.WriteFile(filepath.Join(root, path), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	result, err := MigrateLegacyState(t.Context(), root)
	if err != nil {
		t.Fatalf("migrate legacy state: %v", err)
	}
	if !result.Migrated || result.Contexts != 1 || !result.Layout || !result.ApplicationSession ||
		result.ApplicationAttempts != 0 || result.TerminalActivity != 1 ||
		result.SkippedApplicationAttempts != 1 || result.SkippedTerminalActivity != 1 {
		t.Fatalf("unexpected migration result: %+v", result)
	}
	loadedRegistry, err := ReadRegistrySnapshot(root)
	if err != nil || !reflect.DeepEqual(loadedRegistry, registry) {
		t.Fatalf("migrated registry = %+v, err=%v", loadedRegistry, err)
	}
	var loadedLayout LayoutSnapshot
	if err := LayoutStoreFor(root).LoadInto(&loadedLayout); err != nil || !reflect.DeepEqual(loadedLayout, layout) {
		t.Fatalf("migrated layout = %+v, err=%v", loadedLayout, err)
	}
	var loadedApplication ApplicationSessionState
	expectedApplication := application
	expectedApplication.Attempts = []ApplicationLaunchAttempt{}
	if err := ApplicationSessionStoreFor(root).LoadInto(&loadedApplication); err != nil || !reflect.DeepEqual(loadedApplication, expectedApplication) {
		t.Fatalf("migrated application state = %+v, err=%v", loadedApplication, err)
	}
	loadedActivity, err := ReadTerminalActivitySnapshot(root)
	expectedActivity := activity
	expectedActivity.Terminals = expectedActivity.Terminals[:1]
	if err != nil || !reflect.DeepEqual(loadedActivity, expectedActivity) {
		t.Fatalf("migrated activity = %+v, err=%v", loadedActivity, err)
	}
	for path, before := range written {
		after, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || !bytes.Equal(after, before) {
			t.Fatalf("legacy source %s changed: %q err=%v", path, after, err)
		}
	}

	again, err := MigrateLegacyState(t.Context(), root)
	if err != nil || again.Migrated {
		t.Fatalf("second migration was not an idempotent no-op: result=%+v err=%v", again, err)
	}
}

func TestMigrateLegacyStateReconcilesLostCommitAcknowledgement(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := os.MkdirAll(filepath.Join(root, legacyApplicationSessionDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	registry := validRegistry()
	applicationID := ContextID("22222222-2222-4222-8222-222222222222")
	applicationContext := flatpakApplicationContext("org.example.App", "org.example.App")
	applicationContext.ID, applicationContext.Label, applicationContext.Provider = applicationID, "Example", "desktop"
	registry.Contexts = append(registry.Contexts, applicationContext)
	data, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, legacyContextsFilename), data, 0o600); err != nil {
		t.Fatal(err)
	}
	offset := time.FixedZone("CEST", 2*60*60)
	application := ApplicationSessionState{
		Version: ApplicationSessionSchemaVersion, CompositorID: strings.Repeat("a", 64),
		Attempts: []ApplicationLaunchAttempt{{ContextID: applicationID, StartedAt: time.Date(2026, 9, 4, 20, 0, 0, 0, offset)}},
	}
	applicationData, err := json.Marshal(application)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, legacyApplicationSessionDirectory, legacyApplicationSessionFilename), applicationData, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	previousCommit := executeStateCommit
	executeStateCommit = func(tx *stateWriteTransaction) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		cancel()
		return errors.New("injected lost commit acknowledgement")
	}
	t.Cleanup(func() { executeStateCommit = previousCommit })

	result, err := MigrateLegacyState(ctx, root)
	if err != nil {
		t.Fatalf("reconcile migrated state: %v", err)
	}
	if !result.Migrated || !result.CommitReconciled {
		t.Fatalf("migration did not report reconciled commit: %+v", result)
	}
	loaded, err := ReadRegistrySnapshot(root)
	if err != nil || !reflect.DeepEqual(loaded, registry) {
		t.Fatalf("reconciled registry = %+v, err=%v", loaded, err)
	}
}

func TestMigrateLegacyStateRecoversAfterProcessDeathBeforeCommit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := os.MkdirAll(filepath.Join(root, legacyApplicationSessionDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, legacyTerminalActivityDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	registry := validRegistry()
	createdAt := time.Date(2026, 9, 4, 21, 0, 0, 0, time.UTC)
	layout := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{{
		Name: "98: migration crash", RestoreMode: WorkspaceRestorePlacementOnly, PlacementContexts: []ContextID{testContextID},
	}}}
	application := ApplicationSessionState{Version: ApplicationSessionSchemaVersion, CompositorID: strings.Repeat("b", 64), Attempts: []ApplicationLaunchAttempt{}}
	activity := TerminalActivityState{Version: TerminalActivitySchemaVersion, Terminals: []TerminalActivity{{ContextID: testContextID, CreatedAt: &createdAt}}}
	written := make(map[string][]byte)
	for path, value := range map[string]any{
		legacyContextsFilename: registry,
		legacyLayoutFilename:   layout,
		filepath.Join(legacyApplicationSessionDirectory, legacyApplicationSessionFilename): application,
		filepath.Join(legacyTerminalActivityDirectory, legacyTerminalActivityFilename):     activity,
	} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		written[path] = data
		if err := os.WriteFile(filepath.Join(root, path), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ready := filepath.Join(t.TempDir(), "before-commit")
	command := exec.Command(os.Args[0], "-test.run=^TestMigrationCrashHelper$")
	command.Env = append(os.Environ(),
		"SWAY_SESSION_MIGRATION_CRASH_HELPER=1",
		"SWAY_SESSION_MIGRATION_CRASH_ROOT="+root,
		"SWAY_SESSION_MIGRATION_CRASH_READY="+ready,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal("migration helper did not reach the pre-commit boundary")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("killed migration helper exited successfully")
	}
	for path, before := range written {
		after, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || !bytes.Equal(after, before) {
			t.Fatalf("crashed migration changed legacy source %s: err=%v", path, err)
		}
	}

	result, err := MigrateLegacyState(t.Context(), root)
	if err != nil || !result.Migrated {
		t.Fatalf("retry migration after process death: result=%+v err=%v", result, err)
	}
	loadedRegistry, err := ReadRegistrySnapshot(root)
	if err != nil || !reflect.DeepEqual(loadedRegistry, registry) {
		t.Fatalf("registry after crash recovery = %+v err=%v", loadedRegistry, err)
	}
	var loadedLayout LayoutSnapshot
	if err := LayoutStoreFor(root).LoadInto(&loadedLayout); err != nil || !reflect.DeepEqual(loadedLayout, layout) {
		t.Fatalf("layout after crash recovery = %+v err=%v", loadedLayout, err)
	}
	var loadedApplication ApplicationSessionState
	if err := ApplicationSessionStoreFor(root).LoadInto(&loadedApplication); err != nil || !reflect.DeepEqual(loadedApplication, application) {
		t.Fatalf("application state after crash recovery = %+v err=%v", loadedApplication, err)
	}
	loadedActivity, err := ReadTerminalActivitySnapshot(root)
	if err != nil || !reflect.DeepEqual(loadedActivity, activity) {
		t.Fatalf("terminal activity after crash recovery = %+v err=%v", loadedActivity, err)
	}
	for path, before := range written {
		after, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || !bytes.Equal(after, before) {
			t.Fatalf("successful retry changed legacy source %s: err=%v", path, err)
		}
	}
	if err := VerifyStateDatabaseContext(t.Context(), root); err != nil {
		t.Fatalf("verify recovered state database: %v", err)
	}
}

func TestMigrationCrashHelper(t *testing.T) {
	if os.Getenv("SWAY_SESSION_MIGRATION_CRASH_HELPER") != "1" {
		return
	}
	root := os.Getenv("SWAY_SESSION_MIGRATION_CRASH_ROOT")
	ready := os.Getenv("SWAY_SESSION_MIGRATION_CRASH_READY")
	executeStateCommit = func(*stateWriteTransaction) error {
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			return err
		}
		select {}
	}
	if _, err := MigrateLegacyState(t.Context(), root); err != nil {
		t.Fatal(err)
	}
}

func TestFilterLegacyRuntimeStateSkipsDependentRowsForWrongContextKinds(t *testing.T) {
	terminal := validRegistry().Contexts[0]
	applicationID := ContextID("22222222-2222-4222-8222-222222222222")
	application := flatpakApplicationContext("org.example.App", "org.example.App")
	application.ID = applicationID
	observedAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	legacy := legacyRuntimeState{
		registry: Registry{
			Version:  ContextsSchemaVersion,
			Contexts: []Context{terminal, application},
		},
		application: ApplicationSessionState{
			Version:      ApplicationSessionSchemaVersion,
			CompositorID: strings.Repeat("a", 64),
			Attempts: []ApplicationLaunchAttempt{
				{ContextID: applicationID, StartedAt: observedAt},
				{ContextID: terminal.ID, StartedAt: observedAt},
			},
		},
		activity: TerminalActivityState{
			Version: TerminalActivitySchemaVersion,
			Terminals: []TerminalActivity{
				{ContextID: terminal.ID, CreatedAt: &observedAt},
				{ContextID: applicationID, CreatedAt: &observedAt},
			},
		},
	}

	filtered, skippedAttempts, skippedActivity := filterLegacyRuntimeState(legacy)

	if skippedAttempts != 1 || len(filtered.application.Attempts) != 1 ||
		filtered.application.Attempts[0].ContextID != applicationID {
		t.Fatalf("filtered application attempts = %+v, skipped=%d", filtered.application.Attempts, skippedAttempts)
	}
	if skippedActivity != 1 || len(filtered.activity.Terminals) != 1 ||
		filtered.activity.Terminals[0].ContextID != terminal.ID {
		t.Fatalf("filtered terminal activity = %+v, skipped=%d", filtered.activity.Terminals, skippedActivity)
	}
}

func TestMigrateLegacyStateWaitsForStableSourceLocks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := []byte(`{"version":5,"preferences":{"desktop_indicators":false},"contexts":[]}`)
	if err := os.WriteFile(filepath.Join(root, legacyContextsFilename), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- statefile.WithPrivateDirectoryLock(root, func(*statefile.LockedPrivateDirectory) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := MigrateLegacyState(ctx, root); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("migration ignored legacy source lock: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, StateDatabaseFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled migration created a database: %v", err)
	}
}

func TestStateDatabaseRejectsUnsupportedSchemaWithoutChangingTarget(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := RegistryStoreFor(root).Save(validRegistry()); err != nil {
		t.Fatal(err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	original := validRegistry()
	target := original
	err = RegistryStoreFor(root).LoadInto(&target)
	var versionError *UnsupportedVersionError
	if !errors.As(err, &versionError) || versionError.Got != 2 {
		t.Fatalf("unsupported database schema returned %v", err)
	}
	if !reflect.DeepEqual(target, original) {
		t.Fatalf("failed load changed target: %+v", target)
	}
}

func TestStateDatabaseRejectsUnsafeMainFileWithoutBlocking(t *testing.T) {
	for name, create := range map[string]func(string) error{
		"symlink": func(path string) error { return os.Symlink("missing-target", path) },
		"fifo":    func(path string) error { return unix.Mkfifo(path, 0o600) },
		"mode":    func(path string) error { return os.WriteFile(path, nil, 0o644) },
		"special bits": func(path string) error {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				return err
			}
			return os.Chmod(path, 0o4600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "sway-session")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := create(filepath.Join(root, StateDatabaseFilename)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			var registry Registry
			if err := RegistryStoreFor(root).LoadIntoContext(ctx, &registry); err == nil {
				t.Fatal("unsafe database object was accepted")
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatal("unsafe database object blocked until timeout")
			}
		})
	}
}

func TestStateDatabaseRejectsForeignKeyCorruption(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := RegistryStoreFor(root).Save(validRegistry()); err != nil {
		t.Fatal(err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO terminal_activity (context_id, created_at) VALUES ('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', '2026-09-04T10:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	var registry Registry
	if err := RegistryStoreFor(root).LoadInto(&registry); err != nil {
		t.Fatalf("row-scoped registry read unexpectedly ran a database-wide check: %v", err)
	}
	if err := VerifyStateDatabaseContext(t.Context(), root); err == nil || !strings.Contains(err.Error(), "foreign-key violation") {
		t.Fatalf("foreign-key corruption returned %v", err)
	}
}

func TestStateDatabaseFailsClosedWhenOFDLockingIsUnavailable(t *testing.T) {
	original := sqliteOFDLockingEnabled
	sqliteOFDLockingEnabled = func() bool { return false }
	t.Cleanup(func() { sqliteOFDLockingEnabled = original })

	root := filepath.Join(t.TempDir(), "sway-session")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, StateDatabaseFilename)
	if err := os.WriteFile(databasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RegistryStoreFor(root).Save(validRegistry()); err == nil || !strings.Contains(err.Error(), "OFD locking") {
		t.Fatalf("missing OFD locking returned %v", err)
	}
	info, err := os.Stat(databasePath)
	if err != nil || info.Size() != 0 {
		t.Fatalf("OFD failure mutated the uninitialized database: info=%v err=%v", info, err)
	}
}

func TestStateDatabaseParallelInitializationConverges(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	start := make(chan struct{})
	results := make(chan error, 64)
	for range 64 {
		go func() {
			<-start
			results <- initializeStateDatabase(t.Context(), root)
		}()
	}
	close(start)
	for range 64 {
		if err := <-results; err != nil {
			t.Fatalf("parallel state database initialization failed: %v", err)
		}
	}
}

func TestStateDatabaseParallelInitializationConvergesAcrossProcesses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	coordination := t.TempDir()
	gate := filepath.Join(coordination, "go")
	type child struct {
		command *exec.Cmd
		output  *bytes.Buffer
	}
	children := make([]child, 0, 4)
	for index := range 4 {
		ready := filepath.Join(coordination, fmt.Sprintf("ready-%d", index))
		output := &bytes.Buffer{}
		command := exec.Command(os.Args[0], "-test.run=^TestSQLiteInitializationProcessHelper$")
		command.Env = append(os.Environ(),
			"SWAY_SESSION_SQLITE_INITIALIZATION_HELPER=1",
			"SWAY_SESSION_SQLITE_INITIALIZATION_ROOT="+root,
			"SWAY_SESSION_SQLITE_INITIALIZATION_READY="+ready,
			"SWAY_SESSION_SQLITE_INITIALIZATION_GATE="+gate,
		)
		command.Stdout = output
		command.Stderr = output
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, child{command: command, output: output})
		waitForSQLiteTestFile(t, ready)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if err := child.command.Wait(); err != nil {
			t.Fatalf("SQLite initialization helper failed: %v\n%s", err, child.output.String())
		}
	}
	if err := VerifyStateDatabaseContext(t.Context(), root); err != nil {
		t.Fatalf("verify parallel-initialized database: %v", err)
	}
}

func TestSQLiteInitializationProcessHelper(t *testing.T) {
	if os.Getenv("SWAY_SESSION_SQLITE_INITIALIZATION_HELPER") != "1" {
		return
	}
	root := os.Getenv("SWAY_SESSION_SQLITE_INITIALIZATION_ROOT")
	ready := os.Getenv("SWAY_SESSION_SQLITE_INITIALIZATION_READY")
	gate := os.Getenv("SWAY_SESSION_SQLITE_INITIALIZATION_GATE")
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForSQLiteTestFile(t, gate)
	if err := initializeStateDatabase(t.Context(), root); err != nil {
		t.Fatal(err)
	}
}

func TestStateDatabaseInitializationLockHonorsContextAndCanRetry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, StateDatabaseFilename), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	directory, err := statefile.OpenPrivateDirectory(root, false)
	if err != nil {
		t.Fatal(err)
	}
	initializationLock, err := lockStateDatabaseInitialization(t.Context(), directory)
	if err != nil {
		directory.Close()
		t.Fatal(err)
	}
	defer func() {
		unlockStateDatabaseInitialization(initializationLock)
		if directory != nil {
			_ = directory.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	err = initializeStateDatabase(ctx, root)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked state database initialization returned %v", err)
	}
	unlockStateDatabaseInitialization(initializationLock)
	initializationLock = nil
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	directory = nil
	if err := initializeStateDatabase(t.Context(), root); err != nil {
		t.Fatalf("retry state database initialization: %v", err)
	}
}

func TestStateWriteCancellationBeforeCommitRollsBack(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := RegistryStoreFor(root).Save(validRegistry()); err != nil {
		t.Fatal(err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	tx, err := beginStateWrite(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "UPDATE registry_preferences SET desktop_indicators = 1 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := commitStateWrite(ctx, tx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled commit returned %v", err)
	}
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Fatal(err)
	}
	var registry Registry
	if err := RegistryStoreFor(root).LoadInto(&registry); err != nil {
		t.Fatal(err)
	}
	if registry.Preferences.DesktopIndicators {
		t.Fatal("canceled write became visible")
	}
}

func TestRegistryUpdatesSerializeAcrossProcessesWithoutLosingContexts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	coordination := t.TempDir()
	gate := filepath.Join(coordination, "go")
	type child struct {
		command *exec.Cmd
		output  *bytes.Buffer
	}
	children := make([]child, 0, 2)
	for index := range 2 {
		ready := filepath.Join(coordination, fmt.Sprintf("ready-%d", index))
		output := &bytes.Buffer{}
		command := exec.Command(os.Args[0], "-test.run=^TestSQLiteRegistryUpdateProcessHelper$")
		command.Env = append(os.Environ(),
			"SWAY_SESSION_SQLITE_HELPER=1",
			"SWAY_SESSION_SQLITE_ROOT="+root,
			fmt.Sprintf("SWAY_SESSION_SQLITE_INDEX=%d", index),
			"SWAY_SESSION_SQLITE_READY="+ready,
			"SWAY_SESSION_SQLITE_GATE="+gate,
		)
		command.Stdout = output
		command.Stderr = output
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, child{command: command, output: output})
		waitForSQLiteTestFile(t, ready)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if err := child.command.Wait(); err != nil {
			t.Fatalf("SQLite update helper failed: %v\n%s", err, child.output.String())
		}
	}
	registry, err := ReadRegistrySnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Contexts) != 2 {
		t.Fatalf("concurrent process updates retained %d contexts, want 2: %+v", len(registry.Contexts), registry.Contexts)
	}
}

func TestSQLiteRegistryUpdateProcessHelper(t *testing.T) {
	if os.Getenv("SWAY_SESSION_SQLITE_HELPER") != "1" {
		return
	}
	root := os.Getenv("SWAY_SESSION_SQLITE_ROOT")
	index := os.Getenv("SWAY_SESSION_SQLITE_INDEX")
	ready := os.Getenv("SWAY_SESSION_SQLITE_READY")
	gate := os.Getenv("SWAY_SESSION_SQLITE_GATE")
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(gate); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for SQLite update gate")
		}
		time.Sleep(10 * time.Millisecond)
	}
	id := ContextID("00000000-0000-4000-8000-00000000000" + index)
	_, err := UpdateRegistry(root, func(registry *Registry) error {
		return AddContext(registry, Context{
			ID: id, State: ContextArchived,
			Launcher: Launcher{Kind: LauncherHerdr, Session: "process-" + index, Cwd: "/tmp", Terminal: &TerminalLauncher{Adapter: TerminalAdapterAlacritty}},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func waitForSQLiteTestFile(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStateDatabaseRejectsUnsafeExistingSidecar(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := RegistryStoreFor(root).Save(validRegistry()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-target", filepath.Join(root, StateDatabaseFilename+"-wal")); err != nil {
		t.Fatal(err)
	}
	var registry Registry
	if err := RegistryStoreFor(root).LoadInto(&registry); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("unsafe WAL sidecar returned %v", err)
	}
}

func TestStateDatabaseRejectsOversizedMainFileBeforeSQLiteOpen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := RegistryStoreFor(root).Save(validRegistry()); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(root, StateDatabaseFilename), maxStateDatabaseBytes+1); err != nil {
		t.Fatal(err)
	}
	var registry Registry
	if err := RegistryStoreFor(root).LoadInto(&registry); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized database returned %v", err)
	}
}

func TestStateDatabaseSidecarSizeDoesNotPreventRecoveryPreflight(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := RegistryStoreFor(root).Save(validRegistry()); err != nil {
		t.Fatal(err)
	}
	directory, err := statefile.OpenPrivateDirectory(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		name := StateDatabaseFilename + suffix
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, maxStateDatabaseBytes+1); err != nil {
			t.Fatal(err)
		}
		if _, exists, err := inspectPrivateDatabaseObjectAt(directory, name); err != nil || !exists {
			t.Fatalf("valid large recovery sidecar %s rejected: exists=%t err=%v", name, exists, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}
