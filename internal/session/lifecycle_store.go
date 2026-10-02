package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strconv"
	"time"

	"github.com/marang/sway-session/internal/statefile"
)

const lifecycleOperationsSQL = `CREATE TABLE lifecycle_operations (
	id TEXT PRIMARY KEY,
	encoding_version INTEGER NOT NULL,
	revision INTEGER NOT NULL CHECK (revision > 0),
	payload BLOB NOT NULL CHECK (length(payload) <= 16777216)
) STRICT`

const lifecycleReservationsSQL = `CREATE TABLE lifecycle_reservations (
	resource_kind TEXT NOT NULL CHECK (resource_kind IN ('context', 'launcher', 'application', 'window')),
	resource_key TEXT NOT NULL,
	resource_scope TEXT NOT NULL,
	operation_id TEXT NOT NULL REFERENCES lifecycle_operations(id) ON DELETE CASCADE,
	PRIMARY KEY (resource_kind, resource_key, resource_scope)
) STRICT`

const lifecycleReservationIndexSQL = `CREATE INDEX lifecycle_reservations_operation ON lifecycle_reservations (operation_id)`

type lifecycleResource struct{ kind, key, scope string }

func lifecycleTablesExist(ctx context.Context, queryer stateQueryer) (bool, error) {
	var count int
	if err := queryer.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name IN ('lifecycle_operations', 'lifecycle_reservations')`).Scan(&count); err != nil {
		return false, err
	}
	if count != 0 && count != 2 {
		return false, errors.New("lifecycle operation schema is incomplete")
	}
	return count == 2, nil
}

func createLifecycleTables(ctx context.Context, tx stateTransaction) error {
	exists, err := lifecycleTablesExist(ctx, tx)
	if err != nil || exists {
		return err
	}
	for _, statement := range []string{lifecycleOperationsSQL, lifecycleReservationsSQL, lifecycleReservationIndexSQL} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// BeginLifecycleOperationContext serializes approval reference checks and the
// durable decision under the registry lock. The caller supplies a fresh UUID;
// after an uncertain commit, load that exact UUID before any external effects.
func BeginLifecycleOperationContext(ctx context.Context, root string, operation LifecycleOperation) (LifecycleOperation, error) {
	if ctx == nil {
		return LifecycleOperation{}, errors.New("lifecycle operation context is nil")
	}
	if operation.Revision != 0 || operation.Phase != LifecycleForward {
		return LifecycleOperation{}, errors.New("begin requires a fresh forward lifecycle operation")
	}
	if operation.Before == nil {
		operation.Before = []Context{}
	}
	if operation.After == nil {
		operation.After = []Context{}
	}
	if operation.Targets == nil {
		operation.Targets = []LifecycleWindowTarget{}
	}
	if operation.UpdatedAt.IsZero() {
		operation.UpdatedAt = time.Now().UTC()
	}
	operation.Revision = 1
	operation, err := cloneLifecycleOperation(operation)
	if err != nil {
		return LifecycleOperation{}, err
	}
	if err := operation.Validate(); err != nil {
		return LifecycleOperation{}, err
	}
	database, err := openStateDatabase(ctx, root, true)
	if err != nil {
		return LifecycleOperation{}, err
	}
	defer database.Close()
	err = WithRegistryLockContext(ctx, root, func(*statefile.LockedPrivateDirectory) error {
		registry, revision, err := loadRegistrySnapshotDatabase(ctx, database)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := verifyLifecycleBeginOriginals(registry, operation); err != nil {
			return err
		}
		if _, err := lifecycleForwardCandidate(registry, operation); err != nil {
			return err
		}
		for _, value := range operation.After {
			if err := validateUnreferencedDesktopApproval(ctx, root, registry, value.Launcher); err != nil {
				return err
			}
		}
		var purgeDelta registryDelta
		if operation.Kind == LifecyclePurge {
			candidate, err := cloneLifecycleRegistry(registry)
			if err != nil {
				return err
			}
			index, err := ResolveContext(candidate, string(operation.Before[0].ID))
			if err != nil {
				return err
			}
			candidate.Contexts = append(candidate.Contexts[:index], candidate.Contexts[index+1:]...)
			purgeDelta, err = prepareRegistryDelta(ctx, database, candidate, revision)
			if err != nil {
				return err
			}
			purgeDelta.lifecycleOperationID = operation.ID
		}
		payload, err := marshalDatabasePayload("lifecycle operation", operation)
		if err != nil {
			return err
		}
		tx, err := beginStateWrite(ctx, database)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := createLifecycleTables(ctx, tx); err != nil {
			return err
		}
		var currentRevision int64
		if err := tx.QueryRowContext(ctx, "SELECT registry_revision FROM state_meta WHERE id = 1").Scan(&currentRevision); err != nil {
			return err
		}
		if currentRevision != revision {
			return ErrRegistryConflict
		}
		var duplicate int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM lifecycle_operations WHERE id = ?", operation.ID).Scan(&duplicate); err != nil {
			return err
		}
		if duplicate != 0 {
			return ErrLifecycleOperationConflict
		}
		resources := lifecycleOperationResources(operation)
		if err := checkLifecycleResources(ctx, tx, resources, ""); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO lifecycle_operations(id, encoding_version, revision, payload) VALUES (?, ?, ?, ?)", operation.ID, operation.Version, operation.Revision, payload); err != nil {
			return err
		}
		for resource := range resources {
			if _, err := tx.ExecContext(ctx, "INSERT INTO lifecycle_reservations(resource_kind, resource_key, resource_scope, operation_id) VALUES (?, ?, ?, ?)", resource.kind, resource.key, resource.scope, operation.ID); err != nil {
				return err
			}
		}
		if operation.Kind == LifecyclePurge {
			if err := applyRegistryDeltaTx(ctx, tx, purgeDelta); err != nil {
				return err
			}
		}
		return commitStateWrite(ctx, tx)
	})
	return operation, err
}

func LoadLifecycleOperationContext(ctx context.Context, root, id string) (LifecycleOperation, error) {
	if err := ContextID(id).Validate(); err != nil {
		return LifecycleOperation{}, err
	}
	database, err := openStateDatabase(ctx, root, false)
	if err != nil {
		return LifecycleOperation{}, err
	}
	defer database.Close()
	exists, err := lifecycleTablesExist(ctx, database.db)
	if err != nil {
		return LifecycleOperation{}, err
	}
	if !exists {
		return LifecycleOperation{}, os.ErrNotExist
	}
	return loadLifecycleOperation(ctx, database.db, id)
}

func loadLifecycleOperation(ctx context.Context, queryer stateQueryer, id string) (LifecycleOperation, error) {
	var version int
	var revision int64
	var payload []byte
	err := queryer.QueryRowContext(ctx, "SELECT encoding_version, revision, payload FROM lifecycle_operations WHERE id = ?", id).Scan(&version, &revision, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return LifecycleOperation{}, os.ErrNotExist
	}
	if err != nil {
		return LifecycleOperation{}, err
	}
	return decodeLifecycleOperation(id, version, revision, payload)
}

func decodeLifecycleOperation(id string, version int, revision int64, payload []byte) (LifecycleOperation, error) {
	var operation LifecycleOperation
	if version != LifecycleOperationVersion {
		return operation, &UnsupportedVersionError{Document: "lifecycle operation row", Got: version, Want: LifecycleOperationVersion}
	}
	if err := decodeDatabasePayload("lifecycle operation", payload, &operation); err != nil {
		return operation, err
	}
	if operation.ID != id || operation.Version != version || operation.Revision != revision || revision <= 0 {
		return operation, errors.New("lifecycle operation row does not match its payload")
	}
	return operation, operation.Validate()
}

// ListLifecycleOperationsContext returns one bounded page, including blocked
// operations. An empty next cursor means the scan is exhausted. Scheduling and
// backoff belong to the reconciler, not this read-only inventory operation.
func ListLifecycleOperationsContext(ctx context.Context, root, afterID string, limit int) ([]LifecycleOperation, string, error) {
	if limit <= 0 || limit > MaxLifecycleOperationPage {
		return nil, "", errors.New("invalid lifecycle operation page size")
	}
	if afterID != "" {
		if err := ContextID(afterID).Validate(); err != nil {
			return nil, "", err
		}
	}
	database, err := openStateDatabase(ctx, root, false)
	if errors.Is(err, os.ErrNotExist) {
		return []LifecycleOperation{}, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	defer database.Close()
	exists, err := lifecycleTablesExist(ctx, database.db)
	if err != nil {
		return nil, "", err
	}
	if !exists {
		return []LifecycleOperation{}, "", nil
	}
	rows, err := database.db.QueryContext(ctx, "SELECT id, encoding_version, revision, payload FROM lifecycle_operations WHERE id > ? ORDER BY id LIMIT ?", afterID, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	result := make([]LifecycleOperation, 0, limit)
	for rows.Next() {
		if len(result) == limit {
			return result, result[len(result)-1].ID, nil
		}
		var id string
		var version int
		var revision int64
		var payload []byte
		if err := rows.Scan(&id, &version, &revision, &payload); err != nil {
			return nil, "", err
		}
		operation, err := decodeLifecycleOperation(id, version, revision, payload)
		if err != nil {
			return nil, "", err
		}
		result = append(result, operation)
	}
	return result, "", rows.Err()
}

func WithLifecycleOperationContext(ctx context.Context, root, id string, action func(*LifecycleOperationHandle) error) error {
	if action == nil {
		return errors.New("lifecycle operation callback is nil")
	}
	if err := ContextID(id).Validate(); err != nil {
		return err
	}
	database, err := openStateDatabase(ctx, root, false)
	if err != nil {
		return err
	}
	defer database.Close()
	return WithRegistryLockContext(ctx, root, func(*statefile.LockedPrivateDirectory) error {
		exists, err := lifecycleTablesExist(ctx, database.db)
		if err != nil {
			return err
		}
		if !exists {
			return os.ErrNotExist
		}
		operation, err := loadLifecycleOperation(ctx, database.db, id)
		if err != nil {
			return err
		}
		registry, revision, err := loadRegistrySnapshotDatabase(ctx, database)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		publicOperation, err := cloneLifecycleOperation(operation)
		if err != nil {
			return err
		}
		publicRegistry, err := cloneLifecycleRegistry(registry)
		if err != nil {
			return err
		}
		handle := &LifecycleOperationHandle{Operation: publicOperation, Registry: publicRegistry, database: database, original: operation, registry: registry, registryRevision: revision, root: root, active: true}
		defer func() { handle.active = false }()
		return action(handle)
	})
}

func (handle *LifecycleOperationHandle) UpdateContext(ctx context.Context, operation LifecycleOperation) error {
	if handle == nil || !handle.active {
		return errors.New("lifecycle operation handle is inactive")
	}
	previous := handle.original
	expected := previous
	expected.Phase, expected.Attempts, expected.NextAttempt = operation.Phase, operation.Attempts, operation.NextAttempt
	expected.Reason, expected.UpdatedAt, expected.Blocked = operation.Reason, operation.UpdatedAt, operation.Blocked
	if !reflect.DeepEqual(expected, operation) || operation.Attempts < previous.Attempts || operation.UpdatedAt.Before(previous.UpdatedAt) || previous.Phase == LifecycleRollback && operation.Phase != LifecycleRollback || previous.Revision == math.MaxInt64 {
		return errors.New("lifecycle update changes immutable intent or regresses progress")
	}
	operation.Revision++
	operation, err := cloneLifecycleOperation(operation)
	if err != nil {
		return err
	}
	if err := operation.Validate(); err != nil {
		return err
	}
	payload, err := marshalDatabasePayload("lifecycle operation", operation)
	if err != nil {
		return err
	}
	tx, err := beginStateWrite(ctx, handle.database)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE lifecycle_operations SET revision = ?, payload = ? WHERE id = ? AND revision = ?", operation.Revision, payload, operation.ID, previous.Revision)
	if err != nil {
		return err
	}
	if err := requireLifecycleCAS(result); err != nil {
		return err
	}
	if err := commitStateWrite(ctx, tx); err != nil {
		return err
	}
	handle.original = operation
	handle.Operation, err = cloneLifecycleOperation(operation)
	return err
}

func (handle *LifecycleOperationHandle) CompleteContext(ctx context.Context, candidate Registry) error {
	if handle == nil || !handle.active {
		return errors.New("lifecycle operation handle is inactive")
	}
	operation := handle.original
	if err := verifyLifecycleOriginals(handle.registry, operation); err != nil {
		return err
	}
	expected := handle.registry
	var err error
	if operation.Phase == LifecycleForward {
		expected, err = lifecycleForwardCandidate(handle.registry, operation)
		if err != nil {
			return err
		}
	}
	if err := candidate.Validate(); err != nil {
		return err
	}
	if !reflect.DeepEqual(candidate, expected) {
		return errors.New("lifecycle completion candidate does not match its durable intent")
	}
	if operation.Phase == LifecycleForward {
		for _, value := range operation.After {
			if err := validateUnreferencedDesktopApproval(ctx, handle.root, handle.registry, value.Launcher); err != nil {
				return err
			}
		}
	}
	delta, err := prepareRegistryDelta(ctx, handle.database, candidate, handle.registryRevision)
	if err != nil {
		return err
	}
	delta.lifecycleOperationID = operation.ID
	tx, err := beginStateWrite(ctx, handle.database)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Check the operation before applying any registry mutation. Missing or
	// replaced intent never authorizes rollback to recreate an old context.
	var revision int64
	if err := tx.QueryRowContext(ctx, "SELECT revision FROM lifecycle_operations WHERE id = ?", operation.ID).Scan(&revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLifecycleOperationConflict
		}
		return err
	}
	if revision != operation.Revision {
		return ErrLifecycleOperationConflict
	}
	if err := applyRegistryDeltaTx(ctx, tx, delta); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM lifecycle_operations WHERE id = ? AND revision = ?", operation.ID, operation.Revision)
	if err != nil {
		return err
	}
	if err := requireLifecycleCAS(result); err != nil {
		return err
	}
	if err := commitStateWrite(ctx, tx); err != nil {
		return err
	}
	handle.active = false
	return nil
}

func requireLifecycleCAS(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrLifecycleOperationConflict
	}
	return nil
}

func verifyLifecycleBeginOriginals(registry Registry, operation LifecycleOperation) error {
	if operation.Kind == LifecyclePurge {
		index, err := ResolveContext(registry, string(operation.Before[0].ID))
		if err != nil || !reflect.DeepEqual(registry.Contexts[index], operation.Before[0]) {
			return ErrLifecycleOperationConflict
		}
		return nil
	}
	return verifyLifecycleOriginals(registry, operation)
}

func verifyLifecycleOriginals(registry Registry, operation LifecycleOperation) error {
	if operation.Kind == LifecyclePurge {
		return checkPurgeRegistry(registry, operation)
	}
	current := make(map[ContextID]Context, len(registry.Contexts))
	for _, value := range registry.Contexts {
		current[value.ID] = value
	}
	if operation.Kind == LifecycleRegister {
		for _, value := range operation.After {
			if _, exists := current[value.ID]; exists {
				return ErrLifecycleOperationConflict
			}
		}
		return nil
	}
	for _, value := range operation.Before {
		stored, exists := current[value.ID]
		if !exists || !reflect.DeepEqual(stored, value) {
			return ErrLifecycleOperationConflict
		}
	}
	return nil
}

func lifecycleForwardCandidate(registry Registry, operation LifecycleOperation) (Registry, error) {
	candidate, err := cloneLifecycleRegistry(registry)
	if err != nil {
		return Registry{}, err
	}
	switch operation.Kind {
	case LifecycleRegister:
		candidate.Contexts = append(candidate.Contexts, operation.After...)
		candidate.Preferences.DesktopIndicators = true
	case LifecycleRebind:
		index, err := ResolveContext(candidate, string(operation.Before[0].ID))
		if err != nil {
			return Registry{}, err
		}
		candidate.Contexts[index] = operation.After[0]
	}
	return candidate, candidate.Validate()
}

func cloneLifecycleOperation(operation LifecycleOperation) (LifecycleOperation, error) {
	payload, err := marshalDatabasePayload("lifecycle operation", operation)
	if err != nil {
		return LifecycleOperation{}, err
	}
	var clone LifecycleOperation
	err = decodeDatabasePayload("lifecycle operation", payload, &clone)
	return clone, err
}

func cloneLifecycleRegistry(registry Registry) (Registry, error) {
	// Registry inventory has no per-document payload cap: only each context
	// and operation is bounded, and the database has its existing page budget.
	payload, err := json.Marshal(registry)
	if err != nil {
		return Registry{}, err
	}
	var clone Registry
	err = json.Unmarshal(payload, &clone)
	return clone, err
}

func lifecycleContextResources(contexts []Context) map[lifecycleResource]struct{} {
	result := make(map[lifecycleResource]struct{})
	for _, value := range contexts {
		result[lifecycleResource{kind: "context", key: string(value.ID)}] = struct{}{}
		launcher := value.Launcher.identity()
		result[lifecycleResource{kind: "launcher", key: lifecycleResourceKey(string(launcher.kind), launcher.value)}] = struct{}{}
		if identity, ok := value.Launcher.terminalIdentity(); ok {
			// Reuse the existing launcher kind; its JSON key namespace cannot collide
			// with the two-element launcher key. No schema migration is needed.
			result[lifecycleResource{kind: "launcher", key: lifecycleResourceKey("terminal", string(identity.kind), identity.project)}] = struct{}{}
		}
		if value.App != nil {
			identity := value.App.Identity.primary()
			result[lifecycleResource{kind: "application", key: lifecycleResourceKey(string(identity.protocol), identity.first, identity.second), scope: value.App.Identity.SandboxAppID}] = struct{}{}
		}
	}
	return result
}

func lifecycleOperationResources(operation LifecycleOperation) map[lifecycleResource]struct{} {
	result := lifecycleContextResources(operation.Before)
	for resource := range lifecycleContextResources(operation.After) {
		result[resource] = struct{}{}
	}
	for _, target := range operation.Targets {
		result[lifecycleResource{kind: "window", key: lifecycleResourceKey(operation.CompositorID, strconv.FormatInt(target.ContainerID, 10))}] = struct{}{}
	}
	return result
}

func lifecycleResourceKey(values ...string) string {
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

func checkLifecycleResources(ctx context.Context, queryer stateQueryer, resources map[lifecycleResource]struct{}, owner string) error {
	for resource := range resources {
		var conflict string
		err := queryer.QueryRowContext(ctx, `SELECT operation_id FROM lifecycle_reservations
			WHERE resource_kind = ? AND resource_key = ? AND operation_id <> ?
			AND (resource_scope = ? OR (? = 'application' AND (resource_scope = '' OR ? = ''))) LIMIT 1`,
			resource.kind, resource.key, owner, resource.scope, resource.kind, resource.scope).Scan(&conflict)
		if err == nil {
			return fmt.Errorf("%w: operation %s", ErrLifecycleOperationConflict, conflict)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return nil
}
