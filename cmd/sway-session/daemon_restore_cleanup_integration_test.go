package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// This test never discovers or uses the login session's compositor. Opt in with:
// SWAY_SESSION_HEADLESS_INTEGRATION=1 go test ./cmd/sway-session -run '^TestSessionRuntimeRestoreCleanupHeadless$' -count=1 -v
func TestSessionRuntimeRestoreCleanupHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway/Alacritty integration")
	}
	for _, name := range []string{"sway", "alacritty", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("headless integration requires %s: %v", name, err)
		}
	}
	for _, manualMove := range []bool{false, true} {
		name := "focus_cancellation"
		if manualMove {
			name = "preserve_manual_workspace_move"
		}
		t.Run(name, func(t *testing.T) {
			headless := newRestoreCleanupHeadless(t)
			ids := []sessionstate.ContextID{testManagedContextID, "6ba7b810-9dad-11d1-80b4-00c04fd430c8"}
			registry := sessionRegistryIDs(ids...)
			for index := range registry.Contexts {
				registry.Contexts[index].Launcher.Cwd = headless.root
			}
			if err := sessionstate.RegistryStoreFor(headless.state).Save(registry); err != nil {
				t.Fatal(err)
			}
			desired := exactDaemonSnapshot("98", ids...)
			desired.Workspaces[0].Tiling.Layout = sessionstate.LayoutTabbed
			if err := sessionstate.LayoutStoreFor(headless.state).Save(desired); err != nil {
				t.Fatal(err)
			}

			headless.command("workspace 98")
			owned := make([]int64, 0, len(ids))
			for _, id := range ids {
				owned = append(owned, headless.terminal(id))
			}
			// Keep focus off the source workspace while staging. The explicit
			// focus change below supplies real cancellation intent through IPC.
			headless.command("workspace 99")
			requester := &restoreCleanupRealRequester{Client: headless.client}
			streamState := &swayipc.EventStreamState{}
			runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
				Context: headless.ctx, Root: headless.state, EventStreamState: streamState,
			})
			if err != nil {
				t.Fatal(err)
			}
			headless.subscribe(runtime, streamState)
			stageCount := 1
			if manualMove {
				stageCount = 2
			}
			// Deliberately queue events across a bounded burst of reconciliation,
			// as happens when intent arrives while a restore command is in flight.
			now := time.Now()
			for attempt := 0; ; attempt++ {
				tree := headless.tree()
				staged := 0
				for _, containerID := range owned {
					if restoreCleanupWorkspace(tree, containerID) == sessionstate.RestoreStagingWorkspace {
						staged++
					}
				}
				if staged == stageCount {
					break
				}
				if attempt == 12 {
					t.Fatalf("restore never staged %d owned windows; commands: %q", stageCount, requester.commands)
				}
				if _, err := runtime.Reconcile(tree, now); err != nil {
					t.Fatal(err)
				}
				// Staging removes contexts from startup capture readiness. Advance
				// the injected clock through its settle gate without a real sleep.
				now = now.Add(sessionStartupSettleDelay)
			}
			if runtime.restoreProgress == nil {
				t.Fatal("no active structural restore before cancellation")
			}
			if manualMove {
				// Observe ownership and focus immediately before the user's move.
				tree := headless.tree()
				if restoreCleanupWorkspace(tree, owned[0]) != sessionstate.RestoreStagingWorkspace {
					t.Fatal("manual move target is not an owned staged window")
				}
				headless.command(fmt.Sprintf("[con_id=%d] move container to workspace 99", owned[0]))
			}
			headless.command("workspace 98")
			events := headless.drain(runtime)
			if !slices.ContainsFunc(events, func(event swayipc.Event) bool {
				return event.Type == swayipc.EventWorkspace && event.Change == "focus" && event.Current != nil && event.Current.Name == "98"
			}) {
				t.Fatal("did not receive the real workspace focus cancellation event")
			}
			if runtime.restoreProgress != nil || !runtime.startupComplete {
				t.Fatal("real user event did not cancel structural restoration")
			}
			commandsBeforeCleanup := len(requester.commands)
			moveEvents := 0
			// Continue past initial convergence to catch late event cancellation
			// dropping the cleanup ledger or accidentally rearming reconstruction.
			for range 12 {
				if _, err := runtime.Reconcile(headless.tree(), time.Now()); err != nil {
					t.Fatal(err)
				}
				for _, event := range headless.drain(runtime) {
					if event.Type == swayipc.EventWindow && event.Change == "move" && event.Container != nil && slices.Contains(owned, event.Container.ID) {
						moveEvents++
					}
				}
			}
			if moveEvents == 0 {
				t.Fatal("cleanup did not process any real owned-window move event")
			}
			tree := headless.tree()
			for index, containerID := range owned {
				want := "98"
				if manualMove && index == 0 {
					want = "99"
				}
				if got := restoreCleanupWorkspace(tree, containerID); got != want {
					t.Fatalf("owned window %d is on %q, want %q", containerID, got, want)
				}
				node := findContainerByID(tree, containerID)
				mark, err := ids[index].Mark()
				if err != nil {
					t.Fatal(err)
				}
				if node == nil || !slices.Contains(node.Marks, mark) || (index == 0 && !slices.Contains(node.Marks, "lab143-preserve")) {
					t.Fatalf("cleanup lost persistent marks on window %d: %+v", containerID, node)
				}
			}
			for _, command := range requester.commands[commandsBeforeCleanup:] {
				// Only compensating returns are needed in this fixture. Any layout,
				// focus, mark deletion, or move to another target violates intent.
				if !strings.HasSuffix(command, `] move container to workspace "98"`) {
					t.Fatalf("unexpected command after cancellation: %q", command)
				}
			}
			if workspace := restoreCleanupNamedWorkspace(tree, "98"); workspace == nil || workspace.Layout != "splith" {
				t.Fatalf("cancelled tabbed layout was rebuilt: %+v", workspace)
			}
			if workspace := restoreCleanupNamedWorkspace(tree, sessionstate.RestoreStagingWorkspace); workspace != nil && (len(workspace.Nodes) != 0 || len(workspace.FloatingNodes) != 0) {
				t.Fatal("cleanup left windows in the reserved staging workspace")
			}
			t.Logf("staged=%d; real cleanup move events=%d; manual move preserved=%t; persistent marks and splith layout preserved", stageCount, moveEvents, manualMove)
		})
	}
}

// The wrapper records runtime commands while forwarding every request to Sway.
type restoreCleanupRealRequester struct {
	*swayipc.Client
	commands []string
}

func (client *restoreCleanupRealRequester) RequestContext(ctx context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if kind == swayipc.RunCommand {
		client.commands = append(client.commands, string(payload))
	}
	return client.Client.RequestContext(ctx, kind, payload)
}

type restoreCleanupHeadless struct {
	t      *testing.T
	ctx    context.Context
	root   string
	state  string
	socket string
	env    []string
	client *swayipc.Client
	events chan swayipc.Event
	tick   int
}

func newRestoreCleanupHeadless(t *testing.T) *restoreCleanupHeadless {
	t.Helper()
	// Short paths keep both Unix socket names below sockaddr_un's limit.
	root, err := os.MkdirTemp("/tmp", "lab143-sway-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)
	h := &restoreCleanupHeadless{t: t, ctx: ctx, root: root, state: filepath.Join(root, "state", "sway-session")}
	// An allowlist prevents inherited SWAYSOCK, WAYLAND_DISPLAY, DISPLAY,
	// DBus, configuration overrides, and desktop autostart hooks from escaping.
	h.env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "WLR_BACKENDS=headless", "WLR_HEADLESS_OUTPUTS=1", "WLR_RENDERER=pixman", "LIBGL_ALWAYS_SOFTWARE=1"}
	// Preserve HOME; private XDG roots and explicit Sway/Alacritty configs
	// isolate application settings. The terminal runs sleep without a shell.
	if value, present := os.LookupEnv("HOME"); present {
		h.env = append(h.env, "HOME="+value)
	}
	for _, entry := range []struct{ key, directory string }{
		{"XDG_RUNTIME_DIR", "run"}, {"XDG_CONFIG_HOME", "config"},
		{"XDG_STATE_HOME", "state"}, {"XDG_CACHE_HOME", "cache"}, {"XDG_DATA_HOME", "data"},
	} {
		path := filepath.Join(root, entry.directory)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		h.env = append(h.env, entry.key+"="+path)
	}
	config := filepath.Join(root, "config", "sway.conf")
	if err := os.WriteFile(config, []byte("xwayland disable\noutput HEADLESS-1 mode 1280x720\nworkspace 98 output HEADLESS-1\nworkspace 99 output HEADLESS-1\ndefault_orientation horizontal\nfocus_follows_mouse no\nworkspace 98\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sway := h.start("sway", "--config", config)
	h.socket = filepath.Join(root, "run", fmt.Sprintf("sway-ipc.%d.%d.sock", os.Getuid(), sway.Process.Pid))
	wayland := ""
	h.until("private compositor sockets", func() bool {
		info, err := os.Lstat(h.socket)
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			return false
		}
		paths, err := filepath.Glob(filepath.Join(root, "run", "wayland-*"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			info, err := os.Lstat(path)
			if err == nil && info.Mode()&os.ModeSocket != 0 {
				wayland = filepath.Base(path)
				return true
			}
		}
		return false
	})
	h.env = append(h.env, "WAYLAND_DISPLAY="+wayland, "SWAYSOCK="+h.socket, "WINIT_UNIX_BACKEND=wayland")
	h.client = swayipc.NewClient(h.socket)
	t.Cleanup(h.client.Close)
	return h
}

func (h *restoreCleanupHeadless) start(name string, args ...string) *exec.Cmd {
	h.t.Helper()
	log, err := os.CreateTemp(h.root, name+"-*.log")
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() {
		_ = log.Close()
		if h.t.Failed() {
			contents, _ := os.ReadFile(log.Name())
			h.t.Logf("%s log:\n%s", name, contents)
		}
	})
	command := exec.CommandContext(h.ctx, name, args...)
	command.Env, command.Dir = h.env, h.root
	command.Stdout, command.Stderr = log, log
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	if err := command.Start(); err != nil {
		h.t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	h.t.Cleanup(func() {
		select {
		case <-finished:
			return
		default:
		}
		// Signal only the private process group created by this test, including
		// Alacritty's sleep child. Wait reaps the direct child in every case.
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				h.t.Errorf("%s did not terminate", name)
			}
		}
	})
	return command
}

func (h *restoreCleanupHeadless) until(description string, ready func() bool) {
	h.t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for !ready() {
		select {
		case <-h.ctx.Done():
			h.t.Fatalf("waiting for %s: %v", description, h.ctx.Err())
		case <-deadline.C:
			h.t.Fatalf("timed out waiting for %s", description)
		case <-ticker.C:
		}
	}
}

func (h *restoreCleanupHeadless) command(command string) {
	h.t.Helper()
	message, err := h.client.RequestContext(h.ctx, swayipc.RunCommand, []byte(command))
	if err == nil {
		err = swayipc.CheckRunCommandResponse(message)
	}
	if err != nil {
		h.t.Fatalf("private Sway command %q: %v", command, err)
	}
}

func (h *restoreCleanupHeadless) tree() *Node {
	h.t.Helper()
	tree, err := requestTree(h.ctx, h.client)
	if err != nil {
		h.t.Fatal(err)
	}
	return tree
}

func (h *restoreCleanupHeadless) terminal(id sessionstate.ContextID) int64 {
	h.t.Helper()
	appID, err := id.AppID()
	if err != nil {
		h.t.Fatal(err)
	}
	config := filepath.Join(h.root, "config", "alacritty.toml")
	if err := os.WriteFile(config, []byte("[window]\ndynamic_title = false\n"), 0600); err != nil {
		h.t.Fatal(err)
	}
	command := h.start("alacritty", "--config-file", config, "--class", appID, "--title", "LAB-143 private test", "-e", "/usr/bin/sleep", "120")
	var containerID int64
	h.until("owned Alacritty window", func() bool {
		var visit func(*Node)
		visit = func(node *Node) {
			if node.AppID != nil && *node.AppID == appID && node.PID == command.Process.Pid {
				containerID = node.ID
			}
			for _, child := range append(slices.Clone(node.Nodes), node.FloatingNodes...) {
				visit(child)
			}
		}
		visit(h.tree())
		return containerID != 0
	})
	if id == testManagedContextID {
		h.command(fmt.Sprintf(`[con_id=%d] mark --add "lab143-preserve"`, containerID))
	}
	return containerID
}

func (h *restoreCleanupHeadless) subscribe(runtime *sessionRuntime, state *swayipc.EventStreamState) {
	h.t.Helper()
	h.events = make(chan swayipc.Event, 256)
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		swayipc.StreamSessionEventsWithState(h.socket, h.events, done, state)
	}()
	h.t.Cleanup(func() {
		close(done)
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			h.t.Error("private Sway event subscription did not stop")
		}
	})
	select {
	case event := <-h.events:
		if event.Type != swayipc.EventStream || event.Change != "ready" {
			h.t.Fatalf("unexpected subscription event: %+v", event)
		}
		runtime.HandleEvent(event, time.Now())
	case <-time.After(5 * time.Second):
		h.t.Fatal("private Sway event subscription not ready")
	}
}

func (h *restoreCleanupHeadless) drain(runtime *sessionRuntime) []swayipc.Event {
	h.t.Helper()
	h.tick++
	barrier := fmt.Sprintf("lab143-integration-%d", h.tick)
	message, err := h.client.RequestContext(h.ctx, swayipc.SendTick, []byte(barrier))
	if err == nil {
		err = swayipc.CheckSendTickResponse(message)
	}
	if err != nil {
		h.t.Fatal(err)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var events []swayipc.Event
	for {
		select {
		case event := <-h.events:
			if event.Type == swayipc.EventShutdown || event.Type == swayipc.EventStream {
				h.t.Fatalf("private event stream lost continuity: %+v", event)
			}
			runtime.HandleEvent(event, time.Now())
			events = append(events, event)
			if event.Type == swayipc.EventTick && event.Payload == barrier {
				return events
			}
		case <-timer.C:
			h.t.Fatal("timed out draining real Sway events through tick barrier")
		}
	}
}

func restoreCleanupWorkspace(root *Node, id int64) string {
	if root.Type == "workspace" && findContainerByID(root, id) != nil {
		return root.Name
	}
	for _, child := range append(slices.Clone(root.Nodes), root.FloatingNodes...) {
		if workspace := restoreCleanupWorkspace(child, id); workspace != "" {
			return workspace
		}
	}
	return ""
}

func restoreCleanupNamedWorkspace(root *Node, name string) *Node {
	if root.Type == "workspace" && root.Name == name {
		return root
	}
	for _, child := range root.Nodes {
		if workspace := restoreCleanupNamedWorkspace(child, name); workspace != nil {
			return workspace
		}
	}
	return nil
}
