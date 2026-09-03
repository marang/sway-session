package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/marang/sway-title-animator/internal/herdrinit"
	sessionstate "github.com/marang/sway-title-animator/internal/session"
	"github.com/marang/sway-title-animator/internal/sessionrequest"
	"github.com/marang/sway-title-animator/internal/swayipc"
)

func codexSessionRoles() []string { return []string{"codex", "shell"} }

func brokerTerminalSessionManager() herdrTerminalSessionManager {
	return herdrTerminalSessionManager{
		paths:           sessionstate.DefaultHerdrPaths,
		validateHistory: sessionstate.ValidateHerdrPaneHistory,
		resolveProgram:  sessionstate.ResolveRootOwnedSystemExecutable,
		initialize:      herdrinit.Initialize,
	}
}

type sessionRequestTerminalInitializer struct {
	stateRoot string
	manager   sessionstate.TerminalSessionManager
}

func (initializer sessionRequestTerminalInitializer) Initialize(ctx context.Context, requested sessionstate.Context) error {
	if initializer.manager == nil {
		return errors.New("terminal session initializer is unavailable")
	}
	return sessionstate.WithTerminalLifecycleLockContext(ctx, initializer.stateRoot, func() error {
		return sessionstate.InspectRegistryLockedContext(ctx, initializer.stateRoot, func(registry sessionstate.Registry) error {
			index, err := sessionstate.ResolveContext(registry, string(requested.ID))
			if err != nil {
				return err
			}
			current := registry.Contexts[index]
			if current.State != sessionstate.ContextActive || current.Launcher.Kind != sessionstate.LauncherHerdr ||
				current.Launcher.Session != requested.Launcher.Session || current.Launcher.Cwd != requested.Launcher.Cwd {
				return errors.New("requested terminal context changed before initialization")
			}
			_, err = initializer.manager.Initialize(ctx, current, codexSessionRoles())
			return err
		})
	})
}

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
	reporter := newDiagnosticErrorReporter(stderr, structured, "broker_request", "reject session broker request")
	if err := deps.runBroker(ctx, socket, reporter.Report); err != nil {
		return commandResult{}, failure("broker", "run session broker", err.Error())
	}
	return commandResult{Command: "broker", Contexts: []sessionstate.Context{}}, nil
}

func runSessionRequestBroker(ctx context.Context, swaySocket string, reportError func(error)) error {
	if ctx == nil {
		return errors.New("broker context is nil")
	}
	monitor, err := subscribeToSwayShutdown(ctx, swaySocket)
	if err != nil {
		return err
	}
	defer monitor.Close()
	server, err := startSessionRequestBroker(swaySocket, reportError)
	if err != nil {
		return err
	}
	waitErr := monitor.Wait(ctx)
	closeErr := server.Close()
	return errors.Join(waitErr, closeErr)
}

func startSessionRequestBroker(swaySocket string, reportError func(error)) (*sessionrequest.Server, error) {
	stateRoot, err := sessionstate.DefaultStateRoot()
	if err != nil {
		return nil, err
	}
	socketPath, err := sessionrequest.DefaultSocketPath()
	if err != nil {
		return nil, err
	}
	restoreExecutable, err := sessionstate.ResolveRootOwnedSystemExecutable("sway-session")
	if err != nil {
		return nil, err
	}
	service := &sessionrequest.Service{
		StateRoot:    stateRoot,
		NewContextID: sessionstate.NewContextID,
		NewSway:      func() sessionrequest.SwayRequester { return swayipc.NewClient(swaySocket) },
		Restore:      sessionrequest.ExecRestoreRunner{Executable: restoreExecutable, SwaySocket: swaySocket},
		Now:          time.Now,
		Initializer: sessionRequestTerminalInitializer{
			stateRoot: stateRoot,
			manager:   brokerTerminalSessionManager(),
		},
	}
	return sessionrequest.StartServer(socketPath, service.Handle, reportError)
}

type swayShutdownMonitor struct {
	connection *swayipc.Conn
	result     chan error
}

func subscribeToSwayShutdown(ctx context.Context, socket string) (*swayShutdownMonitor, error) {
	connection, err := swayipc.OpenSubscriptionContext(ctx, socket, []byte(`["shutdown"]`), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("open Sway shutdown subscription: %w", err)
	}
	monitor := &swayShutdownMonitor{connection: connection, result: make(chan error, 1)}
	go func() {
		message, err := connection.ReadContext(ctx)
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
		// The reader uses the same lifecycle context. If its cancellation
		// result wins this select race, shutdown is still intentional.
		if ctx.Err() != nil {
			return nil
		}
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
