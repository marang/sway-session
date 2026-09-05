package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"time"

	"github.com/marang/sway-session/internal/swayipc"
)

type SwayRequestClient interface {
	RequestContext(context.Context, swayipc.MessageType, []byte) (swayipc.Message, error)
}

// NewApplicationContext creates the persistent application-level restore
// record after the launcher and live identity have been independently trusted.
func NewApplicationContext(id ContextID, entry DesktopEntry, window WindowApplication, launcher Launcher) (Context, error) {
	identity := window.Identity
	identity.StartupWMClass = entry.StartupWMClass
	context := Context{
		ID: id, Label: entry.Name, Provider: "desktop", State: ContextActive, Launcher: launcher,
		App: &Application{Identity: identity, DesiredOpen: true, RestorePolicy: ApplicationRestoreFollow},
	}
	if err := context.Validate(); err != nil {
		return Context{}, err
	}
	return context, nil
}

// RegisterApplicationContext commits the registry record and Sway mark as one
// compensating transaction. A state-save failure removes the mark; an
// ambiguous command response is always resolved by fresh GET_TREE evidence.
func RegisterApplicationContext(ctx context.Context, root string, client SwayRequestClient, context Context, containerID int64) error {
	return RegisterApplicationContexts(ctx, root, client, []Context{context}, []int64{containerID})
}

// RegisterApplicationContexts is the all-or-nothing batch variant used by the
// previewed current-workspace operation.
func RegisterApplicationContexts(ctx context.Context, root string, client SwayRequestClient, contexts []Context, containerIDs []int64) error {
	if len(contexts) == 0 || len(contexts) != len(containerIDs) {
		return errors.New("application registration batch is empty or misaligned")
	}
	seenContainers := make(map[int64]struct{}, len(containerIDs))
	for _, containerID := range containerIDs {
		if containerID <= 0 {
			return errors.New("application registration contains an invalid container ID")
		}
		if _, duplicate := seenContainers[containerID]; duplicate {
			return errors.New("application registration contains a duplicate container ID")
		}
		seenContainers[containerID] = struct{}{}
	}
	attemptedItems := 0
	_, err := UpdateRegistryContext(ctx, root, func(registry *Registry) error {
		for index := range contexts {
			if err := validateUnreferencedDesktopApproval(ctx, root, *registry, contexts[index].Launcher); err != nil {
				return err
			}
			if err := AddContext(registry, contexts[index]); err != nil {
				return err
			}
		}
		registry.Preferences.DesktopIndicators = true
		for index := range contexts {
			attemptedItems++
			if err := SetContextMark(ctx, client, containerIDs[index], contexts[index].ID, true); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		return nil
	}
	compensationCtx, cancelCompensation := mutationCompensationContext(ctx)
	defer cancelCompensation()
	committed, reconciliationErr := registryContainsApplicationMutations(compensationCtx, root, contexts)
	if reconciliationErr != nil {
		return fmt.Errorf("register application: %w; cannot reconcile registry state: %v; leaving Sway marks unchanged for daemon reconciliation", err, reconciliationErr)
	}
	if committed {
		return nil
	}
	var rollbackErrors []error
	if attemptedItems != 0 {
		for index := attemptedItems - 1; index >= 0; index-- {
			if rollbackErr := SetContextMark(compensationCtx, client, containerIDs[index], contexts[index].ID, false); rollbackErr != nil {
				rollbackErrors = append(rollbackErrors, rollbackErr)
			}
		}
	}
	if len(rollbackErrors) != 0 {
		return fmt.Errorf("register application: %w; additionally roll back Sway marks: %v", err, errors.Join(rollbackErrors...))
	}
	return err
}

// RebindApplicationContext replaces the exact live identity and typed launcher
// while transferring the context mark to the newly focused window.
func RebindApplicationContext(ctx context.Context, root string, client SwayRequestClient, expected Context, replacement Context, newContainerID int64) (Context, Context, error) {
	expectedRevision, err := ApplicationOperationContextRevision(expected)
	if err != nil {
		return Context{}, Context{}, err
	}
	var previous Context
	var applied Context
	oldContainerID := int64(0)
	removedOldMark := false
	addedNewMark := false
	attemptedOldRemoval := false
	attemptedNewMark := false
	_, err = UpdateRegistryContext(ctx, root, func(registry *Registry) error {
		index, err := ResolveContext(*registry, string(replacement.ID))
		if err != nil {
			return err
		}
		previous = registry.Contexts[index]
		if previous.App == nil {
			return errors.New("rebind is only available for desktop application contexts")
		}
		currentRevision, err := ApplicationOperationContextRevision(previous)
		if err != nil || currentRevision != expectedRevision {
			return errors.New("application context changed while rebind approval was pending")
		}
		if err := validateUnreferencedDesktopApproval(ctx, root, *registry, replacement.Launcher); err != nil {
			return err
		}
		applied = replacement
		applied.State = previous.State
		if applied.App == nil {
			return errors.New("rebind replacement is not a desktop application context")
		}
		applied.App.DesiredOpen = previous.App.DesiredOpen
		applied.App.RestorePolicy = previous.App.RestorePolicy
		candidate := *registry
		candidate.Contexts = append([]Context(nil), registry.Contexts...)
		candidate.Contexts[index] = applied
		if err := candidate.Validate(); err != nil {
			return err
		}
		oldContainerID, err = findMarkedContainer(ctx, client, previous.ID)
		if err != nil {
			return err
		}
		if oldContainerID != 0 && oldContainerID != newContainerID {
			attemptedOldRemoval = true
			if err := SetContextMark(ctx, client, oldContainerID, previous.ID, false); err != nil {
				return err
			}
			removedOldMark = true
		}
		if oldContainerID != newContainerID {
			attemptedNewMark = true
			if err := SetContextMark(ctx, client, newContainerID, previous.ID, true); err != nil {
				return err
			}
			addedNewMark = true
		}
		registry.Contexts[index] = applied
		return nil
	})
	if err == nil {
		return previous, applied, nil
	}
	compensationCtx, cancelCompensation := mutationCompensationContext(ctx)
	defer cancelCompensation()
	committed, reconciliationErr := registryContainsApplicationMutation(compensationCtx, root, applied)
	if reconciliationErr != nil {
		return Context{}, Context{}, fmt.Errorf("rebind application: %w; cannot reconcile registry state: %v; leaving Sway marks unchanged for daemon reconciliation", err, reconciliationErr)
	}
	if committed {
		return previous, applied, nil
	}
	var rollbackErrors []error
	if addedNewMark || attemptedNewMark {
		if rollbackErr := SetContextMark(compensationCtx, client, newContainerID, replacement.ID, false); rollbackErr != nil {
			rollbackErrors = append(rollbackErrors, rollbackErr)
		}
	}
	if removedOldMark || attemptedOldRemoval {
		if rollbackErr := SetContextMark(compensationCtx, client, oldContainerID, replacement.ID, true); rollbackErr != nil {
			rollbackErrors = append(rollbackErrors, rollbackErr)
		}
	}
	if len(rollbackErrors) != 0 {
		return Context{}, Context{}, fmt.Errorf("rebind application: %w; additionally roll back Sway marks: %v", err, errors.Join(rollbackErrors...))
	}
	return Context{}, Context{}, err
}

// ReapproveApplicationContext replaces only the trusted launcher snapshot
// after revalidating the reviewed launcher-and-identity revision under the
// registry lock. Concurrent lifecycle changes remain authoritative.
func ReapproveApplicationContext(ctx context.Context, root string, id ContextID, expectedRevision string, launcher Launcher) (Context, Context, error) {
	var previous Context
	var replacement Context
	_, err := UpdateRegistryContext(ctx, root, func(registry *Registry) error {
		index, err := ResolveContext(*registry, string(id))
		if err != nil {
			return err
		}
		previous = registry.Contexts[index]
		currentRevision, err := ApplicationOperationContextRevision(previous)
		if err != nil || currentRevision != expectedRevision {
			return errors.New("application context changed while launcher reapproval was pending")
		}
		if err := validateUnreferencedDesktopApproval(ctx, root, *registry, launcher); err != nil {
			return err
		}
		replacement = previous
		replacement.Launcher = launcher
		candidate := *registry
		candidate.Contexts = append([]Context(nil), registry.Contexts...)
		candidate.Contexts[index] = replacement
		if err := candidate.Validate(); err != nil {
			return err
		}
		registry.Contexts[index] = replacement
		return nil
	})
	if err == nil {
		return previous, replacement, nil
	}
	compensationCtx, cancelCompensation := mutationCompensationContext(ctx)
	defer cancelCompensation()
	committed, reconciliationErr := registryContainsApplicationMutation(compensationCtx, root, replacement)
	if reconciliationErr != nil {
		return Context{}, Context{}, fmt.Errorf("reapprove application: %w; cannot reconcile registry state: %v", err, reconciliationErr)
	}
	if committed {
		return previous, replacement, nil
	}
	return Context{}, Context{}, err
}

// RepairApplicationMark is the safe no-op/status behavior for an already
// registered focused application whose stable mark was lost.
func RepairApplicationMark(ctx context.Context, root string, client SwayRequestClient, containerID int64, registered Context) error {
	if registered.App == nil {
		return errors.New("registered context is not a desktop application")
	}
	return InspectRegistryLockedContext(ctx, root, func(registry Registry) error {
		index, err := ResolveContext(registry, string(registered.ID))
		if err != nil || !reflect.DeepEqual(registry.Contexts[index], registered) {
			return errors.New("application context changed while mark repair was pending")
		}
		return repairApplicationMark(ctx, client, containerID, registered)
	})
}

func repairApplicationMark(ctx context.Context, client SwayRequestClient, containerID int64, registered Context) error {
	root, err := requestApplicationTree(ctx, client)
	if err != nil {
		return err
	}
	markedContainerID, err := findMarkedContainerInTree(root, registered.ID)
	if err != nil {
		return err
	}
	matches := make([]WindowApplication, 0, 1)
	if err := walkApplicationWindowsIncludingTransient(root, "", func(window WindowApplication, _ bool) {
		if applicationIdentitiesOverlap(registered.App.Identity, window.Identity) {
			matches = append(matches, window)
		}
	}); err != nil {
		return err
	}
	if markedContainerID != 0 {
		for _, match := range matches {
			if match.ContainerID == markedContainerID {
				return nil
			}
		}
		return fmt.Errorf("application anchor mark on container %d does not match the registered application identity", markedContainerID)
	}
	if len(matches) != 1 || matches[0].ContainerID != containerID {
		return fmt.Errorf("application anchor mark is missing and %d matching windows make repair ambiguous", len(matches))
	}
	return SetContextMark(ctx, client, containerID, registered.ID, true)
}

// ForgetApplicationContext removes the live mark before committing removal
// and restores it if the registry update fails.
func ForgetApplicationContext(ctx context.Context, root string, client SwayRequestClient, selector string) (Context, error) {
	var removed Context
	containerID := int64(0)
	unmarked := false
	attemptedUnmark := false
	_, err := UpdateRegistryContext(ctx, root, func(registry *Registry) error {
		index, err := ResolveContext(*registry, selector)
		if err != nil {
			return err
		}
		removed = registry.Contexts[index]
		if removed.App == nil {
			return errors.New("forget is only available for desktop application contexts")
		}
		containerID, err = findMarkedContainer(ctx, client, removed.ID)
		if err != nil {
			return err
		}
		if containerID != 0 {
			attemptedUnmark = true
			if err := SetContextMark(ctx, client, containerID, removed.ID, false); err != nil {
				return err
			}
			unmarked = true
		}
		_, err = RemoveContext(registry, selector)
		return err
	})
	if err == nil {
		return removed, nil
	}
	compensationCtx, cancelCompensation := mutationCompensationContext(ctx)
	defer cancelCompensation()
	stillRegistered, reconciliationErr := registryContainsContextID(compensationCtx, root, removed.ID)
	if reconciliationErr != nil {
		return Context{}, fmt.Errorf("forget application: %w; cannot reconcile registry state: %v; leaving Sway marks unchanged for daemon reconciliation", err, reconciliationErr)
	}
	if !stillRegistered {
		return removed, nil
	}
	if unmarked || attemptedUnmark {
		if rollbackErr := SetContextMark(compensationCtx, client, containerID, removed.ID, true); rollbackErr != nil {
			return Context{}, fmt.Errorf("forget application: %w; additionally restore Sway mark: %v", err, rollbackErr)
		}
	}
	return Context{}, err
}

func mutationCompensationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func SetApplicationRestorePolicy(registry *Registry, selector string, policy ApplicationRestorePolicy) (Context, error) {
	index, err := ResolveContext(*registry, selector)
	if err != nil {
		return Context{}, err
	}
	if registry.Contexts[index].App == nil {
		return Context{}, errors.New("restore policy is only available for desktop application contexts")
	}
	registry.Contexts[index].App.RestorePolicy = policy
	if policy == ApplicationRestorePinned {
		registry.Contexts[index].App.DesiredOpen = true
	}
	if err := registry.Validate(); err != nil {
		return Context{}, err
	}
	return registry.Contexts[index], nil
}

// SetContextMark applies one bounded numeric-selector command and confirms its
// observed outcome. Mutating IPC is never retried blindly.
func SetContextMark(ctx context.Context, client SwayRequestClient, containerID int64, id ContextID, add bool) error {
	if client == nil || containerID <= 0 {
		return errors.New("invalid Sway client or container ID")
	}
	mark, err := id.Mark()
	if err != nil {
		return err
	}
	verb := "unmark"
	if add {
		verb = "mark --add"
	}
	command := fmt.Sprintf("[con_id=%d] %s %s", containerID, verb, mark)
	message, requestErr := client.RequestContext(ctx, swayipc.RunCommand, []byte(command))
	if requestErr == nil {
		requestErr = swayipc.CheckRunCommandResponse(message)
	}
	if requestErr == nil {
		return nil
	}
	hasMark, observeErr := containerHasContextMark(ctx, client, containerID, id)
	if observeErr != nil {
		return fmt.Errorf("apply Sway mark: %w; re-observe outcome: %v", requestErr, observeErr)
	}
	if hasMark == add {
		return nil
	}
	return fmt.Errorf("apply Sway mark: %w", requestErr)
}

func containerHasContextMark(ctx context.Context, client SwayRequestClient, containerID int64, id ContextID) (bool, error) {
	root, err := requestApplicationTree(ctx, client)
	if err != nil {
		return false, err
	}
	mark, _ := id.Mark()
	node, err := findContainer(root, containerID)
	if err != nil {
		return false, err
	}
	return slices.Contains(node.Marks, mark), nil
}

func findMarkedContainer(ctx context.Context, client SwayRequestClient, id ContextID) (int64, error) {
	root, err := requestApplicationTree(ctx, client)
	if err != nil {
		return 0, err
	}
	return findMarkedContainerInTree(root, id)
}

func findMarkedContainerInTree(root *swayipc.TreeNode, id ContextID) (int64, error) {
	mark, _ := id.Mark()
	matches := make([]int64, 0, 1)
	if err := walkTreeNodes(root, func(node *swayipc.TreeNode) {
		if slices.Contains(node.Marks, mark) {
			matches = append(matches, node.ID)
		}
	}); err != nil {
		return 0, err
	}
	if len(matches) > 1 {
		return 0, fmt.Errorf("context %q mark is present on multiple Sway containers", id)
	}
	if len(matches) == 0 {
		return 0, nil
	}
	return matches[0], nil
}

func requestApplicationTree(ctx context.Context, client SwayRequestClient) (*swayipc.TreeNode, error) {
	message, err := client.RequestContext(ctx, swayipc.GetTree, nil)
	if err != nil {
		return nil, err
	}
	if message.Type != swayipc.GetTree {
		return nil, fmt.Errorf("unexpected Sway tree response type %d", message.Type)
	}
	var root swayipc.TreeNode
	if err := json.Unmarshal(message.Payload, &root); err != nil {
		return nil, err
	}
	return &root, nil
}

func findContainer(root *swayipc.TreeNode, id int64) (*swayipc.TreeNode, error) {
	var matches []*swayipc.TreeNode
	if err := walkTreeNodes(root, func(node *swayipc.TreeNode) {
		if node.ID == id {
			matches = append(matches, node)
		}
	}); err != nil {
		return nil, err
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("sway container %d is no longer uniquely present", id)
	}
	return matches[0], nil
}

func walkTreeNodes(node *swayipc.TreeNode, visit func(*swayipc.TreeNode)) error {
	if node == nil {
		return errors.New("sway tree contains a nil node")
	}
	visit(node)
	for _, child := range node.Nodes {
		if err := walkTreeNodes(child, visit); err != nil {
			return err
		}
	}
	for _, child := range node.FloatingNodes {
		if err := walkTreeNodes(child, visit); err != nil {
			return err
		}
	}
	return nil
}

func registryContainsApplicationMutation(ctx context.Context, root string, want Context) (bool, error) {
	return registryContainsApplicationMutations(ctx, root, []Context{want})
}

func registryContainsApplicationMutations(ctx context.Context, root string, want []Context) (bool, error) {
	var registry Registry
	if err := RegistryStoreFor(root).LoadIntoContext(ctx, &registry); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	for _, expected := range want {
		found := false
		for _, current := range registry.Contexts {
			if sameApplicationMutation(current, expected) {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

func sameApplicationMutation(current Context, expected Context) bool {
	if current.App == nil || expected.App == nil {
		return false
	}
	return current.ID == expected.ID &&
		current.Label == expected.Label &&
		current.Provider == expected.Provider &&
		reflect.DeepEqual(current.Launcher, expected.Launcher) &&
		reflect.DeepEqual(current.App.Identity, expected.App.Identity)
}

func registryContainsContextID(ctx context.Context, root string, id ContextID) (bool, error) {
	if id == "" {
		return true, nil
	}
	var registry Registry
	if err := RegistryStoreFor(root).LoadIntoContext(ctx, &registry); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	for _, context := range registry.Contexts {
		if context.ID == id {
			return true, nil
		}
	}
	return false, nil
}
