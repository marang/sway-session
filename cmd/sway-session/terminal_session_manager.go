package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/marang/sway-title-animator/internal/herdrinit"
	sessionstate "github.com/marang/sway-title-animator/internal/session"
)

const herdrInitializationTimeout = 90 * time.Second

var (
	errHerdrSessionPath = errors.New("herdr session path validation failed")
	errHerdrPaneHistory = errors.New("herdr pane history validation failed")
	errHerdrExecutable  = errors.New("herdr executable resolution failed")
)

type herdrTerminalSessionManager struct {
	paths           func() (sessionstate.HerdrPaths, error)
	validateHistory func(sessionstate.HerdrPaths) error
	resolveProgram  func(string) (string, error)
	initialize      func(context.Context, sessionstate.Context, []string, herdrinit.Runner) (herdrinit.Result, error)
}

func terminalSessionManager(kind sessionstate.TerminalSessionManagerKind, deps dependencies) (sessionstate.TerminalSessionManager, error) {
	switch kind {
	case sessionstate.TerminalSessionManagerHerdr:
		return herdrTerminalSessionManager{
			paths:           deps.herdrPaths,
			validateHistory: deps.validateHistory,
			resolveProgram:  deps.resolveProgram,
			initialize:      deps.initializeHerdr,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported terminal session manager %q; supported values: herdr", kind)
	}
}

func terminalSessionManagerForContext(contextValue sessionstate.Context, deps dependencies) (sessionstate.TerminalSessionManager, error) {
	switch contextValue.Launcher.Kind {
	case sessionstate.LauncherHerdr:
		return terminalSessionManager(sessionstate.TerminalSessionManagerHerdr, deps)
	default:
		return nil, fmt.Errorf("context %s uses unsupported terminal session manager %q", contextValue.ID, contextValue.Launcher.Kind)
	}
}

func (herdrTerminalSessionManager) Kind() sessionstate.TerminalSessionManagerKind {
	return sessionstate.TerminalSessionManagerHerdr
}

func (manager herdrTerminalSessionManager) ValidateContext(contextValue sessionstate.Context) error {
	if manager.paths == nil {
		return errors.New("herdr path dependency is unavailable")
	}
	if contextValue.Launcher.Kind != sessionstate.LauncherHerdr || contextValue.Launcher.Terminal == nil {
		return errors.New("herdr terminal session manager requires a Herdr terminal context")
	}
	paths, err := manager.paths()
	if err != nil {
		return fmt.Errorf("%w: resolve paths: %v", errHerdrSessionPath, err)
	}
	if err := sessionstate.ValidateHerdrSessionSocketPaths(paths.Root, contextValue.Launcher.Session); err != nil {
		return fmt.Errorf("%w: %v", errHerdrSessionPath, err)
	}
	return nil
}

func (manager herdrTerminalSessionManager) BuildProcessSpec(contextValue sessionstate.Context, terminalExecutable string) (sessionstate.ProcessSpec, error) {
	if err := manager.ValidateContext(contextValue); err != nil {
		return sessionstate.ProcessSpec{}, err
	}
	if manager.validateHistory == nil || manager.resolveProgram == nil {
		return sessionstate.ProcessSpec{}, errors.New("herdr launch dependencies are incomplete")
	}
	paths, err := manager.paths()
	if err != nil {
		return sessionstate.ProcessSpec{}, fmt.Errorf("resolve Herdr paths: %w", err)
	}
	if err := manager.validateHistory(paths); err != nil {
		return sessionstate.ProcessSpec{}, fmt.Errorf("%w: %v", errHerdrPaneHistory, err)
	}
	executable, err := manager.resolveProgram("herdr")
	if err != nil {
		return sessionstate.ProcessSpec{}, fmt.Errorf("%w: %v", errHerdrExecutable, err)
	}
	return sessionstate.BuildTerminalProcessSpec(contextValue, terminalExecutable, executable, paths.ConfigFile)
}

func (herdrTerminalSessionManager) ValidateRoles(roles []string) error {
	return herdrinit.ValidateRoles(roles)
}

func (manager herdrTerminalSessionManager) Initialize(ctx context.Context, contextValue sessionstate.Context, roles []string) (sessionstate.TerminalSessionInitialization, error) {
	result := sessionstate.TerminalSessionInitialization{
		Manager: manager.Kind(),
		Roles:   append([]string(nil), roles...),
	}
	if err := manager.ValidateContext(contextValue); err != nil {
		return result, err
	}
	if manager.resolveProgram == nil || manager.initialize == nil {
		return result, errors.New("herdr initialization dependencies are incomplete")
	}
	paths, err := manager.paths()
	if err != nil {
		return result, fmt.Errorf("resolve Herdr paths: %w", err)
	}
	executable, err := manager.resolveProgram("herdr")
	if err != nil {
		return result, fmt.Errorf("resolve Herdr executable: %w", err)
	}
	initializationContext, cancel := context.WithTimeout(ctx, herdrInitializationTimeout)
	defer cancel()
	initialized, err := manager.initialize(initializationContext, contextValue, roles, herdrinit.ExecRunner{
		Executable: executable,
		ConfigFile: paths.ConfigFile,
	})
	result.Initialized = initialized.Initialized
	result.Reason = initialized.Reason
	return result, err
}
