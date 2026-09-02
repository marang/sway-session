package session

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBuildTerminalProcessSpecUsesOnlyTypedAdapterArguments(t *testing.T) {
	context := testValidContext(testContextID)
	context.Label = "Agent; $(touch never)"
	context.Launcher.Cwd = t.TempDir()
	appID, err := context.ID.AppID()
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name       string
		adapter    TerminalAdapter
		executable string
		arguments  []string
	}{
		{
			name:       "alacritty",
			adapter:    TerminalAdapterAlacritty,
			executable: "/usr/bin/alacritty",
			arguments: []string{
				"--class=" + appID,
				"--working-directory=" + context.Launcher.Cwd,
				"--title=Agent; $(touch never)",
				"-e", "/usr/bin/herdr", "--session", context.Launcher.Session,
			},
		},
		{
			name:       "foot",
			adapter:    TerminalAdapterFoot,
			executable: "/usr/bin/foot",
			arguments: []string{
				"--app-id=" + appID,
				"--working-directory=" + context.Launcher.Cwd,
				"--title=Agent; $(touch never)",
				"--", "/usr/bin/herdr", "--session", context.Launcher.Session,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			context.Launcher.Terminal = &TerminalLauncher{Adapter: test.adapter}
			spec, err := BuildTerminalProcessSpec(context, test.executable, "/usr/bin/herdr", "/home/test/.config/herdr/custom.toml")
			if err != nil {
				t.Fatal(err)
			}
			if spec.Name != test.executable || !reflect.DeepEqual(spec.Arguments, test.arguments) {
				t.Fatalf("unexpected process spec: got=%+v want name=%q arguments=%q", spec, test.executable, test.arguments)
			}
			if !reflect.DeepEqual(spec.Environment, []string{
				"SWAY_SESSION_CONTEXT_ID=" + string(context.ID),
				"HERDR_CONFIG_PATH=/home/test/.config/herdr/custom.toml",
			}) ||
				!reflect.DeepEqual(spec.UnsetInheritedEnvironment, []string{"CODEX_THREAD_ID"}) ||
				!reflect.DeepEqual(spec.UnsetInheritedEnvironmentPrefixes, []string{"HERDR_"}) {
				t.Fatalf("unsafe terminal environment: %+v", spec)
			}
		})
	}
}

func TestBuildEphemeralTerminalProcessSpecNeverAddsACommandTemplate(t *testing.T) {
	directory := t.TempDir()
	for _, test := range []struct {
		adapter    TerminalAdapter
		executable string
		arguments  []string
	}{
		{TerminalAdapterAlacritty, "/usr/bin/alacritty", []string{"--working-directory=" + directory}},
		{TerminalAdapterFoot, "/usr/bin/foot", []string{"--working-directory=" + directory}},
	} {
		spec, err := BuildEphemeralTerminalProcessSpec(test.adapter, directory, test.executable)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Name != test.executable || !reflect.DeepEqual(spec.Arguments, test.arguments) {
			t.Fatalf("unexpected ephemeral process spec: got=%+v want=%q", spec, test.arguments)
		}
	}
}

func TestTerminalAdapterExecutableNameIsClosedAndDeterministic(t *testing.T) {
	for adapter, want := range map[TerminalAdapter]string{
		TerminalAdapterAlacritty: "alacritty",
		TerminalAdapterFoot:      "foot",
	} {
		got, err := TerminalAdapterExecutableName(adapter)
		if err != nil || got != want {
			t.Fatalf("adapter %q resolved to %q: %v", adapter, got, err)
		}
	}
	if _, err := TerminalAdapterExecutableName("sh -c"); err == nil {
		t.Fatal("generic executable template was accepted")
	}
}

func TestFindTerminalAdapterProcessesUsesStableAppIDWithoutOldBinary(t *testing.T) {
	procRoot := t.TempDir()
	appID, err := testContextID.AppID()
	if err != nil {
		t.Fatal(err)
	}
	processes := map[string][]byte{
		"8101": []byte("/removed/alacritty\x00--class=" + appID + "\x00--title=test\x00"),
		"8102": []byte("/usr/bin/foot\x00--app-id=other\x00"),
		"8103": []byte("/removed/foot\x00--app-id=" + appID + "\x00"),
	}
	for pid, cmdline := range processes {
		directory := filepath.Join(procRoot, pid)
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "cmdline"), cmdline, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	matches, err := FindTerminalAdapterProcesses(procRoot, testContextID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(matches, []int{8101, 8103}) {
		t.Fatalf("stable terminal process matches=%v", matches)
	}
}
