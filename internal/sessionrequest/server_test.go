package sessionrequest

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

const testContextID = sessionstate.ContextID("11111111-1111-4111-8111-111111111111")

func testRequest(t *testing.T) Request {
	t.Helper()
	return Request{Version: ProtocolVersion, Session: "reboot-e2e", Cwd: t.TempDir(), Label: "REBOOT-E2E", Provider: "local", Workspace: 7}
}

func testResponse(request Request, created bool) Response {
	contextValue := sessionstate.Context{ID: testContextID, Label: request.Label, Provider: request.Provider, State: sessionstate.ContextActive, Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: request.Session, Cwd: request.Cwd, Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}}}
	return Response{Context: &contextValue, Workspace: request.Workspace, Created: created}
}

func TestServerAcceptsValidatedStartRequest(t *testing.T) {
	request := testRequest(t)
	socketPath := filepath.Join(t.TempDir(), "runtime", SocketFilename)
	server, err := StartServer(socketPath, func(_ context.Context, got Request) (Response, error) {
		if got != request {
			t.Fatalf("request changed across broker: got=%+v want=%+v", got, request)
		}
		return testResponse(got, true), nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	response, err := Send(context.Background(), socketPath, request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Context == nil || response.Context.ID != testContextID || !response.Created || response.Workspace != 7 {
		t.Fatalf("unexpected response: %+v", response)
	}
	info, err := os.Lstat(socketPath)
	if err != nil || info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("broker socket is not owner-only: info=%v err=%v", info, err)
	}
}

func TestServerAcceptsConnectOnlyLivenessProbeWithoutHandler(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", SocketFilename)
	called := make(chan struct{}, 1)
	errorsReported := make(chan error, 1)
	server, err := StartServer(socketPath, func(context.Context, Request) (Response, error) {
		called <- struct{}{}
		return Response{}, nil
	}, func(err error) { errorsReported <- err })
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	select {
	case <-called:
		t.Fatal("connect-only probe invoked the handler")
	case err := <-errorsReported:
		t.Fatalf("connect-only probe reported an error: %v", err)
	default:
	}
}

func TestServerCloseCancelsActiveHandler(t *testing.T) {
	request := testRequest(t)
	socketPath := filepath.Join(t.TempDir(), "runtime", SocketFilename)
	handlerStarted := make(chan struct{})
	handlerStopped := make(chan struct{})
	server, err := StartServer(socketPath, func(ctx context.Context, _ Request) (Response, error) {
		close(handlerStarted)
		<-ctx.Done()
		close(handlerStopped)
		return Response{}, ctx.Err()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	clientDone := make(chan error, 1)
	go func() {
		_, sendErr := Send(context.Background(), socketPath, request)
		clientDone <- sendErr
	}()
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- server.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close server: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server close did not cancel the active handler")
	}
	select {
	case <-handlerStopped:
	default:
		t.Fatal("server returned from close before handler stopped")
	}
	if err := <-clientDone; err == nil {
		t.Fatal("client unexpectedly accepted a canceled handler response")
	}
}

func TestServerRejectsUnknownFieldsBeforeHandler(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", SocketFilename)
	called := false
	server, err := StartServer(socketPath, func(context.Context, Request) (Response, error) {
		called = true
		return Response{}, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	payload, _ := json.Marshal(testRequest(t))
	payload = append(payload[:len(payload)-1], []byte(`,"method":"pane.send_input"}`)...)
	_, _ = connection.Write(append(payload, '\n'))
	var response Response
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error != "request rejected" || called {
		t.Fatalf("unsafe request was not rejected: response=%+v called=%v", response, called)
	}
}

func TestServerRejectsSessionRoleExtensionBeforeHandler(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", SocketFilename)
	called := false
	server, err := StartServer(socketPath, func(context.Context, Request) (Response, error) {
		called = true
		return Response{}, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	payload, _ := json.Marshal(testRequest(t))
	payload = append(payload[:len(payload)-1], []byte(`,"roles":["codex","shell"]}`)...)
	_, _ = connection.Write(append(payload, '\n'))
	var response Response
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error != "request rejected" || called {
		t.Fatalf("role-bearing request was not rejected: response=%+v called=%v", response, called)
	}
}

func TestServerRejectsMalformedOversizedAndTrailingRequestsBeforeHandler(t *testing.T) {
	valid, err := json.Marshal(testRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, payload := range map[string][]byte{
		"malformed": []byte("{\n"),
		"oversized": append([]byte(strings.Repeat("x", maxProtocolMessage+1)), '\n'),
		"trailing":  append(append(append([]byte(nil), valid...), []byte(` {}`)...), '\n'),
	} {
		t.Run(name, func(t *testing.T) {
			runtimeRoot, err := os.MkdirTemp("", "sessionrequest-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(runtimeRoot) })
			socketPath := filepath.Join(runtimeRoot, "runtime", SocketFilename)
			called := false
			server, err := StartServer(socketPath, func(context.Context, Request) (Response, error) {
				called = true
				return Response{}, nil
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			connection, err := net.Dial("unix", socketPath)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			_ = connection.SetDeadline(time.Now().Add(time.Second))
			if _, err := connection.Write(payload); err != nil {
				t.Fatal(err)
			}
			var response Response
			if err := json.NewDecoder(connection).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if response.OK || response.Error != "request rejected" || called {
				t.Fatalf("invalid request was not rejected: response=%+v called=%v", response, called)
			}
		})
	}
}

func TestServerDoesNotLeakHandlerFailure(t *testing.T) {
	request := testRequest(t)
	socketPath := filepath.Join(t.TempDir(), "runtime", SocketFilename)
	server, err := StartServer(socketPath, func(context.Context, Request) (Response, error) {
		return Response{}, errors.New("private path /home/user/.local/state/sway-session")
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	_, err = Send(context.Background(), socketPath, request)
	if err == nil || err.Error() != "session start request rejected" || strings.Contains(err.Error(), "private path") {
		t.Fatalf("broker failure leaked details: %v", err)
	}
}

func TestClientRejectsInactiveContextResponse(t *testing.T) {
	request := testRequest(t)
	socketPath := filepath.Join(t.TempDir(), "runtime", SocketFilename)
	server, err := StartServer(socketPath, func(_ context.Context, got Request) (Response, error) {
		response := testResponse(got, false)
		response.Context.State = sessionstate.ContextArchived
		return response, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if _, err := Send(context.Background(), socketPath, request); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("inactive broker response was accepted: %v", err)
	}
}
