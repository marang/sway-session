package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/marang/sway-session/internal/agentreport"
	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/sessionrequest"
	"github.com/marang/sway-session/internal/statefile"
	"github.com/marang/sway-session/internal/swayipc"
	"golang.org/x/sys/unix"
)

const (
	getVersionMessage   swayipc.MessageType = 7 // Sway GET_VERSION.
	runtimeProbeTimeout                     = 1500 * time.Millisecond
	maxProcRead                             = 64 * 1024
	maxBinaryDigestSize                     = 128 * 1024 * 1024
	maxCapabilityOutput                     = 4096
	// Keep this read-only path check aligned with session.maxStateDatabaseBytes.
	maxStateDatabaseSize = int64(1 << 30)
	appArmorEnabledPath  = "/sys/module/apparmor/parameters/enabled"
)

// runtimeProbes keeps observation boundaries replaceable by focused package
// tests. Production uses only read-only filesystem, procfs, IPC, and the two
// documented dry command probes below.
var runtimeProbes = struct {
	getenv     func(string) string
	executable func() (string, error)
	newSway    func(string) swayRequester
	readFile   func(string, int) ([]byte, error)
	readlink   func(string) (string, error)
	lstat      func(string) (os.FileInfo, error)
	run        func(context.Context, string, ...string) ([]byte, error)
}{
	getenv:     os.Getenv,
	executable: os.Executable,
	newSway: func(socket string) swayRequester {
		return swayipc.NewClient(socket)
	},
	readFile: readFileBounded,
	readlink: os.Readlink,
	lstat:    os.Lstat,
	run:      runReadOnlyProbe,
}

type swayRequester interface {
	RequestContext(context.Context, swayipc.MessageType, []byte) (swayipc.Message, error)
	Close()
}

type swayConfigPathSelection struct {
	path string
	live bool
}

// inspectRuntime is deliberately observational. In particular, it does not
// call a state-store API because several of those APIs initialize a database
// or lock directory when given an absent path.
func inspectRuntime(ctx context.Context, options Options) []Check {
	if ctx == nil {
		ctx = context.Background()
	}
	checks := make([]Check, 0, 12)
	config, configPath, configErr := sessionstate.LoadSessionConfig(options.ConfigPath)
	checks = append(checks, sessionConfigCheck(options, configPath, config, configErr))

	if configErr != nil {
		checks = append(checks,
			unavailableCheck("terminal.adapter", "Terminal adapter", "The selected adapter cannot be resolved until the session configuration is valid."),
			unavailableCheck("herdr.executable", "Herdr executable", "Herdr is not evaluated until the session configuration is valid."),
			unavailableCheck("herdr.pane_history", "Herdr pane history", "Herdr pane history is not evaluated until the session configuration is valid."),
			unavailableCheck("herdr.capabilities", "Herdr capabilities", "Capability evidence is unavailable until Herdr is resolved."),
		)
	} else {
		terminal, herdr := inspectTerminalPrograms(config)
		checks = append(checks, terminal, herdr)
		checks = append(checks, inspectHerdrHistory(config))
		checks = append(checks, inspectHerdrCapabilities(ctx, config, herdr))
	}

	socket, socketCheck := selectedSwaySocket(options)
	checks = append(checks, socketCheck)
	if socket != "" {
		checks = append(checks, inspectSwayTree(ctx, socket))
	} else {
		checks = append(checks, unavailableCheck("sway.tree", "Sway tree", "No Sway IPC socket is available, so the compositor tree cannot be inspected."))
	}

	runtimeRoot, runtimeErr := runtimeDirectory()
	runtime := observePrivateDirectory(runtimeRoot, runtimeErr)
	defer runtime.Close()
	checks = append(checks, inspectRuntimePaths(runtime))
	checks = append(checks, inspectStatePaths())
	observation := inspectDaemonLock(runtime)
	checks = append(checks, observation.check)
	checks = append(checks, inspectDaemonBinary(ctx, options, observation))
	checks = append(checks, inspectOptionalSockets(ctx, runtime, observation)...)
	return checks
}

func sessionConfigCheck(options Options, path string, config sessionstate.SessionConfig, err error) Check {
	if err != nil {
		check := Check{ID: "session.config", Title: "Session configuration", Status: Error, Detail: "The session configuration is invalid.", Hint: "Correct the selected configuration file and run doctor again."}
		if path != "" {
			check.Evidence = []string{"path=" + path}
		}
		return check
	}
	evidence := []string{"path=" + path, "adapter=" + string(config.Terminal.Adapter)}
	if options.ConfigPath == "" {
		if _, statErr := runtimeProbes.lstat(path); errors.Is(statErr, os.ErrNotExist) {
			return Check{ID: "session.config", Title: "Session configuration", Status: OK, Detail: "No default session configuration file exists; compiled defaults are active.", Evidence: evidence}
		}
		return Check{ID: "session.config", Title: "Session configuration", Status: OK, Detail: "The default-location session configuration is valid.", Evidence: evidence}
	}
	return Check{ID: "session.config", Title: "Session configuration", Status: OK, Detail: "The explicit session configuration is valid.", Evidence: evidence}
}

func inspectTerminalPrograms(config sessionstate.SessionConfig) (Check, Check) {
	adapterName, err := sessionstate.TerminalAdapterExecutableName(config.Terminal.Adapter)
	if err != nil {
		return Check{ID: "terminal.adapter", Title: "Terminal adapter", Status: Error, Detail: "The selected terminal adapter is unsupported.", Hint: "Select a supported terminal adapter in the session configuration."}, unavailableCheck("herdr.executable", "Herdr executable", "Herdr is not evaluated because the terminal adapter is invalid.")
	}
	terminalPath, err := sessionstate.ResolveTrustedExecutable(adapterName)
	terminal := Check{ID: "terminal.adapter", Title: "Terminal adapter"}
	if err != nil {
		terminal.Status, terminal.Detail, terminal.Hint = Error, "The selected terminal adapter is not available from a trusted executable path.", "Install the selected adapter or choose an installed adapter."
	} else {
		terminal.Status, terminal.Detail, terminal.Evidence = OK, "The selected terminal adapter resolves through a trusted executable path.", []string{"adapter=" + string(config.Terminal.Adapter), "resolved"}
		_ = terminalPath // The trusted resolver has intentionally validated the path; do not disclose it.
	}
	herdrPath, err := sessionstate.ResolveTrustedExecutable("herdr")
	herdr := Check{ID: "herdr.executable", Title: "Herdr executable"}
	if err != nil {
		herdr.Status, herdr.Detail, herdr.Hint = Error, "Herdr is required by the selected terminal session manager but is not available from a trusted executable path.", "Install Herdr from a trusted location and run doctor again."
	} else {
		herdr.Status, herdr.Detail, herdr.Evidence = OK, "Herdr resolves through a trusted executable path.", []string{"resolved"}
		_ = herdrPath // The trusted resolver has intentionally validated the path; do not disclose it.
	}
	return terminal, herdr
}

func inspectHerdrHistory(config sessionstate.SessionConfig) Check {
	if config.Terminal.SessionManager != sessionstate.TerminalSessionManagerHerdr {
		return unavailableCheck("herdr.pane_history", "Herdr pane history", "The selected session manager does not require Herdr pane history.")
	}
	paths, err := sessionstate.DefaultHerdrPaths()
	if err != nil {
		return Check{ID: "herdr.pane_history", Title: "Herdr pane history", Status: Error, Detail: "Herdr paths are invalid for this environment.", Hint: "Set XDG_CONFIG_HOME and HERDR_CONFIG_PATH to clean absolute paths, then run doctor again."}
	}
	if err := sessionstate.ValidateHerdrPaneHistory(paths); err != nil {
		return Check{ID: "herdr.pane_history", Title: "Herdr pane history", Status: Error, Detail: "Herdr pane history is not ready or its owner-only state is unsafe.", Hint: "Enable [experimental] pane_history = true and correct Herdr state permissions."}
	}
	return Check{ID: "herdr.pane_history", Title: "Herdr pane history", Status: OK, Detail: "Herdr pane history is enabled and its checked state is owner-only."}
}

func inspectHerdrCapabilities(ctx context.Context, config sessionstate.SessionConfig, herdr Check) Check {
	if config.Terminal.SessionManager != sessionstate.TerminalSessionManagerHerdr || herdr.Status != OK {
		return unavailableCheck("herdr.capabilities", "Herdr capabilities", "Capability evidence is unavailable because Herdr is not ready.")
	}
	path, err := sessionstate.ResolveTrustedExecutable("herdr")
	if err != nil {
		return unavailableCheck("herdr.capabilities", "Herdr capabilities", "Capability evidence is unavailable.")
	}
	version, versionErr := runtimeProbes.run(ctx, path, "--version")
	if versionErr != nil {
		return unavailableCheck("herdr.capabilities", "Herdr capabilities", "Herdr version evidence could not be collected with a bounded read-only probe.")
	}
	help, helpErr := runtimeProbes.run(ctx, path, "--help")
	if helpErr != nil || !strings.Contains(string(help), "--session") {
		return Check{ID: "herdr.capabilities", Title: "Herdr capabilities", Status: Unavailable, Detail: "Herdr is installed, but this inspection cannot prove the required session capability.", Evidence: []string{"version=" + printableFirstLine(version)}}
	}
	return Check{ID: "herdr.capabilities", Title: "Herdr capabilities", Status: OK, Detail: "Herdr documents its session option in a bounded read-only help probe.", Evidence: []string{"version=" + printableFirstLine(version), "--session documented"}}
}

func selectedSwaySocket(options Options) (string, Check) {
	socket := options.Socket
	if socket == "" {
		socket = runtimeProbes.getenv("SWAYSOCK")
	}
	if socket == "" {
		return "", unavailableCheck("sway.ipc", "Sway IPC", "No Sway IPC socket is available; this is expected outside a Sway session.")
	}
	if !cleanAbsolute(socket) {
		return "", Check{ID: "sway.ipc", Title: "Sway IPC", Status: Error, Detail: "The selected Sway IPC socket is not a clean absolute path.", Hint: "Set SWAYSOCK or --socket to the clean absolute path reported by the active Sway session.", Evidence: []string{"path=" + socket}}
	}
	return socket, Check{ID: "sway.ipc", Title: "Sway IPC", Status: OK, Detail: "A Sway IPC socket was selected; the compositor tree is checked separately.", Evidence: []string{"path=" + socket}}
}

func inspectSwayTree(ctx context.Context, socket string) Check {
	client := runtimeProbes.newSway(socket)
	if client == nil {
		return unavailableCheck("sway.tree", "Sway tree", "Sway tree inspection is unavailable.")
	}
	defer client.Close()
	message, err := client.RequestContext(ctx, swayipc.GetTree, nil)
	if err != nil {
		return unavailableCheck("sway.tree", "Sway tree", "The Sway compositor tree is unavailable; this is expected when Sway is not running.")
	}
	if message.Type != swayipc.GetTree {
		return Check{ID: "sway.tree", Title: "Sway tree", Status: Error, Detail: "Sway returned an unexpected response to the tree request.", Hint: "Verify that the selected socket belongs to a compatible Sway compositor."}
	}
	var tree swayipc.TreeNode
	if err := json.Unmarshal(message.Payload, &tree); err != nil || tree.Type != "root" {
		return Check{ID: "sway.tree", Title: "Sway tree", Status: Error, Detail: "Sway returned an invalid compositor tree.", Hint: "Verify the Sway version and selected IPC socket, then run doctor again."}
	}
	return Check{ID: "sway.tree", Title: "Sway tree", Status: OK, Detail: "Sway returned a valid compositor tree."}
}

// resolveSwayConfigPath resolves a repair target without reading any config
// content. Use resolveSwayConfigPathSelection where live-vs-on-disk wording is
// needed by a caller.
func resolveSwayConfigPath(ctx context.Context, options Options) (string, error) {
	selection, err := resolveSwayConfigPathSelection(ctx, options)
	return selection.path, err
}

func resolveSwayConfigPathSelection(ctx context.Context, options Options) (swayConfigPathSelection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if options.SwayConfigPath != "" {
		if !cleanAbsolute(options.SwayConfigPath) {
			return swayConfigPathSelection{}, errors.New("sway config path must be a clean absolute path")
		}
		return swayConfigPathSelection{path: options.SwayConfigPath}, nil
	}
	socket, socketCheck := selectedSwaySocket(options)
	if socketCheck.Status == OK && socket != "" {
		client := runtimeProbes.newSway(socket)
		if client != nil {
			defer client.Close()
			message, err := client.RequestContext(ctx, getVersionMessage, nil)
			if err == nil {
				if message.Type != getVersionMessage {
					return swayConfigPathSelection{}, errors.New("sway returned an unexpected version response")
				}
				var version struct {
					LoadedConfigFileName string `json:"loaded_config_file_name"`
				}
				if err := json.Unmarshal(message.Payload, &version); err != nil || !cleanAbsolute(version.LoadedConfigFileName) {
					return swayConfigPathSelection{}, errors.New("sway returned an invalid loaded config path")
				}
				return swayConfigPathSelection{path: version.LoadedConfigFileName, live: true}, nil
			}
		}
	}
	path, err := defaultSwayConfigPath()
	if err != nil {
		return swayConfigPathSelection{}, err
	}
	return swayConfigPathSelection{path: path}, nil
}

func defaultSwayConfigPath() (string, error) {
	configHome := runtimeProbes.getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}
	if !cleanAbsolute(configHome) {
		return "", errors.New("XDG_CONFIG_HOME must be a clean absolute path")
	}
	return filepath.Join(configHome, "sway", "config"), nil
}

func runtimeDirectory() (string, error) {
	runtime := runtimeProbes.getenv("XDG_RUNTIME_DIR")
	if !cleanAbsolute(runtime) {
		return "", errors.New("XDG_RUNTIME_DIR must be a clean absolute path")
	}
	return filepath.Join(runtime, "sway-session"), nil
}

type privateDirectoryObservation struct {
	path         string
	directory    *os.File
	selectionErr error
	err          error
}

func observePrivateDirectory(path string, pathErr error) privateDirectoryObservation {
	observation := privateDirectoryObservation{path: path, selectionErr: pathErr}
	if pathErr != nil {
		return observation
	}
	observation.directory, observation.err = statefile.OpenPrivateDirectory(path, false)
	return observation
}

func (observation *privateDirectoryObservation) Close() {
	if observation == nil || observation.directory == nil {
		return
	}
	_ = observation.directory.Close()
	observation.directory = nil
}

func inspectRuntimePaths(observation privateDirectoryObservation) Check {
	if observation.selectionErr != nil || observation.path == "" {
		return Check{ID: "runtime.paths", Title: "Runtime paths", Status: Unavailable, Detail: "The private runtime directory cannot be located from XDG_RUNTIME_DIR.", Hint: "Set XDG_RUNTIME_DIR to the clean absolute runtime directory created for this login session."}
	}
	if observation.err != nil {
		if errors.Is(observation.err, os.ErrNotExist) {
			return Check{ID: "runtime.paths", Title: "Runtime paths", Status: Unavailable, Detail: "No sway-session runtime directory exists; the daemon is not currently initialized.", Evidence: []string{"path=" + observation.path}}
		}
		return privateObjectErrorCheck("runtime.paths", "Runtime paths", "The sway-session runtime directory or one of its ancestors does not satisfy the private-path policy.", observation.path, "trusted ancestors and owner-only directory mode 0700", unix.Stat_t{}, "Correct the runtime path ownership and permissions, or restart the user session to recreate it safely.")
	}
	return Check{ID: "runtime.paths", Title: "Runtime paths", Status: OK, Detail: "The sway-session runtime path has trusted ancestors and an owner-only directory.", Evidence: []string{"path=" + observation.path}}
}

type daemonObservation struct {
	check Check
	pid   int
	valid bool
}

func inspectDaemonLock(runtime privateDirectoryObservation) daemonObservation {
	if runtime.selectionErr != nil || runtime.path == "" {
		return daemonObservation{check: unavailableCheck("daemon.lock", "Daemon lock", "The runtime directory is unavailable, so daemon lock state cannot be inspected.")}
	}
	path := filepath.Join(runtime.path, "daemon.lock")
	if runtime.err != nil && !errors.Is(runtime.err, os.ErrNotExist) {
		return daemonObservation{check: Check{ID: "daemon.lock", Title: "Daemon lock", Status: Unavailable, Detail: "The daemon lock cannot be inspected because the runtime path is unsafe.", Evidence: []string{"path=" + path}}}
	}
	if runtime.directory == nil {
		return daemonObservation{check: Check{ID: "daemon.lock", Title: "Daemon lock", Status: Unavailable, Detail: "No daemon lock file exists; no daemon is proven to be running.", Evidence: []string{"path=" + path}}}
	}
	lockStat, err := inspectPrivateObjectAt(runtime.directory, "daemon.lock", unix.S_IFREG, statefile.RegularFileMode)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return daemonObservation{check: Check{ID: "daemon.lock", Title: "Daemon lock", Status: Unavailable, Detail: "No daemon lock file exists; no daemon is proven to be running.", Evidence: []string{"path=" + path}}}
		}
		return daemonObservation{check: privateObjectErrorCheck("daemon.lock", "Daemon lock", "The daemon lock is not an owner-only single-link regular file.", path, "single-link owner-only regular file mode 0600", lockStat, "Stop and inspect the lock holder before correcting this path; never replace it while a daemon may be running.")}
	}
	pid, err := lockedByPID(&lockStat)
	if err != nil {
		return daemonObservation{check: Check{ID: "daemon.lock", Title: "Daemon lock", Status: Unavailable, Detail: "The daemon lock file exists, but no held advisory lock could be proven.", Hint: "Start or restart the daemon if persistent-session service is expected.", Evidence: []string{"path=" + path}}}
	}
	if !sameUID(pid) || !daemonCommand(pid) {
		return daemonObservation{check: Check{ID: "daemon.lock", Title: "Daemon lock", Status: Unavailable, Detail: "A process holds the lock, but it is not proven to be this user's sway-session daemon.", Hint: "Inspect the same-user lock holder before restarting the daemon.", Evidence: []string{"path=" + path, fmt.Sprintf("pid=%d", pid)}}}
	}
	return daemonObservation{pid: pid, valid: true, check: Check{ID: "daemon.lock", Title: "Daemon lock", Status: OK, Detail: "A same-user sway-session daemon holds the advisory lock.", Evidence: []string{"path=" + path, fmt.Sprintf("pid=%d", pid), "held"}}}
}

func inspectDaemonBinary(ctx context.Context, options Options, observation daemonObservation) Check {
	if !observation.valid {
		return unavailableCheck("daemon.binary", "Daemon binary", "A live sway-session daemon binary cannot be proven without a verified held daemon lock.")
	}
	candidate := options.Executable
	if candidate == "" {
		var err error
		candidate, err = runtimeProbes.executable()
		if err != nil {
			return unavailableCheck("daemon.binary", "Daemon binary", "The current sway-session executable cannot be identified.")
		}
	}
	if !cleanAbsolute(candidate) {
		return Check{ID: "daemon.binary", Title: "Daemon binary", Status: Error, Detail: "The current sway-session executable is not a clean absolute path.", Hint: "Run doctor through a clean absolute sway-session executable path.", Evidence: []string{"path=" + candidate}}
	}
	currentFile, current, err := openBoundedBinary(candidate)
	if err != nil {
		return Check{ID: "daemon.binary", Title: "Daemon binary", Status: Error, Detail: "The current sway-session executable is not a bounded regular file.", Hint: "Reinstall sway-session at the reported executable path, then run doctor again.", Evidence: []string{"path=" + candidate}}
	}
	defer currentFile.Close()
	runningExecutable := filepath.Join("/proc", strconv.Itoa(observation.pid), "exe")
	runningPath, err := runtimeProbes.readlink(filepath.Join("/proc", strconv.Itoa(observation.pid), "exe"))
	if err != nil {
		return unavailableCheck("daemon.binary", "Daemon binary", "The running daemon executable cannot be inspected.")
	}
	deleted := strings.HasSuffix(runningPath, " (deleted)")
	runningFile, running, err := openBoundedBinary(runningExecutable)
	if err != nil {
		return Check{ID: "daemon.binary", Title: "Daemon binary", Status: Unavailable, Detail: "The running daemon executable cannot be inspected as a bounded regular file.", Hint: "Confirm that the reported daemon PID is still alive, then run doctor again.", Evidence: []string{fmt.Sprintf("pid=%d", observation.pid)}}
	}
	defer runningFile.Close()
	if sameInode(current, running) {
		return Check{ID: "daemon.binary", Title: "Daemon binary", Status: OK, Detail: "The running daemon uses the current sway-session binary.", Evidence: []string{"path=" + candidate, fmt.Sprintf("pid=%d", observation.pid), "inode match"}}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	comparisonContext, cancel := context.WithTimeout(ctx, runtimeProbeTimeout)
	defer cancel()
	match, err := sameDigest(comparisonContext, currentFile, current, runningFile, running)
	if err != nil {
		return Check{ID: "daemon.binary", Title: "Daemon binary", Status: Unavailable, Detail: "The daemon differs by inode and a bounded binary comparison is unavailable.", Hint: "Run doctor again; restart the daemon if the comparison remains unavailable.", Evidence: []string{"path=" + candidate, fmt.Sprintf("pid=%d", observation.pid)}}
	}
	if match {
		return Check{ID: "daemon.binary", Title: "Daemon binary", Status: OK, Detail: "The running daemon differs by inode but matches the current binary content.", Evidence: []string{"path=" + candidate, fmt.Sprintf("pid=%d", observation.pid), "digest match"}}
	}
	detail := "The running daemon binary does not match the current sway-session binary."
	if deleted {
		detail = "The running daemon uses a deleted binary that does not match the current sway-session binary."
	}
	return Check{ID: "daemon.binary", Title: "Daemon binary", Status: Warning, Detail: detail, Hint: "Restart the daemon after upgrading sway-session.", Evidence: []string{"path=" + candidate, fmt.Sprintf("pid=%d", observation.pid)}}
}

func inspectOptionalSockets(ctx context.Context, runtime privateDirectoryObservation, daemon daemonObservation) []Check {
	checks := make([]Check, 0, 3)
	for _, endpoint := range []struct{ id, title, name string }{
		{"broker.session_start", "Session-start broker", sessionrequest.SocketFilename},
		{"broker.agent_report", "Agent-report broker", agentreport.SocketFilename},
	} {
		if runtime.selectionErr != nil || runtime.path == "" {
			checks = append(checks, unavailableCheck(endpoint.id, endpoint.title, "This optional broker endpoint is unavailable because XDG runtime paths are unavailable."))
			continue
		}
		path := filepath.Join(runtime.path, endpoint.name)
		if runtime.err != nil && !errors.Is(runtime.err, os.ErrNotExist) {
			checks = append(checks, Check{ID: endpoint.id, Title: endpoint.title, Status: Unavailable, Detail: "This optional broker endpoint cannot be inspected because the runtime path is unsafe.", Evidence: []string{"path=" + path}})
			continue
		}
		if runtime.directory == nil {
			checks = append(checks, Check{ID: endpoint.id, Title: endpoint.title, Status: Unavailable, Detail: "This optional broker endpoint is not running.", Evidence: []string{"path=" + path}})
			continue
		}
		stat, err := inspectPrivateObjectAt(runtime.directory, endpoint.name, unix.S_IFSOCK, statefile.RegularFileMode)
		if errors.Is(err, os.ErrNotExist) {
			status := Unavailable
			detail := "This optional broker endpoint is not running."
			if daemon.valid {
				status = Warning
				detail = "The verified daemon is running, but this optional broker endpoint is absent."
			}
			checks = append(checks, Check{ID: endpoint.id, Title: endpoint.title, Status: status, Detail: detail, Evidence: []string{"path=" + path}})
		} else if err != nil {
			checks = append(checks, privateObjectErrorCheck(endpoint.id, endpoint.title, "This optional broker endpoint exists but is not an owner-only socket.", path, "single-link owner-only socket mode 0600", stat, "Stop the daemon and inspect the reported endpoint before correcting it, then restart the daemon."))
		} else if !daemon.valid {
			checks = append(checks, Check{ID: endpoint.id, Title: endpoint.title, Status: Unavailable, Detail: "An owner-only broker socket exists, but no verified sway-session daemon is running.", Evidence: []string{"path=" + path, "owner-only socket"}})
		} else {
			err := probePinnedSocketListener(ctx, runtime.directory, endpoint.name, daemon.pid)
			if err != nil {
				checks = append(checks, Check{ID: endpoint.id, Title: endpoint.title, Status: Warning, Detail: "The verified daemon is running, but this owner-only broker socket did not accept a safe liveness probe.", Hint: "Restart the daemon and run doctor again if broker operations are expected.", Evidence: []string{"path=" + path, "owner-only socket", "probe=" + err.Error()}})
				continue
			}
			checks = append(checks, Check{ID: endpoint.id, Title: endpoint.title, Status: OK, Detail: "An owner-only broker listener is accepting connections from the verified sway-session daemon.", Evidence: []string{"path=" + path, "owner-only socket", fmt.Sprintf("pid=%d", daemon.pid), "connect-only probe"}})
		}
	}
	return checks
}

func probePinnedSocketListener(parent context.Context, directory *os.File, name string, expectedPID int) error {
	if directory == nil || name == "" || expectedPID <= 0 {
		return errors.New("invalid liveness probe inputs")
	}
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("pin socket: %w", err)
	}
	defer unix.Close(fd)
	var pinned unix.Stat_t
	if err := unix.Fstat(fd, &pinned); err != nil {
		return fmt.Errorf("inspect pinned socket: %w", err)
	}
	if pinned.Mode&unix.S_IFMT != unix.S_IFSOCK || pinned.Uid != uint32(os.Geteuid()) || pinned.Nlink != 1 || pinned.Mode&0o7777 != statefile.RegularFileMode {
		return errors.New("pinned endpoint is not an owner-only socket")
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, runtimeProbeTimeout)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer connection.Close()
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return errors.New("unexpected non-Unix liveness connection")
	}
	credentials, err := peerCredentials(unixConnection)
	if err != nil {
		return err
	}
	if credentials.Uid != uint32(os.Geteuid()) || int(credentials.Pid) != expectedPID {
		return fmt.Errorf("listener credentials do not match verified daemon pid %d", expectedPID)
	}
	var current unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("reinspect socket: %w", err)
	}
	if current.Dev != pinned.Dev || current.Ino != pinned.Ino {
		return errors.New("socket path changed during liveness probe")
	}
	return nil
}

func peerCredentials(connection *net.UnixConn) (*unix.Ucred, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("inspect listener peer: %w", err)
	}
	var credentials *unix.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, fmt.Errorf("inspect listener peer: %w", err)
	}
	if socketErr != nil || credentials == nil {
		return nil, fmt.Errorf("inspect listener credentials: %w", socketErr)
	}
	return credentials, nil
}

// appArmorAvailabilityCheck deliberately reports only whether AppArmor is
// active. The policy is optional agent hardening, not a sway-session runtime
// requirement, so doctor neither compares nor claims to verify a deployment.
func appArmorAvailabilityCheck() Check {
	enabled, err := runtimeProbes.readFile(appArmorEnabledPath, 16)
	if err != nil {
		return Check{ID: "apparmor", Title: "AppArmor", Status: Unavailable, Detail: "AppArmor availability cannot be inspected on this system.", Hint: "See the optional agent-home-guard template in the sway-session documentation.", Evidence: []string{"path=" + appArmorEnabledPath}}
	}
	if strings.TrimSpace(string(enabled)) != "Y" {
		return Check{ID: "apparmor", Title: "AppArmor", Status: Unavailable, Detail: "AppArmor is not enabled; sway-session remains fully functional without it.", Hint: "See the optional agent-home-guard template in the sway-session documentation.", Evidence: []string{"path=" + appArmorEnabledPath}}
	}
	return Check{ID: "apparmor", Title: "AppArmor", Status: OK, Detail: "AppArmor is enabled. agent-home-guard is an optional explicit-hardening template.", Hint: "See the optional agent-home-guard template in the sway-session documentation.", Evidence: []string{"path=" + appArmorEnabledPath, "module enabled"}}
}

func inspectStatePaths() Check {
	root, err := sessionstate.DefaultStateRoot()
	return inspectStatePathsAt(root, err)
}

func inspectStatePathsAt(root string, rootErr error) Check {
	observation := observePrivateDirectory(root, rootErr)
	defer observation.Close()
	if observation.selectionErr != nil || observation.path == "" {
		return Check{ID: "state.paths", Title: "State paths", Status: Unavailable, Detail: "The private state directory cannot be located from XDG_STATE_HOME.", Hint: "Set XDG_STATE_HOME to a clean absolute path, then run doctor again."}
	}
	if observation.err != nil {
		if errors.Is(observation.err, os.ErrNotExist) {
			return Check{ID: "state.paths", Title: "State paths", Status: Unavailable, Detail: "No sway-session state directory exists yet.", Evidence: []string{"path=" + root}}
		}
		return privateObjectErrorCheck("state.paths", "State paths", "The sway-session state directory or one of its ancestors does not satisfy the private-path policy.", root, "trusted ancestors and owner-only directory mode 0700", unix.Stat_t{}, "Correct the state path ownership and permissions before starting the daemon.")
	}
	evidence := []string{"path=" + root}
	database := filepath.Join(root, sessionstate.StateDatabaseFilename)
	databaseStat, databaseErr := inspectPrivateObjectAt(observation.directory, sessionstate.StateDatabaseFilename, unix.S_IFREG, statefile.RegularFileMode)
	databaseExists := databaseErr == nil
	if databaseErr != nil && !errors.Is(databaseErr, os.ErrNotExist) {
		return privateObjectErrorCheck("state.paths", "State paths", "The sway-session state database is not an owner-only single-link regular file.", database, "single-link owner-only regular file mode 0600", databaseStat, "Restore safe ownership and permissions from a trusted backup before starting the daemon.")
	}
	if databaseExists {
		evidence = append(evidence, "database="+database)
		if databaseStat.Size < 0 || databaseStat.Size > maxStateDatabaseSize {
			return Check{ID: "state.paths", Title: "State paths", Status: Error, Detail: "The sway-session state database exceeds its supported size bound.", Hint: "Do not truncate the database; restore or compact it with an approved recovery procedure.", Evidence: []string{"path=" + database, fmt.Sprintf("size=%d", databaseStat.Size), fmt.Sprintf("maximum=%d", maxStateDatabaseSize)}}
		}
	}
	sidecarExists := false
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		path := database + suffix
		stat, sidecarErr := inspectPrivateObjectAt(observation.directory, sessionstate.StateDatabaseFilename+suffix, unix.S_IFREG, statefile.RegularFileMode)
		if errors.Is(sidecarErr, os.ErrNotExist) {
			continue
		}
		// SQLite may unlink a sidecar after the descriptor is opened but before
		// it is inspected. The detached inode is no longer reachable and the
		// production state-store validation treats the same race as absence.
		if sidecarErr != nil && detachedPrivateSidecar(stat) {
			continue
		}
		if sidecarErr != nil {
			return privateObjectErrorCheck("state.paths", "State paths", "A sway-session SQLite sidecar is not an owner-only single-link regular file.", path, "single-link owner-only regular file mode 0600", stat, "Do not follow or replace the unsafe sidecar; restore the state directory from a trusted backup before starting the daemon.")
		}
		sidecarExists = true
		evidence = append(evidence, "sidecar="+path)
	}
	if !databaseExists {
		if sidecarExists {
			return Check{ID: "state.paths", Title: "State paths", Status: Warning, Detail: "Owner-only SQLite sidecars exist without a main sway-session state database.", Hint: "Keep these files intact and use an approved database recovery procedure before starting the daemon.", Evidence: evidence}
		}
		return Check{ID: "state.paths", Title: "State paths", Status: OK, Detail: "The sway-session state directory is owner-only; no state database has been initialized.", Evidence: evidence}
	}
	return Check{ID: "state.paths", Title: "State paths", Status: OK, Detail: "The sway-session state directory and database objects are owner-only.", Evidence: evidence}
}

func detachedPrivateSidecar(stat unix.Stat_t) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Mode&0o7777 == 0o600 && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 0
}

func inspectPrivateObjectAt(directory *os.File, name string, wantType uint32, wantMode uint32) (unix.Stat_t, error) {
	if directory == nil || name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return unix.Stat_t{}, errors.New("invalid private directory entry")
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return unix.Stat_t{}, err
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return stat, errors.New("untrusted ownership or links")
	}
	if stat.Mode&unix.S_IFMT != wantType {
		return stat, errors.New("unexpected object type")
	}
	if stat.Mode&0o7777 != wantMode {
		return stat, errors.New("unexpected permissions")
	}
	return stat, nil
}

func lockedByPID(stat *unix.Stat_t) (int, error) {
	data, err := readBounded("/proc/locks", maxProcRead)
	if err != nil {
		return 0, err
	}
	major, minor := unixMajorMinor(uint64(stat.Dev))
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[1] != "FLOCK" || fields[3] != "WRITE" {
			continue
		}
		pid, err := strconv.Atoi(fields[4])
		if err != nil || pid <= 0 || !sameLockDeviceInode(fields[5], major, minor, uint64(stat.Ino)) {
			continue
		}
		return pid, nil
	}
	return 0, errors.New("no matching lock")
}

func unixMajorMinor(device uint64) (uint64, uint64) {
	return (device >> 8) & 0xfff, (device & 0xff) | ((device >> 12) & 0xfff00)
}

func sameLockDeviceInode(value string, major, minor, inode uint64) bool {
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return false
	}
	gotMajor, majorErr := strconv.ParseUint(parts[0], 16, 64)
	gotMinor, minorErr := strconv.ParseUint(parts[1], 16, 64)
	gotInode, inodeErr := strconv.ParseUint(parts[2], 10, 64)
	return majorErr == nil && minorErr == nil && inodeErr == nil && gotMajor == major && gotMinor == minor && gotInode == inode
}

func sameUID(pid int) bool {
	data, err := readBounded(filepath.Join("/proc", strconv.Itoa(pid), "status"), maxProcRead)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(line)
		return len(fields) >= 2 && fields[1] == strconv.Itoa(os.Geteuid())
	}
	return false
}

func daemonCommand(pid int) bool {
	data, err := readBounded(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"), maxProcRead)
	if err != nil {
		return false
	}
	return isDaemonCommand(strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00"))
}

func isDaemonCommand(args []string) bool {
	if len(args) < 2 || filepath.Base(args[0]) != "sway-session" {
		return false
	}
	filtered := make([]string, 0, len(args)-1)
	optionsEnded := false
	for _, argument := range args[1:] {
		if optionsEnded {
			filtered = append(filtered, argument)
			continue
		}
		if argument == "--" {
			optionsEnded = true
			filtered = append(filtered, argument)
			continue
		}
		if argument != "--json" {
			filtered = append(filtered, argument)
		}
	}
	if len(filtered) == 0 || filtered[0] != "daemon" {
		return false
	}
	set := flag.NewFlagSet("daemon-observation", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	socket := set.String("socket", "", "")
	if err := set.Parse(filtered[1:]); err != nil || set.NArg() != 0 {
		return false
	}
	return *socket == "" || cleanAbsolute(*socket)
}

func readBounded(path string, limit int) ([]byte, error) {
	return runtimeProbes.readFile(path, limit)
}

func readFileBounded(path string, limit int) ([]byte, error) {
	if limit < 0 {
		return nil, errors.New("negative read bound")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("read exceeds bound")
	}
	return data, nil
}

func sameInode(first, second os.FileInfo) bool {
	one, firstOK := first.Sys().(*syscall.Stat_t)
	two, secondOK := second.Sys().(*syscall.Stat_t)
	return firstOK && secondOK && one.Dev == two.Dev && one.Ino == two.Ino
}

func openBoundedBinary(path string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, nil, errors.New("create binary file handle")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxBinaryDigestSize {
		_ = file.Close()
		return nil, nil, errors.New("binary is not a bounded regular file")
	}
	return file, info, nil
}

func sameDigest(ctx context.Context, first *os.File, firstInfo os.FileInfo, second *os.File, secondInfo os.FileInfo) (bool, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
	if firstInfo.Size() != secondInfo.Size() {
		return false, nil
	}
	firstHash, err := digestOpenFile(ctx, first, firstInfo.Size())
	if err != nil {
		return false, err
	}
	secondHash, err := digestOpenFile(ctx, second, secondInfo.Size())
	if err != nil {
		return false, err
	}
	return firstHash == secondHash, nil
}

func digestOpenFile(ctx context.Context, file *os.File, expectedSize int64) ([sha256.Size]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return [sha256.Size]byte{}, err
	}
	if file == nil || expectedSize < 0 || expectedSize > maxBinaryDigestSize {
		return [sha256.Size]byte{}, errors.New("binary cannot be bounded")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return [sha256.Size]byte{}, err
	}
	hash := sha256.New()
	reader := &contextReader{ctx: ctx, reader: file}
	count, err := io.Copy(hash, io.LimitReader(reader, expectedSize+1))
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	if count != expectedSize {
		return [sha256.Size]byte{}, errors.New("binary changed during bounded comparison")
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(data []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(data)
}

func runReadOnlyProbe(ctx context.Context, executable string, arguments ...string) ([]byte, error) {
	probeCtx, cancel := context.WithTimeout(ctx, runtimeProbeTimeout)
	defer cancel()
	command := exec.CommandContext(probeCtx, executable, arguments...)
	// A wrapper can exit while a child still owns its output pipe. Context
	// cancellation alone does not bound os/exec's pipe-draining wait.
	command.WaitDelay = runtimeProbeTimeout
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	if output.overflow {
		return nil, errors.New("probe output exceeds bound")
	}
	if err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

type limitedBuffer struct {
	data     []byte
	overflow bool
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	if len(buffer.data)+len(data) > maxCapabilityOutput {
		buffer.overflow = true
		remaining := maxCapabilityOutput - len(buffer.data)
		if remaining > 0 {
			buffer.data = append(buffer.data, data[:remaining]...)
		}
		return len(data), nil
	}
	buffer.data = append(buffer.data, data...)
	return len(data), nil
}

func (buffer *limitedBuffer) Bytes() []byte { return append([]byte(nil), buffer.data...) }

func printableFirstLine(data []byte) string {
	line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	line = strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return -1
		}
		return character
	}, line)
	if len(line) > 256 {
		line = line[:256]
	}
	if line == "" {
		return "reported"
	}
	return line
}

func cleanAbsolute(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func privateObjectErrorCheck(id, title, detail, path, expected string, stat unix.Stat_t, hint string) Check {
	evidence := []string{"path=" + path, "expected=" + expected}
	if stat.Mode != 0 {
		evidence = append(evidence, fmt.Sprintf("observed=type:%#o mode:%04o uid:%d links:%d size:%d", stat.Mode&unix.S_IFMT, stat.Mode&0o7777, stat.Uid, stat.Nlink, stat.Size))
	}
	return Check{ID: id, Title: title, Status: Error, Detail: detail, Hint: hint, Evidence: evidence}
}

func unavailableCheck(id, title, detail string) Check {
	return Check{ID: id, Title: title, Status: Unavailable, Detail: detail}
}
