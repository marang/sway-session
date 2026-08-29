package session

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/marang/sway-title-animator/internal/swayipc"
)

const (
	// RestoreStagingWorkspace is reserved for crash-recoverable, transient
	// reconstruction. Capture never persists it.
	RestoreStagingWorkspace = "__sway_session_restore_4f6e2a91"
	restoreMarkPrefix       = "_sway_session_restore_"
	maxRestoreSteps         = 1024
	proportionTolerance     = 0.02
	geometryTolerance       = 2
)

type RestorePhase string

const (
	RestoreStageOut    RestorePhase = "stage_out"
	RestoreBuild       RestorePhase = "build"
	RestoreRollbackOut RestorePhase = "rollback_out"
	RestoreRollbackIn  RestorePhase = "rollback_in"
)

type RestoreProgress struct {
	Workspace string
	Phase     RestorePhase
	Steps     int
}

type RestoreActionKind string

const (
	RestoreMoveWorkspace    RestoreActionKind = "move_workspace"
	RestoreSplit            RestoreActionKind = "split"
	RestoreSetLayout        RestoreActionKind = "set_layout"
	RestoreAddTemporaryMark RestoreActionKind = "add_temporary_mark"
	RestoreRemoveMark       RestoreActionKind = "remove_mark"
	RestoreMoveToMark       RestoreActionKind = "move_to_mark"
	RestoreSetFloating      RestoreActionKind = "set_floating"
	RestoreSetProportion    RestoreActionKind = "set_proportion"
	RestoreResizeFloating   RestoreActionKind = "resize_floating"
	RestoreMoveFloating     RestoreActionKind = "move_floating"
	RestoreSetFullscreen    RestoreActionKind = "set_fullscreen"
	RestoreFocus            RestoreActionKind = "focus"
)

// RestoreAction is one independently acknowledged Sway mutation. Structural
// actions require workspace rollback after an explicit failure; presentation
// detail failures may degrade the affected workspace without touching others.
type RestoreAction struct {
	Kind        RestoreActionKind
	Workspace   string
	ContextID   ContextID
	ContainerID int64
	Target      string
	Layout      LayoutKind
	Axis        string
	Amount      int
	Geometry    Geometry
	Fullscreen  FullscreenMode
	Enable      bool
	Structural  bool
}

func (action RestoreAction) Key() string {
	return fmt.Sprintf("%s:%s:%d:%s:%s:%s:%d:%d:%d:%d:%d:%s:%t",
		action.Workspace,
		action.Kind,
		action.ContainerID,
		action.Target,
		action.Layout,
		action.Axis,
		action.Amount,
		action.Geometry.X,
		action.Geometry.Y,
		action.Geometry.Width,
		action.Geometry.Height,
		action.Fullscreen,
		action.Enable,
	)
}

type RestoreDegradation struct {
	Workspace string
	Reason    string
}

type RestoreSelection struct {
	Progress     *RestoreProgress
	Degradations []RestoreDegradation
}

type RestoreStep struct {
	Action   *RestoreAction
	Progress RestoreProgress
	Done     bool
}

// SelectRestoreWorkspace chooses at most one exact-layout workspace. Existing
// temporary staging state always wins so a daemon restart recovers before a
// new restore begins. eligible contains contexts marked by this daemon run;
// pre-existing marked windows are treated as current user preference.
func SelectRestoreWorkspace(
	root *swayipc.TreeNode,
	registry Registry,
	desired LayoutSnapshot,
	eligible map[ContextID]struct{},
	excluded map[string]struct{},
) (RestoreSelection, error) {
	if err := registry.Validate(); err != nil {
		return RestoreSelection{}, fmt.Errorf("validate context registry: %w", err)
	}
	if err := desired.Validate(); err != nil {
		return RestoreSelection{}, fmt.Errorf("validate desired layout: %w", err)
	}
	observation, err := observeRestoreTree(root, registry)
	if err != nil {
		return RestoreSelection{}, err
	}
	captured, err := CaptureLayout(root, registry)
	if err != nil {
		return RestoreSelection{}, err
	}
	capturedByName := make(map[string]WorkspaceLayout, len(captured.Workspaces))
	for _, workspace := range captured.Workspaces {
		capturedByName[workspace.Name] = workspace
	}
	registered := registeredContextIDs(registry)
	selection := RestoreSelection{Degradations: []RestoreDegradation{}}

	for _, workspace := range desired.Workspaces {
		if workspace.RestoreMode != WorkspaceRestoreLayout {
			continue
		}
		if _, skip := excluded[workspace.Name]; skip {
			continue
		}
		ids := workspaceContextIDs(workspace)
		allRegistered := true
		allVisible := true
		hasEligible := false
		hasTemporary := observation.hasTemporaryMarks(workspace.Name, ids)
		hasStagedContext := false
		for _, id := range ids {
			if _, exists := registered[id]; !exists {
				allRegistered = false
			}
			node, exists := observation.contexts[id]
			if !exists {
				allVisible = false
			} else if observation.workspaceName(node) == RestoreStagingWorkspace {
				hasStagedContext = true
			}
			if _, exists := eligible[id]; exists {
				hasEligible = true
			}
		}
		if hasStagedContext {
			selection.Progress = &RestoreProgress{Workspace: workspace.Name, Phase: RestoreStageOut}
			return selection, nil
		}
		if !allRegistered || !allVisible || (!hasEligible && !hasTemporary) {
			continue
		}
		current, exists := capturedByName[workspace.Name]
		if !exists {
			continue
		}
		if current.RestoreMode == WorkspaceRestorePlacementOnly {
			selection.Degradations = append(selection.Degradations, RestoreDegradation{
				Workspace: workspace.Name,
				Reason:    "mixed managed and unregistered layout is placement-only",
			})
			continue
		}
		if !sameContextSet(workspaceContextIDs(workspace), workspaceContextIDs(current)) {
			selection.Degradations = append(selection.Degradations, RestoreDegradation{
				Workspace: workspace.Name,
				Reason:    "current workspace contains managed contexts outside the saved exact layout",
			})
			continue
		}
		if workspaceLayoutsEqual(workspace, current) {
			if hasTemporary {
				selection.Progress = &RestoreProgress{Workspace: workspace.Name, Phase: RestoreBuild}
				return selection, nil
			}
			continue
		}
		if workspaceHasSingletonGroup(workspace) {
			selection.Degradations = append(selection.Degradations, RestoreDegradation{
				Workspace: workspace.Name,
				Reason:    "single-child layout groups cannot be reconstructed reliably with runtime Sway commands",
			})
			continue
		}
		phase := RestoreStageOut
		if workspaceStructureEqual(workspace, current) {
			phase = RestoreBuild
		}
		selection.Progress = &RestoreProgress{Workspace: workspace.Name, Phase: phase}
		return selection, nil
	}
	return selection, nil
}

// PlanWorkspaceRestoreStep returns one mutation and updated progress. The
// caller must re-observe after every action and call again. skipped contains
// non-structural action keys which explicitly failed and must not be retried.
func PlanWorkspaceRestoreStep(
	root *swayipc.TreeNode,
	registry Registry,
	desired WorkspaceLayout,
	progress RestoreProgress,
	skipped map[string]struct{},
) (RestoreStep, error) {
	if err := (&LayoutSnapshot{
		Version:    LayoutSchemaVersion,
		Workspaces: []WorkspaceLayout{desired},
	}).Validate(); err != nil {
		return RestoreStep{}, fmt.Errorf("validate desired workspace: %w", err)
	}
	if desired.Name != progress.Workspace {
		return RestoreStep{}, errors.New("restore progress does not match desired workspace")
	}
	if desired.RestoreMode != WorkspaceRestoreLayout {
		return RestoreStep{}, errors.New("only exact-layout workspaces can be reconstructed")
	}
	if progress.Steps >= maxRestoreSteps {
		return RestoreStep{}, fmt.Errorf("workspace restore exceeds %d attempted steps", maxRestoreSteps)
	}
	observation, err := observeRestoreTree(root, registry)
	if err != nil {
		return RestoreStep{}, err
	}
	ids := workspaceContextIDs(desired)
	for _, id := range ids {
		if _, exists := observation.contexts[id]; !exists {
			return RestoreStep{}, fmt.Errorf("context %q disappeared during workspace restore", id)
		}
	}
	next := progress

	switch progress.Phase {
	case RestoreStageOut, RestoreRollbackOut:
		if action := observation.disableManagedFullscreen(desired.Name, ids); action != nil {
			action.Workspace = desired.Name
			action.Structural = true
			return restoreActionStep(*action, next), nil
		}
		for _, id := range ids {
			node := observation.contexts[id]
			if observation.workspaceName(node) == RestoreStagingWorkspace {
				continue
			}
			action := RestoreAction{
				Kind:        RestoreMoveWorkspace,
				Workspace:   desired.Name,
				ContextID:   id,
				ContainerID: node.ID,
				Target:      RestoreStagingWorkspace,
				Structural:  true,
			}
			return restoreActionStep(action, next), nil
		}
		if progress.Phase == RestoreStageOut {
			next.Phase = RestoreBuild
		} else {
			next.Phase = RestoreRollbackIn
		}
		return RestoreStep{Progress: next}, nil

	case RestoreRollbackIn:
		for _, id := range ids {
			node := observation.contexts[id]
			if observation.workspaceName(node) == desired.Name {
				continue
			}
			action := RestoreAction{
				Kind:        RestoreMoveWorkspace,
				Workspace:   desired.Name,
				ContextID:   id,
				ContainerID: node.ID,
				Target:      desired.Name,
				Structural:  true,
			}
			return restoreActionStep(action, next), nil
		}
		if action := observation.cleanupTemporaryMark(desired.Name, skipped); action != nil {
			return restoreActionStep(*action, next), nil
		}
		return RestoreStep{Progress: next, Done: true}, nil

	case RestoreBuild:
		for _, id := range ids {
			node := observation.contexts[id]
			if observation.workspaceName(node) == desired.Name {
				continue
			}
			action := RestoreAction{
				Kind:        RestoreMoveWorkspace,
				Workspace:   desired.Name,
				ContextID:   id,
				ContainerID: node.ID,
				Target:      desired.Name,
				Structural:  true,
			}
			return restoreActionStep(action, next), nil
		}
		workspaceNode := observation.workspaces[desired.Name]
		if workspaceNode == nil {
			return RestoreStep{}, fmt.Errorf("workspace %q is not visible during restore", desired.Name)
		}
		if observation.workspaceHasUnexpectedManaged(workspaceNode, ids) {
			return RestoreStep{}, errors.New("managed context outside the saved exact layout appeared during restore")
		}
		if observation.workspaceHasUnmanagedTiling(workspaceNode, ids) ||
			observation.workspaceHasMixedManagedFloating(workspaceNode, ids) {
			return RestoreStep{}, errors.New("unregistered window appeared inside a managed exact-layout subtree")
		}
		if action := observation.disableUnexpectedManagedFloating(desired.Name, desired.Floating); action != nil {
			if fullscreen := observation.disableManagedFullscreen(desired.Name, ids); fullscreen != nil {
				fullscreen.Workspace = desired.Name
				fullscreen.Structural = true
				return restoreActionStep(*fullscreen, next), nil
			}
			action.Workspace = desired.Name
			action.Structural = true
			return restoreActionStep(*action, next), nil
		}
		if desired.Tiling != nil {
			action, err := planDesiredStructure(observation, desired.Name, *desired.Tiling, "t")
			if err != nil {
				return RestoreStep{}, err
			}
			if action != nil {
				if fullscreen := observation.disableManagedFullscreen(desired.Name, ids); fullscreen != nil {
					fullscreen.Workspace = desired.Name
					fullscreen.Structural = true
					return restoreActionStep(*fullscreen, next), nil
				}
				return restoreActionStep(*action, next), nil
			}
		}
		for index, floating := range desired.Floating {
			path := fmt.Sprintf("f%d", index)
			action, err := planDesiredStructure(observation, desired.Name, floating, path)
			if err != nil {
				return RestoreStep{}, err
			}
			if action != nil {
				if fullscreen := observation.disableManagedFullscreen(desired.Name, ids); fullscreen != nil {
					fullscreen.Workspace = desired.Name
					fullscreen.Structural = true
					return restoreActionStep(*fullscreen, next), nil
				}
				return restoreActionStep(*action, next), nil
			}
		}

		if desired.Tiling != nil {
			action, err := planProportions(observation, desired.Name, *desired.Tiling, "t", skipped)
			if err != nil {
				return RestoreStep{}, err
			}
			if action != nil {
				if fullscreen := observation.disableManagedFullscreen(desired.Name, ids); fullscreen != nil {
					fullscreen.Workspace = desired.Name
					fullscreen.Structural = true
					return restoreActionStep(*fullscreen, next), nil
				}
				return restoreActionStep(*action, next), nil
			}
		}
		for index, floating := range desired.Floating {
			path := fmt.Sprintf("f%d", index)
			action, err := planProportions(observation, desired.Name, floating, path, skipped)
			if err != nil {
				return RestoreStep{}, err
			}
			if action != nil {
				if fullscreen := observation.disableManagedFullscreen(desired.Name, ids); fullscreen != nil {
					fullscreen.Workspace = desired.Name
					fullscreen.Structural = true
					return restoreActionStep(*fullscreen, next), nil
				}
				return restoreActionStep(*action, next), nil
			}
		}

		for index, floating := range desired.Floating {
			path := fmt.Sprintf("f%d", index)
			node, err := observation.nodeForDesired(desired.Name, floating, path)
			if err != nil {
				return RestoreStep{}, err
			}
			if !observation.isFloatingRoot(node) {
				if fullscreen := observation.disableManagedFullscreen(desired.Name, ids); fullscreen != nil {
					fullscreen.Workspace = desired.Name
					fullscreen.Structural = true
					return restoreActionStep(*fullscreen, next), nil
				}
				action := RestoreAction{
					Kind:        RestoreSetFloating,
					Workspace:   desired.Name,
					ContainerID: node.ID,
					Enable:      true,
					Structural:  true,
				}
				return restoreActionStep(action, next), nil
			}
			geometry := clampGeometry(*floating.Geometry, workspaceNode.Rect)
			actualGeometry := outerContainerRect(node)
			if !rectSizeClose(actualGeometry, geometry) {
				action := RestoreAction{
					Kind:        RestoreResizeFloating,
					Workspace:   desired.Name,
					ContainerID: node.ID,
					Geometry:    geometry,
				}
				if _, skip := skipped[action.Key()]; !skip {
					if fullscreen := observation.disableManagedFullscreen(desired.Name, ids); fullscreen != nil {
						fullscreen.Workspace = desired.Name
						fullscreen.Structural = true
						return restoreActionStep(*fullscreen, next), nil
					}
					return restoreActionStep(action, next), nil
				}
			}
			if !rectPositionClose(actualGeometry, geometry) {
				action := RestoreAction{
					Kind:        RestoreMoveFloating,
					Workspace:   desired.Name,
					ContainerID: node.ID,
					Geometry:    geometry,
				}
				if _, skip := skipped[action.Key()]; !skip {
					if fullscreen := observation.disableManagedFullscreen(desired.Name, ids); fullscreen != nil {
						fullscreen.Workspace = desired.Name
						fullscreen.Structural = true
						return restoreActionStep(*fullscreen, next), nil
					}
					return restoreActionStep(action, next), nil
				}
			}
		}

		if desired.Tiling != nil {
			if action, err := planFullscreen(observation, desired.Name, *desired.Tiling, "t", skipped); err != nil {
				return RestoreStep{}, err
			} else if action != nil {
				return restoreActionStep(*action, next), nil
			}
		}
		for index, floating := range desired.Floating {
			if action, err := planFullscreen(observation, desired.Name, floating, fmt.Sprintf("f%d", index), skipped); err != nil {
				return RestoreStep{}, err
			} else if action != nil {
				return restoreActionStep(*action, next), nil
			}
		}

		if action := observation.cleanupTemporaryMark(desired.Name, skipped); action != nil {
			return restoreActionStep(*action, next), nil
		}
		if desired.FocusedContext != nil {
			node := observation.contexts[*desired.FocusedContext]
			if node != nil && !node.Focused {
				action := RestoreAction{
					Kind:        RestoreFocus,
					Workspace:   desired.Name,
					ContextID:   *desired.FocusedContext,
					ContainerID: node.ID,
				}
				if _, skip := skipped[action.Key()]; !skip {
					return restoreActionStep(action, next), nil
				}
			}
		}
		return RestoreStep{Progress: next, Done: true}, nil
	default:
		return RestoreStep{}, fmt.Errorf("unsupported restore phase %q", progress.Phase)
	}
}

func restoreActionStep(action RestoreAction, progress RestoreProgress) RestoreStep {
	progress.Steps++
	return RestoreStep{Action: &action, Progress: progress}
}

type restoreObservation struct {
	registered    map[ContextID]struct{}
	contexts      map[ContextID]*swayipc.TreeNode
	contextByNode map[*swayipc.TreeNode]ContextID
	parents       map[*swayipc.TreeNode]*swayipc.TreeNode
	workspaces    map[string]*swayipc.TreeNode
	marks         map[string]*swayipc.TreeNode
}

func observeRestoreTree(root *swayipc.TreeNode, registry Registry) (*restoreObservation, error) {
	if root == nil {
		return nil, errors.New("sway tree is nil")
	}
	if err := registry.Validate(); err != nil {
		return nil, fmt.Errorf("validate context registry: %w", err)
	}
	observation := &restoreObservation{
		registered:    registeredContextIDs(registry),
		contexts:      make(map[ContextID]*swayipc.TreeNode),
		contextByNode: make(map[*swayipc.TreeNode]ContextID),
		parents:       make(map[*swayipc.TreeNode]*swayipc.TreeNode),
		workspaces:    make(map[string]*swayipc.TreeNode),
		marks:         make(map[string]*swayipc.TreeNode),
	}
	var walk func(*swayipc.TreeNode, *swayipc.TreeNode, *swayipc.TreeNode) error
	walk = func(node *swayipc.TreeNode, parent *swayipc.TreeNode, workspace *swayipc.TreeNode) error {
		if node == nil {
			return errors.New("sway tree contains a nil node")
		}
		observation.parents[node] = parent
		if node.Type == "workspace" {
			workspace = node
			if _, exists := observation.workspaces[node.Name]; exists {
				return fmt.Errorf("duplicate workspace %q", node.Name)
			}
			observation.workspaces[node.Name] = node
		}
		for _, mark := range node.Marks {
			if !strings.HasPrefix(mark, restoreMarkPrefix) {
				continue
			}
			if _, exists := observation.marks[mark]; exists {
				return fmt.Errorf("temporary restore mark %q appears more than once", mark)
			}
			observation.marks[mark] = node
		}
		id, managed, _, err := managedNodeIdentity(node, observation.registered)
		if err != nil {
			return err
		}
		if managed {
			if len(node.Nodes) != 0 || len(node.FloatingNodes) != 0 {
				return fmt.Errorf("managed identity %q is attached to a layout parent", id)
			}
			if workspace == nil {
				return fmt.Errorf("managed context %q is outside a workspace", id)
			}
			if previous, exists := observation.contexts[id]; exists {
				return fmt.Errorf("context %q appears in containers %d and %d", id, previous.ID, node.ID)
			}
			observation.contexts[id] = node
			observation.contextByNode[node] = id
		}
		for _, child := range node.Nodes {
			if err := walk(child, node, workspace); err != nil {
				return err
			}
		}
		for _, child := range node.FloatingNodes {
			if err := walk(child, node, workspace); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, nil, nil); err != nil {
		return nil, err
	}
	return observation, nil
}

func (observation *restoreObservation) workspaceName(node *swayipc.TreeNode) string {
	for node != nil && node.Type != "workspace" {
		node = observation.parents[node]
	}
	if node == nil {
		return ""
	}
	return node.Name
}

func (observation *restoreObservation) hasTemporaryMarks(workspace string, ids []ContextID) bool {
	prefix := temporaryMarkPrefix(workspace)
	wanted := make(map[ContextID]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	for mark, node := range observation.marks {
		if !strings.HasPrefix(mark, prefix) || observation.workspaceName(node) != workspace {
			continue
		}
		for _, id := range observation.managedDescendants(node) {
			if _, exists := wanted[id]; exists {
				return true
			}
		}
	}
	return false
}

func (observation *restoreObservation) managedDescendants(node *swayipc.TreeNode) []ContextID {
	ids := make([]ContextID, 0)
	var walk func(*swayipc.TreeNode)
	walk = func(candidate *swayipc.TreeNode) {
		if candidate == nil {
			return
		}
		if id, exists := observation.contextByNode[candidate]; exists {
			ids = append(ids, id)
		}
		for _, child := range candidate.Nodes {
			walk(child)
		}
		for _, child := range candidate.FloatingNodes {
			walk(child)
		}
	}
	walk(node)
	return ids
}

func (observation *restoreObservation) structuralManagedDescendants(node *swayipc.TreeNode) []ContextID {
	if node == nil || node.Type != "workspace" {
		return observation.managedDescendants(node)
	}
	ids := make([]ContextID, 0)
	for _, child := range node.Nodes {
		ids = append(ids, observation.managedDescendants(child)...)
	}
	return ids
}

func (observation *restoreObservation) directChildContaining(parent *swayipc.TreeNode, id ContextID) *swayipc.TreeNode {
	contains := func(node *swayipc.TreeNode) bool {
		for _, candidate := range observation.managedDescendants(node) {
			if candidate == id {
				return true
			}
		}
		return false
	}
	for _, child := range parent.Nodes {
		if contains(child) {
			return child
		}
	}
	for _, child := range parent.FloatingNodes {
		if contains(child) {
			return child
		}
	}
	return nil
}

func (observation *restoreObservation) disableManagedFullscreen(workspace string, ids []ContextID) *RestoreAction {
	wanted := make(map[ContextID]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	var found *swayipc.TreeNode
	var walk func(*swayipc.TreeNode)
	walk = func(node *swayipc.TreeNode) {
		if node == nil || found != nil {
			return
		}
		// Sway mirrors descendant fullscreen state onto workspace nodes. Only
		// container state can be disabled with a con_id-scoped command.
		if node.Type != "workspace" && node.FullscreenMode != 0 {
			for _, id := range observation.managedDescendants(node) {
				if _, exists := wanted[id]; exists {
					found = node
					return
				}
			}
		}
		for _, child := range node.Nodes {
			walk(child)
		}
		for _, child := range node.FloatingNodes {
			walk(child)
		}
	}
	// A previous acknowledged staging move, or a daemon restart midway through
	// reconstruction, can leave a fullscreen managed container on the reserved
	// workspace. Inspect both locations so fullscreen state cannot prevent the
	// remaining staging operations from converging.
	for _, name := range []string{workspace, RestoreStagingWorkspace} {
		walk(observation.workspaces[name])
	}
	if found == nil {
		return nil
	}
	return &RestoreAction{Kind: RestoreSetFullscreen, ContainerID: found.ID, Fullscreen: FullscreenNone}
}

func (observation *restoreObservation) disableUnexpectedManagedFloating(workspace string, desired []LayoutNode) *RestoreAction {
	workspaceNode := observation.workspaces[workspace]
	if workspaceNode == nil {
		return nil
	}
	for _, floating := range workspaceNode.FloatingNodes {
		actual := observation.managedDescendants(floating)
		if len(actual) == 0 {
			continue
		}
		expected := false
		for _, candidate := range desired {
			if sameContextSet(actual, layoutNodeContextIDs(candidate)) {
				expected = true
				break
			}
		}
		if !expected {
			return &RestoreAction{Kind: RestoreSetFloating, ContainerID: floating.ID, Enable: false}
		}
	}
	return nil
}

func (observation *restoreObservation) isFloatingRoot(node *swayipc.TreeNode) bool {
	parent := observation.parents[node]
	if parent == nil || parent.Type != "workspace" {
		return false
	}
	for _, floating := range parent.FloatingNodes {
		if floating == node {
			return true
		}
	}
	return false
}

func (observation *restoreObservation) workspaceHasUnmanagedTiling(workspace *swayipc.TreeNode, ids []ContextID) bool {
	wanted := make(map[ContextID]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	var inspect func(*swayipc.TreeNode) bool
	inspect = func(node *swayipc.TreeNode) bool {
		if node == nil {
			return true
		}
		if len(node.Nodes) == 0 && len(node.FloatingNodes) == 0 {
			id, managed := observation.contextByNode[node]
			if !managed {
				return true
			}
			_, expected := wanted[id]
			return !expected
		}
		for _, child := range node.Nodes {
			if inspect(child) {
				return true
			}
		}
		return false
	}
	for _, child := range workspace.Nodes {
		if inspect(child) {
			return true
		}
	}
	return false
}

func (observation *restoreObservation) workspaceHasUnexpectedManaged(workspace *swayipc.TreeNode, ids []ContextID) bool {
	wanted := make(map[ContextID]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	for _, id := range observation.managedDescendants(workspace) {
		if _, exists := wanted[id]; !exists {
			return true
		}
	}
	return false
}

func (observation *restoreObservation) workspaceHasMixedManagedFloating(workspace *swayipc.TreeNode, ids []ContextID) bool {
	wanted := make(map[ContextID]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	for _, floating := range workspace.FloatingNodes {
		managed := false
		unmanaged := false
		var inspect func(*swayipc.TreeNode)
		inspect = func(node *swayipc.TreeNode) {
			if node == nil {
				unmanaged = true
				return
			}
			if len(node.Nodes) == 0 && len(node.FloatingNodes) == 0 {
				id, exists := observation.contextByNode[node]
				if _, expected := wanted[id]; exists && expected {
					managed = true
				} else {
					unmanaged = true
				}
				return
			}
			for _, child := range node.Nodes {
				inspect(child)
			}
			for _, child := range node.FloatingNodes {
				inspect(child)
			}
		}
		inspect(floating)
		if managed && unmanaged {
			return true
		}
	}
	return false
}

func (observation *restoreObservation) removeWorkspaceTemporaryMark(workspace string) *RestoreAction {
	prefix := temporaryMarkPrefix(workspace)
	marks := make([]string, 0)
	for mark, node := range observation.marks {
		if strings.HasPrefix(mark, prefix) && observation.workspaceName(node) == workspace {
			marks = append(marks, mark)
		}
	}
	if len(marks) == 0 {
		return nil
	}
	sort.Strings(marks)
	node := observation.marks[marks[0]]
	return &RestoreAction{Kind: RestoreRemoveMark, ContainerID: node.ID, Target: marks[0]}
}

func (observation *restoreObservation) cleanupTemporaryMark(workspace string, skipped map[string]struct{}) *RestoreAction {
	action := observation.removeWorkspaceTemporaryMark(workspace)
	if action == nil {
		return nil
	}
	action.Workspace = workspace
	if _, skip := skipped[action.Key()]; skip {
		return nil
	}
	return action
}

func planDesiredStructure(observation *restoreObservation, workspace string, desired LayoutNode, path string) (*RestoreAction, error) {
	if desired.ContextID != nil {
		return nil, nil
	}
	mark := temporaryMark(workspace, path)
	target := observation.marks[mark]
	desiredIDs := layoutNodeContextIDs(desired)
	if target == nil {
		if existing := observation.existingNodeForDesired(workspace, desired, path); existing != nil {
			target = existing
		}
	}
	if target == nil {
		anchorID := firstLayoutContextID(desired)
		anchor := observation.contexts[anchorID]
		if anchor == nil {
			return nil, fmt.Errorf("restore anchor context %q is missing", anchorID)
		}
		candidate := observation.parents[anchor]
		if candidate != nil && candidate.Type != "workspace" &&
			isSubset(observation.managedDescendants(candidate), desiredIDs) &&
			!nodeHasRestoreMark(candidate) {
			return &RestoreAction{
				Kind:        RestoreAddTemporaryMark,
				Workspace:   workspace,
				ContainerID: candidate.ID,
				Target:      mark,
				Structural:  true,
			}, nil
		}
		splitLayout := LayoutSplitHorizontal
		if desired.Layout == LayoutSplitVertical {
			splitLayout = LayoutSplitVertical
		}
		return &RestoreAction{
			Kind:        RestoreSplit,
			Workspace:   workspace,
			ContextID:   anchorID,
			ContainerID: anchor.ID,
			Layout:      splitLayout,
			Structural:  true,
		}, nil
	}
	if len(target.Nodes) == 0 {
		return nil, fmt.Errorf("temporary restore mark %q is not attached to a layout parent", mark)
	}
	targetIDs := observation.structuralManagedDescendants(target)
	if !containsContextID(targetIDs, firstLayoutContextID(desired)) || !isSubset(targetIDs, desiredIDs) {
		return nil, fmt.Errorf("temporary restore group %q contains unrelated contexts", mark)
	}

	for _, child := range desired.Children {
		anchorID := firstLayoutContextID(child)
		direct := observation.directChildContaining(target, anchorID)
		childIDs := layoutNodeContextIDs(child)
		if direct == nil || !isSubset(observation.managedDescendants(direct), childIDs) {
			anchor := observation.contexts[anchorID]
			if anchor == nil {
				return nil, fmt.Errorf("restore child anchor %q is missing", anchorID)
			}
			return &RestoreAction{
				Kind:        RestoreMoveToMark,
				Workspace:   workspace,
				ContextID:   anchorID,
				ContainerID: anchor.ID,
				Target:      mark,
				Structural:  true,
			}, nil
		}
	}
	if !directDesiredOrderConverges(observation, target, desired.Children) {
		return nil, fmt.Errorf("restore group %q has non-convergent child order", mark)
	}
	if LayoutKind(target.Layout) != desired.Layout {
		firstChild := observation.directChildContaining(target, firstLayoutContextID(desired.Children[0]))
		if firstChild == nil {
			return nil, fmt.Errorf("restore group %q has no layout anchor", mark)
		}
		return &RestoreAction{
			Kind:        RestoreSetLayout,
			Workspace:   workspace,
			ContainerID: firstChild.ID,
			Layout:      desired.Layout,
			Structural:  true,
		}, nil
	}
	for index, child := range desired.Children {
		if child.ContextID != nil {
			continue
		}
		action, err := planDesiredStructure(observation, workspace, child, fmt.Sprintf("%s_%d", path, index))
		if err != nil || action != nil {
			return action, err
		}
	}
	return nil, nil
}

func planProportions(
	observation *restoreObservation,
	workspace string,
	desired LayoutNode,
	path string,
	skipped map[string]struct{},
) (*RestoreAction, error) {
	if desired.ContextID != nil {
		return nil, nil
	}
	if desired.Layout == LayoutSplitHorizontal || desired.Layout == LayoutSplitVertical {
		parent, err := observation.nodeForDesired(workspace, desired, path)
		if err != nil {
			return nil, err
		}
		proportionOverrides := capturedChildProportions(parent.Layout, parent.Nodes)
		for index := 0; index+1 < len(desired.Children); index++ {
			child := desired.Children[index]
			if child.Proportion <= 0 {
				continue
			}
			node := observation.directChildContaining(parent, firstLayoutContextID(child))
			if node == nil {
				return nil, fmt.Errorf("restore group %q has no direct proportion child for context %q", path, firstLayoutContextID(child))
			}
			observedProportion := node.Percent
			for childIndex, candidate := range parent.Nodes {
				if candidate == node && proportionOverrides[childIndex] != nil {
					observedProportion = proportionOverrides[childIndex]
					break
				}
			}
			if observedProportion != nil && math.Abs(*observedProportion-child.Proportion) <= proportionTolerance {
				continue
			}
			axis := "width"
			if desired.Layout == LayoutSplitVertical {
				axis = "height"
			}
			amount := int(math.Round(child.Proportion * 100))
			amount = max(1, min(99, amount))
			action := RestoreAction{
				Kind:        RestoreSetProportion,
				Workspace:   workspace,
				ContainerID: node.ID,
				Axis:        axis,
				Amount:      amount,
			}
			if _, skip := skipped[action.Key()]; !skip {
				return &action, nil
			}
		}
	}
	for index, child := range desired.Children {
		action, err := planProportions(observation, workspace, child, fmt.Sprintf("%s_%d", path, index), skipped)
		if err != nil || action != nil {
			return action, err
		}
	}
	return nil, nil
}

func planFullscreen(observation *restoreObservation, workspace string, desired LayoutNode, path string, skipped map[string]struct{}) (*RestoreAction, error) {
	node, err := observation.nodeForDesired(workspace, desired, path)
	if err != nil {
		return nil, err
	}
	want := 0
	switch desired.Fullscreen {
	case FullscreenWorkspace:
		want = 1
	case FullscreenGlobal:
		want = 2
	}
	// A workspace node is used as the observable stand-in for a synthetic
	// top-level layout. Sway reports descendant fullscreen state on that node,
	// so it must not be interpreted as fullscreen state of the synthetic group.
	if node.Type != "workspace" && node.FullscreenMode != want {
		action := RestoreAction{
			Kind:        RestoreSetFullscreen,
			Workspace:   workspace,
			ContainerID: node.ID,
			Fullscreen:  desired.Fullscreen,
		}
		if _, skip := skipped[action.Key()]; !skip {
			return &action, nil
		}
	}
	if desired.ContextID == nil {
		for index, child := range desired.Children {
			action, err := planFullscreen(observation, workspace, child, fmt.Sprintf("%s_%d", path, index), skipped)
			if err != nil || action != nil {
				return action, err
			}
		}
	}
	return nil, nil
}

func (observation *restoreObservation) nodeForDesired(workspace string, desired LayoutNode, path string) (*swayipc.TreeNode, error) {
	if desired.ContextID != nil {
		node := observation.contexts[*desired.ContextID]
		if node == nil {
			return nil, fmt.Errorf("context %q is missing", *desired.ContextID)
		}
		return node, nil
	}
	mark := temporaryMark(workspace, path)
	node := observation.marks[mark]
	if node == nil {
		node = observation.existingNodeForDesired(workspace, desired, path)
	}
	if node == nil {
		return nil, fmt.Errorf("temporary restore group %q is missing", mark)
	}
	return node, nil
}

func (observation *restoreObservation) existingNodeForDesired(workspace string, desired LayoutNode, path string) *swayipc.TreeNode {
	workspaceNode := observation.workspaces[workspace]
	if workspaceNode == nil || desired.ContextID != nil {
		return nil
	}
	candidates := make([]*swayipc.TreeNode, 0)
	// A workspace can stand in for a synthetic top-level tiling node, but it
	// cannot represent fullscreen or parent-relative state captured from a real
	// top-level layout container.
	if path == "t" && desired.Fullscreen == FullscreenNone && desired.Proportion == 0 {
		candidates = append(candidates, workspaceNode)
	}
	var collect func(*swayipc.TreeNode)
	collect = func(node *swayipc.TreeNode) {
		if node == nil {
			return
		}
		candidates = append(candidates, node)
		for _, child := range node.Nodes {
			collect(child)
		}
	}
	if strings.HasPrefix(path, "f") {
		for _, node := range workspaceNode.FloatingNodes {
			collect(node)
		}
	} else {
		for _, node := range workspaceNode.Nodes {
			collect(node)
		}
	}
	for _, candidate := range candidates {
		if observation.nodeStructureMatches(candidate, desired) {
			return candidate
		}
	}
	return nil
}

func (observation *restoreObservation) nodeStructureMatches(actual *swayipc.TreeNode, desired LayoutNode) bool {
	if actual == nil {
		return false
	}
	if desired.ContextID != nil {
		id, exists := observation.contextByNode[actual]
		return exists && id == *desired.ContextID
	}
	if !sameContextSet(observation.structuralManagedDescendants(actual), layoutNodeContextIDs(desired)) ||
		!directDesiredOrderConverges(observation, actual, desired.Children) {
		return false
	}
	for _, child := range desired.Children {
		direct := observation.directChildContaining(actual, firstLayoutContextID(child))
		if direct == nil || !observation.nodeStructureMatches(direct, child) {
			return false
		}
	}
	return true
}

func layoutNodeContextIDs(node LayoutNode) []ContextID {
	ids := make([]ContextID, 0)
	collectLayoutContextIDs(node, func(id ContextID) {
		ids = append(ids, id)
	})
	return ids
}

func firstLayoutContextID(node LayoutNode) ContextID {
	if node.ContextID != nil {
		return *node.ContextID
	}
	return firstLayoutContextID(node.Children[0])
}

func isSubset(actual []ContextID, allowed []ContextID) bool {
	wanted := make(map[ContextID]struct{}, len(allowed))
	for _, id := range allowed {
		wanted[id] = struct{}{}
	}
	for _, id := range actual {
		if _, exists := wanted[id]; !exists {
			return false
		}
	}
	return true
}

func sameContextSet(left []ContextID, right []ContextID) bool {
	return len(left) == len(right) && isSubset(left, right) && isSubset(right, left)
}

func containsContextID(ids []ContextID, wanted ContextID) bool {
	for _, id := range ids {
		if id == wanted {
			return true
		}
	}
	return false
}

func workspaceHasSingletonGroup(workspace WorkspaceLayout) bool {
	if workspace.Tiling != nil && layoutHasSingletonGroup(*workspace.Tiling) {
		return true
	}
	for _, floating := range workspace.Floating {
		if layoutHasSingletonGroup(floating) {
			return true
		}
	}
	return false
}

func layoutHasSingletonGroup(node LayoutNode) bool {
	if node.ContextID != nil {
		return false
	}
	if len(node.Children) == 1 {
		return true
	}
	for _, child := range node.Children {
		if layoutHasSingletonGroup(child) {
			return true
		}
	}
	return false
}

func nodeHasRestoreMark(node *swayipc.TreeNode) bool {
	for _, mark := range node.Marks {
		if strings.HasPrefix(mark, restoreMarkPrefix) {
			return true
		}
	}
	return false
}

func directDesiredOrderConverges(observation *restoreObservation, parent *swayipc.TreeNode, desired []LayoutNode) bool {
	previous := -1
	for _, child := range parent.Nodes {
		ids := observation.managedDescendants(child)
		if len(ids) == 0 {
			continue
		}
		matched := -1
		for index, wanted := range desired {
			if isSubset(ids, layoutNodeContextIDs(wanted)) {
				matched = index
				break
			}
		}
		if matched < previous || matched < 0 {
			return false
		}
		previous = matched
	}
	return true
}

func temporaryMarkPrefix(workspace string) string {
	digest := sha256.Sum256([]byte(workspace))
	return fmt.Sprintf("%s%x_", restoreMarkPrefix, digest[:8])
}

func temporaryMark(workspace string, path string) string {
	return temporaryMarkPrefix(workspace) + path
}

func clampGeometry(saved Geometry, workspace swayipc.Rect) Geometry {
	if workspace.Width <= 0 || workspace.Height <= 0 {
		return saved
	}
	result := saved
	result.Width = max(1, min(result.Width, workspace.Width))
	result.Height = max(1, min(result.Height, workspace.Height))
	result.X = max(workspace.X, min(result.X, workspace.X+workspace.Width-result.Width))
	result.Y = max(workspace.Y, min(result.Y, workspace.Y+workspace.Height-result.Height))
	return result
}

func rectSizeClose(rect swayipc.Rect, geometry Geometry) bool {
	return abs(rect.Width-geometry.Width) <= geometryTolerance &&
		abs(rect.Height-geometry.Height) <= geometryTolerance
}

func rectPositionClose(rect swayipc.Rect, geometry Geometry) bool {
	return abs(rect.X-geometry.X) <= geometryTolerance &&
		abs(rect.Y-geometry.Y) <= geometryTolerance
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func workspaceLayoutsEqual(left WorkspaceLayout, right WorkspaceLayout) bool {
	leftSnapshot := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{left}}
	rightSnapshot := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: []WorkspaceLayout{right}}
	leftHash, leftErr := SemanticSnapshotHash(leftSnapshot)
	rightHash, rightErr := SemanticSnapshotHash(rightSnapshot)
	return leftErr == nil && rightErr == nil && leftHash == rightHash
}

func workspaceStructureEqual(left WorkspaceLayout, right WorkspaceLayout) bool {
	if left.RestoreMode != right.RestoreMode || (left.Tiling == nil) != (right.Tiling == nil) ||
		len(left.Floating) != len(right.Floating) {
		return false
	}
	if left.Tiling != nil && !layoutStructureEqual(*left.Tiling, *right.Tiling) {
		return false
	}
	for index := range left.Floating {
		if !layoutStructureEqual(left.Floating[index], right.Floating[index]) {
			return false
		}
	}
	return true
}

func layoutStructureEqual(left LayoutNode, right LayoutNode) bool {
	if (left.ContextID == nil) != (right.ContextID == nil) {
		return false
	}
	if left.ContextID != nil {
		return *left.ContextID == *right.ContextID
	}
	if left.Layout != right.Layout || len(left.Children) != len(right.Children) {
		return false
	}
	for index := range left.Children {
		if !layoutStructureEqual(left.Children[index], right.Children[index]) {
			return false
		}
	}
	return true
}
