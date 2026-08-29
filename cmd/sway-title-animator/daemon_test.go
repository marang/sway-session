package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marang/sway-title-animator/internal/swayipc"
)

func TestSubscribeTreatsMissingFixedEndpointAsShutdown(t *testing.T) {
	events := make(chan swayipc.Event, 1)
	done := make(chan struct{})
	defer close(done)
	socket := filepath.Join(t.TempDir(), "missing.sock")
	go subscribe(socket, events, done)

	select {
	case event := <-events:
		if event.Type != swayipc.EventShutdown || event.Change != "endpoint-gone" {
			t.Fatalf("unexpected terminal event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing fixed Sway endpoint did not stop the subscriber")
	}
}

func TestSwayEndpointGoneRechecksCurrentPath(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "recreated.sock")
	if err := os.WriteFile(socket, []byte("replacement"), 0o600); err != nil {
		t.Fatalf("create replacement endpoint: %v", err)
	}
	if swayEndpointGone(socket) {
		t.Fatal("a recreated endpoint was mistaken for a terminated compositor")
	}
	if err := os.Remove(socket); err != nil {
		t.Fatalf("remove endpoint: %v", err)
	}
	if !swayEndpointGone(socket) {
		t.Fatal("a removed fixed endpoint was not recognized")
	}
}
