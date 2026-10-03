package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// Opt-in connected vertical slice, not a Chrome matrix or VM acceptance:
// SWAY_SESSION_DESKTOP_ACCEPTANCE=1 GOTOOLCHAIN=go1.26.5 GOMAXPROCS=4 GOFLAGS=-p=1 go test ./cmd/sway-session -run '^TestDesktopAcceptanceChromePrivate$' -count=1 -timeout=5m -v
// Repeat that command with -race after it is green. Each protocol is limited to
// two minutes. Installed versions and stock catalog parsing are observations;
// only a valid disposable user entry is registered. Stock launcher acceptance
// is not proven, even if a future package's entry parses successfully.
// Production launcher preparation executes fixed real /usr/bin/gio through the
// fixture-only ownership starter in desktop_acceptance_lab95_helper_test.go.
// No downloads, profile contents, system desktop edits, or login-session IPC.
func TestDesktopAcceptanceChromePrivate(t *testing.T) {
	if os.Getenv("SWAY_SESSION_DESKTOP_ACCEPTANCE") != "1" {
		t.Skip("set SWAY_SESSION_DESKTOP_ACCEPTANCE=1 for sandboxed real Chrome/private Sway acceptance")
	}
	// exec.Command resolves names before applying the child environment.
	t.Setenv("PATH", "/usr/bin:/bin")
	for _, name := range []string{"sway", "alacritty", "dbus-daemon", "Xwayland", "printenv"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("opt-in acceptance prerequisite %s: %v", name, err)
		}
	}
	root := t.TempDir()
	for _, directory := range []string{"home", "config", "cache", "data", "profile", "state"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// HOME must be private BEFORE newScratchpadHeadless starts the compositor;
	// its inherited harness otherwise explicitly carries the user's real HOME.
	t.Setenv("HOME", filepath.Join(root, "home"))
	overall, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	for _, path := range []string{"/usr/bin/google-chrome-stable", "/usr/bin/sway"} {
		if resolved, err := sessionstate.ResolveRootOwnedSystemExecutable(filepath.Base(path)); err != nil || resolved != path {
			t.Fatalf("trusted acceptance executable %s unavailable: resolved=%q error=%v", path, resolved, err)
		}
		versionContext, cancelVersion := context.WithTimeout(overall, 5*time.Second)
		command := exec.CommandContext(versionContext, path, "--version")
		command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + filepath.Join(root, "home"), "XDG_CONFIG_HOME=" + filepath.Join(root, "config")}
		output, err := command.Output()
		cancelVersion()
		if err != nil {
			t.Fatalf("acceptance executable %s --version: %v", path, err)
		}
		t.Logf("installed %s version: %.160q", path, strings.TrimSpace(string(output)))
	}
	if gio, err := sessionstate.ResolveRootOwnedSystemExecutable("gio"); err != nil || gio != "/usr/bin/gio" {
		t.Fatalf("fixed trusted GIO unavailable: %q %v", gio, err)
	}
	for _, protocol := range []sessionstate.WindowProtocol{sessionstate.WindowWayland, sessionstate.WindowXWayland} {
		t.Run(string(protocol), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(overall, 2*time.Minute)
			defer cancel()
			lab95ChromeProtocol(t, ctx, protocol)
		})
	}
}

func lab95ChromeProtocol(t *testing.T, ctx context.Context, protocol sessionstate.WindowProtocol) {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{"home", "config", "cache", "data", "profile", "state"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(root, "state", "sway-session")
	entryPath := filepath.Join(root, "home", ".local", "share", "applications", "lab95-chrome.desktop")
	if err := os.MkdirAll(filepath.Dir(entryPath), 0700); err != nil {
		t.Fatal(err)
	}
	platform := "wayland"
	if protocol == sessionstate.WindowXWayland {
		platform = "x11"
	}
	entry := fmt.Sprintf("[Desktop Entry]\nType=Application\nName=LAB95 Disposable Chrome\nExec=/usr/bin/google-chrome-stable --ozone-platform=%s --user-data-dir=%s --no-first-run --no-default-browser-check --password-store=basic --disable-background-networking --disable-component-update --disable-sync --disable-extensions --disable-gpu about:blank\nStartupWMClass=google-chrome\n", platform, filepath.Join(root, "profile"))
	if err := os.WriteFile(entryPath, []byte(entry), 0600); err != nil {
		t.Fatal(err)
	}
	var registered sessionstate.Context
	var captured sessionstate.LayoutSnapshot
	var oldSocket, oldCompositor, oldRoot string
	if !t.Run("focused_registration_and_capture", func(t *testing.T) {
		h := lab95PrivateHeadless(t, ctx, protocol, root)
		h.state = state
		oldSocket, oldRoot = h.socket, h.root
		var err error
		oldCompositor, err = compositorIdentity(h.socket)
		if err != nil {
			t.Fatal(err)
		}
		starter := lab95NewGIOStarter(t, h, root)
		// Bootstrap an already-running application through real GIO, before the
		// user explicitly approves its registration. Runtime capture must adopt it.
		if err := starter.Start(sessionstate.ProcessSpec{Name: "/usr/bin/gio", Arguments: []string{"launch", entryPath}, Environment: []string{"PATH=/usr/local/bin:/usr/bin"}}); err != nil {
			t.Fatal(err)
		}
		window, identity := lab95ChromeWindow(t, h, starter, protocol)
		entry = strings.Replace(entry, "StartupWMClass=google-chrome", "StartupWMClass="+lab95IdentityClass(identity), 1)
		if err := os.WriteFile(entryPath, []byte(entry), 0600); err != nil {
			t.Fatal(err)
		}
		catalog := lab95DesktopCatalog(t)
		lab95ReportStockCatalog(t, catalog)
		local, exists := catalog.ByID("lab95-chrome.desktop")
		if !exists || local.Origin != sessionstate.DesktopEntryUser || local.Path != entryPath {
			t.Fatal("actual XDG catalog did not resolve the disposable user entry")
		}
		h.command(fmt.Sprintf("[con_id=%d] focus", window))
		if focusedContainerID(h.tree()) != window || restoreCleanupWorkspace(h.tree(), window) != "98" {
			t.Fatal("registration did not focus the owned Chrome on workspace 98")
		}
		deps := defaultDependencies(strings.NewReader(""))
		var stdout, stderr bytes.Buffer
		args := []string{"app", "register-focused", "--yes", "--desktop-id", "lab95-chrome.desktop", "--socket", h.socket}
		if code := runWithContext(h.ctx, args, strings.NewReader(""), &stdout, &stderr, deps); code != exitSuccess {
			t.Fatalf("explicit focused registration: code=%d stderr=%s", code, stderr.String())
		}
		registry := lab95LoadRegistry(t, state)
		if len(registry.Contexts) != 1 {
			t.Fatalf("focused registration count=%d, want one", len(registry.Contexts))
		}
		registered = registry.Contexts[0]
		expectedIdentity := identity
		expectedIdentity.StartupWMClass = local.StartupWMClass
		if registered.App == nil || registered.App.Identity != expectedIdentity || !registered.App.DesiredOpen || registered.App.RestorePolicy != sessionstate.ApplicationRestoreFollow {
			t.Fatalf("registration did not preserve observed identity and follow/open intent: observed=%+v registered=%+v", identity, registered.App)
		}
		lab95AssertApproval(t, root, registered, []byte(entry))
		stdout.Reset()
		stderr.Reset()
		if code := runWithContext(h.ctx, args, strings.NewReader(""), &stdout, &stderr, deps); code != exitSuccess || !strings.Contains(stdout.String(), "already registered") {
			t.Fatalf("repeat focused registration: code=%d stderr=%s", code, stderr.String())
		}
		if !reflect.DeepEqual(lab95LoadRegistry(t, state), registry) {
			t.Fatal("repeat registration changed durable registry")
		}
		controls := lab95OrdinaryControls(h, protocol)
		requester := &restoreCleanupRealRequester{Client: h.client}
		now := time.Now()
		stream := &swayipc.EventStreamState{}
		runtime := lab95Runtime(t, h, requester, starter, registered, oldCompositor, &now, stream)
		h.subscribe(runtime, stream)
		lab95Settle(t, h, runtime, &now)
		if starter.starts != 1 || len(requester.commands) != 0 {
			t.Fatalf("capture relaunched or changed adopted application: real GIO starts=%d commands=%q", starter.starts, requester.commands)
		}
		lab95AssertGroup(t, h, registered, window)
		if err := sessionstate.LayoutStoreFor(state).LoadInto(&captured); err != nil {
			t.Fatal(err)
		}
		lab95AssertLayout(t, captured, registered.ID)
		lab95Steady(t, h, runtime, requester, starter, registered, &now, captured)
		lab95AssertControls(t, h, controls)
		t.Logf("observed %s Chrome identity %s; explicit registration; protected user snapshot; runtime capture; one bootstrap GIO and zero runtime launches", protocol, lab95IdentityClass(identity))
	}) {
		return
	}
	if _, err := os.Lstat(oldRoot); !os.IsNotExist(err) {
		t.Fatalf("capture compositor/private bus root survived cleanup: %v", err)
	}
	t.Run("new_compositor_reload_and_real_gio_restore", func(t *testing.T) {
		h := lab95PrivateHeadless(t, ctx, protocol, root)
		h.state = state
		compositor, err := compositorIdentity(h.socket)
		if err != nil {
			t.Fatal(err)
		}
		if h.socket == oldSocket || compositor == oldCompositor {
			t.Fatal("reload reused capture compositor identity/socket")
		}
		registry := lab95LoadRegistry(t, state)
		if len(registry.Contexts) != 1 || !reflect.DeepEqual(registry.Contexts[0], registered) {
			t.Fatal("new compositor did not reload the same typed approved registration")
		}
		var reloaded sessionstate.LayoutSnapshot
		if err := sessionstate.LayoutStoreFor(state).LoadInto(&reloaded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(reloaded, captured) {
			t.Fatal("new compositor did not reload captured layout")
		}
		lab95AssertApproval(t, root, registered, []byte(entry))
		controls := lab95OrdinaryControls(h, protocol)
		starter := lab95NewGIOStarter(t, h, root)
		requester := &restoreCleanupRealRequester{Client: h.client}
		now := time.Now()
		stream := &swayipc.EventStreamState{}
		runtime := lab95Runtime(t, h, requester, starter, registered, compositor, &now, stream)
		h.subscribe(runtime, stream)
		for range 4 {
			if _, err := runtime.Reconcile(h.tree(), now); err != nil {
				t.Fatal(err)
			}
			if starter.starts != 0 {
				break
			}
			h.drain(runtime)
			now = now.Add(sessionStartupSettleDelay)
		}
		if starter.starts != 1 {
			t.Fatalf("runtime did not start real GIO exactly once: %d", starter.starts)
		}
		window, identity := lab95ChromeWindow(t, h, starter, protocol)
		expectedIdentity := registered.App.Identity
		expectedIdentity.StartupWMClass = "" // Desktop metadata is absent from a live Sway window.
		if identity != expectedIdentity {
			t.Fatal("Chrome identity changed across private compositor restart")
		}
		events := lab95Settle(t, h, runtime, &now)
		for _, change := range []string{"new", "focus", "move", "mark"} {
			if !slices.ContainsFunc(events, func(event swayipc.Event) bool {
				return event.Type == swayipc.EventWindow && event.Change == change && event.Container != nil && event.Container.ID == window
			}) {
				t.Fatalf("private subscription did not deliver Chrome's real %s event", change)
			}
		}
		lab95AssertGroup(t, h, registered, window)
		lab95AssertControls(t, h, controls)
		var saved sessionstate.LayoutSnapshot
		if err := sessionstate.LayoutStoreFor(state).LoadInto(&saved); err != nil {
			t.Fatal(err)
		}
		lab95AssertLayout(t, saved, registered.ID)
		lab95Steady(t, h, runtime, requester, starter, registered, &now, saved)
		lab95AssertControls(t, h, controls)
		lab95AssertApproval(t, root, registered, []byte(entry))
		if !reflect.DeepEqual(lab95LoadRegistry(t, state).Contexts, registry.Contexts) {
			t.Fatal("runtime restore changed approved registration")
		}
		t.Logf("fresh Sway identity; connected real new/focus/move events; one runtime /usr/bin/gio launch; Chrome marked on 98; ordinary focused/hidden controls retained; four passes without duplicate effects")
	})
}

func lab95PrivateHeadless(t *testing.T, ctx context.Context, protocol sessionstate.WindowProtocol, root string) *restoreCleanupHeadless {
	t.Helper()
	t.Setenv("HOME", filepath.Join(root, "home"))
	h := newScratchpadHeadless(t, protocol)
	phase, cancel := context.WithTimeout(ctx, 40*time.Second)
	t.Cleanup(cancel)
	h.ctx = phase
	for _, variable := range []struct{ key, directory string }{
		{"HOME", "home"}, {"XDG_CONFIG_HOME", "config"}, {"XDG_STATE_HOME", "state"}, {"XDG_CACHE_HOME", "cache"}, {"XDG_DATA_HOME", "home/.local/share"},
	} {
		value := filepath.Join(root, variable.directory)
		h.env = slices.DeleteFunc(h.env, func(entry string) bool { return strings.HasPrefix(entry, variable.key+"=") })
		h.env = append(h.env, variable.key+"="+value)
		t.Setenv(variable.key, value)
	}
	// The compositor's startup allowlist has no bus or login DISPLAY and a
	// private runtime directory. App/control children use this private bus with
	// no service directories, preventing host D-Bus activation/autostart.
	bus := filepath.Join(h.root, "run", "lab95-bus")
	config := filepath.Join(h.root, "config", "lab95-dbus.conf")
	data := fmt.Sprintf("<busconfig><type>session</type><listen>unix:path=%s</listen><auth>EXTERNAL</auth><policy context=\"default\"><allow user=\"%d\"/><allow own=\"*\"/><allow send_destination=\"*\"/><allow receive_sender=\"*\"/></policy></busconfig>", bus, os.Getuid())
	if err := os.WriteFile(config, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	h.start("dbus-daemon", "--nofork", "--config-file="+config)
	h.until("private D-Bus socket", func() bool { info, err := os.Lstat(bus); return err == nil && info.Mode()&os.ModeSocket != 0 })
	h.env = append(h.env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+bus, "XDG_DATA_DIRS=/usr/local/share:/usr/share")
	for _, key := range []string{"SWAYSOCK", "WAYLAND_DISPLAY", "DISPLAY", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR", "XDG_DATA_DIRS"} {
		value := ""
		for _, entry := range h.env {
			if strings.HasPrefix(entry, key+"=") {
				value = strings.TrimPrefix(entry, key+"=")
			}
		}
		t.Setenv(key, value)
	}
	return h
}

func lab95DesktopCatalog(t *testing.T) sessionstate.DesktopCatalog {
	t.Helper()
	search, err := sessionstate.DefaultDesktopSearchPath()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := sessionstate.LoadDesktopCatalog(search)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func lab95ReportStockCatalog(t *testing.T, catalog sessionstate.DesktopCatalog) {
	t.Helper()
	if _, exists := catalog.ByID("google-chrome.desktop"); exists {
		t.Log("stock google-chrome.desktop parses; this slice selects the disposable user entry, so stock launching remains unproven")
		return
	}
	for _, issue := range catalog.Issues() {
		if issue.Path == "/usr/share/applications/google-chrome.desktop" {
			t.Logf("observed stock Chrome catalog rejection: %.200q; disposable user entry selected", issue.Reason)
			return
		}
	}
	t.Log("stock google-chrome.desktop absent from the actual catalog; disposable user entry selected")
}

func lab95LoadRegistry(t *testing.T, state string) sessionstate.Registry {
	t.Helper()
	var registry sessionstate.Registry
	if err := sessionstate.RegistryStoreFor(state).LoadInto(&registry); err != nil {
		t.Fatal(err)
	}
	return registry
}

func lab95AssertApproval(t *testing.T, root string, item sessionstate.Context, entry []byte) {
	t.Helper()
	launcher := item.Launcher
	wantDigest := fmt.Sprintf("%x", sha256.Sum256(entry))
	if launcher.Kind != sessionstate.LauncherDesktop || launcher.DesktopOrigin != sessionstate.DesktopEntryUser || launcher.DesktopID != "lab95-chrome.desktop" || launcher.DesktopEntrySHA256 != wantDigest || filepath.Dir(launcher.ApprovedDesktopPath) != filepath.Join(root, "state", "sway-session", "desktop-approvals") {
		t.Fatal("typed launcher did not preserve protected user approval")
	}
	for _, path := range []string{launcher.ApprovedDesktopPath, filepath.Dir(launcher.ApprovedDesktopPath), filepath.Join(root, "state", "sway-session", "state.sqlite3")} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			t.Fatalf("approval/state path is not owner-only: %s (%v)", path, err)
		}
	}
	data, err := os.ReadFile(launcher.ApprovedDesktopPath)
	if err != nil || !bytes.Equal(data, entry) {
		t.Fatal("approved snapshot differs from explicit source approval")
	}
}

func lab95IdentityClass(identity sessionstate.ApplicationIdentity) string {
	if identity.Protocol == sessionstate.WindowXWayland {
		return identity.X11Class
	}
	return identity.WaylandAppID
}

func lab95ChromeWindow(t *testing.T, h *restoreCleanupHeadless, starter *lab95GIOStarter, protocol sessionstate.WindowProtocol) (int64, sessionstate.ApplicationIdentity) {
	t.Helper()
	var id int64
	var identity sessionstate.ApplicationIdentity
	var last lab95SupervisorResult
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("last bounded Chrome observation: descendants=%d browser=%d renderers=%d sandboxed=%d %s", last.Processes, last.BrowserPID, last.Renderers, last.SandboxedRenderers, last.SandboxStatus)
			data, _ := os.ReadFile(filepath.Join(starter.root, "lab95-chrome.log"))
			t.Logf("private Chrome startup log bytes=%d", len(data))
			for _, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, "sandbox") || strings.Contains(line, "FATAL") || strings.Contains(line, "ozone_platform") {
					t.Logf("Chrome startup diagnostic: %.500s", line)
				}
			}
		}
	})
	h.until("owned Chrome window and normal renderer sandbox (no --no-sandbox)", func() bool {
		result, err := starter.exchange(lab95SupervisorRequest{Inspect: true})
		if err != nil {
			t.Fatal(err)
		}
		last = result
		if result.BrowserPID == 0 || result.SandboxedRenderers == 0 {
			return false
		}
		windows, err := sessionstate.ApplicationWindows(h.tree())
		if err != nil {
			t.Fatal(err)
		}
		id = 0
		for _, window := range windows {
			node := findContainerByID(h.tree(), window.ContainerID)
			if node.PID != result.BrowserPID {
				continue
			}
			if id != 0 {
				t.Fatal("owned Chrome mapped multiple top-level windows")
			}
			if window.Identity.Protocol != protocol || window.Workspace != "98" && window.Workspace != "99" {
				t.Fatal("Chrome escaped the requested private protocol/high workspaces")
			}
			id, identity = window.ContainerID, window.Identity
		}
		return id != 0
	})
	return id, identity
}

type lab95Control struct {
	id        int64
	workspace string
	before    Node
}

func lab95OrdinaryControls(h *restoreCleanupHeadless, protocol sessionstate.WindowProtocol) []lab95Control {
	h.t.Helper()
	h.command("workspace 99")
	hidden := scratchpadHeadlessWindow(h, scratchpadHeadlessIdentity(protocol, "org.example.LAB95.Hidden"))
	h.command(fmt.Sprintf("[con_id=%d] move scratchpad", hidden))
	focused := scratchpadHeadlessWindow(h, scratchpadHeadlessIdentity(protocol, "org.example.LAB95.Control"))
	// Mapping precedes Alacritty's first configured frame. Wait for the normal
	// control to fill the private output and both geometries to stabilize before
	// recording the preservation baseline; no runtime has started yet.
	var tree *Node
	var previous []swayipc.Rect
	stable := 0
	h.until("ordinary controls configured and stable", func() bool {
		tree = h.tree()
		control, scratch := findContainerByID(tree, focused), findContainerByID(tree, hidden)
		if control == nil || scratch == nil || control.Rect.Width < 1200 || control.Rect.Height < 600 {
			return false
		}
		rects := []swayipc.Rect{control.Rect, control.DecoRect, scratch.Rect, scratch.DecoRect}
		if slices.Equal(rects, previous) {
			stable++
		} else {
			stable = 0
			previous = rects
		}
		return stable >= 2
	})
	return []lab95Control{{hidden, "__i3_scratch", *findContainerByID(tree, hidden)}, {focused, "99", *findContainerByID(tree, focused)}}
}

func lab95AssertControls(t *testing.T, h *restoreCleanupHeadless, controls []lab95Control) {
	t.Helper()
	tree := h.tree()
	for _, control := range controls {
		if restoreCleanupWorkspace(tree, control.id) != control.workspace || !reflect.DeepEqual(findContainerByID(tree, control.id), &control.before) {
			after := findContainerByID(tree, control.id)
			if after != nil {
				beforeValue, afterValue := reflect.ValueOf(control.before), reflect.ValueOf(*after)
				var fields []string
				for i := 0; i < beforeValue.NumField(); i++ {
					if !reflect.DeepEqual(beforeValue.Field(i).Interface(), afterValue.Field(i).Interface()) {
						fields = append(fields, beforeValue.Type().Field(i).Name)
					}
				}
				t.Fatalf("ordinary control %d changed during desktop acceptance: fields=%v workspace=%s", control.id, fields, restoreCleanupWorkspace(tree, control.id))
			}
			t.Fatalf("ordinary control %d disappeared", control.id)
		}
	}
	if focusedContainerID(tree) != controls[1].id {
		t.Fatal("desktop restore stole ordinary control focus")
	}
	assertRestoreFocusNoTemporaryState(t, tree)
}

func lab95Runtime(t *testing.T, h *restoreCleanupHeadless, requester swayRequester, starter *lab95GIOStarter, item sessionstate.Context, compositor string, now *time.Time, stream *swayipc.EventStreamState) *sessionRuntime {
	t.Helper()
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Context: h.ctx, Root: h.state, CompositorID: compositor, EventStreamState: stream,
		StartedAt: now.Add(-2 * time.Second), Now: func() time.Time { return *now },
		ApplicationLauncher: lab95OwnedLauncher{starter: starter, state: h.state, approved: item.Launcher},
		ApplicationRestore:  sessionstate.ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: 2 * time.Second, LaunchTimeout: time.Minute, MaxConcurrent: 1},
		IndicatorCatalog:    func() (sessionstate.DesktopCatalog, error) { return lab95DesktopCatalog(t), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtime.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	return runtime
}

func lab95Settle(t *testing.T, h *restoreCleanupHeadless, runtime *sessionRuntime, now *time.Time) []swayipc.Event {
	t.Helper()
	var events []swayipc.Event
	for range 16 {
		refresh, err := runtime.Reconcile(h.tree(), *now)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, h.drain(runtime)...)
		*now = now.Add(sessionStartupSettleDelay)
		if err := runtime.Flush(*now); err != nil {
			t.Fatal(err)
		}
		if runtime.restoreCancelled {
			t.Fatal("real Chrome mapping/effect focus cancelled startup restore")
		}
		if !refresh && runtime.startupComplete && runtime.restoreProgress == nil && !runtime.lateRestorePending && !runtime.restoreCleanupPending {
			return events
		}
	}
	t.Fatal("real Chrome runtime did not settle within 16 bounded passes")
	return nil
}

func lab95AssertGroup(t *testing.T, h *restoreCleanupHeadless, item sessionstate.Context, window int64) {
	t.Helper()
	groups, err := sessionstate.ObserveApplicationGroups(h.tree(), lab95LoadRegistry(t, h.state))
	if err != nil {
		t.Fatal(err)
	}
	group := groups[item.ID]
	if group.Ambiguous || len(group.Windows) != 1 || group.Anchor == nil || group.Anchor.ContainerID != window || !group.AnchorMarked || group.Anchor.Workspace != "98" || group.Anchor.Scratchpad {
		t.Fatal("real Chrome was not restored as one marked normal anchor on workspace 98")
	}
}

func lab95AssertLayout(t *testing.T, snapshot sessionstate.LayoutSnapshot, id sessionstate.ContextID) {
	t.Helper()
	if snapshot.Version != sessionstate.LayoutSchemaVersion || len(snapshot.Workspaces) != 1 || len(snapshot.Scratchpad) != 0 || snapshot.Workspaces[0].Name != "98" || snapshot.Workspaces[0].RestoreMode != sessionstate.WorkspaceRestoreLayout {
		t.Fatal("durable capture did not contain only the managed Chrome workspace")
	}
	var ids []sessionstate.ContextID
	var visit func(*sessionstate.LayoutNode)
	visit = func(node *sessionstate.LayoutNode) {
		if node == nil {
			return
		}
		if node.ContextID != nil {
			ids = append(ids, *node.ContextID)
		}
		for index := range node.Children {
			visit(&node.Children[index])
		}
	}
	visit(snapshot.Workspaces[0].Tiling)
	if !slices.Equal(ids, []sessionstate.ContextID{id}) || len(snapshot.Workspaces[0].Floating) != 0 || len(snapshot.Workspaces[0].PlacementContexts) != 0 {
		t.Fatal("durable capture duplicated Chrome or captured ordinary controls")
	}
}

func lab95Steady(t *testing.T, h *restoreCleanupHeadless, runtime *sessionRuntime, requester *restoreCleanupRealRequester, starter *lab95GIOStarter, item sessionstate.Context, now *time.Time, saved sessionstate.LayoutSnapshot) {
	t.Helper()
	starts := starter.starts
	window := lab95WindowAnchor(t, h, item)
	assertScratchpadHeadlessQuiescent(h, runtime, requester, now)
	result, err := starter.exchange(lab95SupervisorRequest{Inspect: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Starts != starts || result.BrowserPID == 0 || result.SandboxedRenderers == 0 {
		t.Fatal("steady reconciliation duplicated a real GIO start or lost sandboxed Chrome")
	}
	var again sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(h.state).LoadInto(&again); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved, again) {
		t.Fatal("repeated runtime flush changed durable layout")
	}
	lab95AssertGroup(t, h, item, window)
}

func lab95WindowAnchor(t *testing.T, h *restoreCleanupHeadless, item sessionstate.Context) int64 {
	t.Helper()
	groups, err := sessionstate.ObserveApplicationGroups(h.tree(), lab95LoadRegistry(t, h.state))
	if err != nil || groups[item.ID].Anchor == nil {
		t.Fatal("steady state lost Chrome anchor")
	}
	return groups[item.ID].Anchor.ContainerID
}

// The fixture's ProcessStarter boundary must reject foreign commands and paths
// before any process is created, even if a runtime regression supplies them.
func TestLAB95OwnedGIORejectsForeignLaunch(t *testing.T) {
	root := t.TempDir()
	valid := sessionstate.ProcessSpec{Name: "/usr/bin/gio", Arguments: []string{"launch", filepath.Join(root, "approved.desktop")}, Environment: []string{"PATH=/usr/local/bin:/usr/bin"}}
	var accepted bytes.Buffer
	starter := &lab95GIOStarter{root: root, input: json.NewEncoder(&accepted), output: json.NewDecoder(strings.NewReader("{}\n"))}
	if err := starter.Start(valid); err != nil || accepted.Len() == 0 {
		t.Fatalf("valid owned launch did not reach supervisor: %v", err)
	}
	cases := []struct {
		name   string
		change func(*sessionstate.ProcessSpec)
	}{
		{"foreign command", func(spec *sessionstate.ProcessSpec) { spec.Name = "/bin/true" }},
		{"system desktop", func(spec *sessionstate.ProcessSpec) {
			spec.Arguments[1] = "/usr/share/applications/google-chrome.desktop"
		}},
		{"outside root", func(spec *sessionstate.ProcessSpec) { spec.Arguments[1] = filepath.Join(root, "..", "foreign.desktop") }},
		{"foreign verb", func(spec *sessionstate.ProcessSpec) { spec.Arguments[0] = "open" }},
		{"relative desktop", func(spec *sessionstate.ProcessSpec) { spec.Arguments[1] = "approved.desktop" }},
		{"extra argument", func(spec *sessionstate.ProcessSpec) { spec.Arguments = append(spec.Arguments, "extra") }},
		{"foreign environment", func(spec *sessionstate.ProcessSpec) { spec.Environment = nil }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			spec := valid
			spec.Arguments = slices.Clone(valid.Arguments)
			test.change(&spec)
			var sent bytes.Buffer
			starter := &lab95GIOStarter{root: root, input: json.NewEncoder(&sent), output: json.NewDecoder(strings.NewReader("{}\n"))}
			if err := starter.Start(spec); err == nil || sent.Len() != 0 {
				t.Fatalf("foreign launch reached supervisor: err=%v bytes=%d", err, sent.Len())
			}
		})
	}
}
