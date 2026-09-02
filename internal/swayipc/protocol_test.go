package swayipc

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadMessageRejectsOversizedPayloadBeforeAllocation(t *testing.T) {
	value := header(MessageType(100), MaxPayloadSize+1)
	if _, err := readMessage(bytes.NewReader(value)); err == nil {
		t.Fatal("expected oversized IPC payload to be rejected before allocation")
	}
}

func TestWriteMessageRejectsOversizedPayload(t *testing.T) {
	payload := make([]byte, int(MaxPayloadSize)+1)
	if err := writeMessage(&bytes.Buffer{}, MessageType(100), payload); err == nil {
		t.Fatal("expected oversized outgoing IPC payload to be rejected")
	}
}

func TestClientRejectsOversizedPayloadBeforeDial(t *testing.T) {
	client := NewClient(filepath.Join(t.TempDir(), "missing.sock"))
	payload := make([]byte, int(MaxPayloadSize)+1)
	if _, err := client.Request(GetTree, payload); err == nil || client.conn != nil {
		t.Fatalf("expected oversized request to fail without opening a connection: err=%v", err)
	}
}

func TestClientUnixSocketRoundTrip(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "sway.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("unix sockets are not permitted in this sandbox: %v", err)
		}
		t.Fatalf("listen unix socket: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})

	serverDone := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer connection.Close()

		message, err := readMessage(connection)
		if err != nil {
			serverDone <- err
			return
		}
		if message.Type != MessageType(99) || string(message.Payload) != "hello" {
			serverDone <- errors.New("unexpected request")
			return
		}
		serverDone <- writeMessage(connection, MessageType(100), []byte("ok"))
	}()

	client := NewClient(socket)
	t.Cleanup(client.Close)
	message, err := client.Request(MessageType(99), []byte("hello"))
	if err != nil {
		t.Fatalf("ipc request: %v", err)
	}
	if message.Type != MessageType(100) || string(message.Payload) != "ok" {
		t.Fatalf("unexpected response type=%d body=%q", message.Type, message.Payload)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func TestClientRequestContextInterruptsUnresponsivePeer(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "sway.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("unix sockets are not permitted in this sandbox: %v", err)
		}
		t.Fatalf("listen unix socket: %v", err)
	}
	defer listener.Close()

	requestRead := make(chan error, 1)
	peerReleased := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			requestRead <- acceptErr
			return
		}
		defer connection.Close()
		_, readErr := readMessage(connection)
		requestRead <- readErr
		<-peerReleased
	}()
	defer close(peerReleased)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	client := NewClient(socket)
	defer client.Close()
	_, err = client.RequestContext(ctx, GetTree, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unresponsive peer returned %v, want context deadline", err)
	}
	if err := <-requestRead; err != nil {
		t.Fatalf("peer did not receive request: %v", err)
	}
	if client.conn != nil {
		t.Fatal("canceled request connection remained cached")
	}
}

func TestConnReadContextInterruptsQuietSubscription(t *testing.T) {
	clientSide, peerSide := net.Pipe()
	defer peerSide.Close()
	connection := &Conn{conn: clientSide}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := connection.ReadContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("quiet subscription returned %v, want context deadline", err)
	}
	if connection.current() != nil {
		t.Fatal("canceled subscription connection remained open")
	}
}

func TestClientRequestHasFiniteDefaultDeadline(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "sway.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("unix sockets are not permitted in this sandbox: %v", err)
		}
		t.Fatalf("listen unix socket: %v", err)
	}
	defer listener.Close()

	requestRead := make(chan error, 1)
	peerReleased := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			requestRead <- acceptErr
			return
		}
		defer connection.Close()
		_, readErr := readMessage(connection)
		requestRead <- readErr
		<-peerReleased
	}()
	defer close(peerReleased)

	client := NewClient(socket)
	client.requestTimeout = 50 * time.Millisecond
	defer client.Close()
	_, err = client.Request(GetTree, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unbounded request returned %v, want finite default deadline", err)
	}
	if err := <-requestRead; err != nil {
		t.Fatalf("peer did not receive request: %v", err)
	}
	if client.conn != nil {
		t.Fatal("timed-out request connection remained cached")
	}
}

func TestConnCloseInterruptsBlockedRead(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "sway.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("unix sockets are not permitted in this sandbox: %v", err)
		}
		t.Fatal(err)
	}
	defer listener.Close()
	connection, err := Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	file, ok := connection.conn.(*os.File)
	if !ok {
		t.Fatalf("unexpected Sway connection type %T", connection.conn)
	}
	readStarted := make(chan struct{})
	connection.conn = &readNotifyingFile{File: file, started: readStarted}
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	readDone := make(chan error, 1)
	go func() {
		_, readErr := connection.Read()
		readDone <- readErr
	}()
	<-readStarted
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("blocked read unexpectedly returned no error")
		}
	case <-time.After(time.Second):
		t.Fatal("closing Sway IPC connection did not interrupt blocked read")
	}
}

type readNotifyingFile struct {
	*os.File
	started chan struct{}
}

func (file *readNotifyingFile) Read(value []byte) (int, error) {
	close(file.started)
	return file.File.Read(value)
}

func TestClientReconnectsOnceAfterFailedExchange(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "sway.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("unix sockets are not permitted in this sandbox: %v", err)
		}
		t.Fatalf("listen unix socket: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})

	serverDone := make(chan error, 1)
	go func() {
		first, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		if _, err := readMessage(first); err != nil {
			_ = first.Close()
			serverDone <- err
			return
		}
		_ = first.Close()

		second, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer second.Close()
		message, err := readMessage(second)
		if err != nil {
			serverDone <- err
			return
		}
		if message.Type != GetTree || len(message.Payload) != 0 {
			serverDone <- errors.New("unexpected retried request")
			return
		}
		serverDone <- writeMessage(second, GetTree, []byte(`{"id":1}`))
	}()

	client := NewClient(socket)
	t.Cleanup(client.Close)
	message, err := client.Request(GetTree, nil)
	if err != nil {
		t.Fatalf("request after reconnect: %v", err)
	}
	if string(message.Payload) != `{"id":1}` {
		t.Fatalf("unexpected response %q", message.Payload)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func TestClientDoesNotRepeatRunCommandAfterResponseConnectionFails(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "sway.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("unix sockets are not permitted in this sandbox: %v", err)
		}
		t.Fatalf("listen unix socket: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		t.Fatal("unix listener has unexpected type")
	}

	serverDone := make(chan error, 1)
	go func() {
		first, acceptErr := unixListener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		message, readErr := readMessage(first)
		_ = first.Close()
		if readErr != nil {
			serverDone <- readErr
			return
		}
		if message.Type != RunCommand || string(message.Payload) != "move right" {
			serverDone <- errors.New("unexpected command request")
			return
		}
		if deadlineErr := unixListener.SetDeadline(time.Now().Add(250 * time.Millisecond)); deadlineErr != nil {
			serverDone <- deadlineErr
			return
		}
		second, secondErr := unixListener.Accept()
		if secondErr == nil {
			_ = second.Close()
			serverDone <- errors.New("mutating command was sent a second time")
			return
		}
		var networkError net.Error
		if !errors.As(secondErr, &networkError) || !networkError.Timeout() {
			serverDone <- secondErr
			return
		}
		serverDone <- nil
	}()

	client := NewClient(socket)
	t.Cleanup(client.Close)
	_, err = client.Request(RunCommand, []byte("move right"))
	var unknown *CommandOutcomeUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("expected ambiguous command outcome, got %v", err)
	}
	if client.conn != nil {
		t.Fatal("failed command connection remained cached")
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func TestCheckRunCommandResponseRequiresMatchingSuccessfulResults(t *testing.T) {
	if err := CheckRunCommandResponse(Message{Type: RunCommand, Payload: []byte(`[{"success":true}]`)}); err != nil {
		t.Fatalf("expected successful command response: %v", err)
	}
	for _, message := range []Message{
		{Type: GetTree, Payload: []byte(`[{"success":true}]`)},
		{Type: RunCommand, Payload: []byte(`[]`)},
		{Type: RunCommand, Payload: []byte(`[{"success":false,"error":"denied"}]`)},
		{Type: RunCommand, Payload: []byte(`not-json`)},
	} {
		if err := CheckRunCommandResponse(message); err == nil {
			t.Fatalf("expected command response to be rejected: %+v", message)
		}
	}
	for _, message := range []Message{
		{Type: GetTree, Payload: []byte(`[{"success":true}]`)},
		{Type: RunCommand, Payload: []byte(`[]`)},
		{Type: RunCommand, Payload: []byte(`not-json`)},
	} {
		var invalid *CommandResponseInvalidError
		if err := CheckRunCommandResponse(message); !errors.As(err, &invalid) {
			t.Fatalf("expected invalid response error, got %v", err)
		}
	}
}

func TestCheckSubscribeResponseRequiresMatchingSuccess(t *testing.T) {
	if err := CheckSubscribeResponse(Message{Type: Subscribe, Payload: []byte(`{"success":true}`)}); err != nil {
		t.Fatalf("expected successful subscription response: %v", err)
	}
	for _, message := range []Message{
		{Type: RunCommand, Payload: []byte(`{"success":true}`)},
		{Type: Subscribe, Payload: []byte(`{"success":false}`)},
		{Type: Subscribe, Payload: []byte(`not-json`)},
	} {
		if err := CheckSubscribeResponse(message); err == nil {
			t.Fatalf("expected subscription response to be rejected: %+v", message)
		}
	}
}

func TestCheckSendTickResponseRequiresMatchingSuccess(t *testing.T) {
	if err := CheckSendTickResponse(Message{Type: SendTick, Payload: []byte(`{"success":true}`)}); err != nil {
		t.Fatalf("expected successful send-tick response: %v", err)
	}
	for _, message := range []Message{
		{Type: RunCommand, Payload: []byte(`{"success":true}`)},
		{Type: SendTick, Payload: []byte(`{"success":false}`)},
		{Type: SendTick, Payload: []byte(`not-json`)},
	} {
		if err := CheckSendTickResponse(message); err == nil {
			t.Fatalf("expected send-tick response to be rejected: %+v", message)
		}
	}
}
