package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSessionConfigDefaultsToAlacrittyWhenDefaultFileIsAbsent(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	t.Setenv("XDG_CONFIG_HOME", configHome)

	config, path, err := LoadSessionConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(configHome, "sway-session", "config.toml") ||
		config.Version != SessionConfigVersion || config.Terminal.Adapter != TerminalAdapterAlacritty {
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
		{name: "foot", contents: "version = 1\n[terminal]\nadapter = \"foot\"\n", want: TerminalAdapterFoot},
		{name: "unsupported", contents: "version = 1\n[terminal]\nadapter = \"sh -c\"\n", wantErr: true},
		{name: "unknown field", contents: "version = 1\n[terminal]\nadapter = \"alacritty\"\ncommand = \"sh\"\n", wantErr: true},
		{name: "future version", contents: "version = 2\n[terminal]\nadapter = \"alacritty\"\n", wantErr: true},
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
			if err != nil || gotPath != path || config.Terminal.Adapter != test.want {
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
