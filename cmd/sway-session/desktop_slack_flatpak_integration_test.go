package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
	"golang.org/x/sys/unix"
)

const (
	slack95App    = "com.slack.Slack"
	slack95Root   = "/tmp/lab95-slack"
	slack95Inner  = "SWAY_SESSION_SLACK_FIXTURE_INNER"
	slack95Digest = "SWAY_SESSION_SLACK_HOST_FLATPAK_SHA256"
)

// SWAY_SESSION_DESKTOP_ACCEPTANCE=1 go test -race ./cmd/sway-session -run '^TestDesktopAcceptanceSlackFlatpakPrivate$' -count=1 -timeout=5m -v
// Native Wayland via the installed launcher's defaults and private environment.
// No application arguments, replacement executable, downloads,
// install/override writes, host profile, host input devices, or host networking.
// The compiled test reexecs inside an outer bwrap before even probing Flatpak.
// The host runs real root-owned executable resolution and pins its digest.
// Inside, real CLI/catalog, verification and DesktopApplicationLauncher.SpecContext
// use that read-only binary. Injected resolution and direct-child tracking replace
// daemonApplicationLauncher.Prepare and ExecProcessStarter's detached lifetime:
// user namespaces translate host root ownership to unmapped UID 65534.
func TestDesktopAcceptanceSlackFlatpakPrivate(t *testing.T) {
	if os.Getenv("SWAY_SESSION_DESKTOP_ACCEPTANCE") != "1" {
		t.Skip("set SWAY_SESSION_DESKTOP_ACCEPTANCE=1 for installed Slack/private Sway acceptance")
	}
	if os.Getenv(slack95Inner) == "1" {
		slack95Acceptance(t)
		return
	}
	for _, tool := range []string{"/usr/bin/bwrap", "/usr/bin/flatpak", "/usr/bin/sway", "/usr/bin/Xwayland", "/usr/bin/printenv", "/usr/bin/dbus-daemon", "/usr/bin/gdbus", "/usr/lib/flatpak-portal"} {
		if info, err := os.Stat(tool); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			t.Fatalf("opt-in Slack prerequisite %s: %v", tool, err)
		}
	}
	flatpak, err := sessionstate.ResolveRootOwnedSystemExecutable("flatpak")
	if err != nil || flatpak != "/usr/bin/flatpak" {
		t.Fatalf("host root-owned Flatpak resolution: path=%q error=%v", flatpak, err)
	}
	binary, err := slack95ReadFlatpakBinary()
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(binary))
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	installation := os.Getenv("FLATPAK_USER_DIR")
	if installation == "" {
		data := os.Getenv("XDG_DATA_HOME")
		if data == "" {
			data = filepath.Join(home, ".local", "share")
		}
		installation = filepath.Join(data, "flatpak")
	}
	installation, err = filepath.EvalSymlinks(installation)
	if err != nil || !filepath.IsAbs(installation) || strings.ContainsAny(installation, "\n\r\t ") {
		t.Fatalf("opt-in Slack user installation must be an existing absolute path: %q %v", installation, err)
	}
	if _, err := os.Stat(filepath.Join(installation, "app", slack95App, "current", "active", "metadata")); err != nil {
		t.Fatalf("opt-in Slack requires installed USER %s (no install attempted): %v", slack95App, err)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"home", "config", "data", "cache", "state", "run", "tmp"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--unshare-all", "--new-session", "--die-with-parent",
		"--ro-bind", "/usr", "/usr", "--ro-bind", "/etc", "/etc",
		"--symlink", "usr/bin", "/bin", "--symlink", "usr/bin", "/sbin",
		"--symlink", "usr/lib", "/lib", "--symlink", "usr/lib", "/lib64",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/run", "--dir", "/var",
		"--tmpfs", "/tmp", "--bind", root, slack95Root,
		"--ro-bind", executable, slack95Root + "/fixture.test",
		"--ro-bind", installation, installation,
		"--ro-bind", installation, filepath.Join(slack95Root, "home", ".local", "share", "flatpak"),
		"--chdir", slack95Root, "--clearenv"}
	env := []string{
		"PATH=/usr/bin", "LANG=C.UTF-8", "HOME=" + slack95Root + "/home",
		"XDG_CONFIG_HOME=" + slack95Root + "/config", "XDG_DATA_HOME=" + slack95Root + "/data",
		"XDG_CACHE_HOME=" + slack95Root + "/cache", "XDG_STATE_HOME=" + slack95Root + "/state",
		"XDG_RUNTIME_DIR=" + slack95Root + "/run", "TMPDIR=" + slack95Root + "/tmp",
		"XDG_DATA_DIRS=" + slack95Root + "/home/.local/share/flatpak/exports/share:/usr/share",
		"FLATPAK_USER_DIR=" + installation,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + slack95Root + "/run/absent-session-bus",
		"DBUS_SYSTEM_BUS_ADDRESS=unix:path=" + slack95Root + "/run/absent-system-bus",
		"SWAY_SESSION_DESKTOP_ACCEPTANCE=1", slack95Inner + "=1", "SWAY_SESSION_SLACK_HOST_HOME=" + home,
		slack95Digest + "=" + digest,
	}
	for _, namespace := range []string{"mnt", "pid", "net", "ipc"} {
		value, err := os.Readlink("/proc/self/ns/" + namespace)
		if err != nil {
			t.Fatal(err)
		}
		env = append(env, "SWAY_SESSION_SLACK_HOST_NS_"+namespace+"="+value)
	}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		args = append(args, "--setenv", key, value)
	}
	args = append(args, "--", slack95Root+"/fixture.test", "-test.run=^TestDesktopAcceptanceSlackFlatpakPrivate$", "-test.v", "-test.count=1", "-test.timeout=4m45s")
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute+55*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/bwrap", args...)
	command.Env, command.Dir = []string{"PATH=/usr/bin"}, root
	command.WaitDelay = 3 * time.Second
	output, err := command.CombinedOutput()
	t.Logf("isolated Slack reexec:\n%s", output)
	if err != nil {
		t.Fatalf("opt-in installed Slack/private namespace failed: %v (overall context: %v)", err, ctx.Err())
	}
}

func slack95Isolation() error {
	if os.Getenv(slack95Inner) != "1" || os.Getenv("HOME") != slack95Root+"/home" {
		return errors.New("Slack requires the isolated test reexec with private HOME")
	}
	for _, namespace := range []string{"mnt", "pid", "net", "ipc"} {
		current, err := os.Readlink("/proc/self/ns/" + namespace)
		parent := os.Getenv("SWAY_SESSION_SLACK_HOST_NS_" + namespace)
		if err != nil || parent == "" || current == parent {
			return fmt.Errorf("outer %s namespace was not isolated: %v", namespace, err)
		}
	}
	for _, path := range []string{
		filepath.Join(os.Getenv("SWAY_SESSION_SLACK_HOST_HOME"), ".var"),
		fmt.Sprintf("/run/user/%d", os.Getuid()), "/run/dbus/system_bus_socket",
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return fmt.Errorf("production home/bus path must be absent before Slack starts: %s (%v)", path, err)
		}
	}
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	for _, path := range []string{"/usr", os.Getenv("FLATPAK_USER_DIR"), slack95Root + "/home/.local/share/flatpak", slack95Root + "/fixture.test"} {
		found := false
		for _, line := range strings.Split(string(mounts), "\n") {
			fields := strings.Fields(line)
			if len(fields) > 6 && fields[4] == path {
				found = slices.Contains(strings.Split(fields[5], ","), "ro")
			}
		}
		if !found {
			return fmt.Errorf("installation %s lacks a kernel read-only bind", path)
		}
	}
	binary, err := slack95ReadFlatpakBinary()
	if err != nil {
		return err
	}
	if err := slack95VerifyBinaryDigest(binary, os.Getenv(slack95Digest)); err != nil {
		return err
	}
	devices, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(devices), "\n") {
		name, _, interfaceLine := strings.Cut(line, ":")
		if interfaceLine && strings.TrimSpace(name) != "lo" {
			return fmt.Errorf("unexpected network interface in outer namespace: %s", name)
		}
	}
	return nil
}

func slack95VerifyBinaryDigest(binary []byte, expected string) error {
	if expected != fmt.Sprintf("%x", sha256.Sum256(binary)) {
		return errors.New("read-only Flatpak binary differs from the host root-verified SHA256")
	}
	return nil
}

func slack95ReadFlatpakBinary() ([]byte, error) {
	const limit = 64 * 1024 * 1024
	file, err := os.Open("/usr/bin/flatpak")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("fixture Flatpak binary must be a regular file of at most 64 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("fixture Flatpak binary grew beyond 64 MiB")
	}
	return data, nil
}

func slack95Acceptance(t *testing.T) {
	if err := slack95Isolation(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{}); err != nil {
		t.Fatalf("disable fixture core files before external startup: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute+35*time.Second)
	defer cancel()
	startup, startupCancel := context.WithTimeout(ctx, 30*time.Second)
	defer startupCancel()
	for _, tool := range []string{"flatpak", "sway"} {
		command := exec.CommandContext(startup, "/usr/bin/"+tool, "--version")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated %s version: %v", tool, err)
		}
		t.Logf("%s", strings.TrimSpace(string(output)))
	}
	info := exec.CommandContext(startup, "/usr/bin/flatpak", "info", "--user", slack95App)
	output, err := info.CombinedOutput()
	if err != nil {
		t.Fatalf("read-only USER installation discovery: %v: %s", err, output)
	}
	t.Logf("installed metadata (read-only):\n%s", output)
	profile := slack95Root + "/home/.var/app/" + slack95App
	if err := os.MkdirAll(profile+"/config", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profile+"/config/slack95-isolation-sentinel", []byte("private-slack95\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Prove the actual normal app-data mapping before the first application Start.
	probeCtx, probeCancel := context.WithTimeout(ctx, 20*time.Second)
	probe := exec.CommandContext(probeCtx, "/usr/bin/flatpak", "run", "--user", "--command=/usr/bin/sh", slack95App, "-eu", "-c", `
test "$HOME" = /tmp/lab95-slack/home
test "$FLATPAK_ID" = com.slack.Slack
test "$XDG_CONFIG_HOME" = /tmp/lab95-slack/home/.var/app/com.slack.Slack/config
test "$XDG_DATA_HOME" = /tmp/lab95-slack/home/.var/app/com.slack.Slack/data
test "$XDG_CACHE_HOME" = /tmp/lab95-slack/home/.var/app/com.slack.Slack/cache
test "$XDG_STATE_HOME" = /tmp/lab95-slack/home/.var/app/com.slack.Slack/.local/state
test "$(cat "$XDG_CONFIG_HOME/slack95-isolation-sentinel")" = private-slack95
test ! -e "$SWAY_SESSION_SLACK_HOST_HOME/.var"
test ! -e "/run/host$SWAY_SESSION_SLACK_HOST_HOME/.var"
printf 'normal Flatpak mapping: temporary profile sentinel verified\n'
cat /proc/net/dev /proc/net/route
`)
	output, err = probe.CombinedOutput()
	probeCancel()
	if err != nil {
		t.Fatalf("harmless profile isolation proof failed; Slack was NOT started: %v: %s", err, output)
	}
	t.Logf("pre-application isolation proof:\n%s", output)
	catalog := slack95Catalog(t)
	entry, exists := catalog.ByID(slack95App + ".desktop")
	if !exists || entry.FlatpakID != slack95App || entry.FlatpakInstallation != sessionstate.FlatpakUser || entry.Origin != sessionstate.DesktopEntryUser {
		t.Fatalf("default real catalog did not discover the mirrored USER export: %+v", entry)
	}
	state := slack95Root + "/state/sway-session"
	var registered sessionstate.Context
	var saved sessionstate.LayoutSnapshot
	var oldCompositor, oldSocket string
	if !t.Run("focused_registration_and_capture", func(t *testing.T) {
		phase, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		h := slack95Headless(t, phase, state)
		oldSocket = h.socket
		oldCompositor = slack95Compositor(t, h)
		h.command("workspace 99")
		starter := &slack95Starter{h: h, isolated: true}
		if err := starter.Start(sessionstate.ProcessSpec{Name: "/usr/bin/flatpak", Arguments: []string{"run", "--user", slack95App}, Environment: []string{"PATH=/usr/local/bin:/usr/bin"}}); err != nil {
			t.Fatal(err)
		}
		window, identity := slack95Window(t, h, starter)
		h.command(fmt.Sprintf("[con_id=%d] focus", window))
		if focusedContainerID(h.tree()) != window || restoreCleanupWorkspace(h.tree(), window) != "99" {
			t.Fatal("registration did not focus the actual isolated Slack on workspace 99")
		}
		var stdout, stderr bytes.Buffer
		args := []string{"app", "register-focused", "--yes", "--desktop-id", slack95App + ".desktop", "--socket", h.socket}
		deps := defaultDependencies(strings.NewReader(""))
		// Factory closures capture their original deps, so replace verification
		// explicitly as well as resolution. The real info probe is never stubbed.
		originalResolve := deps.resolveSystem
		deps.resolveSystem = func(name string) (string, error) {
			if name == "flatpak" {
				return "/usr/bin/flatpak", nil
			}
			return originalResolve(name)
		}
		deps.verifyFlatpak = func(launcher sessionstate.Launcher) error {
			return sessionstate.VerifyFlatpakInstallationContext(phase, "/usr/bin/flatpak", launcher, sessionstate.ExecCommandRunner{})
		}
		if code := runWithContext(phase, args, strings.NewReader(""), &stdout, &stderr, deps); code != exitSuccess {
			t.Fatalf("real focused Slack registration: code=%d stderr=%s", code, stderr.String())
		}
		registry := slack95Registry(t, state)
		if len(registry.Contexts) != 1 {
			t.Fatalf("focused registration stored %d contexts, want one", len(registry.Contexts))
		}
		registered = registry.Contexts[0]
		expected := identity
		expected.StartupWMClass = entry.StartupWMClass
		if registered.Launcher != (sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: slack95App, FlatpakInstallation: sessionstate.FlatpakUser}) || registered.App == nil || registered.App.Identity != expected || !registered.App.DesiredOpen || registered.App.RestorePolicy != sessionstate.ApplicationRestoreFollow {
			t.Fatalf("typed registration differs from actual window/USER launcher: %+v", registered)
		}
		stdout.Reset()
		stderr.Reset()
		if code := runWithContext(phase, args, strings.NewReader(""), &stdout, &stderr, deps); code != exitSuccess || !strings.Contains(stdout.String(), "already registered") || !reflect.DeepEqual(slack95Registry(t, state), registry) {
			t.Fatalf("repeat real registration was not idempotent: code=%d stderr=%s", code, stderr.String())
		}
		now := time.Now()
		requester := &restoreCleanupRealRequester{Client: h.client}
		stream := &swayipc.EventStreamState{}
		runtime := slack95Runtime(t, h, requester, starter, registered, oldCompositor, &now, stream)
		h.subscribe(runtime, stream)
		slack95Converge(t, h, runtime, registered, &now)
		if err := sessionstate.LayoutStoreFor(state).LoadInto(&saved); err != nil {
			t.Fatal(err)
		}
		slack95AssertLayout(t, saved, registered.ID)
		slack95Steady(t, h, runtime, requester, starter, registered, &now, saved)
		if starter.starts != 1 {
			t.Fatalf("adoption duplicated the bootstrap launch: %d", starter.starts)
		}
		t.Logf("actual Slack Wayland identity app_id=%q sandbox=%q; real focused registration; captured workspace 99; one bootstrap and no runtime starts", identity.WaylandAppID, identity.SandboxAppID)
	}) {
		return
	}
	t.Run("fresh_compositor_and_fixed_flatpak_restore", func(t *testing.T) {
		phase, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		h := slack95Headless(t, phase, state)
		compositor := slack95Compositor(t, h)
		if h.socket == oldSocket || compositor == oldCompositor {
			t.Fatal("restore did not use a fresh private compositor")
		}
		registry := slack95Registry(t, state)
		if len(registry.Contexts) != 1 || !reflect.DeepEqual(registry.Contexts[0], registered) {
			t.Fatal("restart failed to load the captured typed registry")
		}
		var reloaded sessionstate.LayoutSnapshot
		if err := sessionstate.LayoutStoreFor(state).LoadInto(&reloaded); err != nil || !reflect.DeepEqual(saved, reloaded) {
			t.Fatalf("restart failed to load the captured workspace: %v", err)
		}
		starter := &slack95Starter{h: h, isolated: true}
		requester := &restoreCleanupRealRequester{Client: h.client}
		now := time.Now()
		stream := &swayipc.EventStreamState{}
		runtime := slack95Runtime(t, h, requester, starter, registered, compositor, &now, stream)
		h.subscribe(runtime, stream)
		for range 4 {
			if _, err := runtime.Reconcile(h.tree(), now); err != nil {
				t.Fatal(err)
			}
			h.drain(runtime)
			if starter.starts != 0 {
				break
			}
			now = now.Add(sessionStartupSettleDelay)
		}
		if starter.starts != 1 {
			t.Fatalf("production typed launcher started %d times, want one", starter.starts)
		}
		_, observed := slack95Window(t, h, starter)
		expected := registered.App.Identity
		expected.StartupWMClass = ""
		if observed != expected {
			t.Fatalf("actual Slack identity changed after restart: got=%+v want=%+v", observed, expected)
		}
		slack95Converge(t, h, runtime, registered, &now)
		var restored sessionstate.LayoutSnapshot
		if err := sessionstate.LayoutStoreFor(state).LoadInto(&restored); err != nil {
			t.Fatal(err)
		}
		slack95AssertLayout(t, restored, registered.ID)
		slack95Steady(t, h, runtime, requester, starter, registered, &now, restored)
		if !reflect.DeepEqual(slack95Registry(t, state).Contexts, registry.Contexts) {
			t.Fatal("restore changed the typed application registration")
		}
		t.Log("fresh private Sway: one real /usr/bin/flatpak run --user com.slack.Slack; same observed sandbox identity; restored saved workspace 99; four steady passes without extra starts")
	})
}

func slack95Catalog(t *testing.T) sessionstate.DesktopCatalog {
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

func slack95Registry(t *testing.T, state string) sessionstate.Registry {
	t.Helper()
	var registry sessionstate.Registry
	if err := sessionstate.RegistryStoreFor(state).LoadInto(&registry); err != nil {
		t.Fatal(err)
	}
	return registry
}

func slack95Compositor(t *testing.T, h *restoreCleanupHeadless) string {
	t.Helper()
	id, err := compositorIdentity(h.socket)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func slack95Until(t *testing.T, ctx context.Context, description string, ready func() bool) {
	t.Helper()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for !ready() {
		select {
		case <-ctx.Done():
			t.Fatalf("isolated Slack %s: %v (network/portal/display permissions will not be widened)", description, ctx.Err())
		case <-tick.C:
		}
	}
}

// Reuse committed tree/command/event helpers, with this fixture's two-minute
// context and allowlist. No Chrome helper dependency or login-session inputs.
func slack95Headless(t *testing.T, ctx context.Context, state string) *restoreCleanupHeadless {
	t.Helper()
	// exec.Command resolves basenames using the parent PATH before child Env.
	t.Setenv("PATH", "/usr/bin")
	root, err := os.MkdirTemp("/tmp", "s95-slack-sway-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	h := &restoreCleanupHeadless{t: t, ctx: ctx, root: root, state: state}
	for _, dir := range []string{"run", "config"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	h.env = []string{"PATH=/usr/bin", "LANG=C.UTF-8", "HOME=" + slack95Root + "/home",
		"XDG_SESSION_TYPE=wayland", "XDG_CURRENT_DESKTOP=sway",
		"XDG_CONFIG_HOME=" + slack95Root + "/config", "XDG_DATA_HOME=" + slack95Root + "/data",
		"XDG_CACHE_HOME=" + slack95Root + "/cache", "XDG_STATE_HOME=" + slack95Root + "/state",
		"XDG_DATA_DIRS=" + os.Getenv("XDG_DATA_DIRS"), "FLATPAK_USER_DIR=" + os.Getenv("FLATPAK_USER_DIR"),
		"XDG_RUNTIME_DIR=" + root + "/run", "TMPDIR=" + slack95Root + "/tmp",
		"DBUS_SYSTEM_BUS_ADDRESS=unix:path=" + root + "/run/absent-system-bus",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + root + "/run/private-bus",
		"WLR_BACKENDS=headless", "WLR_HEADLESS_OUTPUTS=1", "WLR_RENDERER=pixman", "LIBGL_ALWAYS_SOFTWARE=1",
		// Xwayland(1): testing software backend, avoiding host NVIDIA EGL/DRI.
		"XWAYLAND_NO_GLAMOR=1",
		"__EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/50_mesa.json"}
	busConfig := fmt.Sprintf("<busconfig><type>session</type><listen>unix:path=%s/run/private-bus</listen><auth>EXTERNAL</auth><policy context=\"default\"><allow user=\"%d\"/><allow own=\"*\"/><allow send_destination=\"*\"/><allow receive_sender=\"*\"/></policy></busconfig>", root, os.Getuid())
	config := filepath.Join(root, "config", "dbus.conf")
	if err := os.WriteFile(config, []byte(busConfig), 0600); err != nil {
		t.Fatal(err)
	}
	h.start("dbus-daemon", "--nofork", "--nopidfile", "--config-file="+config)
	slack95Until(t, ctx, "private bus startup", func() bool {
		info, err := os.Lstat(filepath.Join(root, "run", "private-bus"))
		return err == nil && info.Mode()&os.ModeSocket != 0
	})
	config = filepath.Join(root, "config", "sway.conf")
	if err := os.WriteFile(config, []byte("xwayland enable\noutput HEADLESS-1 mode 1280x720\nfocus_follows_mouse no\nworkspace 98\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// h.start's log prefix must be a filename, not an absolute executable path.
	sway := h.start("sway", "--config", config)
	h.socket = filepath.Join(root, "run", fmt.Sprintf("sway-ipc.%d.%d.sock", os.Getuid(), sway.Process.Pid))
	slack95Until(t, ctx, "private Sway startup", func() bool {
		info, err := os.Lstat(h.socket)
		return err == nil && info.Mode()&os.ModeSocket != 0
	})
	h.client = swayipc.NewClient(h.socket)
	t.Cleanup(h.client.Close)
	h.env = append(h.env, "SWAYSOCK="+h.socket)
	displayPath := filepath.Join(root, "private-display")
	h.command("exec /usr/bin/printenv DISPLAY > " + quoteSwayString(displayPath))
	var display string
	slack95Until(t, ctx, "private XWayland DISPLAY", func() bool {
		value, err := os.ReadFile(displayPath)
		display = strings.TrimSpace(string(value))
		return err == nil && display != ""
	})
	if !strings.HasPrefix(display, ":") {
		t.Fatalf("nonlocal private DISPLAY: %q", display)
	}
	if _, err := strconv.ParseUint(strings.TrimPrefix(display, ":"), 10, 32); err != nil {
		t.Fatalf("invalid private DISPLAY: %q", display)
	}
	h.env = append(h.env, "DISPLAY="+display)
	waylandPath := filepath.Join(root, "private-wayland-display")
	h.command("exec /usr/bin/printenv WAYLAND_DISPLAY > " + quoteSwayString(waylandPath))
	var wayland string
	slack95Until(t, ctx, "private Wayland socket", func() bool {
		value, err := os.ReadFile(waylandPath)
		wayland = strings.TrimSpace(string(value))
		if err != nil || wayland == "" {
			return false
		}
		if filepath.Base(wayland) != wayland || strings.ContainsAny(wayland, " \t\r\n") {
			t.Fatalf("invalid private WAYLAND_DISPLAY: %q", wayland)
		}
		info, err := os.Lstat(filepath.Join(root, "run", wayland))
		return err == nil && info.Mode()&os.ModeSocket != 0
	})
	h.env = append(h.env, "WAYLAND_DISPLAY="+wayland)
	slack95Portal(t, h)
	// Export the fixture allowlist to the real CLI verifier/catalog and Prepare.
	// The installed default may select Wayland; a fallback lacks required identity.
	for _, entry := range h.env {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	return h
}

// The installed D-Bus and systemd services both specify this exact executable.
// flatpak-spawn(1) documents its role in creating copies of the caller sandbox.
// Start it explicitly on our bus: no system service directories or activation.
func slack95Portal(t *testing.T, h *restoreCleanupHeadless) {
	t.Helper()
	log, err := os.OpenFile(filepath.Join(h.root, "flatpak-portal.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(h.ctx, "/usr/lib/flatpak-portal")
	command.Env, command.Dir, command.Stdout, command.Stderr = h.env, h.root, log, log
	if err := command.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = command.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("owned private Flatpak portal did not reap")
		}
		_ = log.Close()
		if t.Failed() {
			data, _ := os.ReadFile(log.Name())
			t.Logf("private Flatpak portal diagnostics:\n%s", data)
		}
	})
	ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancel()
	slack95Until(t, ctx, "private Flatpak portal ownership", func() bool {
		select {
		case <-done:
			t.Fatalf("private Flatpak portal exited before bus ownership: %v", waitErr)
		default:
		}
		query := exec.CommandContext(ctx, "/usr/bin/gdbus", "call", "--session", "--dest", "org.freedesktop.DBus", "--object-path", "/org/freedesktop/DBus", "--method", "org.freedesktop.DBus.NameHasOwner", "org.freedesktop.portal.Flatpak")
		query.Env = h.env
		output, err := query.CombinedOutput()
		return err == nil && strings.TrimSpace(string(output)) == "(true,)"
	})
}

func slack95ValidateLaunch(spec sessionstate.ProcessSpec) error {
	if spec.Name != "/usr/bin/flatpak" || !slices.Equal(spec.Arguments, []string{"run", "--user", slack95App}) || !slices.Equal(spec.Environment, []string{"PATH=/usr/local/bin:/usr/bin"}) || len(spec.UnsetInheritedEnvironment) != 0 || len(spec.UnsetInheritedEnvironmentPrefixes) != 0 {
		return errors.New("Slack fixture accepts only the exact production USER Flatpak spec")
	}
	return nil
}

type slack95Starter struct {
	h        *restoreCleanupHeadless
	isolated bool
	starts   int
	requests int
	done     chan struct{}
	waitErr  error // published by closing done; read only after done
	instance string
}

func (starter *slack95Starter) Start(spec sessionstate.ProcessSpec) error {
	if err := slack95ValidateLaunch(spec); err != nil {
		return err
	}
	if !starter.isolated || starter.h == nil {
		return errors.New("Slack Start requires the completed outer isolation/profile proof")
	}
	starter.requests++
	if starter.starts != 0 {
		return errors.New("Slack fixture refuses a duplicate real application Start")
	}
	log, err := os.OpenFile(filepath.Join(starter.h.root, "slack.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	command := exec.CommandContext(starter.h.ctx, spec.Name, spec.Arguments...)
	command.Env = append(slices.Clone(starter.h.env), spec.Environment...)
	command.Dir, command.Stdout, command.Stderr = slack95Root, log, log
	command.WaitDelay = 3 * time.Second
	if err := command.Start(); err != nil {
		_ = log.Close()
		return err
	}
	starter.done = make(chan struct{})
	starter.starts++
	go func() { starter.waitErr = command.Wait(); close(starter.done) }()
	starter.h.t.Cleanup(func() {
		// Kill only the observed instance in this phase's private runtime. Never
		// use the app ID to kill a global Slack process. Final namespace exit
		// kills any orphan/detached descendants, without a global Wait4(-1).
		if starter.instance != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			kill := exec.CommandContext(ctx, "/usr/bin/flatpak", "kill", starter.instance)
			kill.Env = starter.h.env
			_ = kill.Run()
			cancel()
		}
		_ = command.Process.Kill()
		select {
		case <-starter.done:
		case <-time.After(5 * time.Second):
			starter.h.t.Error("owned Flatpak direct child did not reap")
		}
		_ = log.Close()
		if starter.h.t.Failed() {
			data, _ := os.ReadFile(log.Name())
			if len(data) > 6000 {
				data = data[len(data)-6000:]
			}
			starter.h.t.Logf("disposable Slack startup diagnostics:\n%s", data)
		}
	})
	return nil
}

func slack95Window(t *testing.T, h *restoreCleanupHeadless, starter *slack95Starter) (int64, sessionstate.ApplicationIdentity) {
	t.Helper()
	var id int64
	var identity sessionstate.ApplicationIdentity
	var loggedUnidentified bool
	ctx, cancel := context.WithTimeout(h.ctx, 20*time.Second)
	defer cancel()
	slack95Until(t, ctx, "unmet fixture prerequisite: a mapped window with Sway sandbox_app_id/engine/instance", func() bool {
		select {
		case <-starter.done:
			t.Fatalf("installed Slack exited before mapping an identified window: %v", starter.waitErr)
		default:
		}
		tree := h.tree()
		// A crash dialog is failure evidence only. Never turn its desktop class
		// into a surrogate for the required observed sandbox application ID.
		var crashed func(*Node) bool
		crashed = func(node *Node) bool {
			if node.Name == "Slack crashed!" && node.Window != nil {
				return true
			}
			for _, child := range append(slices.Clone(node.Nodes), node.FloatingNodes...) {
				if crashed(child) {
					return true
				}
			}
			return false
		}
		if crashed(tree) {
			log, _ := os.ReadFile(filepath.Join(h.root, "slack.log"))
			if strings.Contains(string(log), "Portal call failed: The name org.freedesktop.portal.Flatpak was not provided") {
				t.Fatal("isolated installed Slack/Zypak could not reach org.freedesktop.portal.Flatpak despite private portal startup; renderer failed and only a crash dialog mapped; registration/restore acceptance cannot proceed safely")
			}
			t.Fatal("isolated installed Slack mapped a crash dialog instead of an eligible identified application window")
		}
		windows, err := sessionstate.ApplicationWindows(tree)
		if err != nil {
			t.Fatal(err)
		}
		for _, window := range windows {
			if window.Identity.SandboxAppID != slack95App {
				if !loggedUnidentified {
					node := findContainerByID(tree, window.ContainerID)
					if node == nil {
						t.Fatal("eligible window disappeared from the same private tree observation")
					}
					t.Logf("first eligible private window rejected: observed=%+v; sandbox_engine=%v sandbox_instance_id=%v PID=%d", window.Identity, node.SandboxEngine, node.SandboxInstance, node.PID)
					data, err := os.ReadFile(fmt.Sprintf("/proc/%d/root/.flatpak-info", node.PID))
					t.Logf("private PID %d Flatpak metadata diagnostic only: readable=%t fixed_app_id_matches=%t error=%v; this cannot substitute for Sway's missing identity", node.PID, err == nil, strings.Contains(string(data), "name="+slack95App+"\n"), err)
					loggedUnidentified = true
				}
				continue
			}
			node := findContainerByID(tree, window.ContainerID)
			if id != 0 && id != window.ContainerID {
				t.Fatal("isolated Slack mapped ambiguous top-level windows")
			}
			engine, instance := "", ""
			if node != nil && node.SandboxEngine != nil {
				engine = *node.SandboxEngine
			}
			if node != nil && node.SandboxInstance != nil {
				instance = *node.SandboxInstance
			}
			// Flatpak 1.18.4 common/flatpak-run-wayland.c sets "org.flatpak".
			if window.Identity.Protocol != sessionstate.WindowWayland || window.Identity.WaylandAppID == "" || engine != "org.flatpak" || instance == "" {
				t.Fatalf("unmet native Slack identity prerequisite: observed=%+v sandbox_engine=%q sandbox_instance_id=%q", window.Identity, engine, instance)
			}
			if window.Workspace != "98" && window.Workspace != "99" {
				t.Fatal("isolated Slack escaped high workspaces")
			}
			// Sway 1.12 view_get_sandbox_* reads wp_security_context for this
			// Wayland client. A process PID or proc-root file is not its source.
			t.Logf("actual native Slack window: observed=%+v sandbox_engine=%q sandbox_instance_id=%q PID=%d", window.Identity, engine, instance, node.PID)
			if _, err := strconv.ParseUint(instance, 10, 64); err != nil {
				t.Fatal("non-numeric private Flatpak instance ID")
			}
			starter.instance = instance
			id, identity = window.ContainerID, window.Identity
		}
		return id != 0
	})
	return id, identity
}

type slack95Launcher struct {
	starter  *slack95Starter
	approved sessionstate.Launcher
}

func (launcher slack95Launcher) Prepare(ctx context.Context, item sessionstate.Context) (preparedApplicationLaunch, error) {
	if item.Launcher != launcher.approved {
		return nil, errors.New("approved Slack launcher changed")
	}
	// Host root resolution plus the inner read-only/hash proof substitutes only
	// the default daemon adapter's namespace-incompatible executable resolution.
	if err := sessionstate.VerifyFlatpakInstallationContext(ctx, "/usr/bin/flatpak", item.Launcher, sessionstate.ExecCommandRunner{}); err != nil {
		return nil, err
	}
	adapter := sessionstate.DesktopApplicationLauncher{
		StateRoot: launcher.starter.h.state, Flatpak: "/usr/bin/flatpak",
	}
	spec, err := adapter.SpecContext(ctx, item)
	if err != nil {
		return nil, err
	}
	if err := slack95ValidateLaunch(spec); err != nil {
		return nil, err
	}
	return preparedDesktopApplicationLaunch{starter: launcher.starter, spec: spec}, nil
}

func slack95Runtime(t *testing.T, h *restoreCleanupHeadless, requester swayRequester, starter *slack95Starter, item sessionstate.Context, compositor string, now *time.Time, stream *swayipc.EventStreamState) *sessionRuntime {
	t.Helper()
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Context: h.ctx, Root: h.state, CompositorID: compositor, EventStreamState: stream,
		StartedAt: now.Add(-2 * time.Second), Now: func() time.Time { return *now },
		ApplicationLauncher: slack95Launcher{starter: starter, approved: item.Launcher},
		ApplicationRestore:  sessionstate.ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: 2 * time.Second, LaunchTimeout: time.Minute, MaxConcurrent: 1},
		IndicatorCatalog:    func() (sessionstate.DesktopCatalog, error) { return slack95Catalog(t), nil },
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

func slack95Converge(t *testing.T, h *restoreCleanupHeadless, runtime *sessionRuntime, item sessionstate.Context, now *time.Time) {
	t.Helper()
	for range 24 {
		refresh, err := runtime.Reconcile(h.tree(), *now)
		if err != nil {
			t.Fatal(err)
		}
		h.drain(runtime)
		*now = now.Add(sessionStartupSettleDelay)
		if err := runtime.Flush(now.Add(sessionSnapshotDebounce)); err != nil {
			t.Fatal(err)
		}
		if !refresh && runtime.startupComplete && runtime.restoreProgress == nil && !runtime.lateRestorePending && !runtime.restoreCleanupPending {
			slack95AssertGroup(t, h, item)
			return
		}
	}
	t.Fatal("real Slack capture/restore did not converge within 24 bounded passes")
}

func slack95AssertGroup(t *testing.T, h *restoreCleanupHeadless, item sessionstate.Context) {
	t.Helper()
	groups, err := sessionstate.ObserveApplicationGroups(h.tree(), slack95Registry(t, h.state))
	if err != nil {
		t.Fatal(err)
	}
	group := groups[item.ID]
	if group.Ambiguous || len(group.Windows) != 1 || group.Anchor == nil || !group.AnchorMarked || group.Anchor.Workspace != "99" || group.Anchor.Scratchpad {
		t.Fatalf("Slack is not one marked normal anchor on saved workspace 99: %+v", group)
	}
}

func slack95AssertLayout(t *testing.T, snapshot sessionstate.LayoutSnapshot, id sessionstate.ContextID) {
	t.Helper()
	if snapshot.Version != sessionstate.LayoutSchemaVersion || len(snapshot.Workspaces) != 1 || len(snapshot.Scratchpad) != 0 || snapshot.Workspaces[0].Name != "99" || snapshot.Workspaces[0].RestoreMode != sessionstate.WorkspaceRestoreLayout {
		t.Fatal("durable capture did not preserve only Slack's saved workspace 99")
	}
	var contexts []sessionstate.ContextID
	var visit func(*sessionstate.LayoutNode)
	visit = func(node *sessionstate.LayoutNode) {
		if node == nil {
			return
		}
		if node.ContextID != nil {
			contexts = append(contexts, *node.ContextID)
		}
		for i := range node.Children {
			visit(&node.Children[i])
		}
	}
	visit(snapshot.Workspaces[0].Tiling)
	if !slices.Equal(contexts, []sessionstate.ContextID{id}) || len(snapshot.Workspaces[0].Floating) != 0 || len(snapshot.Workspaces[0].PlacementContexts) != 0 {
		t.Fatal("durable capture duplicated Slack or replaced its saved layout")
	}
}

func slack95Steady(t *testing.T, h *restoreCleanupHeadless, runtime *sessionRuntime, requester *restoreCleanupRealRequester, starter *slack95Starter, item sessionstate.Context, now *time.Time, saved sessionstate.LayoutSnapshot) {
	t.Helper()
	starts := starter.starts
	commands := len(requester.commands)
	for range 4 {
		*now = now.Add(sessionStartupSettleDelay)
		if _, err := runtime.Reconcile(h.tree(), *now); err != nil {
			t.Fatal(err)
		}
		h.drain(runtime)
		if err := runtime.Flush(now.Add(sessionSnapshotDebounce)); err != nil {
			t.Fatal(err)
		}
		slack95AssertGroup(t, h, item)
	}
	var again sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(h.state).LoadInto(&again); err != nil {
		t.Fatal(err)
	}
	if starter.starts != starts || starter.requests != starts || len(requester.commands) != commands || !reflect.DeepEqual(saved, again) {
		t.Fatal("steady reconciliation added starts/effects or changed saved Slack layout")
	}
}

// A changed mounted binary cannot reuse the host's executable trust evidence.
func TestSlackFlatpakFixtureRejectsChangedBinary(t *testing.T) {
	binary := []byte("host root-verified binary")
	digest := fmt.Sprintf("%x", sha256.Sum256(binary))
	if err := slack95VerifyBinaryDigest(binary, digest); err != nil {
		t.Fatal(err)
	}
	if err := slack95VerifyBinaryDigest([]byte("changed binary"), digest); err == nil {
		t.Fatal("fixture accepted a changed binary under the host-verified digest")
	}
}

// The acceptance starter must refuse a foreign process before Start creates it.
func TestSlackFlatpakFixtureRejectsForeignLaunch(t *testing.T) {
	valid := sessionstate.ProcessSpec{Name: "/usr/bin/flatpak", Arguments: []string{"run", "--user", slack95App}, Environment: []string{"PATH=/usr/local/bin:/usr/bin"}}
	if err := slack95ValidateLaunch(valid); err != nil {
		t.Fatalf("fixture rejected the exact production spec: %v", err)
	}
	for _, test := range []struct {
		name   string
		change func(*sessionstate.ProcessSpec)
	}{
		{"executable", func(s *sessionstate.ProcessSpec) { s.Name = "/bin/true" }},
		{"installation", func(s *sessionstate.ProcessSpec) { s.Arguments = []string{"run", "--system", slack95App} }},
		{"application", func(s *sessionstate.ProcessSpec) { s.Arguments = []string{"run", "--user", "org.example.Foreign"} }},
		{"extra argument", func(s *sessionstate.ProcessSpec) { s.Arguments = []string{"run", "--user", "--command=sh", slack95App} }},
		{"environment", func(s *sessionstate.ProcessSpec) { s.Environment = []string{"HOME=/home/foreign"} }},
		{"unset", func(s *sessionstate.ProcessSpec) { s.UnsetInheritedEnvironment = []string{"HOME"} }},
		{"unset prefix", func(s *sessionstate.ProcessSpec) { s.UnsetInheritedEnvironmentPrefixes = []string{"XDG_"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := valid
			test.change(&spec)
			if err := slack95ValidateLaunch(spec); err == nil {
				t.Fatalf("fixture accepted foreign launch: %+v", spec)
			}
		})
	}
}
