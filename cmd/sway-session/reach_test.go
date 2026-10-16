package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

type reachEffectClient struct {
	recordingRequester
	reads      int
	beforeTree func(int)
}

func (client *reachEffectClient) RequestContext(ctx context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if kind == swayipc.GetTree {
		client.reads++
		if client.beforeTree != nil {
			client.beforeTree(client.reads)
		}
	}
	response, err := client.recordingRequester.RequestContext(ctx, kind, payload)
	if kind == swayipc.RunCommand && err == nil {
		var id int64
		if _, err := fmt.Sscanf(string(payload), "[con_id=%d] focus", &id); err == nil {
			var focus func(*Node)
			focus = func(node *Node) {
				node.Focused = node.ID == id
				for _, child := range node.Nodes {
					focus(child)
				}
				for _, child := range node.FloatingNodes {
					focus(child)
				}
			}
			focus(client.tree)
		}
	}
	return response, err
}

func reachExecutionFixture(t *testing.T) (*sessionRuntime, *reachEffectClient, sessionstate.WorkEnvironment, *mutableEventStreamGuard) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	if err := sessionstate.RegistryStoreFor(root).Save(sessionRegistry(testManagedContextID)); err != nil {
		t.Fatal(err)
	}
	store := sessionstate.WorkEnvironmentStoreFor(root)
	environment, err := store.CreateContext(t.Context(), "Development")
	if err != nil {
		t.Fatal(err)
	}
	environment, err = store.AddMemberContext(t.Context(), environment.ID, environment.Revision, testManagedContextID)
	if err != nil {
		t.Fatal(err)
	}
	environment, err = store.SetPrimaryContext(t.Context(), environment.ID, environment.Revision, testManagedContextID)
	if err != nil {
		t.Fatal(err)
	}
	client := &reachEffectClient{recordingRequester: recordingRequester{tree: daemonTree("98", managedDaemonLeaf(t, 41, testManagedContextID), &Node{ID: 42, Type: "con", Focused: true})}}
	runtime, err := newSessionRuntimeWithOptions(client, sessionRuntimeOptions{Root: root, CompositorID: strings.Repeat("a", 64), StartedAt: time.Now(), ApplicationRestore: sessionstate.ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: time.Second, MaxConcurrent: 2}})
	if err != nil {
		t.Fatal(err)
	}
	guard := &mutableEventStreamGuard{epoch: 1, connected: true}
	runtime.eventStreamState = guard
	runtime.eventStreamEpoch = 1
	runtime.eventStreamReady = true
	runtime.reach = newReachDispatcher(strings.Repeat("a", 64))
	return runtime, client, environment, guard
}

func reachExecutionRequest(environment sessionstate.WorkEnvironment) reachRequest {
	return reachRequest{Version: 1, EnvironmentID: environment.ID, CompositorID: strings.Repeat("a", 64)}
}

func TestReachExecuteFocusesOnlyPresentPrimaryAndIsIdempotent(t *testing.T) {
	runtime, client, environment, _ := reachExecutionFixture(t)
	for index := range 2 {
		result, err := runtime.executeReach(t.Context(), reachExecutionRequest(environment), nil)
		if err != nil || len(result.Members) != 1 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		want := sessionstate.ReachFocused
		if index == 1 {
			want = sessionstate.ReachAlreadyPresent
		}
		if result.Members[0].Status != want {
			t.Fatalf("status=%+v want=%s", result, want)
		}
	}
	if len(client.commands) != 1 || client.commands[0] != "[con_id=41] focus" {
		t.Fatalf("unexpected commands: %v", client.commands)
	}
	loaded, err := sessionstate.WorkEnvironmentStoreFor(runtime.root).LoadContext(t.Context(), environment.ID)
	if err != nil || loaded.Revision != environment.Revision {
		t.Fatalf("Reach changed environment: %+v %v", loaded, err)
	}
}

func TestReachExecuteMissingPrimaryDoesNotStartOrRearrange(t *testing.T) {
	runtime, client, environment, _ := reachExecutionFixture(t)
	client.tree.Nodes[0].Nodes[0].Nodes = client.tree.Nodes[0].Nodes[0].Nodes[1:]
	launcher := &recordingApplicationLauncher{}
	runtime.applicationLauncher = launcher
	result, err := runtime.executeReach(t.Context(), reachExecutionRequest(environment), nil)
	if err != nil || result.Members[0].Status != sessionstate.ReachBlocked || len(client.commands) != 0 || launcher.starts != 0 {
		t.Fatalf("missing primary crossed effect boundary: %+v %v commands=%v", result, err, client.commands)
	}
}

func TestReachAlreadyFocusedPrimarySupersedesOlderStartupArrangement(t *testing.T) {
	runtime, client, environment, _ := reachExecutionFixture(t)
	workspace := client.tree.Nodes[0].Nodes[0]
	workspace.Nodes[0].Focused = true
	workspace.Nodes[1].Focused = false
	runtime.startupComplete = false
	runtime.lateRestorePending = true
	result, err := runtime.executeReach(t.Context(), reachExecutionRequest(environment), nil)
	if err != nil || result.Members[0].Status != sessionstate.ReachAlreadyPresent || len(client.commands) != 0 {
		t.Fatalf("already-focused Reach was not idempotent: %+v %v commands=%v", result, err, client.commands)
	}
	if !runtime.restoreCancelled || !runtime.startupComplete || runtime.lateRestorePending {
		t.Fatal("older startup arrangement retained authority after explicit Reach")
	}
}

func TestReachExecuteCancelsChangedDefinitionBeforeFocus(t *testing.T) {
	runtime, client, environment, _ := reachExecutionFixture(t)
	client.beforeTree = func(read int) {
		if read == 1 {
			if _, err := sessionstate.WorkEnvironmentStoreFor(runtime.root).RenameContext(t.Context(), environment.ID, environment.Revision, "Renamed"); err != nil {
				t.Fatal(err)
			}
		}
	}
	result, err := runtime.executeReach(t.Context(), reachExecutionRequest(environment), nil)
	if err != nil || result.Members[0].Status != sessionstate.ReachCanceled || len(client.commands) != 0 {
		t.Fatalf("stale definition focused: %+v %v %v", result, err, client.commands)
	}
}

func TestReachExecuteFocusEventRevokesPendingAuthority(t *testing.T) {
	runtime, client, environment, _ := reachExecutionFixture(t)
	events := make(chan swayipc.Event, 1)
	client.beforeTree = func(read int) {
		if read == 1 {
			events <- swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: 42, Type: "con"}}
		}
	}
	result, err := runtime.executeReach(t.Context(), reachExecutionRequest(environment), events)
	if err != nil || result.Members[0].Status != sessionstate.ReachCanceled || len(client.commands) != 0 {
		t.Fatalf("user focus did not revoke: %+v %v %v", result, err, client.commands)
	}
}

func TestReachExecuteStreamChangedBeforeFinalEffect(t *testing.T) {
	runtime, client, environment, guard := reachExecutionFixture(t)
	client.beforeTree = func(read int) {
		if read == 2 {
			guard.connected = false
			guard.epoch++
		}
	}
	_, _ = runtime.executeReach(t.Context(), reachExecutionRequest(environment), nil)
	if len(client.commands) != 0 {
		t.Fatalf("lost stream still allowed focus: %v", client.commands)
	}
}

func TestReachExecuteRequiresLiveEventAuthority(t *testing.T) {
	for _, mode := range []string{"absent", "not_ready", "different_epoch"} {
		t.Run(mode, func(t *testing.T) {
			runtime, client, environment, guard := reachExecutionFixture(t)
			switch mode {
			case "absent":
				runtime.eventStreamState = nil
			case "not_ready":
				runtime.eventStreamReady = false
			case "different_epoch":
				guard.epoch++
			}
			_, err := runtime.executeReach(t.Context(), reachExecutionRequest(environment), nil)
			if err == nil || len(client.commands) != 0 {
				t.Fatalf("unavailable event authority focused: %v %v", err, client.commands)
			}
		})
	}
}

func TestReachDispatcherLatestRequestCancelsOlderPendingWork(t *testing.T) {
	dispatcher := newReachDispatcher(strings.Repeat("a", 64))
	defer dispatcher.cancel()
	environment := sessionstate.WorkEnvironment{ID: sessionstate.EnvironmentID("00000000-0000-4000-8000-000000000375")}
	firstDone := make(chan error, 1)
	go func() { _, err := dispatcher.call(t.Context(), reachExecutionRequest(environment)); firstDone <- err }()
	first := <-dispatcher.inbox
	secondDone := make(chan error, 1)
	go func() { _, err := dispatcher.call(t.Context(), reachExecutionRequest(environment)); secondDone <- err }()
	second := <-dispatcher.inbox
	if first.ctx.Err() != context.Canceled {
		t.Fatal("old intent retained authority")
	}
	if err := <-firstDone; err != context.Canceled {
		t.Fatalf("old call=%v", err)
	}
	second.reply <- reachReply{result: sessionstate.ReachResult{EnvironmentID: environment.ID, Members: []sessionstate.ReachMemberResult{}}}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}
