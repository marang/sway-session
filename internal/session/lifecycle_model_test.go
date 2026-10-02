package session

import (
	"strings"
	"testing"
	"time"
)

func TestLifecycleModelAcceptsImmediateRetryAndBaselineMark(t *testing.T) {
	operation := lifecycleStoreTestRegister(1)
	operation.Targets[0].HadMark = true
	if err := operation.Validate(); err != nil {
		t.Fatal(err)
	}
	operation.Kind = LifecycleRebind
	operation.Before = operation.After
	old := operation.Targets[0]
	old.ContainerID++
	old.WantMark = false
	operation.Targets = append(operation.Targets, old)
	if err := operation.Validate(); err != nil {
		t.Fatalf("two pinned windows for one context: %v", err)
	}
}

func TestLifecycleModelRejectsInvalidDurableIntents(t *testing.T) {
	for name, mutate := range map[string]func(*LifecycleOperation){
		"version":               func(op *LifecycleOperation) { op.Version++ },
		"id":                    func(op *LifecycleOperation) { op.ID = "invalid" },
		"phase":                 func(op *LifecycleOperation) { op.Phase = "unknown" },
		"kind":                  func(op *LifecycleOperation) { op.Kind = "unknown" },
		"unsupported_kind":      func(op *LifecycleOperation) { op.Kind = "purge" },
		"terminal_context":      func(op *LifecycleOperation) { op.After[0] = validRegistry().Contexts[0] },
		"epoch_empty":           func(op *LifecycleOperation) { op.CompositorID = "" },
		"epoch_bound":           func(op *LifecycleOperation) { op.CompositorID = strings.Repeat("e", 257) },
		"time":                  func(op *LifecycleOperation) { op.UpdatedAt = time.Time{} },
		"reason":                func(op *LifecycleOperation) { op.Reason = "unbounded free text" },
		"register_original":     func(op *LifecycleOperation) { op.Before = op.After },
		"register_unmark":       func(op *LifecycleOperation) { op.Targets[0].WantMark = false },
		"duplicate_target":      func(op *LifecycleOperation) { op.Targets = append(op.Targets, op.Targets[0]) },
		"wrong_target_context":  func(op *LifecycleOperation) { op.Targets[0].ContextID = testContextID },
		"wrong_target_identity": func(op *LifecycleOperation) { op.Targets[0].Identity.WaylandAppID = "another-app" },
		"wrong_desired_rebind": func(op *LifecycleOperation) {
			op.Kind = LifecycleRebind
			before, _ := cloneLifecycleOperation(*op)
			op.Before = before.After
			op.After[0].App.Identity.WaylandAppID = "changed-app"
		},
	} {
		t.Run(name, func(t *testing.T) {
			operation := lifecycleStoreTestRegister(1)
			mutate(&operation)
			if err := operation.Validate(); err == nil {
				t.Fatal("invalid operation accepted")
			}
		})
	}
}
