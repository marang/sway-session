package main

import (
	"slices"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// A late arrival can resume only the saved startup intent. Once an app has
// appeared, a replacement window belongs to normal application lifecycle.
type startupApplication struct {
	identity    sessionstate.ApplicationIdentity
	containerID int64
	observation *startupApplicationLayout
}

// These fingerprints retain only layout-relevant tree data, never mutable
// GET_TREE pointers, titles, marks, or focus history.
type startupApplicationLayout struct {
	windows   map[int64]struct{}
	structure []startupApplicationStructure
	geometry  []startupApplicationGeometry
}

type startupApplicationStructure struct {
	id         int64
	layout     string
	fullscreen int
	floating   bool
	end        bool
}

type startupApplicationGeometry struct {
	rect       swayipc.Rect
	percent    float64
	hasPercent bool
}

// Observe before application adoption so even an eventless IPC layout/resize
// command cancels reconstruction. During our own reconstruction, observations
// establish a fresh baseline instead of mistaking our effects for user input.
func (runtime *sessionRuntime) observeStartupApplicationLayout(root *Node) {
	if len(runtime.startupApplications) == 0 {
		return
	}
	workspaces := make(map[string]*Node)
	var visit func(*Node)
	visit = func(node *Node) {
		if node == nil {
			return
		}
		if node.Type == "workspace" {
			workspaces[node.Name] = node
			return
		}
		for _, child := range node.Nodes {
			visit(child)
		}
	}
	visit(root)
	observations := make(map[string]*startupApplicationLayout)
	projections := make(map[*startupApplicationLayout]*startupApplicationLayout)
	baselines := make(map[*startupApplicationLayout]*startupApplicationLayout)
	excluded := runtime.lifecycleBlockedWorkspaces()
	for name := range runtime.restoreSuspended {
		excluded[name] = struct{}{}
	}
	for id, pending := range runtime.startupApplications {
		name, found := snapshotContextWorkspace(runtime.desired, id)
		if _, paused := runtime.lifecycleBlocked[id]; paused {
			continue
		}
		if _, paused := excluded[name]; paused {
			continue
		}
		if !found {
			delete(runtime.startupApplications, id)
			continue
		}
		current := observations[name]
		if current == nil {
			current = runtime.startupLayoutWithoutForeignWindows(workspaces[name], nil)
			observations[name] = current
		}
		previous := pending.observation
		if previous != nil && runtime.restoreProgress == nil &&
			!runtime.lateRestorePending && !(runtime.restoreCleanupPending && runtime.restoreCleanup.PendingExcluding(excluded)) {
			// New views legitimately resize existing siblings. Compare structure
			// after projecting them out. Initial client/decorations geometry can
			// settle without another map, so compare rectangles only after the
			// startup gate and with the same window set.
			survivors := projections[previous]
			if survivors == nil {
				survivors = runtime.startupLayoutWithoutForeignWindows(workspaces[name], previous.windows)
				projections[previous] = survivors
				// GET_TREE can contain a foreign view before its queued new
				// event. Apply its newly established exclusion to both sides
				// without discarding simultaneous saved-window layout edits.
				baselines[previous] = runtime.projectStartupForeignWindows(previous)
			}
			baseline := baselines[previous]
			if !slices.Equal(baseline.structure, survivors.structure) ||
				runtime.startupComplete && len(previous.windows) == len(current.windows) && !slices.Equal(baseline.geometry, current.geometry) {
				runtime.cancelConflictingRestore()
				return
			}
		}
		pending.observation = current
		runtime.startupApplications[id] = pending
	}
}

// Foreign view lifecycle is not a change to the saved windows' layout. Retain
// the full view set for the existing mapping/geometry-settling guard, while
// comparing structure and geometry only after projecting those views out.
func (runtime *sessionRuntime) startupLayoutWithoutForeignWindows(root *Node, keep map[int64]struct{}) *startupApplicationLayout {
	return runtime.projectStartupForeignWindows(startupApplicationLayoutFingerprint(root, keep))
}

// Project the immutable fingerprint too: newly identified foreign views may
// already be present in an earlier observation. End markers retain enough tree
// structure to prune empty parents together with their excluded descendants.
func (runtime *sessionRuntime) projectStartupForeignWindows(observed *startupApplicationLayout) *startupApplicationLayout {
	if len(runtime.restoreForeignFocus) == 0 {
		return observed
	}
	filtered := &startupApplicationLayout{windows: observed.windows}
	structureIndex, geometryIndex := 0, 0
	var visit func() bool
	visit = func() bool {
		node := observed.structure[structureIndex]
		geometry := observed.geometry[geometryIndex]
		structureIndex++
		geometryIndex++
		structureStart, geometryStart := len(filtered.structure), len(filtered.geometry)
		filtered.structure = append(filtered.structure, node)
		filtered.geometry = append(filtered.geometry, geometry)
		_, present := observed.windows[node.id]
		for !observed.structure[structureIndex].end {
			present = visit() || present
		}
		structureIndex++
		_, foreign := runtime.restoreForeignFocus[node.id]
		if foreign || !present {
			filtered.structure = filtered.structure[:structureStart]
			filtered.geometry = filtered.geometry[:geometryStart]
			return false
		}
		filtered.structure = append(filtered.structure, startupApplicationStructure{end: true})
		return true
	}
	if len(observed.structure) != 0 {
		visit()
	}
	return filtered
}

func startupApplicationLayoutFingerprint(root *Node, keep map[int64]struct{}) *startupApplicationLayout {
	result := &startupApplicationLayout{windows: make(map[int64]struct{})}
	var visit func(*Node, bool) bool
	visit = func(node *Node, floating bool) bool {
		if node == nil {
			return false
		}
		view := node.AppID != nil || node.Window != nil
		if view && keep != nil {
			if _, exists := keep[node.ID]; !exists {
				return false
			}
		}
		structureStart, geometryStart := len(result.structure), len(result.geometry)
		result.structure = append(result.structure, startupApplicationStructure{
			id: node.ID, layout: node.Layout, fullscreen: node.FullscreenMode, floating: floating,
		})
		geometry := startupApplicationGeometry{rect: node.Rect}
		if node.Percent != nil {
			geometry.percent, geometry.hasPercent = *node.Percent, true
		}
		result.geometry = append(result.geometry, geometry)
		present := view
		if view {
			result.windows[node.ID] = struct{}{}
		}
		for _, child := range node.Nodes {
			present = visit(child, false) || present
		}
		for _, child := range node.FloatingNodes {
			present = visit(child, true) || present
		}
		if !present {
			result.structure = result.structure[:structureStart]
			result.geometry = result.geometry[:geometryStart]
			return false
		}
		result.structure = append(result.structure, startupApplicationStructure{end: true})
		return true
	}
	visit(root, false)
	return result
}

// Initial placement can move a newly mapped window before structural restore
// begins. Rebase only its affected workspaces on the next fresh tree; those
// known successful effects must not be mistaken for independent user edits.
func (runtime *sessionRuntime) rebaseStartupApplicationPlacement(root *Node, action sessionstate.PlacementAction) {
	if len(runtime.startupApplications) == 0 {
		return
	}
	source := ""
	if workspace := pathWorkspace(containerPath(root, action.ContainerID)); workspace != nil {
		source = workspace.Name
	}
	for id, pending := range runtime.startupApplications {
		name, found := snapshotContextWorkspace(runtime.desired, id)
		if found && (name == source || name == action.Workspace) {
			pending.observation = nil
			runtime.startupApplications[id] = pending
		}
	}
}

func pendingStartupApplications(registry sessionstate.Registry, desired sessionstate.LayoutSnapshot) map[sessionstate.ContextID]startupApplication {
	pending := make(map[sessionstate.ContextID]startupApplication)
	for _, context := range registry.Contexts {
		if context.State != sessionstate.ContextActive || context.App == nil || !context.App.DesiredOpen {
			continue
		}
		name, found := snapshotContextWorkspace(desired, context.ID)
		workspace, exists := workspaceByName(desired, name)
		if found && exists && workspace.RestoreMode == sessionstate.WorkspaceRestoreLayout {
			pending[context.ID] = startupApplication{identity: context.App.Identity}
		}
	}
	return pending
}

func (runtime *sessionRuntime) observeStartupApplications(registry sessionstate.Registry, groups map[sessionstate.ContextID]sessionstate.ApplicationGroup) {
	if len(runtime.startupApplications) == 0 {
		return
	}
	current := make(map[sessionstate.ContextID]sessionstate.Context, len(registry.Contexts))
	for _, context := range registry.Contexts {
		current[context.ID] = context
	}
	for id, pending := range runtime.startupApplications {
		context, exists := current[id]
		if _, paused := runtime.lifecycleBlocked[id]; paused {
			continue
		}
		if !exists || context.State != sessionstate.ContextActive || context.App == nil || !context.App.DesiredOpen || context.App.Identity != pending.identity {
			delete(runtime.startupApplications, id)
			continue
		}
		group := groups[id]
		if len(group.Windows) == 0 && pending.containerID == 0 {
			continue
		}
		// Existing marks, ambiguous groups, and vanished/replaced anchors must
		// not turn a later user reopen into another structural startup restore.
		if group.Ambiguous || group.Anchor == nil || group.AnchorMarked ||
			pending.containerID != 0 && pending.containerID != group.Anchor.ContainerID {
			delete(runtime.startupApplications, id)
			continue
		}
		pending.containerID = group.Anchor.ContainerID
		runtime.startupApplications[id] = pending
	}
}
