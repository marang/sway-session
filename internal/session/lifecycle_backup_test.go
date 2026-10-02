package session

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLifecycleBackupRecoveryRetainsPendingIntents(t *testing.T) {
	for _, reports := range []bool{false, true} {
		t.Run(map[bool]string{false: "lifecycle_only", true: "lifecycle_and_restore"}[reports], func(t *testing.T) {
			root := backupTestRoot(t)
			if reports {
				if err := RestoreReportStoreFor(root).BeginExplicitContext(t.Context(), []RestoreOutcome{restoreTestOutcome("explicit", testContextID)}); err != nil {
					t.Fatal(err)
				}
			}
			rebind := lifecycleStoreTestBegin(t, root, lifecycleStoreTestRebind(t, root, 2))
			register := lifecycleStoreTestBegin(t, root, lifecycleStoreTestRegister(1))
			registryBefore := lifecycleStoreTestRegistry(t, root)
			if err := WithLifecycleOperationContext(t.Context(), root, register.ID, func(handle *LifecycleOperationHandle) error {
				operation := handle.Operation
				operation.Phase, operation.Blocked, operation.Reason = LifecycleRollback, true, "identity_changed"
				return handle.UpdateContext(t.Context(), operation)
			}); err != nil {
				t.Fatal(err)
			}
			register, err := LoadLifecycleOperationContext(t.Context(), root, register.ID)
			if err != nil {
				t.Fatal(err)
			}
			output := backupTestOutput(t)
			if _, err := BackupState(t.Context(), root, output); err != nil {
				t.Fatal(err)
			}
			before := backupTestRead(t, output)
			if _, err := ValidateStateBackup(t.Context(), output); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, backupTestRead(t, output)) {
				t.Fatal("validation changed archived operation")
			}
			recovered := filepath.Join(t.TempDir(), "recovered")
			if _, err := RecoverState(t.Context(), recovered, output, true); err != nil {
				t.Fatal(err)
			}
			for _, expected := range []LifecycleOperation{rebind, register} {
				loaded, err := LoadLifecycleOperationContext(t.Context(), recovered, expected.ID)
				if err != nil || !reflect.DeepEqual(loaded, expected) {
					t.Fatalf("recovered operation differs: %+v, %v", loaded, err)
				}
			}
			blocked, err := BlockedLifecycleContextIDsContext(t.Context(), recovered)
			if err != nil || len(blocked) != 2 {
				t.Fatalf("recovered reservations: %v, %v", blocked, err)
			}
			if registry := lifecycleStoreTestRegistry(t, recovered); !reflect.DeepEqual(registry, registryBefore) {
				t.Fatal("recovery applied or reversed pending effects")
			}
			for _, side := range [][]Context{rebind.Before, rebind.After, register.After} {
				if err := CheckLifecycleOperationConflictsContext(t.Context(), recovered, side); err == nil {
					t.Fatal("application resources unreserved after recovery")
				}
			}
		})
	}
}

func TestLifecycleBackupRejectsUnsupportedOrInconsistentOperations(t *testing.T) {
	for name, statement := range map[string]string{
		"partial_extension":   "DROP TABLE lifecycle_reservations",
		"missing_index":       "DROP INDEX lifecycle_reservations_operation",
		"executable_schema":   "CREATE TRIGGER malicious_lifecycle AFTER DELETE ON lifecycle_operations BEGIN DELETE FROM contexts; END",
		"extra_column":        "ALTER TABLE lifecycle_operations ADD COLUMN unknown TEXT",
		"row_version":         "UPDATE lifecycle_operations SET encoding_version = 2",
		"payload_version":     "UPDATE lifecycle_operations SET payload = CAST(json_set(payload, '$.version', 2) AS BLOB)",
		"payload_revision":    "UPDATE lifecycle_operations SET revision = revision + 1",
		"payload_unknown":     "UPDATE lifecycle_operations SET payload = CAST(json_set(payload, '$.unknown', 1) AS BLOB)",
		"payload_null":        "UPDATE lifecycle_operations SET payload = CAST('null' AS BLOB)",
		"unsupported_kind":    "UPDATE lifecycle_operations SET payload = CAST(json_set(payload, '$.kind', 'purge') AS BLOB)",
		"unsupported_target":  "UPDATE lifecycle_operations SET payload = CAST(json_set(payload, '$.purge', json('{}')) AS BLOB)",
		"missing_reservation": "DELETE FROM lifecycle_reservations WHERE resource_kind = 'application'",
		"extra_reservation":   "INSERT INTO lifecycle_reservations SELECT resource_kind, 'extra', resource_scope, operation_id FROM lifecycle_reservations WHERE resource_kind = 'window'",
		"wrong_scope":         "UPDATE lifecycle_reservations SET resource_scope = 'wrong' WHERE resource_kind = 'context'",
	} {
		t.Run(name, func(t *testing.T) {
			root := backupTestRoot(t)
			lifecycleStoreTestBegin(t, root, lifecycleStoreTestRegister(1))
			backupTestMutate(t, root, statement)
			output := backupTestOutput(t)
			if _, err := BackupState(t.Context(), root, output); err == nil {
				t.Fatal("invalid lifecycle state published")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("invalid output was exposed: %v", err)
			}
		})
	}
}

func TestLifecycleBackupRejectsDesiredApplicationReusedInCorruptRegistry(t *testing.T) {
	root := backupTestRoot(t)
	operation := lifecycleStoreTestBegin(t, root, lifecycleStoreTestRebind(t, root, 2))
	value := operation.After[0]
	value.ID = lifecycleStoreTestRegister(1).After[0].ID
	payload, err := marshalDatabasePayload("test", value)
	if err != nil {
		t.Fatal(err)
	}
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.db.Exec("INSERT INTO contexts(id, ordinal, encoding_version, payload) VALUES (?, 99, ?, ?)", value.ID, ContextsSchemaVersion, payload); err != nil {
		t.Fatal(err)
	}
	// The registry alone is valid: only the pending desired identity makes
	// this extra context inconsistent with the complete archived state.
	if _, err := loadRegistryDatabase(t.Context(), database); err != nil {
		t.Fatalf("fixture must contain a valid registry: %v", err)
	}
	if _, err := BackupState(t.Context(), root, backupTestOutput(t)); err == nil {
		t.Fatal("corrupt reuse of pending rebind application accepted")
	}
}
