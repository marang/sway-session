package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/marang/sway-session/internal/statefile"
)

// ErrTerminalPurgeProgress reports a successful bounded stop step. The caller
// must reobserve on a fresh pass before attempting delete; it is not a failure.
var ErrTerminalPurgeProgress = errors.New("terminal purge advanced one external step")

// LifecycleSessionDeleter performs at most one destructive Herdr call against
// the immutable pin. Nil success is checked against fresh filesystem absence.
type LifecycleSessionDeleter func(context.Context, Context, LifecyclePurgeTarget) error

func validateLifecyclePurge(operation LifecycleOperation) error {
	if operation.Phase != LifecycleForward || len(operation.Before) != 1 || len(operation.After) != 0 || len(operation.Targets) != 0 || operation.CompositorID != "" || operation.Purge == nil {
		return errors.New("purge requires one original terminal, one directory pin, and forward-only removal")
	}
	before := operation.Before[0]
	if before.Launcher.Kind != LauncherHerdr || before.Launcher.Session == "default" {
		return errors.New("purge requires a named Herdr session")
	}
	return validatePurgeTarget(*operation.Purge)
}

func validatePurgeTarget(target LifecyclePurgeTarget) error {
	if !filepath.IsAbs(target.Root) || filepath.Clean(target.Root) != target.Root || filepath.Base(target.Root) != "herdr" {
		return errors.New("purge root must be a clean absolute path ending in herdr")
	}
	if target.RootInode == 0 && (target.RootDevice != 0 || target.RootBirthNS != 0 || target.SessionExists) ||
		target.SessionExists && (target.RootInode == 0 || target.SessionInode == 0) ||
		!target.SessionExists && (target.SessionDevice != 0 || target.SessionInode != 0 || target.SessionBirthNS != 0) {
		return errors.New("inconsistent purge directory identity")
	}
	return nil
}

// StartTerminalPurgeContext captures the immutable deletion boundary and records
// intent, context removal, and activity cascade in one transaction. No Herdr
// effects occur here. Callers coordinating other terminal effects hold the
// terminal lifecycle lock; Begin serializes registry checks independently.
func StartTerminalPurgeContext(ctx context.Context, root string, expected Context, herdrRoot string, now time.Time) (LifecycleOperation, error) {
	if ctx == nil {
		return LifecycleOperation{}, errors.New("terminal purge context is nil")
	}
	if err := expected.Validate(); err != nil {
		return LifecycleOperation{}, err
	}
	if expected.Launcher.Kind != LauncherHerdr {
		return LifecycleOperation{}, errors.New("purge requires a Herdr context")
	}
	if now.IsZero() {
		return LifecycleOperation{}, errors.New("terminal purge time is zero")
	}
	target, err := ObserveTerminalPurgeTarget(ctx, herdrRoot, expected.Launcher.Session)
	if err != nil {
		return LifecycleOperation{}, err
	}
	id, err := NewContextID()
	if err != nil {
		return LifecycleOperation{}, err
	}
	operation := LifecycleOperation{
		ID: string(id), Version: LifecycleOperationVersion, Kind: LifecyclePurge, Phase: LifecycleForward,
		Before: []Context{expected}, After: []Context{}, Targets: []LifecycleWindowTarget{}, Purge: &target,
		UpdatedAt: now.UTC(), NextAttempt: now.UTC(),
	}
	persisted, err := BeginLifecycleOperationContext(ctx, root, operation)
	if err == nil {
		return persisted, nil
	}
	// Resolve only this exact intent after an uncertain acknowledgement. Never
	// recapture a new pin or restore the removed context after an ambiguous effect.
	checkCtx, cancel := mutationCompensationContext(ctx)
	defer cancel()
	visible, readErr := LoadLifecycleOperationContext(checkCtx, root, operation.ID)
	if readErr == nil && visible.Kind == LifecyclePurge && reflect.DeepEqual(visible.Before, operation.Before) && reflect.DeepEqual(visible.Purge, operation.Purge) {
		return visible, nil
	}
	return operation, fmt.Errorf("record purge operation %s before effects: %w", operation.ID, err)
}

// FindPendingTerminalPurgeContext uses the exact context reservation, never a
// label or an unbounded scan of operation payloads.
func FindPendingTerminalPurgeContext(ctx context.Context, root string, id ContextID) (LifecycleOperation, bool, error) {
	if err := id.Validate(); err != nil {
		return LifecycleOperation{}, false, err
	}
	database, err := openStateDatabase(ctx, root, false)
	if errors.Is(err, os.ErrNotExist) {
		return LifecycleOperation{}, false, nil
	}
	if err != nil {
		return LifecycleOperation{}, false, err
	}
	defer database.Close()
	exists, err := lifecycleTablesExist(ctx, database.db)
	if err != nil || !exists {
		return LifecycleOperation{}, false, err
	}
	var operationID string
	err = database.db.QueryRowContext(ctx, "SELECT operation_id FROM lifecycle_reservations WHERE resource_kind = 'context' AND resource_key = ? AND resource_scope = ''", id).Scan(&operationID)
	if errors.Is(err, sql.ErrNoRows) {
		return LifecycleOperation{}, false, nil
	}
	if err != nil {
		return LifecycleOperation{}, false, err
	}
	operation, err := loadLifecycleOperation(ctx, database.db, operationID)
	if errors.Is(err, os.ErrNotExist) {
		return LifecycleOperation{}, false, nil
	} // concurrent retirement
	if err != nil {
		return LifecycleOperation{}, false, err
	}
	if operation.Kind != LifecyclePurge {
		return LifecycleOperation{}, false, nil
	}
	if operation.Before[0].ID != id {
		return LifecycleOperation{}, false, ErrLifecycleOperationConflict
	}
	return operation, true, nil
}

func checkPurgeRegistry(registry Registry, operation LifecycleOperation) error {
	reserved := lifecycleContextResources(operation.Before)
	for resource := range lifecycleContextResources(registry.Contexts) {
		if _, conflict := reserved[resource]; conflict {
			return lifecycleConflictReason("context_changed", "purged context or terminal identity was registered again")
		}
	}
	return nil
}

func reconcileLifecyclePurge(ctx context.Context, handle *LifecycleOperationHandle, deleter LifecycleSessionDeleter) (complete, effects bool, resultErr error) {
	operation := handle.Operation
	if err := checkPurgeRegistry(handle.Registry, operation); err != nil {
		return false, false, err
	}
	before, target := operation.Before[0], *operation.Purge
	absent, err := verifyTerminalPurgeTarget(ctx, target, before.Launcher.Session)
	if err != nil {
		return false, false, err
	}
	if absent {
		return completeTerminalPurge(ctx, handle, false)
	}
	if deleter == nil {
		return false, false, errors.New("terminal purge deleter is unavailable")
	}
	effectErr := deleter(ctx, before, target)
	// Once any observer sees a replacement, later absence cannot erase that
	// evidence. Persist the conflict even if another actor removes it meanwhile.
	if errors.Is(effectErr, ErrLifecycleOperationConflict) {
		return false, true, effectErr
	}
	// A lost acknowledgement is resolved by observation, including conflicts.
	// Use a bounded fresh context even when the subprocess exhausted its deadline.
	checkCtx, cancel := mutationCompensationContext(ctx)
	defer cancel()
	absent, err = verifyTerminalPurgeTarget(checkCtx, target, before.Launcher.Session)
	if err != nil {
		return false, true, errors.Join(effectErr, err)
	}
	if absent {
		return completeTerminalPurge(checkCtx, handle, true)
	}
	if errors.Is(effectErr, ErrTerminalPurgeProgress) {
		return false, true, nil
	}
	if effectErr != nil {
		return false, true, effectErr
	}
	return false, true, errors.New("herdr session directory remains after delete")
}

func completeTerminalPurge(ctx context.Context, handle *LifecycleOperationHandle, effects bool) (bool, bool, error) {
	if err := handle.CompleteContext(ctx, handle.Registry); err != nil {
		var unknown *statefile.CommitOutcomeUnknownError
		if !errors.As(err, &unknown) {
			return false, effects, err
		}
		// The lifecycle and registry locks still exclude competing operations.
		// Read the exact row from a fresh snapshot, even if the caller's deadline
		// expired with the acknowledgement. A missing database is not evidence:
		// this query must confirm absence in the already-open state database.
		checkCtx, cancel := mutationCompensationContext(ctx)
		defer cancel()
		_, loadErr := loadLifecycleOperation(checkCtx, handle.database.db, handle.original.ID)
		if !errors.Is(loadErr, os.ErrNotExist) {
			return false, effects, errors.Join(err, loadErr)
		}
		registry, _, loadErr := loadRegistrySnapshotDatabase(checkCtx, handle.database)
		if loadErr != nil {
			return false, effects, errors.Join(err, loadErr)
		}
		if conflict := checkPurgeRegistry(registry, handle.original); conflict != nil {
			return false, effects, errors.Join(err, conflict)
		}
		handle.active = false
	}
	return true, effects, nil
}

// DeletePurgeTarget uses Herdr's existing name-based stop/delete interface.
// Repeated guards narrow, but cannot eliminate, the race between the last path
// observation and Herdr resolving that name. There is no upstream identity lock.
// Stop and delete occur on separate passes; no mutable progress flag is trusted.
func (manager HerdrManager) DeletePurgeTarget(ctx context.Context, before Context, target LifecyclePurgeTarget) error {
	if ctx == nil {
		return errors.New("terminal purge context is nil")
	}
	if err := before.Validate(); err != nil {
		return err
	}
	if before.Launcher.Kind != LauncherHerdr || before.Launcher.Session == "default" {
		return errors.New("purge requires a named Herdr session")
	}
	if err := validatePurgeTarget(target); err != nil {
		return err
	}
	if manager.Root != target.Root {
		return lifecycleConflictReason("root_identity_changed", "Herdr manager root differs from purge pin")
	}
	name := before.Launcher.Session
	absent, err := verifyTerminalPurgeTarget(ctx, target, name)
	if err != nil || absent {
		return err
	}
	if manager.Runner == nil || !filepath.IsAbs(manager.Executable) {
		return errors.New("herdr purge runner or executable is unavailable")
	}
	// Default Herdr discovery derives its root from XDG_CONFIG_HOME. Recovery
	// must use the captured root even if the daemon's environment later changed.
	// Injected runners implement this same manager-root contract themselves.
	manager.Runner = bindPurgeRunnerRoot(manager.Runner, target.Root)
	// Guard both sides of discovery, and require every claimed field. A missing
	// running boolean must not authorize delete of a possibly live session.
	output, listErr := manager.output(ctx, "session", "list", "--json")
	absent, err = verifyTerminalPurgeTarget(ctx, target, name)
	if err != nil || absent {
		return err
	}
	if listErr != nil {
		return listErr
	}
	listed, err := parseHerdrObservationList(output)
	if err != nil {
		return fmt.Errorf("decode Herdr purge discovery: %w", err)
	}
	var info herdrSessionInfo
	found := false
	for _, entry := range listed {
		if entry.Name != name {
			continue
		}
		if found {
			return errors.New("herdr purge discovery contains duplicate session")
		}
		if err := manager.validateSessionInfo(entry, name); err != nil {
			return err
		}
		info, found = entry, true
	}
	if !found {
		return errors.New("herdr purge discovery omitted existing directory")
	}
	absent, err = verifyTerminalPurgeTarget(ctx, target, name)
	if err != nil || absent {
		return err
	}
	verb := "delete"
	if info.Running {
		verb = "stop"
	}
	effectErr := manager.run(ctx, "session", verb, name, "--json")
	checkCtx, cancel := mutationCompensationContext(ctx)
	defer cancel()
	absent, err = verifyTerminalPurgeTarget(checkCtx, target, name)
	if err != nil {
		return errors.Join(effectErr, err)
	}
	if absent {
		return nil
	}
	if effectErr != nil {
		return effectErr
	}
	if info.Running {
		return ErrTerminalPurgeProgress
	}
	return errors.New("herdr session directory remains after delete")
}

// Keep the general runner's environment unchanged for non-purge callers.
func bindPurgeRunnerRoot(runner HerdrCommandRunner, root string) HerdrCommandRunner {
	switch runner.(type) {
	case ExecCommandRunner, *ExecCommandRunner:
		return purgeExecCommandRunner{configHome: filepath.Dir(root)}
	default:
		return runner
	}
}

type purgeExecCommandRunner struct{ configHome string }

func (runner purgeExecCommandRunner) CombinedOutput(ctx context.Context, executable string, args ...string) ([]byte, error) {
	return execHerdrCommand(ctx, runner.configHome, executable, args...)
}
