package session

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/marang/sway-session/internal/swayipc"
)

const (
	ApplicationSessionSchemaVersion  = 1
	MaxApplicationRestoreConcurrency = 128
)

// ApplicationSessionState records conservative launch evidence for one real
// compositor lifetime. It prevents a daemon restart or an ambiguous launcher
// outcome from blindly starting the same application again.
type ApplicationSessionState struct {
	Version      int                        `json:"version"`
	CompositorID string                     `json:"compositor_id"`
	Attempts     []ApplicationLaunchAttempt `json:"attempts"`
}

type ApplicationLaunchAttempt struct {
	ContextID ContextID `json:"context_id"`
	StartedAt time.Time `json:"started_at"`
}

func (state *ApplicationSessionState) Validate() error {
	if state == nil {
		return errors.New("application session state is nil")
	}
	if state.Version != ApplicationSessionSchemaVersion {
		return fmt.Errorf("unsupported application session schema version %d; expected %d", state.Version, ApplicationSessionSchemaVersion)
	}
	decoded, err := hex.DecodeString(state.CompositorID)
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != state.CompositorID {
		return errors.New("compositor ID must contain 64 lowercase hexadecimal characters")
	}
	if state.Attempts == nil {
		return errors.New("application session state must contain an attempts array")
	}
	seen := make(map[ContextID]struct{}, len(state.Attempts))
	for index, attempt := range state.Attempts {
		if err := attempt.ContextID.Validate(); err != nil {
			return fmt.Errorf("attempts[%d]: invalid context ID: %w", index, err)
		}
		if attempt.StartedAt.IsZero() {
			return fmt.Errorf("attempts[%d]: started_at is required", index)
		}
		if _, exists := seen[attempt.ContextID]; exists {
			return fmt.Errorf("attempts[%d]: duplicate context %q", index, attempt.ContextID)
		}
		seen[attempt.ContextID] = struct{}{}
	}
	return nil
}

type ApplicationRestoreOptions struct {
	AdoptionGrace time.Duration
	CloseGrace    time.Duration
	LaunchTimeout time.Duration
	MaxConcurrent int
}

type ApplicationRestorePlan struct {
	DesiredOpen []ApplicationDesiredOpen
	Launch      []Context
	LaunchSlots int
}

type ApplicationDesiredOpen struct {
	ContextID ContextID
	Open      bool
}

type ApplicationRestoreCoordinator struct {
	state            ApplicationSessionState
	adoptionDeadline time.Time
	options          ApplicationRestoreOptions
	seenPresent      map[ContextID]bool
	missingSince     map[ContextID]time.Time
}

func NewApplicationRestoreCoordinator(
	compositorID string,
	previous ApplicationSessionState,
	now time.Time,
	options ApplicationRestoreOptions,
) (*ApplicationRestoreCoordinator, error) {
	probe := ApplicationSessionState{Version: ApplicationSessionSchemaVersion, CompositorID: compositorID, Attempts: []ApplicationLaunchAttempt{}}
	if err := probe.Validate(); err != nil {
		return nil, err
	}
	if options.AdoptionGrace <= 0 || options.CloseGrace <= 0 || options.LaunchTimeout <= 0 {
		return nil, errors.New("application restore durations must be positive")
	}
	if options.MaxConcurrent < 1 || options.MaxConcurrent > MaxApplicationRestoreConcurrency {
		return nil, fmt.Errorf("application restore concurrency must be between 1 and %d", MaxApplicationRestoreConcurrency)
	}
	state := probe
	if previous.CompositorID == compositorID {
		if err := previous.Validate(); err != nil {
			return nil, err
		}
		state = previous
	}
	return &ApplicationRestoreCoordinator{
		state: state, adoptionDeadline: now.Add(options.AdoptionGrace), options: options,
		seenPresent: make(map[ContextID]bool), missingSince: make(map[ContextID]time.Time),
	}, nil
}

func (coordinator *ApplicationRestoreCoordinator) Plan(
	registry Registry,
	groups map[ContextID]ApplicationGroup,
	now time.Time,
) (ApplicationRestorePlan, error) {
	if coordinator == nil {
		return ApplicationRestorePlan{}, errors.New("application restore coordinator is nil")
	}
	if err := registry.Validate(); err != nil {
		return ApplicationRestorePlan{}, fmt.Errorf("validate context registry: %w", err)
	}
	registeredApplications := make(map[ContextID]struct{})
	activeApplications := make(map[ContextID]struct{})
	for _, context := range registry.Contexts {
		if context.App != nil {
			registeredApplications[context.ID] = struct{}{}
			if context.State == ContextActive {
				activeApplications[context.ID] = struct{}{}
			}
		}
	}
	for id := range coordinator.seenPresent {
		if _, active := activeApplications[id]; !active {
			delete(coordinator.seenPresent, id)
		}
	}
	for id := range coordinator.missingSince {
		if _, active := activeApplications[id]; !active {
			delete(coordinator.missingSince, id)
		}
	}
	retainedAttempts := make([]ApplicationLaunchAttempt, 0, len(coordinator.state.Attempts))
	for _, attempt := range coordinator.state.Attempts {
		if _, registered := registeredApplications[attempt.ContextID]; registered {
			retainedAttempts = append(retainedAttempts, attempt)
		}
	}
	coordinator.state.Attempts = retainedAttempts
	plan := ApplicationRestorePlan{DesiredOpen: []ApplicationDesiredOpen{}, Launch: []Context{}}
	closing := make(map[ContextID]struct{})
	for _, context := range registry.Contexts {
		if context.App == nil || context.State != ContextActive {
			continue
		}
		present := len(groups[context.ID].Windows) != 0
		if present {
			coordinator.seenPresent[context.ID] = true
			delete(coordinator.missingSince, context.ID)
			if !context.App.DesiredOpen {
				plan.DesiredOpen = append(plan.DesiredOpen, ApplicationDesiredOpen{ContextID: context.ID, Open: true})
			}
			continue
		}
		if !context.App.DesiredOpen {
			coordinator.seenPresent[context.ID] = false
			delete(coordinator.missingSince, context.ID)
			continue
		}
		if !coordinator.seenPresent[context.ID] || context.App.RestorePolicy == ApplicationRestorePinned {
			continue
		}
		missingSince, tracked := coordinator.missingSince[context.ID]
		if !tracked {
			coordinator.missingSince[context.ID] = now
			continue
		}
		if !now.Before(missingSince.Add(coordinator.options.CloseGrace)) {
			plan.DesiredOpen = append(plan.DesiredOpen, ApplicationDesiredOpen{ContextID: context.ID, Open: false})
			closing[context.ID] = struct{}{}
		}
	}
	if now.Before(coordinator.adoptionDeadline) {
		return plan, nil
	}
	attempted := make(map[ContextID]ApplicationLaunchAttempt, len(coordinator.state.Attempts))
	desiredApplications := make(map[ContextID]struct{})
	for _, context := range registry.Contexts {
		if context.App != nil && context.State == ContextActive && context.App.DesiredOpen {
			desiredApplications[context.ID] = struct{}{}
		}
	}
	inFlight := 0
	for _, attempt := range coordinator.state.Attempts {
		attempted[attempt.ContextID] = attempt
		if _, desired := desiredApplications[attempt.ContextID]; !desired {
			continue
		}
		group := groups[attempt.ContextID]
		if len(group.Windows) == 0 && !attempt.StartedAt.After(now) &&
			now.Before(attempt.StartedAt.Add(coordinator.options.LaunchTimeout)) {
			inFlight++
		}
	}
	plan.LaunchSlots = coordinator.options.MaxConcurrent - inFlight
	if plan.LaunchSlots <= 0 {
		return plan, nil
	}
	contexts := append([]Context(nil), registry.Contexts...)
	sort.Slice(contexts, func(left, right int) bool { return contexts[left].ID < contexts[right].ID })
	for _, context := range contexts {
		if context.App == nil || context.State != ContextActive || !context.App.DesiredOpen || len(groups[context.ID].Windows) != 0 {
			continue
		}
		// Presence adopted in this compositor proves that this application
		// already had its one startup opportunity. A later disappearance is
		// lifecycle evidence, not permission to become a process watchdog.
		// Follow mode can be explicitly re-armed after its desired-close state
		// commits; pinned mode waits for the next real compositor session.
		if coordinator.seenPresent[context.ID] {
			continue
		}
		if _, closed := closing[context.ID]; closed {
			continue
		}
		if _, exists := attempted[context.ID]; exists {
			continue
		}
		plan.Launch = append(plan.Launch, context)
	}
	return plan, nil
}

// BeginAttempt records launch intent before any process is started. Callers
// must durably save the returned state before crossing the launcher boundary.
func (coordinator *ApplicationRestoreCoordinator) BeginAttempt(id ContextID, now time.Time) (ApplicationSessionState, error) {
	if coordinator == nil {
		return ApplicationSessionState{}, errors.New("application restore coordinator is nil")
	}
	if err := id.Validate(); err != nil {
		return ApplicationSessionState{}, err
	}
	if now.IsZero() {
		return ApplicationSessionState{}, errors.New("application launch attempt time is required")
	}
	for _, attempt := range coordinator.state.Attempts {
		if attempt.ContextID == id {
			return ApplicationSessionState{}, fmt.Errorf("application context %q was already attempted in this compositor session", id)
		}
	}
	candidate := coordinator.state
	candidate.Attempts = append(append([]ApplicationLaunchAttempt(nil), candidate.Attempts...), ApplicationLaunchAttempt{
		ContextID: id,
		StartedAt: now.UTC(),
	})
	sort.Slice(candidate.Attempts, func(left, right int) bool {
		return candidate.Attempts[left].ContextID < candidate.Attempts[right].ContextID
	})
	if err := candidate.Validate(); err != nil {
		return ApplicationSessionState{}, err
	}
	coordinator.state = candidate
	return cloneApplicationSessionState(candidate), nil
}

func cloneApplicationSessionState(state ApplicationSessionState) ApplicationSessionState {
	attempts := make([]ApplicationLaunchAttempt, len(state.Attempts))
	copy(attempts, state.Attempts)
	state.Attempts = attempts
	return state
}

func (coordinator *ApplicationRestoreCoordinator) State() ApplicationSessionState {
	if coordinator == nil {
		return ApplicationSessionState{}
	}
	return cloneApplicationSessionState(coordinator.state)
}

func (coordinator *ApplicationRestoreCoordinator) RestoreState(state ApplicationSessionState) error {
	if coordinator == nil {
		return errors.New("application restore coordinator is nil")
	}
	if err := state.Validate(); err != nil {
		return err
	}
	if state.CompositorID != coordinator.state.CompositorID {
		return errors.New("application restore state belongs to another compositor session")
	}
	coordinator.state = cloneApplicationSessionState(state)
	return nil
}

// ApplicationGroup is the complete eligible top-level presence of one
// registered desktop application. Anchor is the sole window which may carry
// application-level placement and layout state. Additional windows remain
// owned by the application.
type ApplicationGroup struct {
	Windows      []WindowApplication
	Anchor       *WindowApplication
	AnchorMarked bool
	Ambiguous    bool
}

// ObserveApplicationGroups matches eligible top-level windows to registered
// application identities. A pre-existing matching context mark selects the
// anchor. Without one, exactly one matching unmarked window is an anchor
// candidate; multiple indistinguishable windows prove presence but are never
// guessed between.
func ObserveApplicationGroups(root *swayipc.TreeNode, registry Registry) (map[ContextID]ApplicationGroup, error) {
	if root == nil {
		return nil, fmt.Errorf("sway tree is nil")
	}
	if err := registry.Validate(); err != nil {
		return nil, fmt.Errorf("validate context registry: %w", err)
	}
	groups := make(map[ContextID]ApplicationGroup)
	applications := make([]Context, 0)
	for _, context := range registry.Contexts {
		if context.App == nil || context.State != ContextActive {
			continue
		}
		groups[context.ID] = ApplicationGroup{Windows: []WindowApplication{}}
		applications = append(applications, context)
	}
	index := buildApplicationContextIndex(&registry, true)
	var matchErr error
	if err := walkApplicationWindowsWithScratchpad(root, "", true, func(window WindowApplication, _ bool) {
		if matchErr != nil {
			return
		}
		if len(window.ContextMarks) == 1 {
			if marked, exists := index.contextByID(window.ContextMarks[0]); exists &&
				!applicationIdentitiesOverlap(marked.App.Identity, window.Identity) {
				group := groups[marked.ID]
				group.Windows = append(group.Windows, window)
				group.Ambiguous = true
				groups[marked.ID] = group
				return
			}
		}
		matched, exists, err := index.match(window.Identity)
		if err != nil {
			matchErr = fmt.Errorf("match application window %d: %w", window.ContainerID, err)
			return
		}
		if exists {
			group := groups[matched.ID]
			group.Windows = append(group.Windows, window)
			groups[matched.ID] = group
		}
	}); err != nil {
		return nil, err
	}
	if matchErr != nil {
		return nil, matchErr
	}
	for _, context := range applications {
		group := groups[context.ID]
		if group.Ambiguous {
			groups[context.ID] = group
			continue
		}
		marked := -1
		for index, window := range group.Windows {
			if len(window.ContextMarks) == 1 && window.ContextMarks[0] == context.ID {
				if marked >= 0 {
					group.Ambiguous = true
					marked = -2
					break
				}
				marked = index
			}
		}
		switch {
		case marked >= 0:
			anchor := group.Windows[marked]
			group.Anchor = &anchor
			group.AnchorMarked = true
		case marked == -2:
		case len(group.Windows) == 1 && len(group.Windows[0].ContextMarks) == 0 && validApplicationWorkspace(group.Windows[0].Workspace):
			anchor := group.Windows[0]
			group.Anchor = &anchor
		default:
			group.Ambiguous = len(group.Windows) > 0
		}
		groups[context.ID] = group
	}
	return groups, nil
}

// PlanApplicationPlacementActions adopts only an unambiguous first top-level
// as an application-level layout anchor. Already marked anchors and ambiguous
// multi-window groups remain untouched, preserving live user/app ownership.
func PlanApplicationPlacementActions(groups map[ContextID]ApplicationGroup, desired LayoutSnapshot) ([]PlacementAction, error) {
	return PlanApplicationPlacementActionsAfter(groups, desired, nil)
}

// PlanApplicationPlacementActionsAfter rotates bounded placement work past the
// previous batch even when Sway explicitly rejected every attempted action.
func PlanApplicationPlacementActionsAfter(
	groups map[ContextID]ApplicationGroup,
	desired LayoutSnapshot,
	after *PlacementAction,
) ([]PlacementAction, error) {
	if err := desired.Validate(); err != nil {
		return nil, fmt.Errorf("validate desired layout: %w", err)
	}
	targets := placementTargets(desired)
	ids := make([]ContextID, 0, len(groups))
	for id := range groups {
		if err := id.Validate(); err != nil {
			return nil, fmt.Errorf("invalid application group context ID: %w", err)
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	actions := make([]PlacementAction, 0)
	for _, id := range ids {
		group := groups[id]
		if group.Ambiguous || group.Anchor == nil || group.AnchorMarked {
			continue
		}
		if group.Anchor.ContainerID <= 0 || !validApplicationWorkspace(group.Anchor.Workspace) {
			return nil, fmt.Errorf("application context %q has an invalid anchor", id)
		}
		if target, exists := targets[id]; exists && target != group.Anchor.Workspace {
			actions = append(actions, PlacementAction{
				Kind: PlacementMoveWorkspace, ContextID: id, ContainerID: group.Anchor.ContainerID, Workspace: target,
			})
		}
		actions = append(actions, PlacementAction{
			Kind: PlacementAddMark, ContextID: id, ContainerID: group.Anchor.ContainerID,
		})
	}
	return boundedPlacementActionsAfter(actions, after), nil
}
