package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

// The filesystem represents durable Herdr state while replies may be lost.
// Listing always reflects the current state, including across adapter instances.
type purgeLifecycleRunner struct {
	root         string
	running      bool
	busy         bool
	loseDelete   bool
	effects      []string
	beforeEffect func()
}

func (runner *purgeLifecycleRunner) CombinedOutput(_ context.Context, _ string, arguments ...string) ([]byte, error) {
	if len(arguments) < 3 || arguments[0] != "session" {
		return nil, errors.New("unexpected Herdr command")
	}
	path := filepath.Join(runner.root, "sessions", "lab-80")
	if arguments[1] == "list" {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return []byte(`{"sessions":[]}`), nil
		} else if err != nil {
			return nil, err
		}
		return []byte(herdrList(runner.root, runner.running)), nil
	}
	if len(arguments) != 4 || arguments[2] != "lab-80" || arguments[3] != "--json" {
		return nil, errors.New("wrong native purge target")
	}
	if runner.beforeEffect != nil {
		runner.beforeEffect()
	}
	runner.effects = append(runner.effects, arguments[1])
	switch arguments[1] {
	case "stop":
		if runner.busy {
			return nil, errors.New("active agent refused shutdown")
		}
		runner.running = false
	case "delete":
		if runner.running {
			return nil, errors.New("cannot delete running session")
		}
		if err := os.RemoveAll(path); err != nil {
			return nil, err
		}
		if runner.loseDelete {
			return nil, errors.New("delete acknowledgement lost")
		}
	default:
		return nil, errors.New("unexpected native effect")
	}
	return []byte(`{}`), nil
}

func newPendingPurgeFixture(t *testing.T) (dependencies, sessionstate.Context, sessionstate.LifecycleOperation, *purgeLifecycleRunner) {
	t.Helper()
	deps := testDependencies(t)
	item := registerTestContext(t, deps)
	paths, _ := deps.herdrPaths()
	if err := os.MkdirAll(filepath.Join(paths.Root, "sessions", item.Launcher.Session), 0o700); err != nil {
		t.Fatal(err)
	}
	root, _ := deps.stateRoot()
	var operation sessionstate.LifecycleOperation
	err := sessionstate.WithTerminalLifecycleLockContext(t.Context(), root, func() error {
		var err error
		operation, err = sessionstate.StartTerminalPurgeContext(t.Context(), root, item, paths.Root, deps.now())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := &purgeLifecycleRunner{root: paths.Root, running: true}
	deps.herdrRunner = runner
	deps.herdrPaths = func() (sessionstate.HerdrPaths, error) {
		t.Fatal("pending purge resolved current configuration instead of recorded root")
		return sessionstate.HerdrPaths{}, errors.New("unreachable")
	}
	deps.newSwayClient = func(string) swayRequester { t.Fatal("purge requires no Sway connection"); return nil }
	return deps, item, operation, runner
}

func TestPendingPurgeExactContextRetryUsesRecordedRoot(t *testing.T) {
	deps, item, operation, runner := newPendingPurgeFixture(t)
	result, problem := executePurge(t.Context(), []string{"--yes", string(item.ID)}, strings.NewReader(""), &bytes.Buffer{}, true, deps)
	if problem != nil || !reflect.DeepEqual(result.Actions, []string{"purged"}) || result.StateOperations.Outcome.OperationID != operation.ID {
		t.Fatalf("retry result=%+v failure=%+v", result, problem)
	}
	if !reflect.DeepEqual(runner.effects, []string{"stop", "delete"}) {
		t.Fatalf("retry effects: %v", runner.effects)
	}
}

func TestPurgeFailureBeforeJournalLoadRetainsKindAndHonestRecoveryHint(t *testing.T) {
	deps, item, operation, runner := newPendingPurgeFixture(t)
	root, _ := deps.stateRoot()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, problem := finishPurgeCommand(ctx, root, item, operation.ID, deps.now(), deps)
	if problem == nil || result.StateOperations.Outcome.Kind != sessionstate.LifecyclePurge || len(runner.effects) != 0 {
		t.Fatalf("cancelled result=%+v problem=%+v effects=%v", result, problem, runner.effects)
	}
	var rendered bytes.Buffer
	if err := writeStateOperations(&rendered, result); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered.String(), "--cancel") || !strings.Contains(rendered.String(), "irreversible") || !strings.Contains(rendered.String(), "--retry "+operation.ID) {
		t.Fatalf("misleading interrupted purge output: %s", &rendered)
	}
	if _, err := sessionstate.LoadLifecycleOperationContext(t.Context(), root, operation.ID); err != nil {
		t.Fatalf("cancelled caller lost purge intent: %v", err)
	}
}

func TestDaemonPurgeRetriesBusySessionAndLostDeleteAcknowledgement(t *testing.T) {
	deps, item, operation, runner := newPendingPurgeFixture(t)
	root, _ := deps.stateRoot()
	now := deps.now().UTC()
	runner.busy = true
	adapter := lifecycleCoreAdapter{root: root, deleter: nativePurgeDeleter(deps.resolveProgram, runner)}
	first, err := adapter.Reconcile(t.Context(), now, "", 32)
	if err == nil || len(first.Outcomes) != 1 || first.Outcomes[0].Status != "retry" {
		t.Fatalf("busy result=%+v err=%v", first, err)
	}
	if ids, err := adapter.BlockedContextIDs(t.Context()); err != nil || !reflect.DeepEqual(ids, []sessionstate.ContextID{item.ID}) {
		t.Fatalf("reservation lost: %v %v", ids, err)
	}
	_, _ = adapter.Reconcile(t.Context(), now, "", 32)
	if len(runner.effects) != 1 {
		t.Fatalf("retry ignored backoff: %v", runner.effects)
	}
	runner.busy, runner.loseDelete = false, true
	// Recreate the adapter as after daemon restart; all recovery state is in SQLite.
	adapter = lifecycleCoreAdapter{root: root, deleter: nativePurgeDeleter(deps.resolveProgram, runner)}
	completed := false
	for pass := 1; pass <= 5; pass++ {
		result, _ := adapter.Reconcile(t.Context(), now.Add(time.Duration(pass)*time.Minute), "", 32)
		for _, outcome := range result.Outcomes {
			completed = completed || outcome.Status == "completed"
		}
	}
	if !completed || !reflect.DeepEqual(runner.effects, []string{"stop", "stop", "delete"}) {
		t.Fatalf("completion=%v effects=%v operation=%s", completed, runner.effects, operation.ID)
	}
	if ids, err := adapter.BlockedContextIDs(t.Context()); err != nil || len(ids) != 0 {
		t.Fatalf("completion retained reservations: %v %v", ids, err)
	}
}

func TestPurgeOperationHasNoRollbackHintAndCannotBeCancelled(t *testing.T) {
	deps, _, operation, runner := newPendingPurgeFixture(t)
	root, _ := deps.stateRoot()
	result, err := listLifecycleOperationSummaries(t.Context(), root, "", 32, deps.now())
	if err != nil {
		t.Fatal(err)
	}
	var rendered bytes.Buffer
	if err := writeStateOperations(&rendered, commandResult{StateOperations: &result}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered.String(), "--retry "+operation.ID) || strings.Contains(rendered.String(), "--cancel") {
		t.Fatalf("misleading purge recovery: %s", &rendered)
	}
	outcome, err := runLifecycleOperationAction(t.Context(), root, operation.ID, true, "", deps.now(), deps)
	if err == nil || len(runner.effects) != 0 {
		t.Fatalf("purge cancel outcome=%+v err=%v effects=%v", outcome, err, runner.effects)
	}
	if _, err := sessionstate.LoadLifecycleOperationContext(t.Context(), root, operation.ID); err != nil {
		t.Fatalf("refused cancellation lost intent: %v", err)
	}
	for range 4 {
		outcome, err = runLifecycleOperationAction(t.Context(), root, operation.ID, false, "", deps.now(), deps)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Status == "completed" {
			return
		}
	}
	t.Fatalf("socket-free retry did not complete: %+v", outcome)
}
