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

	"github.com/marang/sway-session/internal/statefile"
)

func lifecycleStoreTestRegister(number int) LifecycleOperation {
	app := desktopApplicationContext(fmt.Sprintf("org.example.App%d.desktop", number), fmt.Sprintf("org.example.App%d", number))
	app.ID = ContextID(fmt.Sprintf("11111111-1111-4111-8111-%012d", number))
	return LifecycleOperation{
		ID: fmt.Sprintf("22222222-2222-4222-8222-%012d", number), Version: LifecycleOperationVersion,
		Kind: LifecycleRegister, Phase: LifecycleForward, After: []Context{app},
		CompositorID: "test-compositor-lifetime", UpdatedAt: time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC),
		Targets: []LifecycleWindowTarget{{ContextID: app.ID, ContainerID: int64(number + 1), Identity: app.App.Identity, WantMark: true}},
	}
}

func lifecycleStoreTestBegin(t *testing.T, root string, operation LifecycleOperation) LifecycleOperation {
	t.Helper()
	operation, err := BeginLifecycleOperationContext(t.Context(), root, operation)
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

func lifecycleStoreTestRegistry(t *testing.T, root string) Registry {
	t.Helper()
	registry, err := ReadRegistrySnapshotContext(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestLifecycleStoreRegisterIntentCompletion(t *testing.T) {
	root := backupTestRoot(t)
	original := lifecycleStoreTestRegistry(t, root)
	operation := lifecycleStoreTestBegin(t, root, lifecycleStoreTestRegister(1))
	if operation.Revision != 1 || operation.Before == nil || !operation.NextAttempt.IsZero() {
		t.Fatalf("unexpected normalized intent: %+v", operation)
	}
	if registry := lifecycleStoreTestRegistry(t, root); !reflect.DeepEqual(registry, original) {
		t.Fatal("register exposed desired context or presentation changes before completion")
	}
	blocked, err := BlockedLifecycleContextIDsContext(t.Context(), root)
	if err != nil || len(blocked) != 1 {
		t.Fatalf("reserved context projection: %v, %v", blocked, err)
	}
	if _, found := blocked[operation.After[0].ID]; !found {
		t.Fatal("pending registration is not reserved")
	}
	var retained *LifecycleOperationHandle
	err = WithLifecycleOperationContext(t.Context(), root, operation.ID, func(handle *LifecycleOperationHandle) error {
		retained = handle
		candidate := handle.Registry
		candidate.Contexts = append(candidate.Contexts, operation.After...)
		candidate.Preferences.DesktopIndicators = true
		return handle.CompleteContext(t.Context(), candidate)
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := lifecycleStoreTestRegistry(t, root)
	if len(registry.Contexts) != 2 || !registry.Preferences.DesktopIndicators {
		t.Fatalf("registration not atomically installed: %+v", registry)
	}
	if _, err := LoadLifecycleOperationContext(t.Context(), root, operation.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed intent remains: %v", err)
	}
	blocked, err = BlockedLifecycleContextIDsContext(t.Context(), root)
	if err != nil || len(blocked) != 0 {
		t.Fatalf("completed reservations remain: %v, %v", blocked, err)
	}
	if err := retained.CompleteContext(t.Context(), registry); err == nil {
		t.Fatal("escaped callback handle remained usable")
	}
}

func TestLifecycleStoreReservedResourcesAndUnrelatedWrites(t *testing.T) {
	for _, resource := range []string{"context", "launcher", "application", "sandbox_wildcard"} {
		t.Run(resource, func(t *testing.T) {
			root := backupTestRoot(t)
			operation := lifecycleStoreTestBegin(t, root, lifecycleStoreTestRegister(1))
			other := lifecycleStoreTestRegister(2).After[0]
			switch resource {
			case "context":
				other.ID = operation.After[0].ID
			case "launcher":
				other.Launcher = operation.After[0].Launcher
			case "application":
				other.App.Identity = operation.After[0].App.Identity
			case "sandbox_wildcard":
				other.Launcher = Launcher{Kind: LauncherFlatpak, FlatpakID: "org.example.Sandbox", FlatpakInstallation: FlatpakUser}
				other.App.Identity = operation.After[0].App.Identity
				other.App.Identity.SandboxAppID = "org.example.Sandbox"
			}
			if err := CheckLifecycleOperationConflictsContext(t.Context(), root, []Context{other}); !errors.Is(err, ErrLifecyclePending) {
				t.Fatalf("preflight accepted %s conflict: %v", resource, err)
			}
			_, err := UpdateRegistryContext(t.Context(), root, func(registry *Registry) error {
				return AddContext(registry, other)
			})
			if !errors.Is(err, ErrLifecyclePending) {
				t.Fatalf("writer accepted %s conflict: %v", resource, err)
			}
			registry := lifecycleStoreTestRegistry(t, root)
			if len(registry.Contexts) != 1 {
				t.Fatal("failed conflict wrote registry")
			}
			registry.Contexts[0].Label = "unrelated metadata"
			registry.Preferences.DesktopIndicators = true
			if err := RegistryStoreFor(root).Save(registry); err != nil {
				t.Fatalf("unrelated changes were blocked: %v", err)
			}
		})
	}
}

func TestLifecycleStoreSandboxReservationsAndWindowEpoch(t *testing.T) {
	root := backupTestRoot(t)
	first := lifecycleStoreTestRegister(1)
	first.After[0].Launcher = Launcher{Kind: LauncherFlatpak, FlatpakID: "org.example.One", FlatpakInstallation: FlatpakUser}
	first.After[0].App.Identity.SandboxAppID = "org.example.One"
	first.Targets[0].Identity = first.After[0].App.Identity
	lifecycleStoreTestBegin(t, root, first)
	second := lifecycleStoreTestRegister(2)
	second.After[0].Launcher = Launcher{Kind: LauncherFlatpak, FlatpakID: "org.example.Two", FlatpakInstallation: FlatpakUser}
	second.After[0].App.Identity = first.After[0].App.Identity
	second.After[0].App.Identity.SandboxAppID = "org.example.Two"
	second.Targets[0].Identity = second.After[0].App.Identity
	second.Targets[0].ContainerID = first.Targets[0].ContainerID
	if _, err := BeginLifecycleOperationContext(t.Context(), root, second); !errors.Is(err, ErrLifecyclePending) {
		t.Fatalf("same lifetime/window accepted: %v", err)
	}
	second.CompositorID = "another-lifetime"
	lifecycleStoreTestBegin(t, root, second)
	third := lifecycleStoreTestRegister(3)
	third.After[0].App.Identity = first.After[0].App.Identity
	third.After[0].App.Identity.SandboxAppID = ""
	third.Targets[0].Identity = third.After[0].App.Identity
	if _, err := BeginLifecycleOperationContext(t.Context(), root, third); !errors.Is(err, ErrLifecyclePending) {
		t.Fatalf("wildcard over scoped reservations accepted: %v", err)
	}
}

func TestLifecycleStoreRebindOriginalCASAndRollback(t *testing.T) {
	root := backupTestRoot(t)
	app := lifecycleStoreTestRegister(1).After[0]
	_, err := UpdateRegistryContext(t.Context(), root, func(registry *Registry) error { return AddContext(registry, app) })
	if err != nil {
		t.Fatal(err)
	}
	operation := lifecycleStoreTestRegister(1)
	operation.Kind, operation.Before = LifecycleRebind, []Context{app}
	operation.After[0].Label = "replacement"
	operation.Before[0].Label = "stale original"
	if _, err := BeginLifecycleOperationContext(t.Context(), root, operation); !errors.Is(err, ErrLifecyclePending) {
		t.Fatalf("stale Before accepted: %v", err)
	}
	operation.Before[0] = app
	operation = lifecycleStoreTestBegin(t, root, operation)
	_, err = UpdateRegistryContext(t.Context(), root, func(registry *Registry) error {
		registry.Contexts[1].Label = "racing rename"
		return nil
	})
	if !errors.Is(err, ErrLifecyclePending) {
		t.Fatalf("reserved context changed: %v", err)
	}
	err = WithLifecycleOperationContext(t.Context(), root, operation.ID, func(handle *LifecycleOperationHandle) error {
		updated := handle.Operation
		updated.Phase = LifecycleRollback
		updated.NextAttempt = time.Time{}
		updated.Attempts++
		if err := handle.UpdateContext(t.Context(), updated); err != nil {
			return err
		}
		return handle.CompleteContext(t.Context(), handle.Registry)
	})
	if err != nil {
		t.Fatal(err)
	}
	if registry := lifecycleStoreTestRegistry(t, root); !reflect.DeepEqual(registry.Contexts[1], app) {
		t.Fatal("rollback changed original context")
	}
}

func lifecycleStoreTestRebind(t *testing.T, root string, number int) LifecycleOperation {
	t.Helper()
	operation := lifecycleStoreTestRegister(number)
	before := operation.After[0]
	if _, err := UpdateRegistryContext(t.Context(), root, func(registry *Registry) error { return AddContext(registry, before) }); err != nil {
		t.Fatal(err)
	}
	desired := lifecycleStoreTestRegister(number + 1)
	after := desired.After[0]
	after.ID = before.ID
	operation.Kind = LifecycleRebind
	operation.Before, operation.After = []Context{before}, []Context{after}
	operation.Targets[0].HadMark, operation.Targets[0].WantMark = true, false
	target := desired.Targets[0]
	target.ContextID = before.ID
	operation.Targets = append(operation.Targets, target)
	return operation
}

func TestLifecycleStoreRegistrationGuardsDirectTerminalCreation(t *testing.T) {
	root := backupTestRoot(t)
	operation := lifecycleStoreTestBegin(t, root, lifecycleStoreTestRegister(1))
	value := validRegistry().Contexts[0]
	value.ID = operation.After[0].ID
	value.Launcher.Session = "new-terminal"
	create := func() error {
		_, err := UpdateRegistryWithTerminalCreationContext(t.Context(), root, func(registry *Registry) error { return AddContext(registry, value) },
			func() (ContextID, time.Time, bool) { return value.ID, time.Now().UTC(), true })
		return err
	}
	if err := create(); !errors.Is(err, ErrLifecyclePending) {
		t.Fatalf("terminal creation reused pending registration ID: %v", err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var count int
	if err := database.db.QueryRow("SELECT count(*) FROM terminal_activity").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed creation left activity: %d, %v", count, err)
	}
	if registry := lifecycleStoreTestRegistry(t, root); len(registry.Contexts) != 1 {
		t.Fatal("failed creation left registry context")
	}
	value.ID = lifecycleStoreTestRegister(2).After[0].ID
	if err := create(); err != nil {
		t.Fatalf("unrelated terminal creation was blocked: %v", err)
	}
	if err := database.db.QueryRow("SELECT count(*) FROM terminal_activity WHERE context_id = ?", value.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("unrelated terminal creation lost activity: %d, %v", count, err)
	}
}

func TestLifecycleStoreRebindReservesBothApplicationIdentities(t *testing.T) {
	root := backupTestRoot(t)
	operation := lifecycleStoreTestBegin(t, root, lifecycleStoreTestRebind(t, root, 2))
	for _, side := range [][]Context{operation.Before, operation.After} {
		other := side[0]
		other.ID = lifecycleStoreTestRegister(9).After[0].ID
		if err := CheckLifecycleOperationConflictsContext(t.Context(), root, []Context{other}); !errors.Is(err, ErrLifecyclePending) {
			t.Fatalf("rebind identity was not reserved: %v", err)
		}
	}
	if err := WithLifecycleOperationContext(t.Context(), root, operation.ID, func(handle *LifecycleOperationHandle) error {
		candidate := handle.Registry
		index, err := ResolveContext(candidate, string(operation.Before[0].ID))
		if err != nil {
			return err
		}
		candidate.Contexts[index] = operation.After[0]
		return handle.CompleteContext(t.Context(), candidate)
	}); err != nil {
		t.Fatal(err)
	}
	registry := lifecycleStoreTestRegistry(t, root)
	index, err := ResolveContext(registry, string(operation.Before[0].ID))
	if err != nil || !reflect.DeepEqual(registry.Contexts[index], operation.After[0]) {
		t.Fatalf("rebind desired context was not installed: %v", err)
	}
	former := operation.Before[0]
	former.ID = lifecycleStoreTestRegister(9).After[0].ID
	if _, err := UpdateRegistryContext(t.Context(), root, func(registry *Registry) error { return AddContext(registry, former) }); err != nil {
		t.Fatalf("completed rebind retained former identity: %v", err)
	}
}

func TestLifecycleStoreLostAcknowledgementAndMissingCompletionCAS(t *testing.T) {
	root := backupTestRoot(t)
	previous := executeStateCommit
	t.Cleanup(func() { executeStateCommit = previous })
	executeStateCommit = func(tx *stateWriteTransaction) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		return errors.New("lost acknowledgement")
	}
	operation, err := BeginLifecycleOperationContext(t.Context(), root, lifecycleStoreTestRegister(1))
	var uncertain *statefile.CommitOutcomeUnknownError
	if !errors.As(err, &uncertain) {
		t.Fatalf("expected uncertain begin: %v", err)
	}
	executeStateCommit = previous
	loaded, err := LoadLifecycleOperationContext(t.Context(), root, operation.ID)
	if err != nil || !reflect.DeepEqual(loaded, operation) {
		t.Fatalf("cannot resolve exact begin UUID: %v", err)
	}
	err = WithLifecycleOperationContext(t.Context(), root, operation.ID, func(handle *LifecycleOperationHandle) error {
		candidate := handle.Registry
		candidate.Contexts = append(candidate.Contexts, operation.After...)
		candidate.Preferences.DesktopIndicators = true
		executeStateCommit = func(tx *stateWriteTransaction) error {
			if err := tx.Commit(); err != nil {
				return err
			}
			return errors.New("lost acknowledgement")
		}
		err := handle.CompleteContext(t.Context(), candidate)
		executeStateCommit = previous
		if !errors.As(err, &uncertain) {
			return fmt.Errorf("expected uncertain completion: %w", err)
		}
		if err := handle.CompleteContext(t.Context(), candidate); !errors.Is(err, ErrLifecyclePending) && !errors.Is(err, ErrRegistryConflict) {
			return fmt.Errorf("missing intent authorized repeat commit: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if registry := lifecycleStoreTestRegistry(t, root); len(registry.Contexts) != 2 {
		t.Fatal("uncertain completion reverted commit")
	}
}

func TestLifecycleStoreHandleSnapshotCASAndNoExternalTransaction(t *testing.T) {
	root := backupTestRoot(t)
	operation := lifecycleStoreTestBegin(t, root, lifecycleStoreTestRegister(1))
	err := WithLifecycleOperationContext(t.Context(), root, operation.ID, func(handle *LifecycleOperationHandle) error {
		operation := handle.Operation
		operation.After[0].Label = "tamper immutable intent"
		if err := handle.UpdateContext(t.Context(), operation); err == nil {
			return errors.New("immutable intent update accepted")
		}
		// A separate writer can commit during the callback: it holds no SQL transaction.
		database, err := openStateDatabase(t.Context(), root, false)
		if err != nil {
			return err
		}
		defer database.Close()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if _, err := database.db.ExecContext(ctx, "UPDATE state_meta SET registry_revision = registry_revision + 1"); err != nil {
			return err
		}
		candidate := handle.Registry
		candidate.Contexts = append(candidate.Contexts, handle.original.After...)
		candidate.Preferences.DesktopIndicators = true
		if err := handle.CompleteContext(ctx, candidate); !errors.Is(err, ErrRegistryConflict) {
			return fmt.Errorf("registry CAS failed: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLifecycleOperationContext(t.Context(), root, operation.ID); err != nil {
		t.Fatal("failed CAS removed intent", err)
	}
}

func TestLifecycleStoreBoundedPagesAndMissingRootReads(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	if operations, next, err := ListLifecycleOperationsContext(t.Context(), root, "", 2); err != nil || len(operations) != 0 || next != "" {
		t.Fatalf("missing inventory: %v", err)
	}
	if _, err := BlockedLifecycleContextIDsContext(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	if _, err := LifecycleReferencesDesktopApprovalContext(t.Context(), root, "/missing"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read created missing root: %v", err)
	}
	const total = MaxLifecycleOperationPage + 2
	for number := 1; number <= total; number++ {
		lifecycleStoreTestBegin(t, root, lifecycleStoreTestRegister(number))
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		operations, next, err := ListLifecycleOperationsContext(t.Context(), root, cursor, 2)
		if err != nil || len(operations) > 2 {
			t.Fatalf("page failed: %v", err)
		}
		for _, operation := range operations {
			if seen[operation.ID] {
				t.Fatal("duplicate operation across pages")
			}
			seen[operation.ID] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != total {
		t.Fatalf("incomplete pagination: %v", seen)
	}
	if _, _, err := ListLifecycleOperationsContext(t.Context(), root, "", MaxLifecycleOperationPage+1); err == nil {
		t.Fatal("unbounded page accepted")
	}
}

func TestLifecycleStoreApprovalReferencesAndCommitValidation(t *testing.T) {
	for _, missingAtBegin := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing_at_begin_%t", missingAtBegin), func(t *testing.T) {
			root := backupTestRoot(t)
			operation := lifecycleStoreTestRegister(1)
			launcher := &operation.After[0].Launcher
			launcher.DesktopOrigin = DesktopEntryUser
			data := []byte("[Desktop Entry]\nType=Application\nName=Fixture\nExec=/usr/bin/true\n")
			launcher.DesktopEntrySHA256 = sha256Hex(data)
			launcher.ApprovedDesktopPath = filepath.Join(root, desktopApprovalDirectory, "fixture.desktop")
			if !missingAtBegin {
				if err := statefile.CreatePrivateFileContext(t.Context(), filepath.Dir(launcher.ApprovedDesktopPath), filepath.Base(launcher.ApprovedDesktopPath), data); err != nil {
					t.Fatal(err)
				}
			}
			stored, err := BeginLifecycleOperationContext(t.Context(), root, operation)
			if missingAtBegin {
				if err == nil {
					t.Fatal("missing snapshot allowed durable intent")
				}
				if _, err := LoadLifecycleOperationContext(t.Context(), root, operation.ID); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed approval validation left intent: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if referenced, err := LifecycleReferencesDesktopApprovalContext(t.Context(), root, launcher.ApprovedDesktopPath); err != nil || !referenced {
				t.Fatalf("pending approval reference lost: %v", err)
			}
			if err := os.WriteFile(launcher.ApprovedDesktopPath, []byte("changed snapshot"), 0o600); err != nil {
				t.Fatal(err)
			}
			err = WithLifecycleOperationContext(t.Context(), root, stored.ID, func(handle *LifecycleOperationHandle) error {
				candidate := handle.Registry
				candidate.Contexts = append(candidate.Contexts, stored.After...)
				candidate.Preferences.DesktopIndicators = true
				return handle.CompleteContext(t.Context(), candidate)
			})
			if err == nil {
				t.Fatal("changed snapshot passed completion")
			}
			if registry := lifecycleStoreTestRegistry(t, root); len(registry.Contexts) != 1 {
				t.Fatal("invalid approval was installed")
			}
			if _, err := LoadLifecycleOperationContext(t.Context(), root, stored.ID); err != nil {
				t.Fatalf("failed completion lost intent: %v", err)
			}
			if err := WithLifecycleOperationContext(t.Context(), root, stored.ID, func(handle *LifecycleOperationHandle) error {
				rollback := handle.Operation
				rollback.Phase = LifecycleRollback
				if err := handle.UpdateContext(t.Context(), rollback); err != nil {
					return err
				}
				return handle.CompleteContext(t.Context(), handle.Registry)
			}); err != nil {
				t.Fatalf("rollback unnecessarily required uninstalled approval: %v", err)
			}
			if referenced, err := LifecycleReferencesDesktopApprovalContext(t.Context(), root, launcher.ApprovedDesktopPath); err != nil || referenced {
				t.Fatalf("completed rollback retained approval: %v", err)
			}
		})
	}
}

func TestLifecycleStoreMissingOperationCannotCompleteRollback(t *testing.T) {
	root := backupTestRoot(t)
	operation := lifecycleStoreTestBegin(t, root, lifecycleStoreTestRegister(1))
	err := WithLifecycleOperationContext(t.Context(), root, operation.ID, func(handle *LifecycleOperationHandle) error {
		rollback := handle.Operation
		rollback.Phase = LifecycleRollback
		if err := handle.UpdateContext(t.Context(), rollback); err != nil {
			return err
		}
		database, err := openStateDatabase(t.Context(), root, false)
		if err != nil {
			return err
		}
		defer database.Close()
		if _, err := database.db.Exec("DELETE FROM lifecycle_operations WHERE id = ?", operation.ID); err != nil {
			return err
		}
		if err := handle.CompleteContext(t.Context(), handle.Registry); !errors.Is(err, ErrLifecyclePending) {
			return fmt.Errorf("missing intent authorized rollback: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleStoreFailedReservationRollsBackRegistryRevision(t *testing.T) {
	root := backupTestRoot(t)
	operation := lifecycleStoreTestBegin(t, root, lifecycleStoreTestRegister(1))
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var before, after int64
	if err := database.db.QueryRow("SELECT registry_revision FROM state_meta WHERE id = 1").Scan(&before); err != nil {
		t.Fatal(err)
	}
	_, err = UpdateRegistryContext(t.Context(), root, func(registry *Registry) error { return AddContext(registry, operation.After[0]) })
	if !errors.Is(err, ErrLifecyclePending) {
		t.Fatalf("expected reservation failure: %v", err)
	}
	if err := database.db.QueryRow("SELECT registry_revision FROM state_meta WHERE id = 1").Scan(&after); err != nil || before != after {
		t.Fatalf("reservation failure changed revision: %d -> %d: %v", before, after, err)
	}
}
