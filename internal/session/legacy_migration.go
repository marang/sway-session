package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/marang/sway-session/internal/statefile"
)

// LegacyMigrationResult describes the runtime state copied by an explicit
// pre-1.0 JSON-to-SQLite migration. Source files remain untouched as a backup.
type LegacyMigrationResult struct {
	Migrated                   bool
	Contexts                   int
	Layout                     bool
	ApplicationSession         bool
	ApplicationAttempts        int
	TerminalActivity           int
	SkippedApplicationAttempts int
	SkippedTerminalActivity    int
	CommitReconciled           bool
}

type legacyRuntimeState struct {
	registry       Registry
	layout         LayoutSnapshot
	application    ApplicationSessionState
	activity       TerminalActivityState
	hasRegistry    bool
	hasLayout      bool
	hasApplication bool
	hasActivity    bool
}

// MigrateLegacyState atomically copies all recognized legacy JSON runtime
// documents into state.sqlite3. It is idempotent once a current database
// exists and deliberately never deletes the source documents.
func MigrateLegacyState(ctx context.Context, root string) (LegacyMigrationResult, error) {
	if ctx == nil {
		return LegacyMigrationResult{}, errors.New("legacy migration context is nil")
	}
	var result LegacyMigrationResult
	err := WithRegistryLockContext(ctx, root, func(rootDirectory *statefile.LockedPrivateDirectory) error {
		if database, openErr := openStateDatabase(ctx, root, false); openErr == nil {
			return database.Close()
		} else if !errors.Is(openErr, os.ErrNotExist) {
			var legacyError *LegacyStateError
			if !errors.As(openErr, &legacyError) && !errors.Is(openErr, ErrUninitializedStateDatabase) {
				return openErr
			}
		}

		return withBoundedPrivateDirectoryLockContext(ctx, filepath.Join(root, legacyApplicationSessionDirectory), "legacy application state", func(applicationDirectory *statefile.LockedPrivateDirectory) error {
			return withBoundedPrivateDirectoryLockContext(ctx, filepath.Join(root, legacyTerminalActivityDirectory), "legacy terminal activity", func(activityDirectory *statefile.LockedPrivateDirectory) error {
				legacy, loadErr := loadLegacyRuntimeStateLocked(ctx, rootDirectory, applicationDirectory, activityDirectory)
				if loadErr != nil {
					return loadErr
				}
				if !legacy.hasRegistry && !legacy.hasLayout && !legacy.hasApplication && !legacy.hasActivity {
					return errors.New("no legacy sway-session JSON state was found")
				}
				legacy, skippedAttempts, skippedActivity := filterLegacyRuntimeState(legacy)
				result = LegacyMigrationResult{
					Contexts: len(legacy.registry.Contexts), Layout: legacy.hasLayout,
					ApplicationSession: legacy.hasApplication, ApplicationAttempts: len(legacy.application.Attempts),
					TerminalActivity: len(legacy.activity.Terminals), SkippedApplicationAttempts: skippedAttempts,
					SkippedTerminalActivity: skippedActivity,
				}
				database, openErr := openStateDatabaseWithInitializer(ctx, root, true, true, func(ctx context.Context, tx stateTransaction) error {
					return importLegacyRuntimeState(ctx, tx, legacy)
				})
				if openErr != nil {
					var unknown *statefile.CommitOutcomeUnknownError
					if errors.As(openErr, &unknown) {
						reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
						defer cancel()
						visible, verifyErr := legacyRuntimeStateVisible(reconcileCtx, root, legacy)
						if verifyErr == nil && visible {
							result.Migrated = true
							result.CommitReconciled = true
							return nil
						}
						return errors.Join(openErr, verifyErr)
					}
					return openErr
				}
				result.Migrated = database.initialized
				if closeErr := database.Close(); closeErr != nil {
					return fmt.Errorf("close migrated state database: %w", closeErr)
				}
				return nil
			})
		})
	})
	return result, err
}

func legacyRuntimeStateVisible(ctx context.Context, root string, expected legacyRuntimeState) (bool, error) {
	registry, err := ReadRegistrySnapshotContext(ctx, root)
	if err != nil {
		return false, err
	}
	wantRegistry := expected.registry
	if !expected.hasRegistry {
		wantRegistry = emptyRegistry()
	}
	if !reflect.DeepEqual(registry, wantRegistry) {
		return false, nil
	}

	var layout LayoutSnapshot
	layoutErr := LayoutStoreFor(root).LoadIntoContext(ctx, &layout)
	if expected.hasLayout {
		if layoutErr != nil || !reflect.DeepEqual(layout, expected.layout) {
			return false, layoutErr
		}
	} else if !errors.Is(layoutErr, os.ErrNotExist) {
		return false, layoutErr
	}

	var application ApplicationSessionState
	applicationErr := ApplicationSessionStoreFor(root).LoadIntoContext(ctx, &application)
	if expected.hasApplication {
		if applicationErr != nil || !equalApplicationSessionState(application, expected.application) {
			return false, applicationErr
		}
	} else if !errors.Is(applicationErr, os.ErrNotExist) {
		return false, applicationErr
	}

	activity, err := ReadTerminalActivitySnapshotContext(ctx, root)
	if err != nil || !equalTerminalActivityState(activity, expected.activity) {
		return false, err
	}
	return true, nil
}

func equalApplicationSessionState(left ApplicationSessionState, right ApplicationSessionState) bool {
	left.Attempts = append([]ApplicationLaunchAttempt(nil), left.Attempts...)
	right.Attempts = append([]ApplicationLaunchAttempt(nil), right.Attempts...)
	sort.Slice(left.Attempts, func(i, j int) bool { return left.Attempts[i].ContextID < left.Attempts[j].ContextID })
	sort.Slice(right.Attempts, func(i, j int) bool { return right.Attempts[i].ContextID < right.Attempts[j].ContextID })
	if left.Version != right.Version || left.CompositorID != right.CompositorID || len(left.Attempts) != len(right.Attempts) {
		return false
	}
	for index := range left.Attempts {
		if left.Attempts[index].ContextID != right.Attempts[index].ContextID ||
			!left.Attempts[index].StartedAt.Equal(right.Attempts[index].StartedAt) {
			return false
		}
	}
	return true
}

func equalTerminalActivityState(left TerminalActivityState, right TerminalActivityState) bool {
	left.Terminals = append([]TerminalActivity(nil), left.Terminals...)
	right.Terminals = append([]TerminalActivity(nil), right.Terminals...)
	sort.Slice(left.Terminals, func(i, j int) bool { return left.Terminals[i].ContextID < left.Terminals[j].ContextID })
	sort.Slice(right.Terminals, func(i, j int) bool { return right.Terminals[i].ContextID < right.Terminals[j].ContextID })
	return reflect.DeepEqual(left, right)
}

func loadLegacyRuntimeStateLocked(
	ctx context.Context,
	rootDirectory *statefile.LockedPrivateDirectory,
	applicationDirectory *statefile.LockedPrivateDirectory,
	activityDirectory *statefile.LockedPrivateDirectory,
) (legacyRuntimeState, error) {
	state := legacyRuntimeState{
		registry:    emptyRegistry(),
		application: ApplicationSessionState{Version: ApplicationSessionSchemaVersion, Attempts: []ApplicationLaunchAttempt{}},
		activity:    emptyTerminalActivityState(),
	}
	var err error
	state.hasRegistry, err = loadOptionalLegacyDocumentLocked(ctx, rootDirectory, legacyContextsFilename, (*Registry).Validate, &state.registry)
	if err != nil {
		return state, fmt.Errorf("load legacy context registry: %w", err)
	}
	state.hasLayout, err = loadOptionalLegacyDocumentLocked(ctx, rootDirectory, legacyLayoutFilename, (*LayoutSnapshot).Validate, &state.layout)
	if err != nil {
		return state, fmt.Errorf("load legacy layout: %w", err)
	}
	state.hasApplication, err = loadOptionalLegacyDocumentLocked(
		ctx, applicationDirectory, legacyApplicationSessionFilename,
		(*ApplicationSessionState).Validate, &state.application,
	)
	if err != nil {
		return state, fmt.Errorf("load legacy application session: %w", err)
	}
	state.hasActivity, err = loadOptionalLegacyDocumentLocked(
		ctx, activityDirectory, legacyTerminalActivityFilename,
		(*TerminalActivityState).Validate, &state.activity,
	)
	if err != nil {
		return state, fmt.Errorf("load legacy terminal activity: %w", err)
	}
	return state, nil
}

func loadOptionalLegacyDocumentLocked[T any](
	ctx context.Context,
	directory *statefile.LockedPrivateDirectory,
	name string,
	validate func(*T) error,
	target *T,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	data, err := directory.Read(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := decodeDatabasePayload("legacy "+name, data, target); err != nil {
		return false, err
	}
	if validate != nil {
		if err := validate(target); err != nil {
			return false, fmt.Errorf("validate legacy %s: %w", name, err)
		}
	}
	return true, nil
}

func filterLegacyRuntimeState(legacy legacyRuntimeState) (legacyRuntimeState, int, int) {
	applicationContexts := make(map[ContextID]struct{}, len(legacy.registry.Contexts))
	terminalContexts := make(map[ContextID]struct{}, len(legacy.registry.Contexts))
	for _, contextValue := range legacy.registry.Contexts {
		if contextValue.App != nil {
			applicationContexts[contextValue.ID] = struct{}{}
		}
		if contextValue.Launcher.Kind == LauncherHerdr && contextValue.Launcher.Terminal != nil {
			terminalContexts[contextValue.ID] = struct{}{}
		}
	}
	filteredAttempts := legacy.application.Attempts[:0]
	for _, attempt := range legacy.application.Attempts {
		if _, exists := applicationContexts[attempt.ContextID]; exists {
			filteredAttempts = append(filteredAttempts, attempt)
		}
	}
	skippedAttempts := len(legacy.application.Attempts) - len(filteredAttempts)
	legacy.application.Attempts = filteredAttempts

	filteredActivity := legacy.activity.Terminals[:0]
	for _, activity := range legacy.activity.Terminals {
		if _, exists := terminalContexts[activity.ContextID]; exists {
			filteredActivity = append(filteredActivity, activity)
		}
	}
	skippedActivity := len(legacy.activity.Terminals) - len(filteredActivity)
	legacy.activity.Terminals = filteredActivity
	return legacy, skippedAttempts, skippedActivity
}

func importLegacyRuntimeState(ctx context.Context, tx stateTransaction, legacy legacyRuntimeState) error {
	registry := legacy.registry
	if !legacy.hasRegistry {
		registry = emptyRegistry()
	}
	if err := saveRegistryBulkTx(ctx, tx, registry, 0); err != nil {
		return err
	}
	if legacy.hasLayout {
		payload, err := marshalDatabasePayload("legacy layout", legacy.layout)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO layout_state (id, encoding_version, payload) VALUES (1, ?, ?)", LayoutSchemaVersion, payload); err != nil {
			return fmt.Errorf("import legacy layout: %w", err)
		}
	}
	if legacy.hasApplication {
		if _, err := tx.ExecContext(ctx, "INSERT INTO application_session (id, compositor_id) VALUES (1, ?)", legacy.application.CompositorID); err != nil {
			return fmt.Errorf("import legacy application session: %w", err)
		}
		for _, attempt := range legacy.application.Attempts {
			if _, err := tx.ExecContext(ctx, "INSERT INTO application_launch_attempts (context_id, started_at) VALUES (?, ?)", attempt.ContextID, attempt.StartedAt.UTC().Format(time.RFC3339Nano)); err != nil {
				return fmt.Errorf("import legacy application launch attempt for %s: %w", attempt.ContextID, err)
			}
		}
	}
	if legacy.hasActivity {
		if err := saveTerminalActivityTx(ctx, tx, legacy.activity); err != nil {
			return fmt.Errorf("import legacy terminal activity: %w", err)
		}
	}
	return nil
}
