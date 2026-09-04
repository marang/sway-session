package session

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestApplicationSessionSaveWritesOnlyAttemptDelta(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	first := applicationDeltaTestContext("00000000-0000-4000-8000-000000000001", "org.example.First")
	changed := applicationDeltaTestContext("00000000-0000-4000-8000-000000000002", "org.example.Changed")
	appended := applicationDeltaTestContext("00000000-0000-4000-8000-000000000003", "org.example.Appended")
	if err := RegistryStoreFor(root).Save(Registry{
		Version:  ContextsSchemaVersion,
		Contexts: []Context{first, changed, appended},
	}); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)
	stored := ApplicationSessionState{
		Version:      ApplicationSessionSchemaVersion,
		CompositorID: strings.Repeat("a", 64),
		Attempts: []ApplicationLaunchAttempt{
			{ContextID: first.ID, StartedAt: startedAt},
			{ContextID: changed.ID, StartedAt: startedAt.Add(time.Second)},
		},
	}
	store := ApplicationSessionStoreFor(root)
	if err := store.Save(stored); err != nil {
		t.Fatal(err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TRIGGER reject_unchanged_attempt_update BEFORE UPDATE ON application_launch_attempts WHEN OLD.context_id = '` + string(first.ID) + `' BEGIN SELECT RAISE(ABORT, 'unchanged attempt updated'); END`,
		`CREATE TRIGGER reject_unchanged_attempt_delete BEFORE DELETE ON application_launch_attempts WHEN OLD.context_id = '` + string(first.ID) + `' BEGIN SELECT RAISE(ABORT, 'unchanged attempt deleted'); END`,
		`CREATE TRIGGER reject_unchanged_compositor_update BEFORE UPDATE OF compositor_id ON application_session BEGIN SELECT RAISE(ABORT, 'unchanged compositor updated'); END`,
	}
	for _, statement := range statements {
		if _, err := database.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	candidate := ApplicationSessionState{
		Version:      ApplicationSessionSchemaVersion,
		CompositorID: stored.CompositorID,
		Attempts: []ApplicationLaunchAttempt{
			stored.Attempts[0],
			{ContextID: changed.ID, StartedAt: startedAt.Add(2 * time.Second)},
			{ContextID: appended.ID, StartedAt: startedAt.Add(3 * time.Second)},
		},
	}
	if err := store.Save(candidate); err != nil {
		t.Fatalf("save attempt delta: %v", err)
	}
	var loaded ApplicationSessionState
	if err := store.LoadInto(&loaded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, candidate) {
		t.Fatalf("application session after delta = %+v, want %+v", loaded, candidate)
	}
}

func TestApplicationSessionDeltaRejectsOverlappingWriter(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	first := applicationDeltaTestContext("00000000-0000-4000-8000-000000000001", "org.example.First")
	second := applicationDeltaTestContext("00000000-0000-4000-8000-000000000002", "org.example.Second")
	if err := RegistryStoreFor(root).Save(Registry{
		Version:  ContextsSchemaVersion,
		Contexts: []Context{first, second},
	}); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)
	store := ApplicationSessionStoreFor(root)
	baseline := ApplicationSessionState{
		Version:      ApplicationSessionSchemaVersion,
		CompositorID: strings.Repeat("a", 64),
		Attempts:     []ApplicationLaunchAttempt{{ContextID: first.ID, StartedAt: startedAt}},
	}
	if err := store.Save(baseline); err != nil {
		t.Fatal(err)
	}

	winnerDatabase, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer winnerDatabase.Close()
	loserDatabase, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer loserDatabase.Close()
	winner := baseline
	winner.Attempts = append([]ApplicationLaunchAttempt(nil), baseline.Attempts...)
	winner.Attempts[0].StartedAt = startedAt.Add(time.Second)
	loser := baseline
	loser.Attempts = append(append([]ApplicationLaunchAttempt(nil), baseline.Attempts...), ApplicationLaunchAttempt{
		ContextID: second.ID,
		StartedAt: startedAt.Add(2 * time.Second),
	})
	winnerDelta, err := prepareApplicationSessionDelta(t.Context(), winnerDatabase, winner)
	if err != nil {
		t.Fatal(err)
	}
	loserDelta, err := prepareApplicationSessionDelta(t.Context(), loserDatabase, loser)
	if err != nil {
		t.Fatal(err)
	}

	winnerTx, err := beginStateWrite(t.Context(), winnerDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyApplicationSessionDeltaTx(t.Context(), winnerTx, winnerDelta); err != nil {
		_ = winnerTx.Rollback()
		t.Fatal(err)
	}
	if err := commitStateWrite(t.Context(), winnerTx); err != nil {
		t.Fatal(err)
	}
	loserTx, err := beginStateWrite(t.Context(), loserDatabase)
	if err != nil {
		t.Fatal(err)
	}
	defer loserTx.Rollback()
	if err := applyApplicationSessionDeltaTx(t.Context(), loserTx, loserDelta); !errors.Is(err, ErrApplicationSessionConflict) {
		t.Fatalf("stale application delta error = %v, want %v", err, ErrApplicationSessionConflict)
	}

	var loaded ApplicationSessionState
	if err := store.LoadInto(&loaded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, winner) {
		t.Fatalf("stale writer changed application session: got %+v, want %+v", loaded, winner)
	}
}

func TestApplicationSessionDeltaDetectsConcurrentAttemptCascade(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	removed := applicationDeltaTestContext("00000000-0000-4000-8000-000000000001", "org.example.Removed")
	retained := applicationDeltaTestContext("00000000-0000-4000-8000-000000000002", "org.example.Retained")
	if err := RegistryStoreFor(root).Save(Registry{
		Version:  ContextsSchemaVersion,
		Contexts: []Context{removed, retained},
	}); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)
	store := ApplicationSessionStoreFor(root)
	baseline := ApplicationSessionState{
		Version:      ApplicationSessionSchemaVersion,
		CompositorID: strings.Repeat("a", 64),
		Attempts:     []ApplicationLaunchAttempt{{ContextID: removed.ID, StartedAt: startedAt}},
	}
	if err := store.Save(baseline); err != nil {
		t.Fatal(err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	candidate := baseline
	candidate.Attempts = append(append([]ApplicationLaunchAttempt(nil), baseline.Attempts...), ApplicationLaunchAttempt{
		ContextID: retained.ID,
		StartedAt: startedAt.Add(time.Second),
	})
	delta, err := prepareApplicationSessionDelta(t.Context(), database, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := RegistryStoreFor(root).Save(Registry{
		Version:  ContextsSchemaVersion,
		Contexts: []Context{retained},
	}); err != nil {
		t.Fatal(err)
	}

	tx, err := beginStateWrite(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := applyApplicationSessionDeltaTx(t.Context(), tx, delta); !errors.Is(err, ErrApplicationSessionConflict) {
		t.Fatalf("delta after cascading attempt removal error = %v, want %v", err, ErrApplicationSessionConflict)
	}
}

func TestApplicationSessionDeltaRejectsConcurrentContextKindChange(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	application := applicationDeltaTestContext("00000000-0000-4000-8000-000000000001", "org.example.App")
	if err := RegistryStoreFor(root).Save(Registry{
		Version: ContextsSchemaVersion, Contexts: []Context{application},
	}); err != nil {
		t.Fatal(err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	candidate := ApplicationSessionState{
		Version:      ApplicationSessionSchemaVersion,
		CompositorID: strings.Repeat("a", 64),
		Attempts: []ApplicationLaunchAttempt{{
			ContextID: application.ID,
			StartedAt: time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC),
		}},
	}
	delta, err := prepareApplicationSessionDelta(t.Context(), database, candidate)
	if err != nil {
		t.Fatal(err)
	}
	terminal := validRegistry().Contexts[0]
	terminal.ID = application.ID
	if err := RegistryStoreFor(root).Save(Registry{
		Version: ContextsSchemaVersion, Contexts: []Context{terminal},
	}); err != nil {
		t.Fatal(err)
	}

	tx, err := beginStateWrite(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := applyApplicationSessionDeltaTx(t.Context(), tx, delta); !errors.Is(err, ErrRegistryConflict) {
		t.Fatalf("delta after context kind change error = %v, want %v", err, ErrRegistryConflict)
	}
}

func TestEmptyApplicationSessionDeltaIgnoresUnrelatedRegistryRevision(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	registry := validRegistry()
	if err := RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	candidate := ApplicationSessionState{
		Version:      ApplicationSessionSchemaVersion,
		CompositorID: strings.Repeat("a", 64),
		Attempts:     []ApplicationLaunchAttempt{},
	}
	delta, err := prepareApplicationSessionDelta(t.Context(), database, candidate)
	if err != nil {
		t.Fatal(err)
	}
	registry.Preferences.DesktopIndicators = !registry.Preferences.DesktopIndicators
	if err := RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}

	tx, err := beginStateWrite(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyApplicationSessionDeltaTx(t.Context(), tx, delta); err != nil {
		_ = tx.Rollback()
		t.Fatalf("apply empty application delta after unrelated registry change: %v", err)
	}
	if err := commitStateWrite(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	var loaded ApplicationSessionState
	if err := ApplicationSessionStoreFor(root).LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, candidate) {
		t.Fatalf("application session after empty delta = %+v err=%v", loaded, err)
	}
}

func applicationDeltaTestContext(id ContextID, applicationID string) Context {
	contextValue := flatpakApplicationContext(applicationID, applicationID)
	contextValue.ID = id
	return contextValue
}
