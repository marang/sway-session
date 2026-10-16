package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/statefile"
)

func environmentTestCreate(t *testing.T, store WorkEnvironmentStore, name string) WorkEnvironment {
	t.Helper()
	value, err := store.CreateContext(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func environmentTestAdd(t *testing.T, store WorkEnvironmentStore, value WorkEnvironment, member ContextID) WorkEnvironment {
	t.Helper()
	value, err := store.AddMemberContext(t.Context(), value.ID, value.Revision, member)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestWorkEnvironmentEditsAreNonowningAndRevisionChecked(t *testing.T) {
	root := backupTestRoot(t)
	store := WorkEnvironmentStoreFor(root)
	before := validRegistry()
	_, registryRevision, _, _, err := RegistryStoreFor(root).LoadIfChangedContext(t.Context(), 0, false)
	if err != nil {
		t.Fatal(err)
	}
	first := environmentTestAdd(t, store, environmentTestCreate(t, store, "Development"), testContextID)
	second := environmentTestAdd(t, store, environmentTestCreate(t, store, "development"), testContextID)
	if _, err := store.CreateContext(t.Context(), "Development"); !errors.Is(err, ErrWorkEnvironmentNameConflict) {
		t.Fatalf("duplicate name = %v", err)
	}
	if _, err := store.RenameContext(t.Context(), second.ID, second.Revision, "Development"); !errors.Is(err, ErrWorkEnvironmentNameConflict) {
		t.Fatalf("rename duplicate = %v", err)
	}
	if _, err := store.AddMemberContext(t.Context(), first.ID, first.Revision, testContextID); err == nil {
		t.Fatal("accepted duplicate member")
	}
	first, err = store.SetPrimaryContext(t.Context(), first.ID, first.Revision, testContextID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RenameContext(t.Context(), first.ID, first.Revision-1, "stale"); !errors.Is(err, ErrWorkEnvironmentConflict) {
		t.Fatalf("stale edit = %v", err)
	}
	first, err = store.RenameContext(t.Context(), first.ID, first.Revision, "Renamed")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadContext(t.Context(), first.ID)
	if err != nil || !reflect.DeepEqual(loaded, first) {
		t.Fatalf("roundtrip = %+v, %v; want %+v", loaded, err, first)
	}
	first, err = store.RemoveMemberContext(t.Context(), first.ID, first.Revision, testContextID)
	if err != nil || first.Primary != "" || len(first.Members) != 0 {
		t.Fatalf("remove primary = %+v %v", first, err)
	}
	if err := store.DeleteContext(t.Context(), first.ID, first.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadContext(t.Context(), first.ID); !errors.Is(err, ErrWorkEnvironmentNotFound) {
		t.Fatalf("deleted load = %v", err)
	}
	var after Registry
	if err := RegistryStoreFor(root).LoadInto(&after); err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("definition edits mutated contexts: %+v %v", after, err)
	}
	if _, gotRevision, _, changed, err := RegistryStoreFor(root).LoadIfChangedContext(t.Context(), registryRevision, true); err != nil || changed || gotRevision != registryRevision {
		t.Fatalf("definition edits invalidated registry cache: revision=%d changed=%t err=%v", gotRevision, changed, err)
	}
	loaded, err = store.LoadContext(t.Context(), second.ID)
	if err != nil || !reflect.DeepEqual(loaded, second) {
		t.Fatalf("shared membership changed: %+v %v", loaded, err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var count int
	if err := database.db.QueryRow("SELECT count(*) FROM work_environment_members WHERE environment_id = ?", first.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("environment-owned cleanup: %d %v", count, err)
	}
}

func TestWorkEnvironmentOldSchemaAndMissingStateReadDoNotMigrate(t *testing.T) {
	root := backupTestRoot(t)
	path := filepath.Join(root, StateDatabaseFilename)
	before := backupTestRead(t, path)
	store := WorkEnvironmentStoreFor(root)
	values, err := store.ListContext(t.Context())
	if err != nil || len(values) != 0 {
		t.Fatalf("old list = %+v %v", values, err)
	}
	if _, err := store.LoadContext(t.Context(), EnvironmentID(testContextID)); !errors.Is(err, ErrWorkEnvironmentNotFound) {
		t.Fatalf("old load = %v", err)
	}
	if !bytes.Equal(before, backupTestRead(t, path)) {
		t.Fatal("read migrated old database")
	}
	if _, err := BackupState(t.Context(), root, backupTestOutput(t)); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if values, err := WorkEnvironmentStoreFor(missing).ListContext(t.Context()); err != nil || len(values) != 0 {
		t.Fatalf("missing list = %v %v", values, err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read created missing state: %v", err)
	}
}

func TestWorkEnvironmentCanStartWithOnlyOrganizationMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "new-state")
	store := WorkEnvironmentStoreFor(root)
	value := environmentTestCreate(t, store, "Start here")
	loaded, err := store.LoadContext(t.Context(), value.ID)
	if err != nil || !reflect.DeepEqual(loaded, value) {
		t.Fatalf("metadata-only state = %+v %v", loaded, err)
	}
	if _, err := BackupState(t.Context(), root, backupTestOutput(t)); err != nil {
		t.Fatal(err)
	}
	var registry Registry
	if err := RegistryStoreFor(root).LoadInto(&registry); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("organization created a registry: %+v %v", registry, err)
	}
}

func TestWorkEnvironmentContextRemovalRetainsOrderedUnresolvedPrimary(t *testing.T) {
	root := backupTestRoot(t)
	secondID := ContextID("22222222-2222-4222-8222-222222222222")
	_, err := UpdateRegistry(root, func(registry *Registry) error {
		value := registry.Contexts[0]
		value.ID = secondID
		value.Launcher.Session = "second"
		registry.Contexts = append(registry.Contexts, value)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	store := WorkEnvironmentStoreFor(root)
	value := environmentTestCreate(t, store, "two worktrees")
	value = environmentTestAdd(t, store, value, testContextID)
	value = environmentTestAdd(t, store, value, secondID)
	value, err = store.SetPrimaryContext(t.Context(), value.ID, value.Revision, testContextID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = UpdateRegistry(root, func(registry *Registry) error { _, err := RemoveContext(registry, string(testContextID)); return err })
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadContext(t.Context(), value.ID)
	if err != nil || !reflect.DeepEqual(loaded, value) {
		t.Fatalf("purge changed refs: %+v %v", loaded, err)
	}
	if _, err := store.AddMemberContext(t.Context(), value.ID, value.Revision, ContextID("33333333-3333-4333-8333-333333333333")); !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("add missing = %v", err)
	}
	value, err = store.RemoveMemberContext(t.Context(), value.ID, value.Revision, testContextID)
	if err != nil {
		t.Fatal(err)
	}
	if value.Primary != "" || !reflect.DeepEqual(value.Members, []ContextID{secondID}) {
		t.Fatalf("unresolved removal changed survivors: %+v", value)
	}
}

func TestWorkEnvironmentConcurrentFirstWritersAndCAS(t *testing.T) {
	root := backupTestRoot(t)
	store := WorkEnvironmentStoreFor(root)
	var wait sync.WaitGroup
	errorsOut := make(chan error, 4)
	for index := range 4 {
		wait.Go(func() { _, err := store.CreateContext(t.Context(), fmt.Sprintf("group-%d", index)); errorsOut <- err })
	}
	wait.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	values, err := store.ListContext(t.Context())
	if err != nil || len(values) != 4 {
		t.Fatalf("first writers = %+v %v", values, err)
	}
	value := values[0]
	errorsOut = make(chan error, 2)
	for index := range 2 {
		wait.Go(func() {
			_, err := store.RenameContext(t.Context(), value.ID, value.Revision, fmt.Sprintf("winner-%d", index))
			errorsOut <- err
		})
	}
	wait.Wait()
	close(errorsOut)
	wins, conflicts := 0, 0
	for err := range errorsOut {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrWorkEnvironmentConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("CAS wins=%d conflicts=%d", wins, conflicts)
	}
}

func TestWorkEnvironmentCancellationAndRevisionOverflowPreserveDefinition(t *testing.T) {
	root := backupTestRoot(t)
	store := WorkEnvironmentStoreFor(root)
	value := environmentTestCreate(t, store, "original")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.RenameContext(ctx, value.ID, value.Revision, "cancelled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	backupTestMutate(t, root, fmt.Sprintf("UPDATE work_environments SET revision = %d", int64(math.MaxInt64)))
	if _, err := store.RenameContext(t.Context(), value.ID, math.MaxInt64, "overflow"); err == nil {
		t.Fatal("accepted revision overflow")
	}
	loaded, err := store.LoadContext(t.Context(), value.ID)
	if err != nil || loaded.Name != value.Name || loaded.Revision != math.MaxInt64 {
		t.Fatalf("failed edit changed definition: %+v %v", loaded, err)
	}
}

func TestWorkEnvironmentCommitFailuresPreserveAtomicExtensionAndCandidate(t *testing.T) {
	for _, outcome := range []string{"rollback", "unknown uncommitted", "unknown committed"} {
		t.Run(outcome, func(t *testing.T) {
			root := backupTestRoot(t)
			store := WorkEnvironmentStoreFor(root)
			previous := executeStateCommit
			t.Cleanup(func() { executeStateCommit = previous })
			failure := errors.New("lost environment commit acknowledgement")
			executeStateCommit = func(tx *stateWriteTransaction) error {
				if outcome == "rollback" {
					return ErrStateDatabaseBusy
				}
				if outcome == "unknown committed" {
					if err := previous(tx); err != nil {
						return err
					}
				}
				return failure
			}
			value, err := store.CreateContext(t.Context(), "uncertain")
			executeStateCommit = previous
			var unknown *statefile.CommitOutcomeUnknownError
			if outcome == "rollback" {
				if !errors.Is(err, ErrStateDatabaseBusy) || errors.As(err, &unknown) {
					t.Fatalf("rollback = %v", err)
				}
			} else if !errors.As(err, &unknown) || !errors.Is(err, failure) || value.ID == "" {
				t.Fatalf("unknown candidate = %+v %v", value, err)
			}
			values, err := store.ListContext(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if outcome == "unknown committed" {
				if len(values) != 1 || !reflect.DeepEqual(values[0], value) {
					t.Fatalf("committed candidate lost: %+v", values)
				}
			} else if len(values) != 0 {
				t.Fatalf("uncommitted rows visible: %+v", values)
			}
			database, err := openStateDatabase(t.Context(), root, false)
			if err != nil {
				t.Fatal(err)
			}
			exists, err := workEnvironmentTablesExist(t.Context(), database.db)
			if closeErr := database.Close(); err != nil || closeErr != nil {
				t.Fatalf("extension check: %v %v", err, closeErr)
			}
			if exists != (outcome == "unknown committed") {
				t.Fatalf("torn extension: %v", exists)
			}
		})
	}
}

func TestWorkEnvironmentEditsRetainTypedUnknownCommitOutcome(t *testing.T) {
	for _, kind := range []string{"rename", "add", "remove", "primary", "delete"} {
		for _, committed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/committed=%t", kind, committed), func(t *testing.T) {
				root := backupTestRoot(t)
				secondID := ContextID("22222222-2222-4222-8222-222222222222")
				_, err := UpdateRegistry(root, func(registry *Registry) error {
					value := registry.Contexts[0]
					value.ID, value.Launcher.Session = secondID, "second"
					registry.Contexts = append(registry.Contexts, value)
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				store := WorkEnvironmentStoreFor(root)
				before := environmentTestAdd(t, store, environmentTestCreate(t, store, "Original"), testContextID)
				before, err = store.SetPrimaryContext(t.Context(), before.ID, before.Revision, testContextID)
				if err != nil {
					t.Fatal(err)
				}
				previous := executeStateCommit
				t.Cleanup(func() { executeStateCommit = previous })
				failure := errors.New("lost edit commit acknowledgement")
				executeStateCommit = func(tx *stateWriteTransaction) error {
					if committed {
						if err := previous(tx); err != nil {
							return err
						}
					}
					return failure
				}
				var candidate WorkEnvironment
				switch kind {
				case "rename":
					candidate, err = store.RenameContext(t.Context(), before.ID, before.Revision, "Renamed")
				case "add":
					candidate, err = store.AddMemberContext(t.Context(), before.ID, before.Revision, secondID)
				case "remove":
					candidate, err = store.RemoveMemberContext(t.Context(), before.ID, before.Revision, testContextID)
				case "primary":
					candidate, err = store.SetPrimaryContext(t.Context(), before.ID, before.Revision, "")
				case "delete":
					err = store.DeleteContext(t.Context(), before.ID, before.Revision)
				}
				executeStateCommit = previous
				var unknown *statefile.CommitOutcomeUnknownError
				if !errors.As(err, &unknown) || !errors.Is(err, failure) {
					t.Fatalf("lost uncertainty: %v", err)
				}
				loaded, err := store.LoadContext(t.Context(), before.ID)
				if committed && kind == "delete" {
					if !errors.Is(err, ErrWorkEnvironmentNotFound) {
						t.Fatalf("deleted definition = %+v %v", loaded, err)
					}
				} else {
					want := before
					if committed {
						want = candidate
					}
					if err != nil || !reflect.DeepEqual(loaded, want) {
						t.Fatalf("torn edit = %+v %v; want %+v", loaded, err, want)
					}
				}
				if _, err := BackupState(t.Context(), root, backupTestOutput(t)); err != nil {
					t.Fatalf("uncertain edit not backup-valid: %v", err)
				}
			})
		}
	}
}

func TestWorkEnvironmentMutationsSerializeWithReachRegistryLock(t *testing.T) {
	for _, kind := range []string{"create", "rename", "add", "remove", "primary", "delete"} {
		t.Run(kind, func(t *testing.T) {
			root := backupTestRoot(t)
			secondID := ContextID("22222222-2222-4222-8222-222222222222")
			_, err := UpdateRegistry(root, func(registry *Registry) error {
				value := registry.Contexts[0]
				value.ID, value.Launcher.Session = secondID, "second"
				registry.Contexts = append(registry.Contexts, value)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			store := WorkEnvironmentStoreFor(root)
			before := environmentTestAdd(t, store, environmentTestCreate(t, store, "Original"), testContextID)
			before, err = store.SetPrimaryContext(t.Context(), before.ID, before.Revision, testContextID)
			if err != nil {
				t.Fatal(err)
			}
			mutate := func(ctx context.Context) error {
				var err error
				switch kind {
				case "create":
					_, err = store.CreateContext(ctx, "Created")
				case "rename":
					_, err = store.RenameContext(ctx, before.ID, before.Revision, "Renamed")
				case "add":
					_, err = store.AddMemberContext(ctx, before.ID, before.Revision, secondID)
				case "remove":
					_, err = store.RemoveMemberContext(ctx, before.ID, before.Revision, testContextID)
				case "primary":
					_, err = store.SetPrimaryContext(ctx, before.ID, before.Revision, "")
				case "delete":
					err = store.DeleteContext(ctx, before.ID, before.Revision)
				}
				return err
			}
			// This is the lock held by Reach between revalidation and focus.
			err = WithRegistryLockContext(t.Context(), root, func(*statefile.LockedPrivateDirectory) error {
				ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
				defer cancel()
				if err := mutate(ctx); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("mutation bypassed final-effect lock or lost cancellation: %v", err)
				}
				loaded, err := store.LoadContext(t.Context(), before.ID)
				if err != nil || !reflect.DeepEqual(loaded, before) {
					t.Fatalf("locked definition changed: %+v %v", loaded, err)
				}
				values, err := store.ListContext(t.Context())
				if err != nil || len(values) != 1 {
					t.Fatalf("locked create changed collection: %+v %v", values, err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := mutate(t.Context()); err != nil {
				t.Fatalf("mutation did not recover after lock release: %v", err)
			}
		})
	}
}
