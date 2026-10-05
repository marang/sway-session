package main

import (
	"slices"
	"strings"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// An unrelated view gets only the focus caused by its observed mapping, within
// that mapping's epoch/tick barrier. While it owns focus, defer reconstruction
// rather than discard saved intent or steal focus from a possible secrets prompt.
// IPC cannot distinguish a coincident manual focus of this same new view.
type restoreInterruption struct {
	epoch, sequence uint64
	active          bool
	focused         bool
	nextFocus       int64
}

func (runtime *sessionRuntime) observeUnregisteredMappingFocus(root *Node, nodes map[int64]*Node, known map[int64]struct{}) {
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
		// allowance by being mistaken for an unrelated authentication view.
		if hasPersistentIdentity(containerPath(root, id)) {
			continue
		}
		if len(runtime.expectedFocus) >= 64 || len(runtime.restoreInterruptions) >= 64 {
			runtime.cancelConflictingRestore()
			return
		}
		if runtime.restoreInterruptions == nil {
			runtime.restoreInterruptions = make(map[int64]restoreInterruption)
		}
		runtime.restoreInterruptions[id] = restoreInterruption{
			epoch: mapping.epoch, sequence: mapping.sequence, active: node.Focused,
		}
		delete(runtime.pendingMappingFocus, id)
		runtime.expectedFocus = append(runtime.expectedFocus, restoreFocusExpectation{
			sequence: mapping.sequence, epoch: mapping.epoch, mapping: true,
			events: []restoreFocusEvent{{kind: swayipc.EventWindow, container: id}},
		})
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

func (runtime *sessionRuntime) restoreInterrupted(root *Node, now time.Time) bool {
	paused := false
	for id, interruption := range runtime.restoreInterruptions {
		if !interruption.active {
			continue
		}
		// Keep a missing view paused until its ordered close event arrives.
		// Disconnection invalidates the episode instead of inventing closure.
		if node := findContainerByID(root, id); node != nil {
			interruption.focused = node.Focused
			interruption.nextFocus = 0
			if workspace := pathWorkspace(containerPath(root, id)); node.Focused && workspace != nil {
				if next := focusLeafExcept(workspace, id); next != nil {
					interruption.nextFocus = next.ID
				}
			}
			runtime.restoreInterruptions[id] = interruption
		}
		paused = paused || interruption.focused
	}
	if paused {
		if !runtime.observeInterruptedLayout(root) {
			return false
		}
		runtime.debouncer.Cancel()
		runtime.startupDeadline = time.Time{}
	} else if runtime.restoreInterruptionPaused && !runtime.restoreCancelled {
		// A user may have edited the layout immediately before closure, with
		// no intervening event or observation. Validate the final surviving
		// tree before discarding the pause baseline.
		if !runtime.observeInterruptedLayout(root) {
			return false
		}
		// Authentication can outlast both startup settling and report timeouts.
		// Resume with a fresh bounded observation period, keeping attempt IDs
		// and actual start timestamps unchanged.
		runtime.restoreResumeAt = now
		if !runtime.startupComplete {
			runtime.startupDeadline = now.Add(sessionStartupSettleDelay)
		}
		runtime.restoreInterruptionLayout = nil
		// The transient view's removal is compositor lifecycle, not a user's
		// layout edit. Rebase the existing late-application observer once.
		for id, pending := range runtime.startupApplications {
			pending.observation = nil
			runtime.startupApplications[id] = pending
		}
	}
	runtime.restoreInterruptionPaused = paused
	return paused
}

func (runtime *sessionRuntime) observeInterruptedLayout(root *Node) bool {
	if runtime.restoreInterruptionLayout == nil {
		runtime.restoreInterruptionLayout = make(map[string]*startupApplicationLayout)
	}
	for _, desired := range runtime.desired.Workspaces {
		node := namedWorkspaceNode(root, desired.Name)
		current := startupApplicationLayoutFingerprint(node, nil)
		for id := range runtime.restoreInterruptions {
			delete(current.windows, id)
		}
		current = startupApplicationLayoutFingerprint(node, current.windows)
		if previous := runtime.restoreInterruptionLayout[desired.Name]; previous != nil {
			// Mapping new views can resize siblings. Project new views out
			// before comparing the surviving structure, as the late-application
			// observer does. Independent IPC layout edits still take precedence.
			survivors := startupApplicationLayoutFingerprint(node, previous.windows)
			if !slices.Equal(previous.structure, survivors.structure) ||
				runtime.startupComplete && len(previous.windows) == len(current.windows) && !slices.Equal(previous.geometry, current.geometry) {
				runtime.cancelConflictingRestore()
				return false
			}
		}
		runtime.restoreInterruptionLayout[desired.Name] = current
	}
	return true
}

func namedWorkspaceNode(root *Node, name string) *Node {
	if root == nil {
		return nil
	}
	if root.Type == "workspace" {
		if root.Name == name {
			return root
		}
		return nil
	}
	for _, child := range root.Nodes {
		if found := namedWorkspaceNode(child, name); found != nil {
			return found
		}
	}
	return nil
}

func (runtime *sessionRuntime) consumeInterruptionClose(event swayipc.Event) bool {
	if event.Container == nil {
		return false
	}
	id := event.Container.ID
	interruption, exists := runtime.restoreInterruptions[id]
	if !exists || interruption.epoch != runtime.eventStreamEpoch ||
		event.StreamEpoch != 0 && event.StreamEpoch != interruption.epoch {
		return false
	}
	delete(runtime.restoreInterruptions, id)
	runtime.expectedFocus = slices.DeleteFunc(runtime.expectedFocus, func(expected restoreFocusExpectation) bool {
		return expected.mapping && expected.events[0].container == id
	})
	// Destruction emits close before refocusing the surviving focus-stack leaf.
	// Consume only that predicted successor, once, before this close's tick.
	if interruption.focused && interruption.nextFocus > 0 {
		if len(runtime.expectedFocus) >= 64 {
			runtime.cancelConflictingRestore()
			return true
		}
		runtime.nextMoveSequence++
		sequence := runtime.nextMoveSequence
		runtime.expectedFocus = append(runtime.expectedFocus, restoreFocusExpectation{
			sequence: sequence, epoch: interruption.epoch, mapping: true,
			events: []restoreFocusEvent{{kind: swayipc.EventWindow, container: interruption.nextFocus}},
		})
		if err := runtime.sendMoveBarrier(sequence); err != nil {
			runtime.cancelConflictingRestore()
		}
	}
	return true
}
