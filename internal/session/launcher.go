package session

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/marang/sway-title-animator/internal/swayipc"
)

type ProcessStarter interface {
	Start(ProcessSpec) error
}

type ExecProcessStarter struct{}

type ProcessSpec struct {
	Name        string
	Arguments   []string
	Environment []string
}

func (ExecProcessStarter) Start(spec ProcessSpec) error {
	command := exec.Command(spec.Name, spec.Arguments...)
	if len(spec.Environment) != 0 {
		command.Env = append(os.Environ(), spec.Environment...)
	}
	if err := command.Start(); err != nil {
		return err
	}
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("release launched process: %w", err)
	}
	return nil
}

type AlacrittyHerdrLauncher struct {
	Alacritty string
	Herdr     string
	Starter   ProcessStarter
}

func AlacrittyHerdrArguments(context Context, herdrExecutable string) ([]string, error) {
	if err := context.Validate(); err != nil {
		return nil, fmt.Errorf("validate context: %w", err)
	}
	if context.Launcher.Kind != LauncherHerdr {
		return nil, fmt.Errorf("herdr launcher does not support context launcher kind %q", context.Launcher.Kind)
	}
	if !filepath.IsAbs(herdrExecutable) {
		return nil, errors.New("herdr executable must be an absolute path")
	}
	appID, err := context.ID.AppID()
	if err != nil {
		return nil, err
	}
	title := context.Label
	if title == "" {
		title = context.Launcher.Session
	}
	return []string{
		"--class=" + appID,
		"--working-directory=" + context.Launcher.Cwd,
		"--title=" + title,
		"-e", herdrExecutable, "--session", context.Launcher.Session,
	}, nil
}

// Launch creates one terminal with a stable Wayland app ID and a typed Herdr
// session attachment. No registry value is interpreted by a shell.
func (launcher AlacrittyHerdrLauncher) Launch(context Context) error {
	if launcher.Starter == nil {
		return errors.New("process starter is nil")
	}
	if !filepath.IsAbs(launcher.Alacritty) || !filepath.IsAbs(launcher.Herdr) {
		return errors.New("launcher executables must be absolute paths")
	}
	arguments, err := AlacrittyHerdrArguments(context, launcher.Herdr)
	if err != nil {
		return err
	}
	info, err := os.Stat(context.Launcher.Cwd)
	if err != nil {
		return fmt.Errorf("inspect project directory %s: %w", context.Launcher.Cwd, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("project path %s is not a directory", context.Launcher.Cwd)
	}
	return launcher.Starter.Start(ProcessSpec{
		Name:        launcher.Alacritty,
		Arguments:   arguments,
		Environment: []string{"SWAY_SESSION_CONTEXT_ID=" + string(context.ID)},
	})
}

// FindPendingAlacrittyLaunches recognizes not-yet-mapped launches by their
// complete typed argv. This closes the idempotency gap after a successful
// process start but before its Wayland window appears in GET_TREE.
func FindPendingAlacrittyLaunches(procRoot string, context Context, alacritty string, herdr string) ([]int, error) {
	if !filepath.IsAbs(procRoot) || !filepath.IsAbs(alacritty) || !filepath.IsAbs(herdr) {
		return nil, errors.New("process root and executables must be absolute paths")
	}
	arguments, err := AlacrittyHerdrArguments(context, herdr)
	if err != nil {
		return nil, err
	}
	want := append([]string{alacritty}, arguments...)
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("read process table: %w", err)
	}
	matches := make([]int, 0, 1)
	for _, entry := range entries {
		pid, err := parsePID(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		path := filepath.Join(procRoot, entry.Name(), "cmdline")
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) {
			_ = file.Close()
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(file, 64*1024+1))
		_ = file.Close()
		if readErr != nil || len(data) > 64*1024 {
			continue
		}
		if equalCommandLine(data, want) {
			matches = append(matches, pid)
		}
	}
	return matches, nil
}

func parsePID(value string) (int, error) {
	pid := 0
	if value == "" {
		return 0, errors.New("empty PID")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, errors.New("non-numeric PID")
		}
		pid = pid*10 + int(character-'0')
		if pid > 1<<30 {
			return 0, errors.New("PID is too large")
		}
	}
	if pid <= 0 {
		return 0, errors.New("PID must be positive")
	}
	return pid, nil
}

func equalCommandLine(data []byte, want []string) bool {
	data = bytes.TrimSuffix(data, []byte{0})
	parts := bytes.Split(data, []byte{0})
	if len(parts) != len(want) {
		return false
	}
	for index := range want {
		if string(parts[index]) != want[index] {
			return false
		}
	}
	return true
}

// ResolveTrustedExecutable resolves a fixed program name and rejects an
// executable or ancestor which another Unix user could replace.
func ResolveTrustedExecutable(name string) (string, error) {
	if name == "" || filepath.Base(name) != name {
		return "", errors.New("executable must be one fixed base name")
	}
	found, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("find %s in PATH: %w", name, err)
	}
	absolute, err := filepath.Abs(found)
	if err != nil {
		return "", fmt.Errorf("resolve %s path: %w", name, err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve %s symlinks: %w", name, err)
	}
	uid := uint32(os.Getuid())
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(resolved, string(filepath.Separator)), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("inspect executable path %s: %w", current, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return "", fmt.Errorf("inspect owner of executable path %s", current)
		}
		if stat.Uid != 0 && stat.Uid != uid {
			return "", fmt.Errorf("executable path %s is owned by untrusted uid %d", current, stat.Uid)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return "", fmt.Errorf("executable path %s is group- or world-writable", current)
		}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("executable %s is not an executable regular file", resolved)
	}
	return resolved, nil
}

// ResolveRootOwnedSystemExecutable resolves a fixed program only from system
// bin directories and requires every resolved path component to be root-owned
// and not group- or world-writable. Security brokers use this stricter variant
// so a confined process cannot prepare a user-owned binary for a later
// unconfined launch.
func ResolveRootOwnedSystemExecutable(name string) (string, error) {
	if name == "" || filepath.Base(name) != name {
		return "", errors.New("system executable must be one fixed base name")
	}
	for _, candidate := range []string{filepath.Join("/usr/bin", name), filepath.Join("/usr/local/bin", name)} {
		resolved, err := filepath.EvalSymlinks(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", candidate, err)
		}
		current := string(filepath.Separator)
		for _, component := range strings.Split(strings.TrimPrefix(resolved, string(filepath.Separator)), string(filepath.Separator)) {
			if component == "" {
				continue
			}
			current = filepath.Join(current, component)
			info, err := os.Lstat(current)
			if err != nil {
				return "", fmt.Errorf("inspect system executable path %s: %w", current, err)
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
				return "", fmt.Errorf("system executable path %s must be root-owned and not group- or world-writable", current)
			}
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("system executable %s is not an executable regular file", resolved)
		}
		return resolved, nil
	}
	return "", fmt.Errorf("find root-owned %s in /usr/bin or /usr/local/bin", name)
}

type ManagedWindow struct {
	ContainerID int64
	Workspace   string
}

type ManagedWindowIssue struct {
	ContextID ContextID
	Cause     error
}

func (issue ManagedWindowIssue) Error() string {
	return fmt.Sprintf("context %q: %v", issue.ContextID, issue.Cause)
}

func (issue ManagedWindowIssue) Unwrap() error {
	return issue.Cause
}

// ObserveManagedWindows scans the complete tree, including scratchpad and the
// restore staging workspace, and rejects duplicate or conflicting identities.
func ObserveManagedWindows(root *swayipc.TreeNode, registry Registry) (map[ContextID]ManagedWindow, error) {
	observation, err := observeRestoreTree(root, registry)
	if err != nil {
		return nil, err
	}
	result := make(map[ContextID]ManagedWindow, len(observation.contexts))
	for id, node := range observation.contexts {
		result[id] = ManagedWindow{ContainerID: node.ID, Workspace: observation.workspaceName(node)}
	}
	return result, nil
}

// ObserveManagedWindowsIsolated returns usable observations even when a
// different registered context is duplicated or structurally invalid. Global
// tree corruption and malformed reserved identities still fail the operation.
func ObserveManagedWindowsIsolated(root *swayipc.TreeNode, registry Registry) (map[ContextID]ManagedWindow, []ManagedWindowIssue, error) {
	if root == nil {
		return nil, nil, errors.New("sway tree is nil")
	}
	if err := registry.Validate(); err != nil {
		return nil, nil, fmt.Errorf("validate context registry: %w", err)
	}
	registered := registeredContextIDs(registry)
	windows := make(map[ContextID]ManagedWindow)
	issueByID := make(map[ContextID]error)
	var walk func(*swayipc.TreeNode, string) error
	walk = func(node *swayipc.TreeNode, workspace string) error {
		if node == nil {
			return errors.New("sway tree contains a nil node")
		}
		if node.Type == "workspace" {
			workspace = node.Name
		}
		identities, err := managedIdentities(node)
		if err != nil {
			return err
		}
		if len(identities) > 1 {
			for _, id := range identities {
				if _, exists := registered[id]; exists {
					issueByID[id] = errors.New("container has conflicting managed identities")
					delete(windows, id)
				}
			}
		} else if len(identities) == 1 {
			id := identities[0]
			if _, exists := registered[id]; exists {
				var issue error
				switch {
				case len(node.Nodes) != 0 || len(node.FloatingNodes) != 0:
					issue = errors.New("managed identity is attached to a layout parent")
				case workspace == "":
					issue = errors.New("managed identity is outside a workspace")
				case node.ID <= 0:
					issue = errors.New("managed identity has an invalid container ID")
				case issueByID[id] != nil:
				case windows[id].ContainerID != 0:
					issue = fmt.Errorf("appears in containers %d and %d", windows[id].ContainerID, node.ID)
				default:
					windows[id] = ManagedWindow{ContainerID: node.ID, Workspace: workspace}
				}
				if issue != nil {
					issueByID[id] = issue
					delete(windows, id)
				}
			}
		}
		for _, child := range node.Nodes {
			if err := walk(child, workspace); err != nil {
				return err
			}
		}
		for _, child := range node.FloatingNodes {
			if err := walk(child, workspace); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return nil, nil, err
	}
	ids := make([]ContextID, 0, len(issueByID))
	for id := range issueByID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left int, right int) bool { return ids[left] < ids[right] })
	issues := make([]ManagedWindowIssue, 0, len(ids))
	for _, id := range ids {
		issues = append(issues, ManagedWindowIssue{ContextID: id, Cause: issueByID[id]})
	}
	return windows, issues, nil
}

func managedIdentities(node *swayipc.TreeNode) ([]ContextID, error) {
	identities := make(map[ContextID]struct{})
	for _, mark := range node.Marks {
		if !strings.HasPrefix(mark, MarkPrefix) {
			continue
		}
		id, err := ParseMark(mark)
		if err != nil {
			return nil, fmt.Errorf("invalid managed mark %q: %w", mark, err)
		}
		identities[id] = struct{}{}
	}
	if node.AppID != nil && strings.HasPrefix(*node.AppID, AppIDPrefix) {
		id, err := ParseAppID(*node.AppID)
		if err != nil {
			return nil, fmt.Errorf("invalid managed application ID %q: %w", *node.AppID, err)
		}
		identities[id] = struct{}{}
	}
	result := make([]ContextID, 0, len(identities))
	for id := range identities {
		result = append(result, id)
	}
	sort.Slice(result, func(left int, right int) bool { return result[left] < result[right] })
	return result, nil
}
