package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/statefile"
	"github.com/marang/sway-session/internal/swayipc"
)

func TestTerminalManagerCreatesAttachesThenReusesFocusedDefault(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	client := &terminalManagerClient{id: testContextID}
	starter := &terminalManagerStarter{onStart: func() { client.setMapped(true) }}
	manager := terminalTestManager(root, client, starter)
	createdAt := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	manager.Now = func() time.Time { return createdAt }
	request := TerminalOpenRequest{
		Identity: TerminalIdentity{Kind: TerminalIdentityDefault},
		Adapter:  TerminalAdapterAlacritty,
		Cwd:      t.TempDir(),
		Focus:    true,
	}

	first, err := manager.Open(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Context.ID != testContextID || !reflect.DeepEqual(first.Actions, []TerminalOpenAction{
		TerminalActionCreated, TerminalActionAttached, TerminalActionFocused,
	}) {
		t.Fatalf("unexpected first open: %+v", first)
	}
	activity, err := ReadTerminalActivitySnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	createdActivity, exists := FindTerminalActivity(activity, first.Context.ID)
	if !exists || createdActivity.CreatedAt == nil || !createdActivity.CreatedAt.Equal(createdAt) || createdActivity.LastFocusedAt != nil {
		t.Fatalf("terminal creation activity = %+v, exists=%t", createdActivity, exists)
	}
	manager.SessionManager = failingTerminalSessionManager{err: errors.New("visible terminal must not resolve session manager")}
	manager.Now = func() time.Time { return createdAt.Add(2 * time.Minute) }
	second, err := manager.Open(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if second.Context.ID != first.Context.ID || !reflect.DeepEqual(second.Actions, []TerminalOpenAction{
		TerminalActionReused, TerminalActionNoChange,
	}) {
		t.Fatalf("unexpected repeated open: %+v", second)
	}
	if len(starter.specs) != 1 {
		t.Fatalf("repeated open launched %d processes", len(starter.specs))
	}
	activity, err = ReadTerminalActivitySnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	reusedActivity, exists := FindTerminalActivity(activity, first.Context.ID)
	if !exists || !reflect.DeepEqual(reusedActivity, createdActivity) {
		t.Fatalf("reuse or manager focus changed daemon-owned activity: got=%+v want=%+v", reusedActivity, createdActivity)
	}
}

func TestTerminalManagerRetainsNewContextWhenStartedAdapterWindowDisappears(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	client := &sequencedTerminalManagerClient{
		id:      testContextID,
		mapped:  []bool{false, true, false, false},
		focused: true,
	}
	starter := &terminalManagerStarter{}
	manager := terminalTestManager(root, nil, starter)
	manager.Client = client
	manager.StabilityDelay = time.Millisecond

	result, err := manager.Open(context.Background(), TerminalOpenRequest{
		New: true, Adapter: TerminalAdapterAlacritty, Cwd: t.TempDir(), Focus: true,
	})
	if !errors.Is(err, ErrTerminalWindowUnavailable) || !strings.Contains(err.Error(), "remains active") {
		t.Fatalf("disappearing terminal was reported as successful: result=%+v err=%v", result, err)
	}
	if !reflect.DeepEqual(result.Actions, []TerminalOpenAction{TerminalActionCreated}) {
		t.Fatalf("failed terminal reported completed actions: %+v", result.Actions)
	}
	if len(starter.specs) != 1 {
		t.Fatalf("failed terminal launched %d adapters", len(starter.specs))
	}
	var registry Registry
	if loadErr := RegistryStoreFor(root).LoadInto(&registry); loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(registry.Contexts) != 1 || registry.Contexts[0].ID != testContextID {
		t.Fatalf("failed terminal lost its backend recovery identity: %+v", registry.Contexts)
	}
}

func TestTerminalManagerCancellationAfterRegistryCommitRollsBackNewContext(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	manager := terminalTestManager(root, &terminalManagerClient{id: testContextID}, &terminalManagerStarter{})
	ctx, cancel := context.WithCancel(context.Background())
	manager.BeforeWindowTxn = cancel

	result, err := manager.Open(ctx, TerminalOpenRequest{
		New: true, Adapter: TerminalAdapterAlacritty, Cwd: t.TempDir(), Focus: true,
	})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("post-commit cancellation was not rolled back: result=%+v err=%v", result, err)
	}
	if !reflect.DeepEqual(result, TerminalOpenResult{}) {
		t.Fatalf("rolled-back context was returned as active: %+v", result)
	}
	var registry Registry
	if loadErr := RegistryStoreFor(root).LoadInto(&registry); loadErr != nil || len(registry.Contexts) != 0 {
		t.Fatalf("post-commit cancellation left registry=%+v err=%v", registry, loadErr)
	}
	activity, loadErr := ReadTerminalActivitySnapshot(root)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if _, exists := FindTerminalActivity(activity, testContextID); exists {
		t.Fatalf("rolled-back context retained activity: %+v", activity)
	}
}

func TestTerminalManagerPreservesNewContextWhenAdapterSurvivesCanceledOpen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	manager := terminalTestManager(root, &terminalManagerClient{id: testContextID}, &terminalManagerStarter{})
	manager.FindProcesses = func(string, ContextID) ([]int, error) { return []int{4321}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	manager.BeforeWindowTxn = cancel

	_, err := manager.Open(ctx, TerminalOpenRequest{
		New: true, Adapter: TerminalAdapterAlacritty, Cwd: t.TempDir(), Focus: true,
	})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "remained pending before rollback") {
		t.Fatalf("live adapter did not prevent rollback: %v", err)
	}
	var registry Registry
	if loadErr := RegistryStoreFor(root).LoadInto(&registry); loadErr != nil || len(registry.Contexts) != 1 || registry.Contexts[0].ID != testContextID {
		t.Fatalf("live adapter lost recovery identity: registry=%+v err=%v", registry, loadErr)
	}
	activity, loadErr := ReadTerminalActivitySnapshot(root)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if created, exists := FindTerminalActivity(activity, testContextID); !exists || created.CreatedAt == nil {
		t.Fatalf("recoverable context lost creation activity: %+v", activity)
	}
}

func TestTerminalManagerFailedInitializationStillRejectsDisappearedWindow(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	client := &sequencedTerminalManagerClient{
		id:      testContextID,
		mapped:  []bool{false, true, true, false},
		focused: true,
	}
	manager := terminalTestManager(root, nil, &terminalManagerStarter{})
	manager.Client = client
	manager.SessionManager = testTerminalSessionManager{
		root: "/tmp/herdr-test", configFile: "/tmp/herdr-test/config.toml",
		initialize: func(_ context.Context, _ Context, roles []string) (TerminalSessionInitialization, error) {
			return TerminalSessionInitialization{
				Manager: TerminalSessionManagerHerdr, Roles: append([]string(nil), roles...),
			}, errors.New("agent trust required")
		},
	}

	result, err := manager.Open(context.Background(), TerminalOpenRequest{
		New: true, Adapter: TerminalAdapterAlacritty, Cwd: t.TempDir(), Focus: true,
		Roles: []string{"codex", "shell"},
	})
	if !errors.Is(err, ErrTerminalWindowUnavailable) || !strings.Contains(err.Error(), "agent trust required") ||
		!strings.Contains(err.Error(), "remains active") {
		t.Fatalf("failed initialization hid the unstable window: result=%+v err=%v", result, err)
	}
	if !reflect.DeepEqual(result.Actions, []TerminalOpenAction{TerminalActionCreated}) {
		t.Fatalf("failed initialization reported stale window actions: %+v", result.Actions)
	}
	var registry Registry
	if loadErr := RegistryStoreFor(root).LoadInto(&registry); loadErr != nil || len(registry.Contexts) != 1 || registry.Contexts[0].ID != testContextID {
		t.Fatalf("failed initialization lost recovery identity: registry=%+v err=%v", registry, loadErr)
	}
}

func TestTerminalManagerProjectRecoveryReconcilesRolesWithoutRestartingAgent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	identity := TerminalIdentity{Kind: TerminalIdentityProject, Project: "LAB-112"}
	contextValue := testValidContext(testContextID)
	contextValue.Launcher.Cwd = t.TempDir()
	contextValue.Launcher.Terminal.Identity = &identity
	if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{contextValue}}); err != nil {
		t.Fatal(err)
	}
	client := &sequencedTerminalManagerClient{
		id:      testContextID,
		mapped:  []bool{false, true, true},
		focused: true,
	}
	starter := &terminalManagerStarter{}
	manager := terminalTestManager(root, nil, starter)
	manager.Client = client
	manager.StabilityDelay = time.Millisecond
	initializations := 0
	manager.SessionManager = testTerminalSessionManager{
		root: "/tmp/herdr-test", configFile: "/tmp/herdr-test/config.toml",
		initialize: func(_ context.Context, _ Context, roles []string) (TerminalSessionInitialization, error) {
			initializations++
			return TerminalSessionInitialization{
				Manager: TerminalSessionManagerHerdr, Roles: append([]string(nil), roles...),
				Reason: "Herdr session was not proven empty and was left unchanged",
			}, nil
		},
	}

	result, err := manager.Open(context.Background(), TerminalOpenRequest{
		Identity: identity, Adapter: TerminalAdapterAlacritty, Cwd: contextValue.Launcher.Cwd,
		Focus: true, Roles: []string{"codex", "shell"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Initialization == nil || result.Initialization.Initialized || initializations != 1 || !reflect.DeepEqual(result.Actions, []TerminalOpenAction{
		TerminalActionReused, TerminalActionAttached, TerminalActionFocused,
	}) {
		t.Fatalf("unexpected recovered terminal result: %+v", result)
	}
	if len(starter.specs) != 1 {
		t.Fatalf("project recovery launched %d adapters", len(starter.specs))
	}
}

func TestTerminalManagerRetainsRecoveryIdentityWhenWindowDisappearsDuringInitialization(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	client := &sequencedTerminalManagerClient{
		id:      testContextID,
		mapped:  []bool{false, true, true, false, false},
		focused: true,
	}
	manager := terminalTestManager(root, nil, &terminalManagerStarter{})
	manager.Client = client
	manager.SessionManager = testTerminalSessionManager{
		root: "/tmp/herdr-test", configFile: "/tmp/herdr-test/config.toml",
		initialize: func(_ context.Context, contextValue Context, roles []string) (TerminalSessionInitialization, error) {
			return TerminalSessionInitialization{
				Manager: TerminalSessionManagerHerdr, Roles: append([]string(nil), roles...), Initialized: true,
			}, nil
		},
	}

	result, err := manager.Open(context.Background(), TerminalOpenRequest{
		New: true, Adapter: TerminalAdapterAlacritty, Cwd: t.TempDir(), Focus: true,
		Roles: []string{"codex", "shell"},
	})
	if !errors.Is(err, ErrTerminalWindowUnavailable) || !strings.Contains(err.Error(), "remains active") {
		t.Fatalf("post-initialization disappearance was successful: result=%+v err=%v", result, err)
	}
	if !reflect.DeepEqual(result.Actions, []TerminalOpenAction{TerminalActionCreated}) {
		t.Fatalf("post-initialization failure reported completed actions: %+v", result.Actions)
	}
	var registry Registry
	if loadErr := RegistryStoreFor(root).LoadInto(&registry); loadErr != nil || len(registry.Contexts) != 1 || registry.Contexts[0].ID != testContextID {
		t.Fatalf("post-initialization failure registry=%+v err=%v", registry, loadErr)
	}
}

func TestTerminalManagerReconcilesAmbiguousExactFocusWithoutReplay(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	contextValue := testValidContext(testContextID)
	identity := TerminalIdentity{Kind: TerminalIdentityDefault}
	contextValue.Launcher.Terminal.Identity = &identity
	if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{contextValue}}); err != nil {
		t.Fatal(err)
	}
	client := &terminalManagerClient{id: testContextID, mapped: true, focused: false, ambiguousFocus: true}
	manager := terminalTestManager(root, client, &terminalManagerStarter{})

	result, err := manager.Open(context.Background(), TerminalOpenRequest{
		Identity: identity, Adapter: TerminalAdapterAlacritty, Cwd: contextValue.Launcher.Cwd, Focus: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Actions, []TerminalOpenAction{TerminalActionReused, TerminalActionFocused}) || client.focusCommands != 1 {
		t.Fatalf("ambiguous focus was not reconciled exactly once: result=%+v commands=%d", result, client.focusCommands)
	}
}

func TestTerminalManagerRejectsAlreadyFocusedContainerReplacement(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	contextValue := testValidContext(testContextID)
	contextValue.Launcher.Cwd = t.TempDir()
	identity := TerminalIdentity{Kind: TerminalIdentityDefault}
	contextValue.Launcher.Terminal.Identity = &identity
	if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{contextValue}}); err != nil {
		t.Fatal(err)
	}
	client := &sequencedTerminalManagerClient{
		id:           testContextID,
		mapped:       []bool{true, true},
		containerIDs: []int64{42, 43},
		focused:      true,
	}
	manager := terminalTestManager(root, nil, &terminalManagerStarter{})
	manager.Client = client

	result, err := manager.Open(context.Background(), TerminalOpenRequest{
		Identity: identity, Adapter: TerminalAdapterAlacritty, Cwd: contextValue.Launcher.Cwd, Focus: true,
	})
	if !errors.Is(err, ErrTerminalWindowUnavailable) || !reflect.DeepEqual(result.Actions, []TerminalOpenAction{TerminalActionReused}) {
		t.Fatalf("replacement of focused container was accepted: result=%+v err=%v", result, err)
	}
}

func TestTerminalManagerAcceptsMappedContainerAfterUserChangesFocus(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	contextValue := testValidContext(testContextID)
	contextValue.Launcher.Cwd = t.TempDir()
	identity := TerminalIdentity{Kind: TerminalIdentityDefault}
	contextValue.Launcher.Terminal.Identity = &identity
	if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{contextValue}}); err != nil {
		t.Fatal(err)
	}
	client := &sequencedTerminalManagerClient{
		id:            testContextID,
		mapped:        []bool{true, true},
		focusedStates: []bool{true, false},
	}
	manager := terminalTestManager(root, nil, &terminalManagerStarter{})
	manager.Client = client

	result, err := manager.Open(context.Background(), TerminalOpenRequest{
		Identity: identity, Adapter: TerminalAdapterAlacritty, Cwd: contextValue.Launcher.Cwd, Focus: true,
	})
	if err != nil {
		t.Fatalf("healthy terminal failed after user focus change: result=%+v err=%v", result, err)
	}
	if !reflect.DeepEqual(result.Actions, []TerminalOpenAction{TerminalActionReused, TerminalActionNoChange}) {
		t.Fatalf("unexpected result after user focus change: %+v", result)
	}
}

func TestTerminalManagerAcceptsMappedContainerAfterFocusChangesDuringInitialization(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	contextValue := testValidContext(testContextID)
	contextValue.Launcher.Cwd = t.TempDir()
	identity := TerminalIdentity{Kind: TerminalIdentityDefault}
	contextValue.Launcher.Terminal.Identity = &identity
	if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{contextValue}}); err != nil {
		t.Fatal(err)
	}
	client := &sequencedTerminalManagerClient{
		id:            testContextID,
		mapped:        []bool{true, true, true},
		focusedStates: []bool{true, true, false},
	}
	manager := terminalTestManager(root, nil, &terminalManagerStarter{})
	manager.Client = client

	result, err := manager.Open(context.Background(), TerminalOpenRequest{
		Identity: identity, Adapter: TerminalAdapterAlacritty, Cwd: contextValue.Launcher.Cwd,
		Focus: true, Roles: []string{"codex", "shell"},
	})
	if err != nil {
		t.Fatalf("healthy terminal failed after focus changed during initialization: result=%+v err=%v", result, err)
	}
	if !reflect.DeepEqual(result.Actions, []TerminalOpenAction{TerminalActionReused, TerminalActionNoChange}) || result.Initialization == nil || !result.Initialization.Initialized {
		t.Fatalf("unexpected initialized result after user focus change: %+v", result)
	}
}

func TestTerminalManagerConcurrentOpenCreatesOneContextAndOneProcess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	client := &terminalManagerClient{id: testContextID}
	starter := &terminalManagerStarter{onStart: func() { client.setMapped(true) }}
	manager := terminalTestManager(root, client, starter)
	var idMu sync.Mutex
	idCalls := 0
	manager.NewContextID = func() (ContextID, error) {
		idMu.Lock()
		defer idMu.Unlock()
		idCalls++
		if idCalls == 1 {
			return testContextID, nil
		}
		return "6ba7b810-9dad-41d1-80b4-00c04fd430c8", nil
	}
	request := TerminalOpenRequest{
		Identity: TerminalIdentity{Kind: TerminalIdentityDefault},
		Adapter:  TerminalAdapterAlacritty,
		Cwd:      t.TempDir(),
		Focus:    true,
	}
	results := make(chan TerminalOpenResult, 2)
	errorsCh := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := manager.Open(context.Background(), request)
			results <- result
			errorsCh <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent terminal open: %v", err)
		}
	}
	for result := range results {
		if result.Context.ID != testContextID {
			t.Fatalf("concurrent open resolved another context: %+v", result)
		}
	}
	starter.mu.Lock()
	starts := len(starter.specs)
	starter.mu.Unlock()
	if starts != 1 {
		t.Fatalf("concurrent open started %d terminal processes", starts)
	}
	idMu.Lock()
	generated := idCalls
	idMu.Unlock()
	if generated != 1 {
		t.Fatalf("concurrent reuse generated %d context IDs", generated)
	}
	var registry Registry
	if err := RegistryStoreFor(root).LoadInto(&registry); err != nil || len(registry.Contexts) != 1 {
		t.Fatalf("concurrent open registry=%+v err=%v", registry, err)
	}
}

func TestTerminalManagerSerializesCreationThroughWindowTransaction(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	client := &terminalManagerClient{id: testContextID}
	starter := &terminalManagerStarter{onStart: func() { client.setMapped(true) }}
	creator := terminalTestManager(root, client, starter)
	request := TerminalOpenRequest{
		Identity: TerminalIdentity{Kind: TerminalIdentityDefault},
		Adapter:  TerminalAdapterAlacritty,
		Cwd:      t.TempDir(),
		Focus:    true,
	}
	creatorPaused := make(chan struct{})
	releaseCreator := make(chan struct{})
	creator.BeforeWindowTxn = func() {
		close(creatorPaused)
		<-releaseCreator
	}
	creatorDone := make(chan error, 1)
	go func() {
		_, err := creator.Open(context.Background(), request)
		creatorDone <- err
	}()
	select {
	case <-creatorPaused:
	case <-time.After(time.Second):
		t.Fatal("creator did not reach the window transaction")
	}

	reuser := terminalTestManager(root, client, starter)
	registryUpdates := 0
	reuser.RegistryUpdate = func(ctx context.Context, root string, mutate func(*Registry) error) (Registry, error) {
		registryUpdates++
		return UpdateRegistryContext(ctx, root, mutate)
	}
	reuseCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := reuser.Open(reuseCtx, request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent reuser bypassed lifecycle serialization: %v", err)
	}
	if registryUpdates != 0 {
		t.Fatalf("concurrent reuser entered registry transaction %d times", registryUpdates)
	}
	close(releaseCreator)
	select {
	case err := <-creatorDone:
		if err != nil {
			t.Fatalf("creator failed after serialized reuser cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("creator did not finish after release")
	}
}

func TestTerminalManagerRetainsContextAfterConcurrentPendingLaunchMayCreateManagerState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	manager := terminalTestManager(root, &terminalManagerClient{id: testContextID}, &terminalManagerStarter{})
	ctx, cancel := context.WithCancel(context.Background())
	manager.FindPending = func(string, ProcessSpec) ([]int, error) {
		cancel()
		return []int{4321}, nil
	}
	manager.FindProcesses = func(string, ContextID) ([]int, error) { return nil, nil }

	result, err := manager.Open(ctx, TerminalOpenRequest{
		New: true, Adapter: TerminalAdapterAlacritty, Cwd: t.TempDir(), Focus: true,
	})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "remains active") {
		t.Fatalf("pending concurrent launch did not preserve the recovery identity: result=%+v err=%v", result, err)
	}
	if !reflect.DeepEqual(result.Actions, []TerminalOpenAction{TerminalActionCreated}) {
		t.Fatalf("failed creator reported completed actions: %+v", result.Actions)
	}
	var registry Registry
	if loadErr := RegistryStoreFor(root).LoadInto(&registry); loadErr != nil || len(registry.Contexts) != 1 || registry.Contexts[0].ID != testContextID {
		t.Fatalf("pending manager state lost its recovery identity: registry=%+v err=%v", registry, loadErr)
	}
}

func TestTerminalManagerRetainsContextAfterConflictingTargetWindowsDisappear(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	client := &sequencedTerminalManagerClient{
		id:         testContextID,
		mapped:     []bool{true, false},
		duplicates: []bool{true, false},
		focused:    true,
	}
	manager := terminalTestManager(root, nil, &terminalManagerStarter{})
	manager.Client = client
	manager.FindProcesses = func(string, ContextID) ([]int, error) { return nil, nil }

	result, err := manager.Open(context.Background(), TerminalOpenRequest{
		New: true, Adapter: TerminalAdapterAlacritty, Cwd: t.TempDir(), Focus: true,
	})
	if err == nil || !strings.Contains(err.Error(), "appears in containers") || !strings.Contains(err.Error(), "remains active") {
		t.Fatalf("conflicting target windows did not preserve the recovery identity: result=%+v err=%v", result, err)
	}
	var registry Registry
	if loadErr := RegistryStoreFor(root).LoadInto(&registry); loadErr != nil || len(registry.Contexts) != 1 || registry.Contexts[0].ID != testContextID {
		t.Fatalf("conflicting manager windows lost their recovery identity: registry=%+v err=%v", registry, loadErr)
	}
}

func TestTerminalManagerConcurrentNewCreatesIndependentContextsAndProcesses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	client := &multiTerminalManagerClient{windows: make(map[string]int64)}
	starter := &terminalManagerStarter{onStartSpec: client.mapProcess}
	manager := terminalTestManager(root, nil, starter)
	manager.Client = client
	ids := []ContextID{
		testContextID,
		"6ba7b810-9dad-41d1-80b4-00c04fd430c8",
	}
	var idMu sync.Mutex
	manager.NewContextID = func() (ContextID, error) {
		idMu.Lock()
		defer idMu.Unlock()
		id := ids[0]
		ids = ids[1:]
		return id, nil
	}
	request := TerminalOpenRequest{
		New:     true,
		Adapter: TerminalAdapterAlacritty,
		Cwd:     t.TempDir(),
		Focus:   true,
	}

	results := make(chan TerminalOpenResult, 2)
	errorsCh := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := manager.Open(context.Background(), request)
			results <- result
			errorsCh <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent new terminal: %v", err)
		}
	}
	seen := make(map[ContextID]struct{}, 2)
	for result := range results {
		if !reflect.DeepEqual(result.Actions, []TerminalOpenAction{
			TerminalActionCreated, TerminalActionAttached, TerminalActionFocused,
		}) {
			t.Fatalf("unexpected concurrent new result: %+v", result)
		}
		seen[result.Context.ID] = struct{}{}
	}
	if len(seen) != 2 {
		t.Fatalf("concurrent --new reused a context: %+v", seen)
	}
	starter.mu.Lock()
	starts := len(starter.specs)
	starter.mu.Unlock()
	if starts != 2 {
		t.Fatalf("concurrent --new started %d terminal processes", starts)
	}
	var registry Registry
	if err := RegistryStoreFor(root).LoadInto(&registry); err != nil || len(registry.Contexts) != 2 ||
		registry.Contexts[0].Launcher.Session == registry.Contexts[1].Launcher.Session {
		t.Fatalf("concurrent --new registry=%+v err=%v", registry, err)
	}
}

func TestTerminalManagerNewRejectsOverlongHerdrSocketBeforePersistOrStart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	starter := &terminalManagerStarter{}
	manager := terminalTestManager(root, &terminalManagerClient{id: testContextID}, starter)
	longHerdrRoot := "/" + strings.Repeat("a", 39)
	manager.SessionManager = testTerminalSessionManager{root: longHerdrRoot, configFile: filepath.Join(longHerdrRoot, "config.toml")}

	_, err := manager.Open(context.Background(), TerminalOpenRequest{
		New: true, Adapter: TerminalAdapterAlacritty, Cwd: t.TempDir(), Focus: true,
	})
	if err == nil || !strings.Contains(err.Error(), herdrClientSocketFilename) || !strings.Contains(err.Error(), "shorten XDG_CONFIG_HOME") {
		t.Fatalf("overlong Herdr client socket was not rejected actionably: %v", err)
	}
	if len(starter.specs) != 0 {
		t.Fatalf("overlong Herdr socket started a process: %+v", starter.specs)
	}
	var registry Registry
	if err := RegistryStoreFor(root).LoadInto(&registry); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("overlong Herdr socket persisted registry=%+v err=%v", registry, err)
	}
}

func TestTerminalManagerNewReturnsIdentityWhenVisibleCommitCannotBeReobserved(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing-state", "sway-session")
	starter := &terminalManagerStarter{}
	manager := terminalTestManager(root, &terminalManagerClient{id: testContextID}, starter)
	wantCommitError := errors.New("directory sync failed")
	manager.RegistryUpdate = func(ctx context.Context, _ string, mutate func(*Registry) error) (Registry, error) {
		registry := emptyRegistry()
		if err := mutate(&registry); err != nil {
			return Registry{}, err
		}
		return registry, &statefile.CommitOutcomeUnknownError{Cause: wantCommitError}
	}

	result, err := manager.Open(context.Background(), TerminalOpenRequest{
		New: true, Adapter: TerminalAdapterAlacritty, Cwd: t.TempDir(), Focus: true,
	})
	if !errors.Is(err, wantCommitError) || !strings.Contains(err.Error(), "re-observe") {
		t.Fatalf("expected failed commit re-observation, got result=%+v err=%v", result, err)
	}
	wantSession, sessionErr := DeriveTerminalInstanceSessionName(testContextID)
	if sessionErr != nil {
		t.Fatal(sessionErr)
	}
	if result.Context.ID != testContextID || result.Context.Launcher.Session != wantSession ||
		!IsTerminalInstanceContext(result.Context) || !reflect.DeepEqual(result.Actions, []TerminalOpenAction{TerminalActionCreated}) {
		t.Fatalf("partial result lost visible fresh identity: %+v", result)
	}
	if len(starter.specs) != 0 {
		t.Fatalf("failed commit re-observation launched a process: %+v", starter.specs)
	}
}

func TestTerminalManagerRejectsInvalidLabelBeforeRegistryUpdate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	manager := terminalTestManager(root, &terminalManagerClient{id: testContextID}, &terminalManagerStarter{})
	manager.RegistryUpdate = func(context.Context, string, func(*Registry) error) (Registry, error) {
		t.Fatal("invalid label reached registry update")
		return Registry{}, nil
	}

	result, err := manager.Open(context.Background(), TerminalOpenRequest{
		New: true, Adapter: TerminalAdapterAlacritty, Cwd: t.TempDir(), Label: " terminal", Focus: true,
	})
	if err == nil || !strings.Contains(err.Error(), "label must not have surrounding whitespace") || result.Context.ID != "" {
		t.Fatalf("invalid terminal label was not rejected before effects: result=%+v err=%v", result, err)
	}
	if _, statErr := os.Stat(root); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid terminal label created state root: %v", statErr)
	}
}

func TestTerminalManagerRejectsControlCharacterCwdBeforeRegistryUpdate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	manager := terminalTestManager(root, &terminalManagerClient{id: testContextID}, &terminalManagerStarter{})
	manager.RegistryUpdate = func(context.Context, string, func(*Registry) error) (Registry, error) {
		t.Fatal("invalid cwd reached registry update")
		return Registry{}, nil
	}
	badCwd := filepath.Join(t.TempDir(), "terminal\nwork")
	if err := os.Mkdir(badCwd, 0o700); err != nil {
		t.Fatal(err)
	}

	result, err := manager.Open(context.Background(), TerminalOpenRequest{
		New: true, Adapter: TerminalAdapterAlacritty, Cwd: badCwd, Focus: true,
	})
	if err == nil || !strings.Contains(err.Error(), "control characters") || result.Context.ID != "" {
		t.Fatalf("invalid terminal cwd was not rejected before effects: result=%+v err=%v", result, err)
	}
	if _, statErr := os.Stat(root); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid terminal cwd created state root: %v", statErr)
	}
}

func TestTerminalManagerCancellationInterruptsSwayAndReleasesRegistryLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	client := &blockingTerminalManagerClient{started: make(chan struct{})}
	manager := terminalTestManager(root, nil, &terminalManagerStarter{})
	manager.Client = client
	ctx, cancel := context.WithCancel(context.Background())
	cwd := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := manager.Open(ctx, TerminalOpenRequest{
			Identity: TerminalIdentity{Kind: TerminalIdentityDefault},
			Adapter:  TerminalAdapterAlacritty,
			Cwd:      cwd,
			Focus:    true,
		})
		done <- err
	}()
	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("terminal manager did not begin the bounded Sway request")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("terminal manager did not return cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal manager ignored cancellation while holding registry lock")
	}
	lockCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := UpdateRegistryContext(lockCtx, root, func(*Registry) error { return nil }); err != nil {
		t.Fatalf("terminal manager retained registry lock after cancellation: %v", err)
	}
	var registry Registry
	if err := RegistryStoreFor(root).LoadInto(&registry); err != nil || len(registry.Contexts) != 0 {
		t.Fatalf("canceled terminal left registry=%+v err=%v", registry, err)
	}
}

func TestTerminalManagerRejectsArchiveBetweenEnsureAndWindowTransaction(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	client := &terminalManagerClient{id: testContextID}
	starter := &terminalManagerStarter{}
	manager := terminalTestManager(root, client, starter)
	request := TerminalOpenRequest{
		Identity: TerminalIdentity{Kind: TerminalIdentityDefault},
		Adapter:  TerminalAdapterAlacritty,
		Cwd:      t.TempDir(),
		Focus:    true,
	}
	manager.BeforeWindowTxn = func() {
		if _, err := UpdateRegistry(root, func(registry *Registry) error {
			_, err := SetContextStateAt(registry, string(testContextID), ContextArchived, time.Now())
			return err
		}); err != nil {
			t.Fatalf("archive between terminal transactions: %v", err)
		}
	}

	result, err := manager.Open(context.Background(), request)
	if !errors.Is(err, ErrTerminalIdentityArchived) {
		t.Fatalf("concurrent archive did not stop terminal open: result=%+v err=%v", result, err)
	}
	if len(starter.specs) != 0 {
		t.Fatalf("concurrently archived terminal launched a process: %+v", starter.specs)
	}
	if result.Context.State != ContextArchived {
		t.Fatalf("partial result exposed stale pre-archive state: %+v", result.Context)
	}
}

func TestTerminalManagerRevalidatesExplicitCwdAfterIdentityReplacement(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	client := &terminalManagerClient{id: testContextID}
	starter := &terminalManagerStarter{}
	manager := terminalTestManager(root, client, starter)
	requestedCwd := t.TempDir()
	replacementCwd := t.TempDir()
	identity := TerminalIdentity{Kind: TerminalIdentityProject, Project: "LAB-105"}
	request := TerminalOpenRequest{
		Identity: identity, Adapter: TerminalAdapterAlacritty, Cwd: requestedCwd, CwdExplicit: true, Focus: true,
	}
	manager.BeforeWindowTxn = func() {
		if _, err := UpdateRegistry(root, func(registry *Registry) error {
			registry.Contexts = nil
			replacement := testValidContext(testContextID)
			replacement.Launcher.Cwd = replacementCwd
			replacement.Launcher.Terminal.Identity = &identity
			return AddContext(registry, replacement)
		}); err != nil {
			t.Fatalf("replace terminal identity between transactions: %v", err)
		}
	}

	result, err := manager.Open(context.Background(), request)
	if !errors.Is(err, ErrTerminalIdentityConflict) || result.Context.Launcher.Cwd != replacementCwd {
		t.Fatalf("replacement cwd was not revalidated: result=%+v err=%v", result, err)
	}
	if len(starter.specs) != 0 {
		t.Fatalf("replacement with conflicting cwd launched a process: %+v", starter.specs)
	}
}

func TestTerminalAdapterReconfigureRejectsStillMappedArchivedIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	identity := TerminalIdentity{Kind: TerminalIdentityDefault}
	contextValue := testValidContext(testContextID)
	contextValue.State = ContextArchived
	archivedAt := time.Now().UTC()
	contextValue.ArchivedAt = &archivedAt
	contextValue.Launcher.Terminal.Identity = &identity
	if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{contextValue}}); err != nil {
		t.Fatal(err)
	}
	client := &terminalManagerClient{id: testContextID, mapped: true}
	reconfigurer := TerminalAdapterReconfigurer{
		StateRoot: root, Client: client,
		FindProcesses: func(string, ContextID) ([]int, error) { return nil, nil },
	}

	changed, reconfigured, err := reconfigurer.Reconfigure(context.Background(), identity, TerminalAdapterFoot)
	if !errors.Is(err, ErrTerminalAdapterInUse) || reconfigured || changed.ID != contextValue.ID {
		t.Fatalf("mapped archived terminal reconfigured: context=%+v changed=%t err=%v", changed, reconfigured, err)
	}
	var registry Registry
	if loadErr := RegistryStoreFor(root).LoadInto(&registry); loadErr != nil {
		t.Fatal(loadErr)
	}
	if registry.Contexts[0].Launcher.Terminal.Adapter != TerminalAdapterAlacritty {
		t.Fatalf("mapped terminal adapter was mutated: %+v", registry.Contexts[0])
	}
}

func TestTerminalAdapterReconfigureRejectsPendingOldAdapterLaunch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	identity := TerminalIdentity{Kind: TerminalIdentityDefault}
	contextValue := testValidContext(testContextID)
	contextValue.State = ContextArchived
	archivedAt := time.Now().UTC()
	contextValue.ArchivedAt = &archivedAt
	contextValue.Launcher.Terminal.Identity = &identity
	if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{contextValue}}); err != nil {
		t.Fatal(err)
	}
	client := &terminalManagerClient{id: testContextID}
	reconfigurer := TerminalAdapterReconfigurer{
		StateRoot: root, Client: client,
		FindProcesses: func(string, ContextID) ([]int, error) { return []int{4321}, nil },
	}

	_, reconfigured, err := reconfigurer.Reconfigure(context.Background(), identity, TerminalAdapterFoot)
	if !errors.Is(err, ErrTerminalAdapterInUse) || reconfigured {
		t.Fatalf("pending old adapter launch reconfigured: changed=%t err=%v", reconfigured, err)
	}
}

func TestTerminalAdapterReconfigureChangesClosedArchivedIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	identity := TerminalIdentity{Kind: TerminalIdentityDefault}
	contextValue := testValidContext(testContextID)
	contextValue.State = ContextArchived
	archivedAt := time.Now().UTC()
	contextValue.ArchivedAt = &archivedAt
	contextValue.Launcher.Terminal.Identity = &identity
	if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{contextValue}}); err != nil {
		t.Fatal(err)
	}
	client := &terminalManagerClient{id: testContextID}
	reconfigurer := TerminalAdapterReconfigurer{
		StateRoot: root, Client: client,
		FindProcesses: func(string, ContextID) ([]int, error) { return nil, nil },
	}

	changed, reconfigured, err := reconfigurer.Reconfigure(context.Background(), identity, TerminalAdapterFoot)
	if err != nil || !reconfigured || changed.Launcher.Terminal.Adapter != TerminalAdapterFoot {
		t.Fatalf("closed archived terminal was not reconfigured: context=%+v changed=%t err=%v", changed, reconfigured, err)
	}
}

func terminalTestManager(root string, client *terminalManagerClient, starter *terminalManagerStarter) TerminalManager {
	return TerminalManager{
		StateRoot: root,
		ProcRoot:  "/proc",
		Client:    client,
		NewContextID: func() (ContextID, error) {
			return testContextID, nil
		},
		ResolveProgram: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		SessionManager: testTerminalSessionManager{root: "/tmp/herdr-test", configFile: "/tmp/herdr-test/config.toml"},
		FindPending:    func(string, ProcessSpec) ([]int, error) { return nil, nil },
		Starter:        starter,
		Now:            time.Now,
		Sleep:          func(time.Duration) {},
		SettleTimeout:  time.Second,
		StabilityDelay: time.Millisecond,
	}
}

type testTerminalSessionManager struct {
	root       string
	configFile string
	initialize func(context.Context, Context, []string) (TerminalSessionInitialization, error)
}

func (testTerminalSessionManager) Kind() TerminalSessionManagerKind {
	return TerminalSessionManagerHerdr
}

func (manager testTerminalSessionManager) ValidateContext(contextValue Context) error {
	return ValidateHerdrSessionSocketPaths(manager.root, contextValue.Launcher.Session)
}

func (manager testTerminalSessionManager) BuildProcessSpec(contextValue Context, terminalExecutable string) (ProcessSpec, error) {
	if err := manager.ValidateContext(contextValue); err != nil {
		return ProcessSpec{}, err
	}
	return BuildTerminalProcessSpec(contextValue, terminalExecutable, "/usr/bin/herdr", manager.configFile)
}

func (testTerminalSessionManager) ValidateRoles(roles []string) error {
	if len(roles) != 2 {
		return errors.New("exactly two roles required")
	}
	return nil
}

func (manager testTerminalSessionManager) Initialize(ctx context.Context, contextValue Context, roles []string) (TerminalSessionInitialization, error) {
	if manager.initialize != nil {
		return manager.initialize(ctx, contextValue, roles)
	}
	return TerminalSessionInitialization{Manager: manager.Kind(), Roles: append([]string(nil), roles...), Initialized: true}, nil
}

type failingTerminalSessionManager struct{ err error }

func (failingTerminalSessionManager) Kind() TerminalSessionManagerKind {
	return TerminalSessionManagerHerdr
}
func (manager failingTerminalSessionManager) ValidateContext(Context) error { return manager.err }
func (manager failingTerminalSessionManager) BuildProcessSpec(Context, string) (ProcessSpec, error) {
	return ProcessSpec{}, manager.err
}
func (manager failingTerminalSessionManager) ValidateRoles([]string) error { return manager.err }
func (manager failingTerminalSessionManager) Initialize(context.Context, Context, []string) (TerminalSessionInitialization, error) {
	return TerminalSessionInitialization{}, manager.err
}

type terminalManagerStarter struct {
	mu          sync.Mutex
	specs       []ProcessSpec
	onStart     func()
	onStartSpec func(ProcessSpec)
}

func (starter *terminalManagerStarter) Start(spec ProcessSpec) error {
	starter.mu.Lock()
	starter.specs = append(starter.specs, spec)
	starter.mu.Unlock()
	if starter.onStart != nil {
		starter.onStart()
	}
	if starter.onStartSpec != nil {
		starter.onStartSpec(spec)
	}
	return nil
}

type terminalManagerClient struct {
	mu             sync.Mutex
	id             ContextID
	mapped         bool
	focused        bool
	ambiguousFocus bool
	focusCommands  int
}

type sequencedTerminalManagerClient struct {
	mu            sync.Mutex
	id            ContextID
	mapped        []bool
	containerIDs  []int64
	focused       bool
	focusedStates []bool
	duplicates    []bool
	requests      int
}

func (client *sequencedTerminalManagerClient) RequestContext(ctx context.Context, messageType swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if err := ctx.Err(); err != nil {
		return swayipc.Message{}, err
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	switch messageType {
	case swayipc.GetTree:
		mapped := false
		requestIndex := client.requests
		if len(client.mapped) != 0 {
			index := requestIndex
			if index >= len(client.mapped) {
				index = len(client.mapped) - 1
			}
			mapped = client.mapped[index]
		}
		focused := client.focused
		if len(client.focusedStates) != 0 {
			index := requestIndex
			if index >= len(client.focusedStates) {
				index = len(client.focusedStates) - 1
			}
			focused = client.focusedStates[index]
		}
		duplicate := false
		if len(client.duplicates) != 0 {
			index := requestIndex
			if index >= len(client.duplicates) {
				index = len(client.duplicates) - 1
			}
			duplicate = client.duplicates[index]
		}
		client.requests++
		workspace := &swayipc.TreeNode{ID: 2, Type: "workspace", Name: "98"}
		if mapped {
			appID, _ := client.id.AppID()
			containerID := int64(42)
			if len(client.containerIDs) != 0 {
				index := client.requests - 1
				if index >= len(client.containerIDs) {
					index = len(client.containerIDs) - 1
				}
				containerID = client.containerIDs[index]
			}
			workspace.Nodes = []*swayipc.TreeNode{{ID: containerID, Type: "con", AppID: &appID, Focused: focused}}
			if duplicate {
				workspace.Nodes = append(workspace.Nodes, &swayipc.TreeNode{ID: containerID + 1, Type: "con", AppID: &appID})
			}
		}
		encoded, _ := json.Marshal(&swayipc.TreeNode{ID: 1, Type: "root", Nodes: []*swayipc.TreeNode{workspace}})
		return swayipc.Message{Type: swayipc.GetTree, Payload: encoded}, nil
	case swayipc.RunCommand:
		client.focused = true
		return swayipc.Message{Type: swayipc.RunCommand, Payload: []byte(`[{"success":true}]`)}, nil
	default:
		return swayipc.Message{}, errors.New("unexpected request")
	}
}

func (client *terminalManagerClient) setMapped(focused bool) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.mapped = true
	client.focused = focused
}

func (client *terminalManagerClient) Request(messageType swayipc.MessageType, _ []byte) (swayipc.Message, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	switch messageType {
	case swayipc.GetTree:
		root := &swayipc.TreeNode{ID: 1, Type: "root", Nodes: []*swayipc.TreeNode{{ID: 2, Type: "workspace", Name: "98"}}}
		if client.mapped {
			appID, _ := client.id.AppID()
			root.Nodes[0].Nodes = []*swayipc.TreeNode{{ID: 42, Type: "con", AppID: &appID, Focused: client.focused}}
		}
		payload, _ := json.Marshal(root)
		return swayipc.Message{Type: swayipc.GetTree, Payload: payload}, nil
	case swayipc.RunCommand:
		client.focusCommands++
		client.focused = true
		if client.ambiguousFocus {
			client.ambiguousFocus = false
			return swayipc.Message{}, &swayipc.CommandOutcomeUnknownError{Cause: errors.New("connection lost")}
		}
		return swayipc.Message{Type: swayipc.RunCommand, Payload: []byte(`[{"success":true}]`)}, nil
	default:
		return swayipc.Message{}, errors.New("unexpected request")
	}
}

func (client *terminalManagerClient) RequestContext(ctx context.Context, messageType swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if err := ctx.Err(); err != nil {
		return swayipc.Message{}, err
	}
	return client.Request(messageType, payload)
}

type blockingTerminalManagerClient struct {
	started  chan struct{}
	mu       sync.Mutex
	requests int
}

type multiTerminalManagerClient struct {
	mu      sync.Mutex
	windows map[string]int64
	focused int64
}

func (client *multiTerminalManagerClient) mapProcess(spec ProcessSpec) {
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, argument := range spec.Arguments {
		appID, found := strings.CutPrefix(argument, "--class=")
		if !found {
			appID, found = strings.CutPrefix(argument, "--app-id=")
		}
		if !found {
			continue
		}
		containerID := int64(len(client.windows) + 40)
		client.windows[appID] = containerID
		client.focused = containerID
		return
	}
}

func (client *multiTerminalManagerClient) RequestContext(ctx context.Context, messageType swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if err := ctx.Err(); err != nil {
		return swayipc.Message{}, err
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	switch messageType {
	case swayipc.GetTree:
		workspace := &swayipc.TreeNode{ID: 2, Type: "workspace", Name: "98"}
		for appID, containerID := range client.windows {
			value := appID
			workspace.Nodes = append(workspace.Nodes, &swayipc.TreeNode{
				ID: containerID, Type: "con", AppID: &value, Focused: containerID == client.focused,
			})
		}
		encoded, _ := json.Marshal(&swayipc.TreeNode{ID: 1, Type: "root", Nodes: []*swayipc.TreeNode{workspace}})
		return swayipc.Message{Type: swayipc.GetTree, Payload: encoded}, nil
	case swayipc.RunCommand:
		var containerID int64
		if _, err := fmt.Sscanf(string(payload), "[con_id=%d] focus", &containerID); err != nil {
			return swayipc.Message{}, err
		}
		client.focused = containerID
		return swayipc.Message{Type: swayipc.RunCommand, Payload: []byte(`[{"success":true}]`)}, nil
	default:
		return swayipc.Message{}, errors.New("unexpected request")
	}
}

func (client *blockingTerminalManagerClient) RequestContext(ctx context.Context, messageType swayipc.MessageType, _ []byte) (swayipc.Message, error) {
	client.mu.Lock()
	client.requests++
	request := client.requests
	client.mu.Unlock()
	if request == 1 {
		select {
		case <-client.started:
		default:
			close(client.started)
		}
		<-ctx.Done()
		return swayipc.Message{}, ctx.Err()
	}
	if messageType != swayipc.GetTree {
		return swayipc.Message{}, errors.New("unexpected request")
	}
	encoded, _ := json.Marshal(&swayipc.TreeNode{ID: 1, Type: "root", Nodes: []*swayipc.TreeNode{{ID: 2, Type: "workspace", Name: "98"}}})
	return swayipc.Message{Type: swayipc.GetTree, Payload: encoded}, nil
}
