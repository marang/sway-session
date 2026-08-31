package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionDaemonLockExcludesSecondRuntime(t *testing.T) {
	runtimeRoot := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	first, err := acquireSessionDaemonLock()
	if err != nil {
		t.Fatalf("acquire first daemon lock: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	if _, err := acquireSessionDaemonLock(); !errors.Is(err, errSessionDaemonRunning) {
		t.Fatalf("expected second daemon to be rejected, got %v", err)
	}
	info, err := os.Stat(filepath.Join(runtimeRoot, "sway-session", "daemon.lock"))
	if err != nil {
		t.Fatalf("inspect daemon lock: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected daemon lock mode: %v", info.Mode())
	}
}

func TestSessionDaemonLockRejectsUnsafeRuntimeRoot(t *testing.T) {
	for _, root := range []string{"", "relative", "/tmp/../tmp"} {
		t.Run(root, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", root)
			if _, err := acquireSessionDaemonLock(); err == nil {
				t.Fatalf("unsafe runtime root %q was accepted", root)
			}
		})
	}
}
