package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

func (operations commandTerminalManageOperations) LoadEnvironments(ctx context.Context, _ string) (terminalManageEnvironmentSnapshot, error) {
	root, failed := stateRoot(operations.deps)
	if failed != nil {
		return terminalManageEnvironmentSnapshot{}, terminalManageFailure(failed)
	}
	snapshot, err := sessionstate.ReadReachSnapshotContext(ctx, root)
	if err != nil {
		return terminalManageEnvironmentSnapshot{}, err
	}
	items := terminalInventory(snapshot.Inventory.Registry.Contexts, snapshot.Activity)
	latest := latestCLIRestoreOutcomes(snapshot.Inventory.RestoreReport)
	for index := range items {
		if record, found := latest[items[index].ContextID]; found {
			items[index].RestoreOutcome = &record
		}
	}
	return terminalManageEnvironmentSnapshot{environments: snapshot.Environments, contexts: snapshot.Inventory.Registry.Contexts, terminals: items, applications: sessionstate.BuildApplicationInventory(snapshot.Inventory)}, nil
}

// ObserveEnvironments decorates a copied saved projection outside all SQLite
// transactions. One bounded tree serves both terminal and app members; typing
// and definition editing do not wait for Herdr or launcher observations.
func (operations commandTerminalManageOperations) ObserveEnvironments(ctx context.Context, socket string, saved terminalManageEnvironmentSnapshot) (terminalManageEnvironmentSnapshot, error) {
	saved.terminals = slices.Clone(saved.terminals)
	saved.applications = slices.Clone(saved.applications)
	registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: saved.contexts}
	root, failed := stateRoot(operations.deps)
	if failed != nil {
		return saved, terminalManageFailure(failed)
	}
	ctx, cancel := context.WithTimeout(ctx, terminalObservationTimeout)
	defer cancel()
	var presence map[sessionstate.ContextID]sessionstate.ReachPresence
	var groups map[sessionstate.ContextID]sessionstate.ApplicationGroup
	var windowErr error
	var activity map[sessionstate.ContextID]sessionstate.TerminalSessionObservation
	var wait sync.WaitGroup
	wait.Add(3)
	go func() {
		defer wait.Done()
		client, err := operations.environmentClient(socket)
		if err != nil {
			windowErr = err
			return
		}
		defer client.Close()
		tree, err := requestTree(ctx, client)
		if err == nil {
			presence, err = sessionstate.ObserveReachPresence(tree, registry)
		}
		if err == nil {
			groups, err = sessionstate.ObserveRegisteredApplicationGroups(tree, registry)
		}
		windowErr = err
	}()
	go func() { defer wait.Done(); activity = operations.ObserveActivity(ctx, saved.terminals) }()
	go func() {
		defer wait.Done()
		observeApplicationLaunchers(ctx, root, operations.deps, registry, saved.applications)
	}()
	wait.Wait()
	at := time.Now().UTC()
	if operations.deps.now != nil {
		at = operations.deps.now().UTC()
	}
	for index := range saved.terminals {
		item := &saved.terminals[index]
		item.WindowPresence = "unknown"
		item.WindowReason = "window_observation_unavailable"
		item.WindowObservedAt = nil
		if observed := presence[item.ContextID]; windowErr == nil && observed.Known && !observed.Ambiguous {
			item.WindowPresence = "closed"
			if observed.ContainerID > 0 {
				item.WindowPresence = "open"
			}
			item.WindowReason = ""
			item.WindowObservedAt = &at
		}
		observation, found := activity[item.ContextID]
		if !found {
			observation = sessionstate.UnknownTerminalSessionObservation("observation_unavailable")
		}
		item.Activity = observation
		item.PaneDirectory = observation.Directory.Directory
	}
	for index := range saved.applications {
		item := &saved.applications[index]
		if windowErr == nil {
			item.Presence = sessionstate.ApplicationGroupPresence(groups[item.ContextID], at)
		} else {
			item.Presence = sessionstate.ApplicationPresence{State: "unknown", Reason: "window_observation_unavailable"}
		}
	}
	return saved, windowErr
}

// SaveEnvironment accepts one explicit edit at a time. This preserves atomic
// Save/Cancel semantics without replacing an unbounded membership collection in
// one transaction or partially applying an aggregate draft.
func (operations commandTerminalManageOperations) SaveEnvironment(ctx context.Context, desired sessionstate.WorkEnvironment) (sessionstate.WorkEnvironment, error) {
	root, failed := stateRoot(operations.deps)
	if failed != nil {
		return sessionstate.WorkEnvironment{}, terminalManageFailure(failed)
	}
	store := sessionstate.WorkEnvironmentStoreFor(root)
	if desired.ID == "" && desired.Revision == 0 {
		if len(desired.Members) != 0 || desired.Primary != "" {
			return sessionstate.WorkEnvironment{}, errors.New("create an empty environment, then add existing members explicitly")
		}
		return store.CreateContext(ctx, desired.Name)
	}
	if err := desired.Validate(); err != nil {
		return sessionstate.WorkEnvironment{}, err
	}
	current, err := store.LoadContext(ctx, desired.ID)
	if err != nil {
		return current, err
	}
	if current.Revision != desired.Revision {
		return current, sessionstate.ErrWorkEnvironmentConflict
	}
	sameMembers := slices.Equal(current.Members, desired.Members)
	if sameMembers && current.Primary == desired.Primary {
		if current.Name == desired.Name {
			return current, nil
		}
		return store.RenameContext(ctx, current.ID, current.Revision, desired.Name)
	}
	if current.Name != desired.Name {
		return current, errors.New("save the name and membership as separate edits")
	}
	if sameMembers {
		return store.SetPrimaryContext(ctx, current.ID, current.Revision, desired.Primary)
	}
	if current.Primary == desired.Primary && len(desired.Members) == len(current.Members)+1 && slices.Equal(current.Members, desired.Members[:len(current.Members)]) {
		return store.AddMemberContext(ctx, current.ID, current.Revision, desired.Members[len(current.Members)])
	}
	if len(current.Members) == len(desired.Members)+1 {
		index := 0
		for index < len(desired.Members) && current.Members[index] == desired.Members[index] {
			index++
		}
		if slices.Equal(current.Members[index+1:], desired.Members[index:]) {
			removed := current.Members[index]
			primary := current.Primary
			if primary == removed {
				primary = ""
			}
			if desired.Primary == primary {
				return store.RemoveMemberContext(ctx, current.ID, current.Revision, removed)
			}
		}
	}
	return current, errors.New("save one member or primary change at a time; refresh after concurrent edits")
}

func (operations commandTerminalManageOperations) DeleteEnvironment(ctx context.Context, id sessionstate.EnvironmentID, revision int64) error {
	root, failed := stateRoot(operations.deps)
	if failed != nil {
		return terminalManageFailure(failed)
	}
	return sessionstate.WorkEnvironmentStoreFor(root).DeleteContext(ctx, id, revision)
}

func (operations commandTerminalManageOperations) environmentClient(socket string) (swayRequester, error) {
	if socket == "" {
		socket = os.Getenv("SWAYSOCK")
	}
	if socket == "" || !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || operations.deps.newSwayClient == nil {
		return nil, errors.New("sway is unavailable; saved environments remain editable")
	}
	client := operations.deps.newSwayClient(socket)
	if client == nil {
		return nil, errors.New("sway observation is unavailable")
	}
	return client, nil
}

func (operations commandTerminalManageOperations) PreviewEnvironment(ctx context.Context, id sessionstate.EnvironmentID, primary sessionstate.ContextID, socket string) (sessionstate.ReachPreview, error) {
	root, failed := stateRoot(operations.deps)
	if failed != nil {
		return sessionstate.ReachPreview{}, terminalManageFailure(failed)
	}
	snapshot, err := sessionstate.ReadReachEnvironmentSnapshotContext(ctx, root, id)
	if err != nil {
		return sessionstate.ReachPreview{}, err
	}
	presence := map[sessionstate.ContextID]sessionstate.ReachPresence{}
	observationCtx, cancel := context.WithTimeout(ctx, terminalObservationTimeout)
	defer cancel()
	if client, err := operations.environmentClient(socket); err == nil {
		defer client.Close()
		if tree, err := requestTree(observationCtx, client); err == nil {
			if observed, err := sessionstate.ObserveReachPresence(tree, snapshot.Inventory.Registry); err == nil {
				presence = observed
			}
		}
	}
	return sessionstate.PlanWorkEnvironmentReach(ctx, snapshot.Environments[0], snapshot.Inventory.Registry, presence, primary)
}

func (operations commandTerminalManageOperations) ReachEnvironment(ctx context.Context, id sessionstate.EnvironmentID, primary sessionstate.ContextID, socket string) (sessionstate.ReachResult, error) {
	if socket == "" {
		socket = os.Getenv("SWAYSOCK")
	}
	compositor, err := compositorIdentity(socket)
	if err != nil {
		return sessionstate.ReachResult{}, err
	}
	endpoint, err := defaultReachSocketPath()
	if err != nil {
		return sessionstate.ReachResult{}, err
	}
	result, err := sendReachRequest(ctx, endpoint, reachRequest{Version: 1, EnvironmentID: id, Primary: primary, CompositorID: compositor})
	return result, err
}
