package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// CheckLifecycleOperationConflictsContext is a read-only preflight for external
// effects. Call it while holding the registry lock, which Begin also respects.
// It checks context IDs, launcher identities, and overlapping application
// identities; it never acquires a registry lock.
func CheckLifecycleOperationConflictsContext(ctx context.Context, root string, contexts []Context) error {
	for _, value := range contexts {
		if err := value.Validate(); err != nil {
			return err
		}
	}
	database, err := openStateDatabase(ctx, root, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer database.Close()
	exists, err := lifecycleTablesExist(ctx, database.db)
	if err != nil || !exists {
		return err
	}
	return checkLifecycleResources(ctx, database.db, lifecycleContextResources(contexts), "")
}

// BlockedLifecycleContextIDsContext includes every reserved context, whether
// its operation is due, deferred, or explicitly blocked. It projects only the
// indexed reservation keys and does not decode all operation payloads.
func BlockedLifecycleContextIDsContext(ctx context.Context, root string) (map[ContextID]struct{}, error) {
	result := make(map[ContextID]struct{})
	database, err := openStateDatabase(ctx, root, false)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	defer database.Close()
	exists, err := lifecycleTablesExist(ctx, database.db)
	if err != nil || !exists {
		return result, err
	}
	rows, err := database.db.QueryContext(ctx, "SELECT resource_key FROM lifecycle_reservations WHERE resource_kind = 'context'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id ContextID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if err := id.Validate(); err != nil {
			return nil, err
		}
		result[id] = struct{}{}
	}
	return result, rows.Err()
}

// LifecycleReferencesDesktopApprovalContext retains snapshots referenced by
// either side of any pending intent, including blocked/rollback operations.
// Callers deleting a snapshot must hold the registry lock across this check
// and the filesystem effect. No SQL transaction encompasses that effect.
func LifecycleReferencesDesktopApprovalContext(ctx context.Context, root, path string) (bool, error) {
	database, err := openStateDatabase(ctx, root, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer database.Close()
	exists, err := lifecycleTablesExist(ctx, database.db)
	if err != nil || !exists {
		return false, err
	}
	rows, err := database.db.QueryContext(ctx, "SELECT id, encoding_version, revision, payload FROM lifecycle_operations ORDER BY id")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		operation, err := scanLifecycleOperation(rows)
		if err != nil {
			return false, err
		}
		for _, contexts := range [][]Context{operation.Before, operation.After} {
			for _, value := range contexts {
				if path != "" && value.Launcher.ApprovedDesktopPath == path {
					return true, nil
				}
			}
		}
	}
	return false, rows.Err()
}

func scanLifecycleOperation(rows *sql.Rows) (LifecycleOperation, error) {
	var id string
	var version int
	var revision int64
	var payload []byte
	if err := rows.Scan(&id, &version, &revision, &payload); err != nil {
		return LifecycleOperation{}, err
	}
	return decodeLifecycleOperation(id, version, revision, payload)
}

func checkLifecycleRegistryDelta(ctx context.Context, tx stateTransaction, delta registryDelta) error {
	exists, err := lifecycleTablesExist(ctx, tx)
	if err != nil || !exists {
		return err
	}
	resources := make(map[lifecycleResource]struct{})
	add := func(value Context) {
		for resource := range lifecycleContextResources([]Context{value}) {
			resources[resource] = struct{}{}
		}
	}
	changed := make(map[ContextID]struct{}, len(delta.upserts)+len(delta.deletes))
	for _, write := range delta.upserts {
		var value Context
		if err := decodeDatabasePayload("context row", write.payload, &value); err != nil {
			return err
		}
		add(value)
		changed[write.id] = struct{}{}
	}
	for _, id := range delta.deletes {
		changed[id] = struct{}{}
	}
	for id := range changed {
		resources[lifecycleResource{kind: "context", key: string(id)}] = struct{}{}
		var payload []byte
		err := tx.QueryRowContext(ctx, "SELECT payload FROM contexts WHERE id = ?", id).Scan(&payload)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		var value Context
		if err := decodeDatabasePayload("context row", payload, &value); err != nil {
			return err
		}
		add(value)
	}
	// Pure ordinal maintenance and global presentation preferences do not alter
	// any reserved context or identity. They remain available during a saga.
	return checkLifecycleResources(ctx, tx, resources, delta.lifecycleOperationID)
}

// validateLifecycleOperationsQuery validates the entire optional document set
// and its derived reservations from the same read snapshot used by backup.
func validateLifecycleOperationsQuery(ctx context.Context, queryer stateQueryer, registry Registry) error {
	exists, err := lifecycleTablesExist(ctx, queryer)
	if err != nil || !exists {
		return err
	}
	if err := validateStateBackupPayloadBudget(ctx, queryer, "lifecycle_operations"); err != nil {
		return err
	}
	rows, err := queryer.QueryContext(ctx, "SELECT id, encoding_version, revision, payload FROM lifecycle_operations ORDER BY id")
	if err != nil {
		return err
	}
	expected := make(map[lifecycleResource]string)
	applicationScopes := make(map[string]map[string]string)
	for rows.Next() {
		operation, err := scanLifecycleOperation(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		if err := verifyLifecycleOriginals(registry, operation); err != nil {
			_ = rows.Close()
			return err
		}
		if _, err := lifecycleForwardCandidate(registry, operation); err != nil {
			_ = rows.Close()
			return err
		}
		for resource := range lifecycleOperationResources(operation) {
			if owner, duplicate := expected[resource]; duplicate && owner != operation.ID {
				_ = rows.Close()
				return errors.New("lifecycle operations have conflicting reservations")
			}
			if resource.kind == "application" {
				scopes := applicationScopes[resource.key]
				if scopes == nil {
					scopes = make(map[string]string)
					applicationScopes[resource.key] = scopes
				}
				overlaps := scopes[""] != "" && scopes[""] != operation.ID
				if resource.scope == "" {
					for _, owner := range scopes {
						if owner != operation.ID {
							overlaps = true
							break
						}
					}
				}
				if overlaps {
					_ = rows.Close()
					return errors.New("lifecycle operation application reservations overlap")
				}
				scopes[resource.scope] = operation.ID
			}
			expected[resource] = operation.ID
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	rows, err = queryer.QueryContext(ctx, "SELECT resource_kind, resource_key, resource_scope, operation_id FROM lifecycle_reservations")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var resource lifecycleResource
		var owner string
		if err := rows.Scan(&resource.kind, &resource.key, &resource.scope, &owner); err != nil {
			return err
		}
		if expectedOwner, exists := expected[resource]; !exists || expectedOwner != owner {
			return errors.New("lifecycle reservation does not match its operation")
		}
		delete(expected, resource)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(expected) != 0 {
		return fmt.Errorf("lifecycle operations are missing %d reservations", len(expected))
	}
	return nil
}
