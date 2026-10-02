package session

import (
	"errors"
	"fmt"
	"time"
)

const (
	LifecycleOperationVersion  = 1
	MaxLifecycleOperationItems = 128
	MaxLifecycleOperationPage  = 128
)

type LifecycleOperationKind string

const (
	LifecycleRegister LifecycleOperationKind = "register"
	LifecycleRebind   LifecycleOperationKind = "rebind"
	LifecyclePurge    LifecycleOperationKind = "purge"
)

type LifecycleOperationPhase string

const (
	LifecycleForward  LifecycleOperationPhase = "forward"
	LifecycleRollback LifecycleOperationPhase = "rollback"
)

var ErrLifecycleOperationConflict = errors.New("context or identity is reserved by a pending lifecycle operation")

// ErrLifecyclePending is the CLI-compatible name for the same sentinel.
var ErrLifecyclePending = ErrLifecycleOperationConflict

// LifecycleWindowTarget pins one compositor-lifetime window. HadMark records
// the baseline for rollback; two windows may legitimately share ContextID.
// PID and WindowID retain the observed evidence, including an unavailable PID.
type LifecycleWindowTarget struct {
	ContextID   ContextID           `json:"context_id"`
	ContainerID int64               `json:"container_id"`
	Identity    ApplicationIdentity `json:"identity"`
	PID         int                 `json:"pid"`
	WindowID    *int64              `json:"window_id,omitempty"`
	HadMark     bool                `json:"had_mark"`
	WantMark    bool                `json:"want_mark"`
}

// LifecyclePurgeTarget pins the original Herdr root and named session directory.
// Zero inode means absent; zero birth time means unavailable, so the pin has
// weaker device/inode evidence against inode reuse. Never manufacture birth time.
type LifecyclePurgeTarget struct {
	Root           string `json:"root"`
	RootDevice     uint64 `json:"root_device"`
	RootInode      uint64 `json:"root_inode"`
	RootBirthNS    int64  `json:"root_birth_ns,omitempty"`
	SessionDevice  uint64 `json:"session_device"`
	SessionInode   uint64 `json:"session_inode"`
	SessionBirthNS int64  `json:"session_birth_ns,omitempty"`
	SessionExists  bool   `json:"session_exists"`
}

// LifecycleOperation is a durable, typed intent. Before, After, Targets, Kind,
// CompositorID, and Purge are immutable after Begin. Forward completion installs
// After; rollback completion preserves Before after external compensation.
// Purge removes Before atomically at Begin and cannot roll back.
// No intent expires automatically, and Blocked intents retain reservations.
type LifecycleOperation struct {
	ID           string                  `json:"id"`
	Version      int                     `json:"version"`
	Revision     int64                   `json:"revision"`
	Kind         LifecycleOperationKind  `json:"kind"`
	Phase        LifecycleOperationPhase `json:"phase"`
	Before       []Context               `json:"before"`
	After        []Context               `json:"after"`
	Targets      []LifecycleWindowTarget `json:"targets"`
	CompositorID string                  `json:"compositor_id,omitempty"`
	Purge        *LifecyclePurgeTarget   `json:"purge,omitempty"`
	Attempts     int                     `json:"attempts"`
	NextAttempt  time.Time               `json:"next_attempt"`
	Reason       string                  `json:"reason,omitempty"`
	UpdatedAt    time.Time               `json:"updated_at"`
	Blocked      bool                    `json:"blocked"`
}

// LifecycleOperationHandle is valid only inside WithLifecycleOperationContext.
// The callback holds the registry lock, not a SQL transaction. Callers must
// hold the outer terminal lifecycle execution lock for every execution/retry.
// Operation and Registry are snapshots; mutating them alone writes nothing.
// UpdateContext persists only phase/retry metadata using operation CAS.
// CompleteContext atomically applies a phase-consistent candidate and removes
// the intent/reservations. A missing operation is a conflict, never permission
// to recreate Before after an uncertain commit.
type LifecycleOperationHandle struct {
	Operation        LifecycleOperation
	Registry         Registry
	database         *stateDatabase
	original         LifecycleOperation
	registry         Registry
	root             string
	registryRevision int64
	active           bool
}

func (operation LifecycleOperation) Validate() error {
	if operation.Version != LifecycleOperationVersion {
		return &UnsupportedVersionError{Document: "lifecycle operation", Got: operation.Version, Want: LifecycleOperationVersion}
	}
	if err := ContextID(operation.ID).Validate(); err != nil {
		return fmt.Errorf("lifecycle operation ID: %w", err)
	}
	if operation.Revision < 0 || operation.Attempts < 0 {
		return errors.New("lifecycle operation revision and attempts must be nonnegative")
	}
	if operation.Phase != LifecycleForward && operation.Phase != LifecycleRollback {
		return errors.New("invalid lifecycle operation phase")
	}
	if len(operation.Before) > MaxLifecycleOperationItems || len(operation.After) > MaxLifecycleOperationItems || len(operation.Targets) > 2*MaxLifecycleOperationItems {
		return errors.New("lifecycle operation exceeds its item budget")
	}
	for _, contexts := range [][]Context{operation.Before, operation.After} {
		if contexts == nil {
			contexts = []Context{}
		}
		registry := Registry{Version: ContextsSchemaVersion, Contexts: contexts}
		if err := registry.Validate(); err != nil {
			return fmt.Errorf("lifecycle operation contexts: %w", err)
		}
	}
	if operation.UpdatedAt.IsZero() || operation.UpdatedAt.Location() != time.UTC || !operation.NextAttempt.IsZero() && operation.NextAttempt.Location() != time.UTC {
		return errors.New("lifecycle operation timestamps must be UTC, with a nonzero update time")
	}
	if len(operation.Reason) > 64 {
		return errors.New("lifecycle operation reason exceeds its bound")
	}
	for _, ch := range operation.Reason {
		if ch != '_' && (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') {
			return errors.New("lifecycle operation reason must be a bounded code")
		}
	}
	if operation.Kind == LifecyclePurge {
		return validateLifecyclePurge(operation)
	}
	if operation.Purge != nil {
		return errors.New("application lifecycle operation cannot contain a purge target")
	}
	switch operation.Kind {
	case LifecycleRegister:
		if len(operation.Before) != 0 || len(operation.After) == 0 {
			return errors.New("register requires absent originals and nonempty desired contexts")
		}
	case LifecycleRebind:
		if len(operation.Before) != 1 || len(operation.After) != 1 || operation.Before[0].ID != operation.After[0].ID {
			return errors.New("rebind requires one original and replacement with the same ID")
		}
	default:
		return errors.New("invalid lifecycle operation kind")
	}
	if len(operation.Targets) == 0 {
		return errors.New("application lifecycle operation requires window targets")
	}
	if operation.CompositorID == "" || len(operation.CompositorID) > 256 {
		return errors.New("lifecycle compositor ID must contain 1 to 256 bytes")
	}
	contexts := make(map[ContextID][]Context)
	for _, values := range [][]Context{operation.Before, operation.After} {
		for _, value := range values {
			if value.App == nil || value.Launcher.Kind != LauncherDesktop && value.Launcher.Kind != LauncherFlatpak {
				return errors.New("application lifecycle operation requires application contexts")
			}
			contexts[value.ID] = append(contexts[value.ID], value)
		}
	}
	seen := make(map[int64]struct{})
	covered := make(map[ContextID]bool)
	wanted := 0
	for _, target := range operation.Targets {
		if err := target.ContextID.Validate(); err != nil {
			return err
		}
		if target.ContainerID <= 0 || target.PID < 0 || target.WindowID != nil && (*target.WindowID <= 0 || *target.WindowID > 1<<32-1) {
			return errors.New("invalid lifecycle window target")
		}
		if err := target.Identity.validate(); err != nil {
			return err
		}
		if _, duplicate := seen[target.ContainerID]; duplicate {
			return errors.New("duplicate lifecycle window target")
		}
		seen[target.ContainerID] = struct{}{}
		if target.WantMark {
			wanted++
			matchedDesired := false
			for _, value := range operation.After {
				matchedDesired = matchedDesired || value.ID == target.ContextID && applicationIdentitiesOverlap(value.App.Identity, target.Identity)
			}
			if !matchedDesired {
				return errors.New("desired lifecycle mark does not match its desired context")
			}
		}
		matched := false
		for _, value := range contexts[target.ContextID] {
			matched = matched || applicationIdentitiesOverlap(value.App.Identity, target.Identity)
		}
		if !matched || operation.Kind == LifecycleRegister && !target.WantMark {
			return errors.New("lifecycle window does not match its context or baseline mark")
		}
		covered[target.ContextID] = true
	}
	if operation.Kind == LifecycleRegister && len(operation.Targets) != len(operation.After) || operation.Kind == LifecycleRebind && (len(operation.Targets) > 2 || wanted != 1) {
		return errors.New("lifecycle operation has an invalid forward mark plan")
	}
	for _, value := range operation.After {
		if !covered[value.ID] {
			return errors.New("desired context has no lifecycle window target")
		}
	}
	return nil
}
