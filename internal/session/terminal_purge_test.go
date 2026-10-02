package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type purgeFixture struct {
	Base   string
	Root   string
	Herdr  string
	Before Context
}

func newPurgeFixture(t *testing.T) purgeFixture {
	t.Helper()
	base := lifecycleFixtureDir(t)
	fixture := purgeFixture{Base: base, Root: filepath.Join(base, "state"), Herdr: filepath.Join(base, "herdr"), Before: validRegistry().Contexts[0]}
	if err := os.MkdirAll(fixture.sessionPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.sessionPath(), "running"), []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RegistryStoreFor(fixture.Root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{fixture.Before}}); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateTerminalActivity(fixture.Root, func(state *TerminalActivityState) error {
		_, _, err := RecordTerminalCreatedAt(state, fixture.Before.ID, lifecycleCrashNow)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return fixture
}
func (fixture purgeFixture) sessionPath() string {
	return filepath.Join(fixture.Herdr, "sessions", fixture.Before.Launcher.Session)
}
func (fixture purgeFixture) begin(t *testing.T) LifecycleOperation {
	t.Helper()
	var operation LifecycleOperation
	err := WithTerminalLifecycleLockContext(t.Context(), fixture.Root, func() error {
		var err error
		operation, err = StartTerminalPurgeContext(t.Context(), fixture.Root, fixture.Before, fixture.Herdr, lifecycleCrashNow)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return operation
}
func (fixture purgeFixture) manager(runner *purgeFixtureRunner) HerdrManager {
	return HerdrManager{Root: fixture.Herdr, Executable: "/fixture/herdr", Runner: runner}
}

type purgeFixtureRunner struct {
	fixture  purgeFixture
	boundary func(string)
	fail     string
	loseAck  bool
	lists    func([]herdrSessionInfo) []herdrSessionInfo
}

func (runner *purgeFixtureRunner) CombinedOutput(ctx context.Context, executable string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if executable != "/fixture/herdr" || len(args) < 3 || args[0] != "session" {
		return nil, fmt.Errorf("unexpected command: %s %v", executable, args)
	}
	verb := args[1]
	if runner.boundary != nil {
		runner.boundary("before-" + verb)
	}
	if runner.fail == verb {
		return nil, errors.New("fixture busy/unavailable")
	}
	path := runner.fixture.sessionPath()
	if verb == "list" {
		if !slices.Equal(args, []string{"session", "list", "--json"}) {
			return nil, errors.New("unexpected discovery arguments")
		}
		infos := []herdrSessionInfo{}
		if _, err := os.Stat(path); err == nil {
			_, liveErr := os.Stat(filepath.Join(path, "running"))
			infos = append(infos, herdrSessionInfo{Name: runner.fixture.Before.Launcher.Session, Running: liveErr == nil, SessionDir: path, SocketPath: filepath.Join(path, "herdr.sock")})
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if runner.lists != nil {
			infos = runner.lists(infos)
		}
		output, err := json.Marshal(herdrSessionList{Sessions: infos})
		if runner.boundary != nil {
			runner.boundary("after-list")
		}
		return output, err
	}
	if !slices.Equal(args, []string{"session", verb, runner.fixture.Before.Launcher.Session, "--json"}) {
		return nil, errors.New("unexpected effect arguments")
	}
	var err error
	switch verb {
	case "stop":
		err = os.Remove(filepath.Join(path, "running"))
	case "delete":
		if _, statErr := os.Stat(filepath.Join(path, "running")); !errors.Is(statErr, os.ErrNotExist) {
			return nil, errors.New("fixture refuses live deletion")
		}
		err = os.RemoveAll(path) // Fixture effect only; production always calls Herdr.
	default:
		return nil, fmt.Errorf("unexpected effect %q", verb)
	}
	if err != nil {
		return nil, err
	}
	if err := lifecycleFixturePublish(filepath.Join(runner.fixture.Base, verb+"-effect.json"), true); err != nil {
		return nil, err
	}
	if runner.boundary != nil {
		runner.boundary("after-" + verb)
	}
	if runner.loseAck {
		return nil, errors.New("fixture lost effect acknowledgement")
	}
	return []byte(`{"ok":true}`), nil
}

func assertPurgeReserved(t *testing.T, fixture purgeFixture, operation LifecycleOperation) {
	t.Helper()
	registry := lifecycleStoreTestRegistry(t, fixture.Root)
	if len(registry.Contexts) != 0 {
		t.Fatalf("purge remains restore eligible: %+v", registry)
	}
	activity, err := ReadTerminalActivitySnapshot(fixture.Root)
	if err != nil || len(activity.Terminals) != 0 {
		t.Fatalf("purge did not cascade activity: %+v %v", activity, err)
	}
	found, exists, err := FindPendingTerminalPurgeContext(t.Context(), fixture.Root, fixture.Before.ID)
	if err != nil || !exists || found.ID != operation.ID {
		t.Fatalf("exact context lookup: %+v %v %v", found, exists, err)
	}
	blocked, err := BlockedLifecycleContextIDsContext(t.Context(), fixture.Root)
	if _, exists := blocked[fixture.Before.ID]; err != nil || !exists {
		t.Fatalf("purge not reserved for restore: %+v %v", blocked, err)
	}
	for _, sameID := range []bool{true, false} {
		replacement := fixture.Before
		if !sameID {
			replacement.ID = thirdContextID
		}
		if err := CheckLifecycleOperationConflictsContext(t.Context(), fixture.Root, []Context{replacement}); !errors.Is(err, ErrLifecycleOperationConflict) {
			t.Fatalf("preflight reused purge identity: %v", err)
		}
		registry.Contexts = []Context{replacement}
		if err := RegistryStoreFor(fixture.Root).Save(registry); !errors.Is(err, ErrLifecycleOperationConflict) {
			t.Fatalf("registry reused purge identity: %v", err)
		}
	}
}

func TestTerminalPurgeBeginAndBoundedNativeRecovery(t *testing.T) {
	fixture := newPurgeFixture(t)
	operation := fixture.begin(t)
	assertPurgeReserved(t, fixture, operation)
	if _, err := StartTerminalPurgeContext(t.Context(), fixture.Root, fixture.Before, fixture.Herdr, lifecycleCrashNow); !errors.Is(err, ErrLifecycleOperationConflict) {
		t.Fatalf("duplicate begin: %v", err)
	}
	runner := &purgeFixtureRunner{fixture: fixture}
	manager := fixture.manager(runner)
	outcome, err := ReconcileLifecycleOperationWithPurgeContext(t.Context(), fixture.Root, operation.ID, nil, manager.DeletePurgeTarget, lifecycleCrashNow)
	if err != nil || outcome.Status != "pending" || !outcome.Effects {
		t.Fatalf("stop step: %+v %v", outcome, err)
	}
	if _, err := os.Stat(fixture.sessionPath()); err != nil {
		t.Fatalf("stop also deleted: %v", err)
	}
	pending, err := LoadLifecycleOperationContext(t.Context(), fixture.Root, operation.ID)
	if err != nil || pending.Attempts != 0 {
		t.Fatalf("progress counted as failure: %+v %v", pending, err)
	}
	outcome, err = ReconcileLifecycleOperationWithPurgeContext(t.Context(), fixture.Root, operation.ID, nil, manager.DeletePurgeTarget, lifecycleCrashNow)
	if err != nil || outcome.Status != "completed" {
		t.Fatalf("delete step: %+v %v", outcome, err)
	}
	for range 3 {
		outcome, err = ReconcileLifecycleOperationWithPurgeContext(t.Context(), fixture.Root, operation.ID, nil, manager.DeletePurgeTarget, lifecycleCrashNow)
		if err != nil || outcome.Status != "completed" {
			t.Fatalf("retired retry: %+v %v", outcome, err)
		}
	}
	if _, exists, err := FindPendingTerminalPurgeContext(t.Context(), fixture.Root, fixture.Before.ID); err != nil || exists {
		t.Fatalf("retirement retained intent: %v %v", exists, err)
	}
	if err := RegistryStoreFor(fixture.Root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{fixture.Before}}); err != nil {
		t.Fatalf("completed reservation retained: %v", err)
	}
}

func TestTerminalPurgeReplacementIsDurableConflict(t *testing.T) {
	for _, replaceRoot := range []bool{false, true} {
		t.Run(fmt.Sprint("root=", replaceRoot), func(t *testing.T) {
			fixture := newPurgeFixture(t)
			operation := fixture.begin(t)
			path := fixture.sessionPath()
			if replaceRoot {
				path = fixture.Herdr
			}
			if err := os.Rename(path, path+"-original"); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(fixture.sessionPath(), 0o700); err != nil {
				t.Fatal(err)
			}
			calls := 0
			deleter := func(context.Context, Context, LifecyclePurgeTarget) error { calls++; return nil }
			result, err := ReconcileLifecycleOperationWithPurgeContext(t.Context(), fixture.Root, operation.ID, nil, deleter, lifecycleCrashNow)
			if !errors.Is(err, ErrLifecycleOperationConflict) || result.Status != "conflict" || calls != 0 {
				t.Fatalf("replacement authorized effect: %+v %v calls=%d", result, err, calls)
			}
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			// The replacement's later absence cannot silently erase the durable block.
			result, err = ReconcileLifecycleOperationWithPurgeContext(t.Context(), fixture.Root, operation.ID, nil, deleter, lifecycleCrashNow.Add(time.Hour))
			if err != nil || result.Status != "conflict" {
				t.Fatalf("restart lost block: %+v %v", result, err)
			}
			if err := RetryLifecycleOperationContext(t.Context(), fixture.Root, operation.ID, lifecycleCrashNow.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			pending, err := LoadLifecycleOperationContext(t.Context(), fixture.Root, operation.ID)
			if err != nil || !reflect.DeepEqual(pending.Purge, operation.Purge) {
				t.Fatalf("retry changed pin: %+v %v", pending, err)
			}
			if err := CancelLifecycleOperationContext(t.Context(), fixture.Root, operation.ID, lifecycleCrashNow.Add(time.Hour)); err == nil {
				t.Fatal("purge cancellation resurrects ambiguous target")
			}
			assertPurgeReserved(t, fixture, operation)
		})
	}
}

func TestTerminalPurgeGuardsAroundDiscoveryAndEffects(t *testing.T) {
	for _, point := range []string{"after-list", "after-stop", "after-delete"} {
		t.Run(point, func(t *testing.T) {
			fixture := newPurgeFixture(t)
			if point == "after-delete" {
				if err := os.Remove(filepath.Join(fixture.sessionPath(), "running")); err != nil {
					t.Fatal(err)
				}
			}
			operation := fixture.begin(t)
			replaced := false
			runner := &purgeFixtureRunner{fixture: fixture, boundary: func(observed string) {
				if observed != point || replaced {
					return
				}
				replaced = true
				if point != "after-delete" {
					if err := os.Rename(fixture.sessionPath(), fixture.sessionPath()+"-old"); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Mkdir(fixture.sessionPath(), 0o700); err != nil {
					t.Fatal(err)
				}
			}}
			result, err := ReconcileLifecycleOperationWithPurgeContext(t.Context(), fixture.Root, operation.ID, nil, fixture.manager(runner).DeletePurgeTarget, lifecycleCrashNow)
			if !errors.Is(err, ErrLifecycleOperationConflict) || result.Status != "conflict" {
				t.Fatalf("guard missed replacement: %+v %v", result, err)
			}
			if point == "after-list" {
				if _, err := os.Stat(filepath.Join(fixture.Base, "stop-effect.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("stopped after discovery replacement")
				}
			}
		})
	}
}

func TestTerminalPurgeRetriesNilUnavailableBusyAndLostAck(t *testing.T) {
	for _, failure := range []string{"nil", "list", "stop", "delete", "lost-delete-ack"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newPurgeFixture(t)
			if failure == "delete" || failure == "lost-delete-ack" {
				if err := os.Remove(filepath.Join(fixture.sessionPath(), "running")); err != nil {
					t.Fatal(err)
				}
			}
			operation := fixture.begin(t)
			runner := &purgeFixtureRunner{fixture: fixture, fail: failure, loseAck: failure == "lost-delete-ack"}
			var deleter LifecycleSessionDeleter = fixture.manager(runner).DeletePurgeTarget
			if failure == "nil" {
				deleter = nil
			}
			outcome, err := ReconcileLifecycleOperationWithPurgeContext(t.Context(), fixture.Root, operation.ID, nil, deleter, lifecycleCrashNow)
			if failure == "lost-delete-ack" {
				if err != nil || outcome.Status != "completed" {
					t.Fatalf("absence did not resolve lost ack: %+v %v", outcome, err)
				}
				return
			}
			if err == nil || outcome.Status != "retry" {
				t.Fatalf("failure not retryable: %+v %v", outcome, err)
			}
			loaded, err := LoadLifecycleOperationContext(t.Context(), fixture.Root, operation.ID)
			if err != nil || loaded.Attempts != 1 || !loaded.NextAttempt.Equal(lifecycleCrashNow.Add(2*time.Second)) {
				t.Fatalf("missing backoff: %+v %v", loaded, err)
			}
			for range 10 {
				outcome, err = ReconcileLifecycleOperationWithPurgeContext(t.Context(), fixture.Root, operation.ID, nil, deleter, loaded.NextAttempt)
				if err == nil || outcome.Status != "retry" {
					t.Fatalf("repeated failure: %+v %v", outcome, err)
				}
				next, err := LoadLifecycleOperationContext(t.Context(), fixture.Root, operation.ID)
				if err != nil || next.NextAttempt.Sub(loaded.NextAttempt) > 64*time.Second {
					t.Fatalf("unbounded backoff: %+v %v", next, err)
				}
				loaded = next
			}
			if err := os.RemoveAll(fixture.sessionPath()); err != nil {
				t.Fatal(err)
			}
			outcome, err = ReconcileLifecycleOperationContext(t.Context(), fixture.Root, operation.ID, nil, loaded.NextAttempt)
			if err != nil || outcome.Status != "completed" {
				t.Fatalf("nil deleter cannot retire absence: %+v %v", outcome, err)
			}
		})
	}
}

func TestTerminalPurgeImmutablePinAndNoRollback(t *testing.T) {
	for _, mutation := range []string{"root", "inode", "birth", "exists", "before", "rollback"} {
		t.Run(mutation, func(t *testing.T) {
			fixture := newPurgeFixture(t)
			operation := fixture.begin(t)
			err := WithLifecycleOperationContext(t.Context(), fixture.Root, operation.ID, func(handle *LifecycleOperationHandle) error {
				changed := handle.Operation
				switch mutation {
				case "root":
					changed.Purge.Root += "-new"
				case "inode":
					changed.Purge.SessionInode++
				case "birth":
					changed.Purge.SessionBirthNS++
				case "exists":
					changed.Purge.SessionExists = false
				case "before":
					changed.Before[0].Label = "new"
				case "rollback":
					changed.Phase = LifecycleRollback
				}
				return handle.UpdateContext(t.Context(), changed)
			})
			if err == nil {
				t.Fatal("store accepted immutable pin mutation")
			}
			loaded, err := LoadLifecycleOperationContext(t.Context(), fixture.Root, operation.ID)
			if err != nil || !reflect.DeepEqual(loaded, operation) {
				t.Fatalf("failed mutation changed intent: %+v %v", loaded, err)
			}
		})
	}
}

func TestTerminalPurgeConcurrentRetriesAndBackup(t *testing.T) {
	fixture := newPurgeFixture(t)
	operation := fixture.begin(t)
	output := backupTestOutput(t)
	if _, err := BackupState(t.Context(), fixture.Root, output); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateStateBackup(t.Context(), output); err != nil {
		t.Fatal(err)
	}
	recovered := filepath.Join(fixture.Base, "recovered")
	if _, err := RecoverState(t.Context(), recovered, output, true); err != nil {
		t.Fatal(err)
	}
	fixture.Root = recovered
	assertPurgeReserved(t, fixture, operation)
	manager := fixture.manager(&purgeFixtureRunner{fixture: fixture})
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 3 {
				result, err := ReconcileLifecycleOperationWithPurgeContext(t.Context(), fixture.Root, operation.ID, nil, manager.DeletePurgeTarget, lifecycleCrashNow)
				if err != nil || result.Status != "pending" && result.Status != "completed" {
					t.Errorf("concurrent retry: %+v %v", result, err)
				}
			}
		})
	}
	workers.Wait()
	if _, err := LoadLifecycleOperationContext(t.Context(), fixture.Root, operation.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("concurrent retries did not converge: %v", err)
	}
}

func TestTerminalPurgeTypedIdentityUsesExistingLauncherReservationKind(t *testing.T) {
	fixture := newPurgeFixture(t)
	fixture.Before.Launcher.Terminal.Identity = &TerminalIdentity{Kind: TerminalIdentityProject, Project: "purge-test"}
	if err := RegistryStoreFor(fixture.Root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{fixture.Before}}); err != nil {
		t.Fatal(err)
	}
	fixture.begin(t)
	other := fixture.Before
	other.ID = thirdContextID
	other.Launcher.Session = "different-name"
	if err := RegistryStoreFor(fixture.Root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{other}}); !errors.Is(err, ErrLifecycleOperationConflict) {
		t.Fatalf("typed identity reused: %v", err)
	}
	if _, err := BackupState(t.Context(), fixture.Root, backupTestOutput(t)); err != nil {
		t.Fatalf("typed reservation incompatible with existing schema: %v", err)
	}
}

func TestTerminalPurgeRejectsUntrustedDiscovery(t *testing.T) {
	for _, mode := range []string{"omitted", "duplicate", "default", "directory", "socket"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newPurgeFixture(t)
			operation := fixture.begin(t)
			runner := &purgeFixtureRunner{fixture: fixture, lists: func(infos []herdrSessionInfo) []herdrSessionInfo {
				switch mode {
				case "omitted":
					return []herdrSessionInfo{}
				case "duplicate":
					return append(infos, infos[0])
				case "default":
					infos[0].Default = true
				case "directory":
					infos[0].SessionDir += "-elsewhere"
				case "socket":
					infos[0].SocketPath += "-elsewhere"
				}
				return infos
			}}
			result, err := ReconcileLifecycleOperationWithPurgeContext(t.Context(), fixture.Root, operation.ID, nil, fixture.manager(runner).DeletePurgeTarget, lifecycleCrashNow)
			if err == nil || result.Status != "retry" {
				t.Fatalf("accepted discovery: %+v %v", result, err)
			}
			if _, err := os.Stat(filepath.Join(fixture.Base, "stop-effect.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("effect followed invalid discovery")
			}
		})
	}
}

func TestTerminalPurgeAbsentPinsAndBirthEvidence(t *testing.T) {
	for _, rootAbsent := range []bool{false, true} {
		for _, replacement := range []bool{false, true} {
			t.Run(fmt.Sprintf("rootAbsent=%v/replacement=%v", rootAbsent, replacement), func(t *testing.T) {
				fixture := newPurgeFixture(t)
				path := fixture.sessionPath()
				if rootAbsent {
					path = fixture.Herdr
				}
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
				operation := fixture.begin(t)
				if replacement {
					if err := os.MkdirAll(fixture.sessionPath(), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				result, err := ReconcileLifecycleOperationContext(t.Context(), fixture.Root, operation.ID, nil, lifecycleCrashNow)
				if replacement {
					if !errors.Is(err, ErrLifecycleOperationConflict) || result.Status != "conflict" {
						t.Fatalf("absence pin retargeted: %+v %v", result, err)
					}
				} else if err != nil || result.Status != "completed" {
					t.Fatalf("absence not completed: %+v %v", result, err)
				}
			})
		}
	}
	fixture := newPurgeFixture(t)
	target, err := ObserveTerminalPurgeTarget(t.Context(), fixture.Herdr, fixture.Before.Launcher.Session)
	if err != nil {
		t.Fatal(err)
	}
	weaker := target
	weaker.RootBirthNS = 0
	weaker.SessionBirthNS = 0
	if absent, err := verifyTerminalPurgeTarget(t.Context(), weaker, fixture.Before.Launcher.Session); err != nil || absent {
		t.Fatalf("dev/inode fallback rejected: %v %v", absent, err)
	}
	target.SessionBirthNS++
	if _, err := verifyTerminalPurgeTarget(t.Context(), target, fixture.Before.Launcher.Session); !errors.Is(err, ErrLifecycleOperationConflict) {
		t.Fatalf("birth mismatch ignored: %v", err)
	}
}

func TestTerminalPurgeRejectsUnsafePaths(t *testing.T) {
	for _, part := range []string{"root", "sessions", "session"} {
		t.Run(part, func(t *testing.T) {
			fixture := newPurgeFixture(t)
			operation := fixture.begin(t)
			path := fixture.sessionPath()
			if part == "root" {
				path = fixture.Herdr
			} else if part == "sessions" {
				path = filepath.Dir(path)
			}
			if err := os.Rename(path, path+"-original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(path+"-original", path); err != nil {
				t.Fatal(err)
			}
			calls := 0
			_, err := ReconcileLifecycleOperationWithPurgeContext(t.Context(), fixture.Root, operation.ID, nil, func(context.Context, Context, LifecyclePurgeTarget) error { calls++; return nil }, lifecycleCrashNow)
			if err == nil || calls != 0 {
				t.Fatalf("unsafe path authorized effects: %v calls=%d", err, calls)
			}
		})
	}
}

func TestTerminalPurgeCancellationPersistsRetryAfterEffectTimeout(t *testing.T) {
	fixture := newPurgeFixture(t)
	operation := fixture.begin(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	deleter := func(context.Context, Context, LifecyclePurgeTarget) error { cancel(); return context.DeadlineExceeded }
	result, err := ReconcileLifecycleOperationWithPurgeContext(ctx, fixture.Root, operation.ID, nil, deleter, lifecycleCrashNow)
	if err == nil || result.Status != "retry" {
		t.Fatalf("timeout not recorded: %+v %v", result, err)
	}
	loaded, err := LoadLifecycleOperationContext(t.Context(), fixture.Root, operation.ID)
	if err != nil || loaded.Attempts != 1 || loaded.NextAttempt.IsZero() {
		t.Fatalf("timeout lost durable backoff: %+v %v", loaded, err)
	}
}

func TestTerminalPurgeProductionRunnerUsesRecordedRoot(t *testing.T) {
	printenv, err := exec.LookPath("printenv")
	if err != nil {
		t.Skip("printenv unavailable")
	}
	fixture := newPurgeFixture(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(fixture.Base, "changed-environment"))
	for _, original := range []HerdrCommandRunner{ExecCommandRunner{}, &ExecCommandRunner{}} {
		scoped := bindPurgeRunnerRoot(original, fixture.Herdr)
		output, err := scoped.CombinedOutput(t.Context(), printenv, "XDG_CONFIG_HOME")
		if err != nil || strings.TrimSpace(string(output)) != filepath.Dir(fixture.Herdr) {
			t.Fatalf("purge used environment instead of durable root: %q %v", output, err)
		}
		output, err = original.CombinedOutput(t.Context(), printenv, "XDG_CONFIG_HOME")
		if err != nil || strings.TrimSpace(string(output)) != os.Getenv("XDG_CONFIG_HOME") {
			t.Fatalf("purge mutated generic runner or process environment: %q %v", output, err)
		}
	}
	if err := validatePurgeTarget(LifecyclePurgeTarget{Root: filepath.Join(fixture.Base, "other-root")}); err == nil {
		t.Fatal("root unrepresentable by XDG_CONFIG_HOME accepted")
	}
}

func TestTerminalPurgeObservedConflictSurvivesSubsequentAbsence(t *testing.T) {
	fixture := newPurgeFixture(t)
	operation := fixture.begin(t)
	deleter := func(context.Context, Context, LifecyclePurgeTarget) error {
		if err := os.RemoveAll(fixture.sessionPath()); err != nil {
			t.Fatal(err)
		}
		return lifecycleConflictReason("session_identity_changed", "replacement observed during native effect")
	}
	outcome, err := ReconcileLifecycleOperationWithPurgeContext(t.Context(), fixture.Root, operation.ID, nil, deleter, lifecycleCrashNow)
	if !errors.Is(err, ErrLifecycleOperationConflict) || outcome.Status != "conflict" {
		t.Fatalf("absence erased observed conflict: %+v %v", outcome, err)
	}
	loaded, err := LoadLifecycleOperationContext(t.Context(), fixture.Root, operation.ID)
	if err != nil || !loaded.Blocked {
		t.Fatalf("conflict not durable: %+v %v", loaded, err)
	}
}
