package session

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"
)

const (
	SessionConfigVersion = 1
	maxSessionConfigSize = 64 * 1024
)

type SessionConfig struct {
	Version  int                   `toml:"version"`
	Terminal SessionTerminalConfig `toml:"terminal"`
}

type SessionTerminalConfig struct {
	Adapter TerminalAdapter `toml:"adapter"`
}

func DefaultSessionConfig() SessionConfig {
	return SessionConfig{
		Version: SessionConfigVersion,
		Terminal: SessionTerminalConfig{
			Adapter: TerminalAdapterAlacritty,
		},
	}
}

// DefaultSessionConfigPath returns sway-session's versioned typed launcher
// configuration. It is separate from animator configuration and state.
func DefaultSessionConfigPath() (string, error) {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(configHome) {
		return "", errors.New("XDG_CONFIG_HOME must be an absolute path")
	}
	return filepath.Join(filepath.Clean(configHome), "sway-session", "config.toml"), nil
}

// LoadSessionConfig loads a strict typed config. An empty path selects the
// default location and treats an absent file as the compiled Alacritty default;
// an explicit path must exist.
func LoadSessionConfig(path string) (SessionConfig, string, error) {
	explicit := path != ""
	if !explicit {
		var err error
		path, err = DefaultSessionConfigPath()
		if err != nil {
			return SessionConfig{}, "", err
		}
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return SessionConfig{}, path, errors.New("session config path must be a clean absolute path")
	}
	data, err := readSessionConfig(path)
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return DefaultSessionConfig(), path, nil
	}
	if err != nil {
		return SessionConfig{}, path, err
	}
	config := SessionConfig{}
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return SessionConfig{}, path, fmt.Errorf("decode session config: %w", err)
	}
	if err := config.Validate(); err != nil {
		return SessionConfig{}, path, err
	}
	return config, path, nil
}

func (config SessionConfig) Validate() error {
	if config.Version != SessionConfigVersion {
		return fmt.Errorf("unsupported session config version %d; expected %d", config.Version, SessionConfigVersion)
	}
	if _, err := TerminalAdapterExecutableName(config.Terminal.Adapter); err != nil {
		return err
	}
	return nil
}

func readSessionConfig(path string) ([]byte, error) {
	fd, err := openNoSymlinks(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open session config %s: %w", path, err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("inspect session config: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		_ = unix.Close(fd)
		return nil, errors.New("session config must be a single-link regular file")
	}
	if stat.Size < 0 || stat.Size > maxSessionConfigSize {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("session config exceeds %d bytes", maxSessionConfigSize)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create session config file handle")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxSessionConfigSize+1))
	if err != nil {
		return nil, fmt.Errorf("read session config: %w", err)
	}
	if len(data) > maxSessionConfigSize {
		return nil, fmt.Errorf("session config exceeds %d bytes", maxSessionConfigSize)
	}
	return data, nil
}
