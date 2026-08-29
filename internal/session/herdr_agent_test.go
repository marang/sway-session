package session

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReportHerdrCodexSessionSendsOnlyFixedAssociation(t *testing.T) {
	root := privateHerdrRoot(t)
	sessionDir := filepath.Join(root, "sessions", "lab-80")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(sessionDir, herdrAgentSocketName)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	requestChannel := make(chan map[string]any, 2)
	serverError := make(chan error, 1)
	go func() {
		for index := range 2 {
			connection, err := listener.Accept()
			if err != nil {
				serverError <- err
				return
			}
			line, err := bufio.NewReader(connection).ReadBytes('\n')
			if err != nil {
				_ = connection.Close()
				serverError <- err
				return
			}
			var request map[string]any
			if err := json.Unmarshal(line, &request); err != nil {
				_ = connection.Close()
				serverError <- err
				return
			}
			requestChannel <- request
			result := map[string]any{}
			if index == 0 {
				result["type"] = "pane_process_info"
				result["process_info"] = map[string]any{"pane_id": "work:p1", "shell_pid": os.Getpid()}
			} else {
				result["type"] = "ok"
			}
			response, _ := json.Marshal(map[string]any{"id": request["id"], "result": result})
			_, err = connection.Write(append(response, '\n'))
			_ = connection.Close()
			if err != nil {
				serverError <- err
				return
			}
		}
		serverError <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	launcher := Launcher{Kind: LauncherHerdr, Session: "lab-80", Cwd: t.TempDir()}
	sessionID := "01a04a4b-7fb9-7a90-8ace-51f7ae68e0ee"
	now := time.Unix(0, 123456789)
	if err := ReportHerdrCodexSession(ctx, HerdrPaths{Root: root}, launcher, "work:p1", sessionID, os.Getpid(), now); err != nil {
		t.Fatal(err)
	}
	if err := <-serverError; err != nil {
		t.Fatal(err)
	}
	processRequest := <-requestChannel
	if processRequest["method"] != "pane.process_info" {
		t.Fatalf("reporter was not bound to the pane process: %#v", processRequest)
	}
	request := <-requestChannel
	if request["method"] != "pane.report_agent_session" {
		t.Fatalf("unexpected method: %#v", request)
	}
	params, ok := request["params"].(map[string]any)
	if !ok {
		t.Fatalf("missing params: %#v", request)
	}
	want := map[string]any{
		"pane_id": "work:p1", "source": "herdr:codex", "agent": "codex",
		"seq": float64(now.UnixNano()), "agent_session_id": sessionID,
	}
	if len(params) != len(want) {
		t.Fatalf("report exposed extra fields: %#v", params)
	}
	for key, value := range want {
		if params[key] != value {
			t.Fatalf("unexpected %s: got %#v want %#v", key, params[key], value)
		}
	}
}

func TestReportHerdrCodexSessionRejectsUnsafeEndpointBeforeConnect(t *testing.T) {
	root := privateHerdrRoot(t)
	sessionDir := filepath.Join(root, "sessions", "lab-80")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, herdrAgentSocketName), []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	launcher := Launcher{Kind: LauncherHerdr, Session: "lab-80", Cwd: t.TempDir()}
	if err := ReportHerdrCodexSession(context.Background(), HerdrPaths{Root: root}, launcher, "work:p1", string(testContextID), os.Getpid(), time.Now()); err == nil {
		t.Fatal("expected a regular-file endpoint to be rejected")
	}
}

func TestHerdrEndpointPinsSocketAcrossPathReplacement(t *testing.T) {
	root := privateHerdrRoot(t)
	sessionDir := filepath.Join(root, "sessions", "lab-80")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(sessionDir, herdrAgentSocketName)
	original, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	original.SetUnlinkOnClose(false)
	defer original.Close()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint, err := openHerdrAPIEndpoint(root, "lab-80")
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.Close()

	originalPath := socketPath + ".original"
	if err := os.Rename(socketPath, originalPath); err != nil {
		t.Fatal(err)
	}
	replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}

	originalAccepted := make(chan error, 1)
	go func() {
		connection, err := original.AcceptUnix()
		if err == nil {
			_ = connection.Close()
		}
		originalAccepted <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := endpoint.request(ctx, "request", "pane.process_info", map[string]string{"pane_id": "work:p1"}); err == nil {
		t.Fatal("replaced Herdr socket path was accepted")
	}
	select {
	case err := <-originalAccepted:
		if err != nil {
			t.Fatalf("pinned original endpoint was not contacted: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pinned original endpoint was not contacted")
	}
	if err := replacement.SetDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	connection, err := replacement.AcceptUnix()
	if err == nil {
		_ = connection.Close()
		t.Fatal("replacement endpoint received the pinned request")
	}
	if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("inspect replacement endpoint: %v", err)
	}
}

func TestProcessDescendsFromUsesActualProcAncestry(t *testing.T) {
	parent, err := readParentPID(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	belongs, err := processDescendsFrom(os.Getpid(), parent)
	if err != nil || !belongs {
		t.Fatalf("current process was not linked to its parent: belongs=%v err=%v", belongs, err)
	}
	belongs, err = processDescendsFrom(os.Getpid(), 1<<30)
	if err != nil || belongs {
		t.Fatalf("unrelated process was accepted: belongs=%v err=%v", belongs, err)
	}
}

func privateHerdrRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "herdr")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}
