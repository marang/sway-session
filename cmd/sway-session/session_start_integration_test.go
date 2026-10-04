package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/herdrinit"
	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/sessionrequest"
	"github.com/marang/sway-session/internal/swayipc"
)

// These tests exercise the real owner-only broker and Service, private SQLite,
// and real Sway windows. Restore is an injected owned terminal launcher, not
// ExecRestoreRunner or the restore CLI. The fixed logical Codex/shell roles are
// checked by an injected empty initializer; no authenticated agent is started.
func TestSessionStartSharedWorkspaceHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway broker acceptance")
	}
	for _, herdr := range []bool{false, true} {
		name := "owned_terminals"
		if herdr {
			name = "real_herdr_shells"
		}
		t.Run(name, func(t *testing.T) {
			f := newSessionStartHeadless(t, herdr)
			unrelated := f.unrelatedWindows()
			defer f.assertUnrelated(unrelated)
			before := f.herdrLayouts()
			first := f.send(f.requests[0])
			if !first.Created {
				t.Fatal("first exact request did not create its context")
			}
			// Reopening must focus the context leaf, even while another window
			// on the requested workspace already has focus.
			f.assertReopen(f.requests[0], *first.Context, unrelated[0].ID)
			requests := []sessionrequest.Request{
				f.requests[0], f.requests[1], f.requests[2],
				f.requests[2], f.requests[1], f.requests[0],
			}
			type outcome struct {
				request  sessionrequest.Request
				response sessionrequest.Response
				err      error
			}
			results := make(chan outcome, len(requests))
			start := make(chan struct{})
			for _, request := range requests {
				go func() {
					<-start
					response, err := sessionrequest.Send(f.h.ctx, f.socket, request)
					results <- outcome{request, response, err}
				}()
			}
			close(start)
			contexts := map[string]sessionstate.Context{f.requests[0].Session: *first.Context}
			created := map[string]int{f.requests[0].Session: 1}
			for range requests {
				result := <-results
				if result.err != nil {
					t.Errorf("concurrent request %s: %v", result.request.Session, result.err)
					continue
				}
				current := *result.response.Context
				if prior, found := contexts[result.request.Session]; found && !reflect.DeepEqual(prior, current) {
					t.Errorf("exact request returned another context: before=%+v after=%+v", prior, current)
				}
				contexts[result.request.Session] = current
				if result.response.Created {
					created[result.request.Session]++
				}
			}
			if t.Failed() {
				return
			}
			if len(contexts) != 3 || len(lifecycleHeadlessWindows(f.h.tree())) != 5 {
				t.Fatalf("want three context windows plus both unrelated windows: contexts=%d windows=%d", len(contexts), len(lifecycleHeadlessWindows(f.h.tree())))
			}
			for _, request := range f.requests {
				current := contexts[request.Session]
				if created[request.Session] != 1 {
					t.Errorf("%s created %d times, want once", request.Session, created[request.Session])
				}
				f.assertOneWindow(current.ID)
				f.assertReopen(request, current, unrelated[1].ID)
			}
			var registry sessionstate.Registry
			if err := sessionstate.RegistryStoreFor(f.h.state).LoadInto(&registry); err != nil || len(registry.Contexts) != 3 {
				t.Fatalf("requests did not leave exactly three registered contexts: %+v, %v", registry, err)
			}
			f.assertHerdrLayouts(before)
			t.Log("three exact identities created once under repeated/distinct concurrent requests; requested leaves refocused; unrelated tiled/floating windows preserved")
		})
	}
}

// Seeding the existing identity separately makes the mapped-context rejection
// observable even before creation on an occupied workspace has been fixed.
func TestSessionStartReopenHeadless(t *testing.T) {
	f := newSessionStartHeadless(t, false)
	current := sessionstate.Context{
		ID: "27000000-0000-4270-8270-000000000001", Label: f.requests[0].Label,
		Provider: f.requests[0].Provider, State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr,
			Session: f.requests[0].Session, Cwd: f.requests[0].Cwd,
			Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}},
	}
	if err := sessionstate.RegistryStoreFor(f.h.state).Save(sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{current}}); err != nil {
		t.Fatal(err)
	}
	if err := f.Restore(f.h.ctx, current.ID); err != nil {
		t.Fatal(err)
	}
	unrelated := f.unrelatedWindows()
	defer f.assertUnrelated(unrelated)
	f.assertReopen(f.requests[0], current, unrelated[0].ID)
	f.assertReopen(f.requests[0], current, unrelated[1].ID)
	f.assertOneWindow(current.ID)
}

// This separate check can run before LAB-270's workspace-policy fix: real
// named Herdr servers and clients, live interactive bash panes, and unchanged
// preexisting custom splits on exact broker retry. The initializer is empty.
func TestSessionStartHerdrRetryHeadless(t *testing.T) {
	f := newSessionStartHeadless(t, true)
	before := f.herdrLayouts()
	f.requests[0].Workspace = 99
	first := f.send(f.requests[0])
	if !first.Created {
		t.Fatal("first exact request did not create its context")
	}
	f.assertHerdrLayouts(before)
	// The first client changes terminal dimensions. All later retries must
	// preserve the established split topology and leave idle project shells.
	type outcome struct {
		response sessionrequest.Response
		err      error
	}
	results := make(chan outcome, 3)
	start := make(chan struct{})
	for range 3 {
		go func() {
			<-start
			response, err := sessionrequest.Send(f.h.ctx, f.socket, f.requests[0])
			results <- outcome{response, err}
		}()
	}
	close(start)
	for range 3 {
		result := <-results
		if result.err != nil {
			t.Errorf("concurrent exact Herdr retry: %v", result.err)
			continue
		}
		retry := result.response
		if retry.Created || !reflect.DeepEqual(retry.Context, first.Context) {
			t.Errorf("Herdr retry did not reuse its exact context: first=%+v retry=%+v", first, retry)
		}
	}
	f.assertOneWindow(first.Context.ID)
	if got := len(lifecycleHeadlessWindows(f.h.tree())); got != 1 {
		t.Fatalf("Herdr retry duplicated its terminal: windows=%d", got)
	}
	f.assertHerdrLayouts(before)
	f.assertHerdrShells()
	f.mu.Lock()
	roles := slices.Clone(f.roles[first.Context.ID])
	f.mu.Unlock()
	if !reflect.DeepEqual(roles, []string{"codex", "shell"}) {
		t.Fatalf("initializer did not receive fixed logical roles: %v", roles)
	}
	t.Log("real named Herdr client on private Sway; three independent custom shell layouts unchanged on concurrent exact retry; fixed logical roles checked by empty initializer")
}

type sessionStartHeadless struct {
	t        *testing.T
	h        *restoreCleanupHeadless
	socket   string
	herdr    bool
	requests []sessionrequest.Request
	events   *swayipc.Conn
	mu       sync.Mutex
	roles    map[sessionstate.ContextID][]string
}

func newSessionStartHeadless(t *testing.T, herdr bool) *sessionStartHeadless {
	t.Helper()
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway broker acceptance")
	}
	tools := []string{"sway", "alacritty", "sleep"}
	if herdr {
		tools = append(tools, "herdr", "bash")
	}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("private broker acceptance requires %s: %v", tool, err)
		}
	}
	h := newRestoreCleanupHeadless(t)
	// Sway already uses an explicit private config. Shells additionally get an
	// empty HOME and an allowlist without inherited Herdr/agent credentials.
	home := filepath.Join(h.root, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	h.env = slices.DeleteFunc(h.env, func(value string) bool { return strings.HasPrefix(value, "HOME=") })
	h.env = append(h.env, "HOME="+home, "SHELL=/bin/bash", "TERM=xterm-256color")
	if err := os.WriteFile(filepath.Join(h.root, "config", "alacritty.toml"), []byte("[window]\ndynamic_title = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f := &sessionStartHeadless{t: t, h: h, herdr: herdr, socket: filepath.Join(h.root, "run", "session-start.sock"), roles: make(map[sessionstate.ContextID][]string)}
	for index, name := range []string{"alpha", "beta", "gamma"} {
		cwd := filepath.Join(h.root, name)
		if err := os.Mkdir(cwd, 0700); err != nil {
			t.Fatal(err)
		}
		f.requests = append(f.requests, sessionrequest.Request{Version: sessionrequest.ProtocolVersion,
			Session: "lab270-" + name, Cwd: cwd, Label: fmt.Sprintf("LAB-270 %d", index), Provider: "linear", Workspace: 98})
	}
	if herdr {
		f.startHerdrSessions()
	}
	service := &sessionrequest.Service{
		StateRoot: h.state, NewContextID: sessionstate.NewContextID,
		NewSway: func() sessionrequest.SwayRequester { return swayipc.NewClient(h.socket) }, Restore: f,
		Initializer: sessionRequestTerminalInitializer{stateRoot: h.state, manager: herdrTerminalSessionManager{
			paths: func() (sessionstate.HerdrPaths, error) {
				return sessionstate.HerdrPaths{Root: filepath.Join(h.root, "config", "herdr"), ConfigFile: filepath.Join(h.root, "config", "herdr", "config.toml")}, nil
			},
			resolveProgram: func(string) (string, error) { return "/usr/bin/herdr", nil },
			initialize: func(_ context.Context, current sessionstate.Context, roles []string, _ herdrinit.Runner) (herdrinit.Result, error) {
				if !reflect.DeepEqual(roles, []string{"codex", "shell"}) {
					return herdrinit.Result{}, fmt.Errorf("unexpected logical roles: %v", roles)
				}
				f.mu.Lock()
				f.roles[current.ID] = slices.Clone(roles)
				f.mu.Unlock()
				return herdrinit.Result{ContextID: current.ID, Session: current.Launcher.Session, Roles: roles, Reason: "private acceptance empty initializer"}, nil
			},
		}},
	}
	server, err := sessionrequest.StartServer(f.socket, service.Handle, func(err error) { t.Logf("private broker: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	// Read only after bounded request batches. The fixture has at most five
	// windows; a tick drains all preceding window events before assertions.
	f.events, err = swayipc.OpenSubscriptionContext(h.ctx, h.socket, []byte(`["window","tick"]`), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.events.Close() })
	return f
}

func (f *sessionStartHeadless) send(request sessionrequest.Request) sessionrequest.Response {
	f.t.Helper()
	response, err := sessionrequest.Send(f.h.ctx, f.socket, request)
	if err != nil {
		f.t.Fatalf("broker request %s on workspace %d: %v", request.Session, request.Workspace, err)
	}
	return response
}

func (f *sessionStartHeadless) Restore(ctx context.Context, id sessionstate.ContextID) error {
	var registry sessionstate.Registry
	if err := sessionstate.RegistryStoreFor(f.h.state).LoadIntoContext(ctx, &registry); err != nil {
		return err
	}
	index, err := sessionstate.ResolveContext(registry, string(id))
	if err != nil {
		return err
	}
	current := registry.Contexts[index]
	appID, err := id.AppID()
	if err != nil {
		return err
	}
	args := []string{"/usr/bin/sleep", "120"}
	if f.herdr {
		args = []string{"/usr/bin/herdr", "--session", current.Launcher.Session}
	}
	_, err = f.terminal(ctx, appID, current.Launcher.Cwd, args...)
	return err
}

// Return errors instead of Fatal/Goexit: Restore runs in the broker's worker.
// The reused harness owns the compositor; this helper owns each terminal group.
func (f *sessionStartHeadless) terminal(ctx context.Context, appID, cwd string, command ...string) (int64, error) {
	args := []string{"--config-file", filepath.Join(f.h.root, "config", "alacritty.toml"), "--class", appID, "--title", "LAB-270 private acceptance", "-e"}
	args = append(args, command...)
	process := exec.CommandContext(f.h.ctx, "/usr/bin/alacritty", args...)
	process.Env, process.Dir = f.h.env, cwd
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	process.Cancel = func() error { return syscall.Kill(-process.Process.Pid, syscall.SIGKILL) }
	if err := process.Start(); err != nil {
		return 0, err
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	f.t.Cleanup(func() {
		select {
		case <-done:
			return
		default:
		}
		_ = syscall.Kill(-process.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(time.Second):
			_ = syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
			select {
			case <-done:
			case <-time.After(time.Second):
				f.t.Error("private acceptance terminal did not terminate")
			}
		}
	})
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		tree, err := requestTree(deadline, f.h.client)
		if err != nil {
			return 0, err
		}
		for id, node := range lifecycleHeadlessWindows(tree) {
			if *node.AppID == appID && node.PID == process.Process.Pid {
				return id, nil
			}
		}
		select {
		case <-done:
			return 0, fmt.Errorf("private terminal exited before mapping %s", appID)
		case <-deadline.Done():
			return 0, fmt.Errorf("private terminal did not map %s: %w", appID, deadline.Err())
		case <-ticker.C:
		}
	}
}

func (f *sessionStartHeadless) assertOneWindow(id sessionstate.ContextID) int64 {
	f.t.Helper()
	appID, err := id.AppID()
	if err != nil {
		f.t.Fatal(err)
	}
	var matches []int64
	for container, node := range lifecycleHeadlessWindows(f.h.tree()) {
		if *node.AppID == appID {
			matches = append(matches, container)
		}
	}
	if len(matches) != 1 {
		f.t.Fatalf("context %s has %d windows, want one", id, len(matches))
	}
	return matches[0]
}

func (f *sessionStartHeadless) assertReopen(request sessionrequest.Request, current sessionstate.Context, neighbor int64) {
	f.t.Helper()
	target := f.assertOneWindow(current.ID)
	f.h.command(fmt.Sprintf("[con_id=%d] focus", neighbor))
	if focusedContainerID(f.h.tree()) != neighbor {
		f.t.Fatal("fixture neighbor was not focused")
	}
	response := f.send(request)
	if response.Created || !reflect.DeepEqual(response.Context, &current) {
		f.t.Fatalf("reopen did not reuse the exact context: %+v", response)
	}
	tree := f.h.tree()
	if got := focusedContainerID(tree); got != target {
		f.t.Fatalf("reopen focused container %d, want requested container %d", got, target)
	}
	if got := restoreCleanupWorkspace(tree, target); got != fmt.Sprint(request.Workspace) {
		f.t.Fatalf("reopen placed requested container on workspace %s, want %d", got, request.Workspace)
	}
	f.mu.Lock()
	roles := slices.Clone(f.roles[current.ID])
	f.mu.Unlock()
	if !reflect.DeepEqual(roles, []string{"codex", "shell"}) {
		f.t.Fatalf("initializer did not receive fixed logical roles: %v", roles)
	}
}

func (f *sessionStartHeadless) unrelatedWindows() []*Node {
	f.t.Helper()
	f.h.command("workspace 98")
	var windows []*Node
	for _, name := range []string{"tiled", "floating"} {
		id, err := f.terminal(f.h.ctx, "lab270-unrelated-"+name, f.h.root, "/usr/bin/sleep", "120")
		if err != nil {
			f.t.Fatal(err)
		}
		f.h.command(fmt.Sprintf("[con_id=%d] mark --add lab270-preserve-%s", id, name))
		if name == "floating" {
			f.h.command(fmt.Sprintf("[con_id=%d] floating enable", id))
		}
		windows = append(windows, findContainerByID(f.h.tree(), id))
	}
	// Drain fixture creation and floating events before checking broker effects.
	f.assertUnrelated(windows)
	return windows
}

func (f *sessionStartHeadless) assertUnrelated(before []*Node) {
	f.t.Helper()
	tree := f.h.tree()
	for _, old := range before {
		current := findContainerByID(tree, old.ID)
		if current == nil || current.PID != old.PID || !reflect.DeepEqual(current.AppID, old.AppID) ||
			!reflect.DeepEqual(current.Marks, old.Marks) || restoreCleanupWorkspace(tree, old.ID) != "98" {
			f.t.Errorf("broker changed unrelated window %d: before=%+v after=%+v", old.ID, old, current)
		}
	}
	workspace := restoreCleanupNamedWorkspace(tree, "98")
	if workspace == nil || len(workspace.FloatingNodes) != 1 || findContainerByID(workspace.FloatingNodes[0], before[1].ID) == nil {
		f.t.Error("broker changed unrelated floating placement")
	}
	f.h.tick++
	barrier := fmt.Sprintf("lab270-acceptance-%d", f.h.tick)
	message, err := f.h.client.RequestContext(f.h.ctx, swayipc.SendTick, []byte(barrier))
	if err == nil {
		err = swayipc.CheckSendTickResponse(message)
	}
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(f.h.ctx, 3*time.Second)
	defer cancel()
	for range 512 {
		message, err := f.events.ReadContext(ctx)
		if err != nil {
			f.t.Fatalf("private window-event barrier: %v", err)
		}
		event, err := swayipc.DecodeEvent(message)
		if err != nil {
			f.t.Fatal(err)
		}
		if event.Type == swayipc.EventTick && event.Payload == barrier {
			return
		}
		if event.Type == swayipc.EventWindow && event.Container != nil && (event.Change == "close" || event.Change == "move") {
			for _, old := range before {
				if event.Container.ID == old.ID {
					f.t.Errorf("broker emitted %s for unrelated window %d", event.Change, old.ID)
				}
			}
		}
	}
	f.t.Fatal("private window-event stream exceeded acceptance bound")
}

func (f *sessionStartHeadless) startHerdrSessions() {
	f.t.Helper()
	root := filepath.Join(f.h.root, "config", "herdr")
	if err := os.Mkdir(root, 0700); err != nil {
		f.t.Fatal(err)
	}
	config := "onboarding = false\n[terminal]\ndefault_shell = \"/bin/bash\"\nshell_mode = \"non_login\"\n[update]\nversion_check = false\nmanifest_check = false\n[ui.sound]\nenabled = false\n[experimental]\npane_history = false\n"
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(config), 0600); err != nil {
		f.t.Fatal(err)
	}
	for index, request := range f.requests {
		if err := sessionstate.ValidateHerdrSessionSocketPaths(root, request.Session); err != nil {
			f.t.Fatal(err)
		}
		f.h.start("herdr", "--session", request.Session, "server")
		f.t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _ = f.herdrCommand(ctx, request, "server", "stop")
		})
		f.h.until("private named Herdr socket", func() bool {
			info, err := os.Lstat(filepath.Join(root, "sessions", request.Session, "herdr.sock"))
			return err == nil && info.Mode()&os.ModeSocket != 0
		})
		output, err := f.herdrCommand(f.h.ctx, request, "workspace", "create", "--cwd", request.Cwd, "--label", request.Label, "--focus")
		if err != nil {
			f.t.Fatal(err)
		}
		var created struct {
			Result struct {
				RootPane struct {
					PaneID string `json:"pane_id"`
				} `json:"root_pane"`
			} `json:"result"`
		}
		if err := json.Unmarshal(output, &created); err != nil || created.Result.RootPane.PaneID == "" {
			f.t.Fatalf("private Herdr workspace did not identify its root pane: %s, %v", output, err)
		}
		direction := "right"
		if index == 1 {
			direction = "down"
		}
		if _, err := f.herdrCommand(f.h.ctx, request, "pane", "split", created.Result.RootPane.PaneID, "--direction", direction, "--ratio", "0.35", "--cwd", request.Cwd, "--no-focus"); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *sessionStartHeadless) herdrCommand(ctx context.Context, request sessionrequest.Request, arguments ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/herdr", append([]string{"--session", request.Session}, arguments...)...)
	command.Env, command.Dir = f.h.env, request.Cwd
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("private Herdr %s: %w: %s", request.Session, err, output)
	}
	return output, nil
}

// Pixel/cell rectangles change when Sway resizes a terminal or a client first
// attaches. Stable IDs, focus, zoom, split directions and ratios are the layout
// contract. Capture only that contract, never terminal contents or history.
type sessionStartHerdrLayout struct {
	Workspace string `json:"workspace_id"`
	Tab       string `json:"tab_id"`
	Focused   string `json:"focused_pane_id"`
	Zoomed    bool   `json:"zoomed"`
	Panes     []struct {
		ID string `json:"pane_id"`
	} `json:"panes"`
	Splits []struct {
		ID        string  `json:"id"`
		Direction string  `json:"direction"`
		Ratio     float64 `json:"ratio"`
	} `json:"splits"`
}

func (f *sessionStartHeadless) herdrLayouts() map[string][]sessionStartHerdrLayout {
	f.t.Helper()
	layouts := make(map[string][]sessionStartHerdrLayout)
	if !f.herdr {
		return layouts
	}
	for _, request := range f.requests {
		output, err := f.herdrCommand(f.h.ctx, request, "api", "snapshot")
		if err != nil {
			f.t.Fatal(err)
		}
		var response struct {
			Result struct {
				Snapshot struct {
					Layouts []sessionStartHerdrLayout `json:"layouts"`
					Panes   []struct {
						PaneID string  `json:"pane_id"`
						Agent  *string `json:"agent"`
					} `json:"panes"`
				} `json:"snapshot"`
			} `json:"result"`
		}
		if err := json.Unmarshal(output, &response); err != nil || len(response.Result.Snapshot.Panes) != 2 || len(response.Result.Snapshot.Layouts) != 1 {
			f.t.Fatalf("private Herdr session does not have its two custom shell panes: %s, %v", output, err)
		}
		for _, pane := range response.Result.Snapshot.Panes {
			if pane.PaneID == "" || pane.Agent != nil {
				f.t.Fatalf("private Herdr pane unexpectedly hosts an agent: %+v", pane)
			}
		}
		layout := response.Result.Snapshot.Layouts[0]
		if len(layout.Panes) != 2 || len(layout.Splits) != 1 || layout.Splits[0].Ratio != 0.35 {
			f.t.Fatalf("private Herdr custom split changed: %+v", layout)
		}
		layouts[request.Session] = response.Result.Snapshot.Layouts
	}
	return layouts
}

func (f *sessionStartHeadless) assertHerdrLayouts(before map[string][]sessionStartHerdrLayout) {
	f.t.Helper()
	after := f.herdrLayouts()
	if !reflect.DeepEqual(before, after) {
		f.t.Fatalf("broker retry changed independent Herdr layouts: before=%+v after=%+v", before, after)
	}
}

func (f *sessionStartHeadless) assertHerdrShells() {
	f.t.Helper()
	layouts := f.herdrLayouts()
	seen := make(map[int]bool)
	for _, request := range f.requests {
		for _, layout := range layouts[request.Session] {
			for _, pane := range layout.Panes {
				output, err := f.herdrCommand(f.h.ctx, request, "pane", "process-info", "--pane", pane.ID)
				if err != nil {
					f.t.Fatal(err)
				}
				var response struct {
					Result struct {
						Type string `json:"type"`
						Info struct {
							PaneID     string `json:"pane_id"`
							ShellPID   int    `json:"shell_pid"`
							Foreground int    `json:"foreground_process_group_id"`
							Processes  []struct {
								PID int    `json:"pid"`
								Cwd string `json:"cwd"`
							} `json:"foreground_processes"`
						} `json:"process_info"`
					} `json:"result"`
				}
				if err := json.Unmarshal(output, &response); err != nil {
					f.t.Fatal(err)
				}
				info := response.Result.Info
				if response.Result.Type != "pane_process_info" || info.PaneID != pane.ID || info.ShellPID == 0 || info.Foreground != info.ShellPID ||
					len(info.Processes) != 1 || info.Processes[0].PID != info.ShellPID || info.Processes[0].Cwd != request.Cwd {
					f.t.Fatalf("private Herdr pane is not its idle interactive project shell: %s", output)
				}
				if seen[info.ShellPID] {
					f.t.Fatalf("independent named Herdr sessions share shell process %d", info.ShellPID)
				}
				seen[info.ShellPID] = true
			}
		}
	}
}
