package herdrinit

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	sessionstate "github.com/marang/sway-session/internal/session"
)

const emptySnapshot = `{"id":"snapshot","result":{"type":"session_snapshot","snapshot":{"version":"0.8.2","protocol":20,"workspaces":[{"workspace_id":"w1"}],"tabs":[{"workspace_id":"w1","tab_id":"w1:t1"}],"panes":[{"workspace_id":"w1","tab_id":"w1:t1","pane_id":"w1:p1","agent":null}],"layouts":[{"workspace_id":"w1","tab_id":"w1:t1","panes":[{"pane_id":"w1:p1"}],"splits":[]}],"agents":[]}}}`

type runnerCall struct {
	session   string
	cwd       string
	arguments []string
}

type fakeRunner struct {
	outputs [][]byte
	errors  []error
	calls   []runnerCall
}

func (runner *fakeRunner) Run(_ context.Context, session string, cwd string, arguments ...string) ([]byte, error) {
	runner.calls = append(runner.calls, runnerCall{session: session, cwd: cwd, arguments: append([]string(nil), arguments...)})
	index := len(runner.calls) - 1
	var output []byte
	if index < len(runner.outputs) {
		output = runner.outputs[index]
	}
	if index < len(runner.errors) {
		return output, runner.errors[index]
	}
	return output, nil
}

func testContext(t *testing.T) sessionstate.Context {
	t.Helper()
	return sessionstate.Context{
		ID:       sessionstate.ContextID("8f33d6d0-7c54-4da1-9e38-2bd290ef85ca"),
		Label:    "test",
		Provider: "codex",
		State:    sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: "lab-88-e2e", Cwd: t.TempDir(), Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}},
	}
}

func readyShell(cwd string) []byte {
	return []byte(fmt.Sprintf(`{"result":{"type":"pane_process_info","process_info":{"pane_id":"w1:p1","shell_pid":42,"foreground_process_group_id":42,"foreground_processes":[{"pid":42,"name":"zsh","cwd":%q}]}}}`, cwd))
}

func splitSnapshot(original string, created string) []byte {
	return []byte(fmt.Sprintf(`{"result":{"type":"session_snapshot","snapshot":{"version":"0.8.2","protocol":20,"workspaces":[{"workspace_id":"w1"}],"tabs":[{"workspace_id":"w1","tab_id":"w1:t1"}],"panes":[{"workspace_id":"w1","tab_id":"w1:t1","pane_id":%q,"agent":null},{"workspace_id":"w1","tab_id":"w1:t1","pane_id":%q,"agent":null}],"layouts":[{"workspace_id":"w1","tab_id":"w1:t1","panes":[{"pane_id":%q},{"pane_id":%q}],"splits":[{}]}],"agents":[]}}}`, original, created, original, created))
}

func TestInitializeCreatesOnlyRequestedEmptyLayout(t *testing.T) {
	contextValue := testContext(t)
	runner := &fakeRunner{outputs: [][]byte{
		[]byte(emptySnapshot),
		readyShell(contextValue.Launcher.Cwd),
		[]byte(`{"id":"split","result":{"type":"pane_info","pane":{"pane_id":"w1:p2"}}}`),
		[]byte(`{"id":"agent","result":{"type":"agent_info"}}`),
	}}

	result, err := Initialize(context.Background(), contextValue, []string{"codex", "shell"}, runner)
	if err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if !result.Initialized || result.Reason != "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	want := []runnerCall{
		{session: "lab-88-e2e", cwd: contextValue.Launcher.Cwd, arguments: []string{"api", "snapshot"}},
		{session: "lab-88-e2e", cwd: contextValue.Launcher.Cwd, arguments: []string{"pane", "process-info", "--pane", "w1:p1"}},
		{session: "lab-88-e2e", cwd: contextValue.Launcher.Cwd, arguments: []string{"pane", "split", "w1:p1", "--direction", "right", "--ratio", "0.5", "--cwd", contextValue.Launcher.Cwd, "--no-focus"}},
		{session: "lab-88-e2e", cwd: contextValue.Launcher.Cwd, arguments: []string{"agent", "start", "sway-left", "--kind", "codex", "--pane", "w1:p1", "--timeout", "30000"}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("unexpected Herdr calls:\n got: %#v\nwant: %#v", runner.calls, want)
	}
}

func TestInitializeWaitsForFreshShellToBecomeIdle(t *testing.T) {
	contextValue := testContext(t)
	busy := []byte(`{"result":{"type":"pane_process_info","process_info":{"pane_id":"w1:p1","shell_pid":42,"foreground_process_group_id":99,"foreground_processes":[{"pid":99,"name":"zsh","cwd":"/tmp"}]}}}`)
	runner := &fakeRunner{outputs: [][]byte{
		[]byte(emptySnapshot),
		busy,
		readyShell(contextValue.Launcher.Cwd),
		[]byte(`{"result":{"type":"pane_info","pane":{"pane_id":"w1:p2"}}}`),
		[]byte(`{"result":{"type":"agent_info"}}`),
	}}

	result, err := Initialize(context.Background(), contextValue, []string{"codex", "shell"}, runner)
	if err != nil || !result.Initialized {
		t.Fatalf("transiently busy shell did not converge: result=%+v err=%v", result, err)
	}
	if len(runner.calls) != 5 || !reflect.DeepEqual(runner.calls[2].arguments, []string{"pane", "process-info", "--pane", "w1:p1"}) {
		t.Fatalf("shell readiness was not re-observed before mutation: %#v", runner.calls)
	}
}

func TestInitializeStartsRightAgentInCreatedPane(t *testing.T) {
	contextValue := testContext(t)
	runner := &fakeRunner{outputs: [][]byte{
		[]byte(emptySnapshot),
		readyShell(contextValue.Launcher.Cwd),
		[]byte(`{"result":{"type":"pane_info","pane":{"pane_id":"w1:p9"}}}`),
		[]byte(`{"result":{"type":"agent_info"}}`),
	}}
	if _, err := Initialize(context.Background(), contextValue, []string{"shell", "opencode"}, runner); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	last := runner.calls[len(runner.calls)-1].arguments
	want := []string{"agent", "start", "sway-right", "--kind", "opencode", "--pane", "w1:p9", "--timeout", "30000"}
	if !reflect.DeepEqual(last, want) {
		t.Fatalf("unexpected agent call: got %#v want %#v", last, want)
	}
}

func TestInitializeLeavesNonemptySessionUnchanged(t *testing.T) {
	contextValue := testContext(t)
	nonempty := strings.Replace(emptySnapshot, `"agents":[]`, `"agents":[{"name":"existing"}]`, 1)
	runner := &fakeRunner{outputs: [][]byte{[]byte(nonempty)}}
	result, err := Initialize(context.Background(), contextValue, []string{"codex", "shell"}, runner)
	if err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if result.Initialized || result.Reason != "Herdr session was not proven empty and was left unchanged" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(runner.calls) != 1 || !reflect.DeepEqual(runner.calls[0].arguments, []string{"api", "snapshot"}) {
		t.Fatalf("nonempty session was mutated: %#v", runner.calls)
	}
}

func TestInitializeRejectsRoleInjectionBeforeHerdr(t *testing.T) {
	runner := &fakeRunner{}
	_, err := Initialize(context.Background(), testContext(t), []string{"codex", "--help"}, runner)
	if err == nil || !strings.Contains(err.Error(), "roles[1]") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("Herdr was called for invalid roles: %#v", runner.calls)
	}
}

func TestInitializeRejectsUnknownAgentKindBeforeHerdr(t *testing.T) {
	runner := &fakeRunner{}
	_, err := Initialize(context.Background(), testContext(t), []string{"codex", "future-agent"}, runner)
	if err == nil || !strings.Contains(err.Error(), "supported Herdr agent kind") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("Herdr was called for an unknown role: %#v", runner.calls)
	}
}

func TestInitializeRequiresOneShellAndOneAgent(t *testing.T) {
	for _, roles := range [][]string{{"shell", "shell"}, {"codex", "opencode"}} {
		runner := &fakeRunner{}
		if _, err := Initialize(context.Background(), testContext(t), roles, runner); err == nil || !strings.Contains(err.Error(), "exactly one shell") {
			t.Fatalf("roles %v returned unexpected error: %v", roles, err)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("Herdr was called for invalid roles %v: %#v", roles, runner.calls)
		}
	}
}

func TestInitializeLeavesBusyOrDifferentDirectoryPaneUnchanged(t *testing.T) {
	contextValue := testContext(t)
	busy := []byte(`{"result":{"type":"pane_process_info","process_info":{"pane_id":"w1:p1","shell_pid":42,"foreground_process_group_id":99,"foreground_processes":[{"pid":99,"name":"vim","cwd":"/tmp"}]}}}`)
	outputs := [][]byte{[]byte(emptySnapshot)}
	for range shellReadinessAttempts {
		outputs = append(outputs, busy)
	}
	runner := &fakeRunner{outputs: outputs}
	result, err := Initialize(context.Background(), contextValue, []string{"codex", "shell"}, runner)
	if err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if result.Initialized || !strings.Contains(result.Reason, "idle project shell") {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(runner.calls) != 1+shellReadinessAttempts {
		t.Fatalf("busy pane was mutated: %#v", runner.calls)
	}
}

func TestInitializeRejectsUnknownSnapshotWithoutMutation(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{[]byte(`{"result":{"type":"future_snapshot","snapshot":{}}}`)}}
	_, err := Initialize(context.Background(), testContext(t), []string{"codex", "shell"}, runner)
	if err == nil || !strings.Contains(err.Error(), "unexpected result type") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("unexpected mutation after malformed snapshot: %#v", runner.calls)
	}
}

func TestInitializeRejectsUnknownSnapshotProtocolWithoutMutation(t *testing.T) {
	snapshot := strings.Replace(emptySnapshot, `"protocol":20`, `"protocol":21`, 1)
	runner := &fakeRunner{outputs: [][]byte{[]byte(snapshot)}}

	_, err := Initialize(context.Background(), testContext(t), []string{"codex", "shell"}, runner)

	if err == nil || !strings.Contains(err.Error(), "unsupported Herdr snapshot") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("unknown protocol caused mutation: %#v", runner.calls)
	}
}

func TestInitializeRollsBackFailedAgentStartAndRetryConverges(t *testing.T) {
	contextValue := testContext(t)
	runner := &fakeRunner{
		outputs: [][]byte{
			[]byte(emptySnapshot),
			readyShell(contextValue.Launcher.Cwd),
			[]byte(`{"result":{"type":"pane_info","pane":{"pane_id":"w1:p2"}}}`),
			nil,
			nil,
			[]byte(strings.ReplaceAll(emptySnapshot, "w1:p1", "w1:p2")),
			[]byte(strings.ReplaceAll(string(readyShell(contextValue.Launcher.Cwd)), "w1:p1", "w1:p2")),
			[]byte(`{"result":{"type":"pane_info","pane":{"pane_id":"w1:p3"}}}`),
			[]byte(`{"result":{"type":"agent_info"}}`),
		},
		errors: []error{nil, nil, nil, errors.New("agent trust required")},
	}
	_, err := Initialize(context.Background(), contextValue, []string{"codex", "shell"}, runner)
	if err == nil || !strings.Contains(err.Error(), "agent trust required") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calls) != 5 || !reflect.DeepEqual(runner.calls[4].arguments, []string{"pane", "close", "w1:p1"}) {
		t.Fatalf("failed agent start was not rolled back: %#v", runner.calls)
	}
	result, err := Initialize(context.Background(), contextValue, []string{"codex", "shell"}, runner)
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if !result.Initialized || len(runner.calls) != 9 {
		t.Fatalf("retry did not converge: result=%+v calls=%#v", result, runner.calls)
	}
}

func TestInitializeReportsAgentAndRollbackFailure(t *testing.T) {
	contextValue := testContext(t)
	runner := &fakeRunner{
		outputs: [][]byte{[]byte(emptySnapshot), readyShell(contextValue.Launcher.Cwd), []byte(`{"result":{"type":"pane_info","pane":{"pane_id":"w1:p2"}}}`)},
		errors:  []error{nil, nil, nil, errors.New("agent trust required"), errors.New("pane close failed")},
	}
	_, err := Initialize(context.Background(), contextValue, []string{"codex", "shell"}, runner)
	if err == nil || !strings.Contains(err.Error(), "agent trust required") || !strings.Contains(err.Error(), "pane close failed") {
		t.Fatalf("partial failure details were lost: %v", err)
	}
}

func TestInitializeRollsBackSplitAfterMalformedSuccessResponseAndRetryConverges(t *testing.T) {
	contextValue := testContext(t)
	runner := &fakeRunner{outputs: [][]byte{
		[]byte(emptySnapshot),
		readyShell(contextValue.Launcher.Cwd),
		[]byte(`{"result":`),
		splitSnapshot("w1:p1", "w1:p2"),
		nil,
		[]byte(emptySnapshot),
		readyShell(contextValue.Launcher.Cwd),
		[]byte(`{"result":{"type":"pane_info","pane":{"pane_id":"w1:p3"}}}`),
		[]byte(`{"result":{"type":"agent_info"}}`),
	}}
	_, err := Initialize(context.Background(), contextValue, []string{"codex", "shell"}, runner)
	if err == nil || !strings.Contains(err.Error(), "decode Herdr pane split") {
		t.Fatalf("unexpected split error: %v", err)
	}
	if len(runner.calls) != 5 || !reflect.DeepEqual(runner.calls[3].arguments, []string{"api", "snapshot"}) || !reflect.DeepEqual(runner.calls[4].arguments, []string{"pane", "close", "w1:p2"}) {
		t.Fatalf("malformed split response was not reconciled: %#v", runner.calls)
	}
	result, err := Initialize(context.Background(), contextValue, []string{"codex", "shell"}, runner)
	if err != nil || !result.Initialized || len(runner.calls) != 9 {
		t.Fatalf("retry did not converge: result=%+v err=%v calls=%#v", result, err, runner.calls)
	}
}

func TestInitializeReconcilesFailedSplitWithoutClosingOriginalPane(t *testing.T) {
	contextValue := testContext(t)
	runner := &fakeRunner{
		outputs: [][]byte{[]byte(emptySnapshot), readyShell(contextValue.Launcher.Cwd), nil, []byte(emptySnapshot)},
		errors:  []error{nil, nil, errors.New("split request failed")},
	}
	_, err := Initialize(context.Background(), contextValue, []string{"codex", "shell"}, runner)
	if err == nil || !strings.Contains(err.Error(), "split request failed") {
		t.Fatalf("unexpected split error: %v", err)
	}
	if len(runner.calls) != 4 || !reflect.DeepEqual(runner.calls[3].arguments, []string{"api", "snapshot"}) {
		t.Fatalf("failed split reconciliation changed the empty session: %#v", runner.calls)
	}
}

func TestInitializeLeavesAmbiguousSplitStateUnchanged(t *testing.T) {
	contextValue := testContext(t)
	ambiguous := strings.Replace(string(splitSnapshot("w1:p1", "w1:p2")), `"agents":[]`, `"agents":[{"name":"other"}]`, 1)
	runner := &fakeRunner{
		outputs: [][]byte{[]byte(emptySnapshot), readyShell(contextValue.Launcher.Cwd), nil, []byte(ambiguous)},
		errors:  []error{nil, nil, errors.New("split response lost")},
	}
	_, err := Initialize(context.Background(), contextValue, []string{"codex", "shell"}, runner)
	if err == nil || !strings.Contains(err.Error(), "not proven") {
		t.Fatalf("ambiguous split state was not reported: %v", err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("ambiguous split state was mutated: %#v", runner.calls)
	}
}
