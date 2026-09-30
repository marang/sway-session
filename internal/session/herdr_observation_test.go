package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type observationRunner struct {
	output []byte
	err    error
	calls  atomic.Int32
	wait   bool
}

func (runner *observationRunner) CombinedOutput(ctx context.Context, _ string, args ...string) ([]byte, error) {
	runner.calls.Add(1)
	if !reflect.DeepEqual(args, []string{"session", "list", "--json"}) {
		return nil, errors.New("unexpected command")
	}
	if runner.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return runner.output, runner.err
}

func observationList(t *testing.T, root string, running bool, names ...string) []byte {
	t.Helper()
	infos := make([]herdrSessionInfo, 0, len(names))
	for _, name := range names {
		dir := filepath.Join(root, "sessions", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		infos = append(infos, herdrSessionInfo{Name: name, Running: running, SessionDir: dir, SocketPath: filepath.Join(dir, "herdr.sock")})
	}
	data, err := json.Marshal(herdrSessionList{Sessions: infos})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseHerdrLivePanesRejectsUnsupportedSnapshots(t *testing.T) {
	for _, test := range []struct {
		name, data string
		invalid    bool
	}{
		{"shell", `{"type":"session_snapshot","snapshot":{"panes":[{"pane_id":"p1","agent_status":"unknown"}]}}`, false},
		{"null", `{"type":"session_snapshot","snapshot":{"panes":[{"pane_id":"p1","agent":null,"agent_status":"idle"}]}}`, false},
		{"stale association", `{"type":"session_snapshot","snapshot":{"panes":[{"pane_id":"p1","agent_status":"unknown","display_agent":"codex","agent_session":{"agent":"codex","value":"secret"}}],"agents":[{"name":"stored-name"}]}}`, false},
		{"valid live label", `{"type":"session_snapshot","snapshot":{"panes":[{"pane_id":"p1","agent":"codex","agent_status":"idle"}]}}`, false},
		{"custom live label", `{"type":"session_snapshot","snapshot":{"panes":[{"pane_id":"p1","agent":"custom-agent","agent_status":"unknown"}]}}`, false},
		{"empty panes", `{"type":"session_snapshot","snapshot":{"panes":[]}}`, false},
		{"missing panes", `{"type":"session_snapshot","snapshot":{}}`, true},
		{"null panes", `{"type":"session_snapshot","snapshot":{"panes":null}}`, true},
		{"missing status", `{"type":"session_snapshot","snapshot":{"panes":[{"pane_id":"p1"}]}}`, true},
		{"null status", `{"type":"session_snapshot","snapshot":{"panes":[{"pane_id":"p1","agent_status":null}]}}`, true},
		{"invalid status", `{"type":"session_snapshot","snapshot":{"panes":[{"pane_id":"p1","agent_status":"running"}]}}`, true},
		{"empty label", `{"type":"session_snapshot","snapshot":{"panes":[{"pane_id":"p1","agent":"","agent_status":"working"}]}}`, true},
		{"invalid label type", `{"type":"session_snapshot","snapshot":{"panes":[{"pane_id":"p1","agent":true,"agent_status":"working"}]}}`, true},
		{"missing identity", `{"type":"session_snapshot","snapshot":{"panes":[{"agent_status":"idle"}]}}`, true},
		{"duplicate identity", `{"type":"session_snapshot","snapshot":{"panes":[{"pane_id":"p1","agent_status":"idle"},{"pane_id":"p1","agent_status":"idle"}]}}`, true},
		{"wrong result", `{"type":"ok","snapshot":{"panes":[]}}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseHerdrLivePanes([]byte(test.data))
			if (err != nil) != test.invalid {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestObserveHerdrSessionsBulkStoppedMissingAndUntrusted(t *testing.T) {
	root := observationTestRoot(t)
	runner := &observationRunner{output: observationList(t, root, false, "stopped", "duplicate", "duplicate")}
	if err := os.MkdirAll(filepath.Join(root, "sessions", "omitted"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := HerdrManager{Root: root, Executable: "/fake/herdr", Runner: runner}
	got := manager.ObserveSessions(context.Background(), []string{"stopped", "missing", "duplicate", "omitted", "../unsafe", "default", "stopped"}, "/home/example")
	for name, states := range map[string][2]string{"stopped": {"stopped", "none"}, "missing": {"missing", "none"}, "duplicate": {"unknown", "unknown"}, "omitted": {"unknown", "unknown"}, "../unsafe": {"unknown", "unknown"}, "default": {"unknown", "unknown"}} {
		if observation := got[name]; observation.SessionState != states[0] || observation.AgentState != states[1] {
			t.Fatalf("%s: %+v", name, observation)
		}
	}
	if runner.calls.Load() != 1 {
		t.Fatal("discovery was not shared")
	}
	if got["stopped"].ObservedAt == nil || got["stopped"].ObservedAt.Location() != time.UTC {
		t.Fatal("missing UTC evidence timestamp")
	}
}

func TestObserveHerdrSessionsRejectsIncompleteListAndRedactsFailures(t *testing.T) {
	root := observationTestRoot(t)
	for _, data := range []string{`{}`, `{"sessions":null}`, `{"sessions":[{"name":"target"}]}`, `{"sessions":[{"name":"target","default":false,"running":null,"socket_path":"secret","session_dir":"secret"}]}`, `{"sessions":[]} {}`} {
		runner := &observationRunner{output: []byte(data)}
		got := (HerdrManager{Root: root, Executable: "/fake/herdr", Runner: runner}).ObserveSessions(context.Background(), []string{"target"}, "/home/example")["target"]
		if got.SessionState != "unknown" || got.Reason != "invalid_session_list" {
			t.Fatalf("%s: %+v", data, got)
		}
	}
	runner := &observationRunner{output: []byte("private captured output"), err: errors.New("private error")}
	got := (HerdrManager{Root: root, Executable: "/fake/herdr", Runner: runner}).ObserveSessions(context.Background(), []string{"target"}, "/home/example")["target"]
	if got.Reason != "session_list_failed" || strings.Contains(got.Reason, "private") {
		t.Fatalf("unredacted failure: %+v", got)
	}
}

func serveObservation(t *testing.T, root, name, result string, delay time.Duration, requests *atomic.Int32, concurrency ...*atomic.Int32) {
	serveObservationWithProcessInfo(t, root, name, result, `{"type":"pane_process_info","process_info":{"pane_id":"p1","shell_pid":100,"foreground_process_group_id":200,"foreground_processes":[{"pid":200,"name":"codex","argv":["/usr/bin/codex"]}]}}`, delay, requests, concurrency...)
}

func serveObservationWithProcessInfo(t *testing.T, root, name, result, processInfo string, delay time.Duration, requests *atomic.Int32, concurrency ...*atomic.Int32) {
	t.Helper()
	dir := filepath.Join(root, "sessions", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "herdr.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	t.Cleanup(func() { _ = listener.Close(); <-finished })
	go func() {
		defer close(finished)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		line, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			return
		}
		var request struct {
			ID     string `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(line, &request); err != nil {
			return
		}
		if request.Method != "session.snapshot" {
			t.Errorf("unexpected request %s", request.Method)
			return
		}
		requests.Add(1)
		if len(concurrency) == 2 {
			active := concurrency[0].Add(1)
			for peak := concurrency[1].Load(); active > peak; peak = concurrency[1].Load() {
				if concurrency[1].CompareAndSwap(peak, active) {
					break
				}
			}
		}
		time.Sleep(delay)
		if len(concurrency) == 2 {
			// Count withheld responses, not post-response handler cleanup. A
			// client can start its next probe as soon as this reply is delivered.
			concurrency[0].Add(-1)
		}
		_, _ = fmt.Fprintf(conn, "{\"id\":%q,\"result\":%s}\n", request.ID, result)
		_ = conn.Close()
		if delay > 0 {
			return
		}
		processConn, err := listener.Accept()
		if err != nil {
			return
		}
		defer processConn.Close()
		_ = processConn.SetDeadline(time.Now().Add(time.Second))
		line, err = bufio.NewReader(processConn).ReadBytes('\n')
		if err != nil {
			return
		}
		if json.Unmarshal(line, &request) != nil || request.Method != "pane.process_info" {
			t.Error("expected process query")
			return
		}
		_, _ = fmt.Fprintf(processConn, "{\"id\":%q,\"result\":%s}\n", request.ID, processInfo)

	}()
}

func TestObserveHerdrSessionsStaleAssociationAndUnsupportedProcessEvidence(t *testing.T) {
	for _, test := range []struct{ name, process, agent, reason string }{
		{"stale association with idle shell", `{"type":"pane_process_info","process_info":{"pane_id":"p1","shell_pid":100,"foreground_process_group_id":100,"foreground_processes":[{"pid":100,"name":"sh"}]}}`, "none", ""},
		{"unsupported process API", `{"type":"unsupported"}`, "unknown", "agent_activity_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := observationTestRoot(t)
			var requests atomic.Int32
			serveObservationWithProcessInfo(t, root, "live", `{"type":"session_snapshot","snapshot":{"panes":[{"pane_id":"p1","agent":"codex","agent_status":"working","cwd":"/work/project","agent_session":{"agent":"codex","value":"old-session"}}]}}`, test.process, 0, &requests)
			runner := &observationRunner{output: observationList(t, root, true, "live")}
			got := (HerdrManager{Root: root, Executable: "/fake/herdr", Runner: runner}).ObserveSessions(t.Context(), []string{"live"}, "/home/example")["live"]
			if got.SessionState != "running" || got.AgentState != test.agent || got.Reason != test.reason {
				t.Fatalf("stale metadata overrode live evidence: %+v", got)
			}
		})
	}
}

func TestObserveHerdrSessionsLiveSnapshotAlsoSuppliesDirectory(t *testing.T) {
	root := observationTestRoot(t)
	var requests atomic.Int32
	serveObservation(t, root, "live", `{"type":"session_snapshot","snapshot":{"panes":[{"pane_id":"p1","agent":"codex","agent_status":"blocked","cwd":"/work/project"}]}}`, 0, &requests)
	runner := &observationRunner{output: observationList(t, root, true, "live")}
	got := (HerdrManager{Root: root, Executable: "/fake/herdr", Runner: runner}).ObserveSessions(context.Background(), []string{"live", "live"}, "/home/example")["live"]
	if got.SessionState != "running" || got.AgentState != "detected" || got.Directory.Directory != "/work/project" || got.Reason != "" {
		t.Fatalf("got %+v", got)
	}
	if requests.Load() != 1 || runner.calls.Load() != 1 {
		t.Fatal("duplicated snapshot or discovery")
	}
}

func TestObserveHerdrSessionsContinuesAfterPaneDisappears(t *testing.T) {
	root := observationTestRoot(t)
	directory := filepath.Join(root, "sessions", "live")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "herdr.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	t.Cleanup(func() { _ = listener.Close(); <-finished })
	var queries atomic.Int32
	go func() {
		defer close(finished)
		for index := range 3 {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			line, err := bufio.NewReader(conn).ReadBytes('\n')
			if err != nil {
				_ = conn.Close()
				return
			}
			var request struct {
				ID     string `json:"id"`
				Method string `json:"method"`
				Params struct {
					PaneID string `json:"pane_id"`
				} `json:"params"`
			}
			if json.Unmarshal(line, &request) != nil {
				_ = conn.Close()
				return
			}
			if index == 0 {
				if request.Method != "session.snapshot" {
					t.Error("expected snapshot")
				}
				_, _ = fmt.Fprintf(conn, "{\"id\":%q,\"result\":{\"type\":\"session_snapshot\",\"snapshot\":{\"panes\":[{\"pane_id\":\"p1\",\"agent_status\":\"unknown\"},{\"pane_id\":\"p2\",\"agent_status\":\"working\",\"agent\":\"codex\"}]}}}\n", request.ID)
			} else {
				queries.Add(1)
				if request.Method != "pane.process_info" {
					t.Error("expected process evidence")
				}
				if index == 2 {
					if request.Params.PaneID != "p2" {
						t.Error("healthy pane not queried")
					}
					_, _ = fmt.Fprintf(conn, "{\"id\":%q,\"result\":{\"type\":\"pane_process_info\",\"process_info\":{\"pane_id\":\"p2\",\"shell_pid\":100,\"foreground_process_group_id\":200,\"foreground_processes\":[{\"pid\":200,\"name\":\"codex\",\"argv\":[\"/usr/bin/codex\"]}]}}}\n", request.ID)
				}
				// The first pane disappears between snapshot and process lookup.
			}
			_ = conn.Close()
		}
	}()
	runner := &observationRunner{output: observationList(t, root, true, "live")}
	got := (HerdrManager{Root: root, Executable: "/fake/herdr", Runner: runner}).ObserveSessions(t.Context(), []string{"live"}, "/home/example")["live"]
	if got.AgentState != "detected" || got.Reason != "process_info_failed" || queries.Load() != 2 {
		t.Fatalf("healthy pane lost after local failure: %+v queries=%d", got, queries.Load())
	}
}

func TestObserveHerdrSessionsSnapshotTimeoutAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			root := observationTestRoot(t)
			var requests atomic.Int32
			serveObservation(t, root, "live", `{"type":"session_snapshot","snapshot":{"panes":[]}}`, time.Second, &requests)
			runner := &observationRunner{output: observationList(t, root, true, "live")}
			ctx := context.Background()
			if canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
				defer cancel()
			}
			start := time.Now()
			got := (HerdrManager{Root: root, Executable: "/fake/herdr", Runner: runner}).ObserveSessions(ctx, []string{"live"}, "/home/example")["live"]
			if time.Since(start) > time.Second {
				t.Fatal("unbounded RPC")
			}
			if canceled {
				if got.SessionState != "unknown" || got.Reason != "refresh_canceled" {
					t.Fatalf("got %+v", got)
				}
			} else if got.SessionState != "running" || got.AgentState != "unknown" || got.Reason != "snapshot_timeout" {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestObserveHerdrSessionsManyContextsUseOneBulkList(t *testing.T) {
	root := observationTestRoot(t)
	names := make([]string, 160)
	for i := range names {
		names[i] = fmt.Sprintf("context-%d", i)
	}
	runner := &observationRunner{output: observationList(t, root, false, names...)}
	got := (HerdrManager{Root: root, Executable: "/fake/herdr", Runner: runner}).ObserveSessions(context.Background(), names, "/home/example")
	if len(got) != len(names) || runner.calls.Load() != 1 {
		t.Fatal("context limit or repeated discovery")
	}
	for name, observation := range got {
		if observation.SessionState != "stopped" {
			t.Fatalf("%s: %+v", name, observation)
		}
	}
}

func TestObserveHerdrSessionsCancellationDuringList(t *testing.T) {
	root := observationTestRoot(t)
	runner := &observationRunner{wait: true}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	got := (HerdrManager{Root: root, Executable: "/fake/herdr", Runner: runner}).ObserveSessions(ctx, []string{"one", "two"}, "/home/example")
	for _, observation := range got {
		if observation.Reason != "refresh_canceled" || observation.ObservedAt == nil {
			t.Fatalf("got %+v", observation)
		}
	}
}

func TestObserveHerdrSessionsBoundsWorkersAndWholeRefresh(t *testing.T) {
	for _, delay := range []time.Duration{30 * time.Millisecond, time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			root := observationTestRoot(t)
			var requests, active, peak atomic.Int32
			names := make([]string, 20)
			for i := range names {
				names[i] = fmt.Sprintf("live-%d", i)
				serveObservation(t, root, names[i], `{"type":"session_snapshot","snapshot":{"panes":[]}}`, delay, &requests, &active, &peak)
			}
			runner := &observationRunner{output: observationList(t, root, true, names...)}
			start := time.Now()
			got := (HerdrManager{Root: root, Executable: "/fake/herdr", Runner: runner}).ObserveSessions(context.Background(), names, "/home/example")
			if time.Since(start) > 3500*time.Millisecond || runner.calls.Load() != 1 || len(got) != len(names) {
				t.Fatal("refresh was not bounded and shared")
			}
			if delay < time.Second {
				if requests.Load() != int32(len(names)) || peak.Load() > 4 {
					t.Fatalf("requests=%d peak=%d", requests.Load(), peak.Load())
				}
			} else {
				unknown := 0
				for _, observation := range got {
					if observation.SessionState == "unknown" {
						unknown++
					}
				}
				if unknown == 0 || requests.Load() == int32(len(names)) {
					t.Fatal("remaining sessions were not unknown at deadline")
				}
			}
		})
	}
}

func observationTestRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "ss131-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func TestParseHerdrProcessActivity(t *testing.T) {
	agent := "codex"
	for _, test := range []struct{ name, process, want string }{
		{"shell despite stale label", `{"pane_id":"p1","shell_pid":100,"foreground_process_group_id":100,"foreground_processes":[{"pid":100,"name":"sh"}]}`, "none"},
		{"foreground codex", `{"pane_id":"p1","shell_pid":100,"foreground_process_group_id":200,"foreground_processes":[{"pid":200,"name":"codex","argv":["/usr/bin/codex"]}]}`, "detected"},
		{"wrapper", `{"pane_id":"p1","shell_pid":100,"foreground_process_group_id":200,"foreground_processes":[{"pid":200,"name":"node","argv":["node","codex"]}]}`, "unknown"},
		{"missing argv", `{"pane_id":"p1","shell_pid":100,"foreground_process_group_id":200,"foreground_processes":[{"pid":200,"name":"codex"}]}`, "unknown"},
		{"mismatched argv", `{"pane_id":"p1","shell_pid":100,"foreground_process_group_id":200,"foreground_processes":[{"pid":200,"name":"codex","argv":["other"]}]}`, "unknown"},
		{"missing processes", `{"pane_id":"p1","shell_pid":100,"foreground_process_group_id":100}`, "unknown"},
		{"wrong pane", `{"pane_id":"p2","shell_pid":100,"foreground_process_group_id":100,"foreground_processes":[{"pid":100,"name":"sh"}]}`, "unknown"},
		{"shell plus other process", `{"pane_id":"p1","shell_pid":100,"foreground_process_group_id":100,"foreground_processes":[{"pid":100,"name":"sh"},{"pid":101,"name":"other"}]}`, "unknown"},
		{"zero pid", `{"pane_id":"p1","shell_pid":100,"foreground_process_group_id":100,"foreground_processes":[{"pid":0,"name":"sh"}]}`, "unknown"},
		{"missing group", `{"pane_id":"p1","shell_pid":100,"foreground_processes":[{"pid":100,"name":"sh"}]}`, "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := []byte(`{"type":"pane_process_info","process_info":` + test.process + `}`)
			if got := parseHerdrProcessActivity(data, "p1", &agent); got != test.want {
				t.Fatalf("got %s", got)
			}
		})
	}
}

type observationLiveRunner struct {
	environment []string
	directory   string
}

func (runner observationLiveRunner) CombinedOutput(ctx context.Context, executable string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, executable, args...)
	command.Env, command.Dir = runner.environment, runner.directory
	output := &boundedCombinedOutput{limit: maxHerdrOutputSize + 1}
	command.Stdout, command.Stderr = output, output
	err := command.Run()
	return output.Bytes(), err
}

// HOME is deliberately preserved; every Herdr/XDG path is disposable, and the
// shell runs without login startup files. No user server or agent is contacted.
func TestObserveHerdrSessionsLiveIsolated(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HERDR_OBSERVATION_INTEGRATION") != "1" {
		t.Skip("explicit disposable Herdr smoke only")
	}
	const executable = "/usr/bin/herdr"
	if _, err := os.Stat(executable); err != nil {
		t.Fatal(err)
	}
	root := observationTestRoot(t)
	for _, dir := range []string{"config/herdr", "state", "data", "cache", "runtime", "work"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config := "[terminal]\ndefault_shell = \"/bin/sh\"\nshell_mode = \"non_login\"\n[update]\nversion_check = false\nmanifest_check = false\n"
	if err := os.WriteFile(filepath.Join(root, "config/herdr/config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	home := os.Getenv("HOME")
	runner := observationLiveRunner{directory: filepath.Join(root, "work"), environment: []string{
		"HOME=" + home, "PATH=/usr/bin:/bin", "SHELL=/bin/sh", "TERM=xterm-256color",
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_STATE_HOME=" + filepath.Join(root, "state"),
		"XDG_DATA_HOME=" + filepath.Join(root, "data"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "runtime"),
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	version, err := runner.CombinedOutput(ctx, executable, "--version")
	if err != nil {
		t.Fatal(err)
	}
	server := exec.CommandContext(ctx, executable, "--session", "lab131", "server")
	server.Env, server.Dir = runner.environment, runner.directory
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Wait() }()
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		_, _ = runner.CombinedOutput(stopCtx, executable, "--session", "lab131", "server", "stop")
		select {
		case <-done:
		case <-time.After(time.Second):
			_ = server.Process.Kill()
			<-done
		}
	})
	manager := HerdrManager{Root: filepath.Join(root, "config/herdr"), Executable: executable, Runner: runner}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := manager.ObserveSessions(ctx, []string{"lab131"}, home)["lab131"]
		if got.SessionState == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not become observable: %+v", got)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, err := runner.CombinedOutput(ctx, executable, "--session", "lab131", "workspace", "create", "--cwd", runner.directory, "--label", "LAB-131 smoke", "--focus"); err != nil {
		t.Fatal(err)
	}
	got := manager.ObserveSessions(ctx, []string{"lab131", "missing"}, home)
	if got["lab131"].SessionState != "running" || got["lab131"].AgentState != "none" || got["lab131"].Directory.Directory != runner.directory || got["missing"].SessionState != "missing" {
		t.Fatalf("live shell observation: %+v", got)
	}
	if _, err := runner.CombinedOutput(ctx, executable, "--session", "lab131", "server", "stop"); err != nil {
		t.Fatal(err)
	}
	if got := manager.ObserveSessions(ctx, []string{"lab131"}, home)["lab131"]; got.SessionState != "stopped" || got.AgentState != "none" {
		t.Fatalf("stopped observation: %+v", got)
	}
	if os.Getenv("HOME") != home {
		t.Fatal("HOME changed")
	}
	t.Logf("%s: isolated live shell, directory, missing and stopped observations; HOME preserved", strings.TrimSpace(string(version)))
}
