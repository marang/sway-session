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

func TestSessionRestoreIntentEventsAreClassifiedForLosslessDelivery(t *testing.T) {
	for _, event := range []Event{
		{Type: EventBinding, Change: "run"},
		{Type: EventWindow, Change: "focus"},
		{Type: EventWindow, Change: "move"},
		{Type: EventWindow, Change: "close"},
		{Type: EventWorkspace, Change: "focus"},
	} {
		if !eventSupersedesSessionRestore(event) {
			t.Fatalf("user-intent event may be coalesced: %+v", event)
		}
	}
	for _, event := range []Event{
		{Type: EventWindow, Change: "title"},
		{Type: EventWorkspace, Change: "init"},
	} {
		if eventSupersedesSessionRestore(event) {
			t.Fatalf("ordinary tree event was made lossless: %+v", event)
		}
	}
}

func TestLosslessSessionIntentDeliveryHonorsShutdownInsteadOfDropping(t *testing.T) {
	events := make(chan Event, 1)
	events <- Event{Type: EventWindow, Change: "title"}
	done := make(chan struct{})
	close(done)

	if deliverEvent(events, done, Event{Type: EventBinding, Change: "run"}, true) {
		t.Fatal("full session queue silently dropped a user-intent event")
	}
	if !deliverEvent(events, done, Event{Type: EventWindow, Change: "title"}, true) {
		t.Fatal("ordinary coalesced event unexpectedly blocked shutdown")
	}
}
