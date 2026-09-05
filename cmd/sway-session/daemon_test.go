package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

type daemonLoopRequester struct {
	trees    []*swayipc.TreeNode
	commands []string
	requests int
	failAt   int
	failure  error
}

type blockingDaemonRequester struct {
	entered chan struct{}
}

func TestDaemonRegistryInitializationRetriesBoundedContention(t *testing.T) {
	calls := 0
	err := retryStateContention(t.Context(), func() error {
		calls++
		if calls == 1 {
			return sessionstate.ErrStateDatabaseBusy
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("retry state contention: calls=%d err=%v", calls, err)
	}
}

func TestDaemonRegistryInitializationContentionHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := retryStateContention(ctx, func() error { return sessionstate.ErrStateDatabaseBusy })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled state contention retry = %v", err)
	}
}

func (requester *blockingDaemonRequester) RequestContext(ctx context.Context, _ swayipc.MessageType, _ []byte) (swayipc.Message, error) {
	select {
	case <-requester.entered:
	default:
		close(requester.entered)
	}
	<-ctx.Done()
	return swayipc.Message{}, ctx.Err()
}

func (*blockingDaemonRequester) Close() {}

func (requester *daemonLoopRequester) RequestContext(ctx context.Context, messageType swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if err := ctx.Err(); err != nil {
		return swayipc.Message{}, err
	}
	requester.requests++
	switch messageType {
	case swayipc.GetTree:
		if len(requester.trees) == 0 {
			return swayipc.Message{}, errors.New("no fake Sway tree remains")
		}
		tree := requester.trees[0]
		if len(requester.trees) > 1 {
			requester.trees = requester.trees[1:]
		}
		encoded, err := json.Marshal(tree)
		return swayipc.Message{Type: swayipc.GetTree, Payload: encoded}, err
	case swayipc.RunCommand:
		requester.commands = append(requester.commands, string(payload))
		if requester.failAt > 0 && len(requester.commands) == requester.failAt {
			return swayipc.Message{}, requester.failure
		}
		return swayipc.Message{Type: swayipc.RunCommand, Payload: []byte(`[{"success":true}]`)}, nil
	case swayipc.SendTick:
		return swayipc.Message{Type: swayipc.SendTick, Payload: []byte(`{"success":true}`)}, nil
	default:
		return swayipc.Message{}, errors.New("unexpected fake Sway request")
	}
}

func (*daemonLoopRequester) Close() {}

func TestSessionDaemonLoopWaitsForEventSubscriptionBeforeSwayRequests(t *testing.T) {
	requester := &daemonLoopRequester{}
	runtime := &sessionRuntime{ctx: t.Context()}
	events := make(chan swayipc.Event, 1)
	events <- swayipc.Event{Type: swayipc.EventShutdown, Change: "exit"}

	if err := runSessionDaemonLoop(t.Context(), requester, runtime, events, nil); err != nil {
		t.Fatalf("run session daemon loop: %v", err)
	}
	if requester.requests != 0 {
		t.Fatalf("daemon made %d Sway requests before its event subscription was ready", requester.requests)
	}
}

func TestSessionDaemonLoopCancellationInterruptsBlockedSwayRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	requester := &blockingDaemonRequester{entered: make(chan struct{})}
	runtime := &sessionRuntime{ctx: ctx}
	events := make(chan swayipc.Event, 1)
	events <- swayipc.Event{Type: swayipc.EventStream, Change: "ready"}
	done := make(chan error, 1)
	go func() {
		done <- runSessionDaemonLoop(ctx, requester, runtime, events, nil)
	}()

	select {
	case <-requester.entered:
	case <-time.After(time.Second):
		t.Fatal("daemon did not begin the Sway request")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("canceled daemon returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon did not stop after cancellation interrupted the Sway request")
	}
}

func TestSessionDaemonLoopOwnsPlacementAndCaptureWithoutAnimator(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	setSessionTestStateHome(t, stateHome)
	root := filepath.Join(stateHome, "sway-session")
	registry := sessionRegistry(testManagedContextID)
	if err := sessionstate.RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	desired := placementOnlySnapshot("9: saved", testManagedContextID)
	if err := sessionstate.LayoutStoreFor(root).Save(desired); err != nil {
		t.Fatalf("save desired layout: %v", err)
	}

	appID, _ := testManagedContextID.AppID()
	requester := &daemonLoopRequester{trees: []*swayipc.TreeNode{
		daemonTree("1", &Node{ID: 42, Name: "terminal", Type: "con", AppID: &appID}),
		daemonTree("9: saved", managedDaemonLeaf(t, 42, testManagedContextID)),
	}}
	runtime, err := newSessionRuntime(requester)
	if err != nil {
		t.Fatalf("create session runtime: %v", err)
	}
	events := make(chan swayipc.Event, 2)
	events <- swayipc.Event{Type: swayipc.EventStream, Change: "ready"}
	events <- swayipc.Event{Type: swayipc.EventShutdown, Change: "exit"}
	if err := runSessionDaemonLoop(t.Context(), requester, runtime, events, nil); err != nil {
		t.Fatalf("run session daemon loop: %v", err)
	}
	if len(requester.commands) < 2 ||
		!strings.Contains(requester.commands[0], `move container to workspace "9: saved"`) ||
		!strings.Contains(requester.commands[1], `mark --add "persist:`) {
		t.Fatalf("daemon did not perform placement and marking: %v", requester.commands)
	}
	if got := runtime.desired.Workspaces; len(got) != 1 || got[0].Name != "9: saved" {
		t.Fatalf("daemon did not capture the stable workspace: %+v", got)
	}
}

func TestSessionDaemonLoopPublishesIndicatorsWithoutAnimator(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	registry := sessionstate.Registry{
		Version:     sessionstate.ContextsSchemaVersion,
		Preferences: sessionstate.RegistryPreferences{DesktopIndicators: true},
		Contexts:    []sessionstate.Context{},
	}
	if err := sessionstate.RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	catalog := testDesktopCatalog(t, map[string]string{
		"org.example.App.desktop": "[Desktop Entry]\nType=Application\nName=Example\nExec=/usr/bin/true\n",
	}, false)
	appID := "org.example.App"
	unmarked := daemonTree("98: apps", &Node{ID: 42, Name: "Example", Type: "con", AppID: &appID})
	marked := daemonTree("98: apps", &Node{
		ID: 42, Name: "Example", Type: "con", AppID: &appID,
		Marks: []string{"_sway_session_app_indicator_v1_unregistered_42"},
	})
	requester := &daemonLoopRequester{trees: []*swayipc.TreeNode{unmarked, marked}}
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Root: root,
		IndicatorCatalog: func() (sessionstate.DesktopCatalog, error) {
			return catalog, nil
		},
		IndicatorOperations: func() ([]sessionstate.ApplicationOperation, error) {
			return []sessionstate.ApplicationOperation{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan swayipc.Event, 2)
	events <- swayipc.Event{Type: swayipc.EventStream, Change: "ready"}
	events <- swayipc.Event{Type: swayipc.EventShutdown, Change: "exit"}

	if err := runSessionDaemonLoop(t.Context(), requester, runtime, events, nil); err != nil {
		t.Fatalf("run session daemon loop: %v", err)
	}
	if len(requester.commands) != 1 || !strings.Contains(requester.commands[0], `_sway_session_app_indicator_v1_unregistered`) {
		t.Fatalf("daemon did not publish application indicator: %v", requester.commands)
	}
}

func TestIndicatorCatalogLoaderRefreshesInstalledDesktopEntriesPeriodically(t *testing.T) {
	directory := t.TempDir()
	writeEntry := func(name string) {
		t.Helper()
		data := "[Desktop Entry]\nType=Application\nName=" + name + "\nExec=/usr/bin/true\n"
		if err := os.WriteFile(filepath.Join(directory, name+".desktop"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeEntry("First")
	cache := sessionstate.NewDesktopCatalogCache([]sessionstate.DesktopSearchDirectory{{
		Path: directory, Origin: sessionstate.DesktopEntryUser,
	}})
	now := time.Unix(6000, 0)
	load := newRefreshingDesktopCatalogLoader(cache, func() time.Time { return now }, time.Minute)

	first, err := load()
	if err != nil || len(first.Entries()) != 1 {
		t.Fatalf("initial catalog = %+v err=%v", first.Entries(), err)
	}
	writeEntry("Second")
	cached, err := load()
	if err != nil || len(cached.Entries()) != 1 {
		t.Fatalf("catalog refreshed before interval: %+v err=%v", cached.Entries(), err)
	}
	now = now.Add(time.Minute)
	refreshed, err := load()
	if err != nil || len(refreshed.Entries()) != 2 {
		t.Fatalf("catalog did not refresh after interval: %+v err=%v", refreshed.Entries(), err)
	}
}
