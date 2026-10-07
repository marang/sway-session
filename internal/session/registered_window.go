package session

import (
	"errors"
	"fmt"

	"github.com/marang/sway-session/internal/swayipc"
)

// ObserveRegisteredWindowContext identifies an eligible registered window from
// one compositor event node. It needs no workspace ancestors or live tree, so
// queued close events retain their identity after the window has disappeared.
// Structural nodes without their own window identity are left to the caller.
func ObserveRegisteredWindowContext(node *swayipc.TreeNode, registry Registry) (ContextID, bool, error) {
	if err := registry.Validate(); err != nil {
		return "", false, fmt.Errorf("validate context registry: %w", err)
	}
	if node == nil || node.ID <= 0 {
		return "", false, errors.New("invalid compositor window node")
	}
	identities, err := managedIdentities(node)
	if err != nil {
		return "", false, err
	}
	if len(identities) > 1 {
		return "", false, errors.New("container has conflicting managed identities")
	}
	if len(node.Nodes)+len(node.FloatingNodes) != 0 {
		if len(identities) != 0 || pointerString(node.AppID) != "" || node.Window != nil || node.WindowProperties.Class != "" || node.WindowProperties.Instance != "" {
			return "", false, errors.New("window identity is attached to a layout parent")
		}
		return "", false, nil
	}
	if node.WindowProperties.TransientFor != nil || node.WindowProperties.WindowType != "" && node.WindowProperties.WindowType != "normal" {
		return "", false, nil
	}
	index := buildApplicationContextIndex(&registry, false)
	var marked *Context
	if len(identities) == 1 {
		var exists bool
		marked, exists = index.contextByID(identities[0])
		if !exists {
			// An unknown reserved identity claims separate ownership. Do not
			// reinterpret its native application identity as another context.
			return "", false, nil
		}
		if marked.App == nil {
			if EvaluateRestorePolicy(*marked).Eligible {
				return marked.ID, true, nil
			}
			return "", false, nil
		}
	}
	identity, present, err := compositorApplicationIdentity(node)
	if err != nil {
		classes, instances := make(map[string]struct{}), make(map[string]struct{})
		for _, context := range registry.Contexts {
			if context.App != nil && context.App.Identity.Protocol == WindowXWayland {
				classes[context.App.Identity.X11Class] = struct{}{}
				instances[context.App.Identity.X11Instance] = struct{}{}
			}
		}
		if captureUnrelatedIncompleteXWayland(node, classes, instances) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("observe application window identity: %w", err)
	}
	if marked != nil && (!present || !applicationIdentitiesOverlap(marked.App.Identity, identity)) {
		return "", false, fmt.Errorf("application window is marked for context %q but its identity changed", marked.ID)
	}
	if !present {
		return "", false, nil
	}
	matched, exists, err := index.match(identity)
	if err != nil {
		return "", false, fmt.Errorf("match application window: %w", err)
	}
	if !exists || !EvaluateRestorePolicy(*matched).Eligible {
		return "", false, nil
	}
	return matched.ID, true, nil
}
