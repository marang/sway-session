package codexreport

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
	"sync"
	"syscall"
	"time"

	"github.com/marang/sway-title-animator/internal/statefile"
	"golang.org/x/sys/unix"
)

const reportExchangeTimeout = 2 * time.Second

type Handler func(context.Context, Report) error

type Server struct {
	listener    *net.UnixListener
	directory   *os.File
	socketFD    int
	socketName  string
	socketStat  unix.Stat_t
	handler     Handler
	reportError func(error)
	done        chan struct{}
	closeOnce   sync.Once
	wait        sync.WaitGroup
	workers     chan struct{}
}

func StartServer(socketPath string, handler Handler, reportError func(error)) (*Server, error) {
	if handler == nil {
		return nil, errors.New("codex report handler is nil")
	}
	if socketPath == "" || !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath {
		return nil, errors.New("codex report socket must be a clean absolute path")
	}
	socketName := filepath.Base(socketPath)
	if socketName == "." || socketName == string(filepath.Separator) {
		return nil, errors.New("codex report socket must have a base name")
	}
	directory, err := statefile.OpenPrivateDirectory(filepath.Dir(socketPath), true)
	if err != nil {
		return nil, fmt.Errorf("prepare Codex report directory: %w", err)
	}
	cleanup := func() { _ = directory.Close() }
	if err := removeStaleSocket(directory, socketName); err != nil {
		cleanup()
		return nil, err
	}
	descriptorPath := fmt.Sprintf("/proc/self/fd/%d/%s", directory.Fd(), socketName)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: descriptorPath, Net: "unix"})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("listen on Codex report socket: %w", err)
	}
	listener.SetUnlinkOnClose(false)
	socketFD, socketStat, err := pinSocketAt(directory, socketName)
	if err != nil {
		_ = listener.Close()
		cleanup()
		return nil, fmt.Errorf("inspect Codex report socket: %w", err)
	}
	closeSetup := func() {
		_ = listener.Close()
		_ = unix.Close(socketFD)
		cleanup()
	}
	if socketStat.Mode&unix.S_IFMT != unix.S_IFSOCK || socketStat.Uid != uint32(os.Geteuid()) {
		closeSetup()
		return nil, errors.New("codex report endpoint is not a socket owned by the current user")
	}
	if err := unix.Fchmodat(int(directory.Fd()), socketName, 0o600, 0); err != nil {
		closeSetup()
		return nil, fmt.Errorf("restrict Codex report socket: %w", err)
	}
	if err := unix.Fstat(socketFD, &socketStat); err != nil {
		closeSetup()
		return nil, fmt.Errorf("inspect Codex report socket: %w", err)
	}
	var currentSocket unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), socketName, &currentSocket, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		closeSetup()
		return nil, fmt.Errorf("reinspect Codex report socket: %w", err)
	}
	if !sameSocketFile(currentSocket, socketStat) || socketStat.Mode&0o777 != 0o600 {
		closeSetup()
		return nil, errors.New("codex report endpoint is not an owner-only socket")
	}
	server := &Server{
		listener: listener, directory: directory, socketFD: socketFD, socketName: socketName, socketStat: socketStat,
		handler: handler, reportError: reportError, done: make(chan struct{}), workers: make(chan struct{}, 8),
	}
	server.wait.Add(1)
	go server.serve()
	return server, nil
}

func removeStaleSocket(directory *os.File, name string) error {
	return removeStaleSocketWithProbe(directory, name, net.DialTimeout)
}

func removeStaleSocketWithProbe(
	directory *os.File,
	name string,
	probe func(string, string, time.Duration) (net.Conn, error),
) error {
	socketFD, stat, err := pinSocketAt(directory, name)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing Codex report endpoint: %w", err)
	}
	defer unix.Close(socketFD)
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("existing Codex report endpoint is not a socket owned by the current user")
	}
	descriptorPath := fmt.Sprintf("/proc/self/fd/%d", socketFD)
	connection, dialErr := probe("unix", descriptorPath, 100*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		return syscall.EADDRINUSE
	}
	if errors.Is(dialErr, unix.ENOENT) {
		return nil
	}
	if !errors.Is(dialErr, unix.ECONNREFUSED) {
		return fmt.Errorf("probe existing Codex report endpoint: %w", dialErr)
	}
	var current unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		return nil
	} else if err != nil {
		return fmt.Errorf("reinspect existing Codex report endpoint: %w", err)
	}
	if !sameSocketFile(current, stat) {
		return syscall.EADDRINUSE
	}
	if err := unix.Unlinkat(int(directory.Fd()), name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove stale Codex report socket: %w", err)
	}
	return nil
}

func pinSocketAt(directory *os.File, name string) (int, unix.Stat_t, error) {
	// Holding an O_PATH reference prevents an unlinked socket inode from being
	// recycled into a replacement that would pass a later device/inode check.
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, unix.Stat_t{}, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, unix.Stat_t{}, err
	}
	return fd, stat, nil
}

func sameSocketFile(left unix.Stat_t, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino
}

func unlinkPinnedSocket(directory *os.File, name string, pinned unix.Stat_t) error {
	var current unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		return nil
	} else if err != nil {
		return err
	}
	if !sameSocketFile(current, pinned) {
		return syscall.EADDRINUSE
	}
	return unix.Unlinkat(int(directory.Fd()), name, 0)
}

func (server *Server) serve() {
	defer server.wait.Done()
	for {
		connection, err := server.listener.AcceptUnix()
		if err != nil {
			select {
			case <-server.done:
				return
			default:
				server.report(fmt.Errorf("accept Codex report connection: %w", err))
				continue
			}
		}
		select {
		case server.workers <- struct{}{}:
		case <-server.done:
			_ = connection.Close()
			return
		}
		server.wait.Add(1)
		go func() {
			defer server.wait.Done()
			defer func() { <-server.workers }()
			server.handle(connection)
		}()
	}
}

func (server *Server) handle(connection *net.UnixConn) {
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(reportExchangeTimeout)); err != nil {
		server.reject(connection, fmt.Errorf("bound Codex report connection: %w", err))
		return
	}
	credentials, err := requireCurrentUser(connection)
	if err != nil {
		server.reject(connection, err)
		return
	}
	reader := bufio.NewReader(io.LimitReader(connection, maxProtocolMessage+1))
	data, err := reader.ReadBytes('\n')
	if err != nil {
		server.reject(connection, fmt.Errorf("read Codex session report: %w", err))
		return
	}
	if len(data) > maxProtocolMessage {
		server.reject(connection, fmt.Errorf("codex session report exceeds %d bytes", maxProtocolMessage))
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var report Report
	if err := decoder.Decode(&report); err != nil {
		server.reject(connection, fmt.Errorf("decode Codex session report: %w", err))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		server.reject(connection, errors.New("codex session report contains trailing data"))
		return
	}
	if err := report.Validate(); err != nil {
		server.reject(connection, err)
		return
	}
	report.PeerPID = int(credentials.Pid)
	ctx, cancel := context.WithTimeout(context.Background(), reportExchangeTimeout)
	defer cancel()
	if err := server.handler(ctx, report); err != nil {
		server.reject(connection, err)
		return
	}
	server.writeResponse(connection, response{Version: ProtocolVersion, OK: true})
}

func requireCurrentUser(connection *net.UnixConn) (*unix.Ucred, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("inspect Codex report peer: %w", err)
	}
	var credentials *unix.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, fmt.Errorf("inspect Codex report peer: %w", err)
	}
	if socketErr != nil {
		return nil, fmt.Errorf("inspect Codex report peer credentials: %w", socketErr)
	}
	if credentials == nil || credentials.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("codex report peer is not the current user")
	}
	return credentials, nil
}

func (server *Server) reject(connection net.Conn, cause error) {
	server.report(cause)
	server.writeResponse(connection, response{Version: ProtocolVersion, Error: "report rejected"})
}

func (server *Server) writeResponse(connection net.Conn, result response) {
	encoded, err := json.Marshal(result)
	if err != nil {
		server.report(fmt.Errorf("encode Codex report response: %w", err))
		return
	}
	encoded = append(encoded, '\n')
	if _, err := connection.Write(encoded); err != nil {
		server.report(fmt.Errorf("write Codex report response: %w", err))
	}
}

func (server *Server) report(err error) {
	if err != nil && server.reportError != nil {
		server.reportError(err)
	}
}

func (server *Server) Close() error {
	if server == nil {
		return nil
	}
	var closeErr error
	server.closeOnce.Do(func() {
		close(server.done)
		closeErr = server.listener.Close()
		server.wait.Wait()
		if err := unlinkPinnedSocket(server.directory, server.socketName, server.socketStat); err != nil && !errors.Is(err, syscall.EADDRINUSE) {
			closeErr = errors.Join(closeErr, err)
		}
		closeErr = errors.Join(closeErr, unix.Close(server.socketFD))
		closeErr = errors.Join(closeErr, server.directory.Close())
	})
	return closeErr
}
