package agentreport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"golang.org/x/sys/unix"
)

func TestRegistryServiceRetainsStateAccessThroughHerdrReport(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	id := sessionstate.ContextID("123e4567-e89b-12d3-a456-426614174000")
	registered := sessionstate.Context{
		ID: id, State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: "guard-test", Cwd: t.TempDir(), Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}},
	}
	if err := sessionstate.RegistryStoreFor(root).SaveContext(t.Context(), sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{registered}}); err != nil {
		t.Fatal(err)
	}
	checked := false
	service := RegistryService{StateRoot: root, Report: func(context.Context, sessionstate.HerdrPaths, sessionstate.Launcher, string, string, string, string, int, time.Time) error {
		gate, err := os.OpenFile(filepath.Join(root, ".state-access.lock"), os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer gate.Close()
		err = unix.Flock(int(gate.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			t.Fatalf("Herdr effect after database close permitted recovery: %v", err)
		}
		checked = true
		return nil
	}}
	report := Report{Version: ProtocolVersion, ContextID: id, PaneID: "work:p1", Agent: "claude", AgentSessionID: "thread-1", EventOrigin: "resume", PeerPID: 4242}
	if err := service.Handle(t.Context(), report); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("Herdr reporting was not exercised")
	}
}
