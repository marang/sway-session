package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/marang/sway-session/internal/buildmetadata"
	"github.com/marang/sway-session/internal/doctor"
)

func TestVersionAliasesIndependentOfDependencies(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "relative-invalid-config")
	t.Setenv("XDG_STATE_HOME", "relative-invalid-state")
	t.Setenv("XDG_RUNTIME_DIR", "relative-invalid-runtime")
	for _, alias := range []string{"version", "--version"} {
		t.Run(alias, func(t *testing.T) {
			var out, errout bytes.Buffer
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			code := runWithContext(ctx, []string{alias}, strings.NewReader(""), &out, &errout, dependencies{})
			if code != exitSuccess || errout.Len() != 0 || strings.Count(out.String(), "\n") != 1 || !strings.HasPrefix(out.String(), "sway-session ") || !strings.Contains(out.String(), "commit ") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errout.String())
			}
			out.Reset()
			errout.Reset()
			code = runWithContext(ctx, []string{alias, "--json"}, strings.NewReader(""), &out, &errout, dependencies{})
			var envelope struct {
				Version int    `json:"version"`
				Command string `json:"command"`
				Build   struct {
					Version string `json:"product_version"`
					Commit  string `json:"commit"`
				} `json:"build"`
			}
			if code != exitSuccess || json.Unmarshal(out.Bytes(), &envelope) != nil || envelope.Version != 1 || envelope.Command != "version" || envelope.Build.Version == "" || envelope.Build.Commit == "" {
				t.Fatalf("JSON: %d %s %s", code, out.String(), errout.String())
			}
		})
	}
}

func TestVersionRejectsArguments(t *testing.T) {
	for _, args := range [][]string{{"version", "--config", "/missing/config"}, {"version", "extra"}, {"--version", "--bogus"}, {"version", "version"}, {"--version=foo"}, {"version", "--config"}, {"version", "--", "--json"}} {
		var out, errout bytes.Buffer
		if code := runWith(args, strings.NewReader(""), &out, &errout, dependencies{}); code != exitUsage {
			t.Fatalf("%q code=%d stdout=%q stderr=%q", args, code, out.String(), errout.String())
		}
	}
}

func TestVersionDoctorSharesExecutingMetadata(t *testing.T) {
	expected := buildmetadata.Executing(version, commit, modified)
	for _, interactive := range []bool{false, true} {
		called := false
		deps := dependencies{newDoctor: func(options doctor.Options) doctorOperations {
			called = true
			if options.CLIBuild == nil || *options.CLIBuild != expected {
				t.Fatalf("doctor metadata %+v expected %+v", options.CLIBuild, expected)
			}
			return &fakeDoctorOperations{}
		}, doctorInteractive: func(io.Reader, io.Writer) bool { return interactive }, runDoctor: func(context.Context, io.Reader, io.Writer, doctorOperations) error { return nil }}
		if _, failure := executeDoctor(context.Background(), nil, strings.NewReader(""), io.Discard, false, "", deps); failure != nil || !called {
			t.Fatalf("interactive=%v called=%v failure=%+v", interactive, called, failure)
		}
	}
}

func TestDoctorPlainOutputShowsKnownAndUnknownBuilds(t *testing.T) {
	for _, daemon := range []string{
		"Daemon build: version=1.2.3 commit=1234567890123456789012345678901234567890 modified=false",
		"Daemon build: unknown (executable build metadata unreadable or unsupported; older binaries may lack metadata).",
	} {
		for _, arguments := range [][]string{{"doctor"}, {"doctor", "--check"}} {
			cli := "Executing CLI build: version=dev commit=unknown modified=unknown"
			fake := &fakeDoctorOperations{report: doctor.Report{Checks: []doctor.Check{{
				ID: "daemon.binary", Status: doctor.Warning, Detail: "Restart needed.",
				Evidence: []string{cli, daemon}, Hint: "Stop the verified daemon, then start sway-session daemon.",
			}}}}
			deps := doctorTestDeps(t, fake)
			deps.doctorInteractive = func(io.Reader, io.Writer) bool { return false }
			var out, errout bytes.Buffer
			code := runWith(arguments, strings.NewReader(""), &out, &errout, deps)
			if code != exitSuccess || errout.Len() != 0 || !strings.Contains(out.String(), cli) || !strings.Contains(out.String(), daemon) || !strings.Contains(out.String(), "Stop the verified daemon") {
				t.Fatalf("%q code=%d stdout=%q stderr=%q", arguments, code, out.String(), errout.String())
			}
		}
	}
}
