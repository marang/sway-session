package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marang/sway-title-animator/internal/herdrinit"
	sessionstate "github.com/marang/sway-title-animator/internal/session"
	"github.com/marang/sway-title-animator/internal/swayipc"
)

func TestTerminalCommandCreatesThenReusesOneAgentAddressableDefault(t *testing.T) {
	deps := testDependencies(t)
	client := &terminalCommandClient{id: testContextID}
	starter := &terminalCommandStarter{onStart: func() { client.setMapped(true) }}
	deps.newSwayClient = func(string) swayRequester { return client }
	deps.processStarter = starter

	first := runTerminalJSON(t, deps, "--json", "terminal", "--socket", "/run/user/1000/sway.sock")
	if first.Version != commandResultVersion || first.Terminal == nil || first.Terminal.ContextID != testContextID ||
		first.Terminal.Identity == nil || first.Terminal.Identity.Kind != sessionstate.TerminalIdentityDefault ||
		!reflect.DeepEqual(first.Terminal.Actions, []sessionstate.TerminalOpenAction{
			sessionstate.TerminalActionCreated, sessionstate.TerminalActionAttached, sessionstate.TerminalActionFocused,
		}) {
		t.Fatalf("unexpected first terminal result: %+v", first)
	}
	second := runTerminalJSON(t, deps, "terminal", "--json", "--socket", "/run/user/1000/sway.sock")
	if second.Terminal == nil || second.Terminal.ContextID != first.Terminal.ContextID ||
		!reflect.DeepEqual(second.Terminal.Actions, []sessionstate.TerminalOpenAction{
			sessionstate.TerminalActionReused, sessionstate.TerminalActionNoChange,
		}) {
		t.Fatalf("unexpected reused terminal result: %+v", second)
	}
	if len(starter.specs) != 1 {
		t.Fatalf("repeated terminal command started %d processes", len(starter.specs))
	}
}

func TestTerminalCommandNewCreatesIndependentAgentAddressableSessions(t *testing.T) {
	deps := testDependencies(t)
	client := &terminalCommandClient{id: testContextID}
	starter := &terminalCommandStarter{onStart: func() { client.setMapped(true) }}
	deps.newSwayClient = func(string) swayRequester { return client }
	deps.processStarter = starter
	ids := []sessionstate.ContextID{
		testContextID,
		"6ba7b810-9dad-41d1-80b4-00c04fd430c8",
	}
	nextID := 0
	deps.newContextID = func() (sessionstate.ContextID, error) {
		id := ids[nextID]
		nextID++
		return id, nil
	}

	first := runTerminalJSON(t, deps, "--json", "terminal", "--new", "--socket", "/run/user/1000/sway.sock")
	client.reset(ids[1])
	second := runTerminalJSON(t, deps, "--json", "terminal", "--new", "--socket", "/run/user/1000/sway.sock")
	for index, result := range []*commandResult{&first, &second} {
		if result.Terminal == nil || result.Terminal.ContextID != ids[index] || result.Terminal.Identity == nil ||
			result.Terminal.Identity.Kind != sessionstate.TerminalIdentityKind("instance") ||
			result.Terminal.Identity.ContextID != ids[index] ||
			result.Terminal.Session != "sway-terminal-"+strings.ReplaceAll(string(ids[index]), "-", "") ||
			!reflect.DeepEqual(result.Terminal.Actions, []sessionstate.TerminalOpenAction{
				sessionstate.TerminalActionCreated, sessionstate.TerminalActionAttached, sessionstate.TerminalActionFocused,
			}) {
			t.Fatalf("new terminal result %d is not independently addressable: %+v", index, result)
		}
	}
	if len(starter.specs) != 2 {
		t.Fatalf("two --new commands started %d processes", len(starter.specs))
	}
	registry := loadTestRegistry(t, deps)
	if len(registry.Contexts) != 2 || registry.Contexts[0].Launcher.Session == registry.Contexts[1].Launcher.Session {
		t.Fatalf("two --new commands did not persist unique sessions: %+v", registry)
	}
	listed := runTerminalJSON(t, deps, "--json", "terminal", "list")
	if listed.Terminals == nil || len(*listed.Terminals) != 2 {
		t.Fatalf("fresh terminals missing from inventory: %+v", listed)
	}
	for _, terminal := range *listed.Terminals {
		if terminal.Identity.Kind != sessionstate.TerminalIdentityKind("instance") || terminal.Identity.ContextID != terminal.ContextID {
			t.Fatalf("fresh terminal inventory lost instance identity: %+v", terminal)
		}
	}
}

func TestTerminalCommandNewInitializesRolesAndExactRetryConverges(t *testing.T) {
	deps := testDependencies(t)
	client := &terminalCommandClient{id: testContextID}
	starter := &terminalCommandStarter{onStart: func() { client.setMapped(true) }}
	deps.newSwayClient = func(string) swayRequester { return client }
	deps.processStarter = starter
	attempts := 0
	deps.initializeHerdr = func(_ context.Context, contextValue sessionstate.Context, roles []string, runner herdrinit.Runner) (herdrinit.Result, error) {
		attempts++
		if contextValue.ID != testContextID || !reflect.DeepEqual(roles, []string{"codex", "shell"}) || runner == nil {
			t.Fatalf("unexpected initialization context=%+v roles=%v runner=%T", contextValue, roles, runner)
		}
		result := herdrinit.Result{ContextID: contextValue.ID, Session: contextValue.Launcher.Session, Roles: append([]string(nil), roles...)}
		if attempts == 1 {
			return result, errors.New("agent trust required")
		}
		result.Initialized = true
		return result, nil
	}

	var firstOut bytes.Buffer
	var firstErr bytes.Buffer
	code := runWith([]string{
		"--json", "terminal", "--new", "--socket", "/run/user/1000/sway.sock",
		"--role", "codex", "--role", "shell",
	}, strings.NewReader(""), &firstOut, &firstErr, deps)
	if code != exitOperation || !strings.Contains(firstErr.String(), "agent trust required") {
		t.Fatalf("partial initialization code=%d stdout=%q stderr=%q", code, firstOut.String(), firstErr.String())
	}
	var partial commandResult
	if err := json.Unmarshal(firstOut.Bytes(), &partial); err != nil || partial.Terminal == nil ||
		partial.Terminal.ContextID != testContextID || partial.Terminal.Initialization == nil ||
		partial.Terminal.Initialization.Initialized {
		t.Fatalf("partial initialization lost retry identity: result=%+v err=%v", partial, err)
	}

	deps.newContextID = func() (sessionstate.ContextID, error) {
		t.Fatal("exact retry generated another context ID")
		return "", nil
	}
	retried := runTerminalJSON(t, deps,
		"--json", "terminal", "--context", string(testContextID), "--socket", "/run/user/1000/sway.sock",
		"--role", "codex", "--role", "shell",
	)
	if attempts != 2 || retried.Terminal == nil || retried.Terminal.Initialization == nil ||
		!retried.Terminal.Initialization.Initialized || retried.Terminal.ContextID != testContextID ||
		!reflect.DeepEqual(retried.Terminal.Actions, []sessionstate.TerminalOpenAction{
			sessionstate.TerminalActionReused, sessionstate.TerminalActionNoChange,
		}) {
		t.Fatalf("exact retry did not converge: attempts=%d result=%+v", attempts, retried)
	}
	if len(starter.specs) != 1 {
		t.Fatalf("exact retry launched %d terminals", len(starter.specs))
	}
}

func TestTerminalCommandProjectRecoversMissingWindowWithoutRestartingOccupiedAgent(t *testing.T) {
	deps := testDependencies(t)
	identity, err := sessionstate.ParseTerminalIdentity("LAB-112")
	if err != nil {
		t.Fatal(err)
	}
	contextValue := terminalInventoryContext(testContextID, identity, sessionstate.ContextActive, nil)
	contextValue.Launcher.Cwd = t.TempDir()
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := sessionstate.RegistryFile(root).Save(sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue},
	}); err != nil {
		t.Fatal(err)
	}
	client := &terminalCommandClient{id: contextValue.ID}
	deps.newSwayClient = func(string) swayRequester { return client }
	starter := &terminalCommandStarter{onStart: func() { client.setMapped(true) }}
	deps.processStarter = starter
	initializations := 0
	deps.initializeHerdr = func(_ context.Context, contextValue sessionstate.Context, roles []string, _ herdrinit.Runner) (herdrinit.Result, error) {
		initializations++
		return herdrinit.Result{
			ContextID: contextValue.ID, Session: contextValue.Launcher.Session, Roles: append([]string(nil), roles...),
			Reason: "Herdr session was not proven empty and was left unchanged",
		}, nil
	}

	result := runTerminalJSON(t, deps,
		"--json", "terminal", "--project", "LAB-112", "--socket", "/run/user/1000/sway.sock",
		"--role", "codex", "--role", "shell",
	)

	if result.Terminal == nil || result.Terminal.ContextID != contextValue.ID || result.Terminal.Session != contextValue.Launcher.Session ||
		result.Terminal.Initialization == nil || result.Terminal.Initialization.Initialized || initializations != 1 ||
		!reflect.DeepEqual(result.Terminal.Actions, []sessionstate.TerminalOpenAction{
			sessionstate.TerminalActionReused, sessionstate.TerminalActionAttached, sessionstate.TerminalActionFocused,
		}) {
		t.Fatalf("unexpected recovery result: %+v", result)
	}
	if len(starter.specs) != 1 {
		t.Fatalf("recovery launched %d terminal adapters", len(starter.specs))
	}
}

func TestTerminalCommandRejectsManagerRoleBeforeStateOrSway(t *testing.T) {
	deps := testDependencies(t)
	deps.stateRoot = func() (string, error) {
		t.Fatal("invalid role accessed session state")
		return "", nil
	}
	deps.newSwayClient = func(string) swayRequester {
		t.Fatal("invalid role opened Sway")
		return nil
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWith([]string{
		"--json", "terminal", "--new", "--socket", "/run/user/1000/sway.sock",
		"--role", "codex", "--role", "sh -c",
	}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitOperation || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"code":"terminal_roles"`) {
		t.Fatalf("invalid role code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestTerminalCommandNewRejectsConflictingModesWithoutEffects(t *testing.T) {
	for name, arguments := range map[string][]string{
		"project":                {"--json", "terminal", "--new", "--project", "LAB-106"},
		"project-equals-empty":   {"--json", "terminal", "--new", "--project="},
		"project-empty-argument": {"--json", "terminal", "--new", "--project", ""},
		"ephemeral":              {"--json", "terminal", "--new", "--ephemeral"},
		"ephemeral-false":        {"--json", "terminal", "--new", "--ephemeral=false"},
	} {
		t.Run(name, func(t *testing.T) {
			deps := testDependencies(t)
			deps.loadSessionConfig = func(string) (sessionstate.SessionConfig, string, error) {
				t.Fatal("invalid terminal mode loaded configuration")
				return sessionstate.SessionConfig{}, "", nil
			}
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := runWith(arguments, strings.NewReader(""), &stdout, &stderr, deps)
			if code != exitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"code":"usage"`) {
				t.Fatalf("invalid --new mode code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestTerminalCommandRejectsEmptyProjectWithoutEffects(t *testing.T) {
	deps := testDependencies(t)
	deps.loadSessionConfig = func(string) (sessionstate.SessionConfig, string, error) {
		t.Fatal("empty terminal project loaded configuration")
		return sessionstate.SessionConfig{}, "", nil
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWith([]string{"--json", "terminal", "--project="}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"code":"usage"`) {
		t.Fatalf("empty --project code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestTerminalCommandRejectsEmptyCwdWithoutEffects(t *testing.T) {
	deps := testDependencies(t)
	deps.homeDir = func() (string, error) {
		t.Fatal("empty terminal cwd resolved home")
		return "", nil
	}
	deps.loadSessionConfig = func(string) (sessionstate.SessionConfig, string, error) {
		t.Fatal("empty terminal cwd loaded configuration")
		return sessionstate.SessionConfig{}, "", nil
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWith([]string{"--json", "terminal", "--cwd="}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitOperation || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"code":"terminal_cwd"`) {
		t.Fatalf("empty --cwd code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestTerminalCommandRejectsInvalidLabelBeforeAnyDependency(t *testing.T) {
	for name, label := range map[string]string{
		"surrounding-whitespace": " terminal",
		"control-character":      "terminal\nname",
		"too-long":               strings.Repeat("t", 257),
	} {
		t.Run(name, func(t *testing.T) {
			deps := testDependencies(t)
			root, err := deps.stateRoot()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			registry := []byte(`{"version":5,"preferences":{"desktop_indicators":false},"contexts":[]}`)
			registryPath := filepath.Join(root, sessionstate.ContextsFilename)
			if err := os.WriteFile(registryPath, registry, 0o600); err != nil {
				t.Fatal(err)
			}
			deps.stateRoot = func() (string, error) {
				t.Fatal("invalid terminal label resolved state root")
				return "", nil
			}
			deps.loadSessionConfig = func(string) (sessionstate.SessionConfig, string, error) {
				t.Fatal("invalid terminal label loaded configuration")
				return sessionstate.SessionConfig{}, "", nil
			}
			deps.workingDir = func() (string, error) {
				t.Fatal("invalid terminal label resolved working directory")
				return "", nil
			}
			deps.newSwayClient = func(string) swayRequester {
				t.Fatal("invalid terminal label opened Sway")
				return nil
			}

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := runWith([]string{"--json", "terminal", "--label", label}, strings.NewReader(""), &stdout, &stderr, deps)
			if code != exitOperation || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"code":"terminal_label"`) {
				t.Fatalf("invalid --label code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			unchanged, err := os.ReadFile(registryPath)
			if err != nil || !bytes.Equal(unchanged, registry) {
				t.Fatalf("invalid label changed registry: data=%q err=%v", unchanged, err)
			}
		})
	}
}

func TestTerminalCommandRejectsControlCharacterCwdBeforeAnyDependency(t *testing.T) {
	deps := testDependencies(t)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	registry := []byte(`{"version":5,"preferences":{"desktop_indicators":false},"contexts":[]}`)
	registryPath := filepath.Join(root, sessionstate.ContextsFilename)
	if err := os.WriteFile(registryPath, registry, 0o600); err != nil {
		t.Fatal(err)
	}
	badCwd := filepath.Join(t.TempDir(), "terminal\nwork")
	if err := os.Mkdir(badCwd, 0o700); err != nil {
		t.Fatal(err)
	}
	deps.stateRoot = func() (string, error) {
		t.Fatal("invalid terminal cwd resolved state root")
		return "", nil
	}
	deps.loadSessionConfig = func(string) (sessionstate.SessionConfig, string, error) {
		t.Fatal("invalid terminal cwd loaded configuration")
		return sessionstate.SessionConfig{}, "", nil
	}
	deps.newSwayClient = func(string) swayRequester {
		t.Fatal("invalid terminal cwd opened Sway")
		return nil
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWith([]string{"--json", "terminal", "--new", "--cwd", badCwd, "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitOperation || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"code":"terminal_cwd"`) {
		t.Fatalf("invalid --cwd code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	unchanged, err := os.ReadFile(registryPath)
	if err != nil || !bytes.Equal(unchanged, registry) {
		t.Fatalf("invalid cwd changed registry: data=%q err=%v", unchanged, err)
	}
}

func TestTerminalCommandRejectsInvalidExplicitSocketWithoutEffects(t *testing.T) {
	t.Setenv("SWAYSOCK", "/run/user/1000/environment-sway.sock")
	for name, socket := range map[string]string{
		"empty":    "",
		"relative": "sway.sock",
		"unclean":  "/run/user/1000/../sway.sock",
	} {
		t.Run(name, func(t *testing.T) {
			deps := testDependencies(t)
			deps.loadSessionConfig = func(string) (sessionstate.SessionConfig, string, error) {
				t.Fatal("invalid explicit socket loaded configuration")
				return sessionstate.SessionConfig{}, "", nil
			}
			deps.stateRoot = func() (string, error) {
				t.Fatal("invalid explicit socket resolved state root")
				return "", nil
			}
			deps.newSwayClient = func(string) swayRequester {
				t.Fatal("invalid explicit socket opened Sway")
				return nil
			}

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := runWith([]string{"--json", "terminal", "--new", "--cwd", t.TempDir(), "--socket=" + socket}, strings.NewReader(""), &stdout, &stderr, deps)
			if code != exitOperation || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"code":"sway_socket"`) {
				t.Fatalf("invalid --socket code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestTerminalCommandEphemeralRejectsPersistentOptionsWithoutEffects(t *testing.T) {
	for name, arguments := range map[string][]string{
		"new":           {"--json", "terminal", "--ephemeral", "--new"},
		"new-false":     {"--json", "terminal", "--ephemeral", "--new=false"},
		"project":       {"--json", "terminal", "--ephemeral", "--project", "LAB-106"},
		"project-empty": {"--json", "terminal", "--ephemeral", "--project="},
		"label":         {"--json", "terminal", "--ephemeral", "--label", "temporary"},
		"socket":        {"--json", "terminal", "--ephemeral", "--socket", "/run/user/1000/sway.sock"},
	} {
		t.Run(name, func(t *testing.T) {
			deps := testDependencies(t)
			deps.loadSessionConfig = func(string) (sessionstate.SessionConfig, string, error) {
				t.Fatal("invalid ephemeral mode loaded configuration")
				return sessionstate.SessionConfig{}, "", nil
			}
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := runWith(arguments, strings.NewReader(""), &stdout, &stderr, deps)
			if code != exitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"code":"usage"`) {
				t.Fatalf("invalid ephemeral mode code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestTerminalCommandProjectIdentityRejectsConflictingCwd(t *testing.T) {
	deps := testDependencies(t)
	client := &terminalCommandClient{id: testContextID}
	starter := &terminalCommandStarter{onStart: func() { client.setMapped(true) }}
	deps.newSwayClient = func(string) swayRequester { return client }
	deps.processStarter = starter
	firstCwd := filepath.Join(t.TempDir(), "first")
	secondCwd := filepath.Join(t.TempDir(), "second")
	for _, path := range []string{firstCwd, secondCwd} {
		if err := mkdirPrivate(path); err != nil {
			t.Fatal(err)
		}
	}
	runTerminalJSON(t, deps, "--json", "terminal", "--project", "LAB-105", "--cwd", firstCwd, "--socket", "/run/user/1000/sway.sock")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWith([]string{"--json", "terminal", "--project", "LAB-105", "--cwd", secondCwd, "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitOperation || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"code":"terminal_identity_conflict"`) {
		t.Fatalf("conflicting project cwd result code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestTerminalCommandGlobalConfigUsesClosedAdapterAndEphemeralModeDoesNotPersist(t *testing.T) {
	deps := testDependencies(t)
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := writePrivate(configPath, "version = 2\n[terminal]\nadapter = \"foot\"\nsession_manager = \"herdr\"\n"); err != nil {
		t.Fatal(err)
	}
	starter := &terminalCommandStarter{}
	deps.processStarter = starter
	result := runTerminalJSON(t, deps, "--json", "--config", configPath, "terminal", "--ephemeral")
	if result.Terminal == nil || !result.Terminal.Ephemeral || result.Terminal.Adapter != sessionstate.TerminalAdapterFoot ||
		result.Terminal.Identity != nil ||
		!reflect.DeepEqual(result.Terminal.Actions, []sessionstate.TerminalOpenAction{sessionstate.TerminalActionOpened}) {
		t.Fatalf("unexpected ephemeral result: %+v", result)
	}
	if len(starter.specs) != 1 || starter.specs[0].Name != "/trusted/foot" {
		t.Fatalf("ephemeral adapter was not typed: %+v", starter.specs)
	}
	root, _ := deps.stateRoot()
	var registry sessionstate.Registry
	if err := sessionstate.RegistryFile(root).LoadInto(&registry); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ephemeral terminal unexpectedly created registry: %+v", registry)
	}
}

func TestTerminalCommandRejectsUnsupportedConfiguredAdapterWithoutEffects(t *testing.T) {
	deps := testDependencies(t)
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := writePrivate(configPath, "version = 2\n[terminal]\nadapter = \"kitty\"\nsession_manager = \"herdr\"\n"); err != nil {
		t.Fatal(err)
	}
	starter := &terminalCommandStarter{}
	deps.processStarter = starter
	deps.newSwayClient = func(string) swayRequester {
		t.Fatal("invalid adapter opened Sway")
		return nil
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWith([]string{"--json", "--config", configPath, "terminal", "--ephemeral"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitOperation || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"code":"terminal_config"`) {
		t.Fatalf("unsupported adapter code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(starter.specs) != 0 {
		t.Fatalf("unsupported adapter started a process: %+v", starter.specs)
	}
	root, _ := deps.stateRoot()
	if _, err := os.Stat(filepath.Join(root, sessionstate.ContextsFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported adapter changed registry state: %v", err)
	}
}

func TestTerminalListReturnsAnExplicitEmptyArray(t *testing.T) {
	result := runTerminalJSON(t, testDependencies(t), "--json", "terminal", "list")
	if result.Terminals == nil || len(*result.Terminals) != 0 {
		t.Fatalf("empty terminal inventory is not explicit: %+v", result)
	}
}

func TestTerminalInventoryRefusesUnsupportedSchemaWithoutMutation(t *testing.T) {
	deps := testDependencies(t)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	unsupported := []byte(`{"version":4,"preferences":{"desktop_indicators":false},"contexts":[]}`)
	path := filepath.Join(root, sessionstate.ContextsFilename)
	if err := os.WriteFile(path, unsupported, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWith([]string{"--json", "terminal", "list"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitOperation || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"code":"unsupported_version"`) ||
		!strings.Contains(stderr.String(), "schema version 4") {
		t.Fatalf("unsupported inventory code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(unchanged, unsupported) {
		t.Fatalf("read-only inventory modified unsupported registry: data=%q err=%v", unchanged, err)
	}
}

func TestTerminalCommandDoesNotReportRolledBackContextWhenLaunchFails(t *testing.T) {
	for name, mode := range map[string][]string{"reusable": {}, "fresh": {"--new"}} {
		t.Run(name, func(t *testing.T) {
			deps := testDependencies(t)
			deps.newSwayClient = func(string) swayRequester { return &terminalCommandClient{id: testContextID} }
			deps.processStarter = &terminalCommandStarter{err: errors.New("adapter failed")}
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			arguments := append([]string{"--json", "terminal"}, mode...)
			arguments = append(arguments, "--socket", "/run/user/1000/sway.sock")

			code := runWith(arguments, strings.NewReader(""), &stdout, &stderr, deps)
			if code != exitOperation || !strings.Contains(stderr.String(), `"code":"terminal_open"`) {
				t.Fatalf("launch failure code=%d stderr=%q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "was rolled back") {
				t.Fatalf("launch failure did not report registry rollback: %q", stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("rolled-back context was reported as active: %q", stdout.String())
			}
			if registry := loadTestRegistry(t, deps); len(registry.Contexts) != 0 {
				t.Fatalf("launch failure left an active context: %+v", registry.Contexts)
			}
		})
	}
}

func TestTerminalCommandDoesNotLaunchAnArchivedStableIdentity(t *testing.T) {
	deps := testDependencies(t)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	archivedAt := deps.now().UTC().Add(-time.Hour)
	identity := sessionstate.TerminalIdentity{Kind: sessionstate.TerminalIdentityDefault}
	archived := terminalInventoryContext(testContextID, identity, sessionstate.ContextArchived, &archivedAt)
	if err := sessionstate.RegistryFile(root).Save(sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{archived},
	}); err != nil {
		t.Fatal(err)
	}
	client := &terminalCommandClient{id: archived.ID}
	starter := &terminalCommandStarter{}
	deps.newSwayClient = func(string) swayRequester { return client }
	deps.processStarter = starter
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWith([]string{"--json", "terminal", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitOperation || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"code":"terminal_identity_archived"`) ||
		!strings.Contains(stderr.String(), string(archived.ID)) {
		t.Fatalf("archived terminal result code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(starter.specs) != 0 {
		t.Fatalf("archived terminal identity launched a process: %+v", starter.specs)
	}
}

func TestTerminalReconfigureChangesOnlyClosedArchivedAdapter(t *testing.T) {
	deps := testDependencies(t)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	archivedAt := deps.now().UTC().Add(-time.Hour)
	identity := sessionstate.TerminalIdentity{Kind: sessionstate.TerminalIdentityProject, Project: "LAB-105"}
	archived := terminalInventoryContext(testContextID, identity, sessionstate.ContextArchived, &archivedAt)
	if err := sessionstate.RegistryFile(root).Save(sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{archived},
	}); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := writePrivate(configPath, "version = 2\n[terminal]\nadapter = \"foot\"\nsession_manager = \"herdr\"\n"); err != nil {
		t.Fatal(err)
	}
	deps.newSwayClient = func(string) swayRequester { return &terminalCommandClient{id: archived.ID} }
	result := runTerminalJSON(t, deps, "--json", "--config", configPath, "terminal", "reconfigure", "--project", "LAB-105", "--socket", "/run/user/1000/sway.sock")
	if !reflect.DeepEqual(result.Actions, []string{"reconfigured"}) || result.Terminals == nil || len(*result.Terminals) != 1 ||
		(*result.Terminals)[0].Adapter != sessionstate.TerminalAdapterFoot {
		t.Fatalf("unexpected adapter reconfigure result: %+v", result)
	}
	var registry sessionstate.Registry
	if err := sessionstate.RegistryFile(root).LoadInto(&registry); err != nil {
		t.Fatal(err)
	}
	changed := registry.Contexts[0]
	if changed.ID != archived.ID || changed.Launcher.Session != archived.Launcher.Session || changed.Launcher.Cwd != archived.Launcher.Cwd ||
		changed.ArchivedAt == nil || changed.Launcher.Terminal.Adapter != sessionstate.TerminalAdapterFoot {
		t.Fatalf("adapter reconfigure changed identity or history metadata: before=%+v after=%+v", archived, changed)
	}
}

func TestTerminalReconfigureReportsMappedArchivedIdentityAsInUse(t *testing.T) {
	deps := testDependencies(t)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	archivedAt := deps.now().UTC().Add(-time.Hour)
	identity := sessionstate.TerminalIdentity{Kind: sessionstate.TerminalIdentityDefault}
	archived := terminalInventoryContext(testContextID, identity, sessionstate.ContextArchived, &archivedAt)
	if err := sessionstate.RegistryFile(root).Save(sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{archived},
	}); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := writePrivate(configPath, "version = 2\n[terminal]\nadapter = \"foot\"\nsession_manager = \"herdr\"\n"); err != nil {
		t.Fatal(err)
	}
	deps.newSwayClient = func(string) swayRequester {
		return &terminalCommandClient{id: archived.ID, mapped: true}
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWith([]string{"--json", "--config", configPath, "terminal", "reconfigure", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitOperation || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"code":"terminal_adapter_in_use"`) ||
		!strings.Contains(stderr.String(), string(archived.ID)) {
		t.Fatalf("mapped reconfigure code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestTerminalCommandReportsActionableAdapterMismatch(t *testing.T) {
	deps := testDependencies(t)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	identity := sessionstate.TerminalIdentity{Kind: sessionstate.TerminalIdentityDefault}
	active := terminalInventoryContext(testContextID, identity, sessionstate.ContextActive, nil)
	if err := sessionstate.RegistryFile(root).Save(sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{active},
	}); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := writePrivate(configPath, "version = 2\n[terminal]\nadapter = \"foot\"\nsession_manager = \"herdr\"\n"); err != nil {
		t.Fatal(err)
	}
	deps.newSwayClient = func(string) swayRequester { return &terminalCommandClient{id: active.ID} }
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWith([]string{"--json", "--config", configPath, "terminal", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitOperation || !strings.Contains(stderr.String(), `"code":"terminal_adapter_conflict"`) ||
		!strings.Contains(stderr.String(), string(active.ID)) {
		t.Fatalf("adapter mismatch code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestTerminalListStatusAndCleanupAreReadOnlyAgentInventory(t *testing.T) {
	deps := testDependencies(t)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	archivedAt := deps.now().UTC().Add(-48 * time.Hour)
	defaultIdentity := sessionstate.TerminalIdentity{Kind: sessionstate.TerminalIdentityDefault}
	projectIdentity := sessionstate.TerminalIdentity{Kind: sessionstate.TerminalIdentityProject, Project: "LAB-105"}
	active := terminalInventoryContext(testContextID, defaultIdentity, sessionstate.ContextActive, nil)
	archived := terminalInventoryContext("22222222-2222-4222-8222-222222222222", projectIdentity, sessionstate.ContextArchived, &archivedAt)
	if err := sessionstate.RegistryFile(root).Save(sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{archived, active},
	}); err != nil {
		t.Fatal(err)
	}
	deps.newSwayClient = func(string) swayRequester {
		t.Fatal("read-only terminal inventory opened Sway")
		return nil
	}

	listed := runTerminalJSON(t, deps, "--json", "terminal", "list")
	if listed.Terminals == nil || len(*listed.Terminals) != 2 || (*listed.Terminals)[0].ContextID != active.ID || (*listed.Terminals)[1].ContextID != archived.ID {
		t.Fatalf("terminal list is not stable and sorted: %+v", listed.Terminals)
	}
	status := runTerminalJSON(t, deps, "--json", "terminal", "status")
	if status.Terminals == nil || len(*status.Terminals) != 1 || (*status.Terminals)[0].ContextID != active.ID {
		t.Fatalf("default terminal status selected wrong context: %+v", status.Terminals)
	}
	projectStatus := runTerminalJSON(t, deps, "--json", "terminal", "status", "--project", "LAB-105")
	if projectStatus.Terminals == nil || len(*projectStatus.Terminals) != 1 || (*projectStatus.Terminals)[0].ContextID != archived.ID {
		t.Fatalf("project terminal status selected wrong context: %+v", projectStatus.Terminals)
	}
	cleanup := runTerminalJSON(t, deps, "--json", "terminal", "cleanup", "--archived-before", deps.now().UTC().Add(-24*time.Hour).Format("2006-01-02"))
	if !cleanup.Preview || cleanup.Terminals == nil || len(*cleanup.Terminals) != 1 || (*cleanup.Terminals)[0].ContextID != archived.ID {
		t.Fatalf("cleanup preview is not machine-readable: %+v", cleanup)
	}
	if got := loadTestRegistry(t, deps); !reflect.DeepEqual(got.Contexts, []sessionstate.Context{archived, active}) {
		t.Fatalf("inventory mutated registry: %+v", got)
	}
}

func terminalInventoryContext(id sessionstate.ContextID, identity sessionstate.TerminalIdentity, state sessionstate.ContextState, archivedAt *time.Time) sessionstate.Context {
	return sessionstate.Context{
		ID: id, Label: string(identity.Kind), State: state, ArchivedAt: archivedAt,
		Launcher: sessionstate.Launcher{
			Kind: sessionstate.LauncherHerdr, Session: "terminal-" + string(id)[:8], Cwd: "/tmp",
			Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty, Identity: &identity},
		},
	}
}

func runTerminalJSON(t *testing.T, deps dependencies, arguments ...string) commandResult {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runWith(arguments, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("terminal command %v failed code=%d stdout=%q stderr=%q", arguments, code, stdout.String(), stderr.String())
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode terminal result: %v\n%s", err, stdout.String())
	}
	return result
}

type terminalCommandStarter struct {
	mu      sync.Mutex
	specs   []sessionstate.ProcessSpec
	onStart func()
	err     error
}

func (starter *terminalCommandStarter) Start(spec sessionstate.ProcessSpec) error {
	starter.mu.Lock()
	starter.specs = append(starter.specs, spec)
	starter.mu.Unlock()
	if starter.onStart != nil {
		starter.onStart()
	}
	return starter.err
}

type terminalCommandClient struct {
	mu      sync.Mutex
	id      sessionstate.ContextID
	mapped  bool
	focused bool
}

func (client *terminalCommandClient) setMapped(focused bool) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.mapped = true
	client.focused = focused
}

func (client *terminalCommandClient) reset(id sessionstate.ContextID) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.id = id
	client.mapped = false
	client.focused = false
}

func (client *terminalCommandClient) Request(messageType swayipc.MessageType, _ []byte) (swayipc.Message, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	switch messageType {
	case swayipc.GetTree:
		root := &swayipc.TreeNode{ID: 1, Type: "root", Nodes: []*swayipc.TreeNode{{ID: 2, Type: "workspace", Name: "98"}}}
		if client.mapped {
			appID, _ := client.id.AppID()
			root.Nodes[0].Nodes = []*swayipc.TreeNode{{ID: 42, Type: "con", AppID: &appID, Focused: client.focused}}
		}
		payload, _ := json.Marshal(root)
		return swayipc.Message{Type: swayipc.GetTree, Payload: payload}, nil
	case swayipc.RunCommand:
		client.focused = true
		return swayipc.Message{Type: swayipc.RunCommand, Payload: []byte(`[{"success":true}]`)}, nil
	default:
		return swayipc.Message{}, errors.New("unexpected Sway request")
	}
}

func (client *terminalCommandClient) RequestContext(ctx context.Context, messageType swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if err := ctx.Err(); err != nil {
		return swayipc.Message{}, err
	}
	return client.Request(messageType, payload)
}

func (client *terminalCommandClient) Close() {}

func mkdirPrivate(path string) error {
	return os.MkdirAll(path, 0o700)
}

func writePrivate(path string, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o600)
}
