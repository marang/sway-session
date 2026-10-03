package main

import (
	"errors"
	"fmt"
	"strings"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// Adding an ordinary container to the scratchpad changes focus even when the
// target was on an inactive workspace. Restore that original focus explicitly.
func (runtime *sessionRuntime) hideRestoredScratchpad(root *Node, action sessionstate.PlacementAction) error {
	if err := runtime.validateScratchpadAnchor(root, action); err != nil {
		return err
	}
	path := containerPath(root, action.ContainerID)
	source := pathWorkspace(path)
	if len(path) < 4 || source == nil || !literalScratchpadWorkspace(source.Name) || !scratchpadFocusView(path[len(path)-1]) || path[len(path)-1].FullscreenMode != 0 {
		return fmt.Errorf("scratchpad hide requires an observed normal leaf: %w", errLifecyclePlanChanged)
	}
	target, parent := path[len(path)-1], path[len(path)-2]
	originalPath := focusedTreePath(root)
	original := pathWorkspace(originalPath)
	if original == nil || !literalScratchpadWorkspace(original.Name) {
		return errors.New("scratchpad hide requires original focus observation")
	}
	focus := originalPath[len(originalPath)-1]
	if focus == original && len(original.Nodes)+len(original.FloatingNodes) != 0 {
		return errors.New("scratchpad hide cannot preserve non-empty workspace node focus")
	}
	if focus != original && !scratchpadFocusView(focus) {
		return errors.New("scratchpad hide cannot preserve original group focus")
	}
	command := fmt.Sprintf("[con_id=%d] move scratchpad", target.ID)
	var beforeMove, afterMove []restoreFocusEvent
	// Adding membership always picks the source workspace's inactive focus;
	// hiding an existing member does so only if that member held focus.
	changesFocus := !targetScratchpadMember(target) || target.Focused
	if changesFocus {
		next := focusLeafExcept(parent, target.ID)
		if next == nil {
			next = focusLeafExcept(source, target.ID)
		}
		if next == nil && scratchpadOtherViews(source, target.ID) != 0 {
			return errors.New("scratchpad hide cannot predict source focus")
		}
		if source != original {
			beforeMove = append(beforeMove, restoreFocusEvent{kind: swayipc.EventWorkspace, old: original.ID, next: source.ID})
		}
		if next != nil && next != focus {
			beforeMove = append(beforeMove, restoreFocusEvent{kind: swayipc.EventWindow, container: next.ID})
		}
		if focus != target {
			if focus != original {
				command += fmt.Sprintf("; [con_id=%d] focus", focus.ID)
			} else {
				command += "; " + scratchpadWorkspaceCommand(original.Name)
			}
			if source != original {
				originalID := original.ID
				if focus == original && len(original.Nodes)+len(original.FloatingNodes) == 0 {
					originalID = 0
				}
				afterMove = append(afterMove, restoreFocusEvent{kind: swayipc.EventWorkspace, old: source.ID, next: originalID, nextName: original.Name})
			}
			if focus != original && next != focus {
				afterMove = append(afterMove, restoreFocusEvent{kind: swayipc.EventWindow, container: focus.ID})
			}
		}
	}
	err := runtime.runAttributedCommandFocus(target.ID, command, true, beforeMove, afterMove)
	if err != nil && len(afterMove) != 0 {
		runtime.cancelConflictingRestore()
		return &swayipc.CommandOutcomeUnknownError{Cause: err}
	}
	return err
}

func scratchpadOtherViews(node *Node, excluded int64) int {
	if node.ID == excluded {
		return 0
	}
	if scratchpadFocusView(node) {
		return 1
	}
	count := 0
	for _, children := range [][]*Node{node.Nodes, node.FloatingNodes} {
		for _, child := range children {
			count += scratchpadOtherViews(child, excluded)
		}
	}
	return count
}

func targetScratchpadMember(node *Node) bool {
	return node.ScratchpadState == "fresh" || node.ScratchpadState == "changed"
}

// showRestoredScratchpad runs within applyPlacementAction's lifecycle guard.
// The planner must obtain a fresh hidden observation before requesting show.
// An absolute hide also protects show from a visibility change between that
// observation and the command; this is not an atomic Sway compare-and-swap.
func (runtime *sessionRuntime) showRestoredScratchpad(root *Node, action sessionstate.PlacementAction) error {
	placement, saved := savedScratchpadPlacement(runtime.desired, action.ContextID)
	normalWorkspace, normal := snapshotContextWorkspace(runtime.desired, action.ContextID)
	if action.Kind != sessionstate.PlacementShowScratchpad || !(saved && placement.Visible && placement.Workspace == action.Workspace || normal && normalWorkspace == action.Workspace) {
		return fmt.Errorf("scratchpad visibility plan changed: %w", errLifecyclePlanChanged)
	}
	if err := runtime.validateScratchpadAnchor(root, action); err != nil {
		return err
	}
	command, beforeMove, afterMove, err := restoredScratchpadCommand(root, action)
	if err != nil {
		return err
	}
	if err := runtime.runAttributedCommandFocus(action.ContainerID, command, true, beforeMove, afterMove); err != nil {
		// A composed command is not transactional: even an explicit rejection
		// can follow a successful show. Observe again, never retry the toggle.
		clear(runtime.expectedMoves)
		runtime.cancelConflictingRestore()
		var unknown *swayipc.CommandOutcomeUnknownError
		var invalid *swayipc.CommandResponseInvalidError
		if errors.As(err, &unknown) || errors.As(err, &invalid) {
			return err
		}
		return &swayipc.CommandOutcomeUnknownError{Cause: fmt.Errorf("restore scratchpad command requires observation: %w", err)}
	}
	return nil
}

// Leaving the scratchpad is absolute and only permitted after show has been
// observed. Sway removes membership when a shown floating leaf becomes tiled.
// Any saved floating layout is applied by the subsequent structural restore.
func (runtime *sessionRuntime) leaveRestoredScratchpad(root *Node, action sessionstate.PlacementAction) error {
	name, saved := snapshotContextWorkspace(runtime.desired, action.ContextID)
	if !saved || name != action.Workspace {
		return fmt.Errorf("normal placement plan changed: %w", errLifecyclePlanChanged)
	}
	if err := runtime.validateScratchpadAnchor(root, action); err != nil {
		return err
	}
	path := containerPath(root, action.ContainerID)
	if len(path) != 4 || path[2].Type != "workspace" || path[2].Name != name || !literalScratchpadWorkspace(name) {
		return fmt.Errorf("scratchpad exit requires a shown top-level leaf: %w", errLifecyclePlanChanged)
	}
	leaf := path[3]
	if !scratchpadFocusView(leaf) || leaf.ScratchpadState != "fresh" && leaf.ScratchpadState != "changed" || !containsScratchpadNode(path[2].FloatingNodes, leaf) || leaf.FullscreenMode != 0 {
		return fmt.Errorf("scratchpad exit observation changed: %w", errLifecyclePlanChanged)
	}
	return runtime.runAttributedCommandFocus(leaf.ID, fmt.Sprintf("[con_id=%d] floating disable", leaf.ID), true, nil, nil)
}

func savedScratchpadPlacement(snapshot sessionstate.LayoutSnapshot, id sessionstate.ContextID) (sessionstate.ScratchpadPlacement, bool) {
	var result sessionstate.ScratchpadPlacement
	count := 0
	for _, placement := range snapshot.Scratchpad {
		if placement.ContextID == id {
			result = placement
			count++
		}
	}
	return result, count == 1
}

func (runtime *sessionRuntime) validateScratchpadAnchor(root *Node, action sessionstate.PlacementAction) error {
	if root == nil {
		return fmt.Errorf("scratchpad observation is missing: %w", errLifecyclePlanChanged)
	}
	for _, item := range runtime.registry.Contexts {
		if item.ID != action.ContextID {
			continue
		}
		if item.App == nil || !sessionstate.EvaluateRestorePolicy(item).Eligible {
			break
		}
		groups, err := sessionstate.ObserveApplicationGroups(root, runtime.registry)
		if err != nil {
			return err
		}
		group := groups[item.ID]
		if !group.Ambiguous && !group.AnchorMarked && group.Anchor != nil && group.Anchor.ContainerID == action.ContainerID {
			return nil
		}
		break
	}
	return fmt.Errorf("scratchpad anchor changed: %w", errLifecyclePlanChanged)
}

// Sway 1652c, tree/root.c and commands/scratchpad.c: show attaches the hidden
// top-level container to the focused workspace, focuses it, then emits move.
// A single IPC command keeps selection, show, and original focus together.
func restoredScratchpadCommand(root *Node, action sessionstate.PlacementAction) (string, []restoreFocusEvent, []restoreFocusEvent, error) {
	if root == nil || root.Type != "root" || action.ContainerID <= 0 || !literalScratchpadWorkspace(action.Workspace) {
		return "", nil, nil, fmt.Errorf("invalid scratchpad show target")
	}
	// Reject duplicate IDs and nil nodes before using first-match tree helpers.
	ids := make(map[int64]struct{})
	focused := 0
	var validate func(*Node) error
	validate = func(node *Node) error {
		if node == nil {
			return errors.New("scratchpad tree contains a nil node")
		}
		if node.ID > 0 {
			if _, duplicate := ids[node.ID]; duplicate {
				return errors.New("scratchpad tree contains duplicate node IDs")
			}
			ids[node.ID] = struct{}{}
		}
		if node.Focused {
			focused++
		}
		if scratchpadContainerType(node) && node.FullscreenMode == 2 {
			return errors.New("cannot restore scratchpad visibility during global fullscreen")
		}
		for _, children := range [][]*Node{node.Nodes, node.FloatingNodes} {
			for _, child := range children {
				if err := validate(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := validate(root); err != nil {
		return "", nil, nil, err
	}
	path := containerPath(root, action.ContainerID)
	if len(path) != 4 || path[1].Type != "output" || path[1].Name != "__i3" || path[2].Type != "workspace" || path[2].Name != "__i3_scratch" {
		return "", nil, nil, fmt.Errorf("scratchpad target is no longer hidden: %w", errLifecyclePlanChanged)
	}
	target := path[3]
	if !scratchpadContainerType(target) || len(target.Nodes)+len(target.FloatingNodes) != 0 || target.Focused ||
		(target.ScratchpadState != "fresh" && target.ScratchpadState != "changed") ||
		!containsScratchpadNode(path[2].FloatingNodes, target) || target.FullscreenMode != 0 {
		return "", nil, nil, fmt.Errorf("scratchpad target is not an observed hidden leaf: %w", errLifecyclePlanChanged)
	}
	for _, mark := range target.Marks {
		if strings.HasPrefix(mark, sessionstate.MarkPrefix) {
			return "", nil, nil, fmt.Errorf("scratchpad target is already adopted: %w", errLifecyclePlanChanged)
		}
	}
	originalPath := focusedTreePath(root)
	original := pathWorkspace(originalPath)
	if focused != 1 || original == nil || original.ID <= 0 || !literalScratchpadWorkspace(original.Name) {
		return "", nil, nil, errors.New("scratchpad show requires an unambiguous original workspace focus")
	}
	originalFocus := originalPath[len(originalPath)-1]
	if originalFocus != original && (originalFocus.ID <= 0 || !scratchpadContainerType(originalFocus) ||
		len(originalFocus.Nodes)+len(originalFocus.FloatingNodes) == 0 && !scratchpadFocusView(originalFocus)) {
		return "", nil, nil, errors.New("scratchpad show cannot preserve the observed original focus")
	}
	var destination *Node
	var findWorkspace func(*Node) error
	findWorkspace = func(node *Node) error {
		if node.Type == "workspace" && strings.EqualFold(node.Name, action.Workspace) {
			if destination != nil || node.Name != action.Workspace || node.ID <= 0 {
				return errors.New("scratchpad destination workspace is ambiguous")
			}
			destination = node
		}
		for _, children := range [][]*Node{node.Nodes, node.FloatingNodes} {
			for _, child := range children {
				if err := findWorkspace(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := findWorkspace(root); err != nil {
		return "", nil, nil, err
	}
	destinationID := int64(0)
	if destination != nil {
		destinationID = destination.ID
		var fullscreen func(*Node) bool
		fullscreen = func(node *Node) bool {
			// Sway reports fullscreen_mode=1 on every workspace node; only
			// container fullscreen indicates a view that show could disrupt.
			if scratchpadContainerType(node) && node.FullscreenMode != 0 {
				return true
			}
			for _, children := range [][]*Node{node.Nodes, node.FloatingNodes} {
				for _, child := range children {
					if fullscreen(child) {
						return true
					}
				}
			}
			return false
		}
		if fullscreen(destination) {
			return "", nil, nil, errors.New("scratchpad show would disable destination fullscreen")
		}
	}
	// commands/move.c makes this an absolute hide, with no event if already
	// hidden. If live visibility changed, its unmatched focus feedback still
	// cancels reconstruction; no universal focus allowance is granted.
	commands := []string{fmt.Sprintf("[con_id=%d] move scratchpad", target.ID)}
	var beforeMove, afterMove []restoreFocusEvent
	if destination != original {
		beforeMove = append(beforeMove, restoreFocusEvent{
			kind: swayipc.EventWorkspace, old: original.ID, next: destinationID, nextName: action.Workspace,
		})
		if destination != nil && len(destination.Nodes)+len(destination.FloatingNodes) != 0 {
			leaf := focusLeafExcept(destination, 0)
			if leaf == nil || leaf.ID <= 0 || !scratchpadFocusView(leaf) {
				return "", nil, nil, errors.New("scratchpad destination focus stack is incomplete")
			}
			// Explicitly focus the observed leaf: workspace selection can
			// instead select an inactive split node, which emits no window
			// focus and cannot be distinguished by child focus-stack order.
			commands = append(commands, fmt.Sprintf("[con_id=%d] focus", leaf.ID))
			beforeMove = append(beforeMove, restoreFocusEvent{kind: swayipc.EventWindow, container: leaf.ID})
		} else {
			commands = append(commands, scratchpadWorkspaceCommand(action.Workspace))
		}
	}
	commands = append(commands, fmt.Sprintf("[con_id=%d] scratchpad show", target.ID))
	beforeMove = append(beforeMove, restoreFocusEvent{kind: swayipc.EventWindow, container: target.ID})
	if originalFocus != original {
		commands = append(commands, fmt.Sprintf("[con_id=%d] focus", originalFocus.ID))
	} else if destination == original {
		// Workspace selection would focus the newly shown scratchpad instead.
		// The verified top-level leaf's parent is the original empty workspace.
		commands = append(commands, fmt.Sprintf("[con_id=%d] focus parent", target.ID))
	} else if len(original.Nodes)+len(original.FloatingNodes) != 0 {
		// The workspace itself can be focused even with existing windows.
		// Focus a verified top-level node's parent without visiting its view.
		children := original.Nodes
		if len(children) == 0 {
			children = original.FloatingNodes
		}
		if children[0].ID <= 0 || !scratchpadContainerType(children[0]) {
			return "", nil, nil, errors.New("scratchpad show cannot preserve original workspace focus")
		}
		commands = append(commands, fmt.Sprintf("[con_id=%d] focus parent", children[0].ID))
	} else {
		commands = append(commands, scratchpadWorkspaceCommand(original.Name))
	}
	if destination != original {
		originalID := original.ID
		if originalFocus == original && len(original.Nodes)+len(original.FloatingNodes) == 0 {
			// An empty workspace may be destroyed on switching away and gets
			// recreated by the final workspace command, with a new ID.
			originalID = 0
		}
		afterMove = append(afterMove, restoreFocusEvent{
			kind: swayipc.EventWorkspace, old: destinationID, oldName: action.Workspace, next: originalID, nextName: original.Name,
		})
	}
	if scratchpadFocusView(originalFocus) {
		afterMove = append(afterMove, restoreFocusEvent{kind: swayipc.EventWindow, container: originalFocus.ID})
	}
	return strings.Join(commands, "; "), beforeMove, afterMove, nil
}

func scratchpadFocusView(node *Node) bool {
	return scratchpadContainerType(node) && len(node.Nodes)+len(node.FloatingNodes) == 0 &&
		(node.AppID != nil || node.Window != nil || node.Shell != "")
}

func scratchpadContainerType(node *Node) bool {
	return node.Type == "con" || node.Type == "floating_con"
}

func containsScratchpadNode(nodes []*Node, wanted *Node) bool {
	for _, node := range nodes {
		if node == wanted {
			return true
		}
	}
	return false
}

func scratchpadWorkspaceCommand(name string) string {
	return "workspace --no-auto-back-and-forth " + quoteSwayString(name)
}

func literalScratchpadWorkspace(name string) bool {
	if name == "" || len(name) > 256 || strings.TrimSpace(name) != name || name == "__i3_scratch" || name == sessionstate.RestoreStagingWorkspace {
		return false
	}
	for _, character := range name {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	// Even quoted single-token keywords retain workspace command semantics.
	switch strings.ToLower(name) {
	case "next", "prev", "next_on_output", "prev_on_output", "current", "back_and_forth", "number", "output", "gaps", "--no-auto-back-and-forth":
		return false
	}
	return true
}
