package session

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
)

// TerminalAdapterExecutableName maps a closed adapter kind to the one program
// name which may be resolved. Configuration never supplies an executable path
// or command template.
func TerminalAdapterExecutableName(adapter TerminalAdapter) (string, error) {
	switch adapter {
	case TerminalAdapterAlacritty:
		return "alacritty", nil
	case TerminalAdapterFoot:
		return "foot", nil
	default:
		return "", fmt.Errorf("unsupported terminal adapter %q", adapter)
	}
}

// BuildTerminalProcessSpec constructs the complete process boundary for one
// persisted Herdr context. Every adapter shape is compiled in and no value is
// evaluated by a shell.
func BuildTerminalProcessSpec(context Context, terminalExecutable string, herdrExecutable string) (ProcessSpec, error) {
	if err := context.Validate(); err != nil {
		return ProcessSpec{}, fmt.Errorf("validate context: %w", err)
	}
	if context.Launcher.Kind != LauncherHerdr || context.Launcher.Terminal == nil {
		return ProcessSpec{}, errors.New("terminal launcher requires a Herdr context")
	}
	if !filepath.IsAbs(terminalExecutable) || !filepath.IsAbs(herdrExecutable) {
		return ProcessSpec{}, errors.New("launcher executables must be absolute paths")
	}
	appID, err := context.ID.AppID()
	if err != nil {
		return ProcessSpec{}, err
	}
	title := context.Label
	if title == "" {
		title = context.Launcher.Session
	}
	var arguments []string
	switch context.Launcher.Terminal.Adapter {
	case TerminalAdapterAlacritty:
		arguments = []string{
			"--class=" + appID,
			"--working-directory=" + context.Launcher.Cwd,
			"--title=" + title,
			"-e", herdrExecutable, "--session", context.Launcher.Session,
		}
	case TerminalAdapterFoot:
		arguments = []string{
			"--app-id=" + appID,
			"--working-directory=" + context.Launcher.Cwd,
			"--title=" + title,
			"--", herdrExecutable, "--session", context.Launcher.Session,
		}
	default:
		return ProcessSpec{}, fmt.Errorf("unsupported terminal adapter %q", context.Launcher.Terminal.Adapter)
	}
	return ProcessSpec{
		Name:                     terminalExecutable,
		Arguments:                arguments,
		Environment:              []string{"SWAY_SESSION_CONTEXT_ID=" + string(context.ID)},
		UnsetEnvironment:         []string{"CODEX_THREAD_ID"},
		UnsetEnvironmentPrefixes: []string{"HERDR_"},
	}, nil
}

// BuildEphemeralTerminalProcessSpec constructs an ordinary shell terminal for
// the configured adapter. It intentionally has no command parameter.
func BuildEphemeralTerminalProcessSpec(adapter TerminalAdapter, cwd string, terminalExecutable string) (ProcessSpec, error) {
	if !filepath.IsAbs(terminalExecutable) {
		return ProcessSpec{}, errors.New("terminal executable must be an absolute path")
	}
	if cwd == "" || !filepath.IsAbs(cwd) || filepath.Clean(cwd) != cwd {
		return ProcessSpec{}, errors.New("terminal cwd must be a clean absolute path")
	}
	if _, err := TerminalAdapterExecutableName(adapter); err != nil {
		return ProcessSpec{}, err
	}
	return ProcessSpec{
		Name:                     terminalExecutable,
		Arguments:                []string{"--working-directory=" + cwd},
		UnsetEnvironment:         []string{"CODEX_THREAD_ID", "SWAY_SESSION_CONTEXT_ID"},
		UnsetEnvironmentPrefixes: []string{"HERDR_"},
	}, nil
}

// FindTerminalAdapterProcesses conservatively identifies any owner process
// whose argv contains the exact UUID-derived Alacritty or Foot application-ID
// option. It intentionally does not require the old adapter binary to remain
// installed: a closed adapter switch must still detect a delayed process after
// an upgrade or uninstall. A same-UID spoof can only block reconfiguration.
func FindTerminalAdapterProcesses(procRoot string, id ContextID) ([]int, error) {
	if !filepath.IsAbs(procRoot) || filepath.Clean(procRoot) != procRoot {
		return nil, errors.New("process root must be a clean absolute path")
	}
	appID, err := id.AppID()
	if err != nil {
		return nil, err
	}
	wanted := map[string]struct{}{
		"--class=" + appID:  {},
		"--app-id=" + appID: {},
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("read process table: %w", err)
	}
	matches := make([]int, 0, 1)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || pid == os.Getpid() {
			continue
		}
		file, err := os.Open(filepath.Join(procRoot, entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		info, statErr := file.Stat()
		stat, statOK := infoSysStat(info)
		if statErr != nil || !statOK || stat.Uid != uint32(os.Getuid()) {
			_ = file.Close()
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(file, 64*1024+1))
		_ = file.Close()
		if readErr != nil || len(data) > 64*1024 {
			continue
		}
		for _, argument := range bytes.Split(bytes.TrimSuffix(data, []byte{0}), []byte{0}) {
			if _, exists := wanted[string(argument)]; exists {
				matches = append(matches, pid)
				break
			}
		}
	}
	sort.Ints(matches)
	return matches, nil
}

func infoSysStat(info os.FileInfo) (*syscall.Stat_t, bool) {
	if info == nil {
		return nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return stat, ok
}

// ValidateTerminalCwdPath validates the persistent, side-effect-free path
// contract before a registry transaction begins.
func ValidateTerminalCwdPath(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("terminal cwd must be a clean absolute path")
	}
	if containsControl(path) {
		return errors.New("terminal cwd must not contain control characters")
	}
	return nil
}

// ValidateTerminalCwd verifies the last filesystem precondition immediately
// before a typed terminal process is started.
func ValidateTerminalCwd(path string) error {
	if err := ValidateTerminalCwdPath(path); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect terminal cwd %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("terminal cwd %s is not a directory", path)
	}
	return nil
}
