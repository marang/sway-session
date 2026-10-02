package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

func TestStateOperationsRejectsInvalidArgumentsBeforeDependencies(t *testing.T) {
	id := string(testManagedContextID)
	for _, arguments := range [][]string{
		{"extra"}, {"--retry"}, {"--retry="}, {"--cancel", "label"}, {"--after", "label"},
		{"--retry", id, "--cancel", id}, {"--after", id, "--retry", id},
		{"--socket", "/fixture/sway.sock"}, {"--retry", id, "--socket", "relative"},
		{"--retry", id, "--socket", "/fixture/../socket"}, {"--yes"},
	} {
		t.Run(strings.Join(arguments, " "), func(t *testing.T) {
			var out, errout bytes.Buffer
			code := runWith(append([]string{"--json", "state", "operations"}, arguments...), strings.NewReader(""), &out, &errout, dependencies{})
			if code != exitUsage || out.Len() != 0 {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, &out, &errout)
			}
			assertStateDiagnostic(t, errout.Bytes(), "usage")
		})
	}
}

func TestStateOperationsListMissingRootHasNoEffects(t *testing.T) {
	parent := t.TempDir()
	deps := defaultDependencies(strings.NewReader(""))
	deps.stateRoot = func() (string, error) { return filepath.Join(parent, "uncreated"), nil }
	deps.newSwayClient = func(string) swayRequester { t.Fatal("read-only listing created transport"); return nil }
	var out, errout bytes.Buffer
	if code := runWith([]string{"--json", "state", "operations"}, strings.NewReader(""), &out, &errout, deps); code != exitSuccess {
		t.Fatalf("code=%d err=%s", code, &errout)
	}
	var result commandResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Command != "state operations" || result.StateOperations == nil || len(result.StateOperations.Operations) != 0 {
		t.Fatalf("wrong result: %s", &out)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("read-only listing created state: %v %v", entries, err)
	}
}

func TestStateOperationsDispatchAndPartialJSON(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "cancel"}[cancel], func(t *testing.T) {
			ctx, stop := context.WithCancel(t.Context())
			stop()
			id := string(testManagedContextID)
			flag := "--retry"
			if cancel {
				flag = "--cancel"
			}
			now := time.Unix(100, 0)
			deps := dependencies{
				stateRoot: func() (string, error) { return "/fixture/state", nil },
				now:       func() time.Time { return now },
				runLifecycleOperation: func(got context.Context, root, operation string, rollback bool, socket string, at time.Time, _ dependencies) (sessionstate.LifecycleOutcome, error) {
					if got != ctx || !errors.Is(got.Err(), context.Canceled) || root != "/fixture/state" || operation != id || rollback != cancel || socket != "/fixture/sway.sock" || !at.Equal(now) {
						t.Fatal("dispatch lost arguments or cancellation")
					}
					return sessionstate.LifecycleOutcome{OperationID: id, Kind: sessionstate.LifecycleRebind, Status: "retry", Reason: "observation_or_effect_failed", Effects: true}, context.Canceled
				},
			}
			var out, errout bytes.Buffer
			if code := runWithContext(ctx, []string{"--json", "state", "operations", flag, id, "--socket", "/fixture/sway.sock"}, strings.NewReader(""), &out, &errout, deps); code != exitOperation {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, &out, &errout)
			}
			var result commandResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.StateOperations == nil || result.StateOperations.Outcome == nil || !result.StateOperations.Outcome.Effects {
				t.Fatalf("lost partial result: %s", &out)
			}
			assertStateDiagnostic(t, errout.Bytes(), "lifecycle_operation_retry")
			if !strings.Contains(errout.String(), "--retry "+id) {
				t.Fatal("lost exact resume guidance")
			}
		})
	}
}

func newStateOperationFixture(t *testing.T) (string, sessionstate.LifecycleOperation) {
	t.Helper()
	_, _, _, app, now := testApplicationRuntime(t)
	now = now.UTC()
	root := filepath.Join(t.TempDir(), "state")
	if err := sessionstate.RegistryStoreFor(root).Save(sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{}}); err != nil {
		t.Fatal(err)
	}
	operation := sessionstate.LifecycleOperation{
		ID: "22222222-2222-4222-8222-222222222222", Version: sessionstate.LifecycleOperationVersion,
		Kind: sessionstate.LifecycleRegister, Phase: sessionstate.LifecycleForward,
		Before: []sessionstate.Context{}, After: []sessionstate.Context{app},
		Targets:      []sessionstate.LifecycleWindowTarget{{ContextID: app.ID, ContainerID: 42, Identity: app.App.Identity, WantMark: true}},
		CompositorID: "fixture-compositor", UpdatedAt: now, NextAttempt: now,
	}
	stored, err := sessionstate.BeginLifecycleOperationContext(t.Context(), root, operation)
	if err != nil {
		t.Fatal(err)
	}
	return root, stored
}

func TestStateOperationsListsMetadataAndRespectsCursor(t *testing.T) {
	root, operation := newStateOperationFixture(t)
	result, err := listLifecycleOperationSummaries(t.Context(), root, "", 32, operation.UpdatedAt)
	if err != nil || len(result.Operations) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	value := result.Operations[0]
	if value.OperationID != operation.ID || value.Phase != sessionstate.LifecycleForward || value.Status != "pending" || len(value.ContextIDs) != 1 {
		t.Fatalf("summary=%+v", value)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"launcher", "org.example.App", "compositor_id", "container_id", "before", "after"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("full intent disclosed: %s", encoded)
		}
	}
	next, err := listLifecycleOperationSummaries(t.Context(), root, operation.ID, 32, operation.UpdatedAt)
	if err != nil || len(next.Operations) != 0 {
		t.Fatalf("cursor ignored: %+v %v", next, err)
	}
}

func TestStateOperationsCancelPersistsRollbackWhenTransportUnavailable(t *testing.T) {
	root, operation := newStateOperationFixture(t)
	t.Setenv("SWAYSOCK", "")
	outcome, err := runLifecycleOperationAction(t.Context(), root, operation.ID, true, "", operation.UpdatedAt, dependencies{})
	if err == nil || outcome.Status != "rollback" || outcome.OperationID != operation.ID {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	stored, err := sessionstate.LoadLifecycleOperationContext(t.Context(), root, operation.ID)
	if err != nil || stored.Phase != sessionstate.LifecycleRollback {
		t.Fatalf("rollback not durable: %+v %v", stored, err)
	}
}

func TestStateOperationsRejectsNilClientWithoutLosingIntent(t *testing.T) {
	root, operation := newStateOperationFixture(t)
	deps := dependencies{newSwayClient: func(string) swayRequester { return nil }}
	outcome, err := runLifecycleOperationAction(t.Context(), root, operation.ID, false, "/fixture/sway.sock", operation.UpdatedAt, deps)
	if err == nil || outcome.OperationID != operation.ID || outcome.Status != "pending" || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if _, err := sessionstate.LoadLifecycleOperationContext(t.Context(), root, operation.ID); err != nil {
		t.Fatalf("lost intent after unavailable client: %v", err)
	}
}

func TestStateOperationsRetryAdvancesOneStepWithOriginalClient(t *testing.T) {
	root, operation := newStateOperationFixture(t)
	app := operation.After[0]
	appID, sandbox := app.App.Identity.WaylandAppID, app.App.Identity.SandboxAppID
	node := &Node{ID: 42, Type: "con", AppID: &appID, SandboxAppID: &sandbox}
	client := &lifecycleFixtureRequester{tree: daemonTree("98: fixture", node)}
	mark, _ := app.ID.Mark()
	client.afterCommand = func() { node.Marks = []string{mark} }
	deps := dependencies{newSwayClient: func(string) swayRequester { return client }}
	outcome, err := runLifecycleOperationAction(t.Context(), root, operation.ID, false, "/fixture/socket", operation.UpdatedAt, deps)
	if err != nil || outcome.Status != "pending" || !outcome.Effects || len(client.commands) != 1 || !client.closed {
		t.Fatalf("outcome=%+v err=%v commands=%v closed=%v", outcome, err, client.commands, client.closed)
	}
	if _, err := sessionstate.LoadLifecycleOperationContext(t.Context(), root, operation.ID); err != nil {
		t.Fatalf("bounded action drained intent: %v", err)
	}
	outcome, err = runLifecycleOperationAction(t.Context(), root, operation.ID, false, "/fixture/socket", operation.UpdatedAt, deps)
	if err != nil || outcome.Status != "completed" || outcome.Effects || len(client.commands) != 1 {
		t.Fatalf("resumption=%+v err=%v commands=%v", outcome, err, client.commands)
	}
}
