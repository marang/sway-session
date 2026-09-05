package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

const (
	desktopCatalogRefreshInterval = time.Minute
	stateContentionRetryInterval  = 50 * time.Millisecond
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
	stateRoot, err := sessionstate.DefaultStateRoot()
	if err != nil {
		return err
	}
	if err := initializeContextRegistry(ctx, stateRoot); err != nil {
		return fmt.Errorf("initialize context registry: %w", err)
	}
	compositorID, err := compositorIdentity(swaySocket)
	if err != nil {
		return fmt.Errorf("identify Sway compositor session: %w", err)
	}
	search, catalogConfigErr := sessionstate.DefaultDesktopSearchPath()
	catalogCache := sessionstate.NewDesktopCatalogCache(search)
	refreshCatalog := newRefreshingDesktopCatalogLoader(catalogCache, time.Now, desktopCatalogRefreshInterval)
	indicatorCatalog := func() (sessionstate.DesktopCatalog, error) {
		if catalogConfigErr != nil {
			return sessionstate.DesktopCatalog{}, catalogConfigErr
		}
		return refreshCatalog()
	}
	operationStore, operationStoreErr := sessionstate.DefaultApplicationOperationStore()
	indicatorOperations := func() ([]sessionstate.ApplicationOperation, error) {
		if operationStoreErr != nil {
			return nil, operationStoreErr
		}
		return operationStore.ActiveContext(ctx)
	}
	eventStreamState := &swayipc.EventStreamState{}
	runtime, err := newSessionRuntimeWithOptions(control, sessionRuntimeOptions{
		Context:             ctx,
		EventStreamState:    eventStreamState,
		Root:                stateRoot,
		CompositorID:        compositorID,
		StartedAt:           time.Now(),
		ApplicationLauncher: daemonApplicationLauncher{stateRoot: stateRoot},
		ApplicationRestore: sessionstate.ApplicationRestoreOptions{
			AdoptionGrace: applicationAdoptionGrace,
			CloseGrace:    applicationCloseGrace,
			LaunchTimeout: applicationLaunchTimeout,
			MaxConcurrent: 2,
		},
		IndicatorCatalog:    indicatorCatalog,
		IndicatorOperations: indicatorOperations,
	})
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
	agentBroker, err := startAgentReportBroker(reportError)
	if err != nil && reportError != nil {
		reportError(fmt.Errorf("start agent session reporter: %w", err))
	}
	if agentBroker != nil {
		defer func() {
			if err := agentBroker.Close(); err != nil && reportError != nil {
				reportError(fmt.Errorf("stop agent session reporter: %w", err))
			}
		}()
	}
	// Keep the v1 endpoint for installed Codex hooks; it uses the shared reporter.
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
	go swayipc.StreamSessionEventsWithState(swaySocket, events, done, eventStreamState)
	return runSessionDaemonLoop(ctx, control, runtime, events, reportError)
}

func initializeContextRegistry(ctx context.Context, root string) error {
	return retryStateContention(ctx, func() error {
		_, err := sessionstate.UpdateRegistryContext(ctx, root, func(*sessionstate.Registry) error { return nil })
		return err
	})
}

func retryStateContention(ctx context.Context, action func() error) error {
	for {
		err := action()
		if err == nil || !sessionstate.IsStateDatabaseBusy(err) {
			return err
		}
		timer := time.NewTimer(stateContentionRetryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func newRefreshingDesktopCatalogLoader(
	cache *sessionstate.DesktopCatalogCache,
	now func() time.Time,
	interval time.Duration,
) func() (sessionstate.DesktopCatalog, error) {
	if now == nil {
		now = time.Now
	}
	if interval <= 0 {
		interval = desktopCatalogRefreshInterval
	}
	loadedAt := time.Time{}
	return func() (sessionstate.DesktopCatalog, error) {
		current := now()
		if loadedAt.IsZero() || !current.Before(loadedAt.Add(interval)) {
			cache.Invalidate()
		}
		catalog, err := cache.Load()
		if err == nil && (loadedAt.IsZero() || !current.Before(loadedAt.Add(interval))) {
			loadedAt = current
		}
		return catalog, err
	}
}

type daemonApplicationLauncher struct {
	stateRoot string
}

type preparedDesktopApplicationLaunch struct {
	starter sessionstate.ProcessStarter
	spec    sessionstate.ProcessSpec
}

func (launch preparedDesktopApplicationLaunch) Start() error {
	return launch.starter.Start(launch.spec)
}

func (launcher daemonApplicationLauncher) Prepare(ctx context.Context, context sessionstate.Context) (preparedApplicationLaunch, error) {
	starter := sessionstate.ExecProcessStarter{}
	adapter := sessionstate.DesktopApplicationLauncher{
		StateRoot: launcher.stateRoot,
		Starter:   starter,
	}
	switch context.Launcher.Kind {
	case sessionstate.LauncherDesktop:
		gio, err := sessionstate.ResolveRootOwnedSystemExecutable("gio")
		if err != nil {
			return nil, err
		}
		adapter.GIO = gio
	case sessionstate.LauncherFlatpak:
		flatpak, err := sessionstate.ResolveRootOwnedSystemExecutable("flatpak")
		if err != nil {
			return nil, err
		}
		adapter.Flatpak = flatpak
		if err := sessionstate.VerifyFlatpakInstallationContext(ctx, flatpak, context.Launcher, sessionstate.ExecCommandRunner{}); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported desktop application launcher kind %q", context.Launcher.Kind)
	}
	spec, err := adapter.SpecContext(ctx, context)
	if err != nil {
		return nil, err
	}
	return preparedDesktopApplicationLaunch{starter: starter, spec: spec}, nil
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

	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-events:
			if event.Type == swayipc.EventShutdown {
				return nil
			}
			runtime.HandleEvent(event, time.Now())
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
