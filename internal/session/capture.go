package session

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/marang/sway-session/internal/swayipc"
)

type PlacementActionKind string

const (
	PlacementMoveWorkspace PlacementActionKind = "move_workspace"
	PlacementAddMark       PlacementActionKind = "add_mark"
	maxPlacementActions                        = 256
)

// PlacementAction is an absolute, idempotently plannable operation for a
// newly mapped managed window. Moves precede marks so a failed or ambiguous
// move remains discoverable by stable application ID on the next observation.
type PlacementAction struct {
	Kind        PlacementActionKind
	ContextID   ContextID
	ContainerID int64
	Workspace   string
}

type observedContext struct {
	id          ContextID
	containerID int64
	workspace   string
	hasMark     bool
}

type nodeCapture struct {
	layout        *LayoutNode
	managed       []ContextID
	hasUnmanaged  bool
	managedLeaves int
}

// CaptureLayout extracts registered managed windows and their restorable
// ancestors from a complete Sway tree. Mixed managed/unregistered layout
// subtrees degrade the containing workspace to placement-only state.
func CaptureLayout(root *swayipc.TreeNode, registry Registry) (LayoutSnapshot, error) {
	if root == nil {
		return LayoutSnapshot{}, errors.New("sway tree is nil")
	}
	if err := registry.Validate(); err != nil {
		return LayoutSnapshot{}, fmt.Errorf("validate context registry: %w", err)
	}
	registered := activeContextIDs(registry)
	workspaces, err := collectWorkspaces(root, false)
	if err != nil {
		return LayoutSnapshot{}, err
	}
	snapshot := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{}}

	for _, workspaceNode := range workspaces {
		workspace, include, err := captureWorkspace(workspaceNode, registered)
		if err != nil {
			return LayoutSnapshot{}, fmt.Errorf("capture workspace %q: %w", workspaceNode.Name, err)
		}
		if include {
			snapshot.Workspaces = append(snapshot.Workspaces, workspace)
		}
	}
	sort.Slice(snapshot.Workspaces, func(left, right int) bool {
		return snapshot.Workspaces[left].Name < snapshot.Workspaces[right].Name
	})
	if err := snapshot.Validate(); err != nil {
		return LayoutSnapshot{}, fmt.Errorf("validate captured layout: %w", err)
	}
	return snapshot, nil
}

// PlanPlacementActions recognizes registered stable application IDs which do
// not yet have their mark. It moves only those newly mapped windows to saved
// workspaces; already marked windows remain user-controlled and are captured
// at their current workspace instead of being moved back.
func PlanPlacementActions(root *swayipc.TreeNode, registry Registry, desired LayoutSnapshot) ([]PlacementAction, error) {
	return PlanPlacementActionsAfter(root, registry, desired, nil)
}

// PlanPlacementActionsAfter returns the next bounded, rotating action batch.
// Advancing the cursor after planning prevents a permanently rejected prefix
// from starving unrelated contexts on later reconciliation passes.
func PlanPlacementActionsAfter(
	root *swayipc.TreeNode,
	registry Registry,
	desired LayoutSnapshot,
	after *PlacementAction,
) ([]PlacementAction, error) {
	if root == nil {
		return nil, errors.New("sway tree is nil")
	}
	if err := registry.Validate(); err != nil {
		return nil, fmt.Errorf("validate context registry: %w", err)
	}
	if err := desired.Validate(); err != nil {
		return nil, fmt.Errorf("validate desired layout: %w", err)
	}
	registered := activeContextIDs(registry)
	observed, err := observeContexts(root, registered)
	if err != nil {
		return nil, err
	}
	targets := placementTargets(desired)
	actions := make([]PlacementAction, 0)
	for _, context := range observed {
		if context.hasMark {
			continue
		}
		if target, exists := targets[context.id]; exists && target != context.workspace {
			actions = append(actions, PlacementAction{
				Kind:        PlacementMoveWorkspace,
				ContextID:   context.id,
				ContainerID: context.containerID,
				Workspace:   target,
			})
		}
		actions = append(actions, PlacementAction{
			Kind:        PlacementAddMark,
			ContextID:   context.id,
			ContainerID: context.containerID,
		})
	}
	return boundedPlacementActionsAfter(actions, after), nil
}

func registeredContextIDs(registry Registry) map[ContextID]struct{} {
	registered := make(map[ContextID]struct{}, len(registry.Contexts))
	for _, context := range registry.Contexts {
		registered[context.ID] = struct{}{}
	}
	return registered
}

func collectWorkspaces(root *swayipc.TreeNode, includeScratchpad bool) ([]*swayipc.TreeNode, error) {
	workspaces := make([]*swayipc.TreeNode, 0)
	var walk func(*swayipc.TreeNode) error
	walk = func(node *swayipc.TreeNode) error {
		if node == nil {
			return errors.New("sway tree contains a nil node")
		}
		if node.Type == "workspace" {
			// Sway exposes hidden scratchpad contents under a synthetic
			// workspace. Version 1 has no scratchpad restore contract, so a
			// temporary scratchpad move must not replace the saved workspace.
			if (includeScratchpad || node.Name != "__i3_scratch") && node.Name != RestoreStagingWorkspace {
				workspaces = append(workspaces, node)
			}
			return nil
		}
		for _, child := range node.Nodes {
			if err := walk(child); err != nil {
				return err
			}
		}
		for _, child := range node.FloatingNodes {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return workspaces, nil
}

func captureWorkspace(workspace *swayipc.TreeNode, registered map[ContextID]struct{}) (WorkspaceLayout, bool, error) {
	if workspace == nil {
		return WorkspaceLayout{}, false, errors.New("workspace node is nil")
	}
	if strings.TrimSpace(workspace.Name) == "" {
		return WorkspaceLayout{}, false, errors.New("workspace has no name")
	}

	managed := make([]ContextID, 0)
	mixed := false
	tilingLayouts := make([]LayoutNode, 0, len(workspace.Nodes))
	tilingManaged := 0
	tilingHasUnmanaged := false
	tilingProportions := capturedChildProportions(workspace.Layout, workspace.Nodes)
	for index, child := range workspace.Nodes {
		captured, err := captureNode(child, registered, false, tilingProportions[index])
		if err != nil {
			return WorkspaceLayout{}, false, fmt.Errorf("tiling[%d]: %w", index, err)
		}
		managed = append(managed, captured.managed...)
		tilingManaged += captured.managedLeaves
		tilingHasUnmanaged = tilingHasUnmanaged || captured.hasUnmanaged
		if captured.layout != nil {
			tilingLayouts = append(tilingLayouts, *captured.layout)
		}
	}
	if tilingManaged > 0 && tilingHasUnmanaged {
		mixed = true
	}

	floatingLayouts := make([]LayoutNode, 0, len(workspace.FloatingNodes))
	for index, child := range workspace.FloatingNodes {
		captured, err := captureNode(child, registered, true, nil)
		if err != nil {
			return WorkspaceLayout{}, false, fmt.Errorf("floating[%d]: %w", index, err)
		}
		managed = append(managed, captured.managed...)
		if captured.managedLeaves > 0 && captured.hasUnmanaged {
			mixed = true
		}
		if captured.layout != nil {
			floatingLayouts = append(floatingLayouts, *captured.layout)
		}
	}
	if len(managed) == 0 {
		return WorkspaceLayout{}, false, nil
	}

	if mixed {
		return WorkspaceLayout{
			Name:              workspace.Name,
			RestoreMode:       WorkspaceRestorePlacementOnly,
			PlacementContexts: managed,
		}, true, nil
	}
	focused, err := focusedManagedContext(workspace, registered)
	if err != nil {
		return WorkspaceLayout{}, false, fmt.Errorf("capture focused descendant: %w", err)
	}

	var tiling *LayoutNode
	switch len(tilingLayouts) {
	case 0:
	case 1:
		tiling = &tilingLayouts[0]
	default:
		layout, err := captureLayoutKind(workspace.Layout)
		if err != nil {
			return WorkspaceLayout{}, false, fmt.Errorf("workspace tiling layout: %w", err)
		}
		tiling = &LayoutNode{Layout: layout, Children: tilingLayouts}
	}
	return WorkspaceLayout{
		Name:           workspace.Name,
		RestoreMode:    WorkspaceRestoreLayout,
		Tiling:         tiling,
		Floating:       floatingLayouts,
		FocusedContext: focused,
	}, true, nil
}

func captureNode(node *swayipc.TreeNode, registered map[ContextID]struct{}, floatingRoot bool, proportionOverride *float64) (nodeCapture, error) {
	if node == nil {
		return nodeCapture{}, errors.New("layout node is nil")
	}
	identity, managed, hasMark, err := managedNodeIdentity(node, registered)
	if err != nil {
		return nodeCapture{}, err
	}
	if len(node.FloatingNodes) != 0 {
		return nodeCapture{}, errors.New("nested floating children are unsupported")
	}
	if len(node.Nodes) == 0 {
		if !managed {
			return nodeCapture{hasUnmanaged: true}, nil
		}
		if node.ID <= 0 {
			return nodeCapture{}, errors.New("managed leaf has an invalid container ID")
		}
		if !hasMark && (node.AppID == nil || *node.AppID == "") {
			return nodeCapture{}, errors.New("managed leaf has neither a stable mark nor application ID")
		}
		layout := LayoutNode{
			ContextID:  contextIDPointerValue(identity),
			Proportion: capturedProportion(node.Percent, proportionOverride, floatingRoot),
		}
		fullscreen, err := captureFullscreen(node.FullscreenMode)
		if err != nil {
			return nodeCapture{}, err
		}
		layout.Fullscreen = fullscreen
		if floatingRoot {
			layout.Geometry = capturedGeometry(outerContainerRect(node))
		}
		return nodeCapture{
			layout:        &layout,
			managed:       []ContextID{identity},
			managedLeaves: 1,
		}, nil
	}
	if identity != "" {
		return nodeCapture{}, fmt.Errorf("managed identity %q is attached to a layout parent", identity)
	}
	children := make([]LayoutNode, 0, len(node.Nodes))
	captured := nodeCapture{}
	childProportions := capturedChildProportions(node.Layout, node.Nodes)
	for index, child := range node.Nodes {
		result, err := captureNode(child, registered, false, childProportions[index])
		if err != nil {
			return nodeCapture{}, fmt.Errorf("children[%d]: %w", index, err)
		}
		captured.managed = append(captured.managed, result.managed...)
		captured.managedLeaves += result.managedLeaves
		captured.hasUnmanaged = captured.hasUnmanaged || result.hasUnmanaged
		if result.layout != nil {
			children = append(children, *result.layout)
		}
	}
	if captured.managedLeaves == 0 {
		captured.hasUnmanaged = true
		return captured, nil
	}
	if captured.hasUnmanaged {
		return captured, nil
	}
	layoutKind, err := captureLayoutKind(node.Layout)
	if err != nil {
		return nodeCapture{}, err
	}
	fullscreen, err := captureFullscreen(node.FullscreenMode)
	if err != nil {
		return nodeCapture{}, err
	}
	layout := LayoutNode{
		Layout:     layoutKind,
		Children:   children,
		Proportion: capturedProportion(node.Percent, proportionOverride, floatingRoot),
		Fullscreen: fullscreen,
	}
	if floatingRoot {
		layout.Geometry = capturedGeometry(outerContainerRect(node))
	}
	captured.layout = &layout
	return captured, nil
}

// focusedManagedContext follows Sway's per-parent focus ordering, which also
// records the selected descendant of inactive workspaces. The focused boolean
// is only a compatibility fallback for trees that omit parent focus arrays.
func focusedManagedContext(workspace *swayipc.TreeNode, registered map[ContextID]struct{}) (*ContextID, error) {
	node := workspace
	for len(node.Focus) != 0 {
		focusedID := node.Focus[0]
		var focusedChild *swayipc.TreeNode
		consider := func(child *swayipc.TreeNode) error {
			if child == nil {
				return errors.New("focus path contains a nil child")
			}
			if child.ID != focusedID {
				return nil
			}
			if focusedChild != nil {
				return fmt.Errorf("focus child ID %d is ambiguous", focusedID)
			}
			focusedChild = child
			return nil
		}
		for _, child := range node.Nodes {
			if err := consider(child); err != nil {
				return nil, err
			}
		}
		for _, child := range node.FloatingNodes {
			if err := consider(child); err != nil {
				return nil, err
			}
		}
		if focusedChild == nil {
			return nil, fmt.Errorf("focus child ID %d is not a direct child", focusedID)
		}
		node = focusedChild
	}

	if len(node.Nodes) != 0 || len(node.FloatingNodes) != 0 {
		var fallback *swayipc.TreeNode
		var walk func(*swayipc.TreeNode) error
		walk = func(candidate *swayipc.TreeNode) error {
			if candidate == nil {
				return errors.New("focus fallback contains a nil node")
			}
			if candidate.Focused && len(candidate.Nodes) == 0 && len(candidate.FloatingNodes) == 0 {
				if fallback != nil {
					return errors.New("focus fallback contains multiple focused nodes")
				}
				fallback = candidate
			}
			for _, child := range candidate.Nodes {
				if err := walk(child); err != nil {
					return err
				}
			}
			for _, child := range candidate.FloatingNodes {
				if err := walk(child); err != nil {
					return err
				}
			}
			return nil
		}
		if err := walk(node); err != nil {
			return nil, err
		}
		if fallback == nil {
			return nil, nil
		}
		node = fallback
	}

	id, managed, _, err := managedNodeIdentity(node, registered)
	if err != nil {
		return nil, err
	}
	if !managed {
		return nil, nil
	}
	return contextIDPointerValue(id), nil
}

func managedNodeIdentity(node *swayipc.TreeNode, registered map[ContextID]struct{}) (ContextID, bool, bool, error) {
	identities := make(map[ContextID]struct{})
	hasManagedMark := false
	for _, mark := range node.Marks {
		if !strings.HasPrefix(mark, MarkPrefix) {
			continue
		}
		id, err := ParseMark(mark)
		if err != nil {
			return "", false, false, fmt.Errorf("invalid managed mark %q: %w", mark, err)
		}
		identities[id] = struct{}{}
		if _, exists := registered[id]; exists {
			hasManagedMark = true
		}
	}
	if node.AppID != nil && strings.HasPrefix(*node.AppID, AppIDPrefix) {
		id, err := ParseAppID(*node.AppID)
		if err != nil {
			return "", false, false, fmt.Errorf("invalid managed application ID %q: %w", *node.AppID, err)
		}
		identities[id] = struct{}{}
	}
	if len(identities) > 1 {
		return "", false, false, errors.New("container has conflicting managed identities")
	}
	for id := range identities {
		_, managed := registered[id]
		return id, managed, managed && hasManagedMark, nil
	}
	return "", false, false, nil
}

func observeContexts(root *swayipc.TreeNode, registered map[ContextID]struct{}) ([]observedContext, error) {
	observed := make([]observedContext, 0)
	seen := make(map[ContextID]int64)
	workspaces, err := collectWorkspaces(root, true)
	if err != nil {
		return nil, err
	}
	for _, workspace := range workspaces {
		var walk func(*swayipc.TreeNode) error
		walk = func(node *swayipc.TreeNode) error {
			if node == nil {
				return errors.New("sway tree contains a nil node")
			}
			id, managed, hasMark, err := managedNodeIdentity(node, registered)
			if err != nil {
				return err
			}
			if len(node.Nodes) != 0 && id != "" {
				return fmt.Errorf("managed identity %q is attached to a layout parent", id)
			}
			if managed {
				if node.ID <= 0 {
					return fmt.Errorf("managed identity %q has an invalid container ID", id)
				}
				if previous, exists := seen[id]; exists {
					return fmt.Errorf("context %q appears in containers %d and %d", id, previous, node.ID)
				}
				seen[id] = node.ID
				observed = append(observed, observedContext{
					id:          id,
					containerID: node.ID,
					workspace:   workspace.Name,
					hasMark:     hasMark,
				})
			}
			for _, child := range node.Nodes {
				if err := walk(child); err != nil {
					return err
				}
			}
			for _, child := range node.FloatingNodes {
				if err := walk(child); err != nil {
					return err
				}
			}
			return nil
		}
		for _, child := range workspace.Nodes {
			if err := walk(child); err != nil {
				return nil, fmt.Errorf("observe workspace %q: %w", workspace.Name, err)
			}
		}
		for _, child := range workspace.FloatingNodes {
			if err := walk(child); err != nil {
				return nil, fmt.Errorf("observe workspace %q: %w", workspace.Name, err)
			}
		}
	}
	return observed, nil
}

func placementTargets(snapshot LayoutSnapshot) map[ContextID]string {
	targets := make(map[ContextID]string)
	for _, workspace := range snapshot.Workspaces {
		if workspace.RestoreMode == WorkspaceRestorePlacementOnly {
			for _, id := range workspace.PlacementContexts {
				targets[id] = workspace.Name
			}
			continue
		}
		if workspace.Tiling != nil {
			collectLayoutContextIDs(*workspace.Tiling, func(id ContextID) {
				targets[id] = workspace.Name
			})
		}
		for _, floating := range workspace.Floating {
			collectLayoutContextIDs(floating, func(id ContextID) {
				targets[id] = workspace.Name
			})
		}
	}
	return targets
}

func collectLayoutContextIDs(node LayoutNode, collect func(ContextID)) {
	if node.ContextID != nil {
		collect(*node.ContextID)
		return
	}
	for _, child := range node.Children {
		collectLayoutContextIDs(child, collect)
	}
}

func captureLayoutKind(value string) (LayoutKind, error) {
	layout := LayoutKind(value)
	if !validLayout(layout) {
		return "", fmt.Errorf("unsupported layout %q", value)
	}
	return layout, nil
}

func captureFullscreen(value int) (FullscreenMode, error) {
	switch value {
	case 0:
		return FullscreenNone, nil
	case 1:
		return FullscreenWorkspace, nil
	case 2:
		return FullscreenGlobal, nil
	default:
		return "", fmt.Errorf("unsupported fullscreen mode %d", value)
	}
}

func capturedProportion(value *float64, override *float64, floatingRoot bool) float64 {
	if value == nil || floatingRoot {
		return 0
	}
	if override != nil {
		return *override
	}
	return *value
}

// Sway reports a fullscreen tiled child with percent=1 while retaining the
// underlying percentages on its siblings. Reconstruct that child's hidden
// split share when all siblings provide enough information; otherwise omit
// the proportion instead of persisting the fullscreen presentation value.
func capturedChildProportions(layout string, children []*swayipc.TreeNode) []*float64 {
	overrides := make([]*float64, len(children))
	if layout != string(LayoutSplitHorizontal) && layout != string(LayoutSplitVertical) || len(children) < 2 {
		return overrides
	}
	fullscreenIndex := -1
	for index, child := range children {
		if treeNodeHasFullscreen(child) {
			if fullscreenIndex >= 0 {
				return overrides
			}
			fullscreenIndex = index
		}
	}
	if fullscreenIndex < 0 {
		return overrides
	}
	omitFullscreen := func() []*float64 {
		zero := 0.0
		overrides[fullscreenIndex] = &zero
		return overrides
	}
	sum := 0.0
	for _, child := range children {
		if child == nil || child.Percent == nil || math.IsNaN(*child.Percent) || math.IsInf(*child.Percent, 0) || *child.Percent < 0 || *child.Percent > 1 {
			return omitFullscreen()
		}
		sum += *child.Percent
	}
	if sum <= 1+1e-9 {
		return overrides
	}
	remaining := 1 - (sum - *children[fullscreenIndex].Percent)
	if remaining <= 0 || remaining > 1 {
		return omitFullscreen()
	}
	overrides[fullscreenIndex] = &remaining
	return overrides
}

func treeNodeHasFullscreen(node *swayipc.TreeNode) bool {
	if node == nil {
		return false
	}
	if node.FullscreenMode != 0 {
		return true
	}
	for _, child := range node.Nodes {
		if treeNodeHasFullscreen(child) {
			return true
		}
	}
	return false
}

func capturedGeometry(rect swayipc.Rect) *Geometry {
	return &Geometry{X: rect.X, Y: rect.Y, Width: rect.Width, Height: rect.Height}
}

func outerContainerRect(node *swayipc.TreeNode) swayipc.Rect {
	if node == nil || node.DecoRect.Width <= 0 || node.DecoRect.Height <= 0 {
		if node == nil {
			return swayipc.Rect{}
		}
		return node.Rect
	}
	left := min(node.Rect.X, node.DecoRect.X)
	top := min(node.Rect.Y, node.DecoRect.Y)
	right := max(node.Rect.X+node.Rect.Width, node.DecoRect.X+node.DecoRect.Width)
	bottom := max(node.Rect.Y+node.Rect.Height, node.DecoRect.Y+node.DecoRect.Height)
	return swayipc.Rect{X: left, Y: top, Width: right - left, Height: bottom - top}
}

func contextIDPointerValue(id ContextID) *ContextID {
	return &id
}
