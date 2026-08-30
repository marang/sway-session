package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/marang/sway-title-animator/internal/diagnostic"
	sessionstate "github.com/marang/sway-title-animator/internal/session"
	"github.com/marang/sway-title-animator/internal/sessionrequest"
	"github.com/marang/sway-title-animator/internal/swayipc"
)

func executeBroker(ctx context.Context, arguments []string, stderr io.Writer, structured bool, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("broker")
	socketFlag := set.String("socket", "", "Sway IPC socket")
	if err := set.Parse(arguments); err != nil || set.NArg() != 0 {
		return commandResult{}, usageFailure("broker", "broker accepts only an optional --socket")
	}
	socket := *socketFlag
	if socket == "" {
		socket = os.Getenv("SWAYSOCK")
	}
	if socket == "" || !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return commandResult{}, failure("sway_socket", "a valid absolute Sway IPC socket is required", "Run inside Sway or pass --socket PATH.")
	}
	if deps.runBroker == nil {
		return commandResult{}, failure("broker", "run session broker", "Session broker dependency is unavailable.")
	}
	report := func(err error) {
		if err == nil {
			return
		}
		_ = diagnostic.WriteAll(stderr, "sway-session", []diagnostic.Diagnostic{{
			Level: diagnostic.LevelError, Code: "broker_request", Message: "reject session broker request", Hint: err.Error(),
		}}, structured)
	}
	if err := deps.runBroker(ctx, socket, report); err != nil {
		return commandResult{}, failure("broker", "run session broker", err.Error())
	}
	return commandResult{Command: "broker", Contexts: []sessionstate.Context{}}, nil
}

func runSessionRequestBroker(ctx context.Context, swaySocket string, reportError func(error)) error {
	if ctx == nil {
		return errors.New("broker context is nil")
	}
	stateRoot, err := sessionstate.DefaultStateRoot()
	if err != nil {
		return err
	}
	socketPath, err := sessionrequest.DefaultSocketPath()
	if err != nil {
		return err
	}
	restoreExecutable, err := sessionstate.ResolveRootOwnedSystemExecutable("sway-session")
	if err != nil {
		return err
	}
	monitor, err := subscribeToSwayShutdown(swaySocket)
	if err != nil {
		return err
	}
	defer monitor.Close()
	service := &sessionrequest.Service{
		StateRoot:    stateRoot,
		NewContextID: sessionstate.NewContextID,
		NewSway:      func() sessionrequest.SwayRequester { return swayipc.NewClient(swaySocket) },
		Restore:      sessionrequest.ExecRestoreRunner{Executable: restoreExecutable, SwaySocket: swaySocket},
	}
	server, err := sessionrequest.StartServer(socketPath, service.Handle, reportError)
	if err != nil {
		return err
	}
	waitErr := monitor.Wait(ctx)
	closeErr := server.Close()
	return errors.Join(waitErr, closeErr)
}

type swayShutdownMonitor struct {
	connection *swayipc.Conn
	result     chan error
}

func subscribeToSwayShutdown(socket string) (*swayShutdownMonitor, error) {
	connection, err := swayipc.Dial(socket)
	if err != nil {
		return nil, fmt.Errorf("connect to Sway shutdown events: %w", err)
	}
	response, err := connection.Request(swayipc.Subscribe, []byte(`["shutdown"]`))
	if err == nil {
		err = swayipc.CheckSubscribeResponse(response)
	}
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("subscribe to Sway shutdown events: %w", err)
	}
	monitor := &swayShutdownMonitor{connection: connection, result: make(chan error, 1)}
	go func() {
		message, err := connection.Read()
		if err == nil {
			var event swayipc.Event
			event, err = swayipc.DecodeEvent(message)
			if err == nil && event.Type != swayipc.EventShutdown {
				err = fmt.Errorf("unexpected Sway event type %q", event.Type)
			}
		}
		monitor.result <- err
	}()
	return monitor, nil
}

func (monitor *swayShutdownMonitor) Wait(ctx context.Context) error {
	select {
	case err := <-monitor.result:
		if err != nil {
			return fmt.Errorf("monitor Sway shutdown: %w", err)
		}
		return nil
	case <-ctx.Done():
		_ = monitor.Close()
		<-monitor.result
		return nil
	}
}

func (monitor *swayShutdownMonitor) Close() error {
	if monitor == nil || monitor.connection == nil {
		return nil
	}
	err := monitor.connection.Close()
	monitor.connection = nil
	return err
}
