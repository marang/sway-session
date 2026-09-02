package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestSwayShutdownMonitorStopsOnShutdownEvent(t *testing.T) {
	socket, closeServer := fakeSwayShutdownServer(t, true)
	defer closeServer()
	monitor, err := subscribeToSwayShutdown(t.Context(), socket)
	if err != nil {
		t.Fatalf("subscribeToSwayShutdown returned error: %v", err)
	}
	defer monitor.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := monitor.Wait(ctx); err != nil {
		t.Fatalf("monitor shutdown event: %v", err)
	}
}

func TestSwayShutdownMonitorStopsOnContextCancellation(t *testing.T) {
	socket, closeServer := fakeSwayShutdownServer(t, false)
	defer closeServer()
	monitor, err := subscribeToSwayShutdown(t.Context(), socket)
	if err != nil {
		t.Fatalf("subscribeToSwayShutdown returned error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := monitor.Wait(ctx); err != nil {
		t.Fatalf("cancel monitor: %v", err)
	}
}

func TestSwayShutdownMonitorTreatsReaderCancellationRaceAsCleanShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	monitor := &swayShutdownMonitor{result: make(chan error, 1)}
	monitor.result <- fmt.Errorf("sway ipc read canceled: %w", context.Canceled)

	if err := monitor.Wait(ctx); err != nil {
		t.Fatalf("canceled reader won select race: %v", err)
	}
}

func fakeSwayShutdownServer(t *testing.T, sendShutdown bool) (string, func()) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "sway.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			serverDone <- err
			return
		}
		defer connection.Close()
		messageType, payload, err := readTestSwayFrame(connection)
		if err != nil {
			serverDone <- err
			return
		}
		if messageType != 2 || string(payload) != `["shutdown"]` {
			serverDone <- fmt.Errorf("unexpected subscription type=%d payload=%q", messageType, payload)
			return
		}
		if err := writeTestSwayFrame(connection, 2, []byte(`{"success":true}`)); err != nil {
			serverDone <- err
			return
		}
		if sendShutdown {
			serverDone <- writeTestSwayFrame(connection, 1<<31|6, []byte(`{"change":"exit"}`))
			return
		}
		var one [1]byte
		_, err = connection.Read(one[:])
		if !errors.Is(err, io.EOF) {
			serverDone <- fmt.Errorf("wait for monitor close: %w", err)
			return
		}
		serverDone <- nil
	}()
	return socket, func() {
		_ = listener.Close()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("fake Sway server: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("fake Sway server did not stop")
		}
	}
}

func readTestSwayFrame(reader io.Reader) (uint32, []byte, error) {
	header := make([]byte, 14)
	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, nil, err
	}
	if string(header[:6]) != "i3-ipc" {
		return 0, nil, errors.New("invalid Sway IPC magic")
	}
	length := binary.LittleEndian.Uint32(header[6:10])
	if length > 4096 {
		return 0, nil, errors.New("test Sway IPC payload is too large")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	return binary.LittleEndian.Uint32(header[10:14]), payload, nil
}

func writeTestSwayFrame(writer io.Writer, messageType uint32, payload []byte) error {
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
