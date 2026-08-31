package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
	"github.com/marang/sway-title-animator/internal/swayipc"
)

func executeDaemon(ctx context.Context, arguments []string, stderr io.Writer, structured bool, deps dependencies) (commandResult, *commandFailure) {
	set := newFlagSet("daemon")
	socketFlag := set.String("socket", "", "Sway IPC socket")
	if err := set.Parse(arguments); err != nil || set.NArg() != 0 {
		return commandResult{}, usageFailure("daemon", "daemon accepts only an optional --socket")
	}
	socket := *socketFlag
	if socket == "" {
		socket = os.Getenv("SWAYSOCK")
	}
	if socket == "" || !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return commandResult{}, failure("sway_socket", "a valid absolute Sway IPC socket is required", "Run inside Sway or pass --socket PATH.")
	}
	if deps.runDaemon == nil {
		return commandResult{}, failure("daemon", "run session daemon", "Session daemon dependency is unavailable.")
	}
	reporter := newDiagnosticErrorReporter(stderr, structured, "daemon_runtime", "persistent session daemon")
	if err := deps.runDaemon(ctx, socket, reporter.Report); err != nil {
		return commandResult{}, failure("daemon", "run session daemon", err.Error())
	}
	return commandResult{Command: "daemon", Contexts: []sessionstate.Context{}}, nil
}

func runSessionDaemon(ctx context.Context, swaySocket string, reportError func(error)) error {
	if ctx == nil {
		return errors.New("daemon context is nil")
	}
	lock, err := acquireSessionDaemonLock()
	if err != nil {
		return err
	}
	defer lock.Close()

	control := swayipc.NewClient(swaySocket)
	defer control.Close()
	runtime, err := newSessionRuntime(control)
	if err != nil {
		return err
	}
	defer runtime.Shutdown()

	sessionBroker, err := startSessionRequestBroker(swaySocket, reportError)
	if err != nil && reportError != nil {
		reportError(fmt.Errorf("start typed session request broker: %w", err))
	}
	if sessionBroker != nil {
		defer func() {
			if err := sessionBroker.Close(); err != nil && reportError != nil {
				reportError(fmt.Errorf("stop typed session request broker: %w", err))
			}
		}()
	}
	codexBroker, err := startCodexReportBroker(reportError)
	if err != nil && reportError != nil {
		reportError(fmt.Errorf("start secure Codex session reporter: %w", err))
	}
	if codexBroker != nil {
		defer func() {
			if err := codexBroker.Close(); err != nil && reportError != nil {
				reportError(fmt.Errorf("stop secure Codex session reporter: %w", err))
			}
		}()
	}

	events := make(chan swayipc.Event, 16)
	done := make(chan struct{})
	defer close(done)
	go swayipc.StreamEvents(swaySocket, events, done)
	return runSessionDaemonLoop(ctx, control, runtime, events, reportError)
}

func runSessionDaemonLoop(
	ctx context.Context,
	control swayRequester,
	runtime *sessionRuntime,
	events <-chan swayipc.Event,
	reportError func(error),
) error {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	var timerChannel <-chan time.Time
	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		deadline, scheduled := runtime.Deadline()
		if !scheduled {
			timerChannel = nil
			return
		}
		duration := time.Until(deadline)
		if duration < 0 {
			duration = 0
		}
		timer.Reset(duration)
		timerChannel = timer.C
	}

	reconcilePersistentSession(control, runtime, reportError)
	resetTimer()
	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-events:
			if event.Type == swayipc.EventShutdown {
				return nil
			}
			if event.AffectsSessionLayout() {
				reconcilePersistentSession(control, runtime, reportError)
				resetTimer()
			}
		case now := <-timerChannel:
			if runtime.ObservationDue(now) {
				runtime.PostponeObservation(now)
				reconcilePersistentSession(control, runtime, reportError)
			}
			if runtime.StartupDue(now) {
				reconcilePersistentSession(control, runtime, reportError)
				if runtime.StartupDue(time.Now()) {
					runtime.PostponeStartup(time.Now())
				}
			}
			if err := runtime.Flush(now); err != nil && reportError != nil {
				reportError(err)
			}
			resetTimer()
		}
	}
}
