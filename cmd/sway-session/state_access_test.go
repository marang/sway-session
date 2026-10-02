package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	sessionstate "github.com/marang/sway-session/internal/session"
	"golang.org/x/sys/unix"
)

func TestRuntimeStartupRefusesRecoveryBeforeCompositorAccess(t *testing.T) {
	for _, runtime := range []struct {
		name string
		run  func(context.Context, string, func(error)) error
	}{
		{"daemon", runSessionDaemon},
		{"standalone-broker", runSessionRequestBroker},
	} {
		for _, pending := range []bool{false, true} {
			name := runtime.name + "/held-gate"
			if pending {
				name = runtime.name + "/pending-recovery"
			}
			t.Run(name, func(t *testing.T) {
				stateHome := t.TempDir()
				setSessionTestStateHome(t, stateHome)
				root := filepath.Join(stateHome, "sway-session")
				access, err := sessionstate.AcquireStateAccess(t.Context(), root, true)
				if err != nil {
					t.Fatal(err)
				}
				if err := access.Close(); err != nil {
					t.Fatal(err)
				}
				want := sessionstate.ErrStateDatabaseBusy
				if pending {
					if err := os.WriteFile(filepath.Join(root, "state-recovery.json"), []byte("{"), 0o600); err != nil {
						t.Fatal(err)
					}
					want = sessionstate.ErrStateRecoveryPending
				} else {
					gate, err := os.OpenFile(filepath.Join(root, ".state-access.lock"), os.O_RDWR, 0)
					if err != nil {
						t.Fatal(err)
					}
					defer gate.Close()
					if err := unix.Flock(int(gate.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
						t.Fatal(err)
					}
				}
				// No compositor exists here: reaching IPC would produce a different
				// error, and all XDG paths are isolated in this test's private root.
				err = runtime.run(t.Context(), filepath.Join(t.TempDir(), "no-compositor.sock"), nil)
				if !errors.Is(err, want) {
					t.Fatalf("startup did not refuse recovery before effects: %v", err)
				}
				if _, err := os.Stat(filepath.Join(root, sessionstate.StateDatabaseFilename)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("rejected startup created a database: %v", err)
				}
			})
		}
	}
}
