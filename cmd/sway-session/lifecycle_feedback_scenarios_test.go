package main

import (
	"context"
	"encoding/json"
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

// Keep the event-mutating compositor and its measured staging focus sequence.
// This adapter adds only fresh GET_TREE snapshots and the build operations
// needed to reach cancellation after real planner-issued temporary marks.
type feedbackScenarioCompositor struct {
	*focusedMoveCompositor
	nextGroup int64
}

func (c *feedbackScenarioCompositor) RequestContext(ctx context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if kind == swayipc.GetTree {
		encoded, err := json.Marshal(c.root)
		return swayipc.Message{Type: kind, Payload: encoded}, err
	}
	before := len(c.events)
	var reply swayipc.Message
	var err error
	command := string(payload)
	if kind == swayipc.RunCommand && (strings.Contains(command, "] split ") || strings.Contains(command, "] layout ") || strings.Contains(command, "] move container to mark ")) {
		selector, operation, _ := strings.Cut(command, "] ")
		id, parseErr := strconv.ParseInt(strings.TrimPrefix(selector, "[con_id="), 10, 64)
		if parseErr != nil {
			c.t.Fatal(parseErr)
		}
		path := containerPath(c.root, id)
		if len(path) < 2 {
			c.t.Fatalf("missing build target: %q", command)
		}
		node, parent := path[len(path)-1], path[len(path)-2]
		switch {
		case strings.HasPrefix(operation, "split "):
			layout := "splith"
			if operation == "split vertical" {
				layout = "splitv"
			}
			group := &Node{ID: c.nextGroup, Type: "con", Layout: layout, Nodes: []*Node{node}, Focus: []int64{id}}
			c.nextGroup++
			parent.Nodes[slices.Index(parent.Nodes, node)] = group
			for index, focused := range parent.Focus {
				if focused == id {
					parent.Focus[index] = group.ID
				}
			}
		case strings.HasPrefix(operation, "layout "):
			parent.Layout = strings.TrimPrefix(operation, "layout ")
		case strings.HasPrefix(operation, "move container to mark "):
			mark, parseErr := strconv.Unquote(strings.TrimPrefix(operation, "move container to mark "))
			if parseErr != nil {
				c.t.Fatal(parseErr)
			}
			target := uniqueMarkedContainer(c.root, mark)
			if target == nil || pathWorkspace(containerPath(c.root, target.ID)) != pathWorkspace(path) {
				c.t.Fatalf("expected a unique group in the same workspace: %q", command)
			}
			parent.Nodes = slices.DeleteFunc(parent.Nodes, func(child *Node) bool { return child == node })
			parent.Focus = slices.DeleteFunc(parent.Focus, func(child int64) bool { return child == id })
			target.Nodes = append(target.Nodes, node)
			target.Focus = append(target.Focus, id)
			c.events = append(c.events, swayipc.Event{Type: swayipc.EventWindow, Change: "move", Container: node})
		}
		c.commands = append(c.commands, command)
		reply = swayipc.Message{Type: kind, Payload: []byte(`[{"success":true}]`)}
	} else {
		reply, err = c.focusedMoveCompositor.RequestContext(ctx, kind, payload)
	}
	// IPC events contain snapshots, not pointers which change with later tree
	// mutations. In particular, queued staging feedback must survive user moves.
	for index := before; index < len(c.events); index++ {
		if c.events[index].Container != nil {
			c.events[index].Container = feedbackScenarioTreeCopy(c.t, c.events[index].Container)
		}
	}
	return reply, err
}

func feedbackScenarioTreeCopy(t *testing.T, tree *Node) *Node {
	t.Helper()
	encoded, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	var copy Node
	if err := json.Unmarshal(encoded, &copy); err != nil {
		t.Fatal(err)
	}
	return &copy
}

func TestSessionRuntimeLifecycleFeedbackSequence(t *testing.T) {
	for _, checkpoint := range []string{"first_staging_move", "second_workspace_temporary_mark"} {
		t.Run(checkpoint, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			ids := make([]sessionstate.ContextID, 6)
			leaves := make([]*Node, len(ids))
			for index := range ids {
				ids[index] = sessionstate.ContextID(fmt.Sprintf("00000000-0000-4000-8000-%012d", index+1))
				leaves[index] = managedDaemonLeaf(t, int64(41+index), ids[index])
				leaves[index].Marks = []string{"feedback-preserve"}
			}
			registry := sessionRegistryIDs(ids...)
			if err := sessionstate.RegistryStoreFor(root).Save(registry); err != nil {
				t.Fatal(err)
			}
			saved := exactDaemonSnapshot("98", ids[:3]...)
			saved.Workspaces = append(saved.Workspaces, exactDaemonSnapshot("99", ids[3:]...).Workspaces...)
			for index := range saved.Workspaces {
				tiling := saved.Workspaces[index].Tiling
				tiling.Children = []sessionstate.LayoutNode{
					{Layout: sessionstate.LayoutTabbed, Children: slices.Clone(tiling.Children[:2])}, tiling.Children[2],
				}
			}
			if err := sessionstate.LayoutStoreFor(root).Save(saved); err != nil {
				t.Fatal(err)
			}
			tree := daemonTree("98", slices.Clone(leaves[:3])...)
			tree.Nodes[0].Nodes[0].Focus = []int64{41, 42, 43}
			leaves[0].Focused = true
			tree.Nodes[0].Nodes = append(tree.Nodes[0].Nodes, &Node{ID: 4, Type: "workspace", Name: "99", Layout: "splith", Nodes: slices.Clone(leaves[3:]), Focus: []int64{44, 45, 46}})
			c := &feedbackScenarioCompositor{focusedMoveCompositor: &focusedMoveCompositor{&cleanupCompositor{t: t, root: tree}}, nextGroup: 200}
			runtime, err := newSessionRuntimeWithOptions(c, sessionRuntimeOptions{Context: t.Context(), Root: root})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventStream, Change: "ready", StreamEpoch: 1}, now)
			observe := func() *Node {
				t.Helper()
				message, err := c.RequestContext(t.Context(), swayipc.GetTree, nil)
				if err != nil {
					t.Fatal(err)
				}
				var observed Node
				if err := json.Unmarshal(message.Payload, &observed); err != nil {
					t.Fatal(err)
				}
				return &observed
			}
			pass := func() []string {
				t.Helper()
				before := len(c.commands)
				if _, err := runtime.Reconcile(observe(), now); err != nil {
					t.Fatal(err)
				}
				commands := c.commands[before:]
				mutations := 0
				for _, command := range commands {
					if !strings.Contains(command, "] mark --add ") || strings.Contains(command, "_sway_session_restore_") {
						mutations++
					}
				}
				if mutations > 1 {
					t.Fatalf("one observation issued %d restore mutations: %q", mutations, commands)
				}
				return commands
			}
			assertSaved := func(want sessionstate.LayoutSnapshot) {
				t.Helper()
				var got sessionstate.LayoutSnapshot
				if err := sessionstate.LayoutStoreFor(root).LoadInto(&got); err != nil {
					t.Fatal(err)
				}
				feedbackScenarioEqual(t, "persisted layout", got, want)
				var gotRegistry sessionstate.Registry
				if err := sessionstate.RegistryStoreFor(root).LoadInto(&gotRegistry); err != nil {
					t.Fatal(err)
				}
				feedbackScenarioEqual(t, "persisted registry", gotRegistry, registry)
			}

			// Adoption is a separate bounded pass; subsequent commands must use
			// observations of its actual marks rather than an invented runtime cursor.
			if commands := pass(); len(commands) != len(ids) {
				t.Fatalf("adoption commands = %q, want six persistent marks", commands)
			}
			if len(c.events) != 0 {
				t.Fatal("mark adoption unexpectedly supplied focus/move feedback")
			}
			assertSaved(saved)
			now = now.Add(sessionStartupSettleDelay)
			commands := pass()
			if !reflect.DeepEqual(commands, []string{`[con_id=41] move container to workspace "` + sessionstate.RestoreStagingWorkspace + `"`}) {
				t.Fatalf("first structural pass = %q", commands)
			}
			feedbackScenarioFirstMoveEvents(t, c.events)
			assertSaved(saved)

			if checkpoint == "second_workspace_temporary_mark" {
				// First prove that successful daemon feedback permits continued
				// reconstruction. Then carry the same run into the second saved
				// workspace before injecting true user intent.
				c.drain(runtime, now)
				if runtime.startupComplete || runtime.restoreProgress == nil {
					t.Fatal("implicit source-sibling focus cancelled reconstruction")
				}
				reached := false
				withholdFeedback := false
				for range 48 {
					commands = pass()
					assertSaved(saved)
					if slices.Contains(commands, `[con_id=44] move container to workspace "`+sessionstate.RestoreStagingWorkspace+`"`) {
						withholdFeedback = true
					}
					if slices.ContainsFunc(commands, func(command string) bool {
						return strings.HasPrefix(command, "[con_id=202] mark --add \"_sway_session_restore_")
					}) {
						reached = true
						break
					}
					if !withholdFeedback {
						c.drain(runtime, now)
					}
					if runtime.startupComplete {
						t.Fatal("daemon feedback cancelled before the second temporary mark")
					}
				}
				if !reached {
					t.Fatalf("never reached second workspace's temporary mark: %q", c.commands)
				}
				if group := findContainerByID(observe(), 201); group == nil || group.Layout != "tabbed" || len(group.Nodes) != 2 || len(group.Marks) != 0 {
					t.Fatalf("first workspace did not finish before second workspace: %+v", group)
				}
				if len(c.events) != 12 {
					t.Fatalf("second workspace must retain six move/barrier pairs before binding, got %+v", c.events)
				}
				// A user-owned mark on the partially built group must survive
				// removal of the daemon's independently owned temporary mark.
				findContainerByID(c.root, 202).Marks = append(findContainerByID(c.root, 202).Marks, "feedback-user-group")
			}

			// The binding arrives ahead of queued successful-command feedback,
			// just as it can while a staging request is in flight. The user then
			// moves a different owned window; cleanup must respect that new tree.
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding, Change: "run"}, now)
			userIndex := 2
			if checkpoint == "second_workspace_temporary_mark" {
				userIndex = 5
			}
			userWindow := leaves[userIndex]
			c.move(userWindow, "100")
			for _, ws := range c.root.Nodes[0].Nodes {
				ws.Focus = slices.DeleteFunc(ws.Focus, func(id int64) bool { return id == userWindow.ID })
				if ws.Name == "100" {
					ws.Focus = []int64{userWindow.ID}
				}
			}
			c.events = append(c.events, swayipc.Event{Type: swayipc.EventWindow, Change: "move", Container: feedbackScenarioTreeCopy(t, userWindow)})
			c.drain(runtime, now)
			if runtime.restoreProgress != nil || !runtime.startupComplete {
				t.Fatal("binding cancellation did not survive queued daemon feedback")
			}
			if err := runtime.Flush(now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			assertSaved(saved)
			beforeCleanup := len(c.commands)
			pass()
			c.drain(runtime, now)
			if err := runtime.Flush(now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			assertSaved(saved)
			// Cleanup itself emits attributed move/barrier feedback. Continue
			// through capture and several extra observations to expose rearming.
			for range 12 {
				pass()
				c.drain(runtime, now)
			}
			wantCleanup := []string{`[con_id=41] move container to workspace "98"`}
			if checkpoint == "second_workspace_temporary_mark" {
				// The planner's mark is deterministic, but use the observed add
				// command to assert the exact matching unmark without duplicating
				// the private hash algorithm here.
				wantCleanup = []string{strings.Replace(commands[0], "mark --add", "unmark", 1)}
			}
			if !reflect.DeepEqual(c.commands[beforeCleanup:], wantCleanup) {
				t.Fatalf("post-binding commands = %q, want only cleanup %q", c.commands[beforeCleanup:], wantCleanup)
			}
			if runtime.restoreCleanupPending || runtime.restoreProgress != nil {
				t.Fatal("cleanup did not converge")
			}
			assertSaved(saved)
			feedbackScenarioEqualTree(t, observe(), feedbackScenarioExpectedTree(t, ids, checkpoint))
			if err := runtime.Flush(now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			wantLayout := feedbackScenarioExpectedLayout(ids, checkpoint)
			assertSaved(wantLayout)
			before := len(c.commands)
			for range 3 {
				pass()
				c.drain(runtime, now)
			}
			if err := runtime.Flush(now.Add(2 * time.Hour)); err != nil {
				t.Fatal(err)
			}
			if len(c.commands) != before {
				t.Fatalf("settled observation rearmed commands: %q", c.commands[before:])
			}
			feedbackScenarioEqualTree(t, observe(), feedbackScenarioExpectedTree(t, ids, checkpoint))
			assertSaved(wantLayout)
		})
	}
}

func feedbackScenarioFirstMoveEvents(t *testing.T, events []swayipc.Event) {
	t.Helper()
	if len(events) != 3 || events[0].Type != swayipc.EventWindow || events[0].Change != "move" || events[0].Container == nil || events[0].Container.ID != 41 ||
		events[1].Type != swayipc.EventWindow || events[1].Change != "focus" || events[1].Container == nil || events[1].Container.ID != 42 ||
		events[2].Type != swayipc.EventTick {
		t.Fatalf("staging did not queue move(41), implicit focus(42), barrier: %+v", events)
	}
	if _, ok := moveBarrierSequence(events[2].Payload); !ok {
		t.Fatalf("invalid attribution barrier: %q", events[2].Payload)
	}
}

func feedbackScenarioEqual(t *testing.T, label string, got, want any) {
	t.Helper()
	encode := func(value any) string {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	if actual, expected := encode(got), encode(want); actual != expected {
		t.Fatalf("%s:\n got %s\nwant %s", label, actual, expected)
	}
}

func feedbackScenarioEqualTree(t *testing.T, got, want *Node) {
	t.Helper()
	// Empty IPC arrays and nil Go slices have the same tree meaning. Compare
	// every node field, including IDs, order, identity, focus, and all marks.
	var normalize func(*Node)
	normalize = func(node *Node) {
		node.Marks = append([]string{}, node.Marks...)
		node.Focus = append([]int64{}, node.Focus...)
		node.Nodes = append([]*Node{}, node.Nodes...)
		node.FloatingNodes = append([]*Node{}, node.FloatingNodes...)
		for _, child := range append(slices.Clone(node.Nodes), node.FloatingNodes...) {
			normalize(child)
		}
	}
	normalize(got)
	normalize(want)
	feedbackScenarioEqual(t, "observed tree", got, want)
}

func feedbackScenarioExpectedTree(t *testing.T, ids []sessionstate.ContextID, checkpoint string) *Node {
	t.Helper()
	leaves := make([]*Node, len(ids))
	for index, id := range ids {
		leaves[index] = managedDaemonLeaf(t, int64(41+index), id)
		leaves[index].Marks = append([]string{"feedback-preserve"}, leaves[index].Marks...)
	}
	first := &Node{ID: 3, Type: "workspace", Name: "98", Layout: "splith", Nodes: []*Node{leaves[1], leaves[0]}, Focus: []int64{42, 41}}
	second := &Node{ID: 4, Type: "workspace", Name: "99", Layout: "splith", Nodes: leaves[3:], Focus: []int64{44, 45, 46}}
	user := leaves[2]
	leaves[1].Focused = true
	if checkpoint == "second_workspace_temporary_mark" {
		leaves[1].Focused = false
		first.Nodes = []*Node{{ID: 200, Type: "con", Layout: "splith", Nodes: []*Node{
			{ID: 201, Type: "con", Layout: "tabbed", Nodes: leaves[:2], Focus: []int64{41, 42}}, leaves[2],
		}, Focus: []int64{201, 43}}}
		first.Focus = []int64{200}
		second.Nodes = []*Node{{ID: 202, Type: "con", Layout: "splith", Marks: []string{"feedback-user-group"}, Nodes: []*Node{leaves[3]}, Focus: []int64{44}}, leaves[4]}
		second.Focus = []int64{202, 45}
		user = leaves[5]
	}
	return &Node{ID: 1, Type: "root", Nodes: []*Node{{ID: 2, Type: "output", Nodes: []*Node{
		first, second,
		{ID: 102, Type: "workspace", Name: sessionstate.RestoreStagingWorkspace, Layout: "splith"},
		{ID: 103, Type: "workspace", Name: "100", Layout: "splith", Nodes: []*Node{user}, Focus: []int64{user.ID}},
	}}}}
}

func feedbackScenarioExpectedLayout(ids []sessionstate.ContextID, checkpoint string) sessionstate.LayoutSnapshot {
	leaf := func(index int) sessionstate.LayoutNode { return sessionstate.LayoutNode{ContextID: &ids[index]} }
	first := sessionstate.WorkspaceLayout{Name: "98", RestoreMode: sessionstate.WorkspaceRestoreLayout,
		Tiling: &sessionstate.LayoutNode{Layout: sessionstate.LayoutSplitHorizontal, Children: []sessionstate.LayoutNode{leaf(1), leaf(0)}}, FocusedContext: &ids[1]}
	second := exactDaemonSnapshot("99", ids[3:]...).Workspaces[0]
	second.FocusedContext = &ids[3]
	userIndex := 2
	if checkpoint == "second_workspace_temporary_mark" {
		first.Tiling = &sessionstate.LayoutNode{Layout: sessionstate.LayoutSplitHorizontal, Children: []sessionstate.LayoutNode{
			{Layout: sessionstate.LayoutTabbed, Children: []sessionstate.LayoutNode{leaf(0), leaf(1)}}, leaf(2),
		}}
		first.FocusedContext = &ids[0]
		second.Tiling = &sessionstate.LayoutNode{Layout: sessionstate.LayoutSplitHorizontal, Children: []sessionstate.LayoutNode{
			{Layout: sessionstate.LayoutSplitHorizontal, Children: []sessionstate.LayoutNode{leaf(3)}}, leaf(4),
		}}
		userIndex = 5
	}
	user := sessionstate.WorkspaceLayout{Name: "100", RestoreMode: sessionstate.WorkspaceRestoreLayout, Tiling: new(sessionstate.LayoutNode), FocusedContext: &ids[userIndex]}
	*user.Tiling = leaf(userIndex)
	return sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{user, first, second}}
}
