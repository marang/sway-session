package session

import (
	"fmt"
	"slices"
	"strings"

	"github.com/marang/sway-session/internal/swayipc"
)

// RestoreCleanup retains ownership of temporary effects independently of the
// structural restore cursor. Record attempts before IPC: a missing reply does
// not establish whether a move or mark was applied. Plan always reobserves it.
// The persisted layout and reserved staging workspace remain the recovery
// boundary across daemon restarts; this ledger pins container IDs within a run.
type RestoreCleanup struct {
	staged map[ContextID]RestoreAction
	marks  map[string]RestoreAction
	after  string
}

func (cleanup *RestoreCleanup) Remember(action RestoreAction) {
	switch {
	case action.Kind == RestoreMoveWorkspace && action.Target == RestoreStagingWorkspace:
		if action.ContextID == "" || action.Workspace == "" || action.ContainerID <= 0 {
			return
		}
		if cleanup.staged == nil {
			cleanup.staged = make(map[ContextID]RestoreAction)
		}
		cleanup.staged[action.ContextID] = action
	case action.Kind == RestoreAddTemporaryMark && strings.HasPrefix(action.Target, temporaryMarkPrefix(action.Workspace)):
		if cleanup.marks == nil {
			cleanup.marks = make(map[string]RestoreAction)
		}
		cleanup.marks[action.Target] = action
	}
}

func (cleanup *RestoreCleanup) Pending() bool {
	return len(cleanup.staged) != 0 || len(cleanup.marks) != 0
}

// Recover adopts only saved contexts already in the reserved workspace on the
// first trusted observation of a new daemon. This also handles cancellation
// before startup selection has had an opportunity to rediscover staging.
func (cleanup *RestoreCleanup) Recover(root *swayipc.TreeNode, registry Registry, desired LayoutSnapshot) error {
	if err := desired.Validate(); err != nil {
		return fmt.Errorf("validate restore recovery layout: %w", err)
	}
	observation, err := observeRestoreTree(root, registry)
	if err != nil {
		return err
	}
	for _, workspace := range desired.Workspaces {
		ids := workspaceContextIDs(workspace)
		for _, id := range ids {
			node := observation.contexts[id]
			if node == nil || observation.workspaceName(node) != RestoreStagingWorkspace {
				continue
			}
			cleanup.Remember(RestoreAction{Kind: RestoreMoveWorkspace, Workspace: workspace.Name, ContextID: id, ContainerID: node.ID, Target: RestoreStagingWorkspace})
		}
		// Existing deterministic restore marks are the restart recovery evidence
		// used by the structural planner as well. Require a related layout group;
		// a prefix alone does not authorize removing a mark on an unrelated node.
		for mark, node := range observation.marks {
			if !strings.HasPrefix(mark, temporaryMarkPrefix(workspace.Name)) || observation.workspaceName(node) != workspace.Name || len(node.Nodes) == 0 {
				continue
			}
			children := observation.managedDescendants(node)
			if len(children) != 0 && isSubset(children, ids) {
				cleanup.Remember(RestoreAction{Kind: RestoreAddTemporaryMark, Workspace: workspace.Name, ContainerID: node.ID, Target: mark})
			}
		}
	}
	return nil
}

// Plan returns at most one compensating action. It never reconstructs layout,
// changes focus, or moves a window that has left staging, including one moved
// by the user. Entries survive errors and disappear only after observation.
func (cleanup *RestoreCleanup) Plan(root *swayipc.TreeNode, registry Registry) (*RestoreAction, error) {
	observation, err := observeRestoreTree(root, registry)
	if err != nil {
		return nil, err
	}
	actions := make([]RestoreAction, 0, len(cleanup.staged)+len(cleanup.marks))
	for id, owned := range cleanup.staged {
		node := observation.contexts[id]
		if node == nil || node.ID != owned.ContainerID || observation.workspaceName(node) != RestoreStagingWorkspace {
			delete(cleanup.staged, id)
			continue
		}
		actions = append(actions, RestoreAction{Kind: RestoreMoveWorkspace, Workspace: owned.Workspace, ContextID: id, ContainerID: node.ID, Target: owned.Workspace})
	}
	var markErr error
	for mark, owned := range cleanup.marks {
		node := observation.marks[mark]
		if node == nil {
			delete(cleanup.marks, mark)
			continue
		}
		if node.ID != owned.ContainerID {
			markErr = fmt.Errorf("restore cleanup mark changed containers")
			continue
		}
		actions = append(actions, RestoreAction{Kind: RestoreRemoveMark, Workspace: owned.Workspace, ContainerID: node.ID, Target: mark})
	}
	if len(actions) == 0 {
		return nil, markErr
	}
	// Rotate attempts, including rejected commands, so one inaccessible target
	// cannot prevent other windows or marks from being cleaned up.
	slices.SortFunc(actions, func(a, b RestoreAction) int { return strings.Compare(a.Key(), b.Key()) })
	selected := actions[0]
	for _, action := range actions {
		if action.Key() > cleanup.after {
			selected = action
			break
		}
	}
	cleanup.after = selected.Key()
	return &selected, nil
}
