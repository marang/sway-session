package swayipc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// ErrCompositorChanged means a pinned compositor lifetime no longer matches or
// cannot be verified. No request is sent on a connection that fails this check.
var ErrCompositorChanged = errors.New("sway compositor lifetime changed or cannot be verified")

type compositorLifetime struct {
	socketID string
	peer     compositorPeer
}

type compositorPeer struct {
	pid       int32
	uid       uint32
	gid       uint32
	startTime uint64
}

// PinCompositor creates an independently connected, lifetime-pinned client for
// one operation. Call it before capturing the operation's tree/identity, use the
// returned client for every request in that operation, and defer its Close.
// The source client's connection and ordinary reconnect behavior are unchanged.
// When called on an already pinned client, the scope must match that same pin.
func (client *Client) PinCompositor(ctx context.Context) (*Client, error) {
	if client == nil {
		return nil, errors.New("pin compositor requires a client")
	}
	scope := &Client{socket: client.socket, requestTimeout: client.requestTimeout}
	if client.compositor != nil {
		pin := *client.compositor
		scope.compositor = &pin
	}
	if _, err := scope.LifecycleCompositorID(ctx); err != nil {
		scope.Close()
		return nil, err
	}
	return scope, nil
}

// LifecycleCompositorID opts this client into lifetime-pinned requests. It
// establishes a verified connection and returns the socket lifetime digest used
// by sway-session's compositorIdentity (path, device, inode, ctime and owner).
// Connected peer credentials and process start time additionally pin reconnects
// within this client. Close retains the pin. Calls on Client must be serialized.
// No IPC message is sent here, and failure never silently selects a new lifetime.
func (client *Client) LifecycleCompositorID(ctx context.Context) (string, error) {
	if client == nil || ctx == nil {
		return "", errors.New("pin compositor requires a client and context")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	timeout := client.requestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if client.compositor != nil {
		if err := client.ensurePinnedCompositor(ctx); err != nil {
			return "", err
		}
		return client.compositor.socketID, nil
	}
	// A connection opened before opt-in was not bound to socket metadata. It
	// could still refer to an old listener after its pathname was replaced.
	client.Close()
	connection, lifetime, err := dialCompositor(ctx, client.socket, DialContext)
	if err != nil {
		return "", err
	}
	client.conn, client.compositor = connection, &lifetime
	return lifetime.socketID, nil
}

func (client *Client) ensurePinnedCompositor(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if client.conn == nil {
		id, err := compositorSocketID(client.socket)
		if err != nil || id != client.compositor.socketID {
			return errors.Join(ErrCompositorChanged, err)
		}
		connection, lifetime, err := dialCompositor(ctx, client.socket, DialContext)
		if err != nil {
			return errors.Join(ErrCompositorChanged, err)
		}
		if lifetime != *client.compositor {
			_ = connection.Close()
			return ErrCompositorChanged
		}
		client.conn = connection
		return nil
	}
	id, err := compositorSocketID(client.socket)
	if err == nil && id != client.compositor.socketID {
		err = ErrCompositorChanged
	}
	if err == nil {
		var peer compositorPeer
		peer, err = connectedCompositorPeer(client.conn)
		if err == nil && peer != client.compositor.peer {
			err = ErrCompositorChanged
		}
	}
	if err != nil {
		client.Close()
		return errors.Join(ErrCompositorChanged, err)
	}
	return ctx.Err()
}

// dialCompositor connects through a descriptor for the observed socket inode,
// rather than resolving its pathname again during connect. Path observations
// bracket dial and peer inspection; replacement at either boundary fails closed.
func dialCompositor(ctx context.Context, path string, dial func(context.Context, string) (*Conn, error)) (*Conn, compositorLifetime, error) {
	var lifetime compositorLifetime
	before, err := compositorSocketID(path)
	if err != nil {
		return nil, lifetime, err
	}
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, lifetime, fmt.Errorf("pin sway IPC socket inode: %w", err)
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, lifetime, err
	}
	id, err := compositorSocketStatID(path, stat)
	if err != nil || id != before {
		return nil, lifetime, errors.Join(ErrCompositorChanged, err)
	}
	connection, err := dial(ctx, fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return nil, lifetime, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = connection.Close()
		}
	}()
	peer, err := connectedCompositorPeer(connection)
	if err != nil {
		return nil, lifetime, err
	}
	after, err := compositorSocketID(path)
	if err != nil || after != before {
		return nil, lifetime, errors.Join(ErrCompositorChanged, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, lifetime, err
	}
	keep = true
	return connection, compositorLifetime{socketID: before, peer: peer}, nil
}

func compositorSocketID(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("sway IPC socket must be a clean absolute path")
	}
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return "", fmt.Errorf("inspect sway IPC socket: %w", err)
	}
	return compositorSocketStatID(path, stat)
}

func compositorSocketStatID(path string, stat unix.Stat_t) (string, error) {
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Uid != uint32(os.Geteuid()) {
		return "", errors.New("sway IPC endpoint must be a Unix socket owned by the current user")
	}
	evidence := fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%d\x00%d", path, stat.Dev, stat.Ino, stat.Ctim.Sec, stat.Ctim.Nsec, stat.Uid)
	digest := sha256.Sum256([]byte(evidence))
	return hex.EncodeToString(digest[:]), nil
}

func connectedCompositorPeer(connection *Conn) (compositorPeer, error) {
	var peer compositorPeer
	provider, ok := connection.current().(interface {
		SyscallConn() (syscall.RawConn, error)
	})
	if !ok {
		return peer, errors.New("sway IPC connection has no peer credentials")
	}
	raw, err := provider.SyscallConn()
	if err != nil {
		return peer, err
	}
	var credentials *unix.Ucred
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, credentialErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return peer, err
	}
	if credentialErr != nil {
		return peer, credentialErr
	}
	if credentials.Uid != uint32(os.Geteuid()) || credentials.Pid <= 0 {
		return peer, errors.New("sway IPC peer must be a process owned by the current user")
	}
	startTime, err := compositorProcessStartTime(credentials.Pid)
	if err != nil {
		return peer, fmt.Errorf("verify sway IPC peer lifetime: %w", err)
	}
	return compositorPeer{pid: credentials.Pid, uid: credentials.Uid, gid: credentials.Gid, startTime: startTime}, nil
}

func compositorProcessStartTime(pid int32) (uint64, error) {
	file, err := os.Open(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return 0, err
	}
	if len(data) > 4096 {
		return 0, errors.New("compositor process stat exceeds safety budget")
	}
	return parseCompositorProcessStartTime(string(data), pid)
}

func parseCompositorProcessStartTime(data string, pid int32) (uint64, error) {
	// comm (field 2) may contain spaces and parentheses; starttime is field 22.
	end := strings.LastIndexByte(data, ')')
	if !strings.HasPrefix(data, strconv.FormatInt(int64(pid), 10)+" (") || end < 0 {
		return 0, errors.New("invalid compositor process stat identity")
	}
	fields := strings.Fields(data[end+1:])
	if len(fields) <= 19 {
		return 0, errors.New("incomplete compositor process stat")
	}
	return strconv.ParseUint(fields[19], 10, 64)
}
