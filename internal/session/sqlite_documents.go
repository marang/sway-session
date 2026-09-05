package session

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"github.com/marang/sway-session/internal/statefile"
)

var ErrRegistryConflict = errors.New("context registry changed concurrently")

// RegistryStore keeps validated registry rows in the sway-session state
// database. The directory lock serializes external-effect workflows, while
// every database write itself remains a short SQLite transaction.
type RegistryStore struct {
	root string
}

func RegistryStoreFor(root string) RegistryStore {
	return RegistryStore{root: root}
}

func (store RegistryStore) Save(value Registry) error {
	return store.SaveContext(context.Background(), value)
}

func (store RegistryStore) SaveContext(ctx context.Context, value Registry) error {
	if ctx == nil {
		return errors.New("registry save context is nil")
	}
	if err := value.Validate(); err != nil {
		return fmt.Errorf("validate registry: %w", err)
	}
	database, err := openStateDatabase(ctx, store.root, true)
	if err != nil {
		return err
	}
	defer database.Close()
	return WithRegistryLockContext(ctx, store.root, func(*statefile.LockedPrivateDirectory) error {
		_, revision, err := loadRegistrySnapshotDatabase(ctx, database)
		if errors.Is(err, os.ErrNotExist) {
			revision = 0
		} else if err != nil {
			return err
		}
		return saveRegistryDatabase(ctx, database, value, revision)
	})
}

func (store RegistryStore) LoadInto(target *Registry) error {
	return store.LoadIntoContext(context.Background(), target)
}

func (store RegistryStore) LoadIntoContext(ctx context.Context, target *Registry) error {
	if ctx == nil {
		return errors.New("registry load context is nil")
	}
	if target == nil {
		return errors.New("registry target is nil")
	}
	database, err := openStateDatabase(ctx, store.root, false)
	if err != nil {
		return err
	}
	defer database.Close()
	candidate, err := loadRegistryDatabase(ctx, database)
	if err != nil {
		return err
	}
	*target = candidate
	return nil
}

func (store RegistryStore) LoadSnapshotInto(target *Registry) error {
	return store.LoadIntoContext(context.Background(), target)
}

// LoadIfChangedContext reads only the registry revision when the caller's
// cached snapshot is still current. A changed result contains the complete
// validated registry from the same SQLite read transaction.
func (store RegistryStore) LoadIfChangedContext(
	ctx context.Context,
	knownRevision int64,
	knownPresent bool,
) (Registry, int64, bool, bool, error) {
	if ctx == nil {
		return emptyRegistry(), 0, false, false, errors.New("registry load context is nil")
	}
	database, err := openStateDatabase(ctx, store.root, false)
	if err != nil {
		return emptyRegistry(), 0, false, false, err
	}
	defer database.Close()
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return emptyRegistry(), 0, false, false, fmt.Errorf("begin registry revision snapshot: %w", err)
	}
	defer tx.Rollback()
	var presentInteger int
	var revision int64
	if err := tx.QueryRowContext(ctx, "SELECT registry_present, registry_revision FROM state_meta WHERE id = 1").Scan(&presentInteger, &revision); err != nil {
		return emptyRegistry(), 0, false, false, fmt.Errorf("load registry revision: %w", err)
	}
	present := presentInteger == 1
	changed := revision != knownRevision || present != knownPresent
	if !changed || !present {
		if err := tx.Commit(); err != nil {
			return emptyRegistry(), 0, false, false, fmt.Errorf("finish registry revision snapshot: %w", err)
		}
		return emptyRegistry(), revision, present, changed, nil
	}
	registry, loadedRevision, err := loadRegistrySnapshotQuery(ctx, tx)
	if err != nil {
		return emptyRegistry(), 0, false, false, err
	}
	if loadedRevision != revision {
		return emptyRegistry(), 0, false, false, errors.New("registry revision changed inside one read snapshot")
	}
	if err := tx.Commit(); err != nil {
		return emptyRegistry(), 0, false, false, fmt.Errorf("finish changed registry snapshot: %w", err)
	}
	return registry, revision, true, true, nil
}

func loadRegistryDatabase(ctx context.Context, database *stateDatabase) (Registry, error) {
	registry, _, err := loadRegistrySnapshotDatabase(ctx, database)
	return registry, err
}

func loadRegistrySnapshotDatabase(ctx context.Context, database *stateDatabase) (Registry, int64, error) {
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return emptyRegistry(), 0, fmt.Errorf("begin registry snapshot: %w", err)
	}
	defer tx.Rollback()
	registry, revision, loadErr := loadRegistrySnapshotQuery(ctx, tx)
	if loadErr != nil {
		return registry, revision, loadErr
	}
	if err := tx.Commit(); err != nil {
		return emptyRegistry(), 0, fmt.Errorf("finish registry snapshot: %w", err)
	}
	return registry, revision, nil
}

func loadRegistrySnapshotQuery(ctx context.Context, queryer stateQueryer) (Registry, int64, error) {
	registry := emptyRegistry()
	var present int
	var revision int64
	if err := queryer.QueryRowContext(ctx, "SELECT registry_present, registry_revision FROM state_meta WHERE id = 1").Scan(&present, &revision); err != nil {
		return registry, 0, fmt.Errorf("load registry metadata: %w", err)
	}
	if present == 0 {
		return registry, revision, os.ErrNotExist
	}
	var indicators int
	if err := queryer.QueryRowContext(ctx, "SELECT desktop_indicators FROM registry_preferences WHERE id = 1").Scan(&indicators); err != nil {
		return registry, revision, fmt.Errorf("load registry preferences: %w", err)
	}
	if indicators != 0 && indicators != 1 {
		return registry, revision, errors.New("registry desktop indicator preference is invalid")
	}
	registry.Preferences.DesktopIndicators = indicators == 1
	var largestPayload, totalPayload int64
	if err := queryer.QueryRowContext(ctx, "SELECT COALESCE(MAX(length(payload)), 0), COALESCE(SUM(length(payload)), 0) FROM contexts").Scan(&largestPayload, &totalPayload); err != nil {
		return registry, revision, fmt.Errorf("measure registry contexts: %w", err)
	}
	if largestPayload > maxDatabasePayloadBytes || totalPayload > maxStateDatabaseBytes {
		return registry, revision, errors.New("stored registry payload exceeds the state database safety budget")
	}
	rows, err := queryer.QueryContext(ctx, "SELECT id, encoding_version, payload FROM contexts ORDER BY ordinal, id")
	if err != nil {
		return registry, revision, fmt.Errorf("load registry contexts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var encodingVersion int
		var payload []byte
		if err := rows.Scan(&id, &encodingVersion, &payload); err != nil {
			return registry, revision, fmt.Errorf("scan registry context: %w", err)
		}
		if encodingVersion != ContextsSchemaVersion {
			return registry, revision, &UnsupportedVersionError{Document: "context row", Got: encodingVersion, Want: ContextsSchemaVersion}
		}
		var contextValue Context
		if err := decodeDatabasePayload("context row", payload, &contextValue); err != nil {
			return registry, revision, err
		}
		if string(contextValue.ID) != id {
			return registry, revision, fmt.Errorf("context row key %q does not match payload ID %q", id, contextValue.ID)
		}
		registry.Contexts = append(registry.Contexts, contextValue)
	}
	if err := rows.Err(); err != nil {
		return registry, revision, fmt.Errorf("load registry contexts: %w", err)
	}
	if err := registry.Validate(); err != nil {
		return registry, revision, fmt.Errorf("validate registry: %w", err)
	}
	return registry, revision, nil
}

func saveRegistryDatabase(ctx context.Context, database *stateDatabase, registry Registry, expectedRevision int64) error {
	delta, err := prepareRegistryDelta(ctx, database, registry, expectedRevision)
	if err != nil {
		return err
	}
	tx, err := beginStateWrite(ctx, database)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := applyRegistryDeltaTx(ctx, tx, delta); err != nil {
		return err
	}
	return commitStateWrite(ctx, tx)
}

type storedContextRow struct {
	ordinal         int64
	encodingVersion int
	payload         []byte
}

type registryContextWrite struct {
	id      ContextID
	ordinal int64
	payload []byte
}

type registryOrdinalWrite struct {
	id      ContextID
	ordinal int64
}

type registryDelta struct {
	expectedRevision         int64
	desktopIndicators        bool
	desktopIndicatorsChanged bool
	upserts                  []registryContextWrite
	reorders                 []registryOrdinalWrite
	deletes                  []ContextID
}

func prepareRegistryDelta(ctx context.Context, database *stateDatabase, registry Registry, expectedRevision int64) (registryDelta, error) {
	if err := registry.Validate(); err != nil {
		return registryDelta{}, fmt.Errorf("validate registry: %w", err)
	}
	desiredPayloads := make(map[string][]byte, len(registry.Contexts))
	for _, contextValue := range registry.Contexts {
		payload, err := marshalDatabasePayload("context row", contextValue)
		if err != nil {
			return registryDelta{}, err
		}
		desiredPayloads[string(contextValue.ID)] = payload
	}

	existing, storedIndicators, storedRevision, err := loadStoredRegistryRows(ctx, database)
	if err != nil {
		return registryDelta{}, err
	}
	if storedRevision != expectedRevision {
		return registryDelta{}, ErrRegistryConflict
	}

	delta := registryDelta{
		expectedRevision:         expectedRevision,
		desktopIndicators:        registry.Preferences.DesktopIndicators,
		desktopIndicatorsChanged: storedIndicators != registry.Preferences.DesktopIndicators,
	}
	existingOrdinals := make(map[string]int64, len(existing))
	for id, stored := range existing {
		existingOrdinals[id] = stored.ordinal
	}
	desiredOrdinals := stableRegistryOrdinals(registry.Contexts, existingOrdinals)
	for _, contextValue := range registry.Contexts {
		id := string(contextValue.ID)
		ordinal := desiredOrdinals[id]
		stored, exists := existing[id]
		if !exists || stored.encodingVersion != ContextsSchemaVersion || !bytes.Equal(stored.payload, desiredPayloads[id]) {
			delta.upserts = append(delta.upserts, registryContextWrite{
				id:      contextValue.ID,
				ordinal: ordinal,
				payload: desiredPayloads[id],
			})
		} else if stored.ordinal != ordinal {
			delta.reorders = append(delta.reorders, registryOrdinalWrite{id: contextValue.ID, ordinal: ordinal})
		}
		delete(existing, id)
	}
	for id := range existing {
		delta.deletes = append(delta.deletes, ContextID(id))
	}
	sort.Slice(delta.deletes, func(left, right int) bool { return delta.deletes[left] < delta.deletes[right] })
	return delta, nil
}

func loadStoredRegistryRows(ctx context.Context, database *stateDatabase) (map[string]storedContextRow, bool, int64, error) {
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, 0, fmt.Errorf("begin stored registry snapshot: %w", err)
	}
	defer tx.Rollback()
	var revision int64
	if err := tx.QueryRowContext(ctx, "SELECT registry_revision FROM state_meta WHERE id = 1").Scan(&revision); err != nil {
		return nil, false, 0, fmt.Errorf("load stored registry revision: %w", err)
	}
	var indicators int
	if err := tx.QueryRowContext(ctx, "SELECT desktop_indicators FROM registry_preferences WHERE id = 1").Scan(&indicators); err != nil {
		return nil, false, 0, fmt.Errorf("load stored registry preferences: %w", err)
	}
	if indicators != 0 && indicators != 1 {
		return nil, false, 0, errors.New("stored registry desktop indicator preference is invalid")
	}
	existingRows, err := tx.QueryContext(ctx, "SELECT id, ordinal, encoding_version, payload FROM contexts")
	if err != nil {
		return nil, false, 0, fmt.Errorf("load stored context keys: %w", err)
	}
	existing := make(map[string]storedContextRow)
	for existingRows.Next() {
		var id string
		var stored storedContextRow
		if err := existingRows.Scan(&id, &stored.ordinal, &stored.encodingVersion, &stored.payload); err != nil {
			_ = existingRows.Close()
			return nil, false, 0, fmt.Errorf("scan stored context row: %w", err)
		}
		existing[id] = stored
	}
	if err := existingRows.Err(); err != nil {
		_ = existingRows.Close()
		return nil, false, 0, fmt.Errorf("load stored context rows: %w", err)
	}
	if err := existingRows.Close(); err != nil {
		return nil, false, 0, fmt.Errorf("close stored context keys: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, 0, fmt.Errorf("finish stored registry snapshot: %w", err)
	}
	return existing, indicators == 1, revision, nil
}

func applyRegistryDeltaTx(ctx context.Context, tx stateTransaction, delta registryDelta) error {
	result, err := tx.ExecContext(ctx, "UPDATE state_meta SET registry_present = 1, registry_revision = registry_revision + 1 WHERE id = 1 AND registry_revision = ?", delta.expectedRevision)
	if err != nil {
		return fmt.Errorf("store registry metadata: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm registry revision: %w", err)
	}
	if changed != 1 {
		return ErrRegistryConflict
	}
	for _, write := range delta.upserts {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO contexts (id, ordinal, encoding_version, payload)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				ordinal = excluded.ordinal,
				encoding_version = excluded.encoding_version,
				payload = excluded.payload`,
			write.id, write.ordinal, ContextsSchemaVersion, write.payload,
		); err != nil {
			return fmt.Errorf("store context %s: %w", write.id, err)
		}
	}
	for _, write := range delta.reorders {
		if _, err := tx.ExecContext(ctx, "UPDATE contexts SET ordinal = ? WHERE id = ?", write.ordinal, write.id); err != nil {
			return fmt.Errorf("reorder context %s: %w", write.id, err)
		}
	}
	for _, id := range delta.deletes {
		if _, err := tx.ExecContext(ctx, "DELETE FROM contexts WHERE id = ?", id); err != nil {
			return fmt.Errorf("remove context %s: %w", id, err)
		}
	}
	if delta.desktopIndicatorsChanged {
		if _, err := tx.ExecContext(ctx, "UPDATE registry_preferences SET desktop_indicators = ? WHERE id = 1", boolInteger(delta.desktopIndicators)); err != nil {
			return fmt.Errorf("store registry preferences: %w", err)
		}
	}
	return nil
}

// saveRegistryBulkTx is reserved for first-time schema initialization and
// legacy import, where the destination registry tables are known to be empty.
func saveRegistryBulkTx(ctx context.Context, tx stateTransaction, registry Registry, expectedRevision int64) error {
	if err := registry.Validate(); err != nil {
		return fmt.Errorf("validate registry: %w", err)
	}
	for ordinal, contextValue := range registry.Contexts {
		payload, err := marshalDatabasePayload("context row", contextValue)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO contexts (id, ordinal, encoding_version, payload) VALUES (?, ?, ?, ?)", contextValue.ID, ordinal, ContextsSchemaVersion, payload); err != nil {
			return fmt.Errorf("store context %s: %w", contextValue.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE registry_preferences SET desktop_indicators = ? WHERE id = 1", boolInteger(registry.Preferences.DesktopIndicators)); err != nil {
		return fmt.Errorf("store registry preferences: %w", err)
	}
	delta := registryDelta{expectedRevision: expectedRevision}
	return applyRegistryDeltaTx(ctx, tx, delta)
}

// stableRegistryOrdinals preserves ordering keys when a mutation only removes
// contexts or appends new ones. That keeps the common cleanup path O(removed)
// instead of rewriting every surviving payload merely because slice indexes
// became dense again. An explicit reorder or middle insertion still falls back
// to dense ordinals so Save continues to honor the caller's requested order.
func stableRegistryOrdinals(contexts []Context, existing map[string]int64) map[string]int64 {
	desired := make(map[string]int64, len(contexts))
	targetExisting := make([]string, 0, len(contexts))
	newOnlyAtEnd := true
	sawNew := false
	for _, contextValue := range contexts {
		id := string(contextValue.ID)
		if _, exists := existing[id]; exists {
			targetExisting = append(targetExisting, id)
			if sawNew {
				newOnlyAtEnd = false
			}
			continue
		}
		sawNew = true
	}

	storedExisting := append([]string(nil), targetExisting...)
	sort.Slice(storedExisting, func(left, right int) bool {
		leftOrdinal, rightOrdinal := existing[storedExisting[left]], existing[storedExisting[right]]
		if leftOrdinal != rightOrdinal {
			return leftOrdinal < rightOrdinal
		}
		return storedExisting[left] < storedExisting[right]
	})
	stableOrder := newOnlyAtEnd && len(storedExisting) == len(targetExisting)
	if stableOrder {
		for index := range storedExisting {
			if storedExisting[index] != targetExisting[index] {
				stableOrder = false
				break
			}
		}
	}
	if !stableOrder {
		for ordinal, contextValue := range contexts {
			desired[string(contextValue.ID)] = int64(ordinal)
		}
		return desired
	}

	maxOrdinal := int64(-1)
	for _, ordinal := range existing {
		if ordinal > maxOrdinal {
			maxOrdinal = ordinal
		}
	}
	newCount := len(contexts) - len(targetExisting)
	if newCount > 0 && maxOrdinal > math.MaxInt64-int64(newCount) {
		for dense, contextValue := range contexts {
			desired[string(contextValue.ID)] = int64(dense)
		}
		return desired
	}
	nextOrdinal := maxOrdinal + 1
	for _, contextValue := range contexts {
		id := string(contextValue.ID)
		if ordinal, exists := existing[id]; exists {
			desired[id] = ordinal
			continue
		}
		desired[id] = nextOrdinal
		nextOrdinal++
	}
	return desired
}

func ReadRegistrySnapshot(root string) (Registry, error) {
	return ReadRegistrySnapshotContext(context.Background(), root)
}

func ReadRegistrySnapshotContext(ctx context.Context, root string) (Registry, error) {
	registry := emptyRegistry()
	if ctx == nil {
		return registry, errors.New("registry snapshot context is nil")
	}
	if err := ctx.Err(); err != nil {
		return registry, err
	}
	err := RegistryStoreFor(root).LoadIntoContext(ctx, &registry)
	if errors.Is(err, os.ErrNotExist) {
		return registry, nil
	}
	return registry, err
}

func UpdateRegistry(root string, mutate func(*Registry) error) (Registry, error) {
	return UpdateRegistryContext(context.Background(), root, mutate)
}

func UpdateRegistryContext(ctx context.Context, root string, mutate func(*Registry) error) (Registry, error) {
	initial := emptyRegistry()
	if ctx == nil {
		return initial, errors.New("registry update context is nil")
	}
	if mutate == nil {
		return initial, errors.New("registry mutation is nil")
	}
	database, err := openStateDatabase(ctx, root, true)
	if err != nil {
		return initial, err
	}
	defer database.Close()
	var candidate Registry
	var revision int64
	err = WithRegistryLockContext(ctx, root, func(*statefile.LockedPrivateDirectory) error {
		candidate, revision, err = loadRegistrySnapshotDatabase(ctx, database)
		if errors.Is(err, os.ErrNotExist) {
			candidate = initial
		} else if err != nil {
			return err
		}
		if err := mutate(&candidate); err != nil {
			return fmt.Errorf("mutate registry: %w", err)
		}
		return saveRegistryDatabase(ctx, database, candidate, revision)
	})
	if err != nil {
		return initial, err
	}
	return candidate, nil
}

// UpdateRegistryWithTerminalCreationContext commits one registry mutation and
// the newly created terminal's presentation timestamp in the same SQLite
// transaction. The callback is evaluated while the registry lifecycle lock is
// held but outside the database transaction.
func UpdateRegistryWithTerminalCreationContext(
	ctx context.Context,
	root string,
	mutate func(*Registry) error,
	creation func() (ContextID, time.Time, bool),
) (Registry, error) {
	initial := emptyRegistry()
	if ctx == nil {
		return initial, errors.New("registry update context is nil")
	}
	if mutate == nil || creation == nil {
		return initial, errors.New("registry terminal creation mutation is incomplete")
	}
	database, err := openStateDatabase(ctx, root, true)
	if err != nil {
		return initial, err
	}
	defer database.Close()
	var candidate Registry
	err = WithRegistryLockContext(ctx, root, func(*statefile.LockedPrivateDirectory) error {
		candidate, revision, err := loadRegistrySnapshotDatabase(ctx, database)
		if errors.Is(err, os.ErrNotExist) {
			candidate = initial
			revision = 0
		} else if err != nil {
			return err
		}
		if err := mutate(&candidate); err != nil {
			return fmt.Errorf("mutate registry: %w", err)
		}
		id, createdAt, record := creation()
		if !record {
			return saveRegistryDatabase(ctx, database, candidate, revision)
		}
		if err := id.Validate(); err != nil {
			return fmt.Errorf("invalid created terminal context ID: %w", err)
		}
		if createdAt.IsZero() {
			return errors.New("terminal creation time must be non-zero")
		}
		index, resolveErr := ResolveContext(candidate, string(id))
		if resolveErr != nil || candidate.Contexts[index].Launcher.Kind != LauncherHerdr || candidate.Contexts[index].Launcher.Terminal == nil {
			return errors.New("terminal creation activity does not reference a typed terminal context")
		}
		delta, err := prepareRegistryDelta(ctx, database, candidate, revision)
		if err != nil {
			return err
		}
		tx, err := beginStateWrite(ctx, database)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := applyRegistryDeltaTx(ctx, tx, delta); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO terminal_activity (context_id, created_at, last_focused_at) VALUES (?, ?, NULL)
			ON CONFLICT(context_id) DO UPDATE SET created_at = excluded.created_at, last_focused_at = NULL`,
			id, createdAt.UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("store terminal creation activity for %s: %w", id, err)
		}
		return commitStateWrite(ctx, tx)
	})
	if err != nil {
		return initial, err
	}
	return candidate, nil
}

func InspectRegistryLocked(root string, inspect func(Registry) error) error {
	return InspectRegistryLockedContext(context.Background(), root, inspect)
}

func InspectRegistryLockedContext(ctx context.Context, root string, inspect func(Registry) error) error {
	if ctx == nil {
		return errors.New("registry inspection context is nil")
	}
	if inspect == nil {
		return errors.New("registry inspection is nil")
	}
	database, err := openStateDatabase(ctx, root, true)
	if err != nil {
		return err
	}
	defer database.Close()
	return WithRegistryLockContext(ctx, root, func(*statefile.LockedPrivateDirectory) error {
		candidate, err := loadRegistryDatabase(ctx, database)
		if errors.Is(err, os.ErrNotExist) {
			candidate = emptyRegistry()
		} else if err != nil {
			return err
		}
		return inspect(candidate)
	})
}

// WithRegistryLockContext bounds only lock acquisition. Once acquired, the
// callback keeps the caller's original context and may complete its explicit
// observe/act/reconcile saga without an artificial 250 ms action deadline.
func WithRegistryLockContext(ctx context.Context, root string, action func(*statefile.LockedPrivateDirectory) error) error {
	return withBoundedPrivateDirectoryLockContext(ctx, root, "registry", action)
}

func withBoundedPrivateDirectoryLockContext(
	ctx context.Context,
	path string,
	label string,
	action func(*statefile.LockedPrivateDirectory) error,
) error {
	if ctx == nil {
		return fmt.Errorf("%s lock context is nil", label)
	}
	if action == nil {
		return fmt.Errorf("%s lock action is nil", label)
	}
	waitContext := ctx
	cancel := func() {}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > databaseBusyTimeout {
		waitContext, cancel = context.WithTimeout(ctx, databaseBusyTimeout)
	}
	defer cancel()
	err := statefile.WithPrivateDirectoryLockContext(waitContext, path, func(directory *statefile.LockedPrivateDirectory) error {
		return action(directory)
	})
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return fmt.Errorf("%w: wait for %s lock: %w", ErrStateDatabaseBusy, label, err)
	}
	return err
}

func initializeStateDatabase(ctx context.Context, root string) error {
	database, err := openStateDatabase(ctx, root, true)
	if err != nil {
		return err
	}
	return database.Close()
}

type LayoutStore struct{ root string }

func LayoutStoreFor(root string) LayoutStore { return LayoutStore{root: root} }

func (store LayoutStore) Save(value LayoutSnapshot) error {
	return store.SaveContext(context.Background(), value)
}

func (store LayoutStore) SaveContext(ctx context.Context, value LayoutSnapshot) error {
	if ctx == nil {
		return errors.New("layout save context is nil")
	}
	if err := value.Validate(); err != nil {
		return fmt.Errorf("validate layout: %w", err)
	}
	payload, err := marshalDatabasePayload("layout", value)
	if err != nil {
		return err
	}
	database, err := openStateDatabase(ctx, store.root, true)
	if err != nil {
		return err
	}
	defer database.Close()
	tx, err := beginStateWrite(ctx, database)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO layout_state (id, encoding_version, payload) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET encoding_version = excluded.encoding_version, payload = excluded.payload`,
		LayoutSchemaVersion, payload,
	); err != nil {
		return fmt.Errorf("store layout: %w", err)
	}
	return commitStateWrite(ctx, tx)
}

func (store LayoutStore) LoadInto(target *LayoutSnapshot) error {
	return store.LoadIntoContext(context.Background(), target)
}

func (store LayoutStore) LoadIntoContext(ctx context.Context, target *LayoutSnapshot) error {
	if ctx == nil {
		return errors.New("layout load context is nil")
	}
	if target == nil {
		return errors.New("layout target is nil")
	}
	database, err := openStateDatabase(ctx, store.root, false)
	if err != nil {
		return err
	}
	defer database.Close()
	var payloadLength int64
	if err := database.db.QueryRowContext(ctx, "SELECT length(payload) FROM layout_state WHERE id = 1").Scan(&payloadLength); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return os.ErrNotExist
		}
		return fmt.Errorf("measure layout: %w", err)
	}
	if payloadLength > maxDatabasePayloadBytes {
		return fmt.Errorf("layout payload is too large: %d bytes exceeds %d", payloadLength, maxDatabasePayloadBytes)
	}
	var encodingVersion int
	var payload []byte
	if err := database.db.QueryRowContext(ctx, "SELECT encoding_version, payload FROM layout_state WHERE id = 1").Scan(&encodingVersion, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return os.ErrNotExist
		}
		return fmt.Errorf("load layout: %w", err)
	}
	if encodingVersion != LayoutSchemaVersion {
		return &UnsupportedVersionError{Document: "layout", Got: encodingVersion, Want: LayoutSchemaVersion}
	}
	var candidate LayoutSnapshot
	if err := decodeDatabasePayload("layout", payload, &candidate); err != nil {
		return err
	}
	if err := candidate.Validate(); err != nil {
		return fmt.Errorf("validate layout: %w", err)
	}
	*target = candidate
	return nil
}

var ErrApplicationSessionConflict = errors.New("application session changed concurrently")

type ApplicationSessionStore struct{ root string }

func ApplicationSessionStoreFor(root string) ApplicationSessionStore {
	return ApplicationSessionStore{root: root}
}

func (store ApplicationSessionStore) Save(value ApplicationSessionState) error {
	return store.SaveContext(context.Background(), value)
}

func (store ApplicationSessionStore) SaveContext(ctx context.Context, value ApplicationSessionState) error {
	if ctx == nil {
		return errors.New("application session save context is nil")
	}
	if err := value.Validate(); err != nil {
		return fmt.Errorf("validate application session: %w", err)
	}
	database, err := openStateDatabase(ctx, store.root, true)
	if err != nil {
		return err
	}
	defer database.Close()
	delta, err := prepareApplicationSessionDelta(ctx, database, value)
	if err != nil {
		return err
	}
	tx, err := beginStateWrite(ctx, database)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := applyApplicationSessionDeltaTx(ctx, tx, delta); err != nil {
		return err
	}
	return commitStateWrite(ctx, tx)
}

type storedApplicationSession struct {
	present          bool
	revision         int64
	registryRevision int64
	compositorID     string
	attempts         map[ContextID]string
}

type applicationAttemptWrite struct {
	contextID ContextID
	startedAt string
}

type applicationSessionDelta struct {
	expectedPresent          bool
	expectedRevision         int64
	expectedRegistryRevision int64
	requireRegistryRevision  bool
	compositorID             string
	compositorIDChanged      bool
	upserts                  []applicationAttemptWrite
	deletes                  []ContextID
}

func prepareApplicationSessionDelta(ctx context.Context, database *stateDatabase, value ApplicationSessionState) (applicationSessionDelta, error) {
	desiredAttempts := make(map[ContextID]string, len(value.Attempts))
	for _, attempt := range value.Attempts {
		desiredAttempts[attempt.ContextID] = attempt.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	contextIDs := make([]ContextID, 0, len(value.Attempts))
	for _, attempt := range value.Attempts {
		contextIDs = append(contextIDs, attempt.ContextID)
	}
	stored, err := loadStoredApplicationSession(ctx, database, contextIDs)
	if err != nil {
		return applicationSessionDelta{}, err
	}
	delta := applicationSessionDelta{
		expectedPresent:          stored.present,
		expectedRevision:         stored.revision,
		expectedRegistryRevision: stored.registryRevision,
		requireRegistryRevision:  len(contextIDs) != 0,
		compositorID:             value.CompositorID,
		compositorIDChanged:      !stored.present || stored.compositorID != value.CompositorID,
	}
	for _, attempt := range value.Attempts {
		startedAt := desiredAttempts[attempt.ContextID]
		if storedStartedAt, exists := stored.attempts[attempt.ContextID]; !exists || storedStartedAt != startedAt {
			delta.upserts = append(delta.upserts, applicationAttemptWrite{contextID: attempt.ContextID, startedAt: startedAt})
		}
		delete(stored.attempts, attempt.ContextID)
	}
	for id := range stored.attempts {
		delta.deletes = append(delta.deletes, id)
	}
	sort.Slice(delta.deletes, func(left, right int) bool { return delta.deletes[left] < delta.deletes[right] })
	return delta, nil
}

func loadStoredApplicationSession(ctx context.Context, database *stateDatabase, contextIDs []ContextID) (storedApplicationSession, error) {
	stored := storedApplicationSession{attempts: make(map[ContextID]string)}
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return stored, fmt.Errorf("begin stored application session snapshot: %w", err)
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, "SELECT registry_revision FROM state_meta WHERE id = 1").Scan(&stored.registryRevision); err != nil {
		return stored, fmt.Errorf("load registry revision for application session: %w", err)
	}
	for _, id := range contextIDs {
		if err := requireStoredApplicationContext(ctx, tx, id); err != nil {
			return stored, err
		}
	}
	if err := tx.QueryRowContext(ctx, "SELECT compositor_id, revision FROM application_session WHERE id = 1").Scan(&stored.compositorID, &stored.revision); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return stored, fmt.Errorf("load stored application session: %w", err)
		}
	} else {
		stored.present = true
	}
	rows, err := tx.QueryContext(ctx, "SELECT context_id, started_at FROM application_launch_attempts")
	if err != nil {
		return stored, fmt.Errorf("load stored application launch attempts: %w", err)
	}
	for rows.Next() {
		var id ContextID
		var startedAt string
		if err := rows.Scan(&id, &startedAt); err != nil {
			_ = rows.Close()
			return stored, fmt.Errorf("scan stored application launch attempt: %w", err)
		}
		stored.attempts[id] = startedAt
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return stored, fmt.Errorf("load stored application launch attempts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return stored, fmt.Errorf("close stored application launch attempts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return stored, fmt.Errorf("finish stored application session snapshot: %w", err)
	}
	return stored, nil
}

func applyApplicationSessionDeltaTx(ctx context.Context, tx stateTransaction, delta applicationSessionDelta) error {
	var result sql.Result
	var err error
	if !delta.expectedPresent {
		result, err = tx.ExecContext(ctx, `
			INSERT INTO application_session (id, compositor_id, revision)
			VALUES (1, ?, 1)
			ON CONFLICT(id) DO NOTHING`, delta.compositorID)
	} else if delta.compositorIDChanged {
		result, err = tx.ExecContext(ctx, "UPDATE application_session SET compositor_id = ?, revision = revision + 1 WHERE id = 1 AND revision = ?", delta.compositorID, delta.expectedRevision)
	} else {
		result, err = tx.ExecContext(ctx, "UPDATE application_session SET revision = revision + 1 WHERE id = 1 AND revision = ?", delta.expectedRevision)
	}
	if err != nil {
		return fmt.Errorf("compare and store application session: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm application session revision: %w", err)
	}
	if changed != 1 {
		return ErrApplicationSessionConflict
	}
	if delta.requireRegistryRevision {
		var registryRevision int64
		if err := tx.QueryRowContext(ctx, "SELECT registry_revision FROM state_meta WHERE id = 1").Scan(&registryRevision); err != nil {
			return fmt.Errorf("confirm application context registry revision: %w", err)
		}
		if registryRevision != delta.expectedRegistryRevision {
			return ErrRegistryConflict
		}
	}
	for _, write := range delta.upserts {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO application_launch_attempts (context_id, started_at) VALUES (?, ?)
			ON CONFLICT(context_id) DO UPDATE SET started_at = excluded.started_at`, write.contextID, write.startedAt); err != nil {
			return fmt.Errorf("store application launch attempt for %s: %w", write.contextID, err)
		}
	}
	for _, id := range delta.deletes {
		if _, err := tx.ExecContext(ctx, "DELETE FROM application_launch_attempts WHERE context_id = ?", id); err != nil {
			return fmt.Errorf("remove application launch attempt for %s: %w", id, err)
		}
	}
	return nil
}

func requireStoredApplicationContext(ctx context.Context, tx stateTransaction, id ContextID) error {
	contextValue, err := loadStoredContextReference(ctx, tx, id)
	if err != nil {
		return err
	}
	if contextValue.App == nil || contextValue.Launcher.Kind != LauncherDesktop && contextValue.Launcher.Kind != LauncherFlatpak {
		return fmt.Errorf("context %s is not a desktop application context", id)
	}
	return nil
}

func loadStoredContextReference(ctx context.Context, tx stateTransaction, id ContextID) (Context, error) {
	var encodingVersion int
	var payload []byte
	if err := tx.QueryRowContext(ctx, "SELECT encoding_version, payload FROM contexts WHERE id = ?", id).Scan(&encodingVersion, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Context{}, fmt.Errorf("context %s is absent from the authoritative registry", id)
		}
		return Context{}, fmt.Errorf("load context %s: %w", id, err)
	}
	if encodingVersion != ContextsSchemaVersion {
		return Context{}, &UnsupportedVersionError{Document: "context row", Got: encodingVersion, Want: ContextsSchemaVersion}
	}
	var contextValue Context
	if err := decodeDatabasePayload("context row", payload, &contextValue); err != nil {
		return Context{}, err
	}
	if contextValue.ID != id {
		return Context{}, fmt.Errorf("context row key %q does not match payload ID %q", id, contextValue.ID)
	}
	if err := contextValue.Validate(); err != nil {
		return Context{}, fmt.Errorf("validate context %s: %w", id, err)
	}
	return contextValue, nil
}

func (store ApplicationSessionStore) LoadInto(target *ApplicationSessionState) error {
	return store.LoadIntoContext(context.Background(), target)
}

func (store ApplicationSessionStore) LoadIntoContext(ctx context.Context, target *ApplicationSessionState) error {
	if ctx == nil {
		return errors.New("application session load context is nil")
	}
	if target == nil {
		return errors.New("application session target is nil")
	}
	database, err := openStateDatabase(ctx, store.root, false)
	if err != nil {
		return err
	}
	defer database.Close()
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin application session snapshot: %w", err)
	}
	defer tx.Rollback()
	candidate := ApplicationSessionState{Version: ApplicationSessionSchemaVersion, Attempts: []ApplicationLaunchAttempt{}}
	if err := tx.QueryRowContext(ctx, "SELECT compositor_id FROM application_session WHERE id = 1").Scan(&candidate.CompositorID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return os.ErrNotExist
		}
		return fmt.Errorf("load application session: %w", err)
	}
	rows, err := tx.QueryContext(ctx, "SELECT context_id, started_at FROM application_launch_attempts ORDER BY context_id")
	if err != nil {
		return fmt.Errorf("load application launch attempts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id ContextID
		var startedAt string
		if err := rows.Scan(&id, &startedAt); err != nil {
			return fmt.Errorf("scan application launch attempt: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, startedAt)
		if err != nil {
			return fmt.Errorf("decode application launch attempt for %s: %w", id, err)
		}
		candidate.Attempts = append(candidate.Attempts, ApplicationLaunchAttempt{ContextID: id, StartedAt: parsed.UTC()})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load application launch attempts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close application launch attempts: %w", err)
	}
	if err := candidate.Validate(); err != nil {
		return fmt.Errorf("validate application session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finish application session snapshot: %w", err)
	}
	*target = candidate
	return nil
}

type TerminalActivityStore struct{ root string }

func (store TerminalActivityStore) Save(value TerminalActivityState) error {
	return store.SaveContext(context.Background(), value)
}

func (store TerminalActivityStore) SaveContext(ctx context.Context, value TerminalActivityState) error {
	if ctx == nil {
		return errors.New("terminal activity save context is nil")
	}
	if err := value.Validate(); err != nil {
		return fmt.Errorf("validate terminal activity: %w", err)
	}
	database, err := openStateDatabase(ctx, store.root, true)
	if err != nil {
		return err
	}
	defer database.Close()
	tx, err := beginStateWrite(ctx, database)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := saveTerminalActivityTx(ctx, tx, value); err != nil {
		return err
	}
	return commitStateWrite(ctx, tx)
}

func (store TerminalActivityStore) LoadInto(target *TerminalActivityState) error {
	return store.LoadIntoContext(context.Background(), target)
}

func (store TerminalActivityStore) LoadIntoContext(ctx context.Context, target *TerminalActivityState) error {
	if ctx == nil {
		return errors.New("terminal activity load context is nil")
	}
	if target == nil {
		return errors.New("terminal activity target is nil")
	}
	database, err := openStateDatabase(ctx, store.root, false)
	if err != nil {
		return err
	}
	defer database.Close()
	candidate, err := loadTerminalActivityQuery(ctx, database.db)
	if err != nil {
		return err
	}
	*target = candidate
	return nil
}

func (store TerminalActivityStore) LoadSnapshotInto(target *TerminalActivityState) error {
	return store.LoadInto(target)
}

// TerminalInventorySnapshot is the registry and activity projection consumed
// by terminal list, status, cleanup, and manage. Both documents come from one
// SQLite read transaction so a concurrent create or purge cannot tear the
// displayed inventory across generations.
type TerminalInventorySnapshot struct {
	Registry Registry
	Activity TerminalActivityState
}

func ReadTerminalInventorySnapshotContext(ctx context.Context, root string) (TerminalInventorySnapshot, error) {
	snapshot := TerminalInventorySnapshot{Registry: emptyRegistry(), Activity: emptyTerminalActivityState()}
	if ctx == nil {
		return snapshot, errors.New("terminal inventory snapshot context is nil")
	}
	database, err := openStateDatabase(ctx, root, false)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, err
	}
	defer database.Close()
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return snapshot, fmt.Errorf("begin terminal inventory snapshot: %w", err)
	}
	defer tx.Rollback()
	registry, _, err := loadRegistrySnapshotQuery(ctx, tx)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return snapshot, err
	}
	activity, err := loadTerminalActivityQuery(ctx, tx)
	if err != nil {
		return snapshot, err
	}
	if err := tx.Commit(); err != nil {
		return snapshot, fmt.Errorf("finish terminal inventory snapshot: %w", err)
	}
	snapshot.Registry = registry
	snapshot.Activity = activity
	return snapshot, nil
}

func (store TerminalActivityStore) UpdateContext(ctx context.Context, initial TerminalActivityState, mutate func(*TerminalActivityState) error) (TerminalActivityState, error) {
	if ctx == nil {
		return initial, errors.New("terminal activity update context is nil")
	}
	if mutate == nil {
		return initial, errors.New("terminal activity mutation is nil")
	}
	database, err := openStateDatabase(ctx, store.root, true)
	if err != nil {
		return initial, err
	}
	defer database.Close()
	tx, err := beginStateWrite(ctx, database)
	if err != nil {
		return initial, err
	}
	defer tx.Rollback()
	candidate, err := loadTerminalActivityQuery(ctx, tx)
	if err != nil {
		return initial, err
	}
	if err := mutate(&candidate); err != nil {
		return initial, fmt.Errorf("mutate terminal activity: %w", err)
	}
	if err := candidate.Validate(); err != nil {
		return initial, fmt.Errorf("validate terminal activity: %w", err)
	}
	if err := saveTerminalActivityTx(ctx, tx, candidate); err != nil {
		return initial, err
	}
	if err := commitStateWrite(ctx, tx); err != nil {
		return candidate, err
	}
	return candidate, nil
}

type rowQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type stateQueryer interface {
	rowQueryer
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadTerminalActivityQuery(ctx context.Context, queryer rowQueryer) (TerminalActivityState, error) {
	state := emptyTerminalActivityState()
	rows, err := queryer.QueryContext(ctx, "SELECT context_id, created_at, last_focused_at FROM terminal_activity ORDER BY context_id")
	if err != nil {
		return state, fmt.Errorf("load terminal activity: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id ContextID
		var createdValue, focusedValue sql.NullString
		if err := rows.Scan(&id, &createdValue, &focusedValue); err != nil {
			return state, fmt.Errorf("scan terminal activity: %w", err)
		}
		createdAt, err := parseDatabaseTime("terminal creation time", createdValue)
		if err != nil {
			return state, err
		}
		focusedAt, err := parseDatabaseTime("terminal focus time", focusedValue)
		if err != nil {
			return state, err
		}
		state.Terminals = append(state.Terminals, TerminalActivity{ContextID: id, CreatedAt: createdAt, LastFocusedAt: focusedAt})
	}
	if err := rows.Err(); err != nil {
		return state, fmt.Errorf("load terminal activity: %w", err)
	}
	if err := state.Validate(); err != nil {
		return state, fmt.Errorf("validate terminal activity: %w", err)
	}
	return state, nil
}

func saveTerminalActivityTx(ctx context.Context, tx stateTransaction, state TerminalActivityState) error {
	if err := state.Validate(); err != nil {
		return fmt.Errorf("validate terminal activity: %w", err)
	}
	rows, err := tx.QueryContext(ctx, "SELECT context_id FROM terminal_activity")
	if err != nil {
		return fmt.Errorf("load terminal activity keys: %w", err)
	}
	existing := make(map[ContextID]struct{})
	for rows.Next() {
		var id ContextID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan terminal activity key: %w", err)
		}
		existing[id] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close terminal activity keys: %w", err)
	}
	for _, activity := range state.Terminals {
		if err := requireStoredTerminalContext(ctx, tx, activity.ContextID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO terminal_activity (context_id, created_at, last_focused_at) VALUES (?, ?, ?)
			ON CONFLICT(context_id) DO UPDATE SET created_at = excluded.created_at, last_focused_at = excluded.last_focused_at`,
			activity.ContextID, canonicalDatabaseTime(activity.CreatedAt), canonicalDatabaseTime(activity.LastFocusedAt),
		); err != nil {
			return fmt.Errorf("store terminal activity for %s: %w", activity.ContextID, err)
		}
		delete(existing, activity.ContextID)
	}
	for id := range existing {
		if _, err := tx.ExecContext(ctx, "DELETE FROM terminal_activity WHERE context_id = ?", id); err != nil {
			return fmt.Errorf("remove terminal activity for %s: %w", id, err)
		}
	}
	return nil
}
