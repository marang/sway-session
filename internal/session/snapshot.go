package session

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// SemanticSnapshotHash ignores workspace and placement-only list ordering,
// while preserving child and floating order because those affect layout.
func SemanticSnapshotHash(snapshot LayoutSnapshot) ([sha256.Size]byte, error) {
	canonical, err := canonicalSnapshot(snapshot)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("encode semantic layout snapshot: %w", err)
	}
	return sha256.Sum256(encoded), nil
}

// StartupCaptureReady reports whether every still-registered context from the
// previous snapshot is visible. Until then, an empty or partial startup tree
// must not replace the last complete state.
func StartupCaptureReady(previous LayoutSnapshot, captured LayoutSnapshot, registry Registry) (bool, error) {
	if err := previous.Validate(); err != nil {
		return false, fmt.Errorf("validate previous layout: %w", err)
	}
	if err := captured.Validate(); err != nil {
		return false, fmt.Errorf("validate captured layout: %w", err)
	}
	if err := registry.Validate(); err != nil {
		return false, fmt.Errorf("validate context registry: %w", err)
	}
	active := activeContextIDs(registry)
	visible := snapshotContextIDs(captured)
	for id := range snapshotContextIDs(previous) {
		if _, expected := active[id]; !expected {
			continue
		}
		if _, exists := visible[id]; !exists {
			return false, nil
		}
	}
	return true, nil
}

// PreserveMissingPlacements retains the last exact workspace while an expected
// registered leaf is merely absent. If visible siblings moved elsewhere or
// new contexts joined that workspace, it safely degrades the divergent old
// workspace to placement-only state.
func PreserveMissingPlacements(previous LayoutSnapshot, captured LayoutSnapshot, registry Registry) (LayoutSnapshot, error) {
	if err := previous.Validate(); err != nil {
		return LayoutSnapshot{}, fmt.Errorf("validate previous layout: %w", err)
	}
	if err := captured.Validate(); err != nil {
		return LayoutSnapshot{}, fmt.Errorf("validate captured layout: %w", err)
	}
	if err := registry.Validate(); err != nil {
		return LayoutSnapshot{}, fmt.Errorf("validate context registry: %w", err)
	}
	result, err := canonicalSnapshot(captured)
	if err != nil {
		return LayoutSnapshot{}, err
	}
	registered := registeredContextIDs(registry)
	visible := snapshotContextIDs(captured)
	visibleTargets := placementTargets(captured)
	resultByName := make(map[string]int, len(result.Workspaces))
	for index := range result.Workspaces {
		resultByName[result.Workspaces[index].Name] = index
	}

	for _, previousWorkspace := range previous.Workspaces {
		previousIDs := workspaceContextIDs(previousWorkspace)
		missing := make([]ContextID, 0)
		allStillRegistered := true
		for _, id := range previousIDs {
			if _, keep := registered[id]; !keep {
				allStillRegistered = false
				continue
			}
			if _, exists := visible[id]; !exists {
				missing = append(missing, id)
			}
		}
		if len(missing) == 0 {
			continue
		}

		preserveExact := allStillRegistered
		previousSet := make(map[ContextID]struct{}, len(previousIDs))
		for _, id := range previousIDs {
			previousSet[id] = struct{}{}
			if workspace, exists := visibleTargets[id]; exists && workspace != previousWorkspace.Name {
				preserveExact = false
			}
		}
		if index, exists := resultByName[previousWorkspace.Name]; exists {
			if result.Workspaces[index].RestoreMode != WorkspaceRestoreLayout {
				preserveExact = false
			}
			for _, id := range workspaceContextIDs(result.Workspaces[index]) {
				if _, existedBefore := previousSet[id]; !existedBefore {
					preserveExact = false
				}
			}
		}
		if preserveExact {
			if index, exists := resultByName[previousWorkspace.Name]; exists {
				result.Workspaces[index] = previousWorkspace
			} else {
				result.Workspaces = append(result.Workspaces, previousWorkspace)
				resultByName[previousWorkspace.Name] = len(result.Workspaces) - 1
			}
			continue
		}

		placement := make([]ContextID, 0)
		if index, exists := resultByName[previousWorkspace.Name]; exists {
			placement = append(placement, workspaceContextIDs(result.Workspaces[index])...)
		}
		placement = appendUniqueContextIDs(placement, missing...)
		sort.Slice(placement, func(left, right int) bool {
			return placement[left] < placement[right]
		})
		replacement := WorkspaceLayout{
			Name:              previousWorkspace.Name,
			RestoreMode:       WorkspaceRestorePlacementOnly,
			PlacementContexts: placement,
		}
		if index, exists := resultByName[previousWorkspace.Name]; exists {
			result.Workspaces[index] = replacement
		} else {
			result.Workspaces = append(result.Workspaces, replacement)
			resultByName[previousWorkspace.Name] = len(result.Workspaces) - 1
		}
	}
	sort.Slice(result.Workspaces, func(left, right int) bool {
		return result.Workspaces[left].Name < result.Workspaces[right].Name
	})
	if err := result.Validate(); err != nil {
		return LayoutSnapshot{}, fmt.Errorf("validate preserved layout: %w", err)
	}
	return result, nil
}

// PreserveFailedRestoreWorkspaces keeps the persisted version of a workspace
// after a best-effort restore failure, provided the same managed contexts are
// still on that workspace. This prevents the immediate degraded result from
// replacing the retryable last-good tree. A changed context set is treated as
// a later user/lifecycle decision and is not quarantined.
func PreserveFailedRestoreWorkspaces(
	previous LayoutSnapshot,
	candidate LayoutSnapshot,
	failed map[string]struct{},
) (LayoutSnapshot, map[string]struct{}, error) {
	if err := previous.Validate(); err != nil {
		return LayoutSnapshot{}, nil, fmt.Errorf("validate previous layout: %w", err)
	}
	result, err := canonicalSnapshot(candidate)
	if err != nil {
		return LayoutSnapshot{}, nil, err
	}
	previousByName := make(map[string]WorkspaceLayout, len(previous.Workspaces))
	for _, workspace := range previous.Workspaces {
		previousByName[workspace.Name] = workspace
	}
	preserved := make(map[string]struct{})
	for index := range result.Workspaces {
		name := result.Workspaces[index].Name
		if _, protect := failed[name]; !protect {
			continue
		}
		lastGood, exists := previousByName[name]
		if !exists || !sameContextSet(workspaceContextIDs(lastGood), workspaceContextIDs(result.Workspaces[index])) {
			continue
		}
		result.Workspaces[index] = lastGood
		preserved[name] = struct{}{}
	}
	result, err = canonicalSnapshot(result)
	if err != nil {
		return LayoutSnapshot{}, nil, fmt.Errorf("validate failure-preserved layout: %w", err)
	}
	return result, preserved, nil
}

// SnapshotDebouncer retains an immutable candidate until it has remained the
// latest semantic state for the configured quiet period.
type SnapshotDebouncer struct {
	delay         time.Duration
	persistedHash [sha256.Size]byte
	pending       *LayoutSnapshot
	pendingHash   [sha256.Size]byte
	deadline      time.Time
}

func NewSnapshotDebouncer(previous LayoutSnapshot, delay time.Duration) (*SnapshotDebouncer, error) {
	if delay <= 0 {
		return nil, errors.New("snapshot debounce delay must be positive")
	}
	hash, err := SemanticSnapshotHash(previous)
	if err != nil {
		return nil, err
	}
	return &SnapshotDebouncer{delay: delay, persistedHash: hash}, nil
}

// Observe replaces the pending candidate and restarts the quiet period. It
// returns whether a write remains scheduled.
func (debouncer *SnapshotDebouncer) Observe(snapshot LayoutSnapshot, now time.Time) (bool, error) {
	if debouncer == nil {
		return false, errors.New("snapshot debouncer is nil")
	}
	canonical, err := canonicalSnapshot(snapshot)
	if err != nil {
		return false, err
	}
	hash, err := SemanticSnapshotHash(canonical)
	if err != nil {
		return false, err
	}
	if hash == debouncer.persistedHash {
		debouncer.pending = nil
		debouncer.deadline = time.Time{}
		return false, nil
	}
	debouncer.pending = &canonical
	debouncer.pendingHash = hash
	debouncer.deadline = now.Add(debouncer.delay)
	return true, nil
}

func (debouncer *SnapshotDebouncer) Deadline() (time.Time, bool) {
	if debouncer == nil || debouncer.pending == nil {
		return time.Time{}, false
	}
	return debouncer.deadline, true
}

func (debouncer *SnapshotDebouncer) Due(now time.Time) (LayoutSnapshot, bool) {
	if debouncer == nil || debouncer.pending == nil || now.Before(debouncer.deadline) {
		return LayoutSnapshot{}, false
	}
	return *debouncer.pending, true
}

func (debouncer *SnapshotDebouncer) MarkPersisted(snapshot LayoutSnapshot) error {
	if debouncer == nil {
		return errors.New("snapshot debouncer is nil")
	}
	hash, err := SemanticSnapshotHash(snapshot)
	if err != nil {
		return err
	}
	debouncer.persistedHash = hash
	if debouncer.pending != nil && debouncer.pendingHash == hash {
		debouncer.pending = nil
		debouncer.deadline = time.Time{}
	}
	return nil
}

func (debouncer *SnapshotDebouncer) Postpone(now time.Time) {
	if debouncer != nil && debouncer.pending != nil {
		debouncer.deadline = now.Add(debouncer.delay)
	}
}

func (debouncer *SnapshotDebouncer) Cancel() {
	if debouncer == nil {
		return
	}
	debouncer.pending = nil
	debouncer.deadline = time.Time{}
}

func canonicalSnapshot(snapshot LayoutSnapshot) (LayoutSnapshot, error) {
	if err := snapshot.Validate(); err != nil {
		return LayoutSnapshot{}, fmt.Errorf("validate layout snapshot: %w", err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return LayoutSnapshot{}, fmt.Errorf("encode layout snapshot copy: %w", err)
	}
	var canonical LayoutSnapshot
	if err := json.Unmarshal(encoded, &canonical); err != nil {
		return LayoutSnapshot{}, fmt.Errorf("decode layout snapshot copy: %w", err)
	}
	for index := range canonical.Workspaces {
		if canonical.Workspaces[index].RestoreMode == WorkspaceRestorePlacementOnly {
			sort.Slice(canonical.Workspaces[index].PlacementContexts, func(left, right int) bool {
				return canonical.Workspaces[index].PlacementContexts[left] < canonical.Workspaces[index].PlacementContexts[right]
			})
		}
	}
	sort.Slice(canonical.Workspaces, func(left, right int) bool {
		return canonical.Workspaces[left].Name < canonical.Workspaces[right].Name
	})
	return canonical, nil
}

func snapshotContextIDs(snapshot LayoutSnapshot) map[ContextID]struct{} {
	ids := make(map[ContextID]struct{})
	for _, workspace := range snapshot.Workspaces {
		for _, id := range workspaceContextIDs(workspace) {
			ids[id] = struct{}{}
		}
	}
	return ids
}

func activeContextIDs(registry Registry) map[ContextID]struct{} {
	active := make(map[ContextID]struct{})
	for _, context := range registry.Contexts {
		if context.State == ContextActive {
			active[context.ID] = struct{}{}
		}
	}
	return active
}

func workspaceContextIDs(workspace WorkspaceLayout) []ContextID {
	if workspace.RestoreMode == WorkspaceRestorePlacementOnly {
		return append([]ContextID(nil), workspace.PlacementContexts...)
	}
	ids := make([]ContextID, 0)
	if workspace.Tiling != nil {
		collectLayoutContextIDs(*workspace.Tiling, func(id ContextID) {
			ids = append(ids, id)
		})
	}
	for _, floating := range workspace.Floating {
		collectLayoutContextIDs(floating, func(id ContextID) {
			ids = append(ids, id)
		})
	}
	return ids
}

func appendUniqueContextIDs(existing []ContextID, additional ...ContextID) []ContextID {
	seen := make(map[ContextID]struct{}, len(existing)+len(additional))
	result := make([]ContextID, 0, len(existing)+len(additional))
	for _, id := range append(existing, additional...) {
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}
