package session

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteTerminalPurgeCannotReserveOrDeleteLocalNamesake(t *testing.T) {
	for _, machineID := range []string{"", "0123456789abcdef0123456789abcdef"} {
		t.Run("machine="+machineID, func(t *testing.T) {
			value := validRegistry().Contexts[0]
			value.Launcher.Terminal.Remote = &TerminalRemoteBinding{Target: "fixture", Session: "remote-work", MachineID: machineID}
			if err := value.Validate(); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			herdrRoot := filepath.Join(root, "herdr")
			namesake := filepath.Join(herdrRoot, "sessions", value.Launcher.Session)
			if err := os.MkdirAll(namesake, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(namesake, "owned-local-state")
			if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			stateRoot := filepath.Join(root, "state")
			if _, err := StartTerminalPurgeContext(t.Context(), stateRoot, value, herdrRoot, time.Now()); err == nil || !strings.Contains(err.Error(), "remote terminal") {
				t.Fatalf("remote purge was not refused before intent: %v", err)
			}
			if _, err := os.Stat(stateRoot); !os.IsNotExist(err) {
				t.Fatalf("remote purge opened state: %v", err)
			}
			// Deliberately incomplete pin: a remote refusal must precede local
			// identity probing or any manager stop/delete operation.
			target := LifecyclePurgeTarget{Root: herdrRoot}
			if err := (HerdrManager{}).DeletePurgeTarget(t.Context(), value, target); err == nil || !strings.Contains(err.Error(), "remote terminal") {
				t.Fatalf("remote deleter reached local path: %v", err)
			}
			operation := LifecycleOperation{Phase: LifecycleForward, Before: []Context{value}, Purge: &target}
			if err := validateLifecyclePurge(operation); err == nil || !strings.Contains(err.Error(), "remote terminal") {
				t.Fatalf("durable remote purge was accepted: %v", err)
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "preserve" {
				t.Fatalf("local namesake changed: %q %v", data, err)
			}
		})
	}
}

func TestRemoteAgentReportDoesNotConnectToLocalNamesake(t *testing.T) {
	root := privateHerdrRoot(t)
	value := validRegistry().Contexts[0]
	value.Launcher.Terminal.Remote = &TerminalRemoteBinding{Target: "fixture", Session: "remote-work"}
	directory := filepath.Join(root, "sessions", value.Launcher.Session)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(directory, herdrAgentSocketName), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	err = ReportHerdrAgentSession(t.Context(), HerdrPaths{Root: root}, value.Launcher, "w1:p1", "codex", "01a04a4b-7fb9-7a90-8ace-51f7ae68e0ee", "", os.Getpid(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "remote terminal") {
		t.Fatalf("remote report was not refused: %v", err)
	}
	connection, acceptErr := listener.AcceptUnix()
	if connection != nil {
		_ = connection.Close()
		t.Fatal("remote report connected to local namesake")
	}
	if networkError, ok := acceptErr.(net.Error); !ok || !networkError.Timeout() {
		t.Fatalf("unexpected listener result: %v", acceptErr)
	}
}
