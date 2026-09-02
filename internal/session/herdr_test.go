package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultHerdrPathsKeepsDataRootSeparateFromConfigOverride(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	t.Setenv("HERDR_CONFIG_PATH", filepath.Join(base, "custom.toml"))

	paths, err := DefaultHerdrPaths()
	if err != nil {
		t.Fatal(err)
	}
	if paths.Root != filepath.Join(base, "herdr") || paths.ConfigFile != filepath.Join(base, "custom.toml") {
		t.Fatalf("unexpected Herdr paths: %+v", paths)
	}
}

func TestValidateHerdrSessionSocketPathsChecksBothLinuxSocketNames(t *testing.T) {
	sessionName, err := DeriveTerminalInstanceSessionName(testContextID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateHerdrSessionSocketPaths("/home/example/.config/herdr", sessionName); err != nil {
		t.Fatalf("standard home path was rejected: %v", err)
	}

	clientOnlyRoot := "/" + strings.Repeat("a", 39)
	err = ValidateHerdrSessionSocketPaths(clientOnlyRoot, sessionName)
	if err == nil || !strings.Contains(err.Error(), herdrClientSocketFilename) || !strings.Contains(err.Error(), "107") {
		t.Fatalf("overlong client socket path was not explained: %v", err)
	}

	apiRoot := "/" + strings.Repeat("a", 40)
	err = ValidateHerdrSessionSocketPaths(apiRoot, sessionName)
	if err == nil || !strings.Contains(err.Error(), herdrAPISocketFilename) || strings.Contains(err.Error(), herdrClientSocketFilename) {
		t.Fatalf("overlong API socket path was not explained first: %v", err)
	}
}

func TestValidateHerdrPaneHistoryRequiresOwnerOnlyRootConfigAndOptIn(t *testing.T) {
	root := filepath.Join(t.TempDir(), "herdr")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "config.toml")
	if err := os.WriteFile(config, []byte("[experimental]\npane_history = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := HerdrPaths{Root: root, ConfigFile: config}
	if err := ValidateHerdrPaneHistory(paths); err != nil {
		t.Fatalf("validate secure pane-history config: %v", err)
	}

	if err := os.WriteFile(config, []byte("[experimental]\npane_history = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateHerdrPaneHistory(paths); err == nil {
		t.Fatal("expected disabled pane history rejection")
	}
	if err := os.Chmod(config, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateHerdrPaneHistory(paths); err == nil {
		t.Fatal("expected permissive config rejection")
	}
}

func TestValidateHerdrPaneHistoryRejectsConfigSymlink(t *testing.T) {
	root := filepath.Join(t.TempDir(), "herdr")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "actual.toml")
	if err := os.WriteFile(target, []byte("[experimental]\npane_history = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "config.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ValidateHerdrPaneHistory(HerdrPaths{Root: root, ConfigFile: link}); err == nil {
		t.Fatal("expected config symlink rejection")
	}
}

func TestHerdrStateRootExistsAcceptsSafeAbsence(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing-herdr")
	exists, err := HerdrStateRootExists(root)
	if err != nil || exists {
		t.Fatalf("unexpected missing-root result exists=%t err=%v", exists, err)
	}
}

func TestHerdrManagerVerifiesPathsAroundDestructiveCommands(t *testing.T) {
	root := filepath.Join(t.TempDir(), "herdr")
	sessionPath := filepath.Join(root, "sessions", "work")
	if err := os.MkdirAll(sessionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeHerdrRunner{responses: []fakeHerdrResponse{
		{output: sessionListJSON(root, "work", true)},
		{output: []byte(`{}`)},
		{output: sessionListJSON(root, "work", false)},
		{output: []byte(`{}`)},
		{output: []byte(`{"sessions":[]}`)},
	}, deletePath: sessionPath}
	manager := HerdrManager{Executable: "/usr/bin/herdr", Root: root, Runner: runner}

	if err := manager.DeleteSession(context.Background(), "work"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	want := [][]string{
		{"session", "list", "--json"}, {"session", "stop", "work", "--json"},
		{"session", "list", "--json"}, {"session", "delete", "work", "--json"},
		{"session", "list", "--json"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("unexpected commands: got=%v want=%v", runner.calls, want)
	}
}

func TestHerdrManagerRejectsUnexpectedSessionDirectoryBeforeStop(t *testing.T) {
	runner := &fakeHerdrRunner{responses: []fakeHerdrResponse{{output: []byte(`{"sessions":[{"name":"work","default":false,"running":true,"socket_path":"/tmp/herdr.sock","session_dir":"/tmp"}]}`)}}}
	manager := HerdrManager{Executable: "/usr/bin/herdr", Root: "/home/test/.config/herdr", Runner: runner}

	if err := manager.DeleteSession(context.Background(), "work"); err == nil {
		t.Fatal("expected unsafe session directory rejection")
	}
	if len(runner.calls) != 1 {
		t.Fatalf("destructive command ran after unsafe discovery: %v", runner.calls)
	}
}

func TestHerdrManagerTreatsMissingSessionAsAlreadyPurged(t *testing.T) {
	root := filepath.Join(t.TempDir(), "herdr")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeHerdrRunner{responses: []fakeHerdrResponse{{output: []byte(`{"sessions":[]}`)}}}
	manager := HerdrManager{Executable: "/usr/bin/herdr", Root: root, Runner: runner}
	if err := manager.DeleteSession(context.Background(), "work"); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("unexpected commands: %v", runner.calls)
	}
}

func TestHerdrManagerRefusesToTreatOmittedExistingPathAsPurged(t *testing.T) {
	root := filepath.Join(t.TempDir(), "herdr")
	if err := os.MkdirAll(filepath.Join(root, "sessions", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeHerdrRunner{responses: []fakeHerdrResponse{{output: []byte(`{"sessions":[]}`)}}}
	manager := HerdrManager{Executable: "/usr/bin/herdr", Root: root, Runner: runner}

	if err := manager.DeleteSession(context.Background(), "work"); err == nil {
		t.Fatal("expected omitted existing session path rejection")
	}
	if len(runner.calls) != 1 {
		t.Fatalf("destructive command ran after inconsistent discovery: %v", runner.calls)
	}
}

func TestHerdrCommandOutputBufferIsBounded(t *testing.T) {
	output := &boundedCombinedOutput{limit: 4}
	data := []byte("abcdefgh")
	if written, err := output.Write(data); err != nil || written != len(data) {
		t.Fatalf("write bounded output: written=%d err=%v", written, err)
	}
	if string(output.Bytes()) != "abcd" {
		t.Fatalf("unexpected bounded output %q", output.Bytes())
	}
}

type fakeHerdrResponse struct {
	output []byte
	err    error
}

type fakeHerdrRunner struct {
	responses  []fakeHerdrResponse
	calls      [][]string
	deletePath string
}

func (runner *fakeHerdrRunner) CombinedOutput(_ context.Context, _ string, arguments ...string) ([]byte, error) {
	runner.calls = append(runner.calls, append([]string(nil), arguments...))
	if len(runner.responses) == 0 {
		return nil, errors.New("unexpected command")
	}
	response := runner.responses[0]
	runner.responses = runner.responses[1:]
	if len(arguments) >= 2 && arguments[0] == "session" && arguments[1] == "delete" && runner.deletePath != "" {
		if err := os.RemoveAll(runner.deletePath); err != nil {
			return nil, err
		}
	}
	return response.output, response.err
}

func sessionListJSON(root string, name string, running bool) []byte {
	data, _ := json.Marshal(map[string]any{"sessions": []map[string]any{{
		"name": name, "default": false, "running": running,
		"socket_path": filepath.Join(root, "sessions", name, "herdr.sock"),
		"session_dir": filepath.Join(root, "sessions", name),
	}}})
	return data
}
