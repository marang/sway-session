package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"
)

const (
	maxHerdrConfigSize          = 1024 * 1024
	maxHerdrOutputSize          = 1024 * 1024
	herdrCommandTimeout         = 20 * time.Second
	maxHerdrUnixSocketPathBytes = len(unix.RawSockaddrUnix{}.Path) - 1
	herdrAPISocketFilename      = "herdr.sock"
	herdrClientSocketFilename   = "herdr-client.sock"
)

type HerdrPaths struct {
	Root       string
	ConfigFile string
}

// ValidateHerdrSessionSocketPaths checks both filesystem-backed Unix sockets
// which Herdr derives for a named session. Linux reserves one byte in
// sockaddr_un.sun_path for the terminating NUL.
func ValidateHerdrSessionSocketPaths(rootPath string, sessionName string) error {
	if !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) != rootPath {
		return errors.New("herdr state root must be a clean absolute path")
	}
	if !validSessionName(sessionName) || sessionName == "default" {
		return fmt.Errorf("refuse unsafe Herdr session name %q", sessionName)
	}
	sessionRoot := filepath.Join(rootPath, "sessions", sessionName)
	for _, filename := range []string{herdrAPISocketFilename, herdrClientSocketFilename} {
		path := filepath.Join(sessionRoot, filename)
		if len(path) > maxHerdrUnixSocketPathBytes {
			return fmt.Errorf(
				"herdr %s path requires %d bytes; Linux pathname Unix sockets allow at most %d; shorten XDG_CONFIG_HOME",
				filename, len(path), maxHerdrUnixSocketPathBytes,
			)
		}
	}
	return nil
}

// DefaultHerdrPaths follows Herdr's XDG configuration contract. A config-file
// override does not move Herdr's named-session data root.
func DefaultHerdrPaths() (HerdrPaths, error) {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return HerdrPaths{}, fmt.Errorf("resolve home directory: %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(configHome) {
		return HerdrPaths{}, errors.New("XDG_CONFIG_HOME must be an absolute path")
	}
	root := filepath.Join(filepath.Clean(configHome), "herdr")
	configFile := os.Getenv("HERDR_CONFIG_PATH")
	if configFile == "" {
		configFile = filepath.Join(root, "config.toml")
	} else if !filepath.IsAbs(configFile) {
		return HerdrPaths{}, errors.New("HERDR_CONFIG_PATH must be an absolute path")
	} else {
		configFile = filepath.Clean(configFile)
	}
	return HerdrPaths{Root: root, ConfigFile: configFile}, nil
}

// ValidateHerdrPaneHistory verifies that Herdr state is owner-only and that
// pane history is explicitly enabled. It never rewrites an existing config.
func ValidateHerdrPaneHistory(paths HerdrPaths) error {
	if !filepath.IsAbs(paths.Root) || filepath.Clean(paths.Root) != paths.Root {
		return errors.New("herdr state root must be a clean absolute path")
	}
	if !filepath.IsAbs(paths.ConfigFile) || filepath.Clean(paths.ConfigFile) != paths.ConfigFile {
		return errors.New("herdr config path must be a clean absolute path")
	}
	if err := validatePrivatePathAncestors(filepath.Dir(paths.ConfigFile)); err != nil {
		return fmt.Errorf("validate Herdr config ancestors: %w", err)
	}
	if err := ValidateHerdrStateRoot(paths.Root); err != nil {
		return err
	}
	uid := uint32(os.Getuid())
	config, err := openNoSymlinks(paths.ConfigFile, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("open Herdr config %s: %w", paths.ConfigFile, err)
	}
	var configStat unix.Stat_t
	if err := unix.Fstat(config, &configStat); err != nil {
		_ = unix.Close(config)
		return fmt.Errorf("inspect Herdr config: %w", err)
	}
	if configStat.Mode&unix.S_IFMT != unix.S_IFREG || configStat.Uid != uid || configStat.Mode&0o077 != 0 || configStat.Nlink != 1 {
		_ = unix.Close(config)
		return fmt.Errorf("herdr config %s must be a single-link owner-only regular file owned by uid %d", paths.ConfigFile, uid)
	}
	if configStat.Size < 0 || configStat.Size > maxHerdrConfigSize {
		_ = unix.Close(config)
		return fmt.Errorf("herdr config exceeds %d bytes", maxHerdrConfigSize)
	}
	file := os.NewFile(uintptr(config), paths.ConfigFile)
	if file == nil {
		_ = unix.Close(config)
		return errors.New("create Herdr config file handle")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxHerdrConfigSize+1))
	if err != nil {
		return fmt.Errorf("read Herdr config: %w", err)
	}
	if len(data) > maxHerdrConfigSize {
		return fmt.Errorf("herdr config exceeds %d bytes", maxHerdrConfigSize)
	}
	var configValue struct {
		Experimental struct {
			PaneHistory bool `toml:"pane_history"`
		} `toml:"experimental"`
	}
	if err := toml.Unmarshal(data, &configValue); err != nil {
		return fmt.Errorf("decode Herdr config: %w", err)
	}
	if !configValue.Experimental.PaneHistory {
		return errors.New("herdr pane history is disabled; set [experimental] pane_history = true")
	}
	return nil
}

// ValidateHerdrStateRoot checks the deletion boundary without requiring a
// currently usable config file, so purge can recover misconfigured sessions.
func ValidateHerdrStateRoot(rootPath string) error {
	if !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) != rootPath {
		return errors.New("herdr state root must be a clean absolute path")
	}
	if err := validatePrivatePathAncestors(filepath.Dir(rootPath)); err != nil {
		return fmt.Errorf("validate Herdr state ancestors: %w", err)
	}
	uid := uint32(os.Getuid())
	root, err := openNoSymlinks(rootPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open Herdr state root %s: %w", rootPath, err)
	}
	defer unix.Close(root)
	var rootStat unix.Stat_t
	if err := unix.Fstat(root, &rootStat); err != nil {
		return fmt.Errorf("inspect Herdr state root: %w", err)
	}
	if rootStat.Mode&unix.S_IFMT != unix.S_IFDIR || rootStat.Uid != uid || rootStat.Mode&0o077 != 0 {
		return fmt.Errorf("herdr state root %s must be an owner-only directory owned by uid %d", rootPath, uid)
	}
	return nil
}

// HerdrStateRootExists distinguishes a safely absent, never-initialized Herdr
// root from an existing root which must pass the full ownership checks.
func HerdrStateRootExists(rootPath string) (bool, error) {
	err := ValidateHerdrStateRoot(rootPath)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func validatePrivatePathAncestors(path string) error {
	uid := uint32(os.Getuid())
	current := string(filepath.Separator)
	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	for _, component := range components {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", current, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is not a trusted directory", current)
		}
		if stat.Uid != 0 && stat.Uid != uid {
			return fmt.Errorf("%s is owned by untrusted uid %d", current, stat.Uid)
		}
		if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return fmt.Errorf("%s is group- or world-writable without the sticky bit", current)
		}
	}
	return nil
}

func openNoSymlinks(path string, flags int, mode uint32) (int, error) {
	return unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags:   uint64(flags),
		Mode:    uint64(mode),
		Resolve: unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS,
	})
}

type HerdrCommandRunner interface {
	CombinedOutput(context.Context, string, ...string) ([]byte, error)
}

type HerdrManager struct {
	Executable string
	Root       string
	Runner     HerdrCommandRunner
	Timeout    time.Duration
}

// HerdrNamedSessionExists checks the exact descriptor-relative named-session
// directory without following symlinks or crossing a nested mount.
func HerdrNamedSessionExists(root string, sessionName string) (bool, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return false, errors.New("herdr state root must be a clean absolute path")
	}
	if !validSessionName(sessionName) || sessionName == "default" {
		return false, fmt.Errorf("refuse unsafe Herdr session name %q", sessionName)
	}
	return (HerdrManager{Root: root}).sessionPathExists(sessionName)
}

type herdrSessionList struct {
	Sessions []herdrSessionInfo `json:"sessions"`
}

type herdrSessionInfo struct {
	Name       string `json:"name"`
	Default    bool   `json:"default"`
	Running    bool   `json:"running"`
	SocketPath string `json:"socket_path"`
	SessionDir string `json:"session_dir"`
}

// DeleteSession stops and deletes exactly one named Herdr session. Every
// destructive command is bracketed by fresh structured discovery.
func (manager HerdrManager) DeleteSession(ctx context.Context, sessionName string) error {
	if manager.Runner == nil {
		return errors.New("herdr command runner is nil")
	}
	if manager.Executable == "" || !filepath.IsAbs(manager.Executable) {
		return errors.New("herdr executable must be an absolute path")
	}
	if !filepath.IsAbs(manager.Root) || filepath.Clean(manager.Root) != manager.Root {
		return errors.New("herdr state root must be a clean absolute path")
	}
	if !validSessionName(sessionName) || sessionName == "default" {
		return fmt.Errorf("refuse unsafe Herdr session name %q", sessionName)
	}
	info, exists, err := manager.findSession(ctx, sessionName)
	if err != nil || !exists {
		return err
	}
	if info.Running {
		if err := manager.run(ctx, "session", "stop", sessionName, "--json"); err != nil {
			return err
		}
	}
	info, exists, err = manager.findSession(ctx, sessionName)
	if err != nil || !exists {
		return err
	}
	if info.Running {
		return fmt.Errorf("herdr session %q is still running after stop", sessionName)
	}
	if err := manager.run(ctx, "session", "delete", sessionName, "--json"); err != nil {
		return err
	}
	_, exists, err = manager.findSession(ctx, sessionName)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("herdr session %q still exists after delete", sessionName)
	}
	return nil
}

func (manager HerdrManager) findSession(ctx context.Context, name string) (herdrSessionInfo, bool, error) {
	output, err := manager.output(ctx, "session", "list", "--json")
	if err != nil {
		return herdrSessionInfo{}, false, err
	}
	var response herdrSessionList
	decoder := json.NewDecoder(bytes.NewReader(output))
	if err := decoder.Decode(&response); err != nil {
		return herdrSessionInfo{}, false, fmt.Errorf("decode Herdr session list: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return herdrSessionInfo{}, false, errors.New("herdr session list contains multiple JSON values")
		}
		return herdrSessionInfo{}, false, fmt.Errorf("decode trailing Herdr session data: %w", err)
	}
	if response.Sessions == nil {
		return herdrSessionInfo{}, false, errors.New("herdr session list has no sessions array")
	}
	found := false
	var result herdrSessionInfo
	for _, info := range response.Sessions {
		if info.Name != name {
			continue
		}
		if found {
			return herdrSessionInfo{}, false, fmt.Errorf("herdr returned duplicate session %q", name)
		}
		if err := manager.validateSessionInfo(info, name); err != nil {
			return herdrSessionInfo{}, false, err
		}
		found = true
		result = info
	}
	if !found {
		exists, err := manager.sessionPathExists(name)
		if err != nil {
			return herdrSessionInfo{}, false, err
		}
		if exists {
			return herdrSessionInfo{}, false, fmt.Errorf("herdr omitted existing session path %s from its session list", filepath.Join(manager.Root, "sessions", name))
		}
	}
	return result, found, nil
}

func (manager HerdrManager) sessionPathExists(name string) (bool, error) {
	root, err := openNoSymlinks(manager.Root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, fmt.Errorf("open Herdr state root for session inspection: %w", err)
	}
	defer unix.Close(root)
	relative := filepath.Join("sessions", name)
	descriptor, err := unix.Openat2(root, relative, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	})
	if err == nil {
		var stat unix.Stat_t
		if statErr := unix.Fstat(descriptor, &stat); statErr != nil {
			_ = unix.Close(descriptor)
			return false, fmt.Errorf("inspect expected Herdr session path: %w", statErr)
		}
		_ = unix.Close(descriptor)
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Getuid()) {
			return false, fmt.Errorf("expected Herdr session path %s is not a directory owned by uid %d", filepath.Join(manager.Root, relative), os.Getuid())
		}
		return true, nil
	}
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	return false, fmt.Errorf("inspect expected Herdr session path %s: %w", filepath.Join(manager.Root, relative), err)
}

func (manager HerdrManager) validateSessionInfo(info herdrSessionInfo, name string) error {
	expectedDir := filepath.Join(manager.Root, "sessions", name)
	expectedSocket := filepath.Join(expectedDir, "herdr.sock")
	if info.Default || info.SessionDir != expectedDir || info.SocketPath != expectedSocket {
		return fmt.Errorf("herdr session %q resolved outside its expected named-session path", name)
	}
	exists, err := manager.sessionPathExists(name)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("herdr listed session %q without its expected session directory", name)
	}
	return nil
}

func (manager HerdrManager) output(ctx context.Context, arguments ...string) ([]byte, error) {
	timeout := manager.Timeout
	if timeout <= 0 {
		timeout = herdrCommandTimeout
	}
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := manager.Runner.CombinedOutput(commandContext, manager.Executable, arguments...)
	if len(output) > maxHerdrOutputSize {
		return nil, fmt.Errorf("herdr output exceeds %d bytes", maxHerdrOutputSize)
	}
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return nil, fmt.Errorf("herdr %s: %w", strings.Join(arguments, " "), err)
		}
		return nil, fmt.Errorf("herdr %s: %w: %s", strings.Join(arguments, " "), err, message)
	}
	return output, nil
}

func (manager HerdrManager) run(ctx context.Context, arguments ...string) error {
	_, err := manager.output(ctx, arguments...)
	return err
}

// ExecCommandRunner is the production Herdr subprocess boundary.
type ExecCommandRunner struct{}

func (ExecCommandRunner) CombinedOutput(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	output := &boundedCombinedOutput{limit: maxHerdrOutputSize + 1}
	command.Stdout = output
	command.Stderr = output
	err := command.Run()
	return output.Bytes(), err
}

type boundedCombinedOutput struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (output *boundedCombinedOutput) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	remaining := output.limit - len(output.data)
	if remaining > len(data) {
		remaining = len(data)
	}
	if remaining > 0 {
		output.data = append(output.data, data[:remaining]...)
	}
	return len(data), nil
}

func (output *boundedCombinedOutput) Bytes() []byte {
	output.mu.Lock()
	defer output.mu.Unlock()
	return append([]byte(nil), output.data...)
}
