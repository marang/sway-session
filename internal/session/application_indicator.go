package session

import (
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/marang/sway-title-animator/internal/swayipc"
	"github.com/marang/sway-title-animator/internal/titleindicator"
)

type ApplicationIndicatorActionKind string

const (
	ApplicationIndicatorRemove     ApplicationIndicatorActionKind = "remove"
	ApplicationIndicatorAdd        ApplicationIndicatorActionKind = "add"
	maxApplicationIndicatorActions                                = 1024
)

// ApplicationIndicatorAction is one idempotently replannable presentation
// mutation. It carries no registry identity or launcher data.
type ApplicationIndicatorAction struct {
	Kind        ApplicationIndicatorActionKind
	ContainerID int64
	State       titleindicator.State
	Mark        string
}

// PlanApplicationIndicatorActions derives presentation marks without making
// title formatting part of the session runtime. The caller applies actions in
// order and obtains a fresh tree after any ambiguous Sway response.
func PlanApplicationIndicatorActions(
	root *swayipc.TreeNode,
	registry Registry,
	catalog DesktopCatalog,
	pending []ApplicationOperation,
) ([]ApplicationIndicatorAction, error) {
	return PlanApplicationIndicatorActionsAfter(root, registry, catalog, pending, nil)
}

// PlanApplicationIndicatorActionsAfter rotates the bounded batch beyond the
// last planned mutation, so repeated confirmed failures cannot pin all later
// windows behind one deterministic prefix.
func PlanApplicationIndicatorActionsAfter(
	root *swayipc.TreeNode,
	registry Registry,
	catalog DesktopCatalog,
	pending []ApplicationOperation,
	after *ApplicationIndicatorAction,
) ([]ApplicationIndicatorAction, error) {
	if root == nil {
		return nil, errors.New("sway tree is nil")
	}
	if err := registry.Validate(); err != nil {
		return nil, fmt.Errorf("validate context registry: %w", err)
	}
	desired := make(map[int64]titleindicator.State)
	if registry.Preferences.DesktopIndicators {
		index := buildApplicationContextIndex(&registry, false)
		windows, err := ApplicationWindows(root)
		if err != nil {
			return nil, err
		}
		for _, window := range windows {
			state, visible, err := applicationIndicatorState(window, index, catalog, pending)
			if err != nil {
				return nil, err
			}
			if visible {
				desired[window.ContainerID] = state
			}
		}
	}

	nodes := make([]*swayipc.TreeNode, 0)
	collectIndicatorNodes(root, &nodes)
	sort.Slice(nodes, func(left, right int) bool { return nodes[left].ID < nodes[right].ID })
	actions := make([]ApplicationIndicatorAction, 0)
	for _, node := range nodes {
		want, wanted := desired[node.ID]
		hasWanted := false
		for _, observed := range titleindicator.KnownMarks(node.Marks) {
			if wanted && observed.State == want && observed.ContainerID == node.ID {
				hasWanted = true
				continue
			}
			actions = append(actions, ApplicationIndicatorAction{
				Kind: ApplicationIndicatorRemove, ContainerID: node.ID, State: observed.State, Mark: observed.Raw,
			})
		}
		if wanted && !hasWanted {
			mark, err := titleindicator.Mark(want, node.ID)
			if err != nil {
				return nil, err
			}
			actions = append(actions, ApplicationIndicatorAction{
				Kind: ApplicationIndicatorAdd, ContainerID: node.ID, State: want, Mark: mark,
			})
		}
	}
	return boundedApplicationIndicatorActionsAfter(actions, after), nil
}

func applicationIndicatorState(
	window WindowApplication,
	index applicationContextIndex,
	catalog DesktopCatalog,
	pending []ApplicationOperation,
) (titleindicator.State, bool, error) {
	registered, suppressed, err := indicatorRegisteredContext(window, index)
	if err != nil || suppressed {
		return "", false, err
	}
	if operationPendingForWindow(window, registered, pending, index, catalog) {
		return titleindicator.Pending, true, nil
	}
	if registered != nil {
		if registered.App.RestorePolicy == ApplicationRestorePinned {
			return titleindicator.Pinned, true, nil
		}
		return titleindicator.Registered, true, nil
	}
	if len(DesktopCandidatesForWindow(window, catalog)) == 0 {
		return "", false, nil
	}
	return titleindicator.Unregistered, true, nil
}

func indicatorRegisteredContext(window WindowApplication, index applicationContextIndex) (*Context, bool, error) {
	if len(window.ContextMarks) == 1 {
		id := window.ContextMarks[0]
		if context, exists := index.contextByID(id); exists {
			if context.App == nil {
				return nil, true, nil
			}
			if !applicationIdentitiesOverlap(context.App.Identity, window.Identity) {
				return nil, false, fmt.Errorf("application window %d identity conflicts with persistent context %q", window.ContainerID, id)
			}
			return context, false, nil
		}
		return nil, false, fmt.Errorf("application window %d has unknown persistent context mark %q", window.ContainerID, id)
	}
	match, _, err := index.match(window.Identity)
	if err != nil {
		return nil, false, fmt.Errorf("application window %d: %w", window.ContainerID, err)
	}
	return match, false, nil
}

func operationPendingForWindow(window WindowApplication, registered *Context, operations []ApplicationOperation, index applicationContextIndex, catalog DesktopCatalog) bool {
	for _, operation := range operations {
		for _, item := range operation.Items {
			switch operation.Kind {
			case OperationRegister:
				_, contextExists := index.contextByID(item.ContextID)
				if registered == nil && !contextExists && item.Window != nil &&
					reflect.DeepEqual(window, *item.Window) && operationDesktopCandidateCurrent(window, catalog, item.DesktopID) {
					return true
				}
			case OperationRebind:
				if item.Window != nil && reflect.DeepEqual(window, *item.Window) && operationContextCurrent(index, item) &&
					operationDesktopCandidateCurrent(window, catalog, item.DesktopID) {
					return true
				}
			case OperationReapprove:
				_, candidateExists := catalog.ByID(item.DesktopID)
				if candidateExists && registered != nil && registered.ID == item.ContextID && operationContextCurrent(index, item) {
					return true
				}
			}
		}
	}
	return false
}

func operationDesktopCandidateCurrent(window WindowApplication, catalog DesktopCatalog, desktopID string) bool {
	for _, candidate := range DesktopCandidatesForWindow(window, catalog) {
		if candidate.ID == desktopID {
			return true
		}
	}
	return false
}

func operationContextCurrent(index applicationContextIndex, item ApplicationOperationItem) bool {
	context, exists := index.contextByID(item.ContextID)
	if !exists || context.App == nil {
		return false
	}
	revision, err := ApplicationOperationContextRevision(*context)
	return err == nil && revision == item.ContextRevision
}

func collectIndicatorNodes(node *swayipc.TreeNode, nodes *[]*swayipc.TreeNode) {
	if node == nil {
		return
	}
	if node.ID > 0 && (len(titleindicator.KnownMarks(node.Marks)) != 0) {
		*nodes = append(*nodes, node)
	} else if node.ID > 0 && len(node.Nodes) == 0 && len(node.FloatingNodes) == 0 {
		*nodes = append(*nodes, node)
	}
	for _, child := range node.Nodes {
		collectIndicatorNodes(child, nodes)
	}
	for _, child := range node.FloatingNodes {
		collectIndicatorNodes(child, nodes)
	}
}
