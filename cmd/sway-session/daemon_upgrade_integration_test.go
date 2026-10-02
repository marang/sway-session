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
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/buildmetadata"
	sessionstate "github.com/marang/sway-session/internal/session"
	"golang.org/x/sys/unix"
)

// This runs real, differently stamped builds of the current source, not two
// historical releases. sleep is an owned synthetic foreground agent: no Herdr
// or provider session is started, resumed, or read by this harness.
func TestDaemonExecutableReplacementPreservesWorkHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway/Alacritty integration")
	}
	for _, name := range []string{"go", "sway", "alacritty", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("headless integration requires %s: %v", name, err)
		}
	}
	buildRoot := t.TempDir()
	executable, replacement := filepath.Join(buildRoot, "sway-session"), filepath.Join(buildRoot, "replacement")
	oldBuild := buildmetadata.Metadata{Version: "lab134-old", Commit: strings.Repeat("a", 40), Modified: true}
	newBuild := buildmetadata.Metadata{Version: "lab134-new", Commit: strings.Repeat("b", 40), Modified: true}
	buildLifecycleDaemon(t, executable, oldBuild)
	buildLifecycleDaemon(t, replacement, newBuild)

	h := newRestoreCleanupHeadless(t)
	// Never connect this executable test to the workstation system bus. The
	// absent private bus deliberately exercises fail-closed close detection.
	h.env = append(h.env, "DBUS_SYSTEM_BUS_ADDRESS=unix:path="+filepath.Join(h.root, "no-system-bus"))
	ids := []sessionstate.ContextID{testManagedContextID, "6ba7b810-9dad-11d1-80b4-00c04fd430c8"}
	missing := sessionstate.ContextID("6ba7b811-9dad-11d1-80b4-00c04fd430c8")
	registry := sessionRegistryIDs(append(slices.Clone(ids), missing)...)
	for i := range registry.Contexts {
		registry.Contexts[i].Launcher.Cwd = h.root
	}
	if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
		t.Fatal(err)
	}
	desired := placementOnlySnapshot("98", ids[0])
	desired.Workspaces[0].PlacementContexts = slices.Clone(ids)
	if err := sessionstate.LayoutStoreFor(h.state).Save(desired); err != nil {
		t.Fatal(err)
	}
	windows := make([]int64, len(ids))
	adapters, agents := make([]int, len(ids)), make([]int, len(ids))
	identities := make([]string, len(ids))
	for i, id := range ids {
		windows[i] = h.terminal(id)
		adapters[i] = int(findContainerByID(h.tree(), windows[i]).PID)
		h.until("owned foreground agent", func() bool {
			agents[i] = lifecycleSleepChild(t, adapters[i])
			return agents[i] > 0
		})
		identities[i] = lifecycleProcessIdentity(t, agents[i])
	}
	assertWork := func() {
		t.Helper()
		tree := h.tree()
		observed, err := sessionstate.ObserveManagedWindows(tree, registry)
		if err != nil || len(observed) != len(ids) {
			t.Fatalf("managed windows changed: %v %v", observed, err)
		}
		for i, id := range ids {
			if observed[id].ContainerID != windows[i] || restoreCleanupWorkspace(tree, windows[i]) != "98" || findContainerByID(tree, windows[i]).PID != adapters[i] {
				t.Fatalf("context %s no longer uses original adapter/window on 98: %+v", id, observed[id])
			}
			if got := lifecycleSleepChild(t, adapters[i]); got != agents[i] || lifecycleProcessIdentity(t, agents[i]) != identities[i] {
				t.Fatalf("foreground agent replaced: context=%s pid=%d want=%d", id, got, agents[i])
			}
		}
		var saved sessionstate.Registry
		if err := sessionstate.RegistryStoreFor(h.state).LoadInto(&saved); err != nil || !reflect.DeepEqual(saved, registry) {
			t.Fatalf("daemon restart changed eligible-state snapshot (including already-missing context): %+v %v", saved, err)
		}
	}

	old := startLifecycleDaemon(t, h, executable)
	awaitLifecycleDaemon(t, h, old, windows)
	assertWork()
	if got := lifecycleRunningBuild(t, old.cmd.Process.Pid); got != oldBuild {
		t.Fatalf("old running build: %+v", got)
	}
	if err := os.Rename(replacement, executable); err != nil {
		t.Fatal(err)
	}
	if got, err := buildmetadata.ReadFile(executable); err != nil || got != newBuild {
		t.Fatalf("replacement installed: %+v %v", got, err)
	}
	if got := lifecycleRunningBuild(t, old.cmd.Process.Pid); got != oldBuild {
		t.Fatalf("replacement changed executing daemon: %+v", got)
	}
	assertWork()
	// A second daemon must fail without displacing the verified lock owner.
	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
	defer cancel()
	second := exec.CommandContext(ctx, executable, "daemon", "--socket", h.socket)
	second.Env, second.Dir = h.env, h.root
	output, err := second.CombinedOutput()
	if err == nil || ctx.Err() != nil || !strings.Contains(string(output), "already running") {
		t.Fatalf("second daemon did not report lock conflict: %v %s", err, output)
	}
	assertWork()
	old.stop(t)
	assertWork()
	fresh := startLifecycleDaemon(t, h, executable)
	awaitLifecycleDaemon(t, h, fresh, windows)
	if got := lifecycleRunningBuild(t, fresh.cmd.Process.Pid); got != newBuild {
		t.Fatalf("explicit restart did not load replacement: %+v", got)
	}
	// Repeated explicit restore must reuse the mapped contexts, without needing
	// Herdr or another adapter/agent. Verify the public CLI result too.
	for range 2 {
		for _, id := range ids {
			ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
			command := exec.CommandContext(ctx, executable, "--json", "restore", "--socket", h.socket, string(id))
			command.Env, command.Dir = h.env, h.root
			output, err := command.CombinedOutput()
			cancel()
			var result struct {
				Contexts []sessionstate.Context `json:"contexts"`
			}
			if err != nil || json.Unmarshal(output, &result) != nil || len(result.Contexts) != 1 || result.Contexts[0].ID != id {
				t.Fatalf("restore existing context %s: %v %s", id, err, output)
			}
		}
		assertWork()
	}
	fresh.stop(t)
	assertWork()
	t.Log("atomic binary replacement and explicit daemon restart retained exact windows, adapters, foreground agent identities and active missing-context policy; repeated restore created no duplicate managed windows")
}

func buildLifecycleDaemon(t *testing.T, path string, metadata buildmetadata.Metadata) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	stamp := "sway-session-build-v1|" + metadata.Version + "|" + metadata.Commit + "|true|end-sway-session-build-v1"
	flags := "-X main.version=" + metadata.Version + " -X main.commit=" + metadata.Commit + " -X main.modified=true -X github.com/marang/sway-session/internal/buildmetadata.Stamp=" + stamp
	command := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-ldflags", flags, "-o", path, ".")
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	command.WaitDelay = time.Second
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build disposable daemon: %v %s", err, output)
	}
}

type lifecycleDaemonProcess struct {
	cmd         *exec.Cmd
	done        chan struct{}
	err         error // published by closing done
	previousRun string
}

func startLifecycleDaemon(t *testing.T, h *restoreCleanupHeadless, executable string) *lifecycleDaemonProcess {
	t.Helper()
	log, err := os.CreateTemp(h.root, "lab134-daemon-*.log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = log.Close()
		if t.Failed() {
			output, _ := os.ReadFile(log.Name())
			t.Logf("owned daemon log: %s", output)
		}
	})
	command := exec.CommandContext(h.ctx, executable, "daemon", "--socket", h.socket)
	command.Env, command.Dir, command.Stdout, command.Stderr = h.env, h.root, log, log
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	report, err := sessionstate.RestoreReportStoreFor(h.state).LoadContext(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	process := &lifecycleDaemonProcess{cmd: command, done: make(chan struct{}), previousRun: report.AutomaticRunID}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		process.err = command.Wait()
		close(process.done)
	}()
	t.Cleanup(func() {
		select {
		case <-process.done:
		default:
			_ = command.Process.Kill()
			select {
			case <-process.done:
			case <-time.After(3 * time.Second):
				t.Error("owned daemon not reaped")
			}
		}
	})
	return process
}

func (p *lifecycleDaemonProcess) stop(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
		if p.err != nil {
			t.Fatalf("owned daemon did not exit cleanly: %v", p.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owned daemon did not stop after SIGTERM")
	}
}

func awaitLifecycleDaemon(t *testing.T, h *restoreCleanupHeadless, p *lifecycleDaemonProcess, windows []int64) {
	t.Helper()
	// The real held lock and all persistent marks show the new process reached
	// reconciliation; elapsed time alone is not treated as readiness.
	h.until("private daemon reconciliation", func() bool {
		select {
		case <-p.done:
			t.Fatalf("private daemon exited before readiness: %v", p.err)
		default:
		}
		file, err := os.OpenFile(filepath.Join(h.root, "run", "sway-session", "daemon.lock"), os.O_RDWR, 0)
		if err != nil {
			return false
		}
		defer file.Close()
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
			return false
		}
		if err != unix.EWOULDBLOCK {
			t.Fatalf("inspect private lock: %v", err)
		}
		tree := h.tree()
		for _, id := range windows {
			node := findContainerByID(tree, id)
			if node == nil || !slices.ContainsFunc(node.Marks, func(mark string) bool { return strings.HasPrefix(mark, "persist:") }) {
				return false
			}
		}
		report, err := sessionstate.RestoreReportStoreFor(h.state).LoadContext(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return report.AutomaticRunID != "" && report.AutomaticRunID != p.previousRun
	})
}

func lifecycleRunningBuild(t *testing.T, pid int) buildmetadata.Metadata {
	t.Helper()
	file, err := os.Open(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	metadata, err := buildmetadata.ReadExecutable(t.Context(), file)
	if err != nil {
		t.Fatal(err)
	}
	return metadata
}

func lifecycleSleepChild(t *testing.T, pid int) int {
	t.Helper()
	contents, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, field := range strings.Fields(string(contents)) {
		child, err := strconv.Atoi(field)
		if err != nil {
			t.Fatal(err)
		}
		comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", child))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(comm)) == "sleep" {
			if found != 0 {
				t.Fatal("duplicate owned foreground agents")
			}
			found = child
		}
	}
	return found
}

func lifecycleProcessIdentity(t *testing.T, pid int) string {
	t.Helper()
	contents, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(contents), ") ")
	fields := strings.Fields(rest)
	if !ok || len(fields) < 20 || fields[0] == "Z" {
		t.Fatalf("owned agent is missing or zombie: %q", contents)
	}
	return fmt.Sprintf("%d:%s", pid, fields[19]) // Linux stat field 22: start time
}
