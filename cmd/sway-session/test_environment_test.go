package main

import "testing"

// setSessionTestStateHome isolates both halves of sway-session's XDG state:
// the persisted documents and the daemon compatibility lock. Tests that point
// only XDG_STATE_HOME at a temporary directory would otherwise consult the
// user's real running daemon through XDG_RUNTIME_DIR.
func setSessionTestStateHome(t *testing.T, stateHome string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
}
