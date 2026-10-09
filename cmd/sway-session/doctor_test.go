package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marang/sway-session/internal/doctor"
)

type fakeDoctorOperations struct {
	report       doctor.Report
	plan         doctor.Plan
	planErr      error
	applyResult  doctor.FixResult
	applyErr     error
	checkCalls   int
	planCalls    []string
	planRequests []doctor.RepairOptions
	applyCalls   int
}

func (fake *fakeDoctorOperations) Check(context.Context) doctor.Report {
	fake.checkCalls++
	return fake.report
}

func (fake *fakeDoctorOperations) Plan(_ context.Context, id string, request doctor.RepairOptions) (doctor.Plan, error) {
	fake.planCalls = append(fake.planCalls, id)
	fake.planRequests = append(fake.planRequests, request)
	if fake.planErr != nil {
		return doctor.Plan{}, fake.planErr
	}
	return fake.plan, nil
}

func (fake *fakeDoctorOperations) Apply(context.Context, doctor.Plan) (doctor.FixResult, error) {
	fake.applyCalls++
	if fake.applyErr != nil {
		return doctor.FixResult{}, fake.applyErr
	}
	return fake.applyResult, nil
}

func doctorTestDeps(t *testing.T, fake *fakeDoctorOperations) dependencies {
	t.Helper()
	deps := testDependencies(t)
	deps.newDoctor = func(options doctor.Options) doctorOperations {
		return fake
	}
	return deps
}

func TestExecuteDoctorStructuredCheckIsReadOnly(t *testing.T) {
	fake := &fakeDoctorOperations{report: doctor.Report{Checks: []doctor.Check{{ID: "runtime.ok", Status: doctor.OK}}}}
	deps := doctorTestDeps(t, fake)
	var stdout, stderr bytes.Buffer
	code := runWith([]string{"--json", "doctor", "--socket", "/run/user/1000/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != exitSuccess || stderr.Len() != 0 || fake.checkCalls != 1 {
		t.Fatalf("structured doctor check code=%d stderr=%q checks=%d", code, stderr.String(), fake.checkCalls)
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Command != "doctor" || result.Doctor == nil || result.Preview {
		t.Fatalf("unexpected doctor result: %+v", result)
	}
	if fake.planCalls != nil || fake.applyCalls != 0 {
		t.Fatalf("read-only check invoked repair operations: plans=%v applies=%d", fake.planCalls, fake.applyCalls)
	}
}

func TestExecuteDoctorNonTTYChecksAndDefaultTTYRunsUI(t *testing.T) {
	t.Run("nonTTY", func(t *testing.T) {
		fake := &fakeDoctorOperations{}
		deps := doctorTestDeps(t, fake)
		deps.doctorInteractive = func(io.Reader, io.Writer) bool { return false }
		result, failure := executeDoctor(context.Background(), nil, strings.NewReader(""), io.Discard, false, "", deps)
		if failure != nil || result.Doctor == nil || fake.checkCalls != 1 {
			t.Fatalf("non-TTY routing result=%+v failure=%+v checks=%d", result, failure, fake.checkCalls)
		}
	})
	t.Run("defaultTTY", func(t *testing.T) {
		fake := &fakeDoctorOperations{}
		deps := doctorTestDeps(t, fake)
		interactiveCalls := 0
		deps.doctorInteractive = func(io.Reader, io.Writer) bool { return true }
		deps.runDoctor = func(context.Context, io.Reader, io.Writer, doctorOperations) error {
			interactiveCalls++
			return nil
		}
		result, failure := executeDoctor(context.Background(), nil, strings.NewReader(""), io.Discard, false, "", deps)
		if failure != nil || interactiveCalls != 1 || result.Doctor != nil || fake.checkCalls != 0 {
			t.Fatalf("TTY routing result=%+v failure=%+v ui=%d checks=%d", result, failure, interactiveCalls, fake.checkCalls)
		}
	})
}

func TestExecuteDoctorFixPreviewAndApplyRecheck(t *testing.T) {
	t.Run("preview", func(t *testing.T) {
		fake := &fakeDoctorOperations{plan: doctor.Plan{ID: "sway.integration", Summary: "preview"}}
		deps := doctorTestDeps(t, fake)
		result, failure := executeDoctor(context.Background(), []string{"--fix", "sway.integration"}, strings.NewReader(""), io.Discard, false, "", deps)
		if failure != nil || result.DoctorPlan == nil || !result.Preview || fake.applyCalls != 0 || fake.checkCalls != 0 {
			t.Fatalf("preview result=%+v failure=%+v plans=%v applies=%d checks=%d", result, failure, fake.planCalls, fake.applyCalls, fake.checkCalls)
		}
	})
	t.Run("applyAndRecheck", func(t *testing.T) {
		fake := &fakeDoctorOperations{plan: doctor.Plan{ID: "sway.integration"}, applyResult: doctor.FixResult{ID: "sway.integration", Message: "applied"}}
		deps := doctorTestDeps(t, fake)
		result, failure := executeDoctor(context.Background(), []string{"--fix", "sway.integration", "--yes"}, strings.NewReader(""), io.Discard, false, "", deps)
		if failure != nil || result.DoctorFix == nil || result.Doctor == nil || result.Preview || fake.applyCalls != 1 || fake.checkCalls != 1 {
			t.Fatalf("apply result=%+v failure=%+v applies=%d checks=%d", result, failure, fake.applyCalls, fake.checkCalls)
		}
	})
}

func appliedDoctorIntegrationFixture(t *testing.T) (doctor.Plan, doctor.FixResult) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(root, []byte("set $mod Mod4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := doctor.New(doctor.Options{SwayConfigPath: root})
	plan, err := service.Plan(t.Context(), "sway.integration", doctor.RepairOptions{AdoptStandard: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Apply(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	return plan, result
}

func TestExecuteDoctorPreservesAppliedIntegrationGuidance(t *testing.T) {
	plan, applied := appliedDoctorIntegrationFixture(t)
	for _, structured := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[structured], func(t *testing.T) {
			fake := &fakeDoctorOperations{
				plan: plan, applyResult: applied,
				report: doctor.Report{Checks: []doctor.Check{{ID: "sway.integration", Status: doctor.OK}}},
			}
			arguments := []string{"doctor", "--fix", "sway.integration", "--yes"}
			if structured {
				arguments = append([]string{"--json"}, arguments...)
			}
			var stdout, stderr bytes.Buffer
			code := runWith(arguments, strings.NewReader(""), &stdout, &stderr, doctorTestDeps(t, fake))
			if code != exitSuccess || stderr.Len() != 0 || len(fake.planCalls) != 1 || fake.applyCalls != 1 || fake.checkCalls != 1 {
				t.Fatalf("repair/recheck code=%d stderr=%q plans=%v applies=%d checks=%d", code, stderr.String(), fake.planCalls, fake.applyCalls, fake.checkCalls)
			}
			if structured {
				var result commandResult
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.DoctorFix == nil || result.DoctorFix.Message != applied.Message || result.Doctor == nil || len(result.Doctor.Checks) != 1 || result.Doctor.Checks[0].Status != doctor.OK {
					t.Fatalf("JSON lost repair guidance or recheck: %+v", result)
				}
			} else if !strings.Contains(stdout.String(), applied.Message+"\n") || !strings.Contains(stdout.String(), "[ok] sway.integration") {
				t.Fatalf("text lost repair guidance or recheck: %q", stdout.String())
			}
		})
	}
}

func TestExecuteDoctorRejectsInvalidFlagsAndReportsPlanErrors(t *testing.T) {
	fake := &fakeDoctorOperations{planErr: errors.New("bad repair")}
	deps := doctorTestDeps(t, fake)
	if _, failure := executeDoctor(context.Background(), []string{"--yes"}, strings.NewReader(""), io.Discard, false, "", deps); failure == nil || !failure.usage {
		t.Fatal("--yes without --fix was not rejected as usage")
	}
	if _, failure := executeDoctor(context.Background(), []string{"--check", "--fix", "sway.integration"}, strings.NewReader(""), io.Discard, false, "", deps); failure == nil || !failure.usage {
		t.Fatal("--check with --fix was not rejected as usage")
	}
	var stderr bytes.Buffer
	code := runWith([]string{"--json", "doctor", "--fix", "sway.integration"}, strings.NewReader(""), io.Discard, &stderr, deps)
	if code != exitOperation {
		t.Fatalf("plan failure exit code=%d stderr=%q", code, stderr.String())
	}
	var envelope struct {
		Diagnostics []struct {
			Code string `json:"code"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &envelope); err != nil || len(envelope.Diagnostics) != 1 || envelope.Diagnostics[0].Code != "doctor_plan" {
		t.Fatalf("unexpected plan error envelope: err=%v output=%q", err, stderr.String())
	}
}

type failingDoctorWriter struct{}

func (failingDoctorWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestWriteDoctorResultHandlesWriterErrorsAndEscapesControls(t *testing.T) {
	if err := writeDoctorResult(failingDoctorWriter{}, commandResult{Message: "result"}); err == nil {
		t.Fatal("writer error was swallowed")
	}
	var output bytes.Buffer
	result := commandResult{Message: "done\x1b[31m", Doctor: &doctor.Report{Checks: []doctor.Check{{ID: "id\n", Status: doctor.OK, Detail: "detail\r", Hint: "hint\x00"}}}}
	if err := writeDoctorResult(&output, result); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(output.String(), "\x00\x1b\r") {
		t.Fatalf("control bytes leaked into output: %q", output.String())
	}
	if !strings.Contains(output.String(), `id `) || !strings.Contains(output.String(), `done`) {
		t.Fatalf("controls were not sanitized as expected: %q", output.String())
	}
}

func TestDoctorRepairSelectionFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--adopt-standard"}, {"--adopt-standard=false"}, {"--check", "--shortcuts", "none"},
		{"--fix", "other", "--shortcuts", "default"}, {"--fix", "other", "--adopt-standard"},
		{"--fix", "sway.integration", "--shortcuts", ""}, {"--fix", "sway.integration", "--shortcuts", "custom"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			fake := &fakeDoctorOperations{}
			_, failure := executeDoctor(t.Context(), args, strings.NewReader(""), io.Discard, false, "", doctorTestDeps(t, fake))
			if failure == nil || !failure.usage || len(fake.planCalls) != 0 || fake.applyCalls != 0 || fake.checkCalls != 0 {
				t.Fatalf("invalid options reached doctor: failure=%+v operations=%+v", failure, fake)
			}
		})
	}
	for _, choice := range []string{"", "none", "default"} {
		t.Run("forward-"+choice, func(t *testing.T) {
			fake := &fakeDoctorOperations{plan: doctor.Plan{ID: "sway.integration"}}
			args := []string{"--fix", "sway.integration", "--adopt-standard"}
			if choice != "" {
				args = append(args, "--shortcuts", choice)
			}
			result, failure := executeDoctor(t.Context(), args, strings.NewReader(""), io.Discard, false, "", doctorTestDeps(t, fake))
			want := doctor.RepairOptions{AdoptStandard: true, Shortcuts: doctor.ShortcutSelection(choice)}
			if failure != nil || !result.Preview || len(fake.planRequests) != 1 || fake.planRequests[0] != want || fake.applyCalls != 0 {
				t.Fatalf("selection forwarding: result=%+v failure=%+v requests=%v", result, failure, fake.planRequests)
			}
		})
	}
}

func TestDoctorSetupAndRuntimeStatusesStayIndependent(t *testing.T) {
	for _, runtimeStatus := range []doctor.Status{doctor.OK, doctor.Error} {
		for _, setupStatus := range []doctor.Status{doctor.OK, doctor.Warning, doctor.Unavailable} {
			t.Run(string(runtimeStatus)+"-"+string(setupStatus), func(t *testing.T) {
				fake := &fakeDoctorOperations{report: doctor.Report{Checks: []doctor.Check{
					{ID: "daemon.runtime", Status: runtimeStatus},
					{ID: "sway.integration", Status: setupStatus, Evidence: []string{"Sources on disk only; effective bindings are not verified."}, AdoptionRequired: true},
				}}}
				var stdout, stderr bytes.Buffer
				code := runWith([]string{"--json", "doctor", "--check"}, strings.NewReader(""), &stdout, &stderr, doctorTestDeps(t, fake))
				wantCode := exitSuccess
				if runtimeStatus == doctor.Error {
					wantCode = exitOperation
				}
				var result commandResult
				if code != wantCode || json.Unmarshal(stdout.Bytes(), &result) != nil || result.Doctor == nil {
					t.Fatalf("independent status/exit: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
				}
				if checks := result.Doctor.Checks; checks[0].Status != runtimeStatus || checks[1].Status != setupStatus || strings.Contains(stdout.String(), "adoption_required") {
					t.Fatalf("public JSON contract changed: %s", stdout.String())
				}
				stdout.Reset()
				if err := writeDoctorResult(&stdout, result); err != nil || !strings.Contains(stdout.String(), "Runtime checks") || !strings.Contains(stdout.String(), "Standard integration on disk") || !strings.Contains(stdout.String(), "effective bindings are not verified") {
					t.Fatalf("text lost distinct findings or evidence: %s error=%v", stdout.String(), err)
				}
			})
		}
	}
}

func TestDoctorRepairBooleanAndEqualsForms(t *testing.T) {
	for _, example := range []struct {
		args []string
		want doctor.RepairOptions
	}{
		{nil, doctor.RepairOptions{}},
		{[]string{"--adopt-standard=false"}, doctor.RepairOptions{}},
		{[]string{"--shortcuts=default"}, doctor.RepairOptions{Shortcuts: doctor.ShortcutsDefault}},
		{[]string{"--adopt-standard=true", "--shortcuts=none"}, doctor.RepairOptions{AdoptStandard: true, Shortcuts: doctor.ShortcutsNone}},
	} {
		fake := &fakeDoctorOperations{plan: doctor.Plan{ID: "sway.integration"}}
		args := append([]string{"--fix=sway.integration"}, example.args...)
		result, failure := executeDoctor(t.Context(), args, strings.NewReader(""), io.Discard, true, "", doctorTestDeps(t, fake))
		if failure != nil || !result.Preview || len(fake.planRequests) != 1 || fake.planRequests[0] != example.want || fake.checkCalls != 0 || fake.applyCalls != 0 {
			t.Fatalf("option forms: args=%v result=%+v failure=%+v requests=%v", args, result, failure, fake.planRequests)
		}
	}
}

func TestDoctorHelpExplainsMigrationAndEvidenceLimits(t *testing.T) {
	var output bytes.Buffer
	writeCommandUsage(&output, "doctor", commandSpecs["doctor"])
	for _, text := range []string{"--adopt-standard", "--shortcuts none|default", "Existing profiles are preserved", "new setup has no shortcuts", "remove previous starts and includes manually", "define $mod before the first include", "source files only", "next login", "reload Sway yourself"} {
		if !strings.Contains(output.String(), text) {
			t.Fatalf("help omitted %q:\n%s", text, output.String())
		}
	}
}

type doctorRejectTextWriter string

func (writer doctorRejectTextWriter) Write(value []byte) (int, error) {
	if strings.Contains(string(value), string(writer)) {
		return 0, errors.New("write failed")
	}
	return len(value), nil
}

func TestDoctorIntegrationEvidenceEscapesControlsAndPropagatesWriterErrors(t *testing.T) {
	result := commandResult{Doctor: &doctor.Report{Checks: []doctor.Check{{ID: "sway.integration", Status: doctor.Unavailable, Evidence: []string{"standard profile: startup-only\x1b[31m\r"}}}}}
	var output bytes.Buffer
	if err := writeDoctorResult(&output, result); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(output.String(), "\x1b\r") || !strings.Contains(output.String(), "runtime checks are separate") {
		t.Fatalf("unsafe or incomplete source evidence: %q", output.String())
	}
	for _, text := range []string{"Standard integration on disk", "standard profile", "Source files only"} {
		if err := writeDoctorResult(doctorRejectTextWriter(text), result); err == nil {
			t.Fatalf("writer failure swallowed at %q", text)
		}
	}
}
