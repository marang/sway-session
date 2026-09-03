package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type terminalEnvironmentProbe struct {
	NoColorPresent bool   `json:"no_color_present"`
	Term           string `json:"term"`
	ColorTerm      string `json:"colorterm"`
	Sentinel       string `json:"sentinel"`
	ForceColor     string `json:"force_color"`
	CLIColor       string `json:"clicolor"`
	CLIColorForce  string `json:"clicolor_force"`
	ContextID      string `json:"context_id"`
	HerdrConfig    string `json:"herdr_config"`
}

func TestPersistentTerminalDoesNotExposeCallerNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM", "alacritty")
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("SWAY_SESSION_TEST_SENTINEL", "preserved")
	t.Setenv("FORCE_COLOR", "3")
	t.Setenv("CLICOLOR", "1")
	t.Setenv("CLICOLOR_FORCE", "1")

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contextValue := testValidContext(testContextID)
	contextValue.Launcher.Cwd = t.TempDir()
	spec, err := BuildTerminalProcessSpec(contextValue, executable, "/usr/bin/herdr", "/tmp/herdr-config.toml")
	if err != nil {
		t.Fatal(err)
	}
	probePath := filepath.Join(t.TempDir(), "environment.json")
	spec.Arguments = []string{"-test.run=^TestTerminalEnvironmentProbeHelper$"}
	spec.Environment = append(spec.Environment,
		"SWAY_SESSION_ENV_PROBE=1",
		"SWAY_SESSION_ENV_PROBE_PATH="+probePath,
	)
	if err := (ExecProcessStarter{}).Start(spec); err != nil {
		t.Fatal(err)
	}

	probe := waitForTerminalEnvironmentProbe(t, probePath)
	if probe.NoColorPresent {
		t.Fatal("persistent terminal inherited caller-only NO_COLOR")
	}
	if probe.Term != "alacritty" || probe.ColorTerm != "truecolor" || probe.Sentinel != "preserved" {
		t.Fatalf("persistent terminal lost ordinary environment: %+v", probe)
	}
	if probe.ForceColor != "3" || probe.CLIColor != "1" || probe.CLIColorForce != "1" {
		t.Fatalf("persistent terminal changed unrelated color controls: %+v", probe)
	}
	if probe.ContextID != string(contextValue.ID) || probe.HerdrConfig != "/tmp/herdr-config.toml" {
		t.Fatalf("persistent terminal lost trusted context environment: %+v", probe)
	}
}

func TestEphemeralTerminalDoesNotExposeCallerNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM", "alacritty")
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("SWAY_SESSION_TEST_SENTINEL", "preserved")
	t.Setenv("FORCE_COLOR", "3")
	t.Setenv("CLICOLOR", "1")
	t.Setenv("CLICOLOR_FORCE", "1")

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spec, err := BuildEphemeralTerminalProcessSpec(TerminalAdapterAlacritty, t.TempDir(), executable)
	if err != nil {
		t.Fatal(err)
	}
	probePath := filepath.Join(t.TempDir(), "environment.json")
	spec.Arguments = []string{"-test.run=^TestTerminalEnvironmentProbeHelper$"}
	spec.Environment = append(spec.Environment,
		"SWAY_SESSION_ENV_PROBE=1",
		"SWAY_SESSION_ENV_PROBE_PATH="+probePath,
	)
	if err := (ExecProcessStarter{}).Start(spec); err != nil {
		t.Fatal(err)
	}

	probe := waitForTerminalEnvironmentProbe(t, probePath)
	if probe.NoColorPresent {
		t.Fatal("ephemeral terminal inherited caller-only NO_COLOR")
	}
	if probe.Term != "alacritty" || probe.ColorTerm != "truecolor" || probe.Sentinel != "preserved" {
		t.Fatalf("ephemeral terminal lost ordinary environment: %+v", probe)
	}
	if probe.ForceColor != "3" || probe.CLIColor != "1" || probe.CLIColorForce != "1" {
		t.Fatalf("ephemeral terminal changed unrelated color controls: %+v", probe)
	}
	if probe.ContextID != "" || probe.HerdrConfig != "" {
		t.Fatalf("ephemeral terminal inherited persistent context environment: %+v", probe)
	}
}

func TestTerminalEnvironmentProbeHelper(t *testing.T) {
	if os.Getenv("SWAY_SESSION_ENV_PROBE") != "1" {
		return
	}
	_, noColorPresent := os.LookupEnv("NO_COLOR")
	data, err := json.Marshal(terminalEnvironmentProbe{
		NoColorPresent: noColorPresent,
		Term:           os.Getenv("TERM"),
		ColorTerm:      os.Getenv("COLORTERM"),
		Sentinel:       os.Getenv("SWAY_SESSION_TEST_SENTINEL"),
		ForceColor:     os.Getenv("FORCE_COLOR"),
		CLIColor:       os.Getenv("CLICOLOR"),
		CLIColorForce:  os.Getenv("CLICOLOR_FORCE"),
		ContextID:      os.Getenv("SWAY_SESSION_CONTEXT_ID"),
		HerdrConfig:    os.Getenv("HERDR_CONFIG_PATH"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("SWAY_SESSION_ENV_PROBE_PATH"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitForTerminalEnvironmentProbe(t *testing.T, path string) terminalEnvironmentProbe {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var probe terminalEnvironmentProbe
			if err := json.Unmarshal(data, &probe); err != nil {
				t.Fatal(err)
			}
			return probe
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("terminal environment probe %s was not written", path)
	return terminalEnvironmentProbe{}
}

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
				!reflect.DeepEqual(spec.UnsetInheritedEnvironment, []string{"CODEX_THREAD_ID", "NO_COLOR"}) ||
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
		if !reflect.DeepEqual(spec.UnsetInheritedEnvironment, []string{"CODEX_THREAD_ID", "NO_COLOR", "SWAY_SESSION_CONTEXT_ID"}) ||
			!reflect.DeepEqual(spec.UnsetInheritedEnvironmentPrefixes, []string{"HERDR_"}) {
			t.Fatalf("unsafe ephemeral terminal environment: %+v", spec)
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
