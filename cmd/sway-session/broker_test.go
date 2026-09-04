package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
)

func TestBrokerTerminalManagerRejectsUserOwnedExecutableFromPath(t *testing.T) {
	directory := t.TempDir()
	name := "lab110-user-owned-herdr-probe"
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	manager := brokerTerminalSessionManager()
	if resolved, err := manager.resolveProgram(name); err == nil {
		t.Fatalf("broker accepted user-owned executable %q", resolved)
	}
}

type blockingBrokerSessionManager struct {
	started chan struct{}
	release chan struct{}
	roles   []string
}

func (*blockingBrokerSessionManager) Kind() sessionstate.TerminalSessionManagerKind {
	return sessionstate.TerminalSessionManagerHerdr
}
func (*blockingBrokerSessionManager) ValidateContext(sessionstate.Context) error { return nil }
func (*blockingBrokerSessionManager) BuildProcessSpec(sessionstate.Context, string) (sessionstate.ProcessSpec, error) {
	return sessionstate.ProcessSpec{}, nil
}
func (*blockingBrokerSessionManager) ValidateRoles([]string) error { return nil }
func (manager *blockingBrokerSessionManager) Initialize(_ context.Context, _ sessionstate.Context, roles []string) (sessionstate.TerminalSessionInitialization, error) {
	manager.roles = append([]string(nil), roles...)
	close(manager.started)
	<-manager.release
	return sessionstate.TerminalSessionInitialization{Manager: manager.Kind(), Roles: roles, Initialized: true}, nil
}

func TestSessionRequestInitializationSerializesArchiveAndUsesFixedCodexLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	contextValue := sessionstate.Context{
		ID:       sessionstate.ContextID("8f33d6d0-7c54-4da1-9e38-2bd290ef85ca"),
		State:    sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: "lab-110", Cwd: t.TempDir(), Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}},
	}
	if err := sessionstate.RegistryStoreFor(root).Save(sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue}}); err != nil {
		t.Fatal(err)
	}
	manager := &blockingBrokerSessionManager{started: make(chan struct{}), release: make(chan struct{})}
	initializer := sessionRequestTerminalInitializer{stateRoot: root, manager: manager}
	initialized := make(chan error, 1)
	go func() { initialized <- initializer.Initialize(t.Context(), contextValue) }()
	<-manager.started

	archived := make(chan error, 1)
	go func() {
		_, err := sessionstate.UpdateRegistry(root, func(registry *sessionstate.Registry) error {
			_, err := sessionstate.SetContextState(registry, string(contextValue.ID), sessionstate.ContextArchived)
			return err
		})
		archived <- err
	}()
	select {
	case err := <-archived:
		t.Fatalf("archive bypassed terminal initialization lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(manager.release)
	if err := <-initialized; err != nil {
		t.Fatal(err)
	}
	if err := <-archived; err != nil {
		t.Fatal(err)
	}
	if len(manager.roles) != 2 || manager.roles[0] != "codex" || manager.roles[1] != "shell" {
		t.Fatalf("unexpected broker roles: %v", manager.roles)
	}
}

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
