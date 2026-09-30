package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	sessionstate "github.com/marang/sway-session/internal/session"
)

type terminalManageRestoreTestOperations struct {
	terminalManageActivityTestOperations
	retried  []sessionstate.ContextID
	sockets  []string
	retryErr error
}

func (operations *terminalManageRestoreTestOperations) RetryRestore(_ context.Context, id sessionstate.ContextID, socket string) error {
	operations.retried = append(operations.retried, id)
	operations.sockets = append(operations.sockets, socket)
	return operations.retryErr
}

func terminalManageRestoreOutcomeFixture() (terminalManageModel, *terminalManageRestoreTestOperations, terminalInventoryResult) {
	item := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Work", sessionstate.ContextActive)
	item.RestorePolicy = sessionstate.RestorePolicyDecision{Eligible: true, Reason: "active_terminal"}
	item.RestoreOutcome = &sessionstate.RestoreOutcome{
		ContextID: item.ContextID, AttemptID: "22222222-2222-4222-8222-222222222222", Source: "explicit",
		Status: "failed", Reason: "layout_failed", UpdatedAt: time.Date(2026, time.October, 1, 12, 34, 0, 0, time.UTC),
		Requested: sessionstate.RestoreWork{Window: true, Layout: true, Workspace: "98", LayoutDigest: strings.Repeat("0", 64)}, LaunchAccepted: true, WindowMapped: true,
	}
	ops := &terminalManageRestoreTestOperations{}
	ops.snapshots = [][]terminalInventoryResult{{item}}
	ops.observe = func(context.Context, []terminalInventoryResult) map[sessionstate.ContextID]sessionstate.TerminalSessionObservation {
		return map[sessionstate.ContextID]sessionstate.TerminalSessionObservation{item.ContextID: {SessionState: "running", AgentState: "detected"}}
	}
	model := newTerminalManageModel(ops)
	model.noColor = true
	model.width, model.height = 80, 24
	model.socket = "/run/test/sway.sock"
	return model, ops, item
}

func TestTerminalManageRestoreOutcomeProofDoesNotImplyCompletion(t *testing.T) {
	_, _, item := terminalManageRestoreOutcomeFixture()
	for _, test := range []struct {
		status                            string
		launch, mapped, placement, layout bool
		stage                             string
	}{
		{"pending", false, false, false, false, "not started"},
		{"pending", true, false, false, false, "launch accepted"},
		{"failed", true, true, false, false, "window mapped"},
		{"interrupted", true, true, true, false, "placement applied"},
		{"completed", true, true, true, true, "layout applied"},
	} {
		outcome := *item.RestoreOutcome
		outcome.Status = test.status
		outcome.LaunchAccepted, outcome.WindowMapped, outcome.PlacementApplied, outcome.LayoutApplied = test.launch, test.mapped, test.placement, test.layout
		item.RestoreOutcome = &outcome
		if got := terminalManageRestoreResult(item); got != test.status+" · "+test.stage {
			t.Fatalf("restore evidence = %q", got)
		}
	}
	item.RestoreOutcome = nil
	if got := terminalManageRestoreResult(item); got != "No record" {
		t.Fatalf("missing history became a restore result: %q", got)
	}
}

func TestTerminalManageRestoreOutcomeLoadsPersistedHistory(t *testing.T) {
	deps, contexts := terminalManageObservationFixture(t)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	record := sessionstate.RestoreOutcome{
		ContextID: contexts[0].ID, AttemptID: "44444444-4444-4444-8444-444444444444", Source: "explicit",
		StartedAt: at, UpdatedAt: at, Status: "failed", Reason: "layout_failed",
		Requested: sessionstate.RestoreWork{Window: true, Layout: true, Workspace: "98", LayoutDigest: strings.Repeat("0", 64)}, LaunchAccepted: true, WindowMapped: true,
	}
	if err := sessionstate.RestoreReportStoreFor(root).BeginExplicitContext(t.Context(), []sessionstate.RestoreOutcome{record}); err != nil {
		t.Fatal(err)
	}
	deps.newSwayClient = nil
	snapshot, err := (commandTerminalManageOperations{deps: deps}).Load(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	terminalManageRequireInventory(t, snapshot, contexts)
	for _, item := range snapshot.items {
		if item.ContextID == record.ContextID {
			if item.RestoreOutcome == nil || item.RestoreOutcome.AttemptID != record.AttemptID || terminalManageRestoreResult(item) != "failed · window mapped" {
				t.Fatalf("saved history absent from manager loader: %+v", item)
			}
		} else if item.RestoreOutcome != nil {
			t.Fatal("restore outcome associated with a different context")
		}
	}
}

func TestTerminalManageRestoreOutcomeViewportsKeepContextAndControls(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		model, ops, item := terminalManageRestoreOutcomeFixture()
		second := terminalManageTestItem("33333333-3333-4333-8333-333333333333", "Work other", sessionstate.ContextActive)
		if !recorded {
			item.RestoreOutcome = nil
		}
		ops.snapshots = [][]terminalInventoryResult{{item, second}}
		ops.windows = []map[sessionstate.ContextID]terminalWindowPresence{{item.ContextID: terminalWindowClosed}}
		model = terminalManageRunInit(t, model)
		model.filter = "Work"
		model.rebuildVisible()
		model.selectedID = item.ContextID
		model.restoreSelection()
		for _, size := range []tea.WindowSizeMsg{{Width: 48, Height: 16}, {Width: 80, Height: 24}, {Width: 100, Height: 22}, {Width: 120, Height: 28}} {
			model = terminalManageUpdate(t, model, size)
			view := model.render()
			for _, want := range []string{"Context " + string(item.ContextID), "Last restore", "Next login", "Last change", "Herdr", "Agent", "Evidence", "[q] Quit", "[r] Refresh"} {
				if !strings.Contains(view, want) {
					t.Fatalf("missing %q at %dx%d (recorded=%t):\n%s", want, size.Width, size.Height, recorded, view)
				}
			}
			if recorded {
				for _, want := range []string{"failed · window mapped", "Layout failed", item.RestoreOutcome.UpdatedAt.Local().Format("02 Jan"), "[t] Retry"} {
					if !strings.Contains(view, want) {
						t.Fatalf("restore evidence/control missing %q:\n%s", want, view)
					}
				}
				if size.Width >= 100 && (!strings.Contains(view, item.RestoreOutcome.AttemptID) || !strings.Contains(view, "Explicit")) {
					t.Fatalf("wide view lost exact attempt/source:\n%s", view)
				}
			} else if !strings.Contains(view, "No record") || strings.Contains(view, "[t] Retry") {
				t.Fatalf("no history implied an outcome/retry:\n%s", view)
			}
			for _, line := range strings.Split(view, "\n") {
				if ansi.StringWidth(line) > size.Width {
					t.Fatalf("line exceeds viewport: %q", line)
				}
			}
			if size.Width == 80 && !strings.Contains(view, "Window       closed") {
				t.Fatalf("historical mapping implied a current window:\n%s", view)
			}
			t.Logf("%dx%d recorded=%t:\n%s", size.Width, size.Height, recorded, view)
		}
	}
}

func TestTerminalManageRestoreRetryPinsIdentityAndRefreshesOutcome(t *testing.T) {
	model, ops, item := terminalManageRestoreOutcomeFixture()
	updated := item
	updated.Label, updated.Identity.Project = "Work z", "Work z"
	outcome := *item.RestoreOutcome
	outcome.Status, outcome.Reason, outcome.LayoutApplied = "completed", "restore_complete", true
	updated.RestoreOutcome = &outcome
	other := terminalManageTestItem("33333333-3333-4333-8333-333333333333", "Work a", sessionstate.ContextActive)
	ops.snapshots = [][]terminalInventoryResult{{item}, {other, updated}}
	model.filter = "Work"
	model, oldProbe := terminalManageUpdateWithCommand(t, model, model.Init()())
	oldContext := model.probeCtx
	model, retry := terminalManageUpdateWithCommand(t, model, terminalManageKey("t"))
	if retry == nil || !model.pending || oldContext.Err() == nil {
		t.Fatal("retry was not pinned/pending or did not cancel observation")
	}
	for _, key := range []string{"t", "j", "o", "a"} {
		var next tea.Cmd
		model, next = terminalManageUpdateWithCommand(t, model, terminalManageKey(key))
		if next != nil {
			t.Fatal("busy retry permitted another operation")
		}
	}
	model, _ = terminalManageUpdateWithCommand(t, model, oldProbe())
	model = terminalManageRunCommand(t, model, retry)
	selected, ok := model.selected()
	if len(ops.retried) != 1 || ops.retried[0] != item.ContextID || ops.sockets[0] != model.socket || !ok || selected.ContextID != item.ContextID || model.cursor != 1 || model.filter != "Work" {
		t.Fatal("retry changed exact identity/socket or lost filtered selection")
	}
	if len(ops.opens) != 0 || len(ops.stateChanges) != 0 || strings.Contains(model.render(), "[t] Retry") || !strings.Contains(model.render(), "completed") {
		t.Fatal("retry opened/activated a context or failed to refresh its outcome")
	}
}

func TestTerminalManageRestoreRetryRejectsArchivedAndNonfailed(t *testing.T) {
	for _, status := range []string{"", "pending", "completed", "skipped", "failed", "interrupted"} {
		for _, archived := range []bool{false, true} {
			model, ops, item := terminalManageRestoreOutcomeFixture()
			item.RestoreOutcome.Status = status
			if status == "" {
				item.RestoreOutcome = nil
			}
			if archived {
				item.State = sessionstate.ContextArchived
			}
			ops.snapshots = [][]terminalInventoryResult{{item}}
			model = terminalManageRunInit(t, model)
			model, retry := terminalManageUpdateWithCommand(t, model, terminalManageKey("t"))
			want := !archived && (status == "failed" || status == "interrupted")
			if (retry != nil) != want {
				t.Fatalf("retry availability status=%s archived=%t", status, archived)
			}
			if !want && (len(ops.opens) != 0 || len(ops.stateChanges) != 0 || len(ops.retried) != 0 || strings.Contains(model.render(), "[t] Retry")) {
				t.Fatal("ineligible retry opened/activated a context or advertised retry")
			}
		}
	}
}

func TestTerminalManageRestoreRetryFailureReloadsLatestHistory(t *testing.T) {
	model, ops, _ := terminalManageRestoreOutcomeFixture()
	ops.retryErr = errors.New("retry rejected after fresh policy check")
	model = terminalManageRunInit(t, model)
	model = terminalManageSend(t, model, terminalManageKey("t"))
	if ops.listCalls != 2 || model.err == nil || len(ops.retried) != 1 || model.pending {
		t.Fatal("failed retry lost error or did not reload current outcome")
	}
}

func TestTerminalManageRestoreRetryIsOptionalAndDoesNotStealModalTyping(t *testing.T) {
	model, ops, item := terminalManageRestoreOutcomeFixture()
	model = terminalManageRunInit(t, model)
	for _, key := range []string{"/", "e", "d", "?"} {
		current := terminalManageUpdate(t, model, terminalManageKey(key))
		current, command := terminalManageUpdateWithCommand(t, current, terminalManageKey("t"))
		if command != nil {
			// Input blinking is permitted; a retry is never queued by a modal.
			if current.pending {
				t.Fatalf("modal %q triggered restore retry", key)
			}
		}
		if len(ops.retried) != 0 || current.pending {
			t.Fatalf("modal %q triggered retry", key)
		}
	}
	legacy := newTerminalManageModel(&terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{item}}})
	legacy = terminalManageRunInit(t, legacy)
	legacy, command := terminalManageUpdateWithCommand(t, legacy, terminalManageKey("t"))
	if command != nil || legacy.pending || legacy.err == nil || strings.Contains(legacy.render(), "[t] Retry") {
		t.Fatal("optional retry interface was required or falsely advertised")
	}
}

func TestTerminalManageRestoreReasonAndTimeFitMinimumDetailWidth(t *testing.T) {
	_, _, item := terminalManageRestoreOutcomeFixture()
	for _, reason := range []string{
		"restore_requested", "launch_accepted", "window_mapped", "placement_applied", "layout_applied", "restore_complete",
		"policy_archived", "policy_not_desired", "context_missing", "identity_changed", "launch_failed", "mapping_timeout",
		"placement_failed", "layout_failed", "placement_timeout", "layout_timeout", "interrupted", "daemon_restarted",
		"user_cancelled", "awaiting_daemon", "ambiguous_window", "observation_unavailable", "history_unavailable", "restore_deferred",
	} {
		item.RestoreOutcome.Reason = reason
		line := terminalManageDetail("Restore info", terminalManageRestoreInfo(item, 46))
		if strings.Contains(line, "Unknown reason") || ansi.StringWidth(line) > 46 || !strings.Contains(line, item.RestoreOutcome.UpdatedAt.Local().Format("15:04")) {
			t.Fatalf("reason/time unreadable at minimum size: %q", line)
		}
	}
}

func TestTerminalManageRestoreSummaryIncludesHiddenContextsAndModalKeys(t *testing.T) {
	model, ops, item := terminalManageRestoreOutcomeFixture()
	hidden := item
	hidden.ContextID, hidden.Label = "33333333-3333-4333-8333-333333333333", "Hidden"
	outcome := *item.RestoreOutcome
	outcome.ContextID, outcome.Status, outcome.Reason = hidden.ContextID, "interrupted", "daemon_restarted"
	hidden.RestoreOutcome = &outcome
	ops.snapshots = [][]terminalInventoryResult{{item, hidden}}
	model = terminalManageRunInit(t, model)
	model.filter = "Work"
	model.rebuildVisible()
	for _, size := range []tea.WindowSizeMsg{{Width: 48, Height: 16}, {Width: 80, Height: 24}, {Width: 100, Height: 22}} {
		model = terminalManageUpdate(t, model, size)
		for _, test := range []struct{ key, want string }{{"?", "1 failed · 1 interrupted"}, {"/", "[Esc] Clear filter"}, {"e", "[Ctrl+A] Clear"}, {"d", "[n/Esc] Cancel"}} {
			current := terminalManageUpdate(t, model, terminalManageKey(test.key))
			view := current.render()
			if !strings.Contains(view, test.want) {
				t.Fatalf("modal %q lost %q at %dx%d:\n%s", test.key, test.want, size.Width, size.Height, view)
			}
			if test.key == "?" && (!strings.Contains(view, "[q] Quit") || !strings.Contains(view, "[t] Retry")) {
				t.Fatalf("help lost controls:\n%s", view)
			}
		}
	}
}
