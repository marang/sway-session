package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

type testTerminalCloseGuard struct {
	generation uint64
	safe       bool
}

func (guard testTerminalCloseGuard) Snapshot() (uint64, bool) {
	return guard.generation, guard.safe
}

func TestObservedTerminalCloseArchivesAfterGraceAndFreshAbsence(t *testing.T) {
	guard := &testTerminalCloseGuard{generation: 7, safe: true}
	runtime, requester, root, now, leaf := armedTerminalClose(t, guard)
	defer runtime.Shutdown()
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: leaf}, now)
	if err := runtime.Flush(now.Add(terminalCloseGrace - time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	if requester.requests != 0 {
		t.Fatalf("close checked Sway before grace: %d requests", requester.requests)
	}
	if err := runtime.Flush(now.Add(time.Second + terminalCloseGrace)); err != nil {
		t.Fatal(err)
	}

	var registry sessionstate.Registry
	if err := sessionstate.RegistryStoreFor(root).LoadInto(&registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Contexts) != 1 || registry.Contexts[0].State != sessionstate.ContextArchived {
		t.Fatalf("closed terminal remains enabled for automatic restore: %+v", registry.Contexts)
	}
}

func TestObservedTerminalCloseFailsSafeWithoutGuard(t *testing.T) {
	runtime, requester, root, now, leaf := armedTerminalClose(t, nil)
	defer runtime.Shutdown()
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: leaf}, now)
	if err := runtime.Flush(now.Add(terminalCloseGrace)); err != nil {
		t.Fatal(err)
	}
	if requester.requests != 0 {
		t.Fatalf("nil guard contacted Sway: %d requests", requester.requests)
	}
	assertTerminalCloseState(t, root, sessionstate.ContextActive)
}

func TestObservedTerminalCloseDropsChangedGuardGeneration(t *testing.T) {
	guard := &testTerminalCloseGuard{generation: 7, safe: true}
	runtime, requester, root, now, leaf := armedTerminalClose(t, guard)
	defer runtime.Shutdown()
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: leaf}, now)
	guard.generation++
	if err := runtime.Flush(now.Add(terminalCloseGrace)); err != nil {
		t.Fatal(err)
	}
	if requester.requests != 1 {
		t.Fatalf("guard transition did not use one fresh absence check: %d requests", requester.requests)
	}
	assertTerminalCloseState(t, root, sessionstate.ContextActive)
}

func TestObservedTerminalCloseDropsReopenedTerminal(t *testing.T) {
	guard := &testTerminalCloseGuard{generation: 7, safe: true}
	runtime, requester, root, now, leaf := armedTerminalClose(t, guard)
	defer runtime.Shutdown()
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: leaf}, now)
	requester.trees = []*swayipc.TreeNode{daemonTree("98", managedDaemonLeaf(t, 43, testManagedContextID))}
	if err := runtime.Flush(now.Add(terminalCloseGrace)); err != nil {
		t.Fatal(err)
	}
	assertTerminalCloseState(t, root, sessionstate.ContextActive)
}

func TestObservedTerminalCloseDropsAmbiguousReopenedIdentity(t *testing.T) {
	guard := &testTerminalCloseGuard{generation: 7, safe: true}
	runtime, requester, root, now, leaf := armedTerminalClose(t, guard)
	defer runtime.Shutdown()
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: leaf}, now)
	requester.trees = []*swayipc.TreeNode{daemonTree(
		"98",
		managedDaemonLeaf(t, 43, testManagedContextID),
		managedDaemonLeaf(t, 44, testManagedContextID),
	)}
	if err := runtime.Flush(now.Add(terminalCloseGrace)); err != nil {
		t.Fatal(err)
	}
	assertTerminalCloseState(t, root, sessionstate.ContextActive)
}

func TestObservedTerminalCloseDropsOnStreamDisconnectAndShutdown(t *testing.T) {
	for _, event := range []swayipc.Event{
		{Type: swayipc.EventStream, Change: "disconnected", StreamEpoch: 2},
		{Type: swayipc.EventShutdown, Change: "exit"},
	} {
		t.Run(string(event.Type)+"-"+event.Change, func(t *testing.T) {
			guard := &testTerminalCloseGuard{generation: 7, safe: true}
			runtime, requester, root, now, leaf := armedTerminalClose(t, guard)
			defer runtime.Shutdown()
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: leaf}, now)
			runtime.HandleEvent(event, now.Add(time.Millisecond))
			if err := runtime.Flush(now.Add(terminalCloseGrace)); err != nil {
				t.Fatal(err)
			}
			if requester.requests != 0 {
				t.Fatalf("cancelled candidate contacted Sway: %d requests", requester.requests)
			}
			assertTerminalCloseState(t, root, sessionstate.ContextActive)
		})
	}
}

func TestObservedTerminalCloseRetriesAFailedFreshAbsenceCheck(t *testing.T) {
	guard := &testTerminalCloseGuard{generation: 7, safe: true}
	runtime, _, _, now, leaf := armedTerminalClose(t, guard)
	defer runtime.Shutdown()
	runtime.client = &daemonLoopRequester{}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: leaf}, now)
	flushAt := now.Add(terminalCloseGrace)
	if err := runtime.Flush(flushAt); err == nil {
		t.Fatal("failed fresh tree observation did not report an error")
	}
	if got, want := runtime.terminalCloseDeadline, flushAt.Add(terminalFocusBatchDelay); !got.Equal(want) {
		t.Fatalf("close confirmation retry = %v, want %v", got, want)
	}
}

func TestObservedTerminalCloseClearsRetryAfterReopenedCandidate(t *testing.T) {
	guard := &testTerminalCloseGuard{generation: 7, safe: true}
	runtime, _, _, now, leaf := armedTerminalClose(t, guard)
	defer runtime.Shutdown()
	runtime.client = &daemonLoopRequester{}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: leaf}, now)
	failedAt := now.Add(terminalCloseGrace)
	if err := runtime.Flush(failedAt); err == nil {
		t.Fatal("failed fresh tree observation did not report an error")
	}
	if _, err := runtime.Reconcile(daemonTree("98", leaf), failedAt.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if len(runtime.pendingTerminalClose) != 0 {
		t.Fatalf("reopened terminal retained close candidates: %+v", runtime.pendingTerminalClose)
	}
	newCloseAt := failedAt.Add(2 * terminalFocusBatchDelay)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: leaf}, newCloseAt)
	if got, want := runtime.terminalCloseDeadline, newCloseAt.Add(terminalCloseGrace); !got.Equal(want) {
		t.Fatalf("new close deadline = %v, want its grace deadline %v", got, want)
	}
}

func TestObservedTerminalCloseRejectsMalformedFreshTree(t *testing.T) {
	guard := &testTerminalCloseGuard{generation: 7, safe: true}
	runtime, requester, root, now, leaf := armedTerminalClose(t, guard)
	defer runtime.Shutdown()
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: leaf}, now)
	requester.trees = []*swayipc.TreeNode{{}}
	if err := runtime.Flush(now.Add(terminalCloseGrace)); err == nil {
		t.Fatal("malformed fresh tree did not fail close confirmation")
	}
	assertTerminalCloseState(t, root, sessionstate.ContextActive)
	if runtime.terminalCloseRetryDeadline.IsZero() {
		t.Fatal("malformed fresh tree did not schedule a retry")
	}
}

func TestObservedTerminalCloseArchivesEveryCandidateBeyondOneBatch(t *testing.T) {
	const count = 257
	root := filepath.Join(t.TempDir(), "state")
	registry, ids := terminalCloseManyRegistry(count)
	if err := sessionstate.RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	requester := &daemonLoopRequester{trees: []*swayipc.TreeNode{daemonTree("98")}}
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Root:               root,
		TerminalCloseGuard: &testTerminalCloseGuard{generation: 7, safe: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Shutdown()
	now := time.Unix(100, 0)
	leaves := make([]*Node, 0, count)
	for index, id := range ids {
		leaves = append(leaves, managedDaemonLeaf(t, int64(index+42), id))
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 1}, now)
	if _, err := runtime.Reconcile(daemonTree("98", leaves...), now); err != nil {
		t.Fatal(err)
	}
	for _, leaf := range leaves {
		runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: leaf}, now)
	}
	flushAt := now.Add(terminalCloseGrace)
	for pass := 0; pass < 16 && len(runtime.pendingTerminalClose) != 0; pass++ {
		if err := runtime.Flush(flushAt); err != nil {
			t.Fatal(err)
		}
		flushAt = runtime.terminalCloseDeadline
		if flushAt.IsZero() && len(runtime.pendingTerminalClose) != 0 {
			t.Fatal("remaining close candidates have no continuation deadline")
		}
	}
	if len(runtime.pendingTerminalClose) != 0 {
		t.Fatalf("bounded close passes left %d candidates pending", len(runtime.pendingTerminalClose))
	}
	var saved sessionstate.Registry
	if err := sessionstate.RegistryStoreFor(root).LoadInto(&saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Contexts) != count {
		t.Fatalf("saved contexts = %d, want %d", len(saved.Contexts), count)
	}
	for _, contextValue := range saved.Contexts {
		if contextValue.State != sessionstate.ContextArchived {
			t.Fatalf("candidate %s was silently dropped: state=%s", contextValue.ID, contextValue.State)
		}
	}
}

func terminalCloseManyRegistry(count int) (sessionstate.Registry, []sessionstate.ContextID) {
	contexts := make([]sessionstate.Context, 0, count)
	ids := make([]sessionstate.ContextID, 0, count)
	for index := range count {
		id := sessionstate.ContextID(fmt.Sprintf("%08x-e89b-42d3-a456-%012x", index+1, index+1))
		contextValue := sessionRegistry(id).Contexts[0]
		contextValue.Launcher.Session = fmt.Sprintf("terminal-%d", index)
		contextValue.Launcher.Terminal.Identity = &sessionstate.TerminalIdentity{
			Kind: sessionstate.TerminalIdentityProject, Project: fmt.Sprintf("project-%d", index),
		}
		contexts = append(contexts, contextValue)
		ids = append(ids, id)
	}
	return sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: contexts}, ids
}

func TestObservedTerminalCloseRetainsConcurrentUnrelatedRegistryEdit(t *testing.T) {
	guard := &testTerminalCloseGuard{generation: 7, safe: true}
	runtime, _, root, now, leaf := armedTerminalClose(t, guard)
	defer runtime.Shutdown()
	secondID := sessionstate.ContextID("223e4567-e89b-12d3-a456-426614174000")
	if _, err := sessionstate.UpdateRegistryContext(t.Context(), root, func(registry *sessionstate.Registry) error {
		registry.Contexts = append(registry.Contexts, sessionstate.Context{
			ID:    secondID,
			Label: "before",
			State: sessionstate.ContextActive,
			Launcher: sessionstate.Launcher{
				Kind:    sessionstate.LauncherHerdr,
				Session: "unrelated-session",
				Cwd:     "/work",
				Terminal: &sessionstate.TerminalLauncher{
					Adapter:  sessionstate.TerminalAdapterAlacritty,
					Identity: &sessionstate.TerminalIdentity{Kind: sessionstate.TerminalIdentityProject, Project: "unrelated"},
				},
			},
		})
		return registry.Validate()
	}); err != nil {
		t.Fatal(err)
	}
	requester := &terminalCloseBarrierRequester{
		tree:    daemonTree("98"),
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	runtime.client = requester
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: leaf}, now)
	flushed := make(chan error, 1)
	go func() { flushed <- runtime.Flush(now.Add(terminalCloseGrace)) }()
	select {
	case <-requester.entered:
	case <-time.After(time.Second):
		t.Fatal("flush did not begin its locked fresh-tree confirmation")
	}
	if _, err := sessionstate.UpdateRegistryContext(t.Context(), root, func(registry *sessionstate.Registry) error {
		index, resolveErr := sessionstate.ResolveContext(*registry, string(secondID))
		if resolveErr != nil {
			return resolveErr
		}
		registry.Contexts[index].Label = "retained"
		return registry.Validate()
	}); err != nil {
		t.Fatal(err)
	}
	close(requester.release)
	select {
	case err := <-flushed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("flush remained blocked after fresh tree confirmation")
	}
	var registry sessionstate.Registry
	if err := sessionstate.RegistryStoreFor(root).LoadInto(&registry); err != nil {
		t.Fatal(err)
	}
	if registry.Contexts[0].State != sessionstate.ContextArchived || registry.Contexts[1].Label != "retained" {
		t.Fatalf("close archive overwrote concurrent registry edit: %+v", registry.Contexts)
	}
}

func TestObservedTerminalCloseRetainsPriorProofAcrossCloseBurst(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	secondID := sessionstate.ContextID("323e4567-e89b-12d3-a456-426614174000")
	registry := sessionRegistry(testManagedContextID)
	second := sessionRegistry(secondID).Contexts[0]
	second.Launcher.Session = "second-terminal"
	second.Launcher.Terminal.Identity = &sessionstate.TerminalIdentity{Kind: sessionstate.TerminalIdentityProject, Project: "second"}
	registry.Contexts = append(registry.Contexts, second)
	if err := sessionstate.RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	requester := &daemonLoopRequester{trees: []*swayipc.TreeNode{daemonTree("98")}}
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Root:               root,
		TerminalCloseGuard: &testTerminalCloseGuard{generation: 7, safe: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Shutdown()
	now := time.Unix(100, 0)
	first := managedDaemonLeaf(t, 42, testManagedContextID)
	secondLeaf := managedDaemonLeaf(t, 43, secondID)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 1}, now)
	if _, err := runtime.Reconcile(daemonTree("98", first, secondLeaf), now); err != nil {
		t.Fatal(err)
	}
	// The first close wakes a reconciliation that has already observed both
	// leaves absent. The second queued close still needs its prior proof.
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: first}, now.Add(time.Millisecond))
	if _, err := runtime.Reconcile(daemonTree("98"), now.Add(2*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: secondLeaf}, now.Add(3*time.Millisecond))
	if err := runtime.Flush(now.Add(terminalCloseGrace + 3*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var saved sessionstate.Registry
	if err := sessionstate.RegistryStoreFor(root).LoadInto(&saved); err != nil {
		t.Fatal(err)
	}
	for _, contextValue := range saved.Contexts {
		if contextValue.State != sessionstate.ContextArchived {
			t.Fatalf("close burst left context active: %+v", saved.Contexts)
		}
	}
}

func TestObservedTerminalCloseRetainsProofWhenFocusPrecedesClose(t *testing.T) {
	guard := &testTerminalCloseGuard{generation: 7, safe: true}
	runtime, requester, root, now, leaf := armedTerminalClose(t, guard)
	defer runtime.Shutdown()
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: leaf}, now.Add(time.Millisecond))
	if _, err := runtime.Reconcile(daemonTree("98"), now.Add(2*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "close", Container: leaf}, now.Add(3*time.Millisecond))
	if err := runtime.Flush(now.Add(terminalCloseGrace + 3*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if requester.requests != 1 {
		t.Fatalf("fresh terminal close tree requests = %d, want 1", requester.requests)
	}
	assertTerminalCloseState(t, root, sessionstate.ContextArchived)
}

func armedTerminalClose(t *testing.T, guard TerminalCloseGuard) (*sessionRuntime, *daemonLoopRequester, string, time.Time, *Node) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	if err := sessionstate.RegistryStoreFor(root).Save(sessionRegistry(testManagedContextID)); err != nil {
		t.Fatal(err)
	}
	requester := &daemonLoopRequester{trees: []*swayipc.TreeNode{daemonTree("98")}}
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{Root: root, TerminalCloseGuard: guard})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	leaf := managedDaemonLeaf(t, 42, testManagedContextID)
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 1}, now)
	if _, err := runtime.Reconcile(daemonTree("98", leaf), now); err != nil {
		runtime.Shutdown()
		t.Fatal(err)
	}
	return runtime, requester, root, now, leaf
}

func assertTerminalCloseState(t *testing.T, root string, want sessionstate.ContextState) {
	t.Helper()
	var registry sessionstate.Registry
	if err := sessionstate.RegistryStoreFor(root).LoadInto(&registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Contexts) != 1 || registry.Contexts[0].State != want {
		t.Fatalf("terminal lifecycle state = %+v, want %s", registry.Contexts, want)
	}
}

type terminalCloseBarrierRequester struct {
	tree    *swayipc.TreeNode
	entered chan struct{}
	release chan struct{}
}

func (requester *terminalCloseBarrierRequester) RequestContext(ctx context.Context, messageType swayipc.MessageType, _ []byte) (swayipc.Message, error) {
	if messageType != swayipc.GetTree {
		return swayipc.Message{}, nil
	}
	select {
	case requester.entered <- struct{}{}:
	default:
	}
	select {
	case <-requester.release:
	case <-ctx.Done():
		return swayipc.Message{}, ctx.Err()
	}
	payload, err := json.Marshal(requester.tree)
	return swayipc.Message{Type: swayipc.GetTree, Payload: payload}, err
}

func (*terminalCloseBarrierRequester) Close() {}
