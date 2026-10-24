package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDesktopPathAncestorsTrustOnlyRootAndApprovedOwner(t *testing.T) {
	const approvedOwner uint32 = 1000
	for name, test := range map[string]struct {
		owner uint32
		want  bool
	}{
		"root":           {owner: 0, want: true},
		"approved owner": {owner: approvedOwner, want: true},
		"foreign owner":  {owner: 2000},
	} {
		t.Run(name, func(t *testing.T) {
			if got := trustedDesktopPathOwner(test.owner, approvedOwner); got != test.want {
				t.Fatalf("trust ancestor owned by uid %d = %t, want %t", test.owner, got, test.want)
			}
		})
	}
	if trustedDesktopPathOwner(approvedOwner, 0) {
		t.Fatal("a user-owned ancestor was trusted for a root-owned file")
	}
}

func TestOpenTrustedRegularChecksEachAncestorOwner(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "sway-session-desktop-owner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	path := filepath.Join(root, "launcher.desktop")
	if err := os.WriteFile(path, []byte("launcher trust fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner := uint32(os.Geteuid())
	file, err := openTrustedRegular(path, owner, false)
	if err != nil {
		t.Fatalf("open file below root-owned /tmp and current-user ancestor: %v", err)
	}
	_ = file.Close()
	if owner == 0 {
		t.Skip("a non-root process is needed to distinguish the file owner from root")
	}

	// A different approved owner makes this test directory an untrusted
	// ancestor without changing filesystem ownership or executing a launcher.
	file, err = openTrustedRegular(path, owner+1, false)
	if file != nil {
		_ = file.Close()
		t.Fatal("file was opened below an ancestor outside the approved owner policy")
	}
	if err == nil || !strings.Contains(err.Error(), "path component "+root+" must be owned") {
		t.Fatalf("expected rejection at the untrusted ancestor, got %v", err)
	}

	file, err = openTrustedRegular(path, owner, true)
	if file != nil {
		_ = file.Close()
		t.Fatal("root-only traversal accepted a user-owned ancestor")
	}
	if err == nil || !strings.Contains(err.Error(), "path component "+root+" is not root-owned") {
		t.Fatalf("expected root-only rejection at the user ancestor, got %v", err)
	}
}
