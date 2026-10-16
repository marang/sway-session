package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/statefile"
	"golang.org/x/sys/unix"
)

const reachFrameLimit = 8 * 1024
const reachExchangeTimeout = 30 * time.Second

var reachCompositorPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type reachRequest struct {
	Version       int                   `json:"version"`
	CompositorID  string                `json:"compositor_id"`
	EnvironmentID session.EnvironmentID `json:"environment_id"`
	Primary       session.ContextID     `json:"primary,omitempty"`
}

func (r reachRequest) validate() error {
	if !reachCompositorPattern.MatchString(r.CompositorID) {
		return errors.New("invalid Reach compositor identity")
	}
	if r.Version != 1 {
		return errors.New("unsupported Reach protocol version")
	}
	if err := r.EnvironmentID.Validate(); err != nil {
		return err
	}
	if r.Primary != "" {
		_, err := session.ParseContextID(string(r.Primary))
		return err
	}
	return nil
}

// Each response frame is bounded independently; environments have no total
// member limit. Only the daemon handler owns execution and current policy.
type reachFrame struct {
	Version       int                        `json:"version"`
	Type          string                     `json:"type"`
	EnvironmentID session.EnvironmentID      `json:"environment_id,omitempty"`
	Primary       session.ContextID          `json:"primary,omitempty"`
	Member        *session.ReachMemberResult `json:"member,omitempty"`
	Error         string                     `json:"error,omitempty"`
}

func defaultReachSocketPath() (string, error) {
	root := os.Getenv("XDG_RUNTIME_DIR")
	if !filepath.IsAbs(root) {
		return "", errors.New("XDG_RUNTIME_DIR must be an absolute path")
	}
	return filepath.Join(root, "sway-session", "reach.sock"), nil
}

func reachSocketPathValid(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && filepath.Base(path) != string(filepath.Separator)
}

// The existing report/start brokers keep their socket helpers package-private.
// Keep this endpoint's small descriptor-pinned copy local rather than changing
// either established wire protocol or refactoring unrelated brokers.
func reachPinSocket(directory *os.File, name string) (int, unix.Stat_t, error) {
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, unix.Stat_t{}, err
	}
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, stat, err
	}
	return fd, stat, nil
}

func reachSameSocket(a, b unix.Stat_t) bool { return a.Dev == b.Dev && a.Ino == b.Ino }
func reachOwnedSocket(stat unix.Stat_t) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFSOCK && stat.Uid == uint32(os.Geteuid())
}

func reachUnlinkSocket(directory *os.File, name string, pinned unix.Stat_t) error {
	var current unix.Stat_t
	err := unix.Fstatat(int(directory.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if !reachSameSocket(current, pinned) {
		return unix.EADDRINUSE
	}
	return unix.Unlinkat(int(directory.Fd()), name, 0)
}

func reachRemoveStale(directory *os.File, name string, probe func(string, string, time.Duration) (net.Conn, error)) error {
	fd, stat, err := reachPinSocket(directory, name)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if !reachOwnedSocket(stat) {
		return errors.New("reach endpoint is not an owned socket")
	}
	connection, err := probe("unix", fmt.Sprintf("/proc/self/fd/%d", fd), 100*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		return unix.EADDRINUSE
	}
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if !errors.Is(err, unix.ECONNREFUSED) {
		return err
	}
	return reachUnlinkSocket(directory, name, stat)
}

func reachPeerOwned(connection *net.UnixConn) error {
	raw, err := connection.SyscallConn()
	if err != nil {
		return err
	}
	var credentials *unix.Ucred
	var peerErr error
	err = raw.Control(func(fd uintptr) {
		credentials, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err != nil {
		return err
	}
	if peerErr != nil {
		return peerErr
	}
	return reachCredentialsOwned(credentials)
}

func reachCredentialsOwned(credentials *unix.Ucred) error {
	if credentials == nil || credentials.Uid != uint32(os.Geteuid()) {
		return errors.New("reach peer is not the current user")
	}
	return nil
}

// Check duplicate object fields as well as unknown fields; encoding/json alone
// otherwise accepts a later duplicate intent. The byte and nesting budgets also
// apply to callers supplying malformed frames directly.
func reachDecode(data []byte, value any) error {
	if len(data) > reachFrameLimit {
		return errors.New("reach frame exceeds byte budget")
	}
	tokens := json.NewDecoder(bytes.NewReader(data))
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 16 {
			return errors.New("reach frame exceeds nesting budget")
		}
		token, err := tokens.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for tokens.More() {
				key, err := tokens.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate Reach field")
				}
				seen[name] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for tokens.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid Reach JSON")
		}
		_, err = tokens.Token()
		return err
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := tokens.Token(); !errors.Is(err, io.EOF) {
		return errors.New("reach frame contains trailing data")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func reachReadLine(reader *bufio.Reader) ([]byte, error) {
	data, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(data) > reachFrameLimit {
		return nil, errors.New("reach frame exceeds byte budget")
	}
	return data, err
}

func reachWriteFrame(connection net.Conn, frame reachFrame) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > reachFrameLimit {
		return errors.New("reach frame exceeds byte budget")
	}
	n, err := connection.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

type reachServer struct {
	listener  *net.UnixListener
	directory *os.File
	socketFD  int
	name      string
	stat      unix.Stat_t
	ctx       context.Context
	cancel    context.CancelFunc
	handler   func(context.Context, reachRequest) (session.ReachResult, error)
	workers   chan struct{}
	wait      sync.WaitGroup
	once      sync.Once
	closeErr  error
}

func startReachServer(ctx context.Context, socketPath string, handler func(context.Context, reachRequest) (session.ReachResult, error)) (io.Closer, error) {
	if ctx == nil || handler == nil || !reachSocketPathValid(socketPath) {
		return nil, errors.New("invalid Reach server configuration")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, err := statefile.OpenPrivateDirectory(filepath.Dir(socketPath), true)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(socketPath)
	if err := reachRemoveStale(directory, name, net.DialTimeout); err != nil {
		_ = directory.Close()
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: fmt.Sprintf("/proc/self/fd/%d/%s", directory.Fd(), name), Net: "unix"})
	if err != nil {
		_ = directory.Close()
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	fd, stat, err := reachPinSocket(directory, name)
	if err == nil && !reachOwnedSocket(stat) {
		err = errors.New("reach endpoint is not an owned socket")
	}
	if err == nil {
		err = os.Chmod(fmt.Sprintf("/proc/self/fd/%d", fd), 0o600)
	}
	if err == nil {
		err = unix.Fstat(fd, &stat)
	}
	if err == nil {
		var current unix.Stat_t
		err = unix.Fstatat(int(directory.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW)
		if err == nil && (!reachSameSocket(current, stat) || stat.Mode&0o777 != 0o600) {
			err = errors.New("reach endpoint is not an owner-only socket")
		}
	}
	if err != nil {
		_ = listener.Close()
		if fd >= 0 {
			_ = reachUnlinkSocket(directory, name, stat)
			_ = unix.Close(fd)
		}
		_ = directory.Close()
		return nil, err
	}
	serverCtx, cancel := context.WithCancel(ctx)
	server := &reachServer{listener: listener, directory: directory, socketFD: fd, name: name, stat: stat, ctx: serverCtx, cancel: cancel, handler: handler, workers: make(chan struct{}, 8)}
	server.wait.Add(1)
	go server.serve()
	return server, nil
}

func (server *reachServer) serve() {
	defer server.wait.Done()
	stop := context.AfterFunc(server.ctx, func() { _ = server.listener.Close() })
	defer stop()
	for {
		connection, err := server.listener.AcceptUnix()
		if err != nil {
			return
		}
		select {
		case server.workers <- struct{}{}:
			server.wait.Add(1)
			go func() { defer server.wait.Done(); defer func() { <-server.workers }(); server.handle(connection) }()
		default:
			_ = connection.Close()
		}
	}
}

func (server *reachServer) handle(connection *net.UnixConn) {
	defer connection.Close()
	ctx, cancel := context.WithTimeout(server.ctx, reachExchangeTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if err := connection.SetDeadline(time.Now().Add(reachExchangeTimeout)); err != nil {
		return
	}
	reject := func() {
		_ = reachWriteFrame(connection, reachFrame{Version: 1, Type: "error", Error: "Reach request rejected"})
	}
	if err := reachPeerOwned(connection); err != nil {
		reject()
		return
	}
	reader := bufio.NewReaderSize(connection, reachFrameLimit)
	data, err := reachReadLine(reader)
	if errors.Is(err, io.EOF) && len(data) == 0 {
		return
	}
	if err != nil {
		reject()
		return
	}
	var request reachRequest
	if reachDecode(data, &request) != nil || request.validate() != nil || reader.Buffered() != 0 {
		reject()
		return
	}
	// The client keeps its write side open while waiting. EOF, unexpected extra
	// bytes, and read failures revoke pending execution through the same context.
	go func() { var extra [1]byte; _, _ = reader.Read(extra[:]); cancel() }()
	if ctx.Err() != nil {
		return
	}
	result, err := server.handler(ctx, request)
	if err != nil {
		_ = reachWriteFrame(connection, reachFrame{Version: 1, Type: "error", Error: "Reach failed"})
		return
	}
	if ctx.Err() != nil {
		return
	}
	if result.EnvironmentID != request.EnvironmentID || request.Primary != "" && result.Primary != request.Primary {
		reject()
		return
	}
	if result.Primary != "" {
		if _, err := session.ParseContextID(string(result.Primary)); err != nil {
			reject()
			return
		}
	}
	if reachWriteFrame(connection, reachFrame{Version: 1, Type: "header", EnvironmentID: result.EnvironmentID, Primary: result.Primary}) != nil {
		return
	}
	for _, member := range result.Members {
		if ctx.Err() != nil {
			return
		}
		if reachValidateMember(member) != nil {
			reject()
			return
		}
		if reachWriteFrame(connection, reachFrame{Version: 1, Type: "member", Member: &member}) != nil {
			return
		}
	}
	_ = reachWriteFrame(connection, reachFrame{Version: 1, Type: "end"})
}

func (server *reachServer) Close() error {
	server.once.Do(func() {
		server.cancel()
		_ = server.listener.Close()
		server.wait.Wait()
		err := reachUnlinkSocket(server.directory, server.name, server.stat)
		if errors.Is(err, unix.EADDRINUSE) {
			err = nil
		}
		server.closeErr = errors.Join(err, unix.Close(server.socketFD), server.directory.Close())
	})
	return server.closeErr
}

func reachValidateMember(member session.ReachMemberResult) error {
	if _, err := session.ParseContextID(string(member.ContextID)); err != nil {
		return err
	}
	switch member.Status {
	case session.ReachWouldFocus, session.ReachWouldOpen, session.ReachAlreadyPresent, session.ReachSkipped, session.ReachUncertain, session.ReachBlocked, session.ReachFocused, session.ReachOpening, session.ReachOpened, session.ReachFailed, session.ReachCanceled:
	default:
		return errors.New("invalid Reach member status")
	}
	if len(member.Reason) > 4096 {
		return errors.New("reach reason exceeds byte budget")
	}
	return nil
}

func reachContextError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return err
}

func sendReachRequest(ctx context.Context, socketPath string, request reachRequest) (session.ReachResult, error) {
	if ctx == nil || !reachSocketPathValid(socketPath) {
		return session.ReachResult{}, errors.New("invalid Reach client configuration")
	}
	if err := request.validate(); err != nil {
		return session.ReachResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, reachExchangeTimeout)
	defer cancel()
	directory, err := statefile.OpenPrivateDirectory(filepath.Dir(socketPath), false)
	if err != nil {
		return session.ReachResult{}, err
	}
	defer directory.Close()
	fd, stat, err := reachPinSocket(directory, filepath.Base(socketPath))
	if err != nil {
		return session.ReachResult{}, err
	}
	defer unix.Close(fd)
	if !reachOwnedSocket(stat) || stat.Mode&0o777 != 0o600 {
		return session.ReachResult{}, errors.New("reach endpoint is not an owner-only socket")
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return session.ReachResult{}, reachContextError(ctx, err)
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return session.ReachResult{}, errors.New("reach endpoint is not Unix")
	}
	if err := reachPeerOwned(unixConnection); err != nil {
		return session.ReachResult{}, err
	}
	deadline, _ := ctx.Deadline()
	if err := connection.SetDeadline(deadline); err != nil {
		return session.ReachResult{}, err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return session.ReachResult{}, err
	}
	data = append(data, '\n')
	if len(data) > reachFrameLimit {
		return session.ReachResult{}, errors.New("reach request exceeds byte budget")
	}
	if n, err := connection.Write(data); err != nil {
		return session.ReachResult{}, reachContextError(ctx, err)
	} else if n != len(data) {
		return session.ReachResult{}, io.ErrShortWrite
	}
	reader := bufio.NewReaderSize(connection, reachFrameLimit)
	result := session.ReachResult{}
	seen := map[session.ContextID]bool{}
	header := false
	for {
		data, err := reachReadLine(reader)
		if err != nil {
			return session.ReachResult{}, reachContextError(ctx, err)
		}
		var frame reachFrame
		if err := reachDecode(data, &frame); err != nil {
			return session.ReachResult{}, err
		}
		if frame.Version != 1 {
			return session.ReachResult{}, errors.New("unsupported Reach response version")
		}
		switch frame.Type {
		case "error":
			if frame.EnvironmentID != "" || frame.Primary != "" || frame.Member != nil || frame.Error == "" {
				return session.ReachResult{}, errors.New("invalid Reach error frame")
			}
			return session.ReachResult{}, errors.New("reach request failed")
		case "header":
			if header || frame.EnvironmentID != request.EnvironmentID || frame.Member != nil || frame.Error != "" || request.Primary != "" && frame.Primary != request.Primary {
				return session.ReachResult{}, errors.New("invalid Reach header")
			}
			if frame.Primary != "" {
				if _, err := session.ParseContextID(string(frame.Primary)); err != nil {
					return session.ReachResult{}, err
				}
			}
			result.EnvironmentID, result.Primary, result.Members = frame.EnvironmentID, frame.Primary, []session.ReachMemberResult{}
			header = true
		case "member":
			if !header || frame.Member == nil || frame.EnvironmentID != "" || frame.Primary != "" || frame.Error != "" {
				return session.ReachResult{}, errors.New("invalid Reach member frame")
			}
			member := *frame.Member
			if err := reachValidateMember(member); err != nil {
				return session.ReachResult{}, err
			}
			if seen[member.ContextID] || member.Primary != (member.ContextID == result.Primary) {
				return session.ReachResult{}, errors.New("inconsistent Reach member")
			}
			seen[member.ContextID] = true
			result.Members = append(result.Members, member)
		case "end":
			if !header || frame.Member != nil || frame.EnvironmentID != "" || frame.Primary != "" || frame.Error != "" {
				return session.ReachResult{}, errors.New("invalid Reach end frame")
			}
			if result.Primary != "" && !seen[result.Primary] {
				return session.ReachResult{}, errors.New("reach response omits its primary member")
			}
			if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
				if err != nil {
					return session.ReachResult{}, reachContextError(ctx, err)
				}
				return session.ReachResult{}, errors.New("reach response contains trailing data")
			}
			return result, nil
		default:
			return session.ReachResult{}, errors.New("invalid Reach frame type")
		}
	}
}
