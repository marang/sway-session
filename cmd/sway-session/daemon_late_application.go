package main

import (
	"maps"
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
	viewCount int
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

// Observe the windows eligible for persistence before lifecycle effect locks.
// Desktop anchors can be identifiable before adoption adds their context mark.
func (runtime *sessionRuntime) observeRestoreWindows(root *Node, registry sessionstate.Registry) (map[int64]struct{}, error) {
	owned := make(map[int64]struct{})
	if runtime.restoreCancelled || len(runtime.desired.Workspaces)+len(runtime.desired.Scratchpad) == 0 {
		return owned, nil
	}
	windows, issues, err := sessionstate.ObserveManagedWindowsIsolated(root, registry)
	if err != nil {
		return nil, err
	}
	groups, err := sessionstate.ObserveApplicationGroupsForCapture(root, registry)
	if err != nil {
		return nil, err
	}
	unsafe := make(map[sessionstate.ContextID]struct{}, len(issues))
	for _, issue := range issues {
		unsafe[issue.ContextID] = struct{}{}
	}
	counts := make(map[int64]int)
	var countNodes func(*Node)
	countNodes = func(node *Node) {
		counts[node.ID]++
		for _, children := range [][]*Node{node.Nodes, node.FloatingNodes} {
			for _, child := range children {
				countNodes(child)
			}
		}
	}
	countNodes(root)
	for _, item := range registry.Contexts {
		if !sessionstate.EvaluateRestorePolicy(item).Eligible {
			continue
		}
		if _, invalid := unsafe[item.ID]; invalid {
			continue
		}
		if item.App != nil {
			group := groups[item.ID]
			if !group.Ambiguous && group.Anchor != nil && counts[group.Anchor.ContainerID] == 1 {
				owned[group.Anchor.ContainerID] = struct{}{}
			}
		} else if window, exists := windows[item.ID]; exists && counts[window.ContainerID] == 1 {
			owned[window.ContainerID] = struct{}{}
		}
	}
	return owned, nil
}

// Observe before application adoption so even an eventless IPC layout/resize
// command cancels reconstruction. During our own reconstruction, observations
// establish a fresh baseline instead of mistaking our effects for user input.
func (runtime *sessionRuntime) observeStartupApplicationLayout(root *Node, ownedIDs map[int64]struct{}) {
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
			current = startupApplicationLayoutFingerprint(workspaces[name], ownedIDs, nil)
			observations[name] = current
		}
		previous := pending.observation
		if previous != nil && runtime.restoreProgress == nil &&
			!runtime.lateRestorePending && !(runtime.restoreCleanupPending && runtime.restoreCleanup.PendingExcluding(excluded)) {
			// Compare only previously owned views for structural changes. Any
			// mapped sibling can resize them, so compare geometry only after
			// startup, with unchanged ownership and the same total view count.
			survivors := projections[previous]
			if survivors == nil {
				survivors = startupApplicationLayoutFingerprint(workspaces[name], ownedIDs, previous.windows)
				projections[previous] = survivors
			}
			sameViews := previous.viewCount == current.viewCount && maps.Equal(previous.windows, current.windows)
			if !slices.Equal(previous.structure, survivors.structure) ||
				runtime.startupComplete && sameViews && !slices.Equal(previous.geometry, current.geometry) {
				runtime.cancelConflictingRestore()
				return
			}
		}
		pending.observation = current
		runtime.startupApplications[id] = pending
	}
}

// A nil ownedIDs preserves the complete-tree comparison used by lifecycle
// suspension. Startup supplies its freshly validated positive selection.
func startupApplicationLayoutFingerprint(root *Node, ownedIDs, keep map[int64]struct{}) *startupApplicationLayout {
	result := &startupApplicationLayout{windows: make(map[int64]struct{})}
	var visit func(*Node, bool) bool
	visit = func(node *Node, floating bool) bool {
		if node == nil {
			return false
		}
		view := node.AppID != nil || node.Window != nil
		if view {
			result.viewCount++
			if ownedIDs != nil {
				if _, owned := ownedIDs[node.ID]; !owned {
					return false
				}
			}
			if keep != nil {
				if _, retained := keep[node.ID]; !retained {
					return false
				}
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
