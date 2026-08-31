package session

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestApplicationOperationTokenIsOwnerOnlyOneTimeAndBounded(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	store := ApplicationOperationStore{
		RuntimeRoot: t.TempDir(),
		Now:         func() time.Time { return now },
		Random:      bytes.NewReader(bytes.Repeat([]byte{0x4a}, 16)),
	}
	operation := ApplicationOperation{Kind: OperationRegister, Items: []ApplicationOperationItem{{
		ContextID: testContextID,
		Window: &WindowApplication{ContainerID: 42, Workspace: "2:web", Identity: ApplicationIdentity{
			Protocol: WindowWayland, WaylandAppID: "org.example.App",
		}},
		DesktopID: "org.example.App.desktop",
	}}}
	token, err := store.Create(operation)
	if err != nil {
		t.Fatal(err)
	}
	if token != "4a4a4a4a4a4a4a4a4a4a4a4a4a4a4a4a" {
		t.Fatalf("unexpected token %q", token)
	}
	path := filepath.Join(store.directory(), token+".json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token is not owner-only: info=%v err=%v", info, err)
	}
	consumed, err := store.Consume(token)
	if err != nil || consumed.Kind != OperationRegister || consumed.Items[0].Window.ContainerID != 42 {
		t.Fatalf("consume failed: operation=%+v err=%v", consumed, err)
	}
	if _, err := store.Consume(token); err == nil {
		t.Fatal("operation token was replayable")
	}
}

func TestApplicationOperationTokenExpiresAndRejectsArgumentInjection(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	store := ApplicationOperationStore{RuntimeRoot: t.TempDir(), Now: func() time.Time { return now }, Random: bytes.NewReader(bytes.Repeat([]byte{1}, 16))}
	operation := ApplicationOperation{Kind: OperationReapprove, ExpiresAt: now.Add(time.Second), Items: []ApplicationOperationItem{{ContextID: testContextID, DesktopID: "safe.desktop"}}}
	token, err := store.Create(operation)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if _, err := store.Consume(token); err == nil {
		t.Fatalf("expired token was accepted: %v", err)
	}
	for _, token := range []string{"../../safe", "ABCDEF0123456789ABCDEF0123456789", "a;touch-nope", ""} {
		if _, err := store.Consume(token); err == nil {
			t.Fatalf("unsafe token %q was accepted", token)
		}
	}
}

func TestApplicationOperationConcurrentConsumeHasOneWinner(t *testing.T) {
	now := time.Now().UTC()
	store := ApplicationOperationStore{RuntimeRoot: t.TempDir(), Now: func() time.Time { return now }, Random: bytes.NewReader(bytes.Repeat([]byte{2}, 16))}
	token, err := store.Create(ApplicationOperation{Kind: OperationReapprove, Items: []ApplicationOperationItem{{ContextID: testContextID, DesktopID: "safe.desktop"}}})
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	wait.Add(2)
	results := make(chan error, 2)
	for range 2 {
		go func() {
			defer wait.Done()
			_, err := store.Consume(token)
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("expected exactly one consume winner, got %d", success)
	}
}

func TestApplicationOperationRejectsUnsafeWorkspaceAndMarks(t *testing.T) {
	now := time.Now().UTC()
	base := ApplicationOperation{Version: ApplicationOperationVersion, Kind: OperationRegister, ExpiresAt: now.Add(time.Minute), Items: []ApplicationOperationItem{{
		ContextID: testContextID, DesktopID: "safe.desktop", Window: &WindowApplication{ContainerID: 42, Workspace: "bad\nworkspace", Identity: ApplicationIdentity{Protocol: WindowWayland, WaylandAppID: "safe"}},
	}}}
	if err := base.Validate(now); err == nil {
		t.Fatal("workspace control character was accepted")
	}
	base.Items[0].Window.Workspace = "2:web"
	base.Items[0].Window.ContextMarks = []ContextID{"not-a-uuid"}
	if err := base.Validate(now); err == nil {
		t.Fatal("invalid persistent mark was accepted")
	}
	base.Items[0].Window.ContextMarks = []ContextID{testContextID}
	if err := base.Validate(now); err == nil {
		t.Fatal("registration operation for an already marked window was accepted")
	}
}
