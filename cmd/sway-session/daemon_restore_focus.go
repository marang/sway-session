package main

import (
	"fmt"
	"slices"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// Sway focus events have no command-origin token. Match only a predicted,
// ordered transition, within the command's stream epoch and tick barrier.
// A coincident user action with the identical tuple is not distinguishable by
// IPC; bindings and unmatched workspace transitions retain precedence.
type restoreFocusEvent struct {
	kind                 swayipc.EventType
	container, old, next int64
	// Names are used only for workspaces created by this command, whose IDs
	// cannot be observed beforehand, including a recreated empty workspace.
	oldName, nextName string
}

type restoreFocusExpectation struct {
	sequence, epoch uint64
	afterMove       bool
	events          []restoreFocusEvent
}

func (runtime *sessionRuntime) runAttributedCommand(root *Node, action sessionstate.RestoreAction, command string, move bool) error {
	events := predictedRestoreFocus(root, action)
	if move {
		return runtime.runAttributedCommandFocus(action.ContainerID, command, true, nil, events)
	}
	return runtime.runAttributedCommandFocus(action.ContainerID, command, false, events, nil)
}

// Scratchpad show focuses before emitting move; ordinary moves focus after it.
// Keep the two portions ordered under the same command sequence and barrier.
func (runtime *sessionRuntime) runAttributedCommandFocus(containerID int64, command string, move bool, beforeMove, afterMove []restoreFocusEvent) error {
	allowances := 0
	for _, events := range [][]restoreFocusEvent{beforeMove, afterMove} {
		if len(events) != 0 {
			allowances++
		}
	}
	if len(runtime.expectedFocus)+allowances > 64 {
		runtime.cancelConflictingRestore()
		return fmt.Errorf("restore focus attribution backlog exceeded")
	}
	sequence := uint64(0)
	if move {
		sequence = runtime.expectMove(containerID)
	} else if allowances != 0 {
		runtime.nextMoveSequence++
		sequence = runtime.nextMoveSequence
	}
	for index, events := range [][]restoreFocusEvent{beforeMove, afterMove} {
		if len(events) != 0 {
			runtime.expectedFocus = append(runtime.expectedFocus, restoreFocusExpectation{
				sequence: sequence, epoch: runtime.eventStreamEpoch, afterMove: index == 1, events: events,
			})
		}
	}
	err := runtime.runSwayCommand(command)
	if err == nil && sequence != 0 {
		if barrierErr := runtime.sendMoveBarrier(sequence); barrierErr != nil {
			err = &swayipc.CommandOutcomeUnknownError{Cause: fmt.Errorf("establish command attribution barrier: %w", barrierErr)}
		}
	}
	if err != nil && sequence != 0 {
		runtime.handleFailedMove(containerID, sequence, err)
	}
	return err
}

func (runtime *sessionRuntime) expireRestoreFocus(barrier uint64) {
	runtime.expectedFocus = slices.DeleteFunc(runtime.expectedFocus, func(e restoreFocusExpectation) bool {
		return e.sequence <= barrier
	})
}

func (runtime *sessionRuntime) consumeRestoreFocus(event swayipc.Event) bool {
	if runtime.eventStreamState != nil {
		epoch, connected := runtime.eventStreamState.Snapshot()
		if !connected || epoch != runtime.eventStreamEpoch {
			return false
		}
	}
	if len(runtime.expectedFocus) == 0 {
		return false
	}
	expected := &runtime.expectedFocus[0]
	if expected.afterMove || expected.epoch != runtime.eventStreamEpoch ||
		event.StreamEpoch != 0 && event.StreamEpoch != expected.epoch {
		return false
	}
	want := expected.events[0]
	if want.kind != event.Type {
		return false
	}
	if event.Type == swayipc.EventWindow {
		if event.Container == nil || event.Container.ID != want.container {
			return false
		}
	} else if !matchesRestoreWorkspace(event.Old, want.old, want.oldName) || !matchesRestoreWorkspace(event.Current, want.next, want.nextName) {
		return false
	}
	expected.events = expected.events[1:]
	if len(expected.events) == 0 {
		runtime.expectedFocus = runtime.expectedFocus[1:]
	}
	return true
}

func matchesRestoreWorkspace(node *Node, id int64, name string) bool {
	if node == nil || node.ID <= 0 {
		return false
	}
	if id > 0 {
		return node.ID == id
	}
	return name != "" && node.Type == "workspace" && node.Name == name
}

func (runtime *sessionRuntime) discardRestoreFocus(sequence uint64) {
	runtime.expectedFocus = slices.DeleteFunc(runtime.expectedFocus, func(e restoreFocusExpectation) bool {
		return e.sequence == sequence
	})
}

func predictedRestoreFocus(root *Node, action sessionstate.RestoreAction) []restoreFocusEvent {
	if root == nil {
		return nil
	}
	path := containerPath(root, action.ContainerID)
	if len(path) == 0 {
		return nil
	}
	target := path[len(path)-1]
	workspace := pathWorkspace(path)
	isGroup := len(target.Nodes)+len(target.FloatingNodes) != 0
	if workspace == nil || target.Type != "con" && target.Type != "floating_con" || isGroup && action.Kind != sessionstate.RestoreSetFullscreen {
		return nil
	}
	switch action.Kind {
	case sessionstate.RestoreMoveWorkspace, sessionstate.RestoreMoveToMark:
		if !target.Focused || action.Kind == sessionstate.RestoreMoveWorkspace && action.Target == workspace.Name {
			return nil
		}
		if action.Kind == sessionstate.RestoreMoveToMark {
			marked := uniqueMarkedContainer(root, action.Target)
			if marked == nil || pathWorkspace(containerPath(root, marked.ID)) == workspace {
				return nil
			}
		}
		if next := focusLeafExcept(workspace, target.ID); next != nil {
			return []restoreFocusEvent{{kind: swayipc.EventWindow, container: next.ID}}
		}
	case sessionstate.RestoreFocus, sessionstate.RestoreSetFullscreen:
		if target.Focused || action.Kind == sessionstate.RestoreSetFullscreen && action.Fullscreen == sessionstate.FullscreenNone {
			return nil
		}
		previous := pathWorkspace(focusedTreePath(root))
		if previous == nil || action.Kind == sessionstate.RestoreSetFullscreen &&
			action.Fullscreen == sessionstate.FullscreenWorkspace && previous != workspace {
			return nil
		}
		var events []restoreFocusEvent
		if previous != workspace {
			events = append(events, restoreFocusEvent{kind: swayipc.EventWorkspace, old: previous.ID, next: workspace.ID})
		}
		if isGroup {
			// Sway focuses the container node, which has no view, so only the
			// workspace transition is emitted (no descendant window focus).
			return events
		}
		return append(events, restoreFocusEvent{kind: swayipc.EventWindow, container: target.ID})
	}
	return nil
}

func containerPath(root *Node, id int64) []*Node {
	if root == nil {
		return nil
	}
	if root.ID == id {
		return []*Node{root}
	}
	for _, children := range [][]*Node{root.Nodes, root.FloatingNodes} {
		for _, child := range children {
			if path := containerPath(child, id); len(path) != 0 {
				return append([]*Node{root}, path...)
			}
		}
	}
	return nil
}

func focusedTreePath(root *Node) []*Node {
	if root == nil {
		return nil
	}
	for _, children := range [][]*Node{root.Nodes, root.FloatingNodes} {
		for _, child := range children {
			if path := focusedTreePath(child); len(path) != 0 {
				return append([]*Node{root}, path...)
			}
		}
	}
	if root.Focused {
		return []*Node{root}
	}
	return nil
}

func pathWorkspace(path []*Node) *Node {
	for _, node := range path {
		if node.Type == "workspace" {
			return node
		}
	}
	return nil
}

func focusLeafExcept(root *Node, excluded int64) *Node {
	if root.ID == excluded {
		return nil
	}
	if (root.Type == "con" || root.Type == "floating_con") && len(root.Nodes)+len(root.FloatingNodes) == 0 {
		return root
	}
	// Sway's focus stack, not child order, determines the successor. Missing
	// stack information cannot justify an allowance for an arbitrary focus.
	for _, id := range root.Focus {
		for _, children := range [][]*Node{root.Nodes, root.FloatingNodes} {
			for _, child := range children {
				if child.ID == id {
					if leaf := focusLeafExcept(child, excluded); leaf != nil {
						return leaf
					}
				}
			}
		}
	}
	return nil
}

func uniqueMarkedContainer(root *Node, mark string) *Node {
	var result *Node
	count := 0
	var visit func(*Node)
	visit = func(node *Node) {
		if slices.Contains(node.Marks, mark) {
			result = node
			count++
		}
		for _, children := range [][]*Node{node.Nodes, node.FloatingNodes} {
			for _, child := range children {
				visit(child)
			}
		}
	}
	visit(root)
	if count != 1 {
		return nil
	}
	return result
}
