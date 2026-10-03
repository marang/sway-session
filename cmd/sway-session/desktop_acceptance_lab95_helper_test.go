package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"golang.org/x/sys/unix"
)

// This injected starter changes ownership/bookkeeping only. Prepare below uses
// the production daemon adapter; every accepted Start executes real fixed GIO.
// A dedicated subprocess is the subreaper, so Wait4 never steals another test's
// children. Chrome/crashpad may detach from the GIO session/process group.
type lab95GIOStarter struct {
	root   string
	input  *json.Encoder
	output *json.Decoder
	starts int
}

type lab95SupervisorRequest struct {
	Inspect bool
	Spec    sessionstate.ProcessSpec
}

type lab95SupervisorResult struct {
	Error              string
	Starts             int
	BrowserPID         int
	SandboxedRenderers int
	Renderers          int
	SandboxStatus      string
	Processes          int
	Remaining          int
}

func (starter *lab95GIOStarter) validate(spec sessionstate.ProcessSpec) error {
	if spec.Name != "/usr/bin/gio" || len(spec.Arguments) != 2 || spec.Arguments[0] != "launch" {
		return errors.New("LAB95 fixture accepts only fixed /usr/bin/gio launch")
	}
	path := spec.Arguments[1]
	rel, err := filepath.Rel(starter.root, path)
	if err != nil || !filepath.IsAbs(path) || filepath.Clean(path) != path || rel == ".." || strings.HasPrefix(rel, "../") || filepath.Ext(path) != ".desktop" {
		return errors.New("LAB95 fixture refuses a desktop path outside its disposable root")
	}
	if !slices.Equal(spec.Environment, []string{"PATH=/usr/local/bin:/usr/bin"}) || len(spec.UnsetInheritedEnvironment) != 0 || len(spec.UnsetInheritedEnvironmentPrefixes) != 0 {
		return errors.New("LAB95 fixture refuses unexpected environment overrides")
	}
	return nil
}

func (starter *lab95GIOStarter) exchange(request lab95SupervisorRequest) (lab95SupervisorResult, error) {
	var result lab95SupervisorResult
	if err := starter.input.Encode(request); err != nil {
		return result, err
	}
	if err := starter.output.Decode(&result); err != nil {
		return result, err
	}
	starter.starts = result.Starts
	if result.Error != "" {
		return result, errors.New(result.Error)
	}
	return result, nil
}

func (starter *lab95GIOStarter) Start(spec sessionstate.ProcessSpec) error {
	if err := starter.validate(spec); err != nil {
		return err
	}
	_, err := starter.exchange(lab95SupervisorRequest{Spec: spec})
	return err
}

type lab95OwnedLauncher struct {
	starter  *lab95GIOStarter
	state    string
	approved sessionstate.Launcher
}

func (launcher lab95OwnedLauncher) Prepare(ctx context.Context, item sessionstate.Context) (preparedApplicationLaunch, error) {
	if item.Launcher != launcher.approved {
		return nil, errors.New("LAB95 typed approved launcher changed")
	}
	prepared, err := (daemonApplicationLauncher{stateRoot: launcher.state}).Prepare(ctx, item)
	if err != nil {
		return nil, err
	}
	launch, ok := prepared.(preparedDesktopApplicationLaunch)
	if !ok {
		return nil, errors.New("LAB95 production adapter did not prepare desktop GIO")
	}
	if !slices.Equal(launch.spec.Arguments, []string{"launch", launcher.approved.ApprovedDesktopPath}) {
		return nil, errors.New("LAB95 production adapter did not select the protected approved snapshot")
	}
	launch.starter = launcher.starter
	return launch, nil
}

func lab95NewGIOStarter(t *testing.T, h *restoreCleanupHeadless, root string) *lab95GIOStarter {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(filepath.Join(h.root, "lab95-supervisor.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	command := exec.CommandContext(h.ctx, executable, "-test.run=^TestLAB95DesktopSupervisor$")
	command.Env = append(slices.Clone(h.env), "SWAY_SESSION_LAB95_SUPERVISOR_ROOT="+root)
	command.Dir, command.Stderr = root, log
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	starter := &lab95GIOStarter{root: root, input: json.NewEncoder(input), output: json.NewDecoder(output)}
	// Wait starts only after stdout is fully consumed, as required by StdoutPipe.
	t.Cleanup(func() {
		_ = input.Close()
		finished := make(chan error, 1)
		go func() {
			var result lab95SupervisorResult
			err := starter.output.Decode(&result)
			if err == nil && (result.Error != "" || result.Remaining != 0) {
				err = fmt.Errorf("supervisor cleanup: %+v", result)
			}
			finished <- errors.Join(err, command.Wait())
		}()
		select {
		case err := <-finished:
			if err != nil {
				t.Errorf("LAB95 supervisor/child cleanup: %v (private log %s)", err, log.Name())
			}
		case <-time.After(9 * time.Second):
			// SIGTERM asks the dedicated subreaper to clean up its pinned children.
			_ = command.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-finished:
				t.Errorf("LAB95 supervisor exceeded cleanup budget: %v", err)
			case <-time.After(7 * time.Second):
				t.Error("LAB95 supervisor did not acknowledge cleanup")
			}
		}
	})
	return starter
}

type lab95Process struct {
	pid, parent int
	birth       string
}

func lab95ReadProcess(pid int) (lab95Process, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return lab95Process{}, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return lab95Process{}, errors.New("invalid proc stat")
	}
	fields := strings.Fields(string(data)[end+1:])
	if len(fields) < 20 {
		return lab95Process{}, errors.New("short proc stat")
	}
	parent, err := strconv.Atoi(fields[1])
	return lab95Process{pid: pid, parent: parent, birth: fields[19]}, err
}

func lab95Descendants() ([]lab95Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var processes []lab95Process
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		process, err := lab95ReadProcess(pid)
		if err == nil {
			processes = append(processes, process)
		}
	}
	owned := map[int]bool{os.Getpid(): true}
	var result []lab95Process
	for changed := true; changed; {
		changed = false
		for _, process := range processes {
			if !owned[process.pid] && owned[process.parent] {
				owned[process.pid], changed = true, true
				result = append(result, process)
			}
		}
	}
	return result, nil
}

// Pin the PID and recheck its birth time before any signal. A reused PID must
// never turn a fixture cleanup into a signal to an unrelated desktop process.
func lab95SignalOwned(process lab95Process, signal unix.Signal) error {
	fd, err := unix.PidfdOpen(process.pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	current, err := lab95ReadProcess(process.pid)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.birth != process.birth {
		return errors.New("owned process PID was reused")
	}
	err = unix.PidfdSendSignal(fd, signal, nil, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

func lab95ReapChildren() {
	for {
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
	}
}

func lab95CleanupChildren() lab95SupervisorResult {
	deadline := time.Now().Add(6 * time.Second)
	var failures []error
	for {
		lab95ReapChildren()
		processes, err := lab95Descendants()
		if err != nil {
			return lab95SupervisorResult{Error: err.Error()}
		}
		if len(processes) == 0 {
			result := lab95SupervisorResult{}
			if err := errors.Join(failures...); err != nil {
				result.Error = err.Error()
			}
			return result
		}
		if time.Now().After(deadline) {
			return lab95SupervisorResult{Error: "owned GUI children survived cleanup", Remaining: len(processes)}
		}
		signal := unix.SIGTERM
		if time.Until(deadline) < 4*time.Second {
			signal = unix.SIGKILL
		}
		for _, process := range processes {
			if err := lab95SignalOwned(process, signal); err != nil {
				failures = append(failures, err)
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func lab95InspectChildren(root string) lab95SupervisorResult {
	processes, err := lab95Descendants()
	if err != nil {
		return lab95SupervisorResult{Error: err.Error()}
	}
	var result lab95SupervisorResult
	result.Processes = len(processes)
	for _, process := range processes {
		path := fmt.Sprintf("/proc/%d/", process.pid)
		executable, _ := os.Readlink(path + "exe")
		data, _ := os.ReadFile(path + "cmdline")
		// Chrome 154 rewrites /proc cmdline to a single space-separated process
		// title. Our fixed flags and generated profile path contain no spaces;
		// tolerate that representation as well as the original NUL argv.
		args := strings.Fields(strings.ReplaceAll(string(data), "\x00", " "))
		if slices.Contains(args, "--no-sandbox") || slices.Contains(args, "--disable-setuid-sandbox") {
			result.Error = "Chrome disabled its normal sandbox"
			return result
		}
		if executable == "/opt/google/chrome/chrome" && slices.Contains(args, "--user-data-dir="+filepath.Join(root, "profile")) && !slices.ContainsFunc(args, func(arg string) bool { return strings.HasPrefix(arg, "--type=") }) {
			if result.BrowserPID != 0 {
				result.Error = "duplicate owned Chrome browser processes"
				return result
			}
			result.BrowserPID = process.pid
		}
		if slices.Contains(args, "--type=renderer") {
			result.Renderers++
			status, _ := os.ReadFile(path + "status")
			// Sandboxed processes are non-dumpable, so /proc/PID/exe and ns
			// symlinks may be unreadable. NSpid is readable namespace evidence.
			namespaced := false
			for _, line := range strings.Split(string(status), "\n") {
				if strings.HasPrefix(line, "NSpid:") {
					namespaced = len(strings.Fields(line)) > 2
				}
			}
			seccomp := strings.Contains(string(status), "Seccomp:\t2\n")
			noNewPrivs := strings.Contains(string(status), "NoNewPrivs:\t1\n")
			result.SandboxStatus = fmt.Sprintf("seccomp-filter=%t no-new-privs=%t nested-pid-namespace=%t", seccomp, noNewPrivs, namespaced)
			if seccomp && noNewPrivs && namespaced {
				result.SandboxedRenderers++
			}
		}
	}
	return result
}

// Re-exec only: protocol stdout contains bounded observations, never raw trees,
// browser contents, profile data or command output. No host service activation.
func TestLAB95DesktopSupervisor(t *testing.T) {
	root := os.Getenv("SWAY_SESSION_LAB95_SUPERVISOR_ROOT")
	if root == "" {
		return
	}
	code := lab95RunSupervisor(root)
	os.Exit(code)
}

func lab95RunSupervisor(root string) int {
	if !strings.HasPrefix(root, "/tmp/") || filepath.Clean(root) != root {
		return 2
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return 2
	}
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return 2
	}
	_ = unix.Close(fd)
	log, err := os.OpenFile(filepath.Join(root, "lab95-chrome.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 2
	}
	defer log.Close()
	requests := make(chan lab95SupervisorRequest)
	go func() {
		defer close(requests)
		decoder := json.NewDecoder(io.LimitReader(os.Stdin, 128*1024))
		for {
			var request lab95SupervisorRequest
			if decoder.Decode(&request) != nil {
				return
			}
			requests <- request
		}
	}()
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(interrupt)
	encoder := json.NewEncoder(os.Stdout)
	starts := 0
	for {
		select {
		case request, ok := <-requests:
			if !ok {
				result := lab95CleanupChildren()
				_ = encoder.Encode(result)
				if result.Error != "" {
					return 1
				}
				return 0
			}
			lab95ReapChildren()
			result := lab95SupervisorResult{Starts: starts}
			if request.Inspect {
				result = lab95InspectChildren(root)
				result.Starts = starts
			} else if err := (&lab95GIOStarter{root: root}).validate(request.Spec); err != nil {
				result.Error = err.Error()
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				command := exec.CommandContext(ctx, "/usr/bin/gio", request.Spec.Arguments...)
				command.Env = append(os.Environ(), request.Spec.Environment...)
				command.Dir, command.Stdout, command.Stderr = root, log, log
				command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
				if err := command.Start(); err != nil {
					result.Error = err.Error()
				} else {
					starts++
					result.Starts = starts
					if err := command.Wait(); err != nil {
						result.Error = fmt.Sprintf("real GIO launch failed: %v (private log %s)", err, log.Name())
					}
				}
				cancel()
			}
			if encoder.Encode(result) != nil {
				_ = lab95CleanupChildren()
				return 1
			}
		case <-interrupt:
			result := lab95CleanupChildren()
			_ = encoder.Encode(result)
			if result.Error != "" {
				return 1
			}
			return 0
		}
	}
}
