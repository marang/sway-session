package sessionrequest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
	"github.com/marang/sway-title-animator/internal/swayipc"
	"golang.org/x/sys/unix"
)

type fakeRestoreRunner struct {
	calls        []sessionstate.ContextID
	err          error
	mapped       *sessionstate.ContextID
	afterRestore func() error
}

type fakeSessionInitializer struct {
	calls []sessionstate.Context
	err   error
}

func (initializer *fakeSessionInitializer) Initialize(_ context.Context, contextValue sessionstate.Context) error {
	initializer.calls = append(initializer.calls, contextValue)
	err := initializer.err
	initializer.err = nil
	return err
}

func (runner *fakeRestoreRunner) Restore(_ context.Context, id sessionstate.ContextID) error {
	runner.calls = append(runner.calls, id)
	if runner.err == nil && runner.mapped != nil {
		*runner.mapped = id
	}
	if runner.err == nil && runner.afterRestore != nil {
		return runner.afterRestore()
	}
	return runner.err
}

type fakeSwayRequester struct {
	workspace        int
	mapped           sessionstate.ContextID
	occupied         bool
	floatingOccupied bool
	commands         []string
	treeRequests     int
	occupyOnTree     int
	occupyWorkspace  int
	onTree           func(int) error
}

func (client *fakeSwayRequester) RequestContext(_ context.Context, messageType swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	switch messageType {
	case swayipc.RunCommand:
		command := string(payload)
		client.commands = append(client.commands, command)
		if _, err := fmt.Sscanf(command, "workspace number %d", &client.workspace); err != nil {
			return swayipc.Message{}, err
		}
		return swayipc.Message{Type: swayipc.RunCommand, Payload: []byte(`[{"success":true}]`)}, nil
	case swayipc.GetTree:
		client.treeRequests++
		if client.onTree != nil {
			if err := client.onTree(client.treeRequests); err != nil {
				return swayipc.Message{}, err
			}
		}
		if client.treeRequests == client.occupyOnTree {
			client.workspace = client.occupyWorkspace
			client.occupied = true
		}
		encoded, err := json.Marshal(serviceTree(client.workspace, client.mapped, client.occupied, client.floatingOccupied))
		return swayipc.Message{Type: swayipc.GetTree, Payload: encoded}, err
	default:
		return swayipc.Message{}, errors.New("unexpected Sway request")
	}
}

func (client *fakeSwayRequester) Close() {}

type blockingSwayRequester struct {
	started chan struct{}
}

func (client *blockingSwayRequester) RequestContext(ctx context.Context, _ swayipc.MessageType, _ []byte) (swayipc.Message, error) {
	close(client.started)
	<-ctx.Done()
	return swayipc.Message{}, ctx.Err()
}

func (client *blockingSwayRequester) Close() {}

func serviceTree(workspace int, mapped sessionstate.ContextID, occupied bool, floatingOccupied bool) *swayipc.TreeNode {
	workspaces := []*swayipc.TreeNode{{ID: 3, Type: "workspace", Name: "1", Nodes: []*swayipc.TreeNode{}, FloatingNodes: []*swayipc.TreeNode{}}}
	if workspace != 0 {
		target := &swayipc.TreeNode{ID: 4, Type: "workspace", Name: fmt.Sprintf("%d", workspace), Nodes: []*swayipc.TreeNode{}, FloatingNodes: []*swayipc.TreeNode{}}
		if occupied {
			target.Nodes = append(target.Nodes, &swayipc.TreeNode{ID: 5, Type: "con", Nodes: []*swayipc.TreeNode{}, FloatingNodes: []*swayipc.TreeNode{}})
		}
		if floatingOccupied {
			target.FloatingNodes = append(target.FloatingNodes, &swayipc.TreeNode{ID: 7, Type: "floating_con", Nodes: []*swayipc.TreeNode{}, FloatingNodes: []*swayipc.TreeNode{}})
		}
		if mapped != "" {
			appID, _ := mapped.AppID()
			target.Nodes = append(target.Nodes, &swayipc.TreeNode{ID: 6, Type: "con", AppID: &appID, Nodes: []*swayipc.TreeNode{}, FloatingNodes: []*swayipc.TreeNode{}})
		}
		workspaces = append(workspaces, target)
	}
	return &swayipc.TreeNode{ID: 1, Type: "root", Nodes: []*swayipc.TreeNode{{ID: 2, Type: "output", Nodes: workspaces, FloatingNodes: []*swayipc.TreeNode{}}}, FloatingNodes: []*swayipc.TreeNode{}}
}

func testService(t *testing.T) (*Service, Request, *fakeSwayRequester, *fakeRestoreRunner) {
	t.Helper()
	request := Request{Version: ProtocolVersion, Session: "reboot-e2e", Cwd: t.TempDir(), Label: "REBOOT-E2E", Provider: "local", Workspace: 7}
	client := &fakeSwayRequester{}
	runner := &fakeRestoreRunner{mapped: &client.mapped}
	service := &Service{
		StateRoot:    filepath.Join(t.TempDir(), "state"),
		NewContextID: func() (sessionstate.ContextID, error) { return testContextID, nil },
		NewSway:      func() SwayRequester { return client },
		Restore:      runner,
		Now:          func() time.Time { return time.Date(2026, 9, 3, 19, 0, 0, 0, time.UTC) },
	}
	return service, request, client, runner
}

func TestServiceCreatesFocusesAndRestoresOneContext(t *testing.T) {
	service, request, client, runner := testService(t)
	response, err := service.Handle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !response.Created || response.Context == nil || response.Context.ID != testContextID || response.Workspace != 7 {
		t.Fatalf("unexpected response: %+v", response)
	}
	if !reflect.DeepEqual(client.commands, []string{"workspace number 7"}) || !reflect.DeepEqual(runner.calls, []sessionstate.ContextID{testContextID}) {
		t.Fatalf("unexpected effects: commands=%v restores=%v", client.commands, runner.calls)
	}
	activity, err := sessionstate.ReadTerminalActivitySnapshot(service.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	created, exists := sessionstate.FindTerminalActivity(activity, testContextID)
	if !exists || created.CreatedAt == nil || !created.CreatedAt.Equal(service.Now()) {
		t.Fatalf("broker creation activity=%+v exists=%t", created, exists)
	}
}

func TestServicePropagatesCancellationToSwayRequest(t *testing.T) {
	request := Request{Version: ProtocolVersion, Session: "reboot-e2e", Cwd: t.TempDir(), Label: "REBOOT-E2E", Provider: "local", Workspace: 7}
	client := &blockingSwayRequester{started: make(chan struct{})}
	service := &Service{
		StateRoot: filepath.Join(t.TempDir(), "state"),
		NewContextID: func() (sessionstate.ContextID, error) {
			return testContextID, nil
		},
		NewSway: func() SwayRequester { return client },
		Restore: &fakeRestoreRunner{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := service.Handle(ctx, request)
		result <- err
	}()
	<-client.started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Sway request returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("service did not stop after request cancellation")
	}
}

func TestServiceStopsWaitingForRegistryLockAfterCancellation(t *testing.T) {
	service, request, client, _ := testService(t)
	if err := sessionstate.RegistryStoreFor(service.StateRoot).Save(sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{},
	}); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(service.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := unix.Flock(int(directory.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(directory.Fd()), unix.LOCK_UN)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = service.Handle(ctx, request)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("locked registry returned %v, want context deadline", err)
	}
	if client.treeRequests != 0 {
		t.Fatalf("service contacted Sway while registry remained locked: %d requests", client.treeRequests)
	}
}

func TestServiceWaitsForTerminalLifecycleLockBeforeRegistration(t *testing.T) {
	service, request, client, runner := testService(t)
	request.Workspace = 98
	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- sessionstate.WithTerminalLifecycleLockContext(context.Background(), service.StateRoot, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	defer func() {
		close(release)
		if lockErr := <-lockDone; lockErr != nil {
			t.Errorf("release terminal lifecycle lock: %v", lockErr)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := service.Handle(ctx, request)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("locked session request returned %v, want context deadline", err)
	}
	if client.treeRequests != 0 || len(runner.calls) != 0 {
		t.Fatalf("session request crossed lifecycle lock: trees=%d restores=%v", client.treeRequests, runner.calls)
	}
	var registry sessionstate.Registry
	if loadErr := sessionstate.RegistryStoreFor(service.StateRoot).LoadInto(&registry); !errors.Is(loadErr, os.ErrNotExist) {
		t.Fatalf("locked session request created registry=%+v err=%v", registry, loadErr)
	}
}

func TestServiceRepeatedRequestReusesMappedContext(t *testing.T) {
	service, request, client, runner := testService(t)
	first, err := service.Handle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Handle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || second.Created || first.Context == nil || second.Context == nil || first.Context.ID != second.Context.ID {
		t.Fatalf("request was not idempotent: first=%+v second=%+v", first, second)
	}
	if !reflect.DeepEqual(runner.calls, []sessionstate.ContextID{testContextID}) {
		t.Fatalf("mapped context was restored again: %v", runner.calls)
	}
	if !reflect.DeepEqual(client.commands, []string{"workspace number 7", "workspace number 7"}) {
		t.Fatalf("unexpected focus commands: %v", client.commands)
	}
}

func TestServiceInitializationFailureKeepsExactContextAndRetryConverges(t *testing.T) {
	service, request, _, runner := testService(t)
	request.Workspace = 98
	initializer := &fakeSessionInitializer{err: errors.New("agent trust required")}
	service.Initializer = initializer

	partial, err := service.Handle(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "agent trust required") || partial.Context == nil || partial.Context.ID != testContextID {
		t.Fatalf("partial initialization response=%+v err=%v", partial, err)
	}
	retried, err := service.Handle(context.Background(), request)
	if err != nil || retried.Context == nil || retried.Context.ID != testContextID || retried.Created {
		t.Fatalf("initialization retry response=%+v err=%v", retried, err)
	}
	if len(initializer.calls) != 2 || initializer.calls[0].ID != testContextID || initializer.calls[1].ID != testContextID {
		t.Fatalf("initializer did not retry exact context: %+v", initializer.calls)
	}
	if !reflect.DeepEqual(runner.calls, []sessionstate.ContextID{testContextID}) {
		t.Fatalf("initialization retry restored another window: %v", runner.calls)
	}
}

func TestServiceRejectsArchiveRacingMappedContextFocus(t *testing.T) {
	service, request, client, runner := testService(t)
	contextValue := registeredContext(request)
	if err := sessionstate.RegistryStoreFor(service.StateRoot).Save(sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue},
	}); err != nil {
		t.Fatal(err)
	}
	client.workspace = request.Workspace
	client.mapped = contextValue.ID
	client.onTree = func(requestNumber int) error {
		if requestNumber != 1 {
			return nil
		}
		_, err := sessionstate.UpdateRegistry(service.StateRoot, func(registry *sessionstate.Registry) error {
			_, err := sessionstate.SetContextState(registry, string(contextValue.ID), sessionstate.ContextArchived)
			return err
		})
		return err
	}

	_, err := service.Handle(context.Background(), request)

	if err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("archive race was accepted: %v", err)
	}
	if len(client.commands) != 0 || len(runner.calls) != 0 {
		t.Fatalf("archive race caused effects: commands=%v restores=%v", client.commands, runner.calls)
	}
}

func TestServiceV1ReusesManualContextThatLooksLikeFreshTerminal(t *testing.T) {
	service, request, client, runner := testService(t)
	request.Workspace = 98
	request.Provider = sessionstate.TerminalContextProvider
	var err error
	request.Session, err = sessionstate.DeriveTerminalInstanceSessionName(testContextID)
	if err != nil {
		t.Fatal(err)
	}
	contextValue := registeredContext(request)
	if sessionstate.IsTerminalInstanceContext(contextValue) {
		t.Fatal("manual context was classified as a fresh terminal instance")
	}
	if err := sessionstate.RegistryStoreFor(service.StateRoot).Save(sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue},
	}); err != nil {
		t.Fatal(err)
	}
	response, err := service.Handle(context.Background(), request)
	if err != nil || response.Created || response.Context == nil || response.Context.ID != contextValue.ID {
		t.Fatalf("manual lookalike context was not reused: response=%+v err=%v", response, err)
	}
	if len(client.commands) == 0 || !reflect.DeepEqual(runner.calls, []sessionstate.ContextID{contextValue.ID}) {
		t.Fatalf("manual lookalike context did not reach normal focus/restore: commands=%v restores=%v", client.commands, runner.calls)
	}
}

func TestServiceRejectsSavedWorkspaceConflictBeforeRestore(t *testing.T) {
	service, request, client, runner := testService(t)
	contextValue := registeredContext(request)
	if err := sessionstate.RegistryStoreFor(service.StateRoot).Save(sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sessionstate.LayoutStoreFor(service.StateRoot).Save(sessionstate.LayoutSnapshot{
		Version: sessionstate.LayoutSchemaVersion,
		Workspaces: []sessionstate.WorkspaceLayout{{
			Name: "4: saved", RestoreMode: sessionstate.WorkspaceRestorePlacementOnly,
			PlacementContexts: []sessionstate.ContextID{contextValue.ID},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Handle(context.Background(), request); err == nil || !strings.Contains(err.Error(), "saved placement") {
		t.Fatalf("conflicting saved workspace was accepted: %v", err)
	}
	if len(client.commands) != 0 || len(runner.calls) != 0 {
		t.Fatalf("workspace conflict caused effects: commands=%v restores=%v", client.commands, runner.calls)
	}
}

func TestServiceReusesContextWithCompatibleNamedWorkspace(t *testing.T) {
	service, request, client, runner := testService(t)
	contextValue := registeredContext(request)
	if err := sessionstate.RegistryStoreFor(service.StateRoot).Save(sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sessionstate.LayoutStoreFor(service.StateRoot).Save(sessionstate.LayoutSnapshot{
		Version: sessionstate.LayoutSchemaVersion,
		Workspaces: []sessionstate.WorkspaceLayout{{
			Name: "7: requested", RestoreMode: sessionstate.WorkspaceRestorePlacementOnly,
			PlacementContexts: []sessionstate.ContextID{contextValue.ID},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	response, err := service.Handle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Created || response.Context == nil || response.Context.ID != contextValue.ID {
		t.Fatalf("compatible context was not reused: %+v", response)
	}
	if !reflect.DeepEqual(client.commands, []string{"workspace number 7"}) || !reflect.DeepEqual(runner.calls, []sessionstate.ContextID{contextValue.ID}) {
		t.Fatalf("unexpected effects: commands=%v restores=%v", client.commands, runner.calls)
	}
}

func TestServiceRejectsContextMetadataConflictsWithoutEffects(t *testing.T) {
	for name, mutate := range map[string]func(*Request){
		"session metadata": func(value *Request) { value.Cwd = t.TempDir() },
		"duplicate label":  func(value *Request) { value.Session = "different-session" },
	} {
		t.Run(name, func(t *testing.T) {
			service, request, client, runner := testService(t)
			contextValue := registeredContext(request)
			if err := sessionstate.RegistryStoreFor(service.StateRoot).Save(sessionstate.Registry{
				Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue},
			}); err != nil {
				t.Fatal(err)
			}
			mutate(&request)
			if _, err := service.Handle(context.Background(), request); err == nil || !strings.Contains(err.Error(), "conflict") && !strings.Contains(err.Error(), "already used") {
				t.Fatalf("metadata conflict was accepted: %v", err)
			}
			if len(client.commands) != 0 || len(runner.calls) != 0 {
				t.Fatalf("metadata conflict caused effects: commands=%v restores=%v", client.commands, runner.calls)
			}
		})
	}
}

func TestServiceV1RejectsStableOrNonAlacrittyTerminalContexts(t *testing.T) {
	for name, terminal := range map[string]*sessionstate.TerminalLauncher{
		"stable identity": {
			Adapter:  sessionstate.TerminalAdapterAlacritty,
			Identity: &sessionstate.TerminalIdentity{Kind: sessionstate.TerminalIdentityDefault},
		},
		"foot adapter": {Adapter: sessionstate.TerminalAdapterFoot},
	} {
		t.Run(name, func(t *testing.T) {
			service, request, client, runner := testService(t)
			contextValue := registeredContext(request)
			contextValue.Launcher.Terminal = terminal
			if err := sessionstate.RegistryStoreFor(service.StateRoot).Save(sessionstate.Registry{
				Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Handle(context.Background(), request); err == nil || !strings.Contains(err.Error(), "conflicting context metadata") {
				t.Fatalf("protocol-v1-incompatible context was reused: %v", err)
			}
			if len(client.commands) != 0 || len(runner.calls) != 0 {
				t.Fatalf("protocol-v1-incompatible context caused effects: commands=%v restores=%v", client.commands, runner.calls)
			}
		})
	}
}

func TestServiceV1RejectsFreshTerminalInstanceContext(t *testing.T) {
	service, request, client, runner := testService(t)
	request.Provider = sessionstate.TerminalContextProvider
	var err error
	request.Session, err = sessionstate.DeriveTerminalInstanceSessionName(testContextID)
	if err != nil {
		t.Fatal(err)
	}
	contextValue := registeredContext(request)
	contextValue.Launcher.Terminal.Instance = true
	if err := sessionstate.RegistryStoreFor(service.StateRoot).Save(sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Handle(context.Background(), request); err == nil || !strings.Contains(err.Error(), "conflicting context metadata") {
		t.Fatalf("fresh terminal instance was exposed through protocol v1: %v", err)
	}
	if len(client.commands) != 0 || len(runner.calls) != 0 {
		t.Fatalf("fresh terminal instance caused broker effects: commands=%v restores=%v", client.commands, runner.calls)
	}
}

func registeredContext(request Request) sessionstate.Context {
	return sessionstate.Context{
		ID: testContextID, Label: request.Label, Provider: request.Provider, State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: request.Session, Cwd: request.Cwd, Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}},
	}
}

func TestServiceRejectsOccupiedWorkspaceBeforeRegistration(t *testing.T) {
	service, request, client, runner := testService(t)
	client.workspace = request.Workspace
	client.occupied = true
	if _, err := service.Handle(context.Background(), request); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("occupied workspace was accepted: %v", err)
	}
	registry, loadErr := loadRegistry(service.StateRoot)
	if loadErr != nil || len(registry.Contexts) != 0 || len(client.commands) != 0 || len(runner.calls) != 0 {
		t.Fatalf("occupied rejection changed state: registry=%+v err=%v", registry, loadErr)
	}
}

func TestServiceRollsBackNewRegistrationWhenWorkspaceBecomesOccupiedBeforeRestore(t *testing.T) {
	service, request, client, runner := testService(t)
	service.NewContextID = func() (sessionstate.ContextID, error) {
		client.workspace = request.Workspace
		client.occupied = true
		return testContextID, nil
	}
	if _, err := service.Handle(context.Background(), request); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("workspace race was accepted: %v", err)
	}
	registry, loadErr := loadRegistry(service.StateRoot)
	if loadErr != nil || len(registry.Contexts) != 0 || len(client.commands) != 0 || len(runner.calls) != 0 {
		t.Fatalf("pre-restore failure leaked a registration: registry=%+v err=%v commands=%v restores=%v", registry, loadErr, client.commands, runner.calls)
	}
	activity, activityErr := sessionstate.ReadTerminalActivitySnapshot(service.StateRoot)
	if activityErr != nil {
		t.Fatal(activityErr)
	}
	if _, exists := sessionstate.FindTerminalActivity(activity, testContextID); exists {
		t.Fatalf("pre-restore rollback retained activity: %+v", activity)
	}
}

func TestServiceKeepsReusedRegistrationWhenWorkspaceBecomesOccupiedBeforeRestore(t *testing.T) {
	service, request, client, runner := testService(t)
	contextValue := registeredContext(request)
	if err := sessionstate.RegistryStoreFor(service.StateRoot).Save(sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue},
	}); err != nil {
		t.Fatal(err)
	}
	client.occupyOnTree = 2
	client.occupyWorkspace = request.Workspace
	if _, err := service.Handle(context.Background(), request); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("workspace race was accepted: %v", err)
	}
	registry, loadErr := loadRegistry(service.StateRoot)
	if loadErr != nil || len(registry.Contexts) != 1 || registry.Contexts[0].ID != contextValue.ID || len(client.commands) != 0 || len(runner.calls) != 0 {
		t.Fatalf("reused registration was rolled back: registry=%+v err=%v commands=%v restores=%v", registry, loadErr, client.commands, runner.calls)
	}
}

func TestServiceKeepsRegistrationForRetryAfterRestoreFailure(t *testing.T) {
	service, request, _, runner := testService(t)
	runner.err = errors.New("restore unavailable")
	if _, err := service.Handle(context.Background(), request); err == nil || !strings.Contains(err.Error(), "restore unavailable") {
		t.Fatalf("restore failure was hidden: %v", err)
	}
	registry, loadErr := loadRegistry(service.StateRoot)
	if loadErr != nil || len(registry.Contexts) != 1 || len(runner.calls) != 1 {
		t.Fatalf("failed restore is not retryable: registry=%+v err=%v", registry, loadErr)
	}
	activity, activityErr := sessionstate.ReadTerminalActivitySnapshot(service.StateRoot)
	if activityErr != nil {
		t.Fatal(activityErr)
	}
	if created, exists := sessionstate.FindTerminalActivity(activity, testContextID); !exists || created.CreatedAt == nil {
		t.Fatalf("retryable registration lost creation activity: %+v", activity)
	}
}

func TestServiceRejectsArchiveAfterRestoreBeforeFinalObservation(t *testing.T) {
	service, request, _, runner := testService(t)
	runner.afterRestore = func() error {
		_, err := sessionstate.UpdateRegistry(service.StateRoot, func(registry *sessionstate.Registry) error {
			_, err := sessionstate.SetContextState(registry, string(testContextID), sessionstate.ContextArchived)
			return err
		})
		return err
	}

	_, err := service.Handle(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("archive after restore was accepted: %v", err)
	}
	registry, loadErr := loadRegistry(service.StateRoot)
	if loadErr != nil || len(registry.Contexts) != 1 || registry.Contexts[0].State != sessionstate.ContextArchived {
		t.Fatalf("archived registry state was not preserved: registry=%+v err=%v", registry, loadErr)
	}
}

func TestServiceRejectsWorkspaceOccupantAppearingDuringRestore(t *testing.T) {
	for _, floating := range []bool{false, true} {
		name := "tiled"
		if floating {
			name = "floating"
		}
		t.Run(name, func(t *testing.T) {
			service, request, client, runner := testService(t)
			runner.afterRestore = func() error {
				client.occupied = !floating
				client.floatingOccupied = floating
				return nil
			}

			_, err := service.Handle(context.Background(), request)
			if err == nil || !strings.Contains(err.Error(), "not exclusive") {
				t.Fatalf("%s workspace occupant was accepted: %v", name, err)
			}
		})
	}
}

func TestServiceRejectsMappedContextOnMixedWorkspace(t *testing.T) {
	service, request, client, runner := testService(t)
	contextValue := registeredContext(request)
	if err := sessionstate.RegistryStoreFor(service.StateRoot).Save(sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue},
	}); err != nil {
		t.Fatal(err)
	}
	client.workspace = request.Workspace
	client.mapped = contextValue.ID
	client.occupied = true

	_, err := service.Handle(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "not exclusive") {
		t.Fatalf("mapped context on mixed workspace was accepted: %v", err)
	}
	if len(client.commands) != 0 || len(runner.calls) != 0 {
		t.Fatalf("mixed mapped workspace caused effects: commands=%v restores=%v", client.commands, runner.calls)
	}
}

func TestSystemExecutableEnvironmentDropsShadowingAndLoaderVariables(t *testing.T) {
	t.Setenv("PATH", "/home/test/.local/bin:/usr/bin")
	t.Setenv("LD_PRELOAD", "/home/test/inject.so")
	t.Setenv("SWAYSOCK", "/run/user/1000/sway.sock")
	environment := systemExecutableEnvironment()
	joined := "\n" + strings.Join(environment, "\n") + "\n"
	if strings.Contains(joined, "\nLD_PRELOAD=") || strings.Contains(joined, "/home/test/.local/bin") {
		t.Fatalf("unsafe environment survived: %v", environment)
	}
	if !strings.Contains(joined, "\nPATH=/usr/local/sbin:/usr/local/bin:/usr/bin\n") || !strings.Contains(joined, "\nSWAYSOCK=/run/user/1000/sway.sock\n") {
		t.Fatalf("required environment is missing: %v", environment)
	}
}
