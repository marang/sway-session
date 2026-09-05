package agentreport

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

func TestReportAgentSessionReachesFixedHerdrAssociation(t *testing.T) {
	runtimeRoot, err := os.MkdirTemp("", "agent-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeRoot) })
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)

	herdrRoot, err := os.MkdirTemp("", "agent-herdr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(herdrRoot) })
	sessionName := "lab-122"
	herdrSession := filepath.Join(herdrRoot, "sessions", sessionName)
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

	requests := make(chan map[string]any, 2)
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
			var request map[string]any
			if err := json.Unmarshal(line, &request); err != nil {
				_ = connection.Close()
				herdrDone <- err
				return
			}
			requests <- request
			result := map[string]any{"type": "ok"}
			if index == 0 {
				result = map[string]any{"type": "pane_process_info", "process_info": map[string]any{"pane_id": "work:p1", "shell_pid": os.Getpid()}}
			}
			response, _ := json.Marshal(map[string]any{"id": request["id"], "result": result})
			_, err = connection.Write(append(response, '\n'))
			_ = connection.Close()
			if err != nil {
				herdrDone <- err
				return
			}
		}
		herdrDone <- nil
	}()

	stateRoot := filepath.Join(t.TempDir(), "state")
	contextID, err := sessionstate.ParseContextID("123e4567-e89b-12d3-a456-426614174000")
	if err != nil {
		t.Fatal(err)
	}
	registered := sessionstate.Context{
		ID: contextID, State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: sessionName, Cwd: t.TempDir(), Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}},
	}
	if _, err := sessionstate.UpdateRegistry(stateRoot, func(registry *sessionstate.Registry) error {
		return sessionstate.AddContext(registry, registered)
	}); err != nil {
		t.Fatal(err)
	}
	service := RegistryService{StateRoot: stateRoot, HerdrPaths: sessionstate.HerdrPaths{Root: herdrRoot}}
	socketPath, err := DefaultSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	server, err := StartServer(socketPath, service.Handle, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	environment := map[string]string{
		HerdrActiveEnvironment: "1",
		ContextIDEnvironment:   string(contextID),
		HerdrPaneEnvironment:   "work:p1",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ReportAgentSession(ctx, strings.NewReader(`{"agent":"claude","agent_session_id":"claude:thread-122"}`), func(name string) string { return environment[name] }); err != nil {
		t.Fatal(err)
	}
	if err := <-herdrDone; err != nil {
		t.Fatal(err)
	}
	first, second := <-requests, <-requests
	if first["method"] != "pane.process_info" || second["method"] != "pane.report_agent_session" {
		t.Fatalf("unexpected Herdr method sequence: %q then %q", first["method"], second["method"])
	}
	processParams, ok := first["params"].(map[string]any)
	if !ok || len(processParams) != 1 || processParams["pane_id"] != "work:p1" {
		t.Fatalf("unexpected pane verification params: %#v", first["params"])
	}
	params, ok := second["params"].(map[string]any)
	if !ok || len(params) != 5 || params["pane_id"] != "work:p1" || params["source"] != "herdr:claude" || params["agent"] != "claude" || params["agent_session_id"] != "claude:thread-122" {
		t.Fatalf("unexpected Herdr association params: %#v", second["params"])
	}
	if _, ok := params["seq"].(float64); !ok {
		t.Fatalf("association omitted a numeric sequence: %#v", params)
	}
}
