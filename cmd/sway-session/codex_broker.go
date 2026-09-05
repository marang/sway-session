package main

import (
	"fmt"

	"github.com/marang/sway-session/internal/codexreport"
	sessionstate "github.com/marang/sway-session/internal/session"
)

func startCodexReportBroker(reportError func(error)) (*codexreport.Server, error) {
	socketPath, err := codexreport.DefaultSocketPath()
	if err != nil {
		return nil, err
	}
	stateRoot, err := sessionstate.DefaultStateRoot()
	if err != nil {
		return nil, err
	}
	herdrPaths, err := sessionstate.DefaultHerdrPaths()
	if err != nil {
		return nil, err
	}
	service := codexreport.RegistryService{StateRoot: stateRoot, HerdrPaths: herdrPaths}
	return codexreport.StartServer(socketPath, service.Handle, func(err error) {
		if reportError != nil {
			reportError(fmt.Errorf("secure Codex session report: %w", err))
		}
	})
}
