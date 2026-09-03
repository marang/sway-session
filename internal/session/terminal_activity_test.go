package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
		"too many entries": func(state *TerminalActivityState) {
			state.Terminals = make([]TerminalActivity, MaxContexts+1)
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

func TestTerminalActivityFileIsStrictAndTransactional(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
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
	if err := TerminalActivityFile(root).LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, updated) {
		t.Fatalf("activity file round trip: state=%+v err=%v", loaded, err)
	}
	path := filepath.Join(root, TerminalActivityDirectory, TerminalActivityFilename)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), `"created_at": "2026-09-03T09:00:00Z"`) ||
		!strings.Contains(string(contents), `"last_focused_at": "2026-09-03T09:01:00Z"`) {
		t.Fatalf("activity file did not encode canonical UTC values: %s", contents)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("activity state is not private: info=%v err=%v", info, err)
	}

	unsafe := []byte(`{"version":1,"terminals":[],"command":"sh"}`)
	if err := os.WriteFile(path, unsafe, 0o600); err != nil {
		t.Fatal(err)
	}
	target := updated
	if err := TerminalActivityFile(root).LoadInto(&target); err == nil {
		t.Fatal("unknown activity field passed strict load")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, unsafe) {
		t.Fatalf("strict load changed rejected file: data=%q err=%v", after, err)
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
}

func TestRecordTerminalCreationPrunesOrphansBeforeCapacityCheck(t *testing.T) {
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
	if err := RegistryFile(root).Save(registry); err != nil {
		t.Fatal(err)
	}

	focusedAt := time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC)
	orphans := TerminalActivityState{Version: TerminalActivitySchemaVersion, Terminals: make([]TerminalActivity, 0, MaxContexts)}
	for index := 0; index < MaxContexts; index++ {
		id := ContextID(fmt.Sprintf("%08x-1111-4111-8111-%012x", index+1000, index+1000))
		orphans.Terminals = append(orphans.Terminals, TerminalActivity{ContextID: id, LastFocusedAt: &focusedAt})
	}
	if err := TerminalActivityFile(root).Save(orphans); err != nil {
		t.Fatal(err)
	}

	createdAt := focusedAt.Add(time.Hour)
	if err := RecordTerminalCreationContext(t.Context(), root, created.ID, createdAt); err != nil {
		t.Fatalf("orphaned activity blocked authoritative terminal creation: %v", err)
	}
	activity, err := ReadTerminalActivitySnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(activity.Terminals) != 1 || activity.Terminals[0].ContextID != created.ID ||
		activity.Terminals[0].CreatedAt == nil || !activity.Terminals[0].CreatedAt.Equal(createdAt) {
		t.Fatalf("activity was not pruned to the authoritative terminal: %+v", activity)
	}
}
