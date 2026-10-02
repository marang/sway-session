package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/swayipc"
)

func TestLifecycleRebindResumesAndRollsBackObservedMarks(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "cancel"}[rollback], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			before := flatpakApplicationContext("org.example.Old", "org.example.Old")
			before.ID = testContextID
			after := flatpakApplicationContext("org.example.New", "org.example.New")
			after.ID = testContextID
			if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{before}}); err != nil {
				t.Fatal(err)
			}
			old := appWindow(41, false, "org.example.Old", "", "", "org.example.Old")
			replacement := appWindow(42, true, "org.example.New", "", "", "org.example.New")
			mark, _ := testContextID.Mark()
			old.Marks = []string{mark}
			client := &mutationSwayClient{tree: lifecycleApplicationTree(old, replacement)}
			operation, err := beginApplicationOperation(t.Context(), root, client, LifecycleRebind, []Context{before}, []Context{after}, []int64{42})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := ReconcileLifecycleOperationContext(t.Context(), root, operation.ID, client, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.Effects || containsMark(old.Marks, mark) || containsMark(replacement.Marks, mark) {
				t.Fatal("first pass did not remove only the original mark")
			}
			if rollback {
				// Simulate interruption after the next external effect but before any
				// progress write. The fresh observation must drive compensation.
				replacement.Marks = []string{mark}
				if err := CancelLifecycleOperationContext(t.Context(), root, operation.ID, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			for pass := 0; pass < 4; pass++ {
				// Each invocation reloads journal state; no in-memory progress survives.
				outcome, err = ReconcileLifecycleOperationContext(t.Context(), root, operation.ID, client, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				if outcome.Status == "completed" {
					break
				}
			}
			if outcome.Status != "completed" {
				t.Fatalf("operation did not converge: %+v", outcome)
			}
			if containsMark(old.Marks, mark) != rollback || containsMark(replacement.Marks, mark) == rollback {
				t.Fatal("final mark ownership differs from durable phase")
			}
			var registry Registry
			if err := RegistryStoreFor(root).LoadInto(&registry); err != nil {
				t.Fatal(err)
			}
			want := after
			if rollback {
				want = before
			}
			if len(registry.Contexts) != 1 || !sameApplicationMutation(registry.Contexts[0], want) {
				t.Fatalf("wrong completed registry: %+v", registry)
			}
			ops, _, err := ListLifecycleOperationsContext(t.Context(), root, "", 10)
			if err != nil || len(ops) != 0 {
				t.Fatalf("completed operation retained journal: %v %v", ops, err)
			}
		})
	}
}

func TestLifecycleRegistrationRetainsPreexistingMarkOnCancel(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	registered := flatpakApplicationContext("org.example.App", "org.example.App")
	registered.ID = testContextID
	node := appWindow(42, true, "org.example.App", "", "", "org.example.App")
	mark, _ := testContextID.Mark()
	node.Marks = []string{mark}
	client := &mutationSwayClient{tree: applicationTree(node)}
	operation, err := beginApplicationOperation(t.Context(), root, client, LifecycleRegister, nil, []Context{registered}, []int64{42})
	if err != nil {
		t.Fatal(err)
	}
	if err := CancelLifecycleOperationContext(t.Context(), root, operation.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	outcome, err := ReconcileLifecycleOperationContext(t.Context(), root, operation.ID, client, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != "completed" || !containsMark(node.Marks, mark) || client.commandCalls != 0 {
		t.Fatalf("rollback destroyed preexisting ownership: %+v", outcome)
	}
}

func TestLifecycleRefusesChangedWindowOrCompositor(t *testing.T) {
	for _, drift := range []string{"window", "compositor", "foreign_mark"} {
		t.Run(drift, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			registered := flatpakApplicationContext("org.example.App", "org.example.App")
			registered.ID = testContextID
			node := appWindow(42, true, "org.example.App", "", "", "org.example.App")
			client := &lifecycleEpochClient{mutationSwayClient: mutationSwayClient{tree: applicationTree(node)}, epoch: "first"}
			operation, err := beginApplicationOperation(t.Context(), root, client, LifecycleRegister, nil, []Context{registered}, []int64{42})
			if err != nil {
				t.Fatal(err)
			}
			switch drift {
			case "window":
				node.PID++
			case "compositor":
				client.epoch = "replacement"
			case "foreign_mark":
				id, _ := NewContextID()
				mark, _ := id.Mark()
				node.Marks = append(node.Marks, mark)
			}
			outcome, err := ReconcileLifecycleOperationContext(t.Context(), root, operation.ID, client, time.Now())
			if !errors.Is(err, ErrLifecycleOperationConflict) || outcome.Status != "conflict" || client.commandCalls != 0 {
				t.Fatalf("changed target was not isolated: %+v %v", outcome, err)
			}
			blocked, err := BlockedLifecycleContextIDsContext(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := blocked[testContextID]; !exists {
				t.Fatal("conflicted context became restore eligible")
			}
		})
	}
}

type lifecycleEpochClient struct {
	mutationSwayClient
	epoch string
}

func (client *lifecycleEpochClient) LifecycleCompositorID(context.Context) (string, error) {
	return client.epoch, nil
}

func TestLifecycleRegistrationObservesSuccessfulAcknowledgement(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	registered := flatpakApplicationContext("org.example.App", "org.example.App")
	registered.ID = testContextID
	base := &mutationSwayClient{tree: applicationTree(appWindow(42, true, "org.example.App", "", "", "org.example.App"))}
	client := &lifecycleAcknowledgementOnlyClient{mutationSwayClient: base}
	if err := RegisterApplicationContext(t.Context(), root, client, registered, 42); err == nil {
		t.Fatal("acknowledgement without observed effect was committed")
	}
	var registry Registry
	if err := RegistryStoreFor(root).LoadInto(&registry); err == nil && len(registry.Contexts) != 0 {
		t.Fatal("unobserved registration persisted")
	}
}

type lifecycleAcknowledgementOnlyClient struct{ *mutationSwayClient }

func (client *lifecycleAcknowledgementOnlyClient) RequestContext(ctx context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if kind == swayipc.RunCommand {
		return swayipc.Message{Type: kind, Payload: []byte(`[{"success":true}]`)}, nil
	}
	return client.mutationSwayClient.RequestContext(ctx, kind, payload)
}

func TestLifecycleCancelledOperationCannotReportRegistrationSuccess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	registered := flatpakApplicationContext("org.example.App", "org.example.App")
	registered.ID = testContextID
	client := &mutationSwayClient{tree: applicationTree(appWindow(42, true, "org.example.App", "", "", "org.example.App"))}
	operation, err := beginApplicationOperation(t.Context(), root, client, LifecycleRegister, nil, []Context{registered}, []int64{42})
	if err != nil {
		t.Fatal(err)
	}
	if err := CancelLifecycleOperationContext(t.Context(), root, operation.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := finishApplicationOperation(t.Context(), root, client, operation); err == nil {
		t.Fatal("rolled-back registration was reported successful")
	}
	// Also cover a separate reconciler completing cancellation before the CLI
	// resumes: absence of the journal alone never proves forward completion.
	if err := finishApplicationOperation(t.Context(), root, client, operation); err == nil {
		t.Fatal("missing journal row was reported as successful registration")
	}
}

func TestLifecycleRollbackRefusesAmbiguousContainerID(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	registered := flatpakApplicationContext("org.example.App", "org.example.App")
	registered.ID = testContextID
	node := appWindow(42, true, "org.example.App", "", "", "org.example.App")
	client := &mutationSwayClient{tree: applicationTree(node)}
	operation, err := beginApplicationOperation(t.Context(), root, client, LifecycleRegister, nil, []Context{registered}, []int64{42})
	if err != nil {
		t.Fatal(err)
	}
	mark, _ := registered.ID.Mark()
	node.Marks = []string{mark}
	client.tree = lifecycleApplicationTree(node, appWindow(42, false, "org.example.App", "", "", "org.example.App"))
	if err := CancelLifecycleOperationContext(t.Context(), root, operation.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	outcome, err := ReconcileLifecycleOperationContext(t.Context(), root, operation.ID, client, time.Now())
	if !errors.Is(err, ErrLifecycleOperationConflict) || outcome.Status != "conflict" || client.commandCalls != 0 {
		t.Fatalf("ambiguous target accepted as missing: %+v %v", outcome, err)
	}
	if _, err := LoadLifecycleOperationContext(t.Context(), root, operation.ID); err != nil {
		t.Fatalf("ambiguous rollback discarded durable intent: %v", err)
	}
}

func lifecycleApplicationTree(nodes ...*swayipc.TreeNode) *swayipc.TreeNode {
	return &swayipc.TreeNode{ID: 1, Type: "root", Nodes: []*swayipc.TreeNode{{ID: 2, Type: "output", Nodes: []*swayipc.TreeNode{{ID: 3, Type: "workspace", Name: "98", Nodes: nodes}}}}}
}

func TestLifecycleCancellationAfterCompositorRestartRequiresAbsentMarks(t *testing.T) {
	for _, hasMark := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "unowned_mark"}[hasMark], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			registered := flatpakApplicationContext("org.example.App", "org.example.App")
			registered.ID = testContextID
			client := &lifecycleEpochClient{mutationSwayClient: mutationSwayClient{tree: applicationTree(appWindow(42, true, "org.example.App", "", "", "org.example.App"))}, epoch: "original"}
			operation, err := beginApplicationOperation(t.Context(), root, client, LifecycleRegister, nil, []Context{registered}, []int64{42})
			if err != nil {
				t.Fatal(err)
			}
			replacement := appWindow(100, true, "org.example.Other", "", "", "org.example.Other")
			if hasMark {
				mark, _ := registered.ID.Mark()
				replacement.Marks = []string{mark}
			}
			client.epoch = "replacement"
			client.tree = applicationTree(replacement)
			if err := CancelLifecycleOperationContext(t.Context(), root, operation.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
			outcome, err := ReconcileLifecycleOperationContext(t.Context(), root, operation.ID, client, time.Now())
			if hasMark {
				if !errors.Is(err, ErrLifecycleOperationConflict) || outcome.Status != "conflict" {
					t.Fatalf("unknown mark accepted: %+v %v", outcome, err)
				}
			} else if err != nil || outcome.Status != "completed" {
				t.Fatalf("safe cancellation failed: %+v %v", outcome, err)
			}
			if client.commandCalls != 0 {
				t.Fatal("old compositor identity authorized a new command")
			}
		})
	}
}

func TestLifecycleCancelledSameIdentityRebindCannotReportSuccess(t *testing.T) {
	for _, retired := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback_pending", true: "rollback_retired"}[retired], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			registered := flatpakApplicationContext("org.example.App", "org.example.App")
			registered.ID = testContextID
			if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{registered}}); err != nil {
				t.Fatal(err)
			}
			old := appWindow(41, false, "org.example.App", "", "", "org.example.App")
			target := appWindow(42, true, "org.example.App", "", "", "org.example.App")
			mark, _ := registered.ID.Mark()
			old.Marks = []string{mark}
			client := &mutationSwayClient{tree: lifecycleApplicationTree(old, target)}
			operation, err := beginApplicationOperation(t.Context(), root, client, LifecycleRebind, []Context{registered}, []Context{registered}, []int64{42})
			if err != nil {
				t.Fatal(err)
			}
			if err := CancelLifecycleOperationContext(t.Context(), root, operation.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
			if retired {
				outcome, err := ReconcileLifecycleOperationContext(t.Context(), root, operation.ID, client, time.Now())
				if err != nil || outcome.Status != "completed" {
					t.Fatalf("complete rollback: %+v %v", outcome, err)
				}
			}
			if err := finishApplicationOperation(t.Context(), root, client, operation); err == nil {
				t.Fatal("cancelled rebind reported successful despite original mark arrangement")
			}
			if !containsMark(old.Marks, mark) || containsMark(target.Marks, mark) || client.commandCalls != 0 {
				t.Fatal("cancelled rebind changed its baseline")
			}
		})
	}
}
