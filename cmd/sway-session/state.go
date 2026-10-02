package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	sessionstate "github.com/marang/sway-session/internal/session"
)

func executeState(ctx context.Context, arguments []string, deps dependencies) (commandResult, *commandFailure) {
	if len(arguments) == 0 {
		return commandResult{}, usageFailure("state", "state requires backup or recover")
	}
	subcommand := arguments[0]
	var pathOption string
	switch subcommand {
	case "backup":
		pathOption = "output"
	case "recover":
		pathOption = "from"
	default:
		return commandResult{}, usageFailure("state", "state requires backup or recover")
	}
	flags := newFlagSet("state " + subcommand)
	var path string
	pathSet := false
	flags.Func(pathOption, "clean absolute path to a backup file", func(value string) error {
		if pathSet {
			return fmt.Errorf("--%s may be provided only once", pathOption)
		}
		path, pathSet = value, true
		return nil
	})
	apply := false
	if subcommand == "recover" {
		flags.BoolVar(&apply, "yes", false, "apply the recovery instead of previewing it")
	}
	if err := flags.Parse(arguments[1:]); err != nil {
		return commandResult{}, usageFailure("state", err.Error())
	}
	if flags.NArg() != 0 || path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return commandResult{}, usageFailure("state", fmt.Sprintf("state %s requires --%s PATH with a clean absolute file path and no positional arguments", subcommand, pathOption))
	}
	if deps.stateRoot == nil {
		return commandResult{}, failure("state_path", "resolve state directory", "State directory dependency is unavailable.")
	}
	root, problem := stateRoot(deps)
	if problem != nil {
		return commandResult{}, problem
	}
	if subcommand == "backup" {
		if deps.backupState == nil {
			return commandResult{}, failure("state_backup", "back up session metadata", "State backup dependency is unavailable.")
		}
		backup, err := deps.backupState(ctx, root, path)
		if err != nil {
			if backup.Path != "" {
				return commandResult{Command: "state backup", StateBackup: &backup, Message: "Backup publication could not be confirmed; inspect the destination before retrying."}, stateCommandFailure("state_backup", "back up session metadata", err)
			}
			return commandResult{}, stateCommandFailure("state_backup", "back up session metadata", err)
		}
		return commandResult{Command: "state backup", StateBackup: &backup}, nil
	}
	if deps.recoverState == nil {
		return commandResult{}, failure("state_recovery", "recover session metadata", "State recovery dependency is unavailable.")
	}
	if apply {
		// Keep the same lock as the daemon for the entire replacement operation.
		// Preview must not prepare the runtime directory or create this lock.
		lock, err := acquireSessionDaemonLock()
		if err != nil {
			hint := err.Error()
			if errors.Is(err, errSessionDaemonRunning) {
				hint += ". Stop the daemon yourself before retrying state recover --yes."
			}
			return commandResult{}, failure("state_recovery", "cannot acquire recovery daemon lock", hint)
		}
		defer func() { _ = lock.Close() }()
	}
	recovery, err := deps.recoverState(ctx, root, path, apply)
	if err != nil {
		if recovery.Source != "" || recovery.Database != "" || recovery.Previous != "" || recovery.Applied || recovery.Resumed {
			message := "Recovery did not complete successfully; inspect the diagnostic and retained state before retrying."
			if errors.Is(err, sessionstate.ErrStateRecoveryPending) {
				message = "Recovery did not complete; resume with the same --from PATH --yes command."
			}
			if recovery.Applied {
				message = "Recovery reports the replacement installed, but completion could not be confirmed; inspect the diagnostic before restarting state users."
			}
			return commandResult{Command: "state recover", Preview: !apply, StateRecovery: &recovery, Message: message}, stateCommandFailure("state_recovery", "recover session metadata", err)
		}
		return commandResult{}, stateCommandFailure("state_recovery", "recover session metadata", err)
	}
	return commandResult{Command: "state recover", Preview: !apply, StateRecovery: &recovery}, nil
}

func stateCommandFailure(code, action string, err error) *commandFailure {
	if accessFailure := stateAccessFailure(action, err); accessFailure != nil {
		return accessFailure
	}
	return failure(code, action, err.Error())
}

func writeStateHelp(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "Backup: sway-session [--json] state backup --output PATH")
	_, _ = fmt.Fprintln(writer, "Preview: sway-session [--json] state recover --from PATH")
	_, _ = fmt.Fprintln(writer, "Apply: sway-session [--json] state recover --from PATH --yes")
	_, _ = fmt.Fprintln(writer, "Backups contain sway-session metadata only; Herdr history, application state, and configuration are excluded.")
	_, _ = fmt.Fprintln(writer, "Use a clean absolute file path in a current-owner private 0700 parent directory. Recovery input must be a current-owner regular 0600 file.")
	_, _ = fmt.Fprintln(writer, "Recovery defaults to a read-only preview: no replacement, rollback file, daemon lock, or process/compositor effects. Preview does not prove that apply can acquire exclusive access.")
	_, _ = fmt.Fprintln(writer, "Apply requires explicit --yes, a stopped daemon, a valid XDG_RUNTIME_DIR, and exclusive state-writer access. Busy state is refused; this command never stops or signals a daemon.")
	_, _ = fmt.Fprintln(writer, "Stop all older CLI and broker processes before recovery; mixed-version writers are unsupported.")
	_, _ = fmt.Fprintln(writer, "Apply preserves the replaced state at the reported rollback location. A corrupt previous state is preserved as a raw bundle, not a validated backup.")
	_, _ = fmt.Fprintln(writer, "If recovery is interrupted, repeat the same --from PATH --yes command to resume; ordinary state access refuses pending recovery.")
}

func writeStateResult(writer io.Writer, result commandResult) error {
	if backup := result.StateBackup; backup != nil {
		action := "Backed up sway-session metadata to"
		if result.Message != "" {
			action = result.Message + "\nBackup artifact:"
		}
		_, err := fmt.Fprintf(writer, "%s %q (%d bytes; schema %d; %d contexts).\nHerdr history, application state, and configuration are not included.\n", action, backup.Path, backup.SizeBytes, backup.SchemaVersion, backup.ContextCount)
		return err
	}
	return writeStateRecovery(writer, *result.StateRecovery, result.Preview, result.Message)
}

func writeStateRecovery(writer io.Writer, recovery sessionstate.StateRecoveryResult, preview bool, message string) error {
	if preview && message == "" {
		_, err := fmt.Fprintf(writer, "Recovery preview: %q\nTarget database: %q\nNo state was replaced; no rollback file or daemon lock was created.\nPreview validates the backup only; it does not prove exclusive access or that applications and Herdr sessions can resume.\nStop the daemon and other state users, including all older CLI and broker processes, then rerun with --yes to apply.\n", recovery.Source, recovery.Database)
		return err
	}
	status := "Recovered sway-session metadata."
	if recovery.Resumed {
		status = "Resumed recovery of sway-session metadata."
	}
	if !recovery.Applied {
		status = "Recovery has not completed."
	}
	if message != "" {
		status = message
	}
	previous := ""
	if recovery.Applied {
		previous = "No previous state was present.\n"
	}
	if recovery.Previous != "" {
		previous = fmt.Sprintf("Rollback location: %q", recovery.Previous)
		if recovery.PreviousValid {
			previous += " (validated previous database).\n"
		} else {
			previous += " (raw previous-state bundle; not a validated backup).\n"
		}
	}
	_, err := fmt.Fprintf(writer, "%s\nSource: %q\nTarget database: %q\n%sHerdr history, application state, and configuration were not restored.\n", status, recovery.Source, recovery.Database, previous)
	return err
}
