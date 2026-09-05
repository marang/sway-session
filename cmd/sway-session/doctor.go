package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/marang/sway-session/internal/doctor"
	"golang.org/x/term"
)

type doctorOperations interface {
	Check(context.Context) doctor.Report
	Plan(context.Context, string) (doctor.Plan, error)
	Apply(context.Context, doctor.Plan) (doctor.FixResult, error)
}

type doctorRunner func(context.Context, io.Reader, io.Writer, doctorOperations) error

func doctorTerminals(stdin io.Reader, stdout io.Writer) bool {
	in, inputOK := stdin.(*os.File)
	out, outputOK := stdout.(*os.File)
	return inputOK && outputOK && term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

func executeDoctor(ctx context.Context, arguments []string, stdin io.Reader, stdout io.Writer, structured bool, configPath string, deps dependencies) (commandResult, *commandFailure) {
	flags := newFlagSet("doctor")
	checkOnly := flags.Bool("check", false, "print a read-only report")
	fixID := flags.String("fix", "", "preview a known repair ID")
	yes := flags.Bool("yes", false, "apply the selected repair")
	socket := flags.String("socket", "", "Sway IPC socket")
	swayConfig := flags.String("sway-config", "", "explicit Sway config file")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return commandResult{}, usageFailure("doctor", "doctor accepts only named options; see sway-session help doctor")
	}
	emptyFix := false
	flags.Visit(func(option *flag.Flag) {
		if option.Name == "fix" && *fixID == "" {
			emptyFix = true
		}
	})
	if emptyFix {
		return commandResult{}, usageFailure("doctor", "--fix requires a nonempty repair ID")
	}
	if *checkOnly && *fixID != "" || *yes && *fixID == "" {
		return commandResult{}, usageFailure("doctor", "--check cannot be combined with --fix; --yes requires --fix ID")
	}
	for _, path := range []string{configPath, *socket, *swayConfig} {
		if path != "" && (!filepath.IsAbs(path) || filepath.Clean(path) != path) {
			return commandResult{}, usageFailure("doctor", "config and socket paths must be clean absolute paths")
		}
	}
	if deps.newDoctor == nil {
		return commandResult{}, failure("doctor", "inspect setup", "Doctor dependency is unavailable.")
	}
	executable, err := os.Executable()
	if err != nil {
		return commandResult{}, failure("doctor", "identify current executable", err.Error())
	}
	operations := deps.newDoctor(doctor.Options{ConfigPath: configPath, SwayConfigPath: *swayConfig, Socket: *socket, Executable: executable})
	if operations == nil {
		return commandResult{}, failure("doctor", "inspect setup", "Doctor dependency is unavailable.")
	}
	result := commandResult{Command: "doctor"}
	if *fixID != "" {
		plan, err := operations.Plan(ctx, *fixID)
		if err != nil {
			return result, failure("doctor_plan", "prepare configuration repair", err.Error())
		}
		result.DoctorPlan = &plan
		result.Preview = !*yes
		result.Message = "Repair preview only; rerun with --yes to apply."
		if !*yes {
			return result, nil
		}
		fixed, err := operations.Apply(ctx, plan)
		if err != nil {
			result.Message = "Repair did not complete; inspect the diagnostic before retrying."
			return result, failure("doctor_fix", "apply configuration repair", err.Error())
		}
		result.DoctorFix = &fixed
	} else if !structured && !*checkOnly && deps.doctorInteractive != nil && deps.doctorInteractive(stdin, stdout) {
		if deps.runDoctor == nil {
			return result, failure("doctor_tui", "open setup doctor", "Interactive doctor dependency is unavailable.")
		}
		if err := deps.runDoctor(ctx, stdin, stdout, operations); err != nil {
			return result, failure("doctor_tui", "open setup doctor", err.Error())
		}
		return result, nil
	}
	report := operations.Check(ctx)
	result.Doctor = &report
	result.Message = "Setup checks completed."
	if report.HasErrors() {
		return result, failure("doctor_checks", "some setup checks failed", "Review the check IDs and hints; only explicit repairs modify configuration.")
	}
	if err := ctx.Err(); err != nil {
		return result, failure("doctor_cancelled", "setup inspection cancelled", err.Error())
	}
	return result, nil
}

// Render external strings as single-line text; a config path or diagnostic must
// not inject terminal control sequences into either the report or the TUI.
func doctorText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, ansi.Strip(value))
}

func writeDoctorResult(writer io.Writer, result commandResult) error {
	if result.DoctorPlan != nil {
		if _, err := fmt.Fprintf(writer, "%s: %s\n", doctorText(result.DoctorPlan.ID), doctorText(result.DoctorPlan.Summary)); err != nil {
			return err
		}
		for _, change := range result.DoctorPlan.Changes {
			if _, err := fmt.Fprintf(writer, "  %s\n", doctorText(change.Path)); err != nil {
				return err
			}
			for _, line := range strings.Split(change.Preview, "\n") {
				if _, err := fmt.Fprintf(writer, "    %s\n", doctorText(line)); err != nil {
					return err
				}
			}
		}
	}
	if result.DoctorFix != nil {
		if _, err := fmt.Fprintln(writer, doctorText(result.DoctorFix.Message)); err != nil {
			return err
		}
		for _, backup := range result.DoctorFix.Backups {
			if _, err := fmt.Fprintf(writer, "Backup: %s\n", doctorText(backup)); err != nil {
				return err
			}
		}
	}
	if result.Doctor != nil {
		for _, check := range result.Doctor.Checks {
			if _, err := fmt.Fprintf(writer, "[%s] %s — %s\n", check.Status, doctorText(check.ID), doctorText(check.Detail)); err != nil {
				return err
			}
			if check.Hint != "" {
				if _, err := fmt.Fprintf(writer, "  %s\n", doctorText(check.Hint)); err != nil {
					return err
				}
			}
		}
	}
	_, err := fmt.Fprintln(writer, doctorText(result.Message))
	return err
}
