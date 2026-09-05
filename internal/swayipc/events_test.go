package swayipc

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSessionEventStreamReportsEverySuccessfulSubscription(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "sway.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("unix sockets are not permitted in this sandbox: %v", err)
		}
		t.Fatalf("listen on fake Sway socket: %v", err)
	}
	defer listener.Close()

	serverDone := make(chan error, 1)
	go func() {
		for attempt := 0; attempt < 2; attempt++ {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				serverDone <- acceptErr
				return
			}
			message, readErr := readMessage(connection)
			if readErr != nil {
				_ = connection.Close()
				serverDone <- readErr
				return
			}
			if message.Type != Subscribe {
				_ = connection.Close()
				serverDone <- errors.New("event stream did not subscribe")
				return
			}
			if string(message.Payload) != `["window","workspace","binding","shutdown","tick"]` {
				_ = connection.Close()
				serverDone <- errors.New("session event stream omitted its move-attribution tick")
				return
			}
			if writeErr := writeMessage(connection, Subscribe, []byte(`{"success":true}`)); writeErr != nil {
				_ = connection.Close()
				serverDone <- writeErr
				return
			}
			_ = connection.Close()
		}
		serverDone <- nil
	}()

	events := make(chan Event, 4)
	done := make(chan struct{})
	defer close(done)
	go StreamSessionEvents(socket, events, done)

	want := []struct {
		change string
		epoch  uint64
	}{{"ready", 1}, {"disconnected", 2}, {"ready", 3}}
	for index, transition := range want {
		select {
		case event := <-events:
			if event.Type != EventStream || event.Change != transition.change || event.StreamEpoch != transition.epoch {
				t.Fatalf("stream transition %d emitted %+v, want %q epoch %d", index+1, event, transition.change, transition.epoch)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("stream transition %d (%s) was not reported", index+1, transition.change)
		}
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("fake Sway server: %v", err)
	}
}

func TestSessionEventStreamDisconnectsStateBeforeCleanShutdownDelivery(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "sway.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("unix sockets are not permitted in this sandbox: %v", err)
		}
		t.Fatalf("listen on fake Sway socket: %v", err)
	}
	defer listener.Close()

	serverDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer connection.Close()
		if _, readErr := readMessage(connection); readErr != nil {
			serverDone <- readErr
			return
		}
		if writeErr := writeMessage(connection, Subscribe, []byte(`{"success":true}`)); writeErr != nil {
			serverDone <- writeErr
			return
		}
		serverDone <- writeMessage(connection, shutdownEventMessage, []byte(`{"change":"exit"}`))
	}()

	events := make(chan Event, 2)
	done := make(chan struct{})
	defer close(done)
	state := &EventStreamState{}
	go StreamSessionEventsWithState(socket, events, done, state)
	select {
	case ready := <-events:
		if ready.Type != EventStream || ready.Change != "ready" {
			t.Fatalf("ready event = %+v", ready)
		}
	case <-time.After(time.Second):
		t.Fatal("session stream did not become ready")
	}
	select {
	case shutdown := <-events:
		if shutdown.Type != EventShutdown {
			t.Fatalf("shutdown event = %+v", shutdown)
		}
		if epoch, connected := state.Snapshot(); connected || epoch != 2 {
			t.Fatalf("clean shutdown left stream state connected: epoch=%d connected=%t", epoch, connected)
		}
	case <-time.After(time.Second):
		t.Fatal("session stream did not deliver clean shutdown")
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("fake Sway server: %v", err)
	}
}

func TestOpenSubscriptionContextBoundsEntireSetup(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "sway.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("unix sockets are not permitted in this sandbox: %v", err)
		}
		t.Fatalf("listen on fake Sway socket: %v", err)
	}
	defer listener.Close()

	serverDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer connection.Close()
		message, readErr := readMessage(connection)
		if readErr != nil {
			serverDone <- readErr
			return
		}
		if message.Type != Subscribe {
			serverDone <- errors.New("subscription setup sent the wrong request type")
			return
		}
		var probe [1]byte
		_, readErr = connection.Read(probe[:])
		serverDone <- readErr
	}()

	started := time.Now()
	connection, err := OpenSubscriptionContext(context.Background(), socket, []byte(`["shutdown"]`), 50*time.Millisecond)
	if connection != nil {
		connection.Close()
		t.Fatal("timed-out subscription returned a live connection")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unresponsive subscription returned %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("subscription setup exceeded its shared deadline: %v", elapsed)
	}
	select {
	case serverErr := <-serverDone:
		if serverErr == nil {
			t.Fatal("timed-out client did not close the subscription connection")
		}
	case <-time.After(time.Second):
		t.Fatal("timed-out subscription left its peer blocked")
	}
}

func TestSessionEventStreamStopsWhileSubscriptionIsQuiet(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "sway.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("unix sockets are not permitted in this sandbox: %v", err)
		}
		t.Fatalf("listen on fake Sway socket: %v", err)
	}
	defer listener.Close()

	serverDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer connection.Close()
		if _, readErr := readMessage(connection); readErr != nil {
			serverDone <- readErr
			return
		}
		if writeErr := writeMessage(connection, Subscribe, []byte(`{"success":true}`)); writeErr != nil {
			serverDone <- writeErr
			return
		}
		var probe [1]byte
		_, readErr := connection.Read(probe[:])
		serverDone <- readErr
	}()

	events := make(chan Event, 1)
	done := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		StreamSessionEvents(socket, events, done)
		close(returned)
	}()
	select {
	case event := <-events:
		if event.Type != EventStream {
			t.Fatalf("unexpected readiness event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("subscription did not become ready")
	}
	close(done)
	select {
	case <-returned:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("quiet subscription ignored shutdown")
	}
	select {
	case readErr := <-serverDone:
		if readErr == nil {
			t.Fatal("quiet peer unexpectedly read data instead of connection close")
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("event stream did not close quiet peer")
	}
}

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

func TestSessionMoveBarrierTickUsesLosslessDelivery(t *testing.T) {
	if !eventNeedsLosslessSessionDelivery(Event{Type: EventTick, Payload: "_sway_session_move_v1:1"}) {
		t.Fatal("move-attribution barrier could be dropped from a full event channel")
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
