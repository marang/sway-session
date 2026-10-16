package herdrinit

import (
	"path/filepath"
	"strings"
	"testing"

	sessionstate "github.com/marang/sway-session/internal/session"
)

func TestRemoteInitializationRejectsBeforeDirectoryOrRunnerAccess(t *testing.T) {
	for _, machineID := range []string{"", "0123456789abcdef0123456789abcdef"} {
		t.Run("machine="+machineID, func(t *testing.T) {
			value := testContext(t)
			value.Launcher.Terminal.Remote = &sessionstate.TerminalRemoteBinding{Target: "fixture", Session: "remote-work", MachineID: machineID}
			value.Launcher.Cwd = filepath.Join(t.TempDir(), "absent")
			runner := &fakeRunner{}
			result, err := Initialize(t.Context(), value, []string{"codex", "shell"}, runner)
			if err == nil || !strings.Contains(err.Error(), "remote terminal") || result.Initialized || len(runner.calls) != 0 {
				t.Fatalf("remote initialization touched local state: result=%+v err=%v calls=%v", result, err, runner.calls)
			}
		})
	}
}
