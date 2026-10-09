package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/sessionrequest"
)

func TestRequestStartSignalCancellation(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			// A short private path keeps the fixed broker socket within Unix's
			// pathname limit. The helper CLI accesses no live state or compositor.
			root, err := os.MkdirTemp("", "lab280-cli-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			runtime := filepath.Join(root, "sway-session")
			if err := os.Mkdir(runtime, 0o700); err != nil {
				t.Fatal(err)
			}
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: filepath.Join(runtime, sessionrequest.SocketFilename)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			if err := os.Chmod(listener.Addr().String(), 0o600); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable, "-test.run=^TestRequestStartSignalHelper$")
			command.Env = append(os.Environ(),
				"SWAY_SESSION_SIGNAL_TEST_HELPER=1", "SWAY_SESSION_SIGNAL_TEST_CWD="+root,
				"XDG_RUNTIME_DIR="+root, "XDG_STATE_HOME="+filepath.Join(root, "state"),
				"XDG_CONFIG_HOME="+filepath.Join(root, "config"), "XDG_DATA_HOME="+filepath.Join(root, "data"),
				"GORACE=atexit_sleep_ms=0",
			)
			command.WaitDelay = time.Second
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			waited := false
			t.Cleanup(func() {
				if !waited {
					_ = command.Process.Kill()
					<-done
				}
			})
			if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			connection, err := listener.AcceptUnix()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = connection.Close() })
			if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var request sessionrequest.Request
			if err := json.NewDecoder(connection).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if err := request.Validate(); err != nil {
				t.Fatal(err)
			}
			// Receiving the request proves main installed its signal context
			// and the real client is waiting on an established connection.
			if err := command.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				waited = true
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != exitOperation || stdout.Len() != 0 {
					t.Fatalf("signal changed CLI failure contract: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
				}
			case <-time.After(time.Second):
				t.Fatal("signal context kept waiting for the default 100-second broker deadline")
			}
		})
	}
}

func TestRequestStartSignalHelper(t *testing.T) {
	if os.Getenv("SWAY_SESSION_SIGNAL_TEST_HELPER") != "1" {
		return
	}
	os.Args = []string{os.Args[0], "--json", "request-start", "--session", "lab280-signal", "--cwd", os.Getenv("SWAY_SESSION_SIGNAL_TEST_CWD"), "--workspace", "98"}
	main()
}
