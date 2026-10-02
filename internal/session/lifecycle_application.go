package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"time"

	"github.com/marang/sway-session/internal/swayipc"
)

func beginApplicationOperation(ctx context.Context, root string, client SwayRequestClient, kind LifecycleOperationKind, before, after []Context, containers []int64) (LifecycleOperation, error) {
	var operation LifecycleOperation
	err := WithTerminalLifecycleLockContext(ctx, root, func() error {
		scoped, closeScope, err := lifecycleClientScope(ctx, client)
		if err != nil {
			return err
		}
		defer closeScope()
		client = scoped
		id, err := NewContextID()
		if err != nil {
			return err
		}
		epoch, tree, err := observeLifecycleTree(ctx, client, "")
		if err != nil {
			return err
		}
		operation = LifecycleOperation{ID: string(id), Version: LifecycleOperationVersion, Kind: kind, Phase: LifecycleForward, Before: append([]Context{}, before...), After: append([]Context{}, after...), Targets: []LifecycleWindowTarget{}, CompositorID: epoch, UpdatedAt: time.Now().UTC()}
		if kind == LifecycleRebind {
			old, err := findMarkedContainerInTree(tree, before[0].ID)
			if err != nil {
				return err
			}
			if old != 0 && old != containers[0] {
				target, err := captureLifecycleWindow(tree, before[0], old, false)
				if err != nil {
					return err
				}
				operation.Targets = append(operation.Targets, target)
			}
		}
		for index, registered := range after {
			if registered.App == nil {
				return errors.New("durable registration requires application contexts")
			}
			target, err := captureLifecycleWindow(tree, registered, containers[index], true)
			if err != nil {
				return err
			}
			marked, err := findMarkedContainerInTree(tree, registered.ID)
			if err != nil {
				return err
			}
			if kind == LifecycleRegister && marked != 0 && marked != target.ContainerID {
				return lifecycleConflict("registration mark belongs to another container")
			}
			operation.Targets = append(operation.Targets, target)
		}
		persisted, err := BeginLifecycleOperationContext(ctx, root, operation)
		if err != nil {
			// Initial intent may have committed even when its acknowledgement did not.
			checkCtx, cancel := mutationCompensationContext(ctx)
			defer cancel()
			loaded, loadErr := LoadLifecycleOperationContext(checkCtx, root, operation.ID)
			if loadErr == nil && sameLifecycleIntent(loaded, operation) {
				operation = loaded
				return nil
			}
			return fmt.Errorf("record lifecycle operation %s before effects: %w", operation.ID, err)
		}
		operation = persisted
		return nil
	})
	return operation, err
}

func sameLifecycleIntent(left, right LifecycleOperation) bool {
	return left.ID == right.ID && left.Kind == right.Kind && left.CompositorID == right.CompositorID && reflect.DeepEqual(left.Before, right.Before) && reflect.DeepEqual(left.After, right.After) && reflect.DeepEqual(left.Targets, right.Targets)
}

func captureLifecycleWindow(tree *swayipc.TreeNode, registered Context, containerID int64, want bool) (LifecycleWindowTarget, error) {
	var target LifecycleWindowTarget
	node, err := findContainer(tree, containerID)
	if err != nil {
		return target, err
	}
	identity, eligible, err := compositorApplicationIdentity(node)
	if err != nil || !eligible || registered.App == nil || !applicationIdentitiesOverlap(registered.App.Identity, identity) {
		return target, lifecycleConflictReason("window_identity_changed", "selected container does not match the approved application identity")
	}
	marks, err := applicationContextMarks(node.Marks)
	if err != nil {
		return target, err
	}
	for _, id := range marks {
		if id != registered.ID {
			return target, lifecycleConflict("selected container carries another context mark")
		}
	}
	mark, _ := registered.ID.Mark()
	target = LifecycleWindowTarget{ContextID: registered.ID, ContainerID: containerID, Identity: identity, PID: node.PID, WindowID: node.Window, HadMark: slices.Contains(node.Marks, mark), WantMark: want}
	return target, nil
}

func observeLifecycleTree(ctx context.Context, client SwayRequestClient, expected string) (string, *swayipc.TreeNode, error) {
	epoch, err := lifecycleCompositorID(ctx, client)
	if err != nil {
		return "", nil, err
	}
	if expected != "" && epoch != expected {
		return "", nil, lifecycleConflictReason("compositor_changed", "compositor lifetime changed")
	}
	tree, err := requestApplicationTree(ctx, client)
	if err != nil {
		return "", nil, err
	}
	after, err := lifecycleCompositorID(ctx, client)
	if err != nil {
		return "", nil, err
	}
	if after != epoch {
		return "", nil, lifecycleConflictReason("compositor_changed", "compositor changed during observation")
	}
	return epoch, tree, nil
}

// reconcileLifecycleApplication performs at most one mark change. Every pass
// starts from a fresh tree; no persisted command cursor is trusted as evidence.
func reconcileLifecycleApplication(ctx context.Context, handle *LifecycleOperationHandle, client SwayRequestClient) (complete, effects bool, resultErr error) {
	scoped, closeScope, scopeErr := lifecycleClientScope(ctx, client)
	if scopeErr != nil {
		return false, false, scopeErr
	}
	defer closeScope()
	client = scoped
	operation := handle.Operation
	if err := checkApplicationOperationRegistry(handle.Registry, operation); err != nil {
		return false, false, err
	}
	epoch, tree, err := observeLifecycleTree(ctx, client, "")
	if err != nil {
		return false, false, err
	}
	if epoch != operation.CompositorID {
		if operation.Phase != LifecycleRollback {
			return false, false, lifecycleConflictReason("compositor_changed", "compositor lifetime changed")
		}
		// Container IDs from the previous lifetime cannot authorize any effect.
		// Explicit cancellation can still finish if every operation mark is absent
		// from the current compositor; otherwise preserve the conflict for review.
		for _, registered := range operation.After {
			marked, err := findMarkedContainerInTree(tree, registered.ID)
			if err != nil || marked != 0 {
				return false, false, lifecycleConflictReason("mark_ownership_changed", "operation mark exists in another compositor lifetime")
			}
		}
		if err := handle.CompleteContext(ctx, handle.Registry); err != nil {
			return false, false, err
		}
		return true, false, nil
	}
	if err := validateLifecycleTargets(tree, operation); err != nil {
		return false, false, err
	}
	// Remove unwanted marks before adding desired marks, avoiding an ambiguous
	// duplicate context mark during both forward rebind and rollback.
	for _, add := range []bool{false, true} {
		for _, target := range operation.Targets {
			want := target.WantMark
			if operation.Phase == LifecycleRollback {
				want = target.HadMark
			}
			if want != add {
				continue
			}
			node, nodeErr := findContainer(tree, target.ContainerID)
			if nodeErr != nil {
				continue
			} // rollback allows a target that has closed
			mark, _ := target.ContextID.Mark()
			if slices.Contains(node.Marks, mark) == want {
				continue
			}
			// The transport pins the connected compositor, not only its pathname.
			epoch, err := lifecycleCompositorID(ctx, client)
			if err != nil {
				return false, false, err
			}
			if epoch != operation.CompositorID {
				return false, false, lifecycleConflictReason("compositor_changed", "compositor changed before effect")
			}
			effects = true
			commandErr := SetContextMark(ctx, client, target.ContainerID, target.ContextID, want)
			_, observed, observeErr := observeLifecycleTree(ctx, client, operation.CompositorID)
			if observeErr != nil {
				return false, true, errors.Join(commandErr, observeErr)
			}
			if err := validateLifecycleTargets(observed, operation); err != nil {
				return false, true, err
			}
			updated, nodeErr := findContainer(observed, target.ContainerID)
			if nodeErr != nil {
				return false, true, lifecycleConflictReason("window_missing", "target closed during mark change")
			}
			if slices.Contains(updated.Marks, mark) != want {
				if commandErr != nil {
					return false, true, commandErr
				}
				return false, true, errors.New("compositor did not retain the requested context mark")
			}
			return false, true, nil
		}
	}
	candidate := handle.Registry
	candidate.Contexts = append([]Context{}, candidate.Contexts...)
	if operation.Phase == LifecycleForward {
		if operation.Kind == LifecycleRegister {
			for _, registered := range operation.After {
				if err := AddContext(&candidate, registered); err != nil {
					return false, false, err
				}
			}
			candidate.Preferences.DesktopIndicators = true
		} else {
			index, err := ResolveContext(candidate, string(operation.Before[0].ID))
			if err != nil {
				return false, false, err
			}
			candidate.Contexts[index] = operation.After[0]
		}
	}
	if err := handle.CompleteContext(ctx, candidate); err != nil {
		return false, false, err
	}
	return true, false, nil
}

func checkApplicationOperationRegistry(registry Registry, operation LifecycleOperation) error {
	for _, before := range operation.Before {
		index, err := ResolveContext(registry, string(before.ID))
		if err != nil || !reflect.DeepEqual(registry.Contexts[index], before) {
			return lifecycleConflictReason("context_changed", "context changed while operation was pending")
		}
	}
	if operation.Kind == LifecycleRegister {
		for _, after := range operation.After {
			if _, err := ResolveContext(registry, string(after.ID)); !errors.Is(err, ErrContextNotFound) {
				return lifecycleConflictReason("context_changed", "registration context already exists")
			}
		}
	}
	return nil
}

func validateLifecycleTargets(tree *swayipc.TreeNode, operation LifecycleOperation) error {
	for _, target := range operation.Targets {
		node, present, err := lifecycleTargetNode(tree, target.ContainerID)
		if err != nil {
			return err
		}
		if !present {
			if operation.Phase != LifecycleRollback {
				return lifecycleConflictReason("window_missing", "target container is no longer present")
			}
		} else {
			identity, eligible, err := compositorApplicationIdentity(node)
			if err != nil || !eligible || !reflect.DeepEqual(identity, target.Identity) || node.PID != target.PID || !reflect.DeepEqual(node.Window, target.WindowID) {
				return lifecycleConflictReason("window_identity_changed", "target container identity changed")
			}
			marks, err := applicationContextMarks(node.Marks)
			if err != nil {
				return lifecycleConflict("target marks are invalid")
			}
			for _, id := range marks {
				if id != target.ContextID {
					return lifecycleConflictReason("mark_ownership_changed", "target carries another context mark")
				}
			}
		}
		marked, err := findMarkedContainerInTree(tree, target.ContextID)
		if err != nil {
			return lifecycleConflictReason("mark_ambiguous", "context mark is ambiguous")
		}
		if marked != 0 {
			owned := false
			for _, other := range operation.Targets {
				if other.ContextID == target.ContextID && other.ContainerID == marked {
					owned = true
					break
				}
			}
			if !owned {
				return lifecycleConflictReason("mark_ownership_changed", "context mark moved to an unapproved container")
			}
		}
	}
	return nil
}

// finishApplicationOperation preserves synchronous CLI behavior while every
// intermediate step remains independently recoverable by the daemon.
func finishApplicationOperation(ctx context.Context, root string, client SwayRequestClient, operation LifecycleOperation) error {
	for step := 0; step <= len(operation.Targets)+1; step++ {
		outcome, err := ReconcileLifecycleOperationContext(ctx, root, operation.ID, client, time.Now())
		if err == nil && outcome.Status == "completed" {
			if outcome.Phase == LifecycleRollback {
				return fmt.Errorf("lifecycle operation %s was cancelled", operation.ID)
			}
			if outcome.Phase == LifecycleForward {
				return nil
			}
			committed, checkErr := confirmRetiredApplicationOperation(ctx, root, client, operation)
			if checkErr != nil {
				return fmt.Errorf("verify lifecycle operation %s completion: %w", operation.ID, checkErr)
			}
			if !committed {
				return fmt.Errorf("lifecycle operation %s forward completion could not be confirmed", operation.ID)
			}
			return nil
		}
		if err == nil && outcome.Status == "pending" {
			continue
		}
		if err == nil {
			err = fmt.Errorf("operation is %s (%s)", outcome.Status, outcome.Reason)
		}
		checkCtx, cancel := mutationCompensationContext(ctx)
		pending, loadErr := LoadLifecycleOperationContext(checkCtx, root, operation.ID)
		if errors.Is(loadErr, os.ErrNotExist) {
			committed, reconcileErr := confirmRetiredApplicationOperation(checkCtx, root, client, operation)
			cancel()
			if reconcileErr == nil && committed {
				return nil
			}
			return fmt.Errorf("lifecycle operation %s completion could not be confirmed: %w", operation.ID, errors.Join(err, reconcileErr))
		}
		if loadErr != nil {
			cancel()
			return fmt.Errorf("lifecycle operation %s: %w; cannot reconcile registry state: %v; leaving Sway marks unchanged for daemon reconciliation", operation.ID, err, loadErr)
		}
		if pending.Blocked {
			cancel()
			return fmt.Errorf("lifecycle operation %s blocked; inspect state operations, then retry or cancel: %w", operation.ID, err)
		}
		rollbackErr := CancelLifecycleOperationContext(checkCtx, root, operation.ID, time.Now())
		if rollbackErr == nil {
			for attempt := 0; attempt <= len(operation.Targets)+1; attempt++ {
				result, stepErr := ReconcileLifecycleOperationContext(checkCtx, root, operation.ID, client, time.Now())
				if stepErr != nil {
					rollbackErr = stepErr
					break
				}
				if result.Status == "completed" {
					break
				}
			}
		}
		cancel()
		if rollbackErr != nil {
			return fmt.Errorf("lifecycle operation %s requires recovery via state operations: %w", operation.ID, errors.Join(err, rollbackErr))
		}
		return err
	}
	return fmt.Errorf("lifecycle operation %s remains pending; inspect state operations", operation.ID)
}

func lifecycleTargetNode(tree *swayipc.TreeNode, id int64) (*swayipc.TreeNode, bool, error) {
	var found *swayipc.TreeNode
	count := 0
	if err := walkTreeNodes(tree, func(node *swayipc.TreeNode) {
		if node.ID == id {
			found = node
			count++
		}
	}); err != nil {
		return nil, false, err
	}
	if count > 1 {
		return nil, false, lifecycleConflictReason("window_ambiguous", "target container ID is ambiguous")
	}
	return found, count == 1, nil
}

// Retirement alone cannot distinguish a commit from cancellation, especially
// when a rebind changes only the window and Before/After contexts are equal.
func confirmRetiredApplicationOperation(ctx context.Context, root string, client SwayRequestClient, operation LifecycleOperation) (bool, error) {
	confirmed := false
	err := WithTerminalLifecycleLockContext(ctx, root, func() error {
		return InspectRegistryLockedContext(ctx, root, func(registry Registry) error {
			for _, wanted := range operation.After {
				index, err := ResolveContext(registry, string(wanted.ID))
				if err != nil || !sameApplicationMutation(registry.Contexts[index], wanted) {
					return nil
				}
			}
			scoped, closeScope, err := lifecycleClientScope(ctx, client)
			if err != nil {
				return err
			}
			defer closeScope()
			_, tree, err := observeLifecycleTree(ctx, scoped, operation.CompositorID)
			if err != nil {
				return err
			}
			if err := validateLifecycleTargets(tree, operation); err != nil {
				return err
			}
			for _, target := range operation.Targets {
				node, present, err := lifecycleTargetNode(tree, target.ContainerID)
				if err != nil {
					return err
				}
				if !present {
					return nil
				}
				mark, _ := target.ContextID.Mark()
				if slices.Contains(node.Marks, mark) != target.WantMark {
					return nil
				}
			}
			confirmed = true
			return nil
		})
	})
	return confirmed, err
}
