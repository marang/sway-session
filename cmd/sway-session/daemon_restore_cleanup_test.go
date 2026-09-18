package main

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// cleanupCompositor applies commands to an observed tree and queues the move,
// focus and barrier events that the runtime receives on its separate stream.
type cleanupCompositor struct {
	t              *testing.T
	root           *Node
	events         []swayipc.Event
	commands       []string
	rejectReturn   bool
	unknownReturn  bool
	rejectReturnID int64
	rejectUnmark   bool
}

func (*cleanupCompositor) Close() {}

func (c *cleanupCompositor) RequestContext(_ context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if kind == swayipc.SendTick {
		c.events = append(c.events, swayipc.Event{Type: swayipc.EventTick, Payload: string(payload)})
		return swayipc.Message{Type: kind, Payload: []byte(`{"success":true}`)}, nil
	}
	if kind != swayipc.RunCommand {
		return swayipc.Message{}, fmt.Errorf("unexpected request %d", kind)
	}
	command := string(payload)
	c.commands = append(c.commands, command)
	selector, operation, ok := strings.Cut(command, "] ")
	if !ok {
		c.t.Fatalf("unexpected command %q", command)
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(selector, "[con_id="), 10, 64)
	if err != nil {
		c.t.Fatal(err)
	}
	node := findContainerByID(c.root, id)
	if node == nil {
		c.t.Fatalf("missing command target %d", id)
	}
	switch {
	case strings.HasPrefix(operation, "move container to workspace "):
		target, err := strconv.Unquote(strings.TrimPrefix(operation, "move container to workspace "))
		if err != nil {
			c.t.Fatal(err)
		}
		if target == "98" && (c.rejectReturn || c.rejectReturnID == id) {
			return swayipc.Message{Type: kind, Payload: []byte(`[{"success":false,"error":"target temporarily unavailable"}]`)}, nil
		}
		c.move(node, target)
		c.events = append(c.events, swayipc.Event{Type: swayipc.EventWindow, Change: "move", Container: node}, swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: node})
		if target == "98" && c.unknownReturn {
			c.unknownReturn = false
			return swayipc.Message{}, &swayipc.CommandOutcomeUnknownError{Cause: fmt.Errorf("reply lost")}
		}
	case strings.HasPrefix(operation, "mark --add "):
		mark, err := strconv.Unquote(strings.TrimPrefix(operation, "mark --add "))
		if err != nil {
			c.t.Fatal(err)
		}
		node.Marks = append(node.Marks, mark)
	case strings.HasPrefix(operation, "unmark "):
		if c.rejectUnmark {
			return swayipc.Message{Type: kind, Payload: []byte(`[{"success":false,"error":"unmark temporarily rejected"}]`)}, nil
		}
		mark, err := strconv.Unquote(strings.TrimPrefix(operation, "unmark "))
		if err != nil {
			c.t.Fatal(err)
		}
		node.Marks = slices.DeleteFunc(node.Marks, func(value string) bool { return value == mark })
	default:
		c.t.Fatalf("unexpected restore operation %q", operation)
	}
	return swayipc.Message{Type: kind, Payload: []byte(`[{"success":true}]`)}, nil
}

func (c *cleanupCompositor) move(node *Node, workspace string) {
	for _, ws := range c.root.Nodes[0].Nodes {
		ws.Nodes = slices.DeleteFunc(ws.Nodes, func(child *Node) bool { return child == node })
	}
	for _, ws := range c.root.Nodes[0].Nodes {
		if ws.Name == workspace {
			ws.Nodes = append(ws.Nodes, node)
			return
		}
	}
	c.root.Nodes[0].Nodes = append(c.root.Nodes[0].Nodes, &Node{ID: int64(100 + len(c.root.Nodes[0].Nodes)), Type: "workspace", Name: workspace, Layout: "splith", Nodes: []*Node{node}})
}

func (c *cleanupCompositor) drain(runtime *sessionRuntime, now time.Time) {
	events := c.events
	c.events = nil
	for _, event := range events {
		runtime.HandleEvent(event, now)
	}
}

func TestSessionRuntimeCancelledRestoreRecoversStagedWindow(t *testing.T) {
	runtime, c, now := newCleanupScenario(t)
	// The first restore action stages a window before a binding cancels it.
	if _, err := runtime.Reconcile(c.root, now); err != nil {
		t.Fatal(err)
	}
	if c.root.Nodes[0].Nodes[len(c.root.Nodes[0].Nodes)-1].Name != sessionstate.RestoreStagingWorkspace {
		t.Fatal("test did not stage a window")
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding, Change: "run"}, now)
	c.drain(runtime, now)
	finishCleanupScenario(t, runtime, c, now)
	if len(c.root.Nodes[0].Nodes[0].Nodes) != 2 {
		t.Fatal("cleanup did not return the staged window")
	}
	if c.root.Nodes[0].Nodes[0].Layout != "splith" {
		t.Fatal("cleanup rebuilt layout after cancellation")
	}
}

func newCleanupScenario(t *testing.T) (*sessionRuntime, *cleanupCompositor, time.Time) {
	t.Helper()
	stateRoot := filepath.Join(t.TempDir(), "state")
	secondID := sessionstate.ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if err := sessionstate.RegistryStoreFor(stateRoot).Save(sessionRegistryIDs(testManagedContextID, secondID)); err != nil {
		t.Fatal(err)
	}
	desired := exactDaemonSnapshot("98", testManagedContextID, secondID)
	desired.Workspaces[0].Tiling.Layout = sessionstate.LayoutTabbed
	if err := sessionstate.LayoutStoreFor(stateRoot).Save(desired); err != nil {
		t.Fatal(err)
	}
	first, second := managedDaemonLeaf(t, 41, testManagedContextID), managedDaemonLeaf(t, 42, secondID)
	first.Marks, second.Marks = nil, nil
	c := &cleanupCompositor{t: t, root: daemonTree("98", first, second)}
	runtime, err := newSessionRuntimeWithOptions(c, sessionRuntimeOptions{Root: stateRoot})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := runtime.Reconcile(c.root, now); err != nil {
		t.Fatal(err)
	}
	// Startup readiness must not pause halfway through staging in tests that
	// deliberately withhold move/focus delivery until several moves completed.
	return runtime, c, now.Add(sessionStartupSettleDelay)
}

func finishCleanupScenario(t *testing.T, runtime *sessionRuntime, c *cleanupCompositor, now time.Time) {
	t.Helper()
	for range 8 {
		if _, err := runtime.Reconcile(c.root, now); err != nil {
			t.Fatal(err)
		}
		c.drain(runtime, now)
	}
	for _, ws := range c.root.Nodes[0].Nodes {
		if ws.Name == sessionstate.RestoreStagingWorkspace && len(ws.Nodes) != 0 {
			t.Fatal("cancelled restore stranded an owned window in staging")
		}
	}
}

func TestSessionRuntimeCleanupPreservesUserMovedWindow(t *testing.T) {
	runtime, c, now := newCleanupScenario(t)
	for range 2 {
		if _, err := runtime.Reconcile(c.root, now); err != nil {
			t.Fatal(err)
		}
	}
	first := findContainerByID(c.root, 41)
	c.move(first, "99")
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding, Change: "run"}, now)
	c.drain(runtime, now)
	finishCleanupScenario(t, runtime, c, now)
	for _, ws := range c.root.Nodes[0].Nodes {
		if ws.Name == "99" && len(ws.Nodes) == 1 && ws.Nodes[0] == first {
			return
		}
	}
	t.Fatal("cleanup undid the user's workspace move")
}

func TestSessionRuntimeCleanupWaitsForFreshConnection(t *testing.T) {
	runtime, c, now := newCleanupScenario(t)
	guard := &mutableEventStreamGuard{epoch: 1, connected: true}
	runtime.eventStreamState = guard
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 1}, now)
	if _, err := runtime.Reconcile(c.root, now); err != nil {
		t.Fatal(err)
	}
	guard.connected = false
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "disconnected"}, now)
	count := len(c.commands)
	if _, err := runtime.Reconcile(c.root, now); err == nil {
		t.Fatal("disconnected cleanup was allowed")
	}
	if len(c.commands) != count {
		t.Fatal("cleanup issued a command without a trusted connection")
	}
	guard.connected, guard.epoch = true, 2
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 2}, now)
	finishCleanupScenario(t, runtime, c, now)
}

func TestSessionRuntimeCleanupRetriesFailureWithoutPersistingPartialLayout(t *testing.T) {
	runtime, c, now := newCleanupScenario(t)
	for range 2 {
		if _, err := runtime.Reconcile(c.root, now); err != nil {
			t.Fatal(err)
		}
	}
	want := runtime.persisted
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding, Change: "run"}, now)
	c.rejectReturn = true
	if _, err := runtime.Reconcile(c.root, now); err == nil {
		t.Fatal("failed cleanup was not reported")
	}
	if err := runtime.Flush(now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var stored sessionstate.LayoutSnapshot
	if err := sessionstate.LayoutStoreFor(runtime.root).LoadInto(&stored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, want) {
		t.Fatal("failed cleanup replaced the saved layout")
	}
	c.rejectReturn, c.unknownReturn = false, true
	if _, err := runtime.Reconcile(c.root, now); err == nil {
		t.Fatal("unknown cleanup outcome was not reported")
	}
	c.drain(runtime, now)
	finishCleanupScenario(t, runtime, c, now)
}

func TestSessionRuntimeCleanupSurvivesRestartAndEarlyCancellation(t *testing.T) {
	runtime, c, now := newCleanupScenario(t)
	if _, err := runtime.Reconcile(c.root, now); err != nil {
		t.Fatal(err)
	}
	restarted, err := newSessionRuntimeWithOptions(c, sessionRuntimeOptions{Root: runtime.root})
	if err != nil {
		t.Fatal(err)
	}
	restarted.HandleEvent(swayipc.Event{Type: swayipc.EventBinding, Change: "run"}, now)
	finishCleanupScenario(t, restarted, c, now)
}

func TestSessionRuntimeCleanupDoesNotStarveOtherStagedWindows(t *testing.T) {
	runtime, c, now := newCleanupScenario(t)
	for range 2 {
		if _, err := runtime.Reconcile(c.root, now); err != nil {
			t.Fatal(err)
		}
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding, Change: "run"}, now)
	c.rejectReturnID = 41
	for range 4 {
		_, _ = runtime.Reconcile(c.root, now)
		c.drain(runtime, now)
	}
	for _, ws := range c.root.Nodes[0].Nodes {
		if ws.Name == "98" && len(ws.Nodes) == 1 && ws.Nodes[0].ID == 42 {
			return
		}
	}
	t.Fatalf("one rejected return starved another owned staging window; commands=%v workspaces=%+v", c.commands, c.root.Nodes[0].Nodes)
}

func TestSessionRuntimeCleanupRetainsRejectedMarkAfterStructuralCompletion(t *testing.T) {
	runtime, c, now := newCleanupScenario(t)
	workspace := c.root.Nodes[0].Nodes[0]
	group := &Node{ID: 20, Type: "con", Layout: "tabbed", Nodes: workspace.Nodes}
	workspace.Nodes = []*Node{group}
	// Deterministic mark for workspace 98's tiling root.
	mark := "_sway_session_restore_29db0c6782dbd500_t"
	if err := runtime.applyRestoreAction(sessionstate.RestoreAction{Kind: sessionstate.RestoreAddTemporaryMark, Workspace: "98", ContainerID: group.ID, Target: mark}); err != nil {
		t.Fatal(err)
	}
	runtime.restoreProgress = &sessionstate.RestoreProgress{Workspace: "98", Phase: sessionstate.RestoreBuild}
	c.rejectUnmark = true
	if _, err := runtime.Reconcile(c.root, now); err == nil {
		t.Fatal("unmark rejection was not surfaced")
	}
	if _, err := runtime.Reconcile(c.root, now); err != nil {
		t.Fatal(err)
	}
	runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding, Change: "run"}, now)
	c.rejectUnmark = false
	finishCleanupScenario(t, runtime, c, now)
	if slices.Contains(group.Marks, mark) {
		t.Fatal("structural completion discarded unresolved temporary mark ownership")
	}
}
