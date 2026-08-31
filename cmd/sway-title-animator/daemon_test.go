package main

import (
	"encoding/binary"
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

	"github.com/marang/sway-title-animator/internal/swayipc"
)

func TestSubscribeTreatsMissingFixedEndpointAsShutdown(t *testing.T) {
	events := make(chan swayipc.Event, 1)
	done := make(chan struct{})
	defer close(done)
	socket := filepath.Join(t.TempDir(), "missing.sock")
	go swayipc.StreamEvents(socket, events, done)

	select {
	case event := <-events:
		if event.Type != swayipc.EventShutdown || event.Change != "endpoint-gone" {
			t.Fatalf("unexpected terminal event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing fixed Sway endpoint did not stop the subscriber")
	}
}

func TestAnimatorRunsWithoutSessionStateOrDaemon(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	configHome := filepath.Join(t.TempDir(), "config")
	for _, path := range []string{stateHome, runtimeRoot, configHome} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("create isolated root: %v", err)
		}
	}
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	socket, stop := fakeAnimatorSwayServer(t)
	defer stop()
	if code := runLoopWithFPS(socket, 100); code != 0 {
		t.Fatalf("animator exited with status %d", code)
	}
	for _, forbidden := range []string{
		filepath.Join(stateHome, "sway-session"),
		filepath.Join(runtimeRoot, "sway-session"),
		filepath.Join(configHome, "herdr"),
	} {
		if _, err := os.Lstat(forbidden); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("animator touched session-owned path %s: %v", forbidden, err)
		}
	}
}

func fakeAnimatorSwayServer(t *testing.T) (string, func()) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "sway.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen on fake Sway socket: %v", err)
	}
	titleApplied := make(chan struct{})
	serverErrors := make(chan error, 4)
	var titleOnce sync.Once
	var handlers sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for range 2 {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				serverErrors <- acceptErr
				return
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer connection.Close()
				serverErrors <- serveAnimatorConnection(connection, titleApplied, &titleOnce)
			}()
		}
	}()
	return socket, func() {
		_ = listener.Close()
		<-acceptDone
		handlers.Wait()
		close(serverErrors)
		for err := range serverErrors {
			if err != nil {
				t.Errorf("fake Sway server: %v", err)
			}
		}
	}
}

func serveAnimatorConnection(connection net.Conn, titleApplied chan struct{}, titleOnce *sync.Once) error {
	for {
		messageType, payload, err := readAnimatorTestFrame(connection)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch messageType {
		case uint32(swayipc.Subscribe):
			if string(payload) != `["window","workspace","shutdown"]` {
				return fmt.Errorf("unexpected subscription %q", payload)
			}
			if err := writeAnimatorTestFrame(connection, uint32(swayipc.Subscribe), []byte(`{"success":true}`)); err != nil {
				return err
			}
			select {
			case <-titleApplied:
			case <-time.After(time.Second):
				return errors.New("animator did not apply a title")
			}
			return writeAnimatorTestFrame(connection, 1<<31|6, []byte(`{"change":"exit"}`))
		case uint32(swayipc.GetTree):
			if err := writeAnimatorTestFrame(connection, uint32(swayipc.GetTree), []byte(animatorTestTree)); err != nil {
				return err
			}
		case uint32(swayipc.RunCommand):
			if !strings.Contains(string(payload), "title_format") {
				return fmt.Errorf("animator issued non-title command %q", payload)
			}
			if err := writeAnimatorTestFrame(connection, uint32(swayipc.RunCommand), []byte(`[{"success":true}]`)); err != nil {
				return err
			}
			titleOnce.Do(func() { close(titleApplied) })
		default:
			return fmt.Errorf("unexpected message type %d", messageType)
		}
	}
}

const animatorTestTree = `{
  "id": 1,
  "name": "root",
  "type": "root",
  "nodes": [{
    "id": 2,
    "name": "1",
    "type": "workspace",
    "layout": "splith",
    "rect": {"width": 1000, "height": 800},
    "nodes": [{
      "id": 42,
      "name": "Independent animator",
      "type": "con",
      "focused": true,
      "app_id": "test.app",
      "rect": {"width": 800, "height": 600},
      "deco_rect": {"width": 800, "height": 20},
      "nodes": [],
      "floating_nodes": []
    }],
    "floating_nodes": []
  }],
  "floating_nodes": []
}`

func readAnimatorTestFrame(reader io.Reader) (uint32, []byte, error) {
	header := make([]byte, 14)
	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, nil, err
	}
	if string(header[:6]) != "i3-ipc" {
		return 0, nil, errors.New("invalid Sway IPC magic")
	}
	length := binary.LittleEndian.Uint32(header[6:10])
	if length > 64*1024 {
		return 0, nil, errors.New("test Sway IPC payload is too large")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	return binary.LittleEndian.Uint32(header[10:14]), payload, nil
}

func writeAnimatorTestFrame(writer io.Writer, messageType uint32, payload []byte) error {
	header := make([]byte, 14)
	copy(header, "i3-ipc")
	binary.LittleEndian.PutUint32(header[6:10], uint32(len(payload)))
	binary.LittleEndian.PutUint32(header[10:14], messageType)
	if _, err := writer.Write(header); err != nil {
		return err
	}
	_, err := writer.Write(payload)
	return err
}
