package sessionrequest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type cancellationPeer struct {
	connection net.Conn
	request    Request
	err        error
	closed     <-chan struct{}
}

type cancellationResult struct {
	response Response
	err      error
}

type cancellationBroker struct {
	path        string
	listener    *net.UnixListener
	peers       chan cancellationPeer
	stop        chan struct{}
	done        chan struct{}
	workers     sync.WaitGroup
	mutex       sync.Mutex
	connections []net.Conn
	closed      bool
	accepted    atomic.Int32
	requests    atomic.Int32
}

func newCancellationBroker(t *testing.T) *cancellationBroker {
	t.Helper()
	// A short private directory also keeps the Unix socket below sun_path's limit.
	directory, err := os.MkdirTemp("", "session-start-client-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, SocketFilename)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	broker := &cancellationBroker{path: path, listener: listener, peers: make(chan cancellationPeer, 8), stop: make(chan struct{}), done: make(chan struct{})}
	t.Cleanup(broker.close)
	go broker.accept()
	return broker
}

func (broker *cancellationBroker) accept() {
	defer close(broker.done)
	for {
		connection, err := broker.listener.Accept()
		if err != nil {
			return
		}
		broker.accepted.Add(1)
		broker.mutex.Lock()
		if broker.closed {
			broker.mutex.Unlock()
			_ = connection.Close()
			continue
		}
		broker.connections = append(broker.connections, connection)
		broker.mutex.Unlock()
		broker.workers.Add(1)
		go broker.read(connection)
	}
}

func (broker *cancellationBroker) read(connection net.Conn) {
	defer broker.workers.Done()
	closed := make(chan struct{})
	defer close(closed)
	decoder := json.NewDecoder(io.LimitReader(connection, maxProtocolMessage+1))
	decoder.DisallowUnknownFields()
	for {
		var request Request
		err := decoder.Decode(&request)
		if errors.Is(err, io.EOF) {
			return
		}
		if err == nil {
			err = request.Validate()
		}
		if err == nil {
			broker.requests.Add(1)
		}
		select {
		case broker.peers <- cancellationPeer{connection: connection, request: request, err: err, closed: closed}:
		case <-broker.stop:
			return
		}
		if err != nil {
			return
		}
	}
}

func (broker *cancellationBroker) close() {
	broker.mutex.Lock()
	if broker.closed {
		broker.mutex.Unlock()
		return
	}
	broker.closed = true
	close(broker.stop)
	for _, connection := range broker.connections {
		_ = connection.Close()
	}
	broker.mutex.Unlock()
	_ = broker.listener.Close()
	<-broker.done
	broker.workers.Wait()
}

func (broker *cancellationBroker) receive(t *testing.T, want Request) cancellationPeer {
	t.Helper()
	select {
	case peer := <-broker.peers:
		if peer.err != nil {
			t.Fatalf("broker did not receive a valid request: %v", peer.err)
		}
		if peer.request != want {
			t.Fatalf("request changed across exchange: got=%+v want=%+v", peer.request, want)
		}
		return peer
	case <-time.After(time.Second):
		t.Fatal("broker did not receive the request")
		return cancellationPeer{}
	}
}

func (broker *cancellationBroker) send(t *testing.T, ctx context.Context, cancel context.CancelFunc, request Request) <-chan cancellationResult {
	t.Helper()
	result := make(chan cancellationResult, 1)
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		response, err := Send(ctx, broker.path, request)
		result <- cancellationResult{response: response, err: err}
	}()
	t.Cleanup(func() {
		cancel()
		broker.close()
		<-clientDone
	})
	return result
}

func receiveCancellationResult(t *testing.T, results <-chan cancellationResult) cancellationResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(time.Second):
		t.Fatal("exchange kept waiting for the broker response")
		return cancellationResult{}
	}
}

func (peer cancellationPeer) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-peer.closed:
	case <-time.After(time.Second):
		t.Fatal("client did not close its connection")
	}
}

func (broker *cancellationBroker) assertSingleRequest(t *testing.T) {
	t.Helper()
	broker.close()
	if got := broker.accepted.Load(); got != 1 {
		t.Fatalf("mutating request was retried: accepted %d connections", got)
	}
	if got := broker.requests.Load(); got != 1 {
		t.Fatalf("mutating request was resent: received %d requests", got)
	}
}

func cancellationResponse(t *testing.T, request Request) []byte {
	t.Helper()
	response := testResponse(request, true)
	response.Version = ProtocolVersion
	response.OK = true
	encoded, err := encodeResponseV1(response)
	if err != nil {
		t.Fatal(err)
	}
	return append(encoded, '\n')
}

func TestSendCancellationAfterRequestAccepted(t *testing.T) {
	for _, withDeadline := range []bool{false, true} {
		name := "default_deadline"
		if withDeadline {
			name = "explicit_deadline"
		}
		t.Run(name, func(t *testing.T) {
			broker := newCancellationBroker(t)
			request := testRequest(t)
			var ctx context.Context
			var cancel context.CancelFunc
			if withDeadline {
				ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			results := broker.send(t, ctx, cancel, request)
			peer := broker.receive(t, request)
			cancel()
			result := receiveCancellationResult(t, results)
			if !errors.Is(result.err, context.Canceled) {
				t.Fatalf("cancellation cause was lost: %v", result.err)
			}
			peer.waitClosed(t)
			broker.assertSingleRequest(t)
		})
	}
}

func TestSendDeadlineAfterRequestAccepted(t *testing.T) {
	broker := newCancellationBroker(t)
	request := testRequest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	results := broker.send(t, ctx, cancel, request)
	peer := broker.receive(t, request)
	result := receiveCancellationResult(t, results)
	if !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("deadline cause was lost: %v", result.err)
	}
	peer.waitClosed(t)
	broker.assertSingleRequest(t)
}

func TestSendSuccessfulResponseBeforeCancellation(t *testing.T) {
	broker := newCancellationBroker(t)
	request := testRequest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := broker.send(t, ctx, cancel, request)
	peer := broker.receive(t, request)
	if _, err := peer.connection.Write(cancellationResponse(t, request)); err != nil {
		t.Fatal(err)
	}
	result := receiveCancellationResult(t, results)
	if result.err != nil || result.response.Context == nil || result.response.Context.ID != testContextID || !result.response.Created {
		t.Fatalf("valid response was not preserved: response=%+v err=%v", result.response, result.err)
	}
	// Stopping cancellation cleanup must also be safe after the exchange returns.
	cancel()
	peer.waitClosed(t)
	broker.assertSingleRequest(t)
}

func TestSendPeerDisconnectPreservesTransportFailure(t *testing.T) {
	broker := newCancellationBroker(t)
	request := testRequest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := broker.send(t, ctx, cancel, request)
	peer := broker.receive(t, request)
	if err := peer.connection.Close(); err != nil {
		t.Fatal(err)
	}
	result := receiveCancellationResult(t, results)
	if result.err == nil || errors.Is(result.err, context.Canceled) || errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("peer failure was replaced with a context error: %v", result.err)
	}
	if ctx.Err() != nil {
		t.Fatalf("transport failure canceled the caller's context: %v", ctx.Err())
	}
	broker.assertSingleRequest(t)
}

func TestSendResponseCancellationRace(t *testing.T) {
	for iteration := range 25 {
		t.Run(strconv.Itoa(iteration), func(t *testing.T) {
			broker := newCancellationBroker(t)
			request := testRequest(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			results := broker.send(t, ctx, cancel, request)
			peer := broker.receive(t, request)
			response := cancellationResponse(t, request)
			start := make(chan struct{})
			writeDone := make(chan struct{})
			cancelDone := make(chan struct{})
			go func() {
				defer close(writeDone)
				<-start
				_, _ = peer.connection.Write(response)
			}()
			go func() {
				defer close(cancelDone)
				<-start
				cancel()
			}()
			t.Cleanup(func() {
				broker.close()
				<-writeDone
				<-cancelDone
			})
			close(start)
			result := receiveCancellationResult(t, results)
			if result.err != nil && !errors.Is(result.err, context.Canceled) {
				t.Fatalf("response/cancellation race lost its cause: %v", result.err)
			}
			if result.err == nil && (result.response.Context == nil || result.response.Context.ID != testContextID || !result.response.Created) {
				t.Fatalf("response/cancellation race returned an incomplete success: %+v", result.response)
			}
			<-writeDone
			<-cancelDone
			peer.waitClosed(t)
			broker.assertSingleRequest(t)
		})
	}
}
