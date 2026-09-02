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

	"github.com/marang/sway-title-animator/internal/statefile"
	"github.com/marang/sway-title-animator/internal/swayipc"
)

func TestTerminalManagerCreatesAttachesThenReusesFocusedDefault(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	client := &terminalManagerClient{id: testContextID}
	starter := &terminalManagerStarter{onStart: func() { client.setMapped(true) }}
	manager := terminalTestManager(root, client, starter)
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
	manager.HerdrPaths = func() (HerdrPaths, error) {
		return HerdrPaths{}, errors.New("visible terminal must not resolve Herdr paths")
	}
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
}

func TestTerminalManagerReconcilesAmbiguousExactFocusWithoutReplay(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	contextValue := testValidContext(testContextID)
	identity := TerminalIdentity{Kind: TerminalIdentityDefault}
	contextValue.Launcher.Terminal.Identity = &identity
	if err := RegistryFile(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{contextValue}}); err != nil {
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
	if err := RegistryFile(root).LoadInto(&registry); err != nil || len(registry.Contexts) != 1 {
		t.Fatalf("concurrent open registry=%+v err=%v", registry, err)
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
	if err := RegistryFile(root).LoadInto(&registry); err != nil || len(registry.Contexts) != 2 ||
		registry.Contexts[0].Launcher.Session == registry.Contexts[1].Launcher.Session {
		t.Fatalf("concurrent --new registry=%+v err=%v", registry, err)
	}
}

func TestTerminalManagerNewRejectsOverlongHerdrSocketBeforePersistOrStart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "sway-session")
	starter := &terminalManagerStarter{}
	manager := terminalTestManager(root, &terminalManagerClient{id: testContextID}, starter)
	longHerdrRoot := "/" + strings.Repeat("a", 39)
	manager.HerdrPaths = func() (HerdrPaths, error) {
		return HerdrPaths{Root: longHerdrRoot, ConfigFile: filepath.Join(longHerdrRoot, "config.toml")}, nil
	}

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
	if err := RegistryFile(root).LoadInto(&registry); !errors.Is(err, os.ErrNotExist) {
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
	manager := terminalTestManager(filepath.Join(t.TempDir(), "state", "sway-session"), &terminalManagerClient{id: testContextID}, &terminalManagerStarter{})
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
}

func TestTerminalManagerRejectsControlCharacterCwdBeforeRegistryUpdate(t *testing.T) {
	manager := terminalTestManager(filepath.Join(t.TempDir(), "state", "sway-session"), &terminalManagerClient{id: testContextID}, &terminalManagerStarter{})
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
	if err := RegistryFile(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{contextValue}}); err != nil {
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
	if loadErr := RegistryFile(root).LoadInto(&registry); loadErr != nil {
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
	if err := RegistryFile(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{contextValue}}); err != nil {
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
	if err := RegistryFile(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{contextValue}}); err != nil {
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
		HerdrPaths: func() (HerdrPaths, error) {
			return HerdrPaths{Root: "/tmp/herdr-test", ConfigFile: "/tmp/herdr-test/config.toml"}, nil
		},
		ValidateHistory: func(HerdrPaths) error { return nil },
		FindPending:     func(string, ProcessSpec) ([]int, error) { return nil, nil },
		Starter:         starter,
		Now:             time.Now,
		Sleep:           func(time.Duration) {},
		SettleTimeout:   time.Second,
	}
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
	started chan struct{}
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

func (client *blockingTerminalManagerClient) RequestContext(ctx context.Context, _ swayipc.MessageType, _ []byte) (swayipc.Message, error) {
	select {
	case <-client.started:
	default:
		close(client.started)
	}
	<-ctx.Done()
	return swayipc.Message{}, ctx.Err()
}
