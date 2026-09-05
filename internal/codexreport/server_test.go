package codexreport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

func TestServerAllowsOnlyValidatedSessionReports(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", SocketFilename)
	var lock sync.Mutex
	var received []Report
	server, err := StartServer(socketPath, func(_ context.Context, report Report) error {
		lock.Lock()
		defer lock.Unlock()
		received = append(received, report)
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	report := Report{Version: ProtocolVersion, ContextID: testContextID, PaneID: "work:p1", CodexSessionID: testSessionID}
	if err := Send(ctx, socketPath, report); err != nil {
		t.Fatal(err)
	}
	lock.Lock()
	defer lock.Unlock()
	if len(received) != 1 || received[0].Version != report.Version || received[0].ContextID != report.ContextID || received[0].PaneID != report.PaneID || received[0].CodexSessionID != report.CodexSessionID || received[0].PeerPID <= 0 {
		t.Fatalf("unexpected reports: %+v", received)
	}
}

func TestServerRejectsUnknownOperationsWithoutCallingHandler(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", SocketFilename)
	called := false
	server, err := StartServer(socketPath, func(context.Context, Report) error {
		called = true
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	_, _ = connection.Write([]byte(`{"version":1,"context_id":"` + testContextID + `","pane_id":"work:p1","codex_session_id":"` + testSessionID + `","method":"pane.send_input"}` + "\n"))
	var result response
	if err := json.NewDecoder(connection).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Error != "report rejected" || called {
		t.Fatalf("unsafe operation was not rejected generically: result=%+v called=%v", result, called)
	}
}

func TestRegistryServiceMapsAtomicContextSnapshotToFixedLauncher(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	contextID, _ := sessionstate.ParseContextID(testContextID)
	registered := sessionstate.Context{
		ID: contextID, State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: "lab-80", Cwd: t.TempDir(), Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}},
	}
	if _, err := sessionstate.UpdateRegistry(stateRoot, func(registry *sessionstate.Registry) error {
		return sessionstate.AddContext(registry, registered)
	}); err != nil {
		t.Fatal(err)
	}
	called := false
	service := RegistryService{
		StateRoot: stateRoot, HerdrPaths: sessionstate.HerdrPaths{Root: "/fixed/herdr"}, Now: func() time.Time { return time.Unix(10, 0) },
		Report: func(_ context.Context, paths sessionstate.HerdrPaths, launcher sessionstate.Launcher, paneID string, sessionID string, peerPID int, now time.Time) error {
			called = true
			if paths.Root != "/fixed/herdr" || !reflect.DeepEqual(launcher, registered.Launcher) || paneID != "work:p1" || sessionID != testSessionID || peerPID != 4242 || now != time.Unix(10, 0) {
				t.Fatalf("broker changed or exposed routing data: paths=%+v launcher=%+v pane=%q session=%q now=%v", paths, launcher, paneID, sessionID, now)
			}
			return nil
		},
	}
	if err := service.Handle(context.Background(), Report{Version: 1, ContextID: contextID, PaneID: "work:p1", CodexSessionID: testSessionID, PeerPID: 4242}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("Herdr association was not recorded")
	}
	unknown := Report{Version: 1, ContextID: "223e4567-e89b-42d3-a456-426614174000", PaneID: "work:p1", CodexSessionID: testSessionID}
	if err := service.Handle(context.Background(), unknown); err == nil {
		t.Fatal("unregistered context was accepted")
	}
	invalid := Report{Version: 1, ContextID: contextID, PaneID: "work:p1", CodexSessionID: "not-a-codex-uuid"}
	if err := service.Handle(context.Background(), invalid); err == nil {
		t.Fatal("invalid legacy session ID was normalized and accepted")
	}
}

func TestServerRefusesToReplaceNonSocketEndpoint(t *testing.T) {
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runtimeDir, SocketFilename)
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := StartServer(path, func(context.Context, Report) error { return nil }, nil); err == nil {
		t.Fatal("expected non-socket endpoint to be preserved and rejected")
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(data)) != "keep" {
		t.Fatalf("existing file was changed: data=%q err=%v", data, err)
	}
}

func TestClientReceivesOnlyGenericBrokerFailures(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime", SocketFilename)
	server, err := StartServer(socketPath, func(context.Context, Report) error {
		return errors.New("secret registry path /private/context")
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = Send(ctx, socketPath, Report{Version: 1, ContextID: testContextID, PaneID: "work:p1", CodexSessionID: testSessionID})
	if err == nil || err.Error() != "report rejected" {
		t.Fatalf("broker leaked internal failure: %v", err)
	}
}

func TestNarrowReportEndToEndReachesOnlyFixedHerdrMethods(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	herdrRoot := filepath.Join(t.TempDir(), "herdr")
	herdrSession := filepath.Join(herdrRoot, "sessions", "lab-80")
	if err := os.MkdirAll(herdrSession, 0o700); err != nil {
		t.Fatal(err)
	}
	herdrSocket := filepath.Join(herdrSession, "herdr.sock")
	listener, err := net.Listen("unix", herdrSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(herdrSocket, 0o600); err != nil {
		t.Fatal(err)
	}
	methods := make(chan string, 2)
	herdrDone := make(chan error, 1)
	go func() {
		for index := range 2 {
			connection, err := listener.Accept()
			if err != nil {
				herdrDone <- err
				return
			}
			line, err := bufio.NewReader(connection).ReadBytes('\n')
			if err != nil {
				_ = connection.Close()
				herdrDone <- err
				return
			}
			var request struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			if err := json.Unmarshal(line, &request); err != nil {
				_ = connection.Close()
				herdrDone <- err
				return
			}
			methods <- request.Method
			result := map[string]any{"type": "ok"}
			if index == 0 {
				result = map[string]any{
					"type":         "pane_process_info",
					"process_info": map[string]any{"pane_id": "work:p1", "shell_pid": os.Getpid()},
				}
			}
			response, _ := json.Marshal(map[string]any{"id": request.ID, "result": result})
			_, err = connection.Write(append(response, '\n'))
			_ = connection.Close()
			if err != nil {
				herdrDone <- err
				return
			}
		}
		herdrDone <- nil
	}()

	contextID, _ := sessionstate.ParseContextID(testContextID)
	registered := sessionstate.Context{
		ID: contextID, State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: "lab-80", Cwd: t.TempDir(), Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}},
	}
	if _, err := sessionstate.UpdateRegistry(stateRoot, func(registry *sessionstate.Registry) error {
		return sessionstate.AddContext(registry, registered)
	}); err != nil {
		t.Fatal(err)
	}
	service := RegistryService{StateRoot: stateRoot, HerdrPaths: sessionstate.HerdrPaths{Root: herdrRoot}}
	brokerSocket := filepath.Join(t.TempDir(), "runtime", SocketFilename)
	server, err := StartServer(brokerSocket, service.Handle, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := Send(ctx, brokerSocket, Report{Version: 1, ContextID: contextID, PaneID: "work:p1", CodexSessionID: testSessionID}); err != nil {
		t.Fatal(err)
	}
	if err := <-herdrDone; err != nil {
		t.Fatal(err)
	}
	if first, second := <-methods, <-methods; first != "pane.process_info" || second != "pane.report_agent_session" {
		t.Fatalf("unexpected Herdr method sequence: %q then %q", first, second)
	}
}
