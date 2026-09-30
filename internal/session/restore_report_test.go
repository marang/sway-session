package session

import (
	"strings"
	"testing"
	"time"
)

const restoreTestRun = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
const restoreTestOtherRun = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
const restoreTestAttempt = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
const restoreTestOtherAttempt = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"

func restoreTestOutcome(source string, id ContextID) RestoreOutcome {
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	return RestoreOutcome{ContextID: id, AttemptID: restoreTestAttempt, Source: source, StartedAt: now, UpdatedAt: now,
		Status: "pending", Reason: "restore_requested", Requested: RestoreWork{Window: true, Workspace: "98"}, IdentityDigest: strings.Repeat("a", 64)}
}

func TestRestoreReportOutcomeValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RestoreOutcome)
	}{
		{"invalid context", func(o *RestoreOutcome) { o.ContextID = "not-uuid" }},
		{"invalid attempt", func(o *RestoreOutcome) { o.AttemptID = "secret" }},
		{"invalid source", func(o *RestoreOutcome) { o.Source = "provider" }},
		{"zero start", func(o *RestoreOutcome) { o.StartedAt = time.Time{} }},
		{"noncanonical UTC", func(o *RestoreOutcome) { o.UpdatedAt = o.UpdatedAt.In(time.FixedZone("UTC", 0)) }},
		{"non UTC", func(o *RestoreOutcome) { o.StartedAt = o.StartedAt.In(time.FixedZone("offset", 3600)) }},
		{"zero update", func(o *RestoreOutcome) { o.UpdatedAt = time.Time{} }},
		{"time reversal", func(o *RestoreOutcome) { o.UpdatedAt = o.StartedAt.Add(-time.Second) }},
		{"invalid status", func(o *RestoreOutcome) { o.Status = "running" }},
		{"empty reason", func(o *RestoreOutcome) { o.Reason = "" }},
		{"private reason", func(o *RestoreOutcome) { o.Reason = "launch failed: SECRET=value https://private.example/path" }},
		{"multiline reason", func(o *RestoreOutcome) { o.Reason = "restore_requested\nsecret" }},
		{"oversized reason", func(o *RestoreOutcome) { o.Reason = strings.Repeat("x", 1000) }},
		{"private owner", func(o *RestoreOutcome) { o.OwnerRunID = "secret-owner" }},
		{"invalid digest", func(o *RestoreOutcome) { o.IdentityDigest = "private launcher output" }},
		{"uppercase digest", func(o *RestoreOutcome) { o.IdentityDigest = strings.Repeat("A", 64) }},
		{"invalid layout digest", func(o *RestoreOutcome) { o.Requested.LayoutDigest = "bad" }},
		{"workspace newline", func(o *RestoreOutcome) { o.Requested.Workspace = "98\ncommand" }},
		{"workspace bound", func(o *RestoreOutcome) { o.Requested.Workspace = strings.Repeat("x", 257) }},
		{"placement workspace missing", func(o *RestoreOutcome) { o.Requested.Placement = true; o.Requested.Workspace = "" }},
		{"layout workspace missing", func(o *RestoreOutcome) {
			o.Requested.Layout = true
			o.Requested.LayoutDigest = strings.Repeat("b", 64)
			o.Requested.Workspace = ""
		}},
		{"layout digest missing", func(o *RestoreOutcome) { o.Requested.Layout = true }},
		{"unrequested placement proof", func(o *RestoreOutcome) { o.PlacementApplied = true; o.WindowMapped = true }},
		{"unmapped placement proof", func(o *RestoreOutcome) { o.Requested.Placement = true; o.PlacementApplied = true }},
		{"unrequested layout proof", func(o *RestoreOutcome) { o.LayoutApplied = true; o.WindowMapped = true }},
		{"unmapped layout proof", func(o *RestoreOutcome) {
			o.Requested.Layout = true
			o.Requested.LayoutDigest = strings.Repeat("b", 64)
			o.LayoutApplied = true
		}},
		{"layout without requested placement proof", func(o *RestoreOutcome) {
			o.Requested.Layout = true
			o.Requested.Placement = true
			o.Requested.LayoutDigest = strings.Repeat("b", 64)
			o.LayoutApplied = true
			o.WindowMapped = true
		}},
		{"launch alone completed", func(o *RestoreOutcome) { o.Status = "completed"; o.LaunchAccepted = true }},
		{"placement unproven", func(o *RestoreOutcome) { o.Status = "completed"; o.WindowMapped = true; o.Requested.Placement = true }},
		{"layout unproven", func(o *RestoreOutcome) { o.Status = "completed"; o.WindowMapped = true; o.Requested.Layout = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := restoreTestOutcome("explicit", testContextID)
			tt.mutate(&o)
			if err := o.Validate(); err == nil {
				t.Fatal("invalid outcome accepted")
			}
		})
	}
	mapped := restoreTestOutcome("explicit", testContextID)
	mapped.Status, mapped.Reason, mapped.WindowMapped = "completed", "restore_complete", true
	if err := mapped.Validate(); err != nil {
		t.Fatalf("existing mapped window cannot complete: %v", err)
	}
	for _, reason := range []string{"restore_deferred", "awaiting_daemon", "history_unavailable"} {
		o := restoreTestOutcome("explicit", testContextID)
		o.Reason = reason
		if err := o.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRestoreReportInterruptionSummaryValidation(t *testing.T) {
	now := restoreTestOutcome("automatic", testContextID).StartedAt
	for _, report := range []RestoreReport{
		{InterruptedRunID: restoreTestRun},
		{InterruptedAt: &now},
		{InterruptedRunID: "not-uuid", InterruptedAt: &now},
		{InterruptedRunID: restoreTestRun, InterruptedAt: new(time.Time)},
	} {
		if err := report.Validate(); err == nil {
			t.Fatalf("invalid summary accepted: %+v", report)
		}
	}
	if err := (RestoreReport{AutomaticRunID: restoreTestOtherRun, InterruptedRunID: restoreTestRun, InterruptedAt: &now}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreReportStage(t *testing.T) {
	tests := []struct {
		status                            string
		launch, mapped, placement, layout bool
		want                              string
	}{
		{status: "pending", want: "pending"},
		{status: "pending", launch: true, want: "launch_accepted"},
		{status: "pending", launch: true, mapped: true, want: "window_mapped"},
		{status: "pending", mapped: true, placement: true, want: "placement_applied"},
		{status: "completed", mapped: true, placement: true, layout: true, want: "layout_applied"},
		{status: "failed", layout: true, want: "failed"},
		{status: "interrupted", mapped: true, want: "interrupted"},
		{status: "skipped", launch: true, want: "skipped"},
	}
	for _, tt := range tests {
		o := RestoreOutcome{Status: tt.status, LaunchAccepted: tt.launch, WindowMapped: tt.mapped, PlacementApplied: tt.placement, LayoutApplied: tt.layout}
		if got := o.Stage(); got != tt.want {
			t.Fatalf("stage(%+v) = %s, want %s", o, got, tt.want)
		}
	}
}
