package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	sessionstate "github.com/marang/sway-session/internal/session"
)

func TestTerminalManageReadableNextLoginReasons(t *testing.T) {
	for _, test := range []struct {
		reason   string
		eligible bool
		want     string
	}{
		{"active_terminal", true, "Yes · Active terminal"},
		{"archived", false, "No · Archived"},
		{"desired_open_follow", true, "Yes · Desired open (follow)"},
		{"desired_open_pinned", true, "Yes · Desired open (pinned)"},
		{"desired_closed", false, "No · Desired closed"},
		{"", false, "Unknown · Unknown policy"},
		{"future_policy", true, "Unknown · Unknown policy"},
	} {
		t.Run(test.reason, func(t *testing.T) {
			item := terminalInventoryResult{RestorePolicy: sessionstate.RestorePolicyDecision{Eligible: test.eligible, Reason: test.reason}}
			if got := terminalManageNextLogin(item); got != test.want {
				t.Fatalf("next login = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTerminalManageLatestTransitionDoesNotInferManualClose(t *testing.T) {
	at := time.Date(2026, time.September, 30, 12, 34, 0, 0, time.UTC)
	for _, test := range []struct {
		reason sessionstate.LifecycleReason
		want   string
	}{
		{"explicit_archive", "Explicit archive"},
		{"explicit_activate", "Explicit activate"},
		{"observed_terminal_close", "Close observed"},
		{"future_reason", "Unknown reason"},
	} {
		item := terminalInventoryResult{Lifecycle: &sessionstate.LifecycleTransition{Reason: test.reason, At: at}}
		for _, width := range []int{46, 78} {
			got := terminalManageLastChange(item, width)
			format := "02 Jan 2006 15:04"
			if width < 72 {
				format = "02 Jan 15:04"
			}
			if !strings.Contains(got, test.want) || !strings.Contains(got, at.Local().Format(format)) || strings.Contains(strings.ToLower(got), "manual") {
				t.Fatalf("transition misrepresented: %q", got)
			}
		}
	}
	if got := terminalManageLastChange(terminalInventoryResult{}, 46); got != "Unknown (legacy)" {
		t.Fatalf("legacy transition = %q", got)
	}
}

func TestTerminalManageRestoreDetailsStayUsableAcrossViewports(t *testing.T) {
	at := time.Date(2026, time.September, 30, 12, 34, 0, 0, time.UTC)
	items := []terminalInventoryResult{
		terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Active but closed", sessionstate.ContextActive),
		terminalManageTestItem("22222222-2222-4222-8222-222222222222", "Archived but open", sessionstate.ContextArchived),
		terminalManageTestItem("33333333-3333-4333-8333-333333333333", "Legacy", sessionstate.ContextArchived),
	}
	items[0].RestorePolicy = sessionstate.RestorePolicyDecision{Eligible: true, Reason: "active_terminal"}
	items[0].Lifecycle = &sessionstate.LifecycleTransition{Reason: "explicit_activate", At: at}
	items[1].RestorePolicy = sessionstate.RestorePolicyDecision{Reason: "archived"}
	items[1].Lifecycle = &sessionstate.LifecycleTransition{Reason: "observed_terminal_close", At: at}
	items[2].RestorePolicy = sessionstate.RestorePolicyDecision{Reason: "archived"}
	ops := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{items}, windows: []map[sessionstate.ContextID]terminalWindowPresence{{
		items[0].ContextID: terminalWindowClosed, items[1].ContextID: terminalWindowOpen, items[2].ContextID: terminalWindowClosed,
	}}}
	model := newTerminalManageModel(ops)
	model.noColor = true
	model = terminalManageRunInit(t, model)
	for _, size := range []tea.WindowSizeMsg{{Width: 48, Height: 16}, {Width: 80, Height: 24}, {Width: 100, Height: 22}, {Width: 120, Height: 26}} {
		model = terminalManageUpdate(t, model, size)
		for _, item := range items {
			model.selectedID = item.ContextID
			model.restoreSelection()
			view := model.render()
			for _, want := range []string{"Next login", terminalManageNextLogin(item), "Last change", "Herdr", "Agent", "Evidence", "[r] Refresh", "[q] Quit"} {
				if !strings.Contains(view, want) {
					t.Fatalf("missing %q at %dx%d:\n%s", want, size.Width, size.Height, view)
				}
			}
			if item.Lifecycle != nil && !strings.Contains(view, at.Local().Format("02 Jan")) {
				t.Fatalf("transition time clipped:\n%s", view)
			}
			if item.Lifecycle == nil && !strings.Contains(view, "Unknown (legacy)") {
				t.Fatalf("legacy transition invented:\n%s", view)
			}
			if len(strings.Split(view, "\n")) > size.Height {
				t.Fatalf("too many rows at %dx%d:\n%s", size.Width, size.Height, view)
			}
			for _, line := range strings.Split(view, "\n") {
				if ansi.StringWidth(line) > size.Width {
					t.Fatalf("line exceeds viewport: %q", line)
				}
			}
			if strings.Contains(strings.ToLower(view), "manual") {
				t.Fatalf("observed close described as manual:\n%s", view)
			}
		}
		t.Logf("representative %dx%d view:\n%s", size.Width, size.Height, model.render())
	}
	model.selectedID = items[1].ContextID
	model.restoreSelection()
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 80, Height: 24})
	view := model.render()
	for _, want := range []string{"Window       open", "Restore      archived", "Next login   No · Archived", "Close observed"} {
		if !strings.Contains(view, want) {
			t.Fatalf("window and policy conflated, missing %q:\n%s", want, view)
		}
	}
}

func TestTerminalManageRestorePolicyRefreshPreservesFilterAndSelection(t *testing.T) {
	item := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Work", sessionstate.ContextArchived)
	item.RestorePolicy = sessionstate.RestorePolicyDecision{Reason: "archived"}
	active := item
	active.State = sessionstate.ContextActive
	active.RestorePolicy = sessionstate.RestorePolicyDecision{Eligible: true, Reason: "active_terminal"}
	active.Lifecycle = &sessionstate.LifecycleTransition{Reason: "explicit_activate", At: time.Now().UTC()}
	ops := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{item}, {active}}}
	model := newTerminalManageModel(ops)
	model.noColor = true
	model.width, model.height = 48, 16
	model = terminalManageRunInit(t, model)
	model = terminalManageUpdate(t, model, terminalManageKey("/"))
	model = terminalManageUpdate(t, model, terminalManageKey("Work"))
	model = terminalManageUpdate(t, model, terminalManageKey("enter"))
	model = terminalManageSend(t, model, terminalManageKey("a"))
	selected, ok := model.selected()
	if !ok || selected.ContextID != item.ContextID || model.filter != "Work" || len(ops.stateChanges) != 1 {
		t.Fatal("policy action lost selection/filter or failed to activate")
	}
	view := model.render()
	for _, want := range []string{"Yes · Active terminal", "Explicit activate", "[q] Quit"} {
		if !strings.Contains(view, want) {
			t.Fatalf("refreshed policy missing %q:\n%s", want, view)
		}
	}
}

func TestTerminalManageRestoreDetailsKeepModalControlsWithManyMatches(t *testing.T) {
	items := make([]terminalInventoryResult, 8)
	for index := range items {
		items[index] = terminalManageTestItem(sessionstate.ContextID(fmt.Sprintf("%08d-1111-4111-8111-111111111111", index+1)), "Work "+fmt.Sprint(index+1), sessionstate.ContextActive)
		items[index].RestorePolicy = sessionstate.RestorePolicyDecision{Eligible: true, Reason: "active_terminal"}
		items[index].Lifecycle = &sessionstate.LifecycleTransition{Reason: "explicit_activate", At: time.Now().UTC()}
	}
	model := newTerminalManageModel(&terminalManageTestOperations{snapshots: [][]terminalInventoryResult{items}})
	model.noColor = true
	model = terminalManageRunInit(t, model)
	model.filter = "Work"
	model.rebuildVisible()
	model.cursor = 4
	model.rememberSelected()
	for _, size := range []tea.WindowSizeMsg{{Width: 48, Height: 16}, {Width: 80, Height: 24}, {Width: 100, Height: 24}} {
		model = terminalManageUpdate(t, model, size)
		for _, test := range []struct {
			key, control string
		}{
			{"", "[q] Quit"}, {"?", "[r] Refresh"}, {"/", "[Esc] Clear filter"}, {"e", "[Ctrl+A] Clear"}, {"d", "[n/Esc] Cancel"},
		} {
			current := model
			if test.key != "" {
				current = terminalManageUpdate(t, current, terminalManageKey(test.key))
			}
			view := current.render()
			if !strings.Contains(view, test.control) {
				t.Fatalf("%q controls clipped at %dx%d:\n%s", test.key, size.Width, size.Height, view)
			}
			if test.key == "" && (!strings.Contains(view, "Yes · Active terminal") || !strings.Contains(view, "Explicit activate")) {
				t.Fatalf("policy clipped with many matches:\n%s", view)
			}
		}
	}
}
