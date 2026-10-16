package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLoadSessionConfigDefaultsToAlacrittyWhenDefaultFileIsAbsent(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	t.Setenv("XDG_CONFIG_HOME", configHome)

	config, path, err := LoadSessionConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(configHome, "sway-session", "config.toml") ||
		config.Version != SessionConfigVersion || config.Terminal.Adapter != TerminalAdapterAlacritty ||
		config.Terminal.SessionManager != TerminalSessionManagerHerdr {
		t.Fatalf("unexpected default config path=%q config=%+v", path, config)
	}
}

func TestLoadSessionConfigUsesStrictVersionedTypedAdapter(t *testing.T) {
	for _, test := range []struct {
		name     string
		contents string
		want     TerminalAdapter
		wantErr  bool
	}{
		{name: "foot", contents: "version = 2\n[terminal]\nadapter = \"foot\"\nsession_manager = \"herdr\"\n", want: TerminalAdapterFoot},
		{name: "unsupported adapter", contents: "version = 2\n[terminal]\nadapter = \"sh -c\"\nsession_manager = \"herdr\"\n", wantErr: true},
		{name: "unsupported manager", contents: "version = 2\n[terminal]\nadapter = \"alacritty\"\nsession_manager = \"sh -c\"\n", wantErr: true},
		{name: "missing manager", contents: "version = 2\n[terminal]\nadapter = \"alacritty\"\n", wantErr: true},
		{name: "unknown field", contents: "version = 2\n[terminal]\nadapter = \"alacritty\"\nsession_manager = \"herdr\"\ncommand = \"sh\"\n", wantErr: true},
		{name: "old version", contents: "version = 1\n[terminal]\nadapter = \"alacritty\"\nsession_manager = \"herdr\"\n", wantErr: true},
		{name: "future version", contents: "version = 3\n[terminal]\nadapter = \"alacritty\"\nsession_manager = \"herdr\"\n", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			config, gotPath, err := LoadSessionConfig(path)
			if test.wantErr {
				if err == nil {
					t.Fatalf("unsafe config accepted: %+v", config)
				}
				return
			}
			if err != nil || gotPath != path || config.Terminal.Adapter != test.want || config.Terminal.SessionManager != TerminalSessionManagerHerdr {
				t.Fatalf("load config path=%q config=%+v err=%v", gotPath, config, err)
			}
		})
	}
}

func TestLoadSessionConfigExplicitPathMustExistAndBeRegular(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.toml")
	if _, _, err := LoadSessionConfig(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit missing config did not report not-exist: %v", err)
	}

	directory := t.TempDir()
	if _, _, err := LoadSessionConfig(directory); err == nil {
		t.Fatal("directory was accepted as session config")
	}
}

func TestLoadSessionConfigFollowsFilesystemLinks(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "directory symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			sourceDirectory := filepath.Join(root, "dotfiles", "sway-session")
			if err := os.MkdirAll(sourceDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(sourceDirectory, "config.toml")
			contents := []byte("version = 2\n[terminal]\nadapter = \"foot\"\nsession_manager = \"herdr\"\n")
			if err := os.WriteFile(source, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "config.toml")
			switch kind {
			case "symlink":
				if err := os.Symlink(source, path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(source, path); err != nil {
					t.Fatal(err)
				}
			case "directory symlink":
				configHome := filepath.Join(root, "config")
				if err := os.Symlink(filepath.Dir(sourceDirectory), configHome); err != nil {
					t.Fatal(err)
				}
				t.Setenv("XDG_CONFIG_HOME", configHome)
				path = ""
			}
			config, _, err := LoadSessionConfig(path)
			if err != nil || config.Terminal.Adapter != TerminalAdapterFoot || config.Terminal.SessionManager != TerminalSessionManagerHerdr {
				t.Fatalf("linked config = %+v, err = %v", config, err)
			}
		})
	}
}

func TestLoadSessionConfigRejectsFIFOWithoutBlocking(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "direct"
		if linked {
			name = "symlink"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "config.toml")
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			if linked {
				link := filepath.Join(root, "linked.toml")
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				path = link
			}
			done := make(chan error, 1)
			go func() {
				_, _, err := LoadSessionConfig(path)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("FIFO was accepted as configuration")
				}
			case <-time.After(time.Second):
				t.Fatal("configuration read blocked on FIFO")
			}
		})
	}
}

func TestLoadSessionConfigRejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, make([]byte, maxSessionConfigSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadSessionConfig(path); err == nil {
		t.Fatal("oversized configuration was accepted")
	}
}
