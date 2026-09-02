package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marang/sway-title-animator/internal/statefile"
)

type operationBarrierReader struct {
	mu      sync.Mutex
	arrived int
	release chan struct{}
	once    sync.Once
}

func (reader *operationBarrierReader) Read(data []byte) (int, error) {
	reader.mu.Lock()
	reader.arrived++
	value := byte(reader.arrived)
	if reader.arrived == 2 {
		reader.once.Do(func() { close(reader.release) })
	}
	reader.mu.Unlock()
	select {
	case <-reader.release:
	case <-time.After(200 * time.Millisecond):
	}
	for index := range data {
		data[index] = value
	}
	return len(data), nil
}

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

func TestApplicationOperationContextStopsWaitingForStoreLock(t *testing.T) {
	store := ApplicationOperationStore{RuntimeRoot: t.TempDir(), Random: bytes.NewReader(bytes.Repeat([]byte{0x7a}, 16))}
	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- statefile.WithPrivateDirectoryLock(store.directory(), func(*statefile.LockedPrivateDirectory) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	defer func() {
		close(release)
		if err := <-lockDone; err != nil {
			t.Errorf("release operation-store lock: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := store.CreateContext(ctx, ApplicationOperation{
		Kind:  OperationReapprove,
		Items: []ApplicationOperationItem{{ContextID: testContextID, ContextRevision: strings.Repeat("a", 64), DesktopID: "safe.desktop"}},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("locked operation store returned %v, want context deadline", err)
	}
}

func TestApplicationOperationTokenExpiresAndRejectsArgumentInjection(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	store := ApplicationOperationStore{RuntimeRoot: t.TempDir(), Now: func() time.Time { return now }, Random: bytes.NewReader(bytes.Repeat([]byte{1}, 16))}
	operation := ApplicationOperation{Kind: OperationReapprove, ExpiresAt: now.Add(time.Second), Items: []ApplicationOperationItem{{ContextID: testContextID, ContextRevision: strings.Repeat("a", 64), DesktopID: "safe.desktop"}}}
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
	token, err := store.Create(ApplicationOperation{Kind: OperationReapprove, Items: []ApplicationOperationItem{{ContextID: testContextID, ContextRevision: strings.Repeat("a", 64), DesktopID: "safe.desktop"}}})
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

func TestApplicationOperationConcurrentCreateCannotExceedStoreLimit(t *testing.T) {
	now := time.Now().UTC()
	store := ApplicationOperationStore{RuntimeRoot: t.TempDir(), Now: func() time.Time { return now }}
	operation := ApplicationOperation{
		Version: ApplicationOperationVersion, Kind: OperationReapprove, ExpiresAt: now.Add(time.Minute),
		Items: []ApplicationOperationItem{{ContextID: testContextID, ContextRevision: strings.Repeat("a", 64), DesktopID: "safe.desktop"}},
	}
	data, err := json.Marshal(operation)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxStoredOperations-1; index++ {
		name := fmt.Sprintf("%032x.json", index+16)
		if err := statefile.CreatePrivateFile(store.directory(), name, data); err != nil {
			t.Fatalf("seed operation %d: %v", index, err)
		}
	}
	barrier := &operationBarrierReader{release: make(chan struct{})}
	store.Random = barrier
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := store.Create(operation)
			results <- err
		}()
	}
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent creates produced %d winners, want one", successes)
	}
	names, err := statefile.ListPrivateFiles(store.directory(), maxStoredOperations)
	if err != nil || len(names) != maxStoredOperations {
		t.Fatalf("store limit was exceeded: entries=%d err=%v", len(names), err)
	}
}

func TestApplicationOperationCreateRecoversExpiredLegacyOverflow(t *testing.T) {
	now := time.Now().UTC()
	store := ApplicationOperationStore{
		RuntimeRoot: t.TempDir(),
		Now:         func() time.Time { return now },
		Random:      bytes.NewReader(bytes.Repeat([]byte{0xfe}, 16)),
	}
	expired := ApplicationOperation{
		Version: ApplicationOperationVersion, Kind: OperationReapprove, ExpiresAt: now.Add(-time.Second),
		Items: []ApplicationOperationItem{{ContextID: testContextID, ContextRevision: strings.Repeat("a", 64), DesktopID: "safe.desktop"}},
	}
	data, err := json.Marshal(expired)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index <= maxStoredOperations; index++ {
		name := fmt.Sprintf("%032x.json", index+16)
		if err := statefile.CreatePrivateFile(store.directory(), name, data); err != nil {
			t.Fatalf("seed expired operation %d: %v", index, err)
		}
	}

	operation := ApplicationOperation{
		Kind:  OperationReapprove,
		Items: []ApplicationOperationItem{{ContextID: testContextID, ContextRevision: strings.Repeat("b", 64), DesktopID: "safe.desktop"}},
	}
	if _, err := store.Create(operation); err != nil {
		t.Fatalf("create after legacy overflow: %v", err)
	}
	names, err := statefile.ListPrivateFiles(store.directory(), maxStoredOperations)
	if err != nil || len(names) != 1 {
		t.Fatalf("legacy overflow was not pruned: entries=%d err=%v", len(names), err)
	}
}

func TestApplicationOperationStoreListsOnlyActiveOperationsWithoutConsumingThem(t *testing.T) {
	now := time.Unix(4000, 0).UTC()
	store := ApplicationOperationStore{
		RuntimeRoot: t.TempDir(),
		Now:         func() time.Time { return now },
		Random:      bytes.NewReader(append(bytes.Repeat([]byte{3}, 16), bytes.Repeat([]byte{4}, 16)...)),
	}
	operations := []ApplicationOperation{
		{Kind: OperationReapprove, Items: []ApplicationOperationItem{{ContextID: testContextID, ContextRevision: strings.Repeat("a", 64), DesktopID: "safe.desktop"}}},
		{Kind: OperationReapprove, Items: []ApplicationOperationItem{{ContextID: "22222222-2222-4222-8222-222222222222", ContextRevision: strings.Repeat("b", 64), DesktopID: "other.desktop"}}},
	}
	for _, operation := range operations {
		if _, err := store.Create(operation); err != nil {
			t.Fatalf("create operation: %v", err)
		}
	}

	active, err := store.Active()
	if err != nil {
		t.Fatalf("list active operations: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("active operations = %+v", active)
	}

	now = now.Add(3 * time.Minute)
	active, err = store.Active()
	if err != nil {
		t.Fatalf("prune expired operations: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expired operations remained active: %+v", active)
	}
}

func TestApplicationOperationStoreDiscardRemovesUnreachableApproval(t *testing.T) {
	now := time.Unix(4500, 0).UTC()
	store := ApplicationOperationStore{
		RuntimeRoot: t.TempDir(),
		Now:         func() time.Time { return now },
		Random:      bytes.NewReader(bytes.Repeat([]byte{5}, 16)),
	}
	token, err := store.Create(ApplicationOperation{Kind: OperationRegister, Items: []ApplicationOperationItem{{
		ContextID: testContextID,
		Window: &WindowApplication{ContainerID: 42, Workspace: "98: apps", Identity: ApplicationIdentity{
			Protocol: WindowWayland, WaylandAppID: "org.example.App",
		}},
		DesktopID: "org.example.App.desktop",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Discard(token); err != nil {
		t.Fatalf("discard operation: %v", err)
	}
	if err := store.Discard(token); err != nil {
		t.Fatalf("idempotent discard operation: %v", err)
	}
	active, err := store.Active()
	if err != nil || len(active) != 0 {
		t.Fatalf("discarded approval remains active: %+v err=%v", active, err)
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

func TestApplicationOperationRequiresContextRevisionOnlyForContextMutations(t *testing.T) {
	now := time.Now().UTC()
	operation := ApplicationOperation{
		Version:   ApplicationOperationVersion,
		Kind:      OperationReapprove,
		ExpiresAt: now.Add(time.Minute),
		Items: []ApplicationOperationItem{{
			ContextID: testContextID,
			DesktopID: "safe.desktop",
		}},
	}
	if err := operation.Validate(now); err == nil {
		t.Fatal("reapproval without a context revision was accepted")
	}
	operation.Items[0].ContextRevision = strings.Repeat("a", 64)
	if err := operation.Validate(now); err != nil {
		t.Fatalf("valid context revision was rejected: %v", err)
	}
	operation.Items[0].ContextRevision = strings.Repeat("A", 64)
	if err := operation.Validate(now); err == nil {
		t.Fatal("non-canonical context revision was accepted")
	}
}
