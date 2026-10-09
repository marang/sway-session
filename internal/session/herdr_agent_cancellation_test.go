package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHerdrEndpointRequestCanceledAfterMutatingRequest(t *testing.T) {
	server := newHerdrCancellationServer(t, waitForHerdrClientClose)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	finished := server.request(ctx, "cancel-report", "pane.report_agent_session")
	request := server.nextRequest(t)
	if request.Method != "pane.report_agent_session" || request.ID != "cancel-report" {
		t.Fatalf("unexpected accepted request: %#v", request)
	}
	cancel()
	select {
	case result := <-finished:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("canceled Herdr exchange returned %v, want context.Canceled", result.err)
		}
	case <-time.After(750 * time.Millisecond):
		t.Fatal("Herdr exchange stayed blocked after cancellation while the server held its response")
	}
	server.requireSingleRequest(t)
}

func TestHerdrEndpointRequestCanceledWithoutDeadline(t *testing.T) {
	server := newHerdrCancellationServer(t, waitForHerdrClientClose)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := server.request(ctx, "cancel-query", "pane.process_info")
	server.nextRequest(t)
	cancel()
	result := requireHerdrRequestResult(t, finished)
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("canceled Herdr exchange returned %v, want context.Canceled", result.err)
	}
	server.requireSingleRequest(t)
}

func TestHerdrEndpointRequestDeadline(t *testing.T) {
	server := newHerdrCancellationServer(t, waitForHerdrClientClose)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	finished := server.request(ctx, "deadline-query", "pane.process_info")
	server.nextRequest(t)
	result := requireHerdrRequestResult(t, finished)
	if !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("expired Herdr exchange returned %v, want context.DeadlineExceeded", result.err)
	}
	server.requireSingleRequest(t)
}

func TestHerdrEndpointRequestSuccessAfterEarlierContextCleanup(t *testing.T) {
	server := newHerdrCancellationServer(t, func(connection net.Conn, request herdrCancellationRequest) error {
		return writeHerdrCancellationResponse(connection, request.ID)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first := requireHerdrRequestResult(t, server.request(ctx, "first-query", "pane.process_info"))
	if first.err != nil || string(first.result) != `{"type":"ok"}` {
		t.Fatalf("first Herdr result = %s, error = %v", first.result, first.err)
	}
	cancel()
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer secondCancel()
	second := requireHerdrRequestResult(t, server.request(secondCtx, "second-query", "pane.process_info"))
	if second.err != nil || string(second.result) != `{"type":"ok"}` {
		t.Fatalf("Herdr result after earlier cancellation = %s, error = %v", second.result, second.err)
	}
	for _, want := range []string{"first-query", "second-query"} {
		if request := server.nextRequest(t); request.ID != want {
			t.Fatalf("accepted request ID = %q, want %q", request.ID, want)
		}
	}
	server.requireRequestCount(t, 2)
}

func TestHerdrEndpointRequestTransportError(t *testing.T) {
	server := newHerdrCancellationServer(t, func(net.Conn, herdrCancellationRequest) error {
		// Closing without a response exercises the real socket read failure.
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	finished := server.request(ctx, "closed-query", "pane.process_info")
	server.nextRequest(t)
	result := requireHerdrRequestResult(t, finished)
	if !errors.Is(result.err, io.EOF) {
		t.Fatalf("closed Herdr socket returned %v, want io.EOF", result.err)
	}
	if errors.Is(result.err, context.Canceled) || errors.Is(result.err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("ordinary Herdr transport failure became a context error: %v", result.err)
	}
	server.requireSingleRequest(t)
}

func TestHerdrEndpointRequestCanceledDuringPartialResponse(t *testing.T) {
	partialSent := make(chan struct{})
	server := newHerdrCancellationServer(t, func(connection net.Conn, request herdrCancellationRequest) error {
		if _, err := fmt.Fprintf(connection, `{"id":%q,"result":`, request.ID); err != nil {
			return err
		}
		close(partialSent)
		return waitForHerdrClientClose(connection, request)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	finished := server.request(ctx, "partial-query", "pane.process_info")
	server.nextRequest(t)
	select {
	case <-partialSent:
	case <-time.After(time.Second):
		t.Fatal("private Herdr server did not send its partial response")
	}
	cancel()
	result := requireHerdrRequestResult(t, finished)
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("cancellation during a partial Herdr response returned %v, want context.Canceled", result.err)
	}
	server.requireSingleRequest(t)
}

func TestHerdrEndpointRequestRetainsResponseGuards(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		response string
		want     string
	}{
		{"mismatched ID", `{"id":"other","result":{"type":"ok"}}`, "ID did not match"},
		{"unknown field", `{"id":"guard-query","result":{"type":"ok"},"extra":true}`, "unknown field"},
		{"trailing data", `{"id":"guard-query","result":{"type":"ok"}} {}`, "trailing data"},
		{"missing result", `{"id":"guard-query"}`, "omitted its result"},
		{"server rejection", `{"id":"guard-query","error":{"code":"rejected","message":"fixture rejection"}}`, "fixture rejection"},
		{"oversized response", strings.Repeat("x", maxHerdrAPIResponse+1), "exceeds"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := newHerdrCancellationServer(t, func(connection net.Conn, _ herdrCancellationRequest) error {
				_, err := io.WriteString(connection, testCase.response+"\n")
				return err
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			finished := server.request(ctx, "guard-query", "pane.process_info")
			server.nextRequest(t)
			result := requireHerdrRequestResult(t, finished)
			if result.err == nil || !strings.Contains(result.err.Error(), testCase.want) {
				t.Fatalf("Herdr response error = %v, want %q", result.err, testCase.want)
			}
			if errors.Is(result.err, context.Canceled) || errors.Is(result.err, context.DeadlineExceeded) || ctx.Err() != nil {
				t.Fatalf("Herdr response validation became a context error: %v", result.err)
			}
			server.requireSingleRequest(t)
		})
	}
}

func TestHerdrEndpointRequestResponseCancellationRace(t *testing.T) {
	// Both outcomes are legitimate once a valid reply and cancellation race.
	// Repeating the exchange also exercises cleanup of each registered callback.
	for iteration := range 32 {
		respond := make(chan struct{})
		server := newHerdrCancellationServer(t, func(connection net.Conn, request herdrCancellationRequest) error {
			<-respond
			// Cancellation may close the socket before the reply is written.
			_ = writeHerdrCancellationResponse(connection, request.ID)
			return nil
		})
		var release sync.Once
		releaseResponse := func() { release.Do(func() { close(respond) }) }
		t.Cleanup(releaseResponse)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		t.Cleanup(cancel)
		finished := server.request(ctx, fmt.Sprintf("race-%d", iteration), "pane.process_info")
		server.nextRequest(t)
		start := make(chan struct{})
		cancelDone := make(chan struct{})
		go func() {
			<-start
			cancel()
			close(cancelDone)
		}()
		close(start)
		releaseResponse()
		result := requireHerdrRequestResult(t, finished)
		<-cancelDone
		if result.err == nil {
			if string(result.result) != `{"type":"ok"}` {
				t.Fatalf("iteration %d returned an incomplete result: %s", iteration, result.result)
			}
		} else if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("iteration %d returned %v, want success or context.Canceled", iteration, result.err)
		}
		server.requireSingleRequest(t)
	}
}

type herdrCancellationRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type herdrCancellationServer struct {
	endpoint *herdrAPIEndpoint
	listener *net.UnixListener
	requests chan herdrCancellationRequest
	stopped  chan struct{}
	done     chan struct{}
	errors   chan error
	clients  sync.WaitGroup
	mutex    sync.Mutex
	active   net.Conn
	count    int
}

func newHerdrCancellationServer(t *testing.T, respond func(net.Conn, herdrCancellationRequest) error) *herdrCancellationServer {
	t.Helper()
	// Keep Unix socket paths short even when the caller uses a long subtest name.
	root, err := os.MkdirTemp("", "ss-herdr-cancel-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove private Herdr fixture: %v", err)
		}
	})
	sessionDir := filepath.Join(root, "sessions", "lab-280")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(sessionDir, herdrAgentSocketName)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint, err := openHerdrAPIEndpoint(root, "lab-280")
	if err != nil {
		t.Fatal(err)
	}
	server := &herdrCancellationServer{
		endpoint: endpoint, listener: listener, requests: make(chan herdrCancellationRequest, 16),
		stopped: make(chan struct{}), done: make(chan struct{}), errors: make(chan error, 1),
	}
	t.Cleanup(func() {
		close(server.stopped)
		_ = server.listener.Close()
		server.mutex.Lock()
		if server.active != nil {
			_ = server.active.Close()
		}
		server.mutex.Unlock()
		select {
		case <-server.done:
		case <-time.After(2 * time.Second):
			t.Error("private Herdr server did not stop during cleanup")
		}
		server.clients.Wait()
		server.endpoint.Close()
		select {
		case err := <-server.errors:
			t.Errorf("private Herdr server: %v", err)
		default:
		}
	})
	go server.serve(respond)
	return server
}

func (server *herdrCancellationServer) serve(respond func(net.Conn, herdrCancellationRequest) error) {
	defer close(server.done)
	for {
		connection, err := server.listener.AcceptUnix()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				server.errors <- err
			}
			return
		}
		server.mutex.Lock()
		server.active = connection
		select {
		case <-server.stopped:
			_ = connection.Close()
			server.mutex.Unlock()
			return
		default:
		}
		server.mutex.Unlock()
		line, err := bufio.NewReader(connection).ReadBytes('\n')
		var request herdrCancellationRequest
		if err == nil {
			err = json.Unmarshal(line, &request)
		}
		if err == nil {
			server.mutex.Lock()
			server.count++
			server.mutex.Unlock()
			select {
			case server.requests <- request:
			case <-server.stopped:
			}
			err = respond(connection, request)
		}
		_ = connection.Close()
		server.mutex.Lock()
		server.active = nil
		server.mutex.Unlock()
		if err != nil {
			select {
			case <-server.stopped:
			default:
				server.errors <- err
			}
			return
		}
	}
}

func (server *herdrCancellationServer) nextRequest(t *testing.T) herdrCancellationRequest {
	t.Helper()
	select {
	case request := <-server.requests:
		return request
	case err := <-server.errors:
		t.Fatalf("private Herdr server: %v", err)
	case <-time.After(time.Second):
		t.Fatal("private Herdr server did not receive the request")
	}
	return herdrCancellationRequest{}
}

func (server *herdrCancellationServer) requireSingleRequest(t *testing.T) {
	server.requireRequestCount(t, 1)
}

func (server *herdrCancellationServer) requireRequestCount(t *testing.T, want int) {
	t.Helper()
	server.mutex.Lock()
	defer server.mutex.Unlock()
	if server.count != want {
		t.Fatalf("Herdr exchange sent %d requests, want %d", server.count, want)
	}
}

type herdrCancellationResult struct {
	result json.RawMessage
	err    error
}

func (server *herdrCancellationServer) request(ctx context.Context, requestID, method string) <-chan herdrCancellationResult {
	finished := make(chan herdrCancellationResult, 1)
	server.clients.Add(1)
	go func() {
		defer server.clients.Done()
		result, err := server.endpoint.request(ctx, requestID, method, map[string]string{"pane_id": "work:p1"})
		finished <- herdrCancellationResult{result: result, err: err}
	}()
	return finished
}

func requireHerdrRequestResult(t *testing.T, finished <-chan herdrCancellationResult) herdrCancellationResult {
	t.Helper()
	select {
	case result := <-finished:
		return result
	case <-time.After(750 * time.Millisecond):
		t.Fatal("Herdr exchange stayed blocked while the server held its response")
	}
	return herdrCancellationResult{}
}

func waitForHerdrClientClose(connection net.Conn, _ herdrCancellationRequest) error {
	_, err := connection.Read(make([]byte, 1))
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("waiting for canceled client's connection to close: %w", err)
	}
	return nil
}

func writeHerdrCancellationResponse(connection net.Conn, requestID string) error {
	_, err := fmt.Fprintf(connection, "{\"id\":%q,\"result\":{\"type\":\"ok\"}}\n", requestID)
	return err
}
