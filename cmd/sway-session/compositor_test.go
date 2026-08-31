package main

import (
	"net"
	"path/filepath"
	"testing"
)

func TestCompositorIdentityIsStableForSocketLifetimeAndChangesAfterReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sway.sock")
	first := listenUnixSocket(t, path)
	firstID, err := compositorIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := compositorIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if firstID != again {
		t.Fatalf("same compositor socket changed identity: %q != %q", firstID, again)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := listenUnixSocket(t, path)
	t.Cleanup(func() { _ = second.Close() })
	secondID, err := compositorIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if firstID == secondID {
		t.Fatalf("replacement compositor socket reused identity %q", firstID)
	}
}

func listenUnixSocket(t *testing.T, path string) *net.UnixListener {
	t.Helper()
	address, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	return listener
}
