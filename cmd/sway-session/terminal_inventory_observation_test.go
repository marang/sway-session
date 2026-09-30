package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sessionstate "github.com/marang/sway-session/internal/session"
)

type inventoryObservationRunner struct {
	calls   int
	payload []byte
	err     error
}

func (runner *inventoryObservationRunner) CombinedOutput(ctx context.Context, _ string, args ...string) ([]byte, error) {
	runner.calls++
	if !reflect.DeepEqual(args, []string{"session", "list", "--json"}) {
		return nil, errors.New("unexpected mutating command")
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("missing observation deadline")
	}
	return runner.payload, runner.err
}

func TestTerminalInventoryObservesWindowsAndStoppedSessionsSeparately(t *testing.T) {
	deps, contexts := terminalManageObservationFixture(t)
	paths, _ := deps.herdrPaths()
	entries := make([]map[string]any, 0, len(contexts))
	for _, value := range contexts[:2] {
		directory := filepath.Join(paths.Root, "sessions", value.Launcher.Session)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, map[string]any{"name": value.Launcher.Session, "running": false, "default": false, "session_dir": directory, "socket_path": filepath.Join(directory, "herdr.sock")})
	}
	payload, _ := json.Marshal(map[string]any{"sessions": entries})
	runner := &inventoryObservationRunner{payload: payload}
	deps.herdrRunner = runner
	client := &terminalManageObservationRequester{tree: treeWithContexts(contexts[0].ID)}
	deps.newSwayClient = func(string) swayRequester { return client }
	result, failure := executeTerminalList(t.Context(), nil, deps)
	if failure != nil {
		t.Fatalf("list failed: %+v", failure)
	}
	items := *result.Terminals
	for _, item := range items {
		wantWindow := "closed"
		if item.ContextID == contexts[0].ID {
			wantWindow = "open"
		}
		wantSession := "stopped"
		if item.ContextID == contexts[2].ID {
			wantSession = "missing"
		}
		if item.WindowPresence != wantWindow || item.WindowObservedAt == nil || item.Activity.SessionState != wantSession || item.Activity.AgentState != "none" || item.Activity.ObservedAt == nil {
			t.Fatalf("independent evidence missing: %+v", item)
		}
	}
	if runner.calls != 1 {
		t.Fatalf("bulk discoveries=%d, want 1", runner.calls)
	}
	terminalManageRequireObservationOnly(t, client)
}

func TestTerminalInventoryRetainsMetadataAndRedactsExternalFailure(t *testing.T) {
	deps, contexts := terminalManageObservationFixture(t)
	deps.newSwayClient = nil
	runner := &inventoryObservationRunner{payload: []byte("PRIVATE-OUTPUT"), err: errors.New("PRIVATE-ERROR")}
	deps.herdrRunner = runner
	result, failure := executeTerminalStatus(t.Context(), []string{string(contexts[0].ID)}, deps)
	if failure != nil {
		t.Fatalf("metadata inaccessible: %+v", failure)
	}
	item := (*result.Terminals)[0]
	if item.ContextID != contexts[0].ID || item.WindowPresence != "unknown" || item.Activity.SessionState != "unknown" || item.Activity.AgentState != "unknown" || item.Activity.Reason == "" {
		t.Fatalf("unknown evidence missing: %+v", item)
	}
	payload, _ := json.Marshal(result)
	for _, secret := range []string{"PRIVATE-OUTPUT", "PRIVATE-ERROR"} {
		if strings.Contains(string(payload), secret) {
			t.Fatalf("external error leaked: %s", payload)
		}
	}
}

func TestTerminalCleanupObservesArchivedSessionsWithoutMutation(t *testing.T) {
	deps, contexts := terminalManageObservationFixture(t)
	deps.resolveProgram = func(string) (string, error) { return "", errors.New("not installed") }
	result, failure := executeTerminalCleanup(t.Context(), nil, deps)
	if failure != nil || !result.Preview || result.Terminals == nil || len(*result.Terminals) != 1 {
		t.Fatalf("preview=%+v failure=%+v", result, failure)
	}
	item := (*result.Terminals)[0]
	if item.Activity.Reason != "manager_executable_unavailable" || item.State != sessionstate.ContextArchived {
		t.Fatalf("preview missing unavailable activity: %+v", item)
	}
	root, _ := deps.stateRoot()
	registry, err := sessionstate.ReadRegistrySnapshotContext(t.Context(), root)
	if err != nil || !reflect.DeepEqual(registry.Contexts, contexts) {
		t.Fatalf("preview changed saved state: %v", err)
	}
}
