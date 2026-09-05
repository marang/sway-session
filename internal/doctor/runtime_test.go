package doctor

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/sessionrequest"
	"github.com/marang/sway-session/internal/swayipc"
	"golang.org/x/sys/unix"
)

func TestReadOnlyProbeBoundsInheritedOutputPipes(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	_, err = runReadOnlyProbe(ctx, shell, "-c", "sleep 3 &")
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("inherited pipe was not bounded: %v", err)
	}
}

type runtimeTestSway struct {
	message swayipc.Message
	err     error
}

func (client runtimeTestSway) RequestContext(context.Context, swayipc.MessageType, []byte) (swayipc.Message, error) {
	return client.message, client.err
}
func (runtimeTestSway) Close() {}

func TestResolveSwayConfigPathUsesExplicitOnDiskPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	selection, err := resolveSwayConfigPathSelection(t.Context(), Options{SwayConfigPath: path})
	if err != nil {
		t.Fatalf("resolve explicit path: %v", err)
	}
	if selection.path != path || selection.live {
		t.Fatalf("selection = %#v, want explicit on-disk path", selection)
	}
}

func TestResolveSwayConfigPathUsesLiveVersionReply(t *testing.T) {
	previous := runtimeProbes.newSway
	runtimeProbes.newSway = func(string) swayRequester {
		return runtimeTestSway{message: swayipc.Message{Type: getVersionMessage, Payload: []byte(`{"loaded_config_file_name":"/tmp/sway-config"}`)}}
	}
	t.Cleanup(func() { runtimeProbes.newSway = previous })

	selection, err := resolveSwayConfigPathSelection(t.Context(), Options{Socket: "/tmp/sway.sock"})
	if err != nil {
		t.Fatalf("resolve live config: %v", err)
	}
	if selection.path != "/tmp/sway-config" || !selection.live {
		t.Fatalf("selection = %#v, want live config", selection)
	}
}

func TestResolveSwayConfigPathFallsBackWhenNoIPC(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	previous := runtimeProbes.getenv
	runtimeProbes.getenv = func(key string) string {
		if key == "XDG_CONFIG_HOME" {
			return home
		}
		return ""
	}
	t.Cleanup(func() { runtimeProbes.getenv = previous })

	selection, err := resolveSwayConfigPathSelection(t.Context(), Options{})
	if err != nil {
		t.Fatalf("resolve fallback config: %v", err)
	}
	if want := filepath.Join(home, "sway", "config"); selection.path != want || selection.live {
		t.Fatalf("selection = %#v, want fallback %q", selection, want)
	}
}

func TestResolveSwayConfigPathRejectsMalformedLiveReply(t *testing.T) {
	previous := runtimeProbes.newSway
	runtimeProbes.newSway = func(string) swayRequester {
		return runtimeTestSway{message: swayipc.Message{Type: getVersionMessage, Payload: []byte(`{"loaded_config_file_name":"relative"}`)}}
	}
	t.Cleanup(func() { runtimeProbes.newSway = previous })

	if _, err := resolveSwayConfigPath(t.Context(), Options{Socket: "/tmp/sway.sock"}); err == nil {
		t.Fatal("malformed live config path succeeded")
	}
}

func TestPrivateObjectRejectsSymlinkAndAcceptsOwnerOnlyFile(t *testing.T) {
	directory := t.TempDir()
	file := filepath.Join(directory, "owner-only")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("write private file: %v", err)
	}
	if _, err := inspectPrivateObjectStat(file, unix.S_IFREG, 0o600, false); err != nil {
		t.Fatalf("inspect private file: %v", err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatalf("make link: %v", err)
	}
	if _, err := inspectPrivateObjectStat(link, unix.S_IFREG, 0o600, false); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestPrivateObjectAcceptsOwnerOnlySocketWithoutFollowingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("listen socket: %v", err)
	}
	defer listener.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod socket: %v", err)
	}
	if _, err := inspectPrivateObjectStat(path, unix.S_IFSOCK, 0o600, false); err != nil {
		t.Fatalf("inspect private socket: %v", err)
	}
}

func TestOptionalSocketDoesNotClaimUnverifiedListenerIsHealthy(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, sessionrequest.SocketFilename)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("listen socket: %v", err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod socket: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	checks := inspectOptionalSockets(root, nil)
	check := findRuntimeCheck(t, checks, "broker.session_start")
	if check.Status != Unavailable || !slices.Contains(check.Evidence, "path="+path) {
		t.Fatalf("stale socket check = %#v, want unavailable with path evidence", check)
	}
}

func TestStatePathsRejectUnsafeSQLiteSidecar(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "sway-session")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("make state root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, sessionstate.StateDatabaseFilename), nil, 0o600); err != nil {
		t.Fatalf("write state database: %v", err)
	}
	sidecar := filepath.Join(root, sessionstate.StateDatabaseFilename+"-wal")
	if err := os.Symlink("missing", sidecar); err != nil {
		t.Fatalf("make unsafe sidecar: %v", err)
	}

	check := inspectStatePaths()
	if check.Status != Error || check.Hint == "" || !slices.Contains(check.Evidence, "path="+sidecar) {
		t.Fatalf("state check = %#v, want actionable sidecar error", check)
	}
}

func TestStatePathsRejectOversizedMainDatabase(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "sway-session")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("make state root: %v", err)
	}
	database := filepath.Join(root, sessionstate.StateDatabaseFilename)
	if err := os.WriteFile(database, nil, 0o600); err != nil {
		t.Fatalf("write state database: %v", err)
	}
	if err := os.Truncate(database, maxStateDatabaseSize+1); err != nil {
		t.Fatalf("grow state database: %v", err)
	}

	check := inspectStatePaths()
	if check.Status != Error || check.Hint == "" || !slices.Contains(check.Evidence, "path="+database) {
		t.Fatalf("state check = %#v, want actionable size error", check)
	}
}

func TestDetachedPrivateSidecarMatchesProductionRaceHandling(t *testing.T) {
	stat := unix.Stat_t{Mode: unix.S_IFREG | 0o600, Uid: uint32(os.Geteuid()), Nlink: 0}
	if !detachedPrivateSidecar(stat) {
		t.Fatal("safe detached sidecar was not treated as absent")
	}
	stat.Mode = unix.S_IFLNK | 0o777
	if detachedPrivateSidecar(stat) {
		t.Fatal("detached symlink was treated as an absent SQLite sidecar")
	}
}

func TestDaemonArgumentsAcceptGlobalJSONAnywhere(t *testing.T) {
	tests := []struct {
		arguments []string
		want      bool
	}{
		{[]string{"/usr/bin/sway-session", "daemon"}, true},
		{[]string{"/usr/bin/sway-session", "--json", "daemon"}, true},
		{[]string{"/usr/bin/sway-session", "daemon", "--json"}, true},
		{[]string{"/usr/bin/sway-session", "--json", "daemon", "--socket", "/tmp/sway.sock", "--json"}, true},
		{[]string{"/usr/bin/sway-session", "daemon", "--socket=/tmp/sway.sock"}, true},
		{[]string{"/usr/bin/sway-session", "daemon", "--socket", "relative"}, false},
		{[]string{"/usr/bin/sway-session", "restore", "--json"}, false},
	}
	for _, test := range tests {
		if got := isDaemonCommand(test.arguments); got != test.want {
			t.Errorf("isDaemonCommand(%q) = %v, want %v", test.arguments, got, test.want)
		}
	}
}

func TestOpenBoundedBinaryRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binary")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("make FIFO: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		file, _, err := openBoundedBinary(path)
		if file != nil {
			_ = file.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO accepted as a binary")
		}
	case <-time.After(time.Second):
		t.Fatal("opening FIFO blocked")
	}
}

func TestDigestUsesPinnedBinaryAndHonorsContext(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	link := filepath.Join(directory, "binary")
	if err := os.WriteFile(target, []byte("first"), 0o700); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("link binary: %v", err)
	}
	file, info, err := openBoundedBinary(link)
	if err != nil {
		t.Fatalf("open binary: %v", err)
	}
	defer file.Close()
	oldTarget := filepath.Join(directory, "old-target")
	if err := os.Rename(target, oldTarget); err != nil {
		t.Fatalf("pin old binary inode: %v", err)
	}
	if err := os.WriteFile(target, []byte("second"), 0o700); err != nil {
		t.Fatalf("replace binary path: %v", err)
	}
	digest, err := digestOpenFile(t.Context(), file, info.Size())
	if err != nil {
		t.Fatalf("digest pinned binary: %v", err)
	}
	if want := sha256.Sum256([]byte("first")); digest != want {
		t.Fatalf("digest = %x, want pinned content %x", digest, want)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := digestOpenFile(ctx, file, info.Size()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled digest returned %v", err)
	}
}

func TestOpenBoundedBinaryAcceptsProcExecutableDescriptor(t *testing.T) {
	file, _, err := openBoundedBinary("/proc/self/exe")
	if err != nil {
		t.Fatalf("open proc executable: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close proc executable: %v", err)
	}
}

func TestServiceCheckHasStableUnavailableSwayTreeAndDoesNotInitializeState(t *testing.T) {
	root := t.TempDir()
	runtimeHome := filepath.Join(root, "runtime")
	stateHome := filepath.Join(root, "state")
	configHome := filepath.Join(root, "config")
	for _, directory := range []string{runtimeHome, stateHome, configHome} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatalf("make environment directory: %v", err)
		}
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeHome)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("SWAYSOCK", "")
	swayConfig := filepath.Join(root, "sway-config")
	if err := os.WriteFile(swayConfig, []byte(healthySwayConfig()), 0o600); err != nil {
		t.Fatalf("write Sway config: %v", err)
	}
	missingSessionConfig := filepath.Join(root, "missing-session-config")

	report := New(Options{ConfigPath: missingSessionConfig, SwayConfigPath: swayConfig}).Check(t.Context())
	expectedIDs := []string{
		"session.config", "terminal.adapter", "herdr.executable", "herdr.pane_history", "herdr.capabilities",
		"sway.ipc", "sway.tree", "runtime.paths", "state.paths", "daemon.lock", "daemon.binary",
		"broker.session_start", "broker.agent_report", "broker.codex_report", "apparmor", "sway.integration",
	}
	seen := make(map[string]int, len(report.Checks))
	for _, check := range report.Checks {
		seen[check.ID]++
	}
	for _, id := range expectedIDs {
		if seen[id] != 1 {
			t.Errorf("check %q count = %d, want exactly one", id, seen[id])
		}
	}
	tree := findRuntimeCheck(t, report.Checks, "sway.tree")
	if tree.Status != Unavailable || !strings.Contains(tree.Detail, "No Sway IPC socket") {
		t.Fatalf("sway tree check = %#v, want stable unavailable result", tree)
	}
	if _, err := os.Stat(filepath.Join(runtimeHome, "sway-session")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doctor initialized runtime state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateHome, "sway-session")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doctor initialized session state: %v", err)
	}
}

func findRuntimeCheck(t *testing.T, checks []Check, id string) Check {
	t.Helper()
	for _, check := range checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("check %q not found in %#v", id, checks)
	return Check{}
}

func TestBoundedProbeOutputDoesNotLeakOverflow(t *testing.T) {
	buffer := &limitedBuffer{}
	if _, err := buffer.Write(make([]byte, maxCapabilityOutput+1)); err != nil {
		t.Fatalf("write bounded output: %v", err)
	}
	if !buffer.overflow || len(buffer.Bytes()) != maxCapabilityOutput {
		t.Fatalf("buffer overflow=%v size=%d", buffer.overflow, len(buffer.Bytes()))
	}
}

func TestLockParserRequiresExactDeviceAndInode(t *testing.T) {
	if !sameLockDeviceInode("08:01:42", 8, 1, 42) {
		t.Fatal("matching lock was rejected")
	}
	if sameLockDeviceInode("08:01:43", 8, 1, 42) {
		t.Fatal("different inode accepted")
	}
	if sameLockDeviceInode("bad", 8, 1, 42) {
		t.Fatal("malformed lock accepted")
	}
}

func TestReadFileBoundedRejectsLargeFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "large")
	if err := os.WriteFile(file, []byte("1234"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	_, err := readFileBounded(file, 3)
	if err == nil {
		t.Fatal("large file accepted")
	}
}
