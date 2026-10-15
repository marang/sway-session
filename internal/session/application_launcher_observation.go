package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ApplicationLauncherState describes only observed launcher validity. It does
// not imply a running application or eligibility for a restore attempt.
type ApplicationLauncherState string

const (
	ApplicationLauncherUnknown            ApplicationLauncherState = "unknown"
	ApplicationLauncherValid              ApplicationLauncherState = "valid"
	ApplicationLauncherMissing            ApplicationLauncherState = "missing"
	ApplicationLauncherReapprovalRequired ApplicationLauncherState = "reapproval_required"
	ApplicationLauncherInvalid            ApplicationLauncherState = "invalid"
)

// ApplicationLauncherObservation contains allowlisted reasons, never a process
// specification, arbitrary error text, or private approval paths.
type ApplicationLauncherObservation struct {
	State  ApplicationLauncherState `json:"state"`
	Reason string                   `json:"reason"`
}

// launcherValidationError preserves the existing launch error text while
// carrying a bounded classification from the shared validation boundary.
type launcherValidationError struct {
	observation ApplicationLauncherObservation
	err         error
}

func (err *launcherValidationError) Error() string { return err.err.Error() }
func (err *launcherValidationError) Unwrap() error { return err.err }

func launcherValidationFailure(state ApplicationLauncherState, reason string, err error) error {
	return &launcherValidationError{ApplicationLauncherObservation{state, reason}, err}
}

func launcherFileFailure(err error, fallback ApplicationLauncherState, reason, missingReason string) *launcherValidationError {
	if errors.Is(err, os.ErrNotExist) {
		return &launcherValidationError{ApplicationLauncherObservation{ApplicationLauncherMissing, missingReason}, err}
	}
	if errors.Is(err, os.ErrPermission) {
		return &launcherValidationError{ApplicationLauncherObservation{ApplicationLauncherUnknown, "permission_denied"}, err}
	}
	return &launcherValidationError{ApplicationLauncherObservation{fallback, reason}, err}
}

func launcherObservationError(ctx context.Context, err error) ApplicationLauncherObservation {
	if ctx != nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	switch {
	case errors.Is(err, context.Canceled):
		return ApplicationLauncherObservation{ApplicationLauncherUnknown, "observation_canceled"}
	case errors.Is(err, context.DeadlineExceeded):
		return ApplicationLauncherObservation{ApplicationLauncherUnknown, "observation_timeout"}
	case errors.Is(err, os.ErrPermission):
		return ApplicationLauncherObservation{ApplicationLauncherUnknown, "permission_denied"}
	}
	var classified *launcherValidationError
	if errors.As(err, &classified) {
		return classified.observation
	}
	return ApplicationLauncherObservation{ApplicationLauncherUnknown, "validation_unavailable"}
}

// ObserveContext reuses the launch trust validator and discards its private
// process specification. It never starts a process or prepares an approval.
// Flatpak probes use the caller's deadline; callers bound and deduplicate work
// across their inventory. ObservationRunner executes only read-only probes.
func (launcher DesktopApplicationLauncher) ObserveContext(ctx context.Context, item Context) ApplicationLauncherObservation {
	if ctx == nil {
		return ApplicationLauncherObservation{ApplicationLauncherUnknown, "observation_context_missing"}
	}
	if _, err := launcher.SpecContext(ctx, item); err != nil {
		return launcherObservationError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return launcherObservationError(ctx, err)
	}
	command := launcher.GIO
	if item.Launcher.Kind == LauncherFlatpak {
		command = launcher.Flatpak
	}
	resolve := launcher.ObservationResolveSystem
	if resolve == nil {
		resolve = ResolveRootOwnedSystemExecutable
	}
	resolved, err := resolve(filepath.Base(command))
	if err != nil {
		result := launcherObservationError(ctx, err)
		if result.Reason == "validation_unavailable" {
			result.Reason = "launcher_command_unavailable"
		}
		return result
	}
	if err := ctx.Err(); err != nil {
		return launcherObservationError(ctx, err)
	}
	if resolved != command {
		return ApplicationLauncherObservation{ApplicationLauncherInvalid, "launcher_command_invalid"}
	}
	if item.Launcher.Kind == LauncherFlatpak {
		return launcher.observeFlatpakContext(ctx, item.Launcher)
	}
	return ApplicationLauncherObservation{ApplicationLauncherValid, "launcher_validated"}
}

func (launcher DesktopApplicationLauncher) observeFlatpakContext(parent context.Context, item Launcher) ApplicationLauncherObservation {
	if launcher.ObservationRunner == nil {
		return ApplicationLauncherObservation{ApplicationLauncherUnknown, "flatpak_probe_unavailable"}
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if err := VerifyFlatpakInstallationContext(ctx, launcher.Flatpak, item, launcher.ObservationRunner); err == nil {
		if err := ctx.Err(); err != nil {
			return launcherObservationError(ctx, err)
		}
		return ApplicationLauncherObservation{ApplicationLauncherValid, "flatpak_installed"}
	} else if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) {
		result := launcherObservationError(ctx, err)
		if result.Reason == "validation_unavailable" {
			result.Reason = "flatpak_probe_unavailable"
		}
		return result
	}
	// An info failure (including exit 1) does not prove absence. Only a
	// successful, strictly parsed application-ID listing of this installation
	// can establish that the exact saved ID is absent. Diagnostics are ignored.
	installation := "--system"
	if item.FlatpakInstallation == FlatpakUser {
		installation = "--user"
	}
	output, err := launcher.ObservationRunner.CombinedOutput(ctx, launcher.Flatpak, "list", "--app", installation, "--columns=application")
	if err != nil || ctx.Err() != nil {
		result := launcherObservationError(ctx, err)
		if result.Reason == "validation_unavailable" {
			result.Reason = "flatpak_probe_unavailable"
		}
		return result
	}
	if len(output) >= maxHerdrOutputSize {
		return ApplicationLauncherObservation{ApplicationLauncherUnknown, "flatpak_probe_unavailable"}
	}
	found := false
	for _, line := range strings.Split(strings.TrimSuffix(string(output), "\n"), "\n") {
		if line == "" && len(output) == 0 {
			continue
		}
		if !validFlatpakID(line) {
			return ApplicationLauncherObservation{ApplicationLauncherUnknown, "flatpak_probe_unavailable"}
		}
		if line == item.FlatpakID {
			found = true
		}
	}
	if err := ctx.Err(); err != nil {
		return launcherObservationError(ctx, err)
	}
	if found {
		return ApplicationLauncherObservation{ApplicationLauncherUnknown, "flatpak_probe_unavailable"}
	}
	return ApplicationLauncherObservation{ApplicationLauncherMissing, "flatpak_not_installed"}
}
