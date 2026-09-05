package agentreport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestServerAcceptsOnlyValidatedGenericReport(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", SocketFilename)
	reports := make(chan Report, 1)
	server, err := StartServer(socketPath, func(_ context.Context, report Report) error {
		reports <- report
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	report := Report{Version: ProtocolVersion, ContextID: testContextID, PaneID: "work:p1", Agent: "claude", AgentSessionID: "claude:thread-01"}
	if err := Send(context.Background(), socketPath, report); err != nil {
		t.Fatal(err)
	}
	received := <-reports
	if received.ContextID != report.ContextID || received.PaneID != report.PaneID || received.Agent != report.Agent || received.AgentSessionID != report.AgentSessionID || received.PeerPID <= 0 {
		t.Fatalf("unexpected report: %+v", received)
	}
}

func TestStaleSocketCleanupFailsClosedOnAmbiguousProbe(t *testing.T) {
	runtimeDir := t.TempDir()
	path := filepath.Join(runtimeDir, SocketFilename)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	want := errors.New("ambiguous probe failure")
	err = removeStaleSocketWithProbe(directory, SocketFilename, func(string, string, time.Duration) (net.Conn, error) { return nil, want })
	if !errors.Is(err, want) {
		t.Fatalf("ambiguous probe error was not preserved: %v", err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("ambiguous probe removed the socket: info=%v err=%v", info, err)
	}
}

func TestStaleSocketCleanupPreservesReplacedEndpoint(t *testing.T) {
	runtimeDir := t.TempDir()
	path := filepath.Join(runtimeDir, SocketFilename)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	var replacement *net.UnixListener
	err = removeStaleSocketWithProbe(directory, SocketFilename, func(string, string, time.Duration) (net.Conn, error) {
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale endpoint in probe: %w", err)
		}
		var listenErr error
		replacement, listenErr = net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if listenErr != nil {
			return nil, listenErr
		}
		return nil, unix.ECONNREFUSED
	})
	if replacement == nil {
		t.Fatal("probe did not install its replacement endpoint")
	}
	defer replacement.Close()
	if !errors.Is(err, unix.EADDRINUSE) {
		t.Fatalf("replaced endpoint was not treated as active: %v", err)
	}
	connection, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("replacement endpoint was removed: %v", err)
	}
	_ = connection.Close()
}
