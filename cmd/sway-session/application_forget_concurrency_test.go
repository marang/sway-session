package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
	"golang.org/x/sys/unix"
)

func TestAppForgetCancellationSerializesCompensationWithRebindFocused(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	forgetCtx, cancelForget := context.WithCancel(ctx)
	defer cancelForget()
	deps := testDependencies(t)
	catalog := testDesktopCatalog(t, map[string]string{
		"org.example.New.desktop": "[Desktop Entry]\nType=Application\nName=New App\nExec=/usr/bin/flatpak run org.example.New\nX-Flatpak=org.example.New\n",
	}, true)
	deps.desktopCatalog = func() (sessionstate.DesktopCatalog, error) { return catalog, nil }
	registered := sessionstate.Context{
		ID: testContextID, Label: "Old App", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.Old", FlatpakInstallation: sessionstate.FlatpakUser},
		App: &sessionstate.Application{
			Identity:    sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.Old", SandboxAppID: "org.example.Old"},
			DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestorePinned,
		},
	}
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionstate.UpdateRegistryContext(ctx, root, func(registry *sessionstate.Registry) error {
		return sessionstate.AddContext(registry, registered)
	}); err != nil {
		t.Fatal(err)
	}
	// Initialize the actual process-shared lock directory before probing it.
	if err := sessionstate.WithTerminalLifecycleLockContext(ctx, root, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	mark, err := registered.ID.Mark()
	if err != nil {
		t.Fatal(err)
	}
	oldAppID, newAppID := "org.example.Old", "org.example.New"
	windowA := &swayipc.TreeNode{ID: 51, Type: "con", AppID: &oldAppID, SandboxAppID: &oldAppID, Marks: []string{mark}}
	windowB := &swayipc.TreeNode{ID: 52, Type: "con", Focused: true, AppID: &newAppID, SandboxAppID: &newAppID}
	tree := applicationCommandTree(windowA)
	tree.Nodes[0].Nodes[0].Name = "98: forget regression"
	tree.Nodes[0].Nodes[0].Nodes = append(tree.Nodes[0].Nodes[0].Nodes, windowB)
	compositor := &appCommandClient{tree: tree}
	var compositorMu sync.Mutex

	compensating := make(chan struct{})
	releaseCompensation := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCompensation) }) }
	forgetReturned := make(chan struct{})
	reserved := make(chan sessionstate.LifecycleOperation, 1)
	var workers sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		release()
		done := make(chan struct{})
		go func() { workers.Wait(); close(done) }()
		cleanupCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := forgetConcurrencyWait(cleanupCtx, done); err != nil {
			t.Errorf("CLI workers did not stop before fixture cleanup: %v", err)
		}
	})
	forgetClient := &forgetConcurrencyClient{client: compositor, mu: &compositorMu}
	forgetClient.beforeRequest = func(requestCtx context.Context, kind swayipc.MessageType, payload []byte) error {
		if kind == swayipc.RunCommand && string(payload) == "[con_id=51] mark --add "+mark {
			close(compensating)
			return forgetConcurrencyWait(requestCtx, releaseCompensation)
		}
		return nil
	}
	forgetClient.afterRequest = func(kind swayipc.MessageType, payload []byte) {
		if kind == swayipc.RunCommand && string(payload) == "[con_id=51] unmark "+mark {
			// Sway accepted the unmark, but cancellation prevents the registry
			// commit. Compensation must use its own uncancelled context.
			cancelForget()
		}
	}
	forgetDeps := deps
	forgetDeps.newSwayClient = func(string) swayRequester { return forgetClient }

	rebindClient := &forgetConcurrencyClient{client: compositor, mu: &compositorMu}
	var reservationOnce sync.Once
	rebindClient.beforeIdentity = func(requestCtx context.Context) error {
		operations, _, err := sessionstate.ListLifecycleOperationsContext(requestCtx, root, "", 10)
		if err != nil {
			return err
		}
		if len(operations) == 1 {
			// This callback runs after intent creation and before reconciliation
			// observes the tree. No production test hook or fake state lock.
			reservationOnce.Do(func() { reserved <- operations[0] })
			return forgetConcurrencyWait(requestCtx, forgetReturned)
		}
		return nil
	}
	rebindDeps := deps
	rebindDeps.newSwayClient = func(string) swayRequester { return rebindClient }
	rebindEntered := make(chan struct{})
	var enteredOnce sync.Once
	rebindDeps.stateRoot = func() (string, error) {
		enteredOnce.Do(func() { close(rebindEntered) })
		return root, nil
	}
	type cliResult struct {
		code   int
		stderr string
	}
	runCLI := func(commandCtx context.Context, command string, commandDeps dependencies) cliResult {
		var stdout, stderr bytes.Buffer
		code := runWithContext(commandCtx, []string{"--json", "app", command, "--yes", "--socket", "/fake/sway.sock", string(registered.ID)}, strings.NewReader(""), &stdout, &stderr, commandDeps)
		return cliResult{code: code, stderr: stderr.String()}
	}
	forgot := make(chan cliResult, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		result := runCLI(forgetCtx, "forget", forgetDeps)
		close(forgetReturned)
		forgot <- result
	}()
	if err := forgetConcurrencyWait(ctx, compensating); err != nil {
		t.Fatalf("forget never reached mark compensation: %v", err)
	}

	// Inspect the real flock while forget is paused. Never require rebind to
	// enter a lock held by a correct implementation: that would deadlock the
	// regression precisely when the bug is fixed. On the broken implementation
	// force the damaging interleaving instead of relying on scheduler timing.
	serialized := forgetConcurrencyLifecycleLocked(t, root)
	rebound := make(chan cliResult, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		rebound <- runCLI(ctx, "rebind-focused", rebindDeps)
	}()
	if err := forgetConcurrencyWait(ctx, rebindEntered); err != nil {
		t.Fatal(err)
	}
	if !serialized {
		select {
		case operation := <-reserved:
			if operation.Kind != sessionstate.LifecycleRebind || len(operation.Targets) != 1 || operation.Targets[0].ContainerID != windowB.ID {
				t.Fatalf("expected rebind to reserve only unmarked replacement B: %+v", operation)
			}
		case result := <-rebound:
			t.Fatalf("rebind returned before creating its reservation: %+v", result)
		case <-ctx.Done():
			t.Fatalf("rebind never reserved replacement B: %v", ctx.Err())
		}
	}
	release()
	select {
	case result := <-forgot:
		if result.code == exitSuccess || !strings.Contains(result.stderr, "context canceled") {
			t.Errorf("cancelled forget must report failure: %+v", result)
		}
	case <-ctx.Done():
		t.Fatalf("forget did not finish compensation: %v", ctx.Err())
	}
	select {
	case result := <-rebound:
		if result.code != exitSuccess {
			t.Errorf("rebind-focused failed after cancelled forget compensation: code=%d stderr=%s", result.code, result.stderr)
		}
	case <-ctx.Done():
		t.Fatalf("rebind did not finish after compensation: %v", ctx.Err())
	}
	registry := loadTestRegistry(t, deps)
	if len(registry.Contexts) != 1 || registry.Contexts[0].ID != registered.ID || registry.Contexts[0].Launcher.FlatpakID != newAppID || registry.Contexts[0].App.RestorePolicy != sessionstate.ApplicationRestorePinned {
		t.Errorf("rebind must preserve the context and policy with replacement identity: %+v", registry.Contexts)
	}
	compositorMu.Lock()
	oldMarked, newMarked := containsString(windowA.Marks, mark), containsString(windowB.Marks, mark)
	compositorMu.Unlock()
	if oldMarked || !newMarked {
		t.Errorf("stable mark must belong only to replacement B: A=%v B=%v", oldMarked, newMarked)
	}
	operations, _, err := sessionstate.ListLifecycleOperationsContext(ctx, root, "", 10)
	if err != nil || len(operations) != 0 {
		t.Errorf("successful rebind must retire all lifecycle reservations: operations=%+v err=%v", operations, err)
	}
}

func forgetConcurrencyWait(ctx context.Context, ready <-chan struct{}) error {
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func forgetConcurrencyLifecycleLocked(t *testing.T, root string) bool {
	t.Helper()
	directory, err := os.Open(filepath.Join(root, "terminal-lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	err = unix.Flock(int(directory.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(directory.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	return false
}

type forgetConcurrencyClient struct {
	client         *appCommandClient
	mu             *sync.Mutex
	beforeRequest  func(context.Context, swayipc.MessageType, []byte) error
	afterRequest   func(swayipc.MessageType, []byte)
	beforeIdentity func(context.Context) error
}

func (client *forgetConcurrencyClient) Request(kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	return client.RequestContext(context.Background(), kind, payload)
}

func (client *forgetConcurrencyClient) RequestContext(ctx context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if err := ctx.Err(); err != nil {
		return swayipc.Message{}, err
	}
	if client.beforeRequest != nil {
		if err := client.beforeRequest(ctx, kind, payload); err != nil {
			return swayipc.Message{}, err
		}
	}
	client.mu.Lock()
	message, err := client.client.Request(kind, payload)
	client.mu.Unlock()
	if err == nil && client.afterRequest != nil {
		client.afterRequest(kind, payload)
	}
	return message, err
}

func (client *forgetConcurrencyClient) LifecycleCompositorID(ctx context.Context) (string, error) {
	if client.beforeIdentity != nil {
		if err := client.beforeIdentity(ctx); err != nil {
			return "", fmt.Errorf("observe reserved rebind: %w", err)
		}
	}
	return "test-compositor", nil
}

func (*forgetConcurrencyClient) Close() {}
