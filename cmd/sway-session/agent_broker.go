package main

import (
	"fmt"

	"github.com/marang/sway-session/internal/agentreport"
	sessionstate "github.com/marang/sway-session/internal/session"
)

func startAgentReportBroker(reportError func(error)) (*agentreport.Server, error) {
	socketPath, err := agentreport.DefaultSocketPath()
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
	service := agentreport.RegistryService{StateRoot: stateRoot, HerdrPaths: herdrPaths}
	return agentreport.StartServer(socketPath, service.Handle, func(err error) {
		if reportError != nil {
			reportError(fmt.Errorf("agent session report: %w", err))
		}
	})
}
