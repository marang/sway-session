package session

import (
	"errors"
	"fmt"
	"time"
)

// RestoreWork records bounded operational intent, never application-private data.
type RestoreWork struct {
	Window       bool   `json:"window"`
	Placement    bool   `json:"placement"`
	Layout       bool   `json:"layout"`
	Workspace    string `json:"workspace,omitempty"`
	LayoutDigest string `json:"layout_digest,omitempty"`
}

// RestoreOutcome retains the latest attempt for one source and context.
// Acceptance of a launch is not proof that its window mapped or work completed.
type RestoreOutcome struct {
	ContextID        ContextID   `json:"context_id"`
	AttemptID        string      `json:"attempt_id"`
	Source           string      `json:"source"`
	StartedAt        time.Time   `json:"started_at"`
	UpdatedAt        time.Time   `json:"updated_at"`
	Status           string      `json:"status"`
	Reason           string      `json:"reason"`
	Requested        RestoreWork `json:"requested"`
	LaunchAccepted   bool        `json:"launch_accepted"`
	WindowMapped     bool        `json:"window_mapped"`
	PlacementApplied bool        `json:"placement_applied"`
	LayoutApplied    bool        `json:"layout_applied"`
	OwnerRunID       string      `json:"owner_run_id,omitempty"`
	IdentityDigest   string      `json:"identity_digest,omitempty"`
}

// RestoreOutcomeUpdate contains only mutable progress and ownership metadata.
type RestoreOutcomeUpdate struct {
	LaunchAccepted   bool      `json:"launch_accepted"`
	WindowMapped     bool      `json:"window_mapped"`
	PlacementApplied bool      `json:"placement_applied"`
	LayoutApplied    bool      `json:"layout_applied"`
	Status           string    `json:"status"`
	Reason           string    `json:"reason"`
	UpdatedAt        time.Time `json:"updated_at"`
	OwnerRunID       string    `json:"owner_run_id,omitempty"`
}

// RestoreReport separates the latest automatic run from latest explicit attempts.
type RestoreReport struct {
	AutomaticRunID string `json:"automatic_run_id,omitempty"`
	// The last interrupted automatic run remains identifiable after its rows
	// are replaced. This is one summary, never an outcome history.
	InterruptedRunID string           `json:"interrupted_run_id,omitempty"`
	InterruptedAt    *time.Time       `json:"interrupted_at,omitempty"`
	Outcomes         []RestoreOutcome `json:"outcomes"`
}

func validateRestoreUUID(name, value string) error {
	if err := ContextID(value).Validate(); err != nil {
		return fmt.Errorf("invalid %s UUID: %w", name, err)
	}
	return nil
}

func validateRestoreTime(name string, at time.Time) error {
	if at.IsZero() || at.Location() != time.UTC {
		return fmt.Errorf("%s must be a non-zero canonical UTC timestamp", name)
	}
	return nil
}

func validateRestoreSource(source string) error {
	if source != "automatic" && source != "explicit" {
		return errors.New("restore source must be automatic or explicit")
	}
	return nil
}

func validateRestoreStatus(status string) error {
	switch status {
	case "pending", "completed", "skipped", "failed", "interrupted":
		return nil
	default:
		return errors.New("invalid restore status")
	}
}

func validateRestoreReason(reason string) error {
	switch reason {
	case "restore_requested", "launch_accepted", "window_mapped", "placement_applied", "layout_applied", "restore_complete",
		"policy_archived", "policy_not_desired", "context_missing", "identity_changed", "launch_failed", "mapping_timeout",
		"placement_failed", "layout_failed", "placement_timeout", "layout_timeout", "interrupted", "daemon_restarted",
		"user_cancelled", "awaiting_daemon", "ambiguous_window", "observation_unavailable", "history_unavailable", "restore_deferred":
		return nil
	default:
		return errors.New("restore reason must be an allowlisted code")
	}
}

func (work RestoreWork) Validate() error {
	if err := validateMetadata("restore workspace", work.Workspace); err != nil {
		return err
	}
	if (work.Placement || work.Layout) && work.Workspace == "" {
		return errors.New("requested placement/layout requires a workspace")
	}
	if work.Layout && work.LayoutDigest == "" {
		return errors.New("requested layout requires a layout digest")
	}
	if work.LayoutDigest != "" {
		return validateSHA256("restore layout digest", work.LayoutDigest)
	}
	return nil
}

func (outcome RestoreOutcome) Validate() error {
	if err := outcome.ContextID.Validate(); err != nil {
		return fmt.Errorf("invalid restore context ID: %w", err)
	}
	if err := validateRestoreUUID("attempt", outcome.AttemptID); err != nil {
		return err
	}
	if err := validateRestoreSource(outcome.Source); err != nil {
		return err
	}
	if err := validateRestoreTime("restore started_at", outcome.StartedAt); err != nil {
		return err
	}
	if err := validateRestoreTime("restore updated_at", outcome.UpdatedAt); err != nil {
		return err
	}
	if outcome.UpdatedAt.Before(outcome.StartedAt) {
		return errors.New("restore updated_at precedes started_at")
	}
	if err := validateRestoreStatus(outcome.Status); err != nil {
		return err
	}
	if err := validateRestoreReason(outcome.Reason); err != nil {
		return err
	}
	if err := outcome.Requested.Validate(); err != nil {
		return err
	}
	if outcome.IdentityDigest != "" {
		if err := validateSHA256("restore identity digest", outcome.IdentityDigest); err != nil {
			return err
		}
	}
	if outcome.OwnerRunID != "" {
		if err := validateRestoreUUID("owner run", outcome.OwnerRunID); err != nil {
			return err
		}
	}
	if outcome.PlacementApplied && (!outcome.WindowMapped || !outcome.Requested.Placement) {
		return errors.New("placement proof requires window mapping and requested placement")
	}
	if outcome.LayoutApplied && (!outcome.WindowMapped || !outcome.Requested.Layout || (outcome.Requested.Placement && !outcome.PlacementApplied)) {
		return errors.New("layout proof requires window mapping, requested layout and any requested placement proof")
	}
	if outcome.Status == "completed" && (!outcome.WindowMapped || (outcome.Requested.Placement && !outcome.PlacementApplied) || (outcome.Requested.Layout && !outcome.LayoutApplied)) {
		return errors.New("completed restore requires window mapping and every requested placement/layout proof")
	}
	return nil
}

// Stage derives presentation progress from proofs, without treating a launch
// acknowledgement as window mapping or completion.
func (outcome RestoreOutcome) Stage() string {
	switch outcome.Status {
	case "failed", "interrupted", "skipped":
		return outcome.Status
	}
	switch {
	case outcome.LayoutApplied:
		return "layout_applied"
	case outcome.PlacementApplied:
		return "placement_applied"
	case outcome.WindowMapped:
		return "window_mapped"
	case outcome.LaunchAccepted:
		return "launch_accepted"
	default:
		return "pending"
	}
}

func (update RestoreOutcomeUpdate) Validate() error {
	if err := validateRestoreStatus(update.Status); err != nil {
		return err
	}
	if err := validateRestoreReason(update.Reason); err != nil {
		return err
	}
	if err := validateRestoreTime("restore update time", update.UpdatedAt); err != nil {
		return err
	}
	if update.OwnerRunID != "" {
		return validateRestoreUUID("owner run", update.OwnerRunID)
	}
	return nil
}

func (report RestoreReport) Validate() error {
	if report.AutomaticRunID != "" {
		if err := validateRestoreUUID("automatic run", report.AutomaticRunID); err != nil {
			return err
		}
	}
	if report.InterruptedRunID != "" || report.InterruptedAt != nil {
		if err := validateRestoreUUID("interrupted run", report.InterruptedRunID); err != nil {
			return err
		}
		if report.InterruptedAt == nil {
			return errors.New("interrupted run summary requires a timestamp")
		}
		if err := validateRestoreTime("interrupted run time", *report.InterruptedAt); err != nil {
			return err
		}
	}
	seen := make(map[struct {
		source string
		id     ContextID
	}]struct{}, len(report.Outcomes))
	for _, outcome := range report.Outcomes {
		if err := outcome.Validate(); err != nil {
			return err
		}
		if outcome.Source == "automatic" && report.AutomaticRunID == "" {
			return errors.New("automatic outcomes require an automatic run ID")
		}
		key := struct {
			source string
			id     ContextID
		}{outcome.Source, outcome.ContextID}
		if _, exists := seen[key]; exists {
			return errors.New("duplicate restore source/context")
		}
		seen[key] = struct{}{}
	}
	return nil
}
