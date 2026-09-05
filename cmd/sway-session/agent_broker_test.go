package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/marang/sway-session/internal/agentreport"
)

func TestAgentBrokerBootstrapsOnlyGenericEndpoint(t *testing.T) {
	root := t.TempDir()
	runtimeRoot := filepath.Join(root, "runtime")
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))

	broker, err := startAgentReportBroker(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()

	runtimeDirectory := filepath.Join(runtimeRoot, "sway-session")
	genericPath := filepath.Join(runtimeDirectory, agentreport.SocketFilename)
	info, err := os.Lstat(genericPath)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("generic agent endpoint was not bootstrapped: info=%v err=%v", info, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("generic agent endpoint mode = %o, want 0600", info.Mode().Perm())
	}
	if _, err := os.Lstat(filepath.Join(runtimeDirectory, "codex-report.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy Codex endpoint was bootstrapped: %v", err)
	}
}
