package main

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
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/session"
	"golang.org/x/sys/unix"
)

var reachTestEnvironment = session.EnvironmentID("11111111-1111-4111-8111-111111111111")
var reachTestContext = session.ContextID("22222222-2222-4222-8222-222222222222")

func reachTestRequest() reachRequest {
	return reachRequest{Version: 1, CompositorID: strings.Repeat("a", 64), EnvironmentID: reachTestEnvironment, Primary: reachTestContext}
}
func reachTestResult() session.ReachResult {
	return session.ReachResult{EnvironmentID: reachTestEnvironment, Primary: reachTestContext, Members: []session.ReachMemberResult{{ContextID: reachTestContext, Primary: true, Status: session.ReachFocused, Reason: "Focused in place."}}}
}
func reachTestPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "private", "reach.sock")
}
func reachTestStart(t *testing.T, path string, handler func(context.Context, reachRequest) (session.ReachResult, error)) io.Closer {
	t.Helper()
	server, err := startReachServer(t.Context(), path, handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	return server
}
func reachTestReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("Reach operation did not finish")
		var zero T
		return zero
	}
}

func TestReachRoundtripStreamsLargeResultsWithoutTotalMemberCap(t *testing.T) {
	path := reachTestPath(t)
	want := reachTestResult()
	for index := range 3000 {
		want.Members = append(want.Members, session.ReachMemberResult{ContextID: session.ContextID(fmt.Sprintf("%08x-0000-4000-8000-000000000001", index+10)), Status: session.ReachAlreadyPresent, Reason: "Leave the existing window in place."})
	}
	reachTestStart(t, path, func(_ context.Context, request reachRequest) (session.ReachResult, error) {
		if request != reachTestRequest() {
			t.Errorf("changed intent: %+v", request)
		}
		return want, nil
	})
	got, err := sendReachRequest(t.Context(), path, reachTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("streamed %d members, expected %d", len(got.Members), len(want.Members))
	}
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{{filepath.Dir(path), 0o700}, {path, 0o600}} {
		info, err := os.Stat(item.path)
		if err != nil || info.Mode().Perm() != item.mode {
			t.Fatalf("private mode %s: %v %v", item.path, info, err)
		}
	}
}

func TestReachRejectsMalformedRequestsBeforeHandler(t *testing.T) {
	path := reachTestPath(t)
	var calls atomic.Int32
	server := reachTestStart(t, path, func(context.Context, reachRequest) (session.ReachResult, error) {
		calls.Add(1)
		return reachTestResult(), nil
	})
	valid := `{"version":1,"compositor_id":"` + strings.Repeat("a", 64) + `","environment_id":"11111111-1111-4111-8111-111111111111"}`
	cases := []string{
		strings.Replace(valid, `"version":1`, `"version":2`, 1),
		strings.Replace(valid, strings.Repeat("a", 64), strings.Repeat("A", 64), 1),
		strings.Replace(valid, strings.Repeat("a", 64), "short", 1),
		strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		strings.TrimSuffix(valid, "}") + `,"commands":["focus"]}`,
		strings.Replace(valid, "11111111-1111-4111-8111-111111111111", "not-an-id", 1),
		strings.TrimSuffix(valid, "}") + `,"primary":"not-an-id"}`,
		valid + ` {}`, valid + "\n" + valid, "null", strings.Repeat(" ", reachFrameLimit) + valid,
	}
	for index, data := range cases {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			connection, err := net.Dial("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			_ = connection.SetDeadline(time.Now().Add(time.Second))
			if _, err := io.WriteString(connection, data+"\n"); err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(connection)
		})
	}
	probe, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	_ = probe.Close()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid/probe request invoked %d effects", calls.Load())
	}
}

func TestReachDisconnectAndClientCancellationRevokeHandler(t *testing.T) {
	for _, kind := range []string{"disconnect", "context", "extra-data"} {
		t.Run(kind, func(t *testing.T) {
			path := reachTestPath(t)
			started := make(chan struct{})
			stopped := make(chan error, 1)
			reachTestStart(t, path, func(ctx context.Context, _ reachRequest) (session.ReachResult, error) {
				close(started)
				<-ctx.Done()
				stopped <- ctx.Err()
				return session.ReachResult{}, ctx.Err()
			})
			if kind == "context" {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				result := make(chan error, 1)
				go func() { _, err := sendReachRequest(ctx, path, reachTestRequest()); result <- err }()
				reachTestReceive(t, started)
				cancel()
				if err := reachTestReceive(t, result); !errors.Is(err, context.Canceled) {
					t.Fatalf("client cancellation lost: %v", err)
				}
			} else {
				connection, err := net.Dial("unix", path)
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close()
				data, _ := json.Marshal(reachTestRequest())
				_, _ = connection.Write(append(data, '\n'))
				reachTestReceive(t, started)
				if kind == "disconnect" {
					_ = connection.Close()
				} else {
					_, _ = connection.Write([]byte("unexpected"))
				}
			}
			if err := reachTestReceive(t, stopped); !errors.Is(err, context.Canceled) {
				t.Fatalf("handler not canceled: %v", err)
			}
		})
	}
}

func TestReachServerCancellationClosesIdleReadersAndRunningHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	path := reachTestPath(t)
	started := make(chan struct{})
	stopped := make(chan struct{})
	server, err := startReachServer(ctx, path, func(ctx context.Context, _ reachRequest) (session.ReachResult, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return session.ReachResult{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	idle, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	done := make(chan error, 1)
	go func() { _, err := sendReachRequest(t.Context(), path, reachTestRequest()); done <- err }()
	reachTestReceive(t, started)
	cancel()
	reachTestReceive(t, stopped)
	if err := reachTestReceive(t, done); err == nil {
		t.Fatal("server cancellation reported success")
	}
	closed := make(chan error, 1)
	go func() { closed <- server.Close() }()
	if err := reachTestReceive(t, closed); err != nil {
		t.Fatal(err)
	}
}

func TestReachWorkerAdmissionIsBounded(t *testing.T) {
	path := reachTestPath(t)
	started := make(chan struct{}, 8)
	reachTestStart(t, path, func(ctx context.Context, _ reachRequest) (session.ReachResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return session.ReachResult{}, ctx.Err()
	})
	connections := make([]net.Conn, 0, 8)
	defer func() {
		for _, connection := range connections {
			_ = connection.Close()
		}
	}()
	data, _ := json.Marshal(reachTestRequest())
	for range 8 {
		connection, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, connection)
		_, _ = connection.Write(append(data, '\n'))
		reachTestReceive(t, started)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := sendReachRequest(ctx, path, reachTestRequest()); err == nil {
		t.Fatal("ninth worker was admitted")
	}
	select {
	case <-started:
		t.Fatal("worker limit exceeded")
	default:
	}
}

func TestReachOwnerAndPathValidation(t *testing.T) {
	if reachCredentialsOwned(nil) == nil || reachCredentialsOwned(&unix.Ucred{Uid: uint32(os.Geteuid()) + 1}) == nil {
		t.Fatal("accepted foreign/unknown peer")
	}
	if err := reachCredentialsOwned(&unix.Ucred{Uid: uint32(os.Geteuid())}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := defaultReachSocketPath(); err == nil {
		t.Fatal("accepted missing runtime")
	}
	t.Setenv("XDG_RUNTIME_DIR", "relative")
	if _, err := defaultReachSocketPath(); err == nil {
		t.Fatal("accepted relative runtime")
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path, err := defaultReachSocketPath()
	if err != nil || path != filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "sway-session", "reach.sock") {
		t.Fatalf("default path: %s %v", path, err)
	}
	for _, kind := range []string{"file", "symlink", "directory", "public-parent"} {
		t.Run(kind, func(t *testing.T) {
			path := reachTestPath(t)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "file":
				_ = os.WriteFile(path, []byte("keep"), 0o600)
			case "directory":
				_ = os.Mkdir(path, 0o700)
			case "symlink":
				_ = os.Symlink(filepath.Join(t.TempDir(), "target"), path)
			case "public-parent":
				_ = os.Chmod(filepath.Dir(path), 0o755)
			}
			handler := func(context.Context, reachRequest) (session.ReachResult, error) {
				t.Error("unsafe endpoint handler invoked")
				return reachTestResult(), nil
			}
			if server, err := startReachServer(t.Context(), path, handler); err == nil {
				_ = server.Close()
				t.Fatal("unsafe endpoint accepted")
			}
			if kind != "public-parent" {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("unsafe endpoint was removed: %v", err)
				}
			}
			if _, err := sendReachRequest(t.Context(), path, reachTestRequest()); err == nil {
				t.Fatal("unsafe client endpoint accepted")
			}
		})
	}
}

func TestReachStaleCleanupAndClosePreserveReplacement(t *testing.T) {
	path := reachTestPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	_ = stale.Close()
	server := reachTestStart(t, path, func(context.Context, reachRequest) (session.ReachResult, error) { return reachTestResult(), nil })
	if other, err := startReachServer(t.Context(), path, func(context.Context, reachRequest) (session.ReachResult, error) { return reachTestResult(), nil }); !errors.Is(err, unix.EADDRINUSE) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("live endpoint was not preserved: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "replacement" {
		t.Fatalf("replacement removed: %q %v", data, err)
	}
}

func TestReachStaleProbePreservesAmbiguousOrReplacedSocket(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint(replacement), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "reach.sock")
			stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			stale.SetUnlinkOnClose(false)
			_ = stale.Close()
			directory, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer directory.Close()
			ambiguous := errors.New("probe unavailable")
			err = reachRemoveStale(directory, "reach.sock", func(string, string, time.Duration) (net.Conn, error) {
				if replacement {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = listener.Close() })
					return nil, unix.ECONNREFUSED
				}
				return nil, ambiguous
			})
			if replacement && !errors.Is(err, unix.EADDRINUSE) || !replacement && !errors.Is(err, ambiguous) {
				t.Fatalf("probe ambiguity/replacement not preserved: %v", err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("endpoint removed: %v", err)
			}
		})
	}
}

func TestReachClientRejectsMalformedOrIncompleteStreams(t *testing.T) {
	header := `{"version":1,"type":"header","environment_id":"11111111-1111-4111-8111-111111111111","primary":"22222222-2222-4222-8222-222222222222"}` + "\n"
	end := `{"version":1,"type":"end"}` + "\n"
	member := `{"version":1,"type":"member","member":{"context_id":"22222222-2222-4222-8222-222222222222","primary":true,"status":"focused","reason":"ok"}}` + "\n"
	for index, response := range []string{end, header, header + end, header + header + end, header + member + member + end, header + strings.Replace(member, `"focused"`, `"invented"`, 1) + end, header + strings.Replace(member, `"primary":true`, `"primary":false`, 1) + end, header + strings.Repeat(" ", reachFrameLimit) + end, header + end + "garbage", strings.Replace(header, `"version":1`, `"version":2`, 1) + end, strings.Replace(header, `"type":"header"`, `"type":"header","path":"private"`, 1) + end} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			path := reachTestPath(t)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			_ = os.Chmod(path, 0o600)
			done := make(chan struct{})
			go func() {
				defer close(done)
				connection, err := listener.AcceptUnix()
				if err != nil {
					return
				}
				defer connection.Close()
				_, _ = bufio.NewReader(connection).ReadBytes('\n')
				_, _ = io.WriteString(connection, response)
			}()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if _, err := sendReachRequest(ctx, path, reachTestRequest()); err == nil {
				t.Fatal("invalid stream accepted")
			}
			reachTestReceive(t, done)
		})
	}
}

func TestReachHandlerErrorsAreRedactedAndInvalidIntentsNeverDial(t *testing.T) {
	path := reachTestPath(t)
	reachTestStart(t, path, func(context.Context, reachRequest) (session.ReachResult, error) {
		return session.ReachResult{}, errors.New("private state /secret")
	})
	if _, err := sendReachRequest(t.Context(), path, reachTestRequest()); err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("handler details leaked: %v", err)
	}
	for _, request := range []reachRequest{{Version: 2, EnvironmentID: reachTestEnvironment}, {Version: 1, EnvironmentID: "invalid"}, {Version: 1, EnvironmentID: reachTestEnvironment, Primary: "invalid"}} {
		if _, err := sendReachRequest(t.Context(), "/absent/private/reach.sock", request); err == nil {
			t.Fatal("invalid intent accepted")
		}
	}
}

func TestReachEmptyEnvironmentAndAbsentPrimaryAreValidStreamShapes(t *testing.T) {
	path := reachTestPath(t)
	want := session.ReachResult{EnvironmentID: reachTestEnvironment, Members: []session.ReachMemberResult{}}
	reachTestStart(t, path, func(context.Context, reachRequest) (session.ReachResult, error) { return want, nil })
	request := reachTestRequest()
	request.Primary = ""
	got, err := sendReachRequest(t.Context(), path, request)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("empty environment changed: %+v %v", got, err)
	}
}

func TestReachCloseUsesPinnedDirectoryAfterPathReplacement(t *testing.T) {
	path := reachTestPath(t)
	server := reachTestStart(t, path, func(context.Context, reachRequest) (session.ReachResult, error) { return reachTestResult(), nil })
	original := filepath.Dir(path)
	moved := original + "-moved"
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(moved, "reach.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned old inode not removed: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "replacement" {
		t.Fatalf("replacement directory touched: %q %v", data, err)
	}
}
