package herdrinit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	commandTimeout = 40 * time.Second
	outputLimit    = 64 * 1024
)

type ExecRunner struct {
	Executable string
	ConfigFile string
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
	environment, err := runner.environment()
	if err != nil {
		return nil, err
	}
	command.Env = environment
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

func (runner ExecRunner) environment() ([]string, error) {
	if !filepath.IsAbs(runner.ConfigFile) || filepath.Clean(runner.ConfigFile) != runner.ConfigFile || strings.ContainsAny(runner.ConfigFile, "\x00\r\n") {
		return nil, errors.New("herdr config path must be a clean absolute path")
	}
	environment := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		name, _, found := strings.Cut(value, "=")
		if !found || name == "CODEX_THREAD_ID" || strings.HasPrefix(name, "HERDR_") || strings.HasPrefix(name, "LD_") {
			continue
		}
		environment = append(environment, value)
	}
	return append(environment, "HERDR_CONFIG_PATH="+runner.ConfigFile), nil
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
