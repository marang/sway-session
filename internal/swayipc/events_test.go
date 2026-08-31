package swayipc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEndpointGoneRechecksCurrentPath(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "recreated.sock")
	if err := os.WriteFile(socket, []byte("replacement"), 0o600); err != nil {
		t.Fatalf("create replacement endpoint: %v", err)
	}
	if endpointGone(socket) {
		t.Fatal("a recreated endpoint was mistaken for a terminated compositor")
	}
	if err := os.Remove(socket); err != nil {
		t.Fatalf("remove endpoint: %v", err)
	}
	if !endpointGone(socket) {
		t.Fatal("a removed fixed endpoint was not recognized")
	}
}
