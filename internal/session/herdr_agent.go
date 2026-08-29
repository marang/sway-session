package session

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
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	herdrAgentSocketName = "herdr.sock"
	maxHerdrAPIResponse  = 64 * 1024
	maxProcessAncestors  = 128
)

type herdrAPIEndpoint struct {
	directoryFD int
	socketFD    int
	socketStat  unix.Stat_t
}

// ReportHerdrCodexSession sends the one mutating Herdr operation exposed by
// the Codex broker. A fixed read-only process query first proves that the Unix
// peer which supplied the report actually descends from the selected pane.
func ReportHerdrCodexSession(ctx context.Context, paths HerdrPaths, launcher Launcher, paneID string, codexSessionID string, reporterPID int, now time.Time) error {
	if launcher.Kind != LauncherHerdr || !validSessionName(launcher.Session) || launcher.Session == "default" {
		return errors.New("invalid Herdr launcher")
	}
	if err := validateBoundedIdentity("Herdr pane ID", paneID, 256); err != nil {
		return err
	}
	if err := validateBoundedIdentity("Codex session ID", codexSessionID, 512); err != nil {
		return err
	}
	if reporterPID <= 0 {
		return errors.New("codex reporter PID must be positive")
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
	}
	endpoint, err := openHerdrAPIEndpoint(paths.Root, launcher.Session)
	if err != nil {
		return err
	}
	defer endpoint.Close()

	processRequestID := fmt.Sprintf("sway-session:codex:process:%d", reporterPID)
	processResult, err := endpoint.request(ctx, processRequestID, "pane.process_info", struct {
		PaneID string `json:"pane_id"`
	}{PaneID: paneID})
	if err != nil {
		return fmt.Errorf("verify Codex reporter pane: %w", err)
	}
	var processResponse struct {
		Type        string `json:"type"`
		ProcessInfo struct {
			PaneID   string `json:"pane_id"`
			ShellPID *int   `json:"shell_pid"`
		} `json:"process_info"`
	}
	if err := json.Unmarshal(processResult, &processResponse); err != nil {
		return fmt.Errorf("decode Herdr pane process information: %w", err)
	}
	if processResponse.Type != "pane_process_info" || processResponse.ProcessInfo.PaneID != paneID || processResponse.ProcessInfo.ShellPID == nil || *processResponse.ProcessInfo.ShellPID <= 0 {
		return errors.New("herdr did not return the selected pane's shell process")
	}
	belongs, err := processDescendsFrom(reporterPID, *processResponse.ProcessInfo.ShellPID)
	if err != nil {
		return fmt.Errorf("verify Codex reporter process ancestry: %w", err)
	}
	if !belongs {
		return errors.New("codex reporter does not belong to the selected Herdr pane")
	}

	sequence := now.UnixNano()
	if sequence < 0 {
		sequence = 0
	}
	requestID := fmt.Sprintf("sway-session:codex:report:%d", sequence)
	params := struct {
		PaneID         string `json:"pane_id"`
		Source         string `json:"source"`
		Agent          string `json:"agent"`
		Sequence       uint64 `json:"seq"`
		AgentSessionID string `json:"agent_session_id"`
	}{
		PaneID: paneID, Source: "herdr:codex", Agent: "codex",
		Sequence: uint64(sequence), AgentSessionID: codexSessionID,
	}
	result, err := endpoint.request(ctx, requestID, "pane.report_agent_session", params)
	if err != nil {
		return fmt.Errorf("record Codex session association: %w", err)
	}
	var reportResponse struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(result, &reportResponse); err != nil || reportResponse.Type != "ok" {
		return errors.New("herdr session association response was not an ok result")
	}
	return nil
}

func openHerdrAPIEndpoint(rootPath string, sessionName string) (*herdrAPIEndpoint, error) {
	if !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) != rootPath {
		return nil, errors.New("herdr state root must be a clean absolute path")
	}
	if err := validatePrivatePathAncestors(filepath.Dir(rootPath)); err != nil {
		return nil, fmt.Errorf("validate Herdr state ancestors: %w", err)
	}
	rootFD, err := openNoSymlinks(rootPath, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open Herdr state root: %w", err)
	}
	var rootStat unix.Stat_t
	if err := unix.Fstat(rootFD, &rootStat); err != nil {
		_ = unix.Close(rootFD)
		return nil, fmt.Errorf("inspect Herdr state root: %w", err)
	}
	if rootStat.Mode&unix.S_IFMT != unix.S_IFDIR || rootStat.Uid != uint32(os.Geteuid()) || rootStat.Mode&0o077 != 0 {
		_ = unix.Close(rootFD)
		return nil, errors.New("herdr state root must be an owner-only directory owned by the current user")
	}
	relative := filepath.Join("sessions", sessionName)
	directoryFD, err := unix.Openat2(rootFD, relative, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	})
	_ = unix.Close(rootFD)
	if err != nil {
		return nil, fmt.Errorf("open Herdr named-session directory: %w", err)
	}
	var directoryStat unix.Stat_t
	if err := unix.Fstat(directoryFD, &directoryStat); err != nil {
		_ = unix.Close(directoryFD)
		return nil, fmt.Errorf("inspect Herdr named-session directory: %w", err)
	}
	if directoryStat.Mode&unix.S_IFMT != unix.S_IFDIR || directoryStat.Uid != uint32(os.Geteuid()) {
		_ = unix.Close(directoryFD)
		return nil, errors.New("herdr named-session path must be a directory owned by the current user")
	}
	// Pin and later dial the exact socket inode through procfs. A path-based
	// reconnect could otherwise reach a same-UID replacement between checks.
	socketFD, err := unix.Openat(directoryFD, herdrAgentSocketName, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		_ = unix.Close(directoryFD)
		return nil, fmt.Errorf("open Herdr API socket: %w", err)
	}
	var socketStat unix.Stat_t
	if err := unix.Fstat(socketFD, &socketStat); err != nil {
		_ = unix.Close(socketFD)
		_ = unix.Close(directoryFD)
		return nil, fmt.Errorf("inspect Herdr API socket: %w", err)
	}
	if err := validateHerdrSocket(socketStat); err != nil {
		_ = unix.Close(socketFD)
		_ = unix.Close(directoryFD)
		return nil, err
	}
	return &herdrAPIEndpoint{directoryFD: directoryFD, socketFD: socketFD, socketStat: socketStat}, nil
}

func validateHerdrSocket(stat unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o777 != 0o600 {
		return errors.New("herdr API endpoint must be an owner-only socket owned by the current user")
	}
	return nil
}

func (endpoint *herdrAPIEndpoint) Close() {
	if endpoint == nil {
		return
	}
	if endpoint.socketFD >= 0 {
		_ = unix.Close(endpoint.socketFD)
		endpoint.socketFD = -1
	}
	if endpoint.directoryFD >= 0 {
		_ = unix.Close(endpoint.directoryFD)
		endpoint.directoryFD = -1
	}
}

func (endpoint *herdrAPIEndpoint) request(ctx context.Context, requestID string, method string, params any) (json.RawMessage, error) {
	request := struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{ID: requestID, Method: method, Params: params}
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode Herdr request: %w", err)
	}
	encoded = append(encoded, '\n')
	socketPath := fmt.Sprintf("/proc/self/fd/%d", endpoint.socketFD)
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to Herdr reporting endpoint: %w", err)
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("bound Herdr reporting exchange: %w", err)
		}
	}
	var currentSocket unix.Stat_t
	if err := unix.Fstatat(endpoint.directoryFD, herdrAgentSocketName, &currentSocket, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, fmt.Errorf("reinspect Herdr API socket: %w", err)
	}
	if currentSocket.Dev != endpoint.socketStat.Dev || currentSocket.Ino != endpoint.socketStat.Ino {
		return nil, errors.New("herdr API socket changed during connection")
	}
	if err := validateHerdrSocket(currentSocket); err != nil {
		return nil, err
	}
	if _, err := connection.Write(encoded); err != nil {
		return nil, fmt.Errorf("write Herdr request: %w", err)
	}
	reader := bufio.NewReader(io.LimitReader(connection, maxHerdrAPIResponse+1))
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read Herdr response: %w", err)
	}
	if len(line) > maxHerdrAPIResponse {
		return nil, fmt.Errorf("herdr response exceeds %d bytes", maxHerdrAPIResponse)
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var response struct {
		ID     string           `json:"id"`
		Result *json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("decode Herdr response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("herdr response contains trailing data")
	}
	if response.ID != requestID {
		return nil, errors.New("herdr response ID did not match request")
	}
	if response.Error != nil {
		message := strings.TrimSpace(response.Error.Message)
		if message == "" {
			message = "herdr rejected the request"
		}
		return nil, errors.New(message)
	}
	if response.Result == nil {
		return nil, errors.New("herdr response omitted its result")
	}
	return *response.Result, nil
}

func processDescendsFrom(pid int, ancestor int) (bool, error) {
	if pid <= 0 || ancestor <= 0 {
		return false, errors.New("process IDs must be positive")
	}
	seen := make(map[int]struct{}, maxProcessAncestors)
	current := pid
	for range maxProcessAncestors {
		if current == ancestor {
			return true, nil
		}
		if current <= 1 {
			return false, nil
		}
		if _, exists := seen[current]; exists {
			return false, errors.New("process ancestry contains a cycle")
		}
		seen[current] = struct{}{}
		parent, err := readParentPID(current)
		if err != nil {
			return false, err
		}
		current = parent
	}
	return false, fmt.Errorf("process ancestry exceeds %d generations", maxProcessAncestors)
}

func readParentPID(pid int) (int, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, fmt.Errorf("read process %d status: %w", pid, err)
	}
	closing := bytes.LastIndexByte(data, ')')
	if closing < 0 || closing+2 >= len(data) {
		return 0, fmt.Errorf("process %d status is malformed", pid)
	}
	fields := bytes.Fields(data[closing+2:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("process %d status omits its parent", pid)
	}
	parent, err := strconv.Atoi(string(fields[1]))
	if err != nil || parent < 0 {
		return 0, fmt.Errorf("process %d has invalid parent metadata", pid)
	}
	return parent, nil
}

func validateBoundedIdentity(name string, value string, maximum int) error {
	if value == "" || len(value) > maximum || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must contain between 1 and %d bytes without surrounding whitespace", name, maximum)
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("%s must not contain control characters", name)
		}
	}
	return nil
}
