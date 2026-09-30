package session

import "testing"

func TestRestorePolicyMatchesActiveLayoutMembership(t *testing.T) {
	for _, state := range []ContextState{ContextActive, ContextArchived} {
		terminal := validRegistry().Contexts[0]
		terminal.State = state
		decision := EvaluateRestorePolicy(terminal)
		if decision.Eligible != (state == ContextActive) {
			t.Fatalf("terminal: %+v", decision)
		}
		for _, policy := range []ApplicationRestorePolicy{ApplicationRestoreFollow, ApplicationRestorePinned} {
			for _, open := range []bool{false, true} {
				app := desktopApplicationContext("example.desktop", "example.app")
				app.State, app.App.DesiredOpen, app.App.RestorePolicy = state, open, policy
				decision := EvaluateRestorePolicy(app)
				want := state == ContextActive && open
				if decision.Eligible != want {
					t.Fatalf("state=%s policy=%s desired=%t: %+v", state, policy, open, decision)
				}
				registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{app}}
				_, present := activeContextIDs(registry)[app.ID]
				if present != decision.Eligible {
					t.Fatal("layout membership disagrees with policy")
				}
				if RestorePolicyExplanation(decision.Reason) == "restore policy unknown" {
					t.Fatal("missing explanation")
				}
			}
		}
	}
}
