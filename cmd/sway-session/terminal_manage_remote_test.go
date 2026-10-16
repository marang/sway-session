package main

import (
	"testing"

	sessionstate "github.com/marang/sway-session/internal/session"
)

func TestRemoteInventoryNeverUsesLocalHerdrObservation(t *testing.T) {
	operations := commandTerminalManageOperations{deps: dependencies{
		herdrPaths: func() (sessionstate.HerdrPaths, error) {
			t.Fatal("remote inventory attempted to resolve local Herdr paths")
			return sessionstate.HerdrPaths{}, nil
		},
	}}
	remote := terminalInventoryResult{
		ContextID: "11111111-1111-4111-8111-111111111111", Session: "same-name",
		Remote: &sessionstate.TerminalRemoteBinding{Target: "fixture", Session: "remote-work"},
	}
	observed := operations.ObserveActivity(t.Context(), []terminalInventoryResult{remote})
	value, ok := observed[remote.ContextID]
	if !ok || value.SessionState != "unknown" || value.AgentState != "unknown" || value.Reason != "remote_not_observed" || value.Directory.Directory != "" || value.Directory.Ambiguous {
		t.Fatalf("remote health inferred from local session: %+v", observed)
	}
}

func TestMixedInventoryKeepsRemoteAndLocalEvidenceSeparate(t *testing.T) {
	local := terminalInventoryResult{ContextID: "11111111-1111-4111-8111-111111111111", Session: "same-name"}
	remote := terminalInventoryResult{
		ContextID: "22222222-2222-4222-8222-222222222222", Session: local.Session,
		Remote: &sessionstate.TerminalRemoteBinding{Target: "fixture", Session: "remote-work"},
	}
	observed := (commandTerminalManageOperations{}).ObserveActivity(t.Context(), []terminalInventoryResult{local, remote})
	if len(observed) != 2 || observed[local.ContextID].Reason != "manager_unavailable" || observed[remote.ContextID].Reason != "remote_not_observed" {
		t.Fatalf("same-name routes shared observation: %+v", observed)
	}
}
