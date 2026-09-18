package main

import (
	"fmt"

	"github.com/marang/sway-session/internal/diagnostic"
	sessionstate "github.com/marang/sway-session/internal/session"
)

type restoreDegradationError struct {
	degradation sessionstate.RestoreDegradation
}

func (err restoreDegradationError) Error() string {
	return fmt.Sprintf(
		"degrade workspace %q restore: %s",
		err.degradation.Workspace,
		err.degradation.Reason,
	)
}

func (err restoreDegradationError) Diagnostic() diagnostic.Diagnostic {
	layout := string(err.degradation.Layout)
	if layout == "" {
		layout = "none"
	}
	return diagnostic.Diagnostic{
		Level:   diagnostic.LevelError,
		Code:    "layout_restore_degraded",
		Message: "Sway workspace restore degraded",
		Hint:    err.Error(),
		Details: map[string]any{
			"workspace":           err.degradation.Workspace,
			"restore_mode":        string(err.degradation.RestoreMode),
			"layout_type":         layout,
			"managed_child_count": err.degradation.ManagedChildren,
			"degradation_reason":  err.degradation.Reason,
		},
	}
}
