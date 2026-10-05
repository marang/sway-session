package main

import (
	"slices"
	"strings"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// A newly observed unregistered view is not a saved restore target. Its focus,
// movement and closure do not delay reconstruction. Only the automatic focus
// returned to another window on close or movement needs a bounded allowance.
type restoreForeignFocus struct {
	epoch      uint64
	focused    bool
	successors []int64
}

func (runtime *sessionRuntime) observeUnregisteredMappingFocus(root *Node, nodes map[int64]*Node, known map[int64]struct{}) {
	// Registration can take ownership of a formerly unrelated window. Fresh
	// validated identities, including identities on ancestors, revoke its
	// exemption before any subsequent lifecycle event is processed.
	for id := range runtime.restoreForeignFocus {
		_, registered := known[id]
		node, observed := nodes[id]
		if registered || observed && node == nil || hasPersistentIdentity(containerPath(root, id)) {
			delete(runtime.restoreForeignFocus, id)
		}
	}
	if !runtime.restoreMayConflictWithUserIntent() {
		return
	}
	for id, mapping := range runtime.pendingMappingFocus {
		node := nodes[id]
		if node == nil || node.AppID == nil && node.Window == nil || mapping.epoch != runtime.eventStreamEpoch {
			continue
		}
		if _, registered := known[id]; registered {
			continue
		}
		// Unknown or malformed persistent identities must never gain an
		// exemption by being mistaken for an unrelated authentication view.
		if hasPersistentIdentity(containerPath(root, id)) {
			continue
		}
		if len(runtime.restoreForeignFocus) >= 64 {
			runtime.cancelUncertainRestore()
			return
		}
		if runtime.restoreForeignFocus == nil {
			runtime.restoreForeignFocus = make(map[int64]restoreForeignFocus)
		}
		runtime.restoreForeignFocus[id] = restoreForeignFocus{epoch: mapping.epoch, focused: node.Focused}
		delete(runtime.pendingMappingFocus, id)
	}
}

func hasPersistentIdentity(path []*Node) bool {
	for _, node := range path {
		if node.AppID != nil && strings.HasPrefix(*node.AppID, sessionstate.AppIDPrefix) {
			return true
		}
		for _, mark := range node.Marks {
			if strings.HasPrefix(mark, sessionstate.MarkPrefix) {
				return true
			}
		}
	}
	return false
}

func (runtime *sessionRuntime) isForeignWindowEvent(event swayipc.Event) bool {
	if runtime.eventStreamState != nil {
		epoch, connected := runtime.eventStreamState.Snapshot()
		if !connected || epoch != runtime.eventStreamEpoch {
			return false
		}
	}
	if event.Type != swayipc.EventWindow || event.Container == nil || hasPersistentIdentity([]*Node{event.Container}) {
		return false
	}
	foreign, exists := runtime.restoreForeignFocus[event.Container.ID]
	return exists && foreign.epoch == runtime.eventStreamEpoch &&
		(event.StreamEpoch == 0 || event.StreamEpoch == foreign.epoch)
}

// Keep focus-stack successors, including unfocused foreign windows, so queued
// close(B), focus(A), close(A), focus(saved) remains attributable even when the
// next tree already reflects both closures. No saved window waits on this data.
func (runtime *sessionRuntime) observeForeignWindowFocus(root *Node) {
	for id, foreign := range runtime.restoreForeignFocus {
		if node := findContainerByID(root, id); node != nil {
			// Focus comes from the ordered event queue. A newer tree may
			// already show this view moved away before its move/refocus
			// events arrive; retain the last observed source in that case.
			if node.Focused || foreign.successors == nil {
				if workspace := pathWorkspace(containerPath(root, id)); workspace != nil {
					foreign.successors = foreignFocusSuccessors(workspace, id)
				}
			}
			runtime.restoreForeignFocus[id] = foreign
		}
	}
}

func foreignFocusSuccessors(root *Node, excluded int64) []int64 {
	var successors []int64
	var visit func(*Node)
	visit = func(node *Node) {
		// At most 64 foreign views can be tracked. Their closures need only
		// the first 65 alternatives, regardless of the total context count.
		if node.ID == excluded || len(successors) >= 65 {
			return
		}
		if len(node.Nodes)+len(node.FloatingNodes) == 0 {
			if node.AppID != nil || node.Window != nil {
				successors = append(successors, node.ID)
			}
			return
		}
		for _, id := range node.Focus {
			for _, children := range [][]*Node{node.Nodes, node.FloatingNodes} {
				for _, child := range children {
					if child.ID == id {
						visit(child)
						if len(successors) >= 65 {
							return
						}
					}
				}
			}
		}
	}
	visit(root)
	return successors
}

func (runtime *sessionRuntime) consumeForeignWindowClose(event swayipc.Event) bool {
	if !runtime.isForeignWindowEvent(event) {
		return false
	}
	id := event.Container.ID
	foreign := runtime.restoreForeignFocus[id]
	delete(runtime.restoreForeignFocus, id)
	for otherID, other := range runtime.restoreForeignFocus {
		other.successors = slices.DeleteFunc(other.successors, func(successor int64) bool { return successor == id })
		runtime.restoreForeignFocus[otherID] = other
	}
	runtime.expectedFocus = slices.DeleteFunc(runtime.expectedFocus, func(expected restoreFocusExpectation) bool {
		return expected.mapping && expected.events[0].container == id
	})
	runtime.expectForeignReturnFocus(foreign, event.Container.Focused)
	return true
}

func (runtime *sessionRuntime) consumeForeignWindowMove(event swayipc.Event) bool {
	if !runtime.isForeignWindowEvent(event) {
		return false
	}
	runtime.expectForeignReturnFocus(runtime.restoreForeignFocus[event.Container.ID], event.Container.Focused)
	return true
}

// Closing or moving a focused view refocuses the surviving source leaf. Allow
// only that observed successor, once, before the event's stream/tick barrier.
func (runtime *sessionRuntime) expectForeignReturnFocus(foreign restoreForeignFocus, focused bool) {
	if (foreign.focused || focused) && len(foreign.successors) != 0 {
		if len(runtime.expectedFocus) >= 64 {
			runtime.cancelUncertainRestore()
			return
		}
		runtime.nextMoveSequence++
		sequence := runtime.nextMoveSequence
		runtime.expectedFocus = append(runtime.expectedFocus, restoreFocusExpectation{
			sequence: sequence, epoch: foreign.epoch, mapping: true,
			events: []restoreFocusEvent{{kind: swayipc.EventWindow, container: foreign.successors[0]}},
		})
		if err := runtime.sendMoveBarrier(sequence); err != nil {
			runtime.cancelUncertainRestore()
		}
	}
}
