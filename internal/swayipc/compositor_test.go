package swayipc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// All endpoints are disposable test listeners; these tests never consult
// SWAYSOCK or connect to the user's compositor.
type compositorTestServer struct {
	listener   *net.UnixListener
	requests   atomic.Int64
	accepted   atomic.Int64
	failNext   atomic.Bool
	mu         sync.Mutex
	conns      []net.Conn
	workers    sync.WaitGroup
	acceptDone chan struct{}
}

func compositorTestSocket(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "swayipc-pin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return filepath.Join(directory, "sway.sock")
}

func serveCompositorTest(t *testing.T, socket string) *compositorTestServer {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	server := &compositorTestServer{listener: listener, acceptDone: make(chan struct{})}
	server.workers.Add(1)
	go func() {
		defer server.workers.Done()
		defer close(server.acceptDone)
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			server.accepted.Add(1)
			server.mu.Lock()
			server.conns = append(server.conns, conn)
			server.mu.Unlock()
			server.workers.Add(1)
			go func() {
				defer server.workers.Done()
				defer conn.Close()
				for {
					message, err := readMessage(conn)
					if err != nil {
						return
					}
					server.requests.Add(1)
					if server.failNext.Swap(false) {
						return
					}
					if err := writeMessage(conn, message.Type, []byte(`{"ok":true}`)); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-server.acceptDone
		server.mu.Lock()
		for _, conn := range server.conns {
			_ = conn.Close()
		}
		server.mu.Unlock()
		server.workers.Wait()
	})
	return server
}

func TestPinCompositorScopePreservesOrdinaryClientReconnect(t *testing.T) {
	socket := compositorTestSocket(t)
	old := serveCompositorTest(t, socket)
	ordinary := NewClient(socket)
	t.Cleanup(ordinary.Close)
	if _, err := ordinary.Request(GetTree, nil); err != nil {
		t.Fatal(err)
	}
	connection := ordinary.conn
	scope, err := ordinary.PinCompositor(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scope.Close)
	if ordinary.conn != connection || ordinary.compositor != nil || scope.conn == connection {
		t.Fatal("operation scope changed or borrowed the ordinary control connection")
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	replacement := serveCompositorTest(t, socket)
	if _, err := scope.Request(RunCommand, nil); !errors.Is(err, ErrCompositorChanged) {
		t.Fatalf("scope crossed compositor lifetime: %v", err)
	}
	if _, err := scope.PinCompositor(t.Context()); !errors.Is(err, ErrCompositorChanged) {
		t.Fatalf("nested scope silently reset its pin: %v", err)
	}
	ordinary.Close()
	if _, err := ordinary.Request(GetTree, nil); err != nil {
		t.Fatalf("ordinary reconnect was pinned by the scope: %v", err)
	}
	fresh, err := ordinary.PinCompositor(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fresh.Close)
	if fresh.compositor.socketID == scope.compositor.socketID {
		t.Fatal("new operation did not select the new compositor")
	}
	if old.requests.Load() != 1 || replacement.requests.Load() != 1 {
		t.Fatal("failed operation scope sent unexpected requests")
	}
}

func TestPinnedClientRejectsChangedSocketMetadata(t *testing.T) {
	socket := compositorTestSocket(t)
	server := serveCompositorTest(t, socket)
	client := NewClient(socket)
	t.Cleanup(client.Close)
	if _, err := client.LifecycleCompositorID(t.Context()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, info.Mode().Perm()^0o100); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Request(RunCommand, nil); !errors.Is(err, ErrCompositorChanged) {
		t.Fatalf("changed socket ctime accepted: %v", err)
	}
	if server.requests.Load() != 0 {
		t.Fatal("command sent after lifetime metadata changed")
	}
}

func TestLifecycleCompositorIDMatchesCommandDigest(t *testing.T) {
	socket := compositorTestSocket(t)
	server := serveCompositorTest(t, socket)
	client := NewClient(socket)
	t.Cleanup(client.Close)
	id, err := client.LifecycleCompositorID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(socket)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	// The command's existing compositorIdentity contract, independently spelled
	// here so changing the library encoding cannot silently change persisted IDs.
	evidence := fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%d\x00%d", socket, stat.Dev, stat.Ino, stat.Ctim.Sec, stat.Ctim.Nsec, stat.Uid)
	digest := sha256.Sum256([]byte(evidence))
	if id != hex.EncodeToString(digest[:]) || server.requests.Load() != 0 {
		t.Fatalf("pin = %q, want command-compatible digest without requests", id)
	}
	if client.compositor.peer.pid != int32(os.Getpid()) || client.compositor.peer.startTime == 0 {
		t.Fatalf("missing connected process generation: %+v", client.compositor.peer)
	}
	client.Close()
	if again, err := client.LifecycleCompositorID(t.Context()); err != nil || again != id {
		t.Fatalf("same compositor reconnect = %q, %v", again, err)
	}
	server.failNext.Store(true)
	if _, err := client.RequestContext(t.Context(), GetTree, nil); err != nil {
		t.Fatalf("read-only reconnect to same compositor: %v", err)
	}
	if server.requests.Load() != 2 {
		t.Fatalf("read attempts = %d, want 2", server.requests.Load())
	}
	server.failNext.Store(true)
	_, err = client.RequestContext(t.Context(), RunCommand, []byte("nop"))
	var unknown *CommandOutcomeUnknownError
	if !errors.As(err, &unknown) || server.requests.Load() != 3 {
		t.Fatalf("mutating request was replayed or lost ambiguity: %v, requests=%d", err, server.requests.Load())
	}
}

func TestPinnedClientRejectsReplacedSocket(t *testing.T) {
	for _, closeFirst := range []bool{false, true} {
		t.Run(strconv.FormatBool(closeFirst), func(t *testing.T) {
			socket := compositorTestSocket(t)
			old := serveCompositorTest(t, socket)
			client := NewClient(socket)
			t.Cleanup(client.Close)
			if _, err := client.LifecycleCompositorID(t.Context()); err != nil {
				t.Fatal(err)
			}
			if closeFirst {
				client.Close()
			}
			if err := os.Remove(socket); err != nil {
				t.Fatal(err)
			}
			replacement := serveCompositorTest(t, socket)
			for _, kind := range []MessageType{RunCommand, GetTree, RunCommand} {
				if _, err := client.RequestContext(t.Context(), kind, nil); !errors.Is(err, ErrCompositorChanged) {
					t.Fatalf("request %d to replaced socket: %v", kind, err)
				}
			}
			if _, err := client.LifecycleCompositorID(t.Context()); !errors.Is(err, ErrCompositorChanged) {
				t.Fatalf("pin was silently replaced: %v", err)
			}
			if old.requests.Load() != 0 || replacement.requests.Load() != 0 {
				t.Fatal("command reached an endpoint after epoch replacement")
			}
			fresh := NewClient(socket)
			t.Cleanup(fresh.Close)
			if _, err := fresh.LifecycleCompositorID(t.Context()); err != nil {
				t.Fatalf("explicitly selecting new lifetime failed: %v", err)
			}
		})
	}
}

func TestCompositorPinRejectsPathReplacementDuringDial(t *testing.T) {
	for _, replaceBeforeDial := range []bool{true, false} {
		t.Run(strconv.FormatBool(replaceBeforeDial), func(t *testing.T) {
			socket := compositorTestSocket(t)
			old := serveCompositorTest(t, socket)
			var replacement *compositorTestServer
			replace := func() {
				if err := os.Remove(socket); err != nil {
					t.Fatal(err)
				}
				replacement = serveCompositorTest(t, socket)
			}
			connection, _, err := dialCompositor(t.Context(), socket, func(ctx context.Context, pinnedPath string) (*Conn, error) {
				if !strings.HasPrefix(pinnedPath, "/proc/self/fd/") {
					t.Fatalf("dial resolved mutable pathname %q", pinnedPath)
				}
				if replaceBeforeDial {
					replace()
				}
				conn, err := DialContext(ctx, pinnedPath)
				if !replaceBeforeDial {
					replace()
				}
				return conn, err
			})
			if connection != nil || !errors.Is(err, ErrCompositorChanged) {
				t.Fatalf("raced pin = %v, %v", connection, err)
			}
			if old.requests.Load() != 0 || replacement.requests.Load() != 0 || replacement.accepted.Load() != 0 {
				t.Fatal("dial race connected to the replacement or sent IPC")
			}
		})
	}
}

func TestCompositorPinReplacesUnverifiedCachedConnection(t *testing.T) {
	socket := compositorTestSocket(t)
	old := serveCompositorTest(t, socket)
	client := NewClient(socket)
	t.Cleanup(client.Close)
	if _, err := client.Request(GetTree, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	replacement := serveCompositorTest(t, socket)
	if _, err := client.LifecycleCompositorID(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Request(RunCommand, []byte("nop")); err != nil {
		t.Fatal(err)
	}
	if old.requests.Load() != 1 || replacement.requests.Load() != 1 {
		t.Fatal("pin associated replacement metadata with the old cached connection")
	}
}

func TestPinnedClientRejectsPeerGenerationChange(t *testing.T) {
	for _, closeFirst := range []bool{false, true} {
		t.Run(strconv.FormatBool(closeFirst), func(t *testing.T) {
			socket := compositorTestSocket(t)
			server := serveCompositorTest(t, socket)
			client := NewClient(socket)
			t.Cleanup(client.Close)
			if _, err := client.LifecycleCompositorID(t.Context()); err != nil {
				t.Fatal(err)
			}
			// Simulate reuse of the peer PID with a different process generation.
			client.compositor.peer.startTime++
			if closeFirst {
				client.Close()
			}
			if _, err := client.Request(RunCommand, nil); !errors.Is(err, ErrCompositorChanged) {
				t.Fatalf("changed peer generation accepted: %v", err)
			}
			if server.requests.Load() != 0 {
				t.Fatal("sent command before verifying peer generation")
			}
		})
	}
}

func TestCompositorPinRejectsInvalidEndpointsAndCancellation(t *testing.T) {
	socket := compositorTestSocket(t)
	serveCompositorTest(t, socket)
	symlink := socket + "-link"
	if err := os.Symlink(socket, symlink); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "relative.sock", socket + "/../sway.sock", symlink, filepath.Dir(socket)} {
		client := NewClient(path)
		if _, err := client.LifecycleCompositorID(t.Context()); err == nil || client.conn != nil || client.compositor != nil {
			t.Errorf("invalid endpoint %q established a pin: %v", path, err)
		}
		client.Close()
	}
	client := NewClient(socket)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.LifecycleCompositorID(ctx); !errors.Is(err, context.Canceled) || client.conn != nil {
		t.Fatalf("canceled pin established connection: %v", err)
	}
	//lint:ignore SA1012 Verify that the public boundary rejects a nil context.
	if _, err := client.LifecycleCompositorID(nil); err == nil {
		t.Fatal("nil context accepted")
	}
	var absent *Client
	if _, err := absent.LifecycleCompositorID(t.Context()); err == nil {
		t.Fatal("nil client accepted")
	}
}

func TestCompositorProcessStartTimeParsing(t *testing.T) {
	pid := int32(123)
	fields := append([]string{"S"}, strings.Fields(strings.Repeat("0 ", 18))...)
	fields = append(fields, "4242", "0", "0")
	valid := "123 (a tricky ) process (name)) " + strings.Join(fields, " ")
	if got, err := parseCompositorProcessStartTime(valid, pid); err != nil || got != 4242 {
		t.Fatalf("parse command with spaces/parentheses = %d, %v", got, err)
	}
	for _, invalid := range []string{"", "123 malformed", "123 (name) S 0", strings.Replace(valid, "4242", "-1", 1), strings.Replace(valid, "123", "456", 1)} {
		if _, err := parseCompositorProcessStartTime(invalid, pid); err == nil {
			t.Errorf("accepted malformed process stat %q", invalid)
		}
	}
	if start, err := compositorProcessStartTime(int32(os.Getpid())); err != nil || start == 0 {
		t.Fatalf("read current test process start time: %d, %v", start, err)
	}
}

func TestPinnedClientRejectsMissingSocketBeforeCommand(t *testing.T) {
	socket := compositorTestSocket(t)
	server := serveCompositorTest(t, socket)
	client := NewClient(socket)
	t.Cleanup(client.Close)
	if _, err := client.LifecycleCompositorID(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := client.RequestContext(ctx, RunCommand, nil); !errors.Is(err, ErrCompositorChanged) {
		t.Fatalf("missing path accepted: %v", err)
	}
	if server.requests.Load() != 0 {
		t.Fatal("command sent on unverifiable endpoint")
	}
}
