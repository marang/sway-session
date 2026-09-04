package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestTerminalActivityStateValidatesStrictBoundedCanonicalEntries(t *testing.T) {
	createdAt := time.Date(2026, 9, 3, 10, 0, 0, 123, time.UTC)
	focusedAt := createdAt.Add(time.Minute)
	valid := TerminalActivityState{
		Version:   TerminalActivitySchemaVersion,
		Terminals: []TerminalActivity{{ContextID: testContextID, CreatedAt: &createdAt, LastFocusedAt: &focusedAt}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid activity state rejected: %v", err)
	}

	for name, mutate := range map[string]func(*TerminalActivityState){
		"unsupported version": func(state *TerminalActivityState) { state.Version++ },
		"missing array":       func(state *TerminalActivityState) { state.Terminals = nil },
		"duplicate context": func(state *TerminalActivityState) {
			state.Terminals = append(state.Terminals, state.Terminals[0])
		},
		"zero timestamp": func(state *TerminalActivityState) {
			zero := time.Time{}
			state.Terminals[0].CreatedAt = &zero
		},
		"non UTC timestamp": func(state *TerminalActivityState) {
			local := createdAt.In(time.FixedZone("CEST", 2*60*60))
			state.Terminals[0].CreatedAt = &local
		},
		"focus before creation": func(state *TerminalActivityState) {
			before := createdAt.Add(-time.Nanosecond)
			state.Terminals[0].LastFocusedAt = &before
		},
		"empty activity": func(state *TerminalActivityState) {
			state.Terminals[0].CreatedAt = nil
			state.Terminals[0].LastFocusedAt = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.Terminals = append([]TerminalActivity(nil), valid.Terminals...)
			mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("invalid activity state passed validation")
			}
		})
	}
}

func TestTerminalActivityAcceptsLargePersistentInventory(t *testing.T) {
	focusedAt := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	state := TerminalActivityState{Version: TerminalActivitySchemaVersion, Terminals: make([]TerminalActivity, 0, 300)}
	for index := range 300 {
		id := ContextID(fmt.Sprintf("00000000-0000-4000-8000-%012x", index))
		state.Terminals = append(state.Terminals, TerminalActivity{ContextID: id, LastFocusedAt: &focusedAt})
	}
	if err := state.Validate(); err != nil {
		t.Fatalf("validate large terminal activity inventory: %v", err)
	}
}

func TestTerminalActivitySnapshotMissingFileIsEmptyAndReadOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	state, err := ReadTerminalActivitySnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state, emptyTerminalActivityState()) {
		t.Fatalf("missing activity state = %+v", state)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing snapshot created state root: %v", err)
	}
}

func TestTerminalActivityStoreIsStrictAndTransactional(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	if err := RegistryStoreFor(root).Save(validRegistry()); err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 9, 3, 11, 0, 0, 0, time.FixedZone("CEST", 2*60*60))
	focusedAt := createdAt.Add(time.Minute)
	updated, err := UpdateTerminalActivity(root, func(state *TerminalActivityState) error {
		if _, recorded, mutationErr := RecordTerminalCreatedAt(state, testContextID, createdAt); mutationErr != nil || !recorded {
			return errors.Join(mutationErr, errors.New("creation was not recorded"))
		}
		_, recorded, mutationErr := RecordTerminalFocusedAt(state, testContextID, focusedAt)
		if mutationErr != nil || !recorded {
			return errors.Join(mutationErr, errors.New("focus was not recorded"))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	activity, exists := FindTerminalActivity(updated, testContextID)
	if !exists || activity.CreatedAt == nil || activity.CreatedAt.Location() != time.UTC ||
		activity.LastFocusedAt == nil || activity.LastFocusedAt.Location() != time.UTC {
		t.Fatalf("transaction did not canonicalize activity: %+v", updated)
	}

	var loaded TerminalActivityState
	if err := TerminalActivityStoreFor(root).LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, updated) {
		t.Fatalf("activity file round trip: state=%+v err=%v", loaded, err)
	}
	if _, err := os.Stat(filepath.Join(root, legacyTerminalActivityDirectory, legacyTerminalActivityFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("activity still wrote legacy JSON: %v", err)
	}
}

func TestTerminalActivityMutationHelpersAreExactMonotonicAndRemovable(t *testing.T) {
	state := emptyTerminalActivityState()
	createdAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	created, recorded, err := RecordTerminalCreatedAt(&state, testContextID, createdAt)
	if err != nil || !recorded || created.CreatedAt == nil || !created.CreatedAt.Equal(createdAt) {
		t.Fatalf("record creation: activity=%+v recorded=%t err=%v", created, recorded, err)
	}
	recreatedAt := createdAt.Add(time.Hour)
	recreated, recorded, err := RecordTerminalCreatedAt(&state, testContextID, recreatedAt)
	if err != nil || !recorded || recreated.CreatedAt == nil || !recreated.CreatedAt.Equal(recreatedAt) || recreated.LastFocusedAt != nil {
		t.Fatalf("new generation did not reset orphaned activity: activity=%+v recorded=%t err=%v", recreated, recorded, err)
	}

	focusedAt := recreatedAt.Add(time.Minute)
	focused, recorded, err := RecordTerminalFocusedAt(&state, testContextID, focusedAt)
	if err != nil || !recorded || focused.LastFocusedAt == nil || !focused.LastFocusedAt.Equal(focusedAt) {
		t.Fatalf("record focus: activity=%+v recorded=%t err=%v", focused, recorded, err)
	}
	stale, recorded, err := RecordTerminalFocusedAt(&state, testContextID, createdAt)
	if err != nil || recorded || stale.LastFocusedAt == nil || !stale.LastFocusedAt.Equal(focusedAt) {
		t.Fatalf("stale focus changed activity: activity=%+v recorded=%t err=%v", stale, recorded, err)
	}
	if _, recorded, err := RecordTerminalFocusedAt(&state, testContextID, createdAt.Add(-time.Nanosecond)); err != nil || recorded {
		t.Fatalf("focus from an older context generation was not dropped: recorded=%t err=%v", recorded, err)
	}

	lookup, exists := FindTerminalActivity(state, testContextID)
	if !exists || lookup.ContextID != testContextID {
		t.Fatalf("exact activity lookup failed: activity=%+v exists=%t", lookup, exists)
	}
	removed, deleted, err := RemoveTerminalActivity(&state, testContextID)
	if err != nil || !deleted || removed.ContextID != testContextID || len(state.Terminals) != 0 {
		t.Fatalf("remove activity: activity=%+v deleted=%t state=%+v err=%v", removed, deleted, state, err)
	}
	if _, deleted, err := RemoveTerminalActivity(&state, testContextID); err != nil || deleted {
		t.Fatalf("missing activity removal was not idempotent: deleted=%t err=%v", deleted, err)
	}
}

func TestTerminalActivityContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := filepath.Join(t.TempDir(), "sway-session")
	if _, err := ReadTerminalActivitySnapshotContext(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot error = %v", err)
	}
	if _, err := UpdateTerminalActivityContext(ctx, root, func(*TerminalActivityState) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled update error = %v", err)
	}
	if _, err := ReadTerminalInventorySnapshotContext(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled inventory snapshot error = %v", err)
	}
}

func TestTerminalInventoryReadsRegistryAndActivityTogether(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	registry := validRegistry()
	if err := RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 9, 4, 20, 0, 0, 0, time.UTC)
	if err := RecordTerminalCreationContext(t.Context(), root, registry.Contexts[0].ID, createdAt); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadTerminalInventorySnapshotContext(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.Registry, registry) {
		t.Fatalf("inventory registry = %+v, want %+v", snapshot.Registry, registry)
	}
	activity, found := FindTerminalActivity(snapshot.Activity, registry.Contexts[0].ID)
	if !found || activity.CreatedAt == nil || !activity.CreatedAt.Equal(createdAt) {
		t.Fatalf("inventory activity = %+v", snapshot.Activity)
	}
}

func TestTerminalFocusWriteHonorsContextWhileDatabaseIsBusy(t *testing.T) {
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
	if _, err := tx.ExecContext(t.Context(), "UPDATE registry_preferences SET desktop_indicators = desktop_indicators WHERE id = 1"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = RecordTerminalFocusBatchContext(ctx, root, map[ContextID]time.Time{testContextID: time.Now().UTC()})
	if !errors.Is(err, context.DeadlineExceeded) && !IsStateDatabaseBusy(err) {
		t.Fatalf("busy focus write returned %v, want deadline or retryable busy error", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("busy focus write ignored context for %v", elapsed)
	}
}

func TestRegistryRemovalCascadesTerminalActivity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	registry := Registry{Version: ContextsSchemaVersion, Preferences: RegistryPreferences{}, Contexts: []Context{}}
	created, err := CreateTerminalInstanceContext(&registry, TerminalInstanceRequest{
		Adapter: TerminalAdapterAlacritty,
		Cwd:     t.TempDir(),
		Label:   "New terminal",
	}, func() (ContextID, error) { return testContextID, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}

	createdAt := time.Date(2026, 9, 3, 14, 0, 0, 0, time.UTC)
	if err := RecordTerminalCreationContext(t.Context(), root, created.ID, createdAt); err != nil {
		t.Fatalf("record terminal creation: %v", err)
	}
	if _, err := UpdateRegistry(root, func(registry *Registry) error {
		_, removeErr := RemoveContext(registry, string(created.ID))
		return removeErr
	}); err != nil {
		t.Fatalf("remove terminal context: %v", err)
	}
	activity, err := ReadTerminalActivitySnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(activity.Terminals) != 0 {
		t.Fatalf("removed context retained terminal activity: %+v", activity)
	}
}
