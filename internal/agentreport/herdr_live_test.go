package agentreport

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

// TestHerdrLiveAgentReplacement exercises the real Herdr authority rules, not
// an API mock. It never connects to the user's Herdr server or launches Codex:
// the only agent executable is a disposable local reporting fixture.
func TestHerdrLiveAgentReplacement(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HERDR_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HERDR_INTEGRATION=1 for disposable real-Herdr acceptance")
	}
	herdr, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "ss138-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	for _, dir := range []string{"home", "config/herdr", "state", "runtime", "bin", "work"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config := "[terminal]\ndefault_shell = \"/bin/bash\"\nshell_mode = \"non_login\"\n[session]\nresume_agents_on_restore = true\n"
	if err := os.WriteFile(filepath.Join(root, "config/herdr/config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + filepath.Join(root, "home"), "XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_STATE_HOME=" + filepath.Join(root, "state"), "XDG_RUNTIME_DIR=" + filepath.Join(root, "runtime"), "PATH=" + filepath.Join(root, "bin") + ":" + os.Getenv("PATH"), "SHELL=/bin/bash", "TERM=xterm-256color", "SWAY_SESSION_CONTEXT_ID=" + string(testContextID), "LAB138_ROOT=" + root}
	version := exec.Command(herdr, "--version")
	version.Env = env
	out, err := version.CombinedOutput()
	if err != nil {
		t.Fatalf("Herdr version: %s: %v", out, err)
	}
	t.Log(strings.TrimSpace(string(out)))
	source := filepath.Join(root, "fixture.go")
	if err := os.WriteFile(source, []byte(herdrLiveCodexFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(root, "bin/codex"), source)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %s: %v", out, err)
	}
	herdrRoot := filepath.Join(root, "config/herdr")
	socket := filepath.Join(herdrRoot, "sessions/lab138/herdr.sock")
	stateRoot := filepath.Join(root, "registry")
	registered := sessionstate.Context{ID: testContextID, State: sessionstate.ContextActive, Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: "lab138", Cwd: filepath.Join(root, "work"), Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}}}
	if _, err := sessionstate.UpdateRegistry(stateRoot, func(r *sessionstate.Registry) error { return sessionstate.AddContext(r, registered) }); err != nil {
		t.Fatal(err)
	}
	service := RegistryService{StateRoot: stateRoot, HerdrPaths: sessionstate.HerdrPaths{Root: herdrRoot}}
	broker, err := StartServer(filepath.Join(root, "runtime", SocketFilename), service.Handle, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	var server *exec.Cmd
	var done chan error
	start := func() {
		log, err := os.OpenFile(filepath.Join(root, "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		server = exec.Command(herdr, "--session", "lab138", "server")
		server.Env = env
		server.Stdout = log
		server.Stderr = log
		if err := server.Start(); err != nil {
			t.Fatal(err)
		}
		done = make(chan error, 1)
		current := server
		go func() { done <- current.Wait(); _ = log.Close() }()
		herdrLiveWait(t, "Herdr socket", func() bool {
			conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
			if err != nil {
				return false
			}
			_ = conn.Close()
			return true
		})
	}
	stop := func() {
		herdrLiveAPI(t, socket, "server.stop", map[string]any{})
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Herdr stop: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Herdr did not stop")
		}
		server = nil
	}
	t.Cleanup(func() {
		if server != nil && server.Process != nil {
			_ = server.Process.Kill()
			<-done
		}
	})
	start()
	created := herdrLiveAPI(t, socket, "workspace.create", map[string]any{"cwd": filepath.Join(root, "work"), "label": "LAB-138 acceptance", "focus": true})
	var creation struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	if err := json.Unmarshal(created, &creation); err != nil || creation.RootPane.PaneID == "" {
		t.Fatalf("created pane: %s: %v", created, err)
	}
	pane := creation.RootPane.PaneID
	herdrLiveAPI(t, socket, "pane.send_input", map[string]any{"pane_id": pane, "text": "codex", "keys": []string{"enter"}})
	herdrLiveWait(t, "fixture startup", func() bool { _, err := os.Stat(filepath.Join(root, "started.json")); return err == nil })
	report := func(t *testing.T, id, origin string, wantOK bool) {
		t.Helper()
		_ = os.Remove(filepath.Join(root, "reply.json"))
		command, _ := json.Marshal(map[string]string{"id": id, "origin": origin})
		tmp := filepath.Join(root, "command.tmp")
		if err := os.WriteFile(tmp, command, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(root, "command.json")); err != nil {
			t.Fatal(err)
		}
		herdrLiveWait(t, "broker response", func() bool { _, err := os.Stat(filepath.Join(root, "reply.json")); return err == nil })
		data, err := os.ReadFile(filepath.Join(root, "reply.json"))
		if err != nil {
			t.Fatal(err)
		}
		var reply response
		if err := json.Unmarshal(data, &reply); err != nil {
			t.Fatalf("fixture response: %s: %v", data, err)
		}
		if reply.OK != wantOK || reply.Version != ProtocolVersion {
			t.Fatalf("report id=%s origin=%q: reply %s, want ok=%v", id, origin, data, wantOK)
		}
	}
	assertSnapshot := func(t *testing.T, id string) {
		t.Helper()
		snapshot := herdrLiveAPI(t, socket, "session.snapshot", map[string]any{})
		if err := herdrLiveAssociation(snapshot, pane, id); err != nil {
			t.Fatal(err)
		}
	}
	a := "11111111-1111-4111-8111-111111111111"
	b := "22222222-2222-4222-8222-222222222222"
	report(t, a, "", true)
	assertSnapshot(t, a)
	for _, origin := range []string{"", "future_origin"} {
		report(t, b, origin, false)
		assertSnapshot(t, a)
	}
	for _, origin := range []string{"resume", "startup", "clear", "compact"} {
		t.Run(origin, func(t *testing.T) {
			report(t, b, origin, true)
			assertSnapshot(t, b)
			report(t, a, "resume", true)
			assertSnapshot(t, a)
		})
	}
	report(t, b, "resume", true)
	assertSnapshot(t, b)
	stop()
	saved, err := os.ReadFile(filepath.Join(herdrRoot, "sessions/lab138/session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted struct {
		Workspaces []struct {
			Tabs []struct {
				Panes map[string]struct {
					AgentSession *herdrLiveAssociationValue `json:"agent_session"`
				} `json:"panes"`
			} `json:"tabs"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(saved, &persisted); err != nil {
		t.Fatal(err)
	}
	var associations []herdrLiveAssociationValue
	for _, w := range persisted.Workspaces {
		for _, tab := range w.Tabs {
			for _, p := range tab.Panes {
				if p.AgentSession != nil {
					associations = append(associations, *p.AgentSession)
				}
			}
		}
	}
	want := herdrLiveAssociationValue{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: b}
	if !reflect.DeepEqual(associations, []herdrLiveAssociationValue{want}) {
		t.Fatalf("persisted associations=%+v, want exactly %+v", associations, want)
	}
	if err := os.Remove(filepath.Join(root, "started.json")); err != nil {
		t.Fatal(err)
	}
	start()
	herdrLiveWait(t, "exact resume invocation", func() bool {
		data, err := os.ReadFile(filepath.Join(root, "started.json"))
		if err != nil {
			return false
		}
		var argv []string
		if json.Unmarshal(data, &argv) != nil {
			return false
		}
		return reflect.DeepEqual(argv, []string{"resume", b})
	})
	assertSnapshot(t, b)
	stop()
	t.Logf("real Herdr accepted A, rejected unauthorized B, replaced for all native origins, persisted B, and restored codex resume %s", b)
}

type herdrLiveAssociationValue struct {
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Value  string `json:"value"`
}

func herdrLiveAssociation(data []byte, pane, id string) error {
	var snapshot struct {
		Panes []struct {
			PaneID       string                     `json:"pane_id"`
			AgentSession *herdrLiveAssociationValue `json:"agent_session"`
		} `json:"panes"`
	}
	var result struct {
		Snapshot json.RawMessage `json:"snapshot"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	if err := json.Unmarshal(result.Snapshot, &snapshot); err != nil {
		return err
	}
	count := 0
	for _, p := range snapshot.Panes {
		if p.PaneID == pane {
			count++
			want := herdrLiveAssociationValue{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: id}
			if p.AgentSession == nil || *p.AgentSession != want {
				return fmt.Errorf("live association=%+v, want %+v", p.AgentSession, want)
			}
		}
	}
	if count != 1 {
		return fmt.Errorf("snapshot found %d matching panes", count)
	}
	return nil
}
func herdrLiveWait(t *testing.T, label string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}
func herdrLiveAPI(t *testing.T, socket, method string, params any) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	request := map[string]any{"id": "acceptance", "method": method, "params": params}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(io.LimitReader(conn, 65537)).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	if len(line) > 65536 {
		t.Fatal("oversized acceptance response")
	}
	var reply struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Error) > 0 && string(reply.Error) != "null" {
		t.Fatalf("Herdr %s: %s", method, reply.Error)
	}
	return reply.Result
}

const herdrLiveCodexFixture = `package main
import("bufio";"encoding/json";"net";"os";"path/filepath";"time")
func write(root,name string,data []byte){tmp:=filepath.Join(root,name+".tmp");if err:=os.WriteFile(tmp,data,0600);err!=nil{panic(err)};if err:=os.Rename(tmp,filepath.Join(root,name));err!=nil{panic(err)}}
func main(){root:=os.Getenv("LAB138_ROOT");argv,_:=json.Marshal(os.Args[1:]);write(root,"started.json",argv)
for {data,err:=os.ReadFile(filepath.Join(root,"command.json"));if err!=nil{time.Sleep(20*time.Millisecond);continue};if err:=os.Remove(filepath.Join(root,"command.json"));err!=nil{panic(err)};var command struct{ID string;Origin string};if err:=json.Unmarshal(data,&command);err!=nil{panic(err)}
conn,err:=net.DialTimeout("unix",filepath.Join(root,"runtime/agent-report.sock"),time.Second);if err!=nil{panic(err)};conn.SetDeadline(time.Now().Add(4*time.Second));report:=map[string]any{"version":3,"context_id":os.Getenv("SWAY_SESSION_CONTEXT_ID"),"pane_id":os.Getenv("HERDR_PANE_ID"),"agent":"codex","agent_session_id":command.ID};if command.Origin!=""{report["event_origin"]=command.Origin};if err:=json.NewEncoder(conn).Encode(report);err!=nil{panic(err)};reply,err:=bufio.NewReader(conn).ReadBytes('\n');if err!=nil{panic(err)};conn.Close();write(root,"reply.json",reply)
}}
`
