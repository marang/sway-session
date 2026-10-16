package main

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	sessionstate "github.com/marang/sway-session/internal/session"
)

func TestCommandEnvironmentEditsAreNonowningAndRejectAggregateOrStaleSaves(t *testing.T) {
	deps, contexts := terminalManageObservationFixture(t)
	operations := commandTerminalManageOperations{deps: deps}
	before := loadTestRegistry(t, deps)
	environment, err := operations.SaveEnvironment(t.Context(), sessionstate.WorkEnvironment{Name: "Release work"})
	if err != nil {
		t.Fatal(err)
	}
	save := func(edit func(*sessionstate.WorkEnvironment)) {
		t.Helper()
		draft := environment
		draft.Members = slices.Clone(environment.Members)
		edit(&draft)
		environment, err = operations.SaveEnvironment(t.Context(), draft)
		if err != nil {
			t.Fatal(err)
		}
	}
	save(func(draft *sessionstate.WorkEnvironment) { draft.Members = append(draft.Members, contexts[0].ID) })
	// Membership can include archived contexts without activating them.
	save(func(draft *sessionstate.WorkEnvironment) { draft.Members = append(draft.Members, contexts[1].ID) })
	save(func(draft *sessionstate.WorkEnvironment) { draft.Primary = contexts[0].ID })
	stale := environment
	save(func(draft *sessionstate.WorkEnvironment) { draft.Name = "Renamed work" })
	if _, err := operations.SaveEnvironment(t.Context(), stale); !errors.Is(err, sessionstate.ErrWorkEnvironmentConflict) {
		t.Fatalf("stale save error = %v", err)
	}
	aggregate := environment
	aggregate.Name = "Must not be saved"
	aggregate.Primary = contexts[1].ID
	if _, err := operations.SaveEnvironment(t.Context(), aggregate); err == nil {
		t.Fatal("accepted a multi-intent save")
	}
	root, _ := deps.stateRoot()
	loaded, err := sessionstate.WorkEnvironmentStoreFor(root).LoadContext(t.Context(), environment.ID)
	if err != nil || !reflect.DeepEqual(loaded, environment) {
		t.Fatalf("rejected saves changed definition: %+v %v", loaded, err)
	}
	save(func(draft *sessionstate.WorkEnvironment) {
		draft.Members = draft.Members[1:]
		draft.Primary = ""
	})
	if err := operations.DeleteEnvironment(t.Context(), environment.ID, environment.Revision); err != nil {
		t.Fatal(err)
	}
	if after := loadTestRegistry(t, deps); !reflect.DeepEqual(before, after) {
		t.Fatalf("definition editing changed contexts: before=%+v after=%+v", before, after)
	}
	if _, err := sessionstate.WorkEnvironmentStoreFor(root).LoadContext(t.Context(), environment.ID); !errors.Is(err, sessionstate.ErrWorkEnvironmentNotFound) {
		t.Fatalf("deleted definition still available: %v", err)
	}
}

func TestCommandEnvironmentLoadAndPreviewKeepSavedDataWhenOffline(t *testing.T) {
	deps, contexts := terminalManageObservationFixture(t)
	// Saved editing/loading must not depend on Sway or Herdr being reachable.
	deps.newSwayClient = nil
	operations := commandTerminalManageOperations{deps: deps}
	environment, err := operations.SaveEnvironment(t.Context(), sessionstate.WorkEnvironment{Name: "Offline work"})
	if err != nil {
		t.Fatal(err)
	}
	environment.Members = []sessionstate.ContextID{contexts[0].ID}
	environment, err = operations.SaveEnvironment(t.Context(), environment)
	if err != nil {
		t.Fatal(err)
	}
	environment.Primary = contexts[0].ID
	environment, err = operations.SaveEnvironment(t.Context(), environment)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := operations.LoadEnvironments(t.Context(), "")
	if err != nil || len(snapshot.environments) != 1 || len(snapshot.contexts) != len(contexts) {
		t.Fatalf("saved inventory unavailable offline: %+v %v", snapshot, err)
	}
	preview, err := operations.PreviewEnvironment(t.Context(), environment.ID, "", "")
	if err != nil || preview.Ready || len(preview.Members) != 1 || preview.Members[0].Status != sessionstate.ReachUncertain {
		t.Fatalf("offline evidence became effect permission: %+v %v", preview, err)
	}
	if _, err := operations.PreviewEnvironment(t.Context(), environment.ID, contexts[2].ID, ""); err == nil {
		t.Fatal("accepted a primary override outside the environment")
	}
	root, _ := deps.stateRoot()
	after, err := sessionstate.WorkEnvironmentStoreFor(root).LoadContext(t.Context(), environment.ID)
	if err != nil || !reflect.DeepEqual(after, environment) {
		t.Fatalf("preview changed the definition: %+v %v", after, err)
	}
}

func TestCommandEnvironmentRemovesPrimaryAtEachPositionAndRejectsReordering(t *testing.T) {
	for _, index := range []int{0, 1, 2} {
		t.Run(string(rune('0'+index)), func(t *testing.T) {
			deps, contexts := terminalManageObservationFixture(t)
			operations := commandTerminalManageOperations{deps: deps}
			environment, err := operations.SaveEnvironment(t.Context(), sessionstate.WorkEnvironment{Name: "Ordered"})
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range contexts {
				environment.Members = append(environment.Members, item.ID)
				environment, err = operations.SaveEnvironment(t.Context(), environment)
				if err != nil {
					t.Fatal(err)
				}
			}
			environment.Primary = contexts[index].ID
			environment, err = operations.SaveEnvironment(t.Context(), environment)
			if err != nil {
				t.Fatal(err)
			}
			draft := environment
			draft.Members = slices.Delete(slices.Clone(environment.Members), index, index+1)
			draft.Primary = ""
			reordered := draft
			reordered.Members = slices.Clone(draft.Members)
			slices.Reverse(reordered.Members)
			if _, err := operations.SaveEnvironment(t.Context(), reordered); err == nil {
				t.Fatal("removal also reordered remaining members")
			}
			result, err := operations.SaveEnvironment(t.Context(), draft)
			if err != nil || result.Primary != "" || !slices.Equal(result.Members, draft.Members) {
				t.Fatalf("primary removal failed: %+v %v", result, err)
			}
		})
	}
}

func TestCommandEnvironmentPreviewUsesCanonicalPresenceWithoutCommands(t *testing.T) {
	deps, contexts := terminalManageObservationFixture(t)
	client := &terminalManageObservationRequester{tree: treeWithContexts(contexts[0].ID)}
	deps.newSwayClient = func(string) swayRequester { return client }
	operations := commandTerminalManageOperations{deps: deps}
	environment, err := operations.SaveEnvironment(t.Context(), sessionstate.WorkEnvironment{Name: "Present work"})
	if err != nil {
		t.Fatal(err)
	}
	environment.Members = []sessionstate.ContextID{contexts[0].ID}
	environment, err = operations.SaveEnvironment(t.Context(), environment)
	if err != nil {
		t.Fatal(err)
	}
	environment.Primary = contexts[0].ID
	environment, err = operations.SaveEnvironment(t.Context(), environment)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := operations.PreviewEnvironment(t.Context(), environment.ID, "", "/run/private-test/sway.sock")
	if err != nil || !preview.Ready || preview.Members[0].Status != sessionstate.ReachWouldFocus {
		t.Fatalf("canonical present primary not reachable: %+v %v", preview, err)
	}
	terminalManageRequireObservationOnly(t, client)
}
