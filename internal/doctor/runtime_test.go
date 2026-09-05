package doctor

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/marang/sway-session/internal/swayipc"
	"golang.org/x/sys/unix"
)

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
