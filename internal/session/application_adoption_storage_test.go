package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/statefile"
)

func adoptionStorageFixture(t *testing.T) (string, []Context, ApplicationSessionState) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	contexts := []Context{
		applicationDeltaTestContext("00000000-0000-4000-8000-000000000001", "org.example.First"),
		applicationDeltaTestContext("00000000-0000-4000-8000-000000000002", "org.example.Second"),
	}
	if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: contexts}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 3, 12, 0, 0, 123, time.UTC)
	state := ApplicationSessionState{
		Version: ApplicationSessionSchemaVersion, CompositorID: strings.Repeat("a", 64),
		Attempts: []ApplicationLaunchAttempt{{ContextID: contexts[0].ID, StartedAt: now.Add(-time.Second)}},
		Adoptions: []ApplicationAdoption{
			{ContextID: contexts[0].ID, ObservedAt: now},
			{ContextID: contexts[1].ID, ObservedAt: now.Add(time.Second)},
		},
	}
	return root, contexts, state
}

func TestApplicationAdoptionStorageBackupRoundTrip(t *testing.T) {
	root, _, state := adoptionStorageFixture(t)
	if err := ApplicationSessionStoreFor(root).Save(state); err != nil {
		t.Fatal(err)
	}
	output := backupTestOutput(t)
	if _, err := BackupState(t.Context(), root, output); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateStateBackup(t.Context(), output); err != nil {
		t.Fatal(err)
	}
	readRoot := filepath.Join(t.TempDir(), "restored")
	if err := os.Mkdir(readRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readRoot, StateDatabaseFilename), backupTestRead(t, output), 0o600); err != nil {
		t.Fatal(err)
	}
	var loaded ApplicationSessionState
	if err := ApplicationSessionStoreFor(readRoot).LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, state) {
		t.Fatalf("backup round trip = %+v, err=%v; want %+v", loaded, err, state)
	}
}

func TestApplicationAdoptionStorageRoundTripKeepsAttemptsDistinct(t *testing.T) {
	root, _, state := adoptionStorageFixture(t)
	store := ApplicationSessionStoreFor(root)
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	var loaded ApplicationSessionState
	if err := store.LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, state) {
		t.Fatalf("round trip = %+v, err=%v; want %+v", loaded, err, state)
	}
	state.Adoptions = []ApplicationAdoption{}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	state.Adoptions = nil
	if err := store.LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, state) {
		t.Fatalf("empty adoptions = %+v, err=%v; want nil adoptions and retained attempts", loaded, err)
	}
}

func TestApplicationAdoptionStorageOldSchemaReadIsEmptyAndUnchanged(t *testing.T) {
	root, _, state := adoptionStorageFixture(t)
	state.Adoptions = nil
	store := ApplicationSessionStoreFor(root)
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, StateDatabaseFilename)
	before := backupTestRead(t, path)
	var loaded ApplicationSessionState
	if err := store.LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, state) {
		t.Fatalf("old schema load = %+v, err=%v; want %+v", loaded, err, state)
	}
	if !bytes.Equal(before, backupTestRead(t, path)) {
		t.Fatal("old schema read changed the database")
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	var count, version int
	err = database.db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name = 'application_adoptions'").Scan(&count)
	if err != nil || count != 0 {
		t.Fatalf("old schema read initialized the adoption extension: count=%d err=%v", count, err)
	}
	if err := database.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("schema version = %d, err=%v; want 1", version, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := BackupState(t.Context(), root, backupTestOutput(t)); err != nil {
		t.Fatalf("old schema backup: %v", err)
	}
}

func TestApplicationAdoptionStorageCompositorTagsSurviveLegacyMetadataReset(t *testing.T) {
	root, _, state := adoptionStorageFixture(t)
	store := ApplicationSessionStoreFor(root)
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	// An older binary knows only the singleton and launch-attempt table.
	backupTestMutate(t, root, "DELETE FROM application_session", "DELETE FROM application_launch_attempts",
		"INSERT INTO application_session (id, compositor_id) VALUES (1, '"+strings.Repeat("b", 64)+"')")
	state.CompositorID = strings.Repeat("b", 64)
	state.Attempts = []ApplicationLaunchAttempt{}
	oldAdoptions := state.Adoptions
	state.Adoptions = nil
	var loaded ApplicationSessionState
	if err := store.LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, state) {
		t.Fatalf("legacy reset revived old adoptions: loaded=%+v err=%v", loaded, err)
	}
	if _, err := BackupState(t.Context(), root, backupTestOutput(t)); err != nil {
		t.Fatalf("valid stale compositor evidence rejected by backup: %v", err)
	}
	// Reobserving a context must update its compositor tag even at the same time.
	state.Adoptions = oldAdoptions[1:]
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	if err := store.LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, state) {
		t.Fatalf("new compositor adoption = %+v, err=%v; want %+v", loaded, err, state)
	}
	state.CompositorID = strings.Repeat("c", 64)
	state.Adoptions = nil
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	if err := store.LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, state) {
		t.Fatalf("new compositor clearing = %+v, err=%v", loaded, err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var count int
	if err := database.db.QueryRow("SELECT count(*) FROM application_adoptions").Scan(&count); err != nil || count != 0 {
		t.Fatalf("new compositor retained stale adoption rows: count=%d err=%v", count, err)
	}
}

func TestApplicationAdoptionStorageRejectsOverlappingWriter(t *testing.T) {
	root, _, state := adoptionStorageFixture(t)
	state.Attempts = []ApplicationLaunchAttempt{}
	store := ApplicationSessionStoreFor(root)
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	stale, err := prepareApplicationSessionDelta(t.Context(), database, state)
	if err != nil {
		t.Fatal(err)
	}
	state.Adoptions = append([]ApplicationAdoption(nil), state.Adoptions...)
	state.Adoptions[0].ObservedAt = state.Adoptions[0].ObservedAt.Add(time.Second)
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	tx, err := beginStateWrite(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyApplicationSessionDeltaTx(t.Context(), tx, stale); !errors.Is(err, ErrApplicationSessionConflict) {
		t.Fatalf("stale adoption delta = %v; want application session conflict", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var loaded ApplicationSessionState
	if err := store.LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, state) {
		t.Fatalf("stale writer changed adoptions: loaded=%+v err=%v", loaded, err)
	}
}

func TestApplicationAdoptionStorageDMLAndCascadeInvalidatePreparedDelta(t *testing.T) {
	for _, mutation := range []string{"insert", "update", "delete", "cascade"} {
		t.Run(mutation, func(t *testing.T) {
			root, contexts, state := adoptionStorageFixture(t)
			state.Attempts = []ApplicationLaunchAttempt{}
			state.Adoptions = state.Adoptions[:1]
			store := ApplicationSessionStoreFor(root)
			if err := store.Save(state); err != nil {
				t.Fatal(err)
			}
			database, err := openStateDatabase(t.Context(), root, false)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			delta, err := prepareApplicationSessionDelta(t.Context(), database, state)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "insert":
				_, err = database.db.Exec("INSERT INTO application_adoptions (context_id, compositor_id, observed_at) VALUES (?, ?, ?)", contexts[1].ID, state.CompositorID, "2026-10-03T12:00:01Z")
			case "update":
				_, err = database.db.Exec("UPDATE application_adoptions SET observed_at = '2026-10-03T12:01:00Z'")
			case "delete":
				_, err = database.db.Exec("DELETE FROM application_adoptions")
			case "cascade":
				err = RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: contexts[1:]})
			}
			if err != nil {
				t.Fatal(err)
			}
			tx, err := beginStateWrite(t.Context(), database)
			if err != nil {
				t.Fatal(err)
			}
			if err := applyApplicationSessionDeltaTx(t.Context(), tx, delta); !errors.Is(err, ErrApplicationSessionConflict) {
				t.Fatalf("%s did not invalidate prepared delta: %v", mutation, err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if mutation == "cascade" {
				var loaded ApplicationSessionState
				state.Adoptions = nil
				if err := store.LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, state) {
					t.Fatalf("cascade did not remove adoption: loaded=%+v err=%v", loaded, err)
				}
			}
		})
	}
}

func TestApplicationAdoptionStorageRejectsConcurrentContextKindChange(t *testing.T) {
	root, contexts, state := adoptionStorageFixture(t)
	state.Attempts = []ApplicationLaunchAttempt{}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	delta, err := prepareApplicationSessionDelta(t.Context(), database, state)
	if err != nil {
		t.Fatal(err)
	}
	terminal := validRegistry().Contexts[0]
	terminal.ID = contexts[0].ID
	if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{terminal, contexts[1]}}); err != nil {
		t.Fatal(err)
	}
	tx, err := beginStateWrite(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyApplicationSessionDeltaTx(t.Context(), tx, delta); !errors.Is(err, ErrRegistryConflict) {
		t.Fatalf("adoption after context kind change = %v; want registry conflict", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var loaded ApplicationSessionState
	if err := ApplicationSessionStoreFor(root).LoadInto(&loaded); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conflicting delta persisted state: loaded=%+v err=%v", loaded, err)
	}
}

func TestApplicationAdoptionStorageLegacyMigrationRetainsValidObservations(t *testing.T) {
	_, contexts, state := adoptionStorageFixture(t)
	root := filepath.Join(t.TempDir(), "legacy")
	if err := os.MkdirAll(filepath.Join(root, legacyApplicationSessionDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	registry := validRegistry()
	registry.Contexts = append(registry.Contexts, contexts...)
	want := state
	// JSON order is not significant for migration's uncertain-commit comparison.
	state.Adoptions = []ApplicationAdoption{state.Adoptions[1], state.Adoptions[0],
		{ContextID: "00000000-0000-4000-8000-000000000099", ObservedAt: state.Adoptions[0].ObservedAt},
		{ContextID: registry.Contexts[0].ID, ObservedAt: state.Adoptions[0].ObservedAt},
	}
	sources := map[string][]byte{}
	for name, value := range map[string]any{
		legacyContextsFilename: registry,
		filepath.Join(legacyApplicationSessionDirectory, legacyApplicationSessionFilename): state,
	} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = data
		if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if result, err := MigrateLegacyState(t.Context(), root); err != nil || !result.Migrated || result.ApplicationAdoptions != 2 || result.SkippedApplicationAdoptions != 2 {
		t.Fatalf("migration = %+v, err=%v", result, err)
	}
	var loaded ApplicationSessionState
	if err := ApplicationSessionStoreFor(root).LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, want) {
		t.Fatalf("migration lost adoption evidence: loaded=%+v err=%v; want %+v", loaded, err, want)
	}
	for name, before := range sources {
		if !bytes.Equal(before, backupTestRead(t, filepath.Join(root, name))) {
			t.Fatalf("migration changed source %s", name)
		}
	}
	if _, err := BackupState(t.Context(), root, backupTestOutput(t)); err != nil {
		t.Fatalf("migrated adoptions failed backup validation: %v", err)
	}
}

func TestApplicationAdoptionStorageMigrationRejectsUTCOffsetOverflow(t *testing.T) {
	for _, value := range []string{"9999-12-31T23:30:00-01:00", "0000-01-01T00:30:00+01:00"} {
		t.Run(value, func(t *testing.T) {
			_, contexts, state := adoptionStorageFixture(t)
			root := filepath.Join(t.TempDir(), "legacy")
			if err := os.MkdirAll(filepath.Join(root, legacyApplicationSessionDirectory), 0700); err != nil {
				t.Fatal(err)
			}
			stamp, err := time.Parse(time.RFC3339, value)
			if err != nil {
				t.Fatal(err)
			}
			state.Adoptions[0].ObservedAt = stamp
			for name, value := range map[string]any{
				legacyContextsFilename: Registry{Version: ContextsSchemaVersion, Contexts: contexts},
				filepath.Join(legacyApplicationSessionDirectory, legacyApplicationSessionFilename): state,
			} {
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if result, err := MigrateLegacyState(t.Context(), root); err == nil || result.Migrated {
				t.Fatalf("migration committed unreadable UTC time: %+v %v", result, err)
			}
			if _, err := os.Stat(filepath.Join(root, StateDatabaseFilename)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected migration created a database: %v", err)
			}
		})
	}
}

func TestApplicationAdoptionStorageEqualityIncludesObservations(t *testing.T) {
	_, _, state := adoptionStorageFixture(t)
	reordered := state
	reordered.Adoptions = []ApplicationAdoption{state.Adoptions[1], state.Adoptions[0]}
	if !equalApplicationSessionState(state, reordered) {
		t.Fatal("same observations in a different order compare unequal")
	}
	changed := state
	changed.Adoptions = append([]ApplicationAdoption(nil), state.Adoptions...)
	changed.Adoptions[0].ObservedAt = changed.Adoptions[0].ObservedAt.Add(time.Second)
	if equalApplicationSessionState(state, changed) {
		t.Fatal("different adoption observations compare equal")
	}
	changed.Adoptions = nil
	if equalApplicationSessionState(state, changed) {
		t.Fatal("missing adoption observations compare equal")
	}
}

func TestApplicationAdoptionStorageRejectsMalformedExtensionAndRows(t *testing.T) {
	cases := map[string][]string{
		"missing trigger": {"DROP TRIGGER application_adoption_update_revision"},
		"modified trigger": {"DROP TRIGGER application_adoption_insert_revision", `CREATE TRIGGER application_adoption_insert_revision AFTER INSERT ON application_adoptions
			WHEN EXISTS (SELECT 1 FROM application_session WHERE id = 1)
			BEGIN UPDATE application_session SET revision = revision + 2 WHERE id = 1; END`},
		"wrong trigger owner": {"DROP TRIGGER application_adoption_insert_revision", "CREATE TRIGGER application_adoption_insert_revision AFTER INSERT ON contexts BEGIN SELECT 1; END"},
		"missing foreign key": {"DROP TABLE application_adoptions", `CREATE TABLE application_adoptions (context_id TEXT PRIMARY KEY, compositor_id TEXT NOT NULL, observed_at TEXT NOT NULL) STRICT`},
		"wrong compositor":    {"UPDATE application_adoptions SET compositor_id = 'bad'"},
		"bad time":            {"UPDATE application_adoptions SET observed_at = 'bad'"},
		"zero time":           {"UPDATE application_adoptions SET observed_at = '0001-01-01T00:00:00Z'"},
		"bad stale time":      {"UPDATE application_adoptions SET compositor_id = '" + strings.Repeat("b", 64) + "', observed_at = 'bad'"},
		"wrong context kind":  {"UPDATE application_adoptions SET context_id = '" + string(testContextID) + "' WHERE context_id = '00000000-0000-4000-8000-000000000001'"},
		"orphan context":      {"PRAGMA foreign_keys = OFF", "UPDATE application_adoptions SET context_id = '00000000-0000-4000-8000-000000000099' WHERE context_id = '00000000-0000-4000-8000-000000000001'"},
		"bad context ID":      {"PRAGMA foreign_keys = OFF", "UPDATE application_adoptions SET context_id = 'bad' WHERE context_id = '00000000-0000-4000-8000-000000000001'"},
		"missing session":     {"DELETE FROM application_session"},
	}
	for name, mutations := range cases {
		t.Run(name, func(t *testing.T) {
			root, contexts, state := adoptionStorageFixture(t)
			state.Attempts = []ApplicationLaunchAttempt{}
			contexts = append(contexts, validRegistry().Contexts[0])
			if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: contexts}); err != nil {
				t.Fatal(err)
			}
			store := ApplicationSessionStoreFor(root)
			if err := store.Save(state); err != nil {
				t.Fatal(err)
			}
			backupTestMutate(t, root, mutations...)
			path := filepath.Join(root, StateDatabaseFilename)
			before := backupTestRead(t, path)
			if _, err := ValidateStateBackup(t.Context(), path); err == nil {
				t.Fatal("malformed adoption state passed backup validation")
			}
			loaded := state
			if err := store.LoadInto(&loaded); err == nil {
				t.Fatal("malformed adoption state passed load validation")
			}
			if !reflect.DeepEqual(loaded, state) {
				t.Fatal("failed load changed its target")
			}
			if err := store.Save(state); err == nil {
				t.Fatal("save silently repaired malformed adoption state")
			}
			if !bytes.Equal(before, backupTestRead(t, path)) {
				t.Fatal("rejected adoption state was changed")
			}
		})
	}
}

func TestApplicationAdoptionStorageRejectsInvalidDesiredReferencesAndTime(t *testing.T) {
	for _, kind := range []string{"terminal", "missing", "zero time", "out of range time", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			root, contexts, state := adoptionStorageFixture(t)
			state.Attempts = []ApplicationLaunchAttempt{}
			switch kind {
			case "terminal":
				terminal := validRegistry().Contexts[0]
				contexts = append(contexts, terminal)
				if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: contexts}); err != nil {
					t.Fatal(err)
				}
				state.Adoptions[0].ContextID = terminal.ID
			case "missing":
				state.Adoptions[0].ContextID = "00000000-0000-4000-8000-000000000099"
			case "zero time":
				state.Adoptions[0].ObservedAt = time.Time{}
			case "out of range time":
				state.Adoptions[0].ObservedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			case "duplicate":
				state.Adoptions[1] = state.Adoptions[0]
			}
			store := ApplicationSessionStoreFor(root)
			if err := store.Save(state); err == nil {
				t.Fatal("invalid desired adoption was stored")
			}
			var loaded ApplicationSessionState
			if err := store.LoadInto(&loaded); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid adoption left state: loaded=%+v err=%v", loaded, err)
			}
		})
	}
}

func TestApplicationAdoptionStorageCanonicalizesObservationTime(t *testing.T) {
	root, _, state := adoptionStorageFixture(t)
	state.Adoptions[0].ObservedAt = state.Adoptions[0].ObservedAt.In(time.FixedZone("local", 2*60*60))
	if err := ApplicationSessionStoreFor(root).Save(state); err != nil {
		t.Fatal(err)
	}
	state.Adoptions[0].ObservedAt = state.Adoptions[0].ObservedAt.UTC()
	var loaded ApplicationSessionState
	if err := ApplicationSessionStoreFor(root).LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, state) {
		t.Fatalf("canonical time round trip = %+v, err=%v; want %+v", loaded, err, state)
	}
}

func TestApplicationAdoptionStorageFirstWritersShareOptionalExtension(t *testing.T) {
	root, _, state := adoptionStorageFixture(t)
	store := ApplicationSessionStoreFor(root)
	baseline := state
	baseline.Adoptions = nil
	if err := store.Save(baseline); err != nil {
		t.Fatal(err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	winner, err := prepareApplicationSessionDelta(t.Context(), database, state)
	if err != nil {
		t.Fatal(err)
	}
	loserState := state
	loserState.Adoptions = state.Adoptions[1:]
	loser, err := prepareApplicationSessionDelta(t.Context(), database, loserState)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := beginStateWrite(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyApplicationSessionDeltaTx(t.Context(), tx, winner); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := commitStateWrite(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	tx, err = beginStateWrite(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyApplicationSessionDeltaTx(t.Context(), tx, loser); !errors.Is(err, ErrApplicationSessionConflict) {
		t.Fatalf("competing extension creator = %v; want application session conflict", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var loaded ApplicationSessionState
	if err := store.LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, state) {
		t.Fatalf("extension initialization lost winner: loaded=%+v err=%v", loaded, err)
	}
}

func TestApplicationAdoptionStorageWritesOnlyChangedEvidence(t *testing.T) {
	root, _, state := adoptionStorageFixture(t)
	store := ApplicationSessionStoreFor(root)
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	readRevision := func() int64 {
		t.Helper()
		var revision int64
		if err := database.db.QueryRow("SELECT revision FROM application_session WHERE id = 1").Scan(&revision); err != nil {
			t.Fatal(err)
		}
		return revision
	}
	for _, change := range []string{"unchanged", "observation", "removal"} {
		wantIncrement := int64(1) // The singleton CAS always advances once.
		switch change {
		case "observation":
			state.Adoptions[1].ObservedAt = state.Adoptions[1].ObservedAt.Add(time.Minute)
			wantIncrement++
		case "removal":
			state.Adoptions = state.Adoptions[:1]
			wantIncrement++
		}
		before := readRevision()
		if err := store.Save(state); err != nil {
			t.Fatal(err)
		}
		if increment := readRevision() - before; increment != wantIncrement {
			t.Fatalf("%s evidence advanced revision by %d; want %d", change, increment, wantIncrement)
		}
		var loaded ApplicationSessionState
		if err := store.LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, state) {
			t.Fatalf("%s delta = %+v, err=%v; want %+v", change, loaded, err, state)
		}
	}
}

func TestApplicationAdoptionStorageRollbackAndUnknownCommit(t *testing.T) {
	for _, existingExtension := range []bool{false, true} {
		for _, outcome := range []string{"rollback", "unknown uncommitted", "unknown committed"} {
			name := outcome
			if existingExtension {
				name += " existing extension"
			} else {
				name += " initial extension"
			}
			t.Run(name, func(t *testing.T) {
				root, _, state := adoptionStorageFixture(t)
				baseline := state
				if !existingExtension {
					baseline.Adoptions = nil
				}
				store := ApplicationSessionStoreFor(root)
				if err := store.Save(baseline); err != nil {
					t.Fatal(err)
				}
				candidate := state
				candidate.Attempts = append([]ApplicationLaunchAttempt(nil), state.Attempts...)
				candidate.Attempts[0].StartedAt = candidate.Attempts[0].StartedAt.Add(time.Minute)
				candidate.Adoptions = append([]ApplicationAdoption(nil), state.Adoptions[:1]...)
				candidate.Adoptions[0].ObservedAt = candidate.Adoptions[0].ObservedAt.Add(time.Minute)
				previousCommit := executeStateCommit
				t.Cleanup(func() { executeStateCommit = previousCommit })
				failure := errors.New("fixture lost adoption commit acknowledgement")
				injected := false
				executeStateCommit = func(tx *stateWriteTransaction) error {
					injected = true
					if outcome == "rollback" {
						return ErrStateDatabaseBusy
					}
					if outcome == "unknown committed" {
						if err := previousCommit(tx); err != nil {
							return err
						}
					}
					return failure
				}
				err := store.Save(candidate)
				executeStateCommit = previousCommit
				if !injected {
					t.Fatal("commit fault was not exercised")
				}
				var unknown *statefile.CommitOutcomeUnknownError
				if outcome == "rollback" {
					if !errors.Is(err, ErrStateDatabaseBusy) || errors.As(err, &unknown) {
						t.Fatalf("ordinary rollback error = %v", err)
					}
				} else if !errors.As(err, &unknown) || !errors.Is(err, failure) {
					t.Fatalf("unknown commit lost its type or cause: %v", err)
				}
				want := baseline
				if outcome == "unknown committed" {
					want = candidate
				}
				var loaded ApplicationSessionState
				if err := store.LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, want) {
					t.Fatalf("commit outcome tore evidence: loaded=%+v err=%v; want %+v", loaded, err, want)
				}
				database, err := openStateDatabase(t.Context(), root, false)
				if err != nil {
					t.Fatal(err)
				}
				exists, err := applicationAdoptionTablesExist(t.Context(), database.db)
				if closeErr := database.Close(); err != nil || closeErr != nil {
					t.Fatalf("inspect optional extension: %v %v", err, closeErr)
				}
				if wantExists := existingExtension || outcome == "unknown committed"; exists != wantExists {
					t.Fatalf("commit outcome retained incorrect optional DDL: exists=%t want=%t", exists, wantExists)
				}
				// A fresh retry retains committed observations and finishes a
				// rolled-back save without duplicating or resetting evidence.
				if err := store.Save(candidate); err != nil {
					t.Fatal(err)
				}
				if err := store.LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, candidate) {
					t.Fatalf("retry lost adoption evidence: loaded=%+v err=%v", loaded, err)
				}
				if _, err := BackupState(t.Context(), root, backupTestOutput(t)); err != nil {
					t.Fatalf("retried adoption evidence is not backup-valid: %v", err)
				}
			})
		}
	}
}
