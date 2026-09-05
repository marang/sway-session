package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/marang/sway-session/internal/doctor"
)

type fakeDoctorOperations struct {
	report      doctor.Report
	plan        doctor.Plan
	planErr     error
	applyResult doctor.FixResult
	applyErr    error
	checkCalls  int
	planCalls   []string
	applyCalls  int
}

func (fake *fakeDoctorOperations) Check(context.Context) doctor.Report {
	fake.checkCalls++
	return fake.report
}

func (fake *fakeDoctorOperations) Plan(_ context.Context, id string) (doctor.Plan, error) {
	fake.planCalls = append(fake.planCalls, id)
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
