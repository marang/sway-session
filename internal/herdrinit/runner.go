package herdrinit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
)

const (
	commandTimeout = 40 * time.Second
	outputLimit    = 64 * 1024
)

type UserPaths struct {
	Home       string
	Name       string
	UID        int
	ConfigHome string
	StateHome  string
	RuntimeDir string
}

// CurrentUserPaths derives the helper's filesystem boundary from the account
// database instead of caller-controlled HOME and XDG environment variables.
func CurrentUserPaths() (UserPaths, error) {
	uid := os.Getuid()
	account, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return UserPaths{}, fmt.Errorf("resolve current account: %w", err)
	}
	home := filepath.Clean(account.HomeDir)
	if !filepath.IsAbs(home) || home == string(filepath.Separator) {
		return UserPaths{}, errors.New("current account has an unsafe home directory")
	}
	name := account.Username
	if name == "" || strings.ContainsAny(name, "\x00\r\n") {
		return UserPaths{}, errors.New("current account has an unsafe username")
	}
	return UserPaths{
		Home:       home,
		Name:       name,
		UID:        uid,
		ConfigHome: filepath.Join(home, ".config"),
		StateHome:  filepath.Join(home, ".local", "state"),
		RuntimeDir: filepath.Join("/run/user", strconv.Itoa(uid)),
	}, nil
}

func (paths UserPaths) SessionStateRoot() string {
	return filepath.Join(paths.StateHome, "sway-session")
}

func ResolveSystemExecutable(name string) (string, error) {
	return sessionstate.ResolveRootOwnedSystemExecutable(name)
}

type ExecRunner struct {
	Executable string
	User       UserPaths
}

func (runner ExecRunner) Run(ctx context.Context, session string, cwd string, arguments ...string) ([]byte, error) {
	if runner.Executable == "" || !filepath.IsAbs(runner.Executable) {
		return nil, errors.New("herdr executable must be absolute")
	}
	if !filepath.IsAbs(cwd) || filepath.Clean(cwd) != cwd {
		return nil, errors.New("herdr working directory must be a clean absolute path")
	}
	if session == "" || strings.HasPrefix(session, "-") || strings.ContainsAny(session, "\x00\r\n") {
		return nil, errors.New("herdr session name is unsafe")
	}
	if len(arguments) == 0 {
		return nil, errors.New("herdr command is required")
	}

	commandContext, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	commandArguments := make([]string, 0, len(arguments)+2)
	commandArguments = append(commandArguments, "--session", session)
	commandArguments = append(commandArguments, arguments...)
	command := exec.CommandContext(commandContext, runner.Executable, commandArguments...)
	command.Dir = cwd
	command.Env = runner.environment()
	stdout := &limitedBuffer{limit: outputLimit}
	stderr := &limitedBuffer{limit: outputLimit}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("%s: %w", detail, err)
		}
		return nil, err
	}
	if stdout.overflow {
		return nil, fmt.Errorf("herdr output exceeds %d bytes", outputLimit)
	}
	return stdout.Bytes(), nil
}

func (runner ExecRunner) environment() []string {
	return []string{
		"HOME=" + runner.User.Home,
		"USER=" + runner.User.Name,
		"LOGNAME=" + runner.User.Name,
		"PATH=/usr/bin",
		"LANG=C.UTF-8",
		"NO_COLOR=1",
		"XDG_CONFIG_HOME=" + runner.User.ConfigHome,
		"XDG_STATE_HOME=" + runner.User.StateHome,
		"XDG_RUNTIME_DIR=" + runner.User.RuntimeDir,
	}
}

type limitedBuffer struct {
	data     []byte
	limit    int
	overflow bool
}

func (buffer *limitedBuffer) Write(value []byte) (int, error) {
	written := len(value)
	remaining := buffer.limit - len(buffer.data)
	if remaining > 0 {
		if len(value) > remaining {
			buffer.overflow = true
			value = value[:remaining]
		}
		buffer.data = append(buffer.data, value...)
	} else if len(value) > 0 {
		buffer.overflow = true
	}
	return written, nil
}

func (buffer *limitedBuffer) Bytes() []byte  { return append([]byte(nil), buffer.data...) }
func (buffer *limitedBuffer) String() string { return string(buffer.data) }

var _ io.Writer = (*limitedBuffer)(nil)
