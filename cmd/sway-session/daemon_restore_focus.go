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
// IPC; bindings and every unmatched transition always retain precedence.
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
	mapping         bool
	events          []restoreFocusEvent
}

type restoreMappingFocus struct {
	sequence, epoch uint64
}

type restoreMappingCandidate struct {
	contextID  sessionstate.ContextID
	attributed bool
}

// Identity observation must precede effect locks: login's one-shot restore
// can hold the registry lock while its terminals map. Waiting for application
// reconciliation or mark dispatch would misclassify their queued map focus.
// This grants only the exact new window's bounded focus allowance; it neither
// makes a context restore-eligible nor bypasses any lifecycle effect guard.
func (runtime *sessionRuntime) observeRestoreMappingFocus(root *Node, registry sessionstate.Registry) error {
	if runtime.restoreCancelled || len(runtime.desired.Workspaces)+len(runtime.desired.Scratchpad) == 0 {
		return nil
	}
	windows, issues, err := sessionstate.ObserveManagedWindowsIsolated(root, registry)
	if err != nil {
		return err
	}
	groups, err := sessionstate.ObserveApplicationGroupsForCapture(root, registry)
	if err != nil {
		return err
	}
	unsafe := make(map[sessionstate.ContextID]struct{}, len(issues))
	for _, issue := range issues {
		unsafe[issue.ContextID] = struct{}{}
	}
	nodes := make(map[int64]*Node)
	var indexNodes func(*Node)
	indexNodes = func(node *Node) {
		if node.ID > 0 {
			if _, duplicate := nodes[node.ID]; duplicate {
				nodes[node.ID] = nil
			} else {
				nodes[node.ID] = node
			}
		}
		for _, children := range [][]*Node{node.Nodes, node.FloatingNodes} {
			for _, child := range children {
				indexNodes(child)
			}
		}
	}
	indexNodes(root)
	for _, item := range registry.Contexts {
		if !sessionstate.EvaluateRestorePolicy(item).Eligible {
			continue
		}
		if _, invalid := unsafe[item.ID]; invalid {
			continue
		}
		if _, alreadyEligible := runtime.restoreEligible[item.ID]; alreadyEligible {
			continue
		}
		if item.App != nil {
			group := groups[item.ID]
			if !group.Ambiguous && group.Anchor != nil && !group.AnchorMarked && nodes[group.Anchor.ContainerID] != nil {
				runtime.attributeMappingFocus(group.Anchor.ContainerID, item.ID)
			}
			continue
		}
		if window, exists := windows[item.ID]; exists {
			node := nodes[window.ContainerID]
			mark, err := item.ID.Mark()
			if err != nil {
				return err
			}
			if node != nil && !slices.Contains(node.Marks, mark) {
				runtime.attributeMappingFocus(window.ContainerID, item.ID)
			}
		}
	}
	known := make(map[int64]struct{})
	for _, window := range windows {
		known[window.ContainerID] = struct{}{}
	}
	for _, group := range groups {
		for _, window := range group.Windows {
			known[window.ContainerID] = struct{}{}
		}
	}
	runtime.observeUnregisteredMappingFocus(root, nodes, known)
	return nil
}

// A newly mapped window can emit focus before commands issued by the ensuing
// reconciliation. Reserve an ordering boundary at window::new, but authorize
// no focus until a fresh tree identifies a saved restore candidate or an
// unrelated view whose automatic mapping must not cancel reconstruction.
func (runtime *sessionRuntime) observeMappingFocus(node *Node) {
	if node == nil || node.ID <= 0 || runtime.restoreCancelled || len(runtime.desired.Workspaces)+len(runtime.desired.Scratchpad) == 0 {
		return
	}
	if len(runtime.pendingMappingFocus) >= 64 {
		runtime.cancelConflictingRestore()
		return
	}
	if runtime.pendingMappingFocus == nil {
		runtime.pendingMappingFocus = make(map[int64]restoreMappingFocus)
	}
	runtime.nextMoveSequence++
	sequence := runtime.nextMoveSequence
	runtime.pendingMappingFocus[node.ID] = restoreMappingFocus{sequence: sequence, epoch: runtime.eventStreamEpoch}
	if err := runtime.sendMoveBarrier(sequence); err != nil {
		runtime.cancelConflictingRestore()
		return
	}
	// A fresh GET_TREE can observe and adopt several windows before their
	// queued window::new events arrive. Reuse only that exact container's
	// validated adoption; never infer identity from the event alone.
	if candidate, exists := runtime.mappingCandidates[node.ID]; exists {
		runtime.attributeMappingFocus(node.ID, candidate.contextID)
	}
}

func (runtime *sessionRuntime) attributeMappingFocus(containerID int64, id sessionstate.ContextID) {
	if runtime.restoreCancelled {
		return
	}
	_, savedWorkspace := snapshotContextWorkspace(runtime.desired, id)
	_, savedScratchpad := savedScratchpadPlacement(runtime.desired, id)
	if !savedWorkspace && !savedScratchpad {
		return
	}
	candidate, adopted := runtime.mappingCandidates[containerID]
	if !adopted {
		if _, alreadySeen := runtime.restoreEligible[id]; alreadySeen {
			return
		}
		if runtime.mappingCandidates == nil {
			runtime.mappingCandidates = make(map[int64]restoreMappingCandidate)
		}
		candidate = restoreMappingCandidate{contextID: id}
		runtime.mappingCandidates[containerID] = candidate
	}
	if candidate.contextID != id || candidate.attributed {
		return
	}
	mapping, exists := runtime.pendingMappingFocus[containerID]
	if !exists || mapping.epoch != runtime.eventStreamEpoch {
		return
	}
	delete(runtime.pendingMappingFocus, containerID)
	if len(runtime.expectedFocus) >= 64 {
		runtime.cancelConflictingRestore()
		return
	}
	candidate.attributed = true
	runtime.mappingCandidates[containerID] = candidate
	expectation := restoreFocusExpectation{
		sequence: mapping.sequence, epoch: mapping.epoch, mapping: true,
		events: []restoreFocusEvent{{kind: swayipc.EventWindow, container: containerID}},
	}
	runtime.expectedFocus = append(runtime.expectedFocus, expectation)
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
	for id, mapping := range runtime.pendingMappingFocus {
		if mapping.sequence <= barrier {
			delete(runtime.pendingMappingFocus, id)
		}
	}
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
	// The compositor may have mapped B before commands triggered by new(A),
	// while new(B) is still queued. Its map-focus then precedes those command
	// events despite its barrier being issued later. Mapping allowances match
	// only their exact validated new window; command effects stay ordered.
	if event.Type == swayipc.EventWindow && event.Container != nil {
		for index, expected := range runtime.expectedFocus {
			if expected.mapping && expected.epoch == runtime.eventStreamEpoch &&
				(event.StreamEpoch == 0 || event.StreamEpoch == expected.epoch) &&
				expected.events[0].container == event.Container.ID {
				runtime.expectedFocus = slices.Delete(runtime.expectedFocus, index, index+1)
				return true
			}
		}
	}
	index := slices.IndexFunc(runtime.expectedFocus, func(e restoreFocusExpectation) bool { return !e.mapping })
	if index < 0 {
		return false
	}
	expected := &runtime.expectedFocus[index]
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
		runtime.expectedFocus = slices.Delete(runtime.expectedFocus, index, index+1)
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
