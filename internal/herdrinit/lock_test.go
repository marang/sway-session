package herdrinit

import (
	"errors"
	"path/filepath"
	"testing"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
)

func TestAcquireContextLockSerializesOneContext(t *testing.T) {
	id := sessionstate.ContextID("8f33d6d0-7c54-4da1-9e38-2bd290ef85ca")
	root := t.TempDir()
	first, err := AcquireContextLock(root, id)
	if err != nil {
		t.Fatalf("AcquireContextLock returned error: %v", err)
	}
	defer first.Close()

	if _, err := AcquireContextLock(root, id); !errors.Is(err, ErrInitializationRunning) {
		t.Fatalf("second lock returned %v, want ErrInitializationRunning", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first lock: %v", err)
	}
	second, err := AcquireContextLock(root, id)
	if err != nil {
		t.Fatalf("reacquire lock: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close second lock: %v", err)
	}
}

func TestAcquireContextLockSeparatesContexts(t *testing.T) {
	root := t.TempDir()
	first, err := AcquireContextLock(root, sessionstate.ContextID("8f33d6d0-7c54-4da1-9e38-2bd290ef85ca"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := AcquireContextLock(root, sessionstate.ContextID("7b7b62c0-926d-4f8d-a612-2bd290ef85ca"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
}

func TestAcquireContextLockRejectsUnsafeRoot(t *testing.T) {
	id := sessionstate.ContextID("8f33d6d0-7c54-4da1-9e38-2bd290ef85ca")
	if _, err := AcquireContextLock(filepath.Join("relative", "runtime"), id); err == nil {
		t.Fatal("AcquireContextLock accepted a relative runtime root")
	}
}
