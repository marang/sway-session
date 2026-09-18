package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	sessionstate "github.com/marang/sway-session/internal/session"
)

func TestDiagnosticErrorReporterSerializesAndDeduplicatesConcurrentFailures(t *testing.T) {
	var output bytes.Buffer
	reporter := newDiagnosticErrorReporter(&output, false, "daemon_runtime", "persistent session daemon")
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			reporter.Report(errors.New("same failure"))
		}()
	}
	wait.Wait()
	if count := strings.Count(output.String(), "same failure"); count != 1 {
		t.Fatalf("concurrent duplicate failure count = %d, want 1: %q", count, output.String())
	}
}

func TestDiagnosticErrorReporterIncludesSafeLayoutDegradationDetails(t *testing.T) {
	var output bytes.Buffer
	reporter := newDiagnosticErrorReporter(&output, true, "daemon_runtime", "persistent session daemon")
	reporter.Report(restoreDegradationError{degradation: sessionstate.RestoreDegradation{
		Workspace:       "2",
		RestoreMode:     sessionstate.WorkspaceRestorePlacementOnly,
		ManagedChildren: 2,
		Reason:          "mixed managed and unregistered layout is placement-only",
	}})

	var envelope struct {
		Diagnostics []struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatalf("decode structured layout diagnostic: %v\n%s", err, output.String())
	}
	if len(envelope.Diagnostics) != 1 || envelope.Diagnostics[0].Code != "layout_restore_degraded" {
		t.Fatalf("unexpected layout diagnostic: %+v", envelope.Diagnostics)
	}
	details := envelope.Diagnostics[0].Details
	if details["workspace"] != "2" || details["restore_mode"] != string(sessionstate.WorkspaceRestorePlacementOnly) ||
		details["layout_type"] != "none" || details["degradation_reason"] != "mixed managed and unregistered layout is placement-only" {
		t.Fatalf("unexpected layout diagnostic details: %+v", details)
	}
	if details["managed_child_count"] != float64(2) {
		t.Fatalf("unexpected managed child count: %+v", details["managed_child_count"])
	}
}
