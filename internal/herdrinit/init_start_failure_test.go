package herdrinit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestInitializePreservesPaneAfterAgentStartupTimeout(t *testing.T) {
	for _, roles := range [][]string{{"codex", "shell"}, {"shell", "codex"}} {
		t.Run(roles[0]+"_left", func(t *testing.T) {
			contextValue := testContext(t)
			runner := &fakeRunner{
				outputs: [][]byte{
					[]byte(emptySnapshot22),
					readyShell(contextValue.Launcher.Cwd),
					[]byte(`{"result":{"type":"pane_info","pane":{"pane_id":"w1:p2"}}}`),
				},
				errors: []error{nil, nil, nil, context.DeadlineExceeded},
			}
			_, err := Initialize(context.Background(), contextValue, roles, runner)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("startup timeout was lost: %v", err)
			}
			for _, call := range runner.calls {
				if len(call.arguments) >= 2 && call.arguments[0] == "pane" && call.arguments[1] == "close" {
					t.Fatalf("startup timeout closed a pane that may already contain the agent: %v", call.arguments)
				}
			}
		})
	}
}

func TestInitializeObservesFailedStartWithoutTreatingStatusAsSuccess(t *testing.T) {
	for _, status := range []string{"unknown", "working", "blocked", "idle", "done"} {
		t.Run(status, func(t *testing.T) {
			contextValue := testContext(t)
			observed := failedStartSnapshot(t, "w1:p1", status)
			startErr := errors.New("startup acknowledgement lost")
			runner := &fakeRunner{
				outputs: [][]byte{[]byte(emptySnapshot22), readyShell(contextValue.Launcher.Cwd),
					[]byte(`{"result":{"type":"pane_info","pane":{"pane_id":"w1:p2"}}}`), nil, observed, observed},
				errors: []error{nil, nil, nil, startErr},
			}
			result, err := Initialize(context.Background(), contextValue, []string{"codex", "shell"}, runner)
			if !errors.Is(err, startErr) || result.Initialized || !strings.Contains(err.Error(), "with agent codex (Herdr status "+status+")") {
				t.Fatalf("failed start lost its original error or observed state: result=%+v err=%v", result, err)
			}
			if len(runner.calls) != 5 || strings.Join(runner.calls[4].arguments, " ") != "api snapshot" {
				t.Fatalf("failed start did more than one read-only observation: %#v", runner.calls)
			}
			result, err = Initialize(context.Background(), contextValue, []string{"codex", "shell"}, runner)
			if err != nil || result.Initialized || result.Reason == "" || len(runner.calls) != 6 {
				t.Fatalf("retry changed the existing agent session: result=%+v err=%v calls=%#v", result, err, runner.calls)
			}
		})
	}
}

func TestInitializeAcceptsHerdrConfirmedUnknownReadyAgent(t *testing.T) {
	contextValue := testContext(t)
	runner := &fakeRunner{outputs: [][]byte{
		[]byte(emptySnapshot22), readyShell(contextValue.Launcher.Cwd),
		[]byte(`{"result":{"type":"pane_info","pane":{"pane_id":"w1:p2"}}}`),
		[]byte(`{"result":{"type":"agent_info","agent":{"pane_id":"w1:p1","agent":"codex","agent_status":"unknown","interactive_ready":true}}}`),
	}}
	result, err := Initialize(context.Background(), contextValue, []string{"codex", "shell"}, runner)
	if err != nil || !result.Initialized || len(runner.calls) != 4 {
		t.Fatalf("Herdr-confirmed readiness was confused with turn status: result=%+v err=%v calls=%#v", result, err, runner.calls)
	}
}

type failedStartRunnerFunc func(context.Context, string, string, ...string) ([]byte, error)

func (run failedStartRunnerFunc) Run(ctx context.Context, session, cwd string, arguments ...string) ([]byte, error) {
	return run(ctx, session, cwd, arguments...)
}

func TestFailedStartObservationHonorsDeadlineAndCancellation(t *testing.T) {
	contextValue := testContext(t)
	startErr := errors.New("agent startup timed out")
	calls := 0
	runner := failedStartRunnerFunc(func(ctx context.Context, session, cwd string, arguments ...string) ([]byte, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > startupObservationTimeout {
			t.Fatalf("observation is not bounded: deadline=%v present=%v", deadline, ok)
		}
		if session != contextValue.Launcher.Session || cwd != contextValue.Launcher.Cwd || strings.Join(arguments, " ") != "api snapshot" {
			t.Fatalf("unexpected observation target: %s %s %v", session, cwd, arguments)
		}
		return nil, context.DeadlineExceeded
	})
	err := observeFailedAgentStart(context.Background(), contextValue, "w1:p1", startErr, runner)
	if !errors.Is(err, startErr) || !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("bounded observation replaced startup error: err=%v calls=%d", err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = observeFailedAgentStart(ctx, contextValue, "w1:p1", startErr, runner)
	if !errors.Is(err, startErr) || calls != 1 {
		t.Fatalf("canceled initialization ran another command: err=%v calls=%d", err, calls)
	}
}

func TestFailedStartObservationDoesNotTrustMissingOrMalformedEvidence(t *testing.T) {
	valid := failedStartSnapshot(t, "w1:p1", "unknown")
	for _, test := range []struct {
		name   string
		output []byte
		want   string
		err    bool
	}{
		{"missing target", valid, "absent", false},
		{"malformed", []byte(`{"result":`), "", true},
		{"missing pane list", []byte(`{"result":{"type":"session_snapshot","snapshot":{"version":"0.9.5","protocol":22}}}`), "", true},
		{"unsupported protocol", []byte(strings.Replace(string(valid), `"protocol":22`, `"protocol":999`, 1)), "", true},
		{"untrusted labels", []byte(strings.ReplaceAll(strings.ReplaceAll(string(valid), `"agent":"codex"`, `"agent":"private-agent"`), `"agent_status":"unknown"`, `"agent_status":"private-status"`)), "still present", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := "w1:p1"
			if test.name == "missing target" {
				target = "w1:p99"
			}
			observation, err := failedStartObservation(test.output, target)
			if (err != nil) != test.err || !strings.Contains(observation, test.want) || strings.Contains(observation, "private-") {
				t.Fatalf("unexpected untrusted observation: %q err=%v", observation, err)
			}
		})
	}
}

func failedStartSnapshot(t *testing.T, target, status string) []byte {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(splitSnapshot("w1:p1", "w1:p2"), &response); err != nil {
		t.Fatal(err)
	}
	snapshot := response["result"].(map[string]any)["snapshot"].(map[string]any)
	snapshot["version"], snapshot["protocol"] = "0.9.5", 22
	for _, item := range snapshot["panes"].([]any) {
		pane := item.(map[string]any)
		pane["agent_status"] = "unknown"
		if pane["pane_id"] == target {
			pane["agent"], pane["agent_status"] = "codex", status
		}
	}
	// A timed-out managed start can lose its name and readiness metadata while
	// the process remains detected in its pane.
	output, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return output
}
