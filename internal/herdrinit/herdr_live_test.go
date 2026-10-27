package herdrinit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

// TestHerdrLiveInitialization exercises the public Herdr CLI against a
// disposable headless server. Its Codex executable is a harmless local shim;
// it never launches a terminal window or the real Codex executable.
func TestHerdrLiveInitialization(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HERDR_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HERDR_INTEGRATION=1 for disposable real-Herdr acceptance")
	}
	herdr, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name    string
		roles   []string
		lostAck bool
	}{
		{name: "successful", roles: []string{"codex", "shell"}},
		{name: "lost_ack_codex_left", roles: []string{"codex", "shell"}, lostAck: true},
		{name: "lost_ack_codex_right", roles: []string{"shell", "codex"}, lostAck: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			herdrLiveInitialization(t, herdr, scenario.roles, scenario.lostAck)
		})
	}
}

func herdrLiveInitialization(t *testing.T, herdr string, roles []string, lostAck bool) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "ss195-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	for _, dir := range []string{"home", "config/herdr", "state", "runtime", "bin", "work"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configFile := filepath.Join(root, "config/herdr/config.toml")
	config := "[terminal]\ndefault_shell = \"/bin/bash\"\nshell_mode = \"non_login\"\n[experimental]\npane_history = true\n"
	if err := os.WriteFile(configFile, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"HOME":            filepath.Join(root, "home"),
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"XDG_STATE_HOME":  filepath.Join(root, "state"),
		"XDG_RUNTIME_DIR": filepath.Join(root, "runtime"),
		"PATH":            filepath.Join(root, "bin") + ":" + os.Getenv("PATH"),
		"SHELL":           "/bin/bash",
		"TERM":            "xterm-256color",
		"HERDR_ENV":       "",
	} {
		t.Setenv(key, value)
	}
	version := exec.Command(herdr, "--version")
	version.Env = herdrLiveEnvironment(root)
	output, err := version.CombinedOutput()
	if err != nil || !strings.HasPrefix(string(output), "herdr ") {
		t.Fatalf("Herdr version: %s: %v", output, err)
	}
	versionLabel := strings.TrimSpace(string(output))
	if err := os.WriteFile(filepath.Join(root, "fixture.go"), []byte(herdrLiveCodex), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(root, "bin/codex"), filepath.Join(root, "fixture.go"))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build harmless Codex fixture: %s: %v", output, err)
	}

	sessionName := "lab195"
	server := exec.Command(herdr, "--session", sessionName, "server")
	server.Env = herdrLiveEnvironment(root)
	serverLog, err := os.OpenFile(filepath.Join(root, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		_ = serverLog.Close()
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Wait(); _ = serverLog.Close() }()
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stop := exec.CommandContext(stopCtx, herdr, "--session", sessionName, "server", "stop")
		stop.Env = herdrLiveEnvironment(root)
		_, _ = stop.CombinedOutput()
		select {
		case <-serverDone:
		case <-time.After(5 * time.Second):
			_ = server.Process.Kill()
			<-serverDone
		}
	})
	socket := filepath.Join(root, "config/herdr/sessions", sessionName, "herdr.sock")
	herdrLiveUntil(t, "isolated Herdr socket", func() bool {
		conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})

	work := filepath.Join(root, "work")
	runner := &herdrLiveRunner{Runner: ExecRunner{Executable: herdr, ConfigFile: configFile}}
	if lostAck {
		// The real CLI must finish a successful launch before this injected error.
		// This models an uncertain caller outcome, not an upstream readiness timeout.
		runner.startError = errors.New("injected lost acknowledgement after successful real agent start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := runner.Run(ctx, sessionName, work, "workspace", "create", "--cwd", work, "--label", "LAB-195 acceptance", "--focus"); err != nil {
		t.Fatalf("create disposable Herdr workspace: %v", err)
	}
	contextValue := sessionstate.Context{
		ID:       sessionstate.ContextID("c19955a0-f719-4195-843c-558bc545d4e7"),
		Label:    "LAB-195 acceptance",
		Provider: "codex",
		State:    sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: sessionName, Cwd: work,
			Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}},
	}
	initial, err := runner.Run(ctx, sessionName, work, "api", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	left, empty, err := parseEmptySession(initial)
	if err != nil || !empty {
		t.Fatalf("new Herdr session is not an empty one-pane layout: empty=%v error=%v", empty, err)
	}
	result, err := Initialize(ctx, contextValue, roles, runner)
	if lostAck {
		if !errors.Is(err, runner.startError) || result.Initialized {
			t.Fatalf("Initialize should retain uncertain start failure: result=%+v error=%v; Herdr log: %s", result, err, herdrLiveLog(root))
		}
	} else if err != nil || !result.Initialized {
		t.Fatalf("Initialize: result=%+v error=%v; Herdr log: %s", result, err, herdrLiveLog(root))
	}
	right, err := parseCreatedPane(runner.splitOutput, left)
	if err != nil {
		t.Fatalf("real split response: %s: %v", runner.splitOutput, err)
	}
	first, err := runner.Run(ctx, sessionName, work, "api", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := decodeSessionSnapshot(first)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Panes) != 2 || len(snapshot.Layouts) != 1 || len(snapshot.Layouts[0].Splits) != 1 || len(snapshot.Layouts[0].Panes) != 2 {
		t.Fatalf("Initialize did not produce one two-pane split: %s", first)
	}
	var split struct {
		Direction string  `json:"direction"`
		Ratio     float64 `json:"ratio"`
	}
	if err := json.Unmarshal(snapshot.Layouts[0].Splits[0], &split); err != nil || split.Direction != "right" || split.Ratio != 0.5 {
		t.Fatalf("Initialize did not split right at half width: %s: %v", snapshot.Layouts[0].Splits[0], err)
	}
	if snapshot.Panes[0].PaneID != left || snapshot.Panes[1].PaneID != right {
		t.Fatalf("Initialize did not preserve both exact panes: left=%s right=%s snapshot=%s", left, right, first)
	}
	for index, role := range roles {
		agent := snapshot.Panes[index].Agent
		if role == "shell" && agent != nil || role == "codex" && (agent == nil || *agent != "codex") {
			t.Fatalf("requested pane roles were not established: roles=%v snapshot=%s", roles, first)
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, "codex-started.json")); err != nil || string(data) != "[]" {
		t.Fatalf("harmless Codex fixture did not start: args=%s error=%v", data, err)
	}
	pidData, err := os.ReadFile(filepath.Join(root, "codex-pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid harmless Codex PID: %s: %v", pidData, err)
	}
	processStart := herdrLiveProcessStart(t, pid)
	identities := herdrLivePaneIdentities(t, first)
	secondResult, err := Initialize(ctx, contextValue, roles, runner)
	if err != nil || secondResult.Initialized || secondResult.Reason == "" {
		t.Fatalf("repeat Initialize should leave populated layout unchanged: result=%+v error=%v", secondResult, err)
	}
	second, err := runner.Run(ctx, sessionName, work, "api", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	after, err := decodeSessionSnapshot(second)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Panes) != 2 || len(after.Layouts) != 1 || len(after.Layouts[0].Splits) != 1 || after.Panes[0].PaneID != snapshot.Panes[0].PaneID || after.Panes[1].PaneID != snapshot.Panes[1].PaneID {
		t.Fatalf("repeat Initialize changed Herdr topology: before=%s after=%s", first, second)
	}
	afterIdentities := herdrLivePaneIdentities(t, second)
	for pane, terminal := range identities {
		if afterIdentities[pane] != terminal {
			t.Fatalf("repeat Initialize replaced a pane terminal: pane=%s before=%s after=%s", pane, terminal, afterIdentities[pane])
		}
	}
	if afterStart := herdrLiveProcessStart(t, pid); afterStart != processStart {
		t.Fatalf("repeat Initialize replaced harmless Codex process: pid=%d before=%s after=%s", pid, processStart, afterStart)
	}
	if runner.agentStarts != 1 {
		t.Fatalf("expected one real agent start across repeated Initialize, got %d", runner.agentStarts)
	}
	if data, err := os.ReadFile(filepath.Join(root, "codex-pid")); err != nil || string(data) != string(pidData) {
		t.Fatalf("repeat Initialize changed harmless Codex PID: before=%s after=%s error=%v", pidData, data, err)
	}
	t.Logf("%s roles=%v injected_lost_ack=%v preserved exact panes %s/%s and Codex pid=%d across repeat; real agent starts=%d", versionLabel, roles, lostAck, left, right, pid, runner.agentStarts)
}

type herdrLiveRunner struct {
	Runner
	startError  error
	agentStarts int
	splitOutput []byte
}

func (runner *herdrLiveRunner) Run(ctx context.Context, session, cwd string, args ...string) ([]byte, error) {
	start := len(args) >= 2 && args[0] == "agent" && args[1] == "start"
	if start {
		runner.agentStarts++
	}
	output, err := runner.Runner.Run(ctx, session, cwd, args...)
	if err == nil && len(args) >= 2 && args[0] == "pane" && args[1] == "split" {
		runner.splitOutput = append([]byte(nil), output...)
	}
	if start && err == nil && runner.startError != nil {
		return nil, runner.startError
	}
	return output, err
}

func herdrLivePaneIdentities(t *testing.T, output []byte) map[string]string {
	t.Helper()
	var response struct {
		Result struct {
			Snapshot struct {
				Panes []struct {
					PaneID     string `json:"pane_id"`
					TerminalID string `json:"terminal_id"`
				} `json:"panes"`
			} `json:"snapshot"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatal(err)
	}
	identities := make(map[string]string)
	for _, pane := range response.Result.Snapshot.Panes {
		if pane.PaneID == "" || pane.TerminalID == "" || identities[pane.PaneID] != "" {
			t.Fatalf("missing or duplicate live pane identity: %s", output)
		}
		identities[pane.PaneID] = pane.TerminalID
	}
	return identities
}

func herdrLiveProcessStart(t *testing.T, pid int) string {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatalf("harmless Codex process %d is not alive: %v", pid, err)
	}
	// The parenthesized command can contain spaces. Fields after it begin at
	// field 3 (state); field 22 (start time) distinguishes reuse of the same PID.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		t.Fatalf("invalid stat for harmless Codex process %d: %s", pid, data)
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 || fields[0] == "Z" || fields[0] == "X" {
		t.Fatalf("harmless Codex process %d is not running: %s", pid, data)
	}
	return fields[19]
}

func herdrLiveEnvironment(root string) []string {
	return []string{
		"HOME=" + filepath.Join(root, "home"),
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"),
		"XDG_STATE_HOME=" + filepath.Join(root, "state"),
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "runtime"),
		"PATH=" + filepath.Join(root, "bin") + ":" + os.Getenv("PATH"),
		"SHELL=/bin/bash", "TERM=xterm-256color",
		"LAB195_ROOT=" + root,
	}
}

func herdrLiveUntil(t *testing.T, label string, ready func() bool) {
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

func herdrLiveLog(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "server.log"))
	if err != nil {
		return fmt.Sprintf("unavailable: %v", err)
	}
	if len(data) > 4096 {
		data = data[len(data)-4096:]
	}
	return string(data)
}

const herdrLiveCodex = `package main
import("bufio";"encoding/json";"fmt";"os";"path/filepath";"strconv")
func main(){root:=os.Getenv("LAB195_ROOT");if root!=""{data,_:=json.Marshal(os.Args[1:]);_ = os.WriteFile(filepath.Join(root,"codex-started.json"),data,0600);_ = os.WriteFile(filepath.Join(root,"codex-pid"),[]byte(strconv.Itoa(os.Getpid())),0600)}
fmt.Println("OpenAI Codex");fmt.Println("› Ask Codex to do anything")
reader:=bufio.NewReader(os.Stdin);for {if _,err:=reader.ReadString('\n');err!=nil{return};fmt.Println("› Ask Codex to do anything")}}
`
