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

type terminalManageActivityTestOperations struct {
	terminalManageTestOperations
	observe func(context.Context, []terminalInventoryResult) map[sessionstate.ContextID]sessionstate.TerminalSessionObservation
}

func (operations *terminalManageActivityTestOperations) ObserveActivity(ctx context.Context, items []terminalInventoryResult) map[sessionstate.ContextID]sessionstate.TerminalSessionObservation {
	return operations.observe(ctx, items)
}

func terminalManageActivityFixture(t *testing.T) (terminalManageModel, *terminalManageActivityTestOperations, terminalInventoryResult) {
	t.Helper()
	item := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Work", sessionstate.ContextActive)
	ops := &terminalManageActivityTestOperations{terminalManageTestOperations: terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{item}}}}
	ops.observe = func(context.Context, []terminalInventoryResult) map[sessionstate.ContextID]sessionstate.TerminalSessionObservation {
		return map[sessionstate.ContextID]sessionstate.TerminalSessionObservation{item.ContextID: {SessionState: "running", AgentState: "detected"}}
	}
	model := newTerminalManageModel(ops)
	model.noColor = true
	model.width, model.height = 80, 24
	return model, ops, item
}

func TestTerminalManageActivityBulkPendingAndIndependentEvidence(t *testing.T) {
	for _, scenario := range []struct {
		window       terminalWindowPresence
		herdr, agent string
	}{
		{terminalWindowClosed, "running", "detected"},
		{terminalWindowOpen, "running", "none"},
		{terminalWindowOpen, "stopped", "none"},
		{terminalWindowClosed, "missing", "unknown"},
		{terminalWindowUnknown, "unknown", "unknown"},
		// A stale association does not turn unknown evidence into detected activity.
		{terminalWindowClosed, "unknown", "unknown"},
	} {
		t.Run(scenario.window.String()+scenario.herdr+scenario.agent, func(t *testing.T) {
			model, ops, item := terminalManageActivityFixture(t)
			ops.windows = []map[sessionstate.ContextID]terminalWindowPresence{{item.ContextID: scenario.window}}
			calls := 0
			now := time.Now()
			ops.observe = func(ctx context.Context, items []terminalInventoryResult) map[sessionstate.ContextID]sessionstate.TerminalSessionObservation {
				calls++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 3*time.Second || len(items) != 1 {
					t.Errorf("unbounded or nonbulk observation: deadline=%v items=%d", deadline, len(items))
				}
				return map[sessionstate.ContextID]sessionstate.TerminalSessionObservation{item.ContextID: {SessionState: scenario.herdr, AgentState: scenario.agent, ObservedAt: &now}}
			}
			model, probe := terminalManageUpdateWithCommand(t, model, model.Init()())
			if model.loading || calls != 0 || probe == nil || !strings.Contains(model.render(), "Herdr        pending") || !strings.Contains(model.render(), "Agent        pending") {
				t.Fatalf("load did not render pending asynchronously:\n%s", model.render())
			}
			model = terminalManageRunCommand(t, model, probe)
			view := model.render()
			for _, want := range []string{"Window       " + scenario.window.String(), "Restore      enabled", "Herdr        " + scenario.herdr, "Agent        " + scenario.agent, "Evidence", "old", "[q] Quit"} {
				if !strings.Contains(view, want) {
					t.Fatalf("missing %q:\n%s", want, view)
				}
			}
			_, next := terminalManageUpdateWithCommand(t, model, terminalManagePulseMsg{})
			if calls != 1 || next != nil || model.activityPending {
				t.Fatal("activity probe repeated or remained pending")
			}
		})
	}
}

func TestTerminalManageActivityRefreshCancelsAndDiscardsGeneration(t *testing.T) {
	model, ops, _ := terminalManageActivityFixture(t)
	model, old := terminalManageUpdateWithCommand(t, model, model.Init()())
	oldContext := model.probeCtx
	model, reload := terminalManageUpdateWithCommand(t, model, terminalManageKey("r"))
	if !errors.Is(oldContext.Err(), context.Canceled) {
		t.Fatal("refresh did not cancel probe")
	}
	model, current := terminalManageUpdateWithCommand(t, model, reload())
	ops.observe = func(context.Context, []terminalInventoryResult) map[sessionstate.ContextID]sessionstate.TerminalSessionObservation {
		return nil
	}
	model, ignored := terminalManageUpdateWithCommand(t, model, old())
	if ignored != nil || !model.activityPending || model.items[0].Activity.SessionState != "pending" {
		t.Fatal("old result replaced new pending evidence")
	}
	model = terminalManageRunCommand(t, model, current)
	if model.items[0].Activity.SessionState != "unknown" {
		t.Fatal("missing bulk result is not unknown")
	}
}

func TestTerminalManageActivityCancellationOnActionsAndQuit(t *testing.T) {
	for _, key := range []string{"q", "ctrl+c", "o", "a", "m"} {
		t.Run(key, func(t *testing.T) {
			model, _, _ := terminalManageActivityFixture(t)
			model, old := terminalManageUpdateWithCommand(t, model, model.Init()())
			ctx := model.probeCtx
			msg := terminalManageKey(key)
			if key == "ctrl+c" {
				msg = tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
			}
			model, _ = terminalManageUpdateWithCommand(t, model, msg)
			if ctx.Err() == nil {
				t.Fatal("probe was not canceled")
			}
			model, _ = terminalManageUpdateWithCommand(t, model, old())
			if model.items[0].Activity.AgentState != "pending" {
				t.Fatal("canceled result applied")
			}
		})
	}
}

func TestTerminalManageActivityFailedActionRestartRejectsOldProbe(t *testing.T) {
	model, ops, _ := terminalManageActivityFixture(t)
	ops.openErr = errors.New("open failed")
	model, old := terminalManageUpdateWithCommand(t, model, model.Init()())
	model, action := terminalManageUpdateWithCommand(t, model, terminalManageKey("o"))
	model, current := terminalManageUpdateWithCommand(t, model, action())
	model, _ = terminalManageUpdateWithCommand(t, model, old())
	if !model.activityPending || model.items[0].Activity.AgentState != "pending" {
		t.Fatal("old probe won action restart race")
	}
	model = terminalManageRunCommand(t, model, current)
	if model.err == nil || model.items[0].Activity.AgentState != "detected" {
		t.Fatal("failed action lost error or evidence refresh")
	}
}

func TestTerminalManageActivityDirectoryUpdatesFilterAndNaming(t *testing.T) {
	model, ops, item := terminalManageActivityFixture(t)
	item.Label, item.Identity.Project = "", ""
	ops.snapshots = [][]terminalInventoryResult{{item}}
	ops.observe = func(context.Context, []terminalInventoryResult) map[sessionstate.ContextID]sessionstate.TerminalSessionObservation {
		return map[sessionstate.ContextID]sessionstate.TerminalSessionObservation{item.ContextID: {SessionState: "running", AgentState: "none", Directory: sessionstate.HerdrDirectoryObservation{Directory: "/work/new-name"}}}
	}
	model, probe := terminalManageUpdateWithCommand(t, model, model.Init()())
	model.filter = "new-name"
	model.rebuildVisible()
	model = terminalManageRunCommand(t, model, probe)
	if len(model.visible) != 1 || terminalManageName(model.items[0]) != "Terminal · new-name" {
		t.Fatal("activity directory did not update naming and filter")
	}
}

func TestTerminalManageActivityLegacyFallbackExplicitUnknown(t *testing.T) {
	item := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "", sessionstate.ContextActive)
	ops := &terminalManageDirectoryTestOperations{terminalManageTestOperations: terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{item}}}, observations: map[sessionstate.ContextID]sessionstate.HerdrDirectoryObservation{item.ContextID: {Directory: "/work/fallback"}}}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))
	if len(ops.probed) != 1 || model.items[0].PaneDirectory != "/work/fallback" || model.items[0].Activity.SessionState != "unknown" || model.items[0].Activity.AgentState != "unknown" {
		t.Fatal("legacy fallback lost naming or implied activity")
	}
}

func TestTerminalManageActivityCleanupAgeRedactionAndFailedPurgeRefresh(t *testing.T) {
	model, ops, item := terminalManageActivityFixture(t)
	observed := time.Now().Add(-2 * time.Hour)
	ops.observe = func(context.Context, []terminalInventoryResult) map[sessionstate.ContextID]sessionstate.TerminalSessionObservation {
		return map[sessionstate.ContextID]sessionstate.TerminalSessionObservation{item.ContextID: {SessionState: "stopped", AgentState: "none", ObservedAt: &observed, Reason: "private /home/user/token\nsecret"}}
	}
	model = terminalManageRunInit(t, model)
	model = terminalManageUpdate(t, model, terminalManageKey("d"))
	view := model.render()
	for _, want := range []string{"Herdr stopped · Agent none", "2h", "old", "observation_unavailable", "[y] Delete"} {
		if !strings.Contains(view, want) {
			t.Fatalf("cleanup preview missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "secret") || strings.Contains(view, "token") {
		t.Fatal("preview leaked failure detail")
	}
	ops.purgeErr = errors.New("cleanup refused")
	model = terminalManageSend(t, model, terminalManageKey("y"))
	if ops.listCalls != 2 || model.err == nil || !strings.Contains(model.render(), "Cleanup refused") {
		t.Fatal("failed purge did not refresh and preserve error")
	}
}

func TestTerminalManageActivityNarrowLayoutKeepsKeyboardFooter(t *testing.T) {
	model, _, _ := terminalManageActivityFixture(t)
	model = terminalManageRunInit(t, model)
	for _, size := range []tea.WindowSizeMsg{{Width: 80, Height: 24}, {Width: 48, Height: 16}, {Width: 100, Height: 24}} {
		model = terminalManageUpdate(t, model, size)
		for _, mode := range []terminalManageMode{terminalManageListMode, terminalManagePurgeMode} {
			model.mode = mode
			view := model.render()
			want := "[q] Quit"
			if mode == terminalManagePurgeMode {
				want = "[y] Delete"
			}
			if !strings.Contains(view, want) || len(strings.Split(view, "\n")) > size.Height {
				t.Fatalf("clipped controls at %dx%d:\n%s", size.Width, size.Height, view)
			}
			for _, line := range strings.Split(view, "\n") {
				if ansi.StringWidth(line) > size.Width {
					t.Fatal("line exceeds viewport")
				}
			}
		}
	}
}

func TestTerminalManageActivityOneProbeIncludesArchivedAndFilteredItems(t *testing.T) {
	model, ops, first := terminalManageActivityFixture(t)
	second := terminalManageTestItem("22222222-2222-4222-8222-222222222222", "Hidden", sessionstate.ContextArchived)
	ops.snapshots = [][]terminalInventoryResult{{first, second}}
	model.filter = string(first.ContextID)
	calls := 0
	ops.observe = func(_ context.Context, items []terminalInventoryResult) map[sessionstate.ContextID]sessionstate.TerminalSessionObservation {
		calls++
		if len(items) != 2 {
			t.Fatalf("probe omitted filtered or archived item: %d", len(items))
		}
		return map[sessionstate.ContextID]sessionstate.TerminalSessionObservation{
			first.ContextID:  {SessionState: "running", AgentState: "none"},
			second.ContextID: {SessionState: "stopped", AgentState: "none"},
		}
	}
	model, probe := terminalManageUpdateWithCommand(t, model, model.Init()())
	ctx := model.probeCtx
	for _, item := range model.items {
		if item.Activity.SessionState != "pending" {
			t.Fatal("not all items pending")
		}
	}
	model = terminalManageRunCommand(t, model, probe)
	if calls != 1 || len(model.visible) != 1 || model.items[1].Activity.SessionState != "stopped" || ctx.Err() == nil {
		t.Fatal("bulk observation or context cleanup failed")
	}
}

func TestTerminalManageActivityExpiredDeadlineBecomesUnknown(t *testing.T) {
	model, ops, _ := terminalManageActivityFixture(t)
	// An already-expired parent tests cancellation without waiting for the real budget.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	model.ctx = ctx
	ops.observe = func(ctx context.Context, _ []terminalInventoryResult) map[sessionstate.ContextID]sessionstate.TerminalSessionObservation {
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal("observer did not receive expired deadline")
		}
		return nil
	}
	model = terminalManageRunInit(t, model)
	if model.activityPending || model.items[0].Activity.SessionState != "unknown" || model.items[0].Activity.AgentState != "unknown" {
		t.Fatal("expired evidence implied absence or remained pending")
	}
}
