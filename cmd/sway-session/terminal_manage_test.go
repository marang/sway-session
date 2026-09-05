package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	sessionstate "github.com/marang/sway-session/internal/session"
)

// These tests deliberately exercise the TUI only through its Bubble Tea seam
// and its injected operations. They do not read session state or invoke the
// CLI recursively.

func TestTerminalManageLoadsInventoryAndExplainsAnEmptyList(t *testing.T) {
	ops := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{
		terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Daily work", sessionstate.ContextActive),
		terminalManageTestItem("22222222-2222-4222-8222-222222222222", "Archived work", sessionstate.ContextArchived),
	}}}
	model := newTerminalManageModel(ops)
	model = terminalManageRunInit(t, model)

	view := model.View().Content
	for _, want := range []string{"2 saved", "0 open", "Daily work", "Archived work", "unknown", "restore enabled", "archived", "j/k", "Open"} {
		if !strings.Contains(view, want) {
			t.Fatalf("loaded inventory is missing %q:\n%s", want, view)
		}
	}

	empty := newTerminalManageModel(&terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{}}})
	empty = terminalManageRunInit(t, empty)
	if view := empty.View().Content; !strings.Contains(view, "No managed terminals") || !strings.Contains(view, "[q] Quit") {
		t.Fatalf("empty inventory does not give a recovery path:\n%s", view)
	}
}

func TestTerminalManageSeparatesSavedWindowAndRestoreState(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	first := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Daily work", sessionstate.ContextActive)
	second := terminalManageTestItem("22222222-2222-4222-8222-222222222222", "Review", sessionstate.ContextActive)
	archived := terminalManageTestItem("33333333-3333-4333-8333-333333333333", "Archived but open", sessionstate.ContextArchived)
	archived.ArchivedAt = terminalManageTestArchivedAt()
	windows := map[sessionstate.ContextID]terminalWindowPresence{
		first.ContextID:    terminalWindowOpen,
		second.ContextID:   terminalWindowClosed,
		archived.ContextID: terminalWindowOpen,
	}
	model := terminalManageRunInit(t, newTerminalManageModel(&terminalManageTestOperations{
		snapshots: [][]terminalInventoryResult{{first, second, archived}},
		windows:   []map[sessionstate.ContextID]terminalWindowPresence{windows},
	}))
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 80, Height: 24})
	view := model.View().Content
	for _, want := range []string{
		"3 saved · 2 open", "Snapshot [r] refresh",
		"● open · restore enabled  Daily work",
		"○ closed · restore enabled  Review",
		"● open · archived  Archived but open",
		"Window       open", "Restore      enabled", "Last focused ",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("80x24 presence view is missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "State       ") || strings.Contains(view, "Last active") || strings.Contains(view, "\x1b[") {
		t.Fatalf("80x24 NO_COLOR view retained obsolete or styled state:\n%s", view)
	}
	t.Logf("representative 80x24 NO_COLOR view:\n%s", view)

	model = terminalManageUpdate(t, model, terminalManageKey("j"))
	model = terminalManageUpdate(t, model, terminalManageKey("j"))
	narrow := terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 48, Height: 16}).View().Content
	for _, want := range []string{"3 saved · 2 open", "Snapshot · [r] refresh", "open · archived"} {
		if !strings.Contains(narrow, want) {
			t.Fatalf("48x16 presence view is missing %q:\n%s", want, narrow)
		}
	}
	if strings.Contains(narrow, "\x1b[") {
		t.Fatalf("48x16 NO_COLOR view contains ANSI styling: %q", narrow)
	}
	t.Logf("representative 48x16 NO_COLOR view:\n%s", narrow)
}

func TestTerminalManageRefreshRetainsSelectionAndClearsStalePresenceOnObservationError(t *testing.T) {
	first := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "First", sessionstate.ContextActive)
	second := terminalManageTestItem("22222222-2222-4222-8222-222222222222", "Selected", sessionstate.ContextActive)
	initialWindows := map[sessionstate.ContextID]terminalWindowPresence{
		first.ContextID: terminalWindowOpen, second.ContextID: terminalWindowClosed,
	}
	ops := &terminalManageTestOperations{
		snapshots:    [][]terminalInventoryResult{{first, second}, {second, first}},
		windows:      []map[sessionstate.ContextID]terminalWindowPresence{initialWindows, {}},
		windowErrors: []error{nil, errors.New("Sway socket is unavailable")},
	}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 80, Height: 24})
	model = terminalManageUpdate(t, model, terminalManageKey("j"))
	selected, ok := model.selected()
	if !ok || selected.ContextID != second.ContextID || model.windowPresence(first.ContextID) != terminalWindowOpen {
		t.Fatalf("invalid initial state: selected=%+v windows=%v", selected, model.windows)
	}

	updated, command := terminalManageUpdateWithCommand(t, model, terminalManageKey("r"))
	if command == nil || updated.windowPresence(first.ContextID) != terminalWindowUnknown || updated.windowPresence(second.ContextID) != terminalWindowUnknown {
		t.Fatalf("refresh retained stale window certainty: loading=%t windows=%v", updated.loading, updated.windows)
	}
	updated = terminalManageRunCommand(t, updated, command)
	selected, ok = updated.selected()
	if !ok || selected.ContextID != second.ContextID {
		t.Fatalf("selection drifted across failed observation refresh: selected=%+v", selected)
	}
	view := updated.View().Content
	if !strings.Contains(view, "2 saved · 0 open · 2 unknown") ||
		!strings.Contains(view, "Window observation incomplete") || !strings.Contains(view, "[r] Refresh") {
		t.Fatalf("observation failure is stale or not actionable:\n%s", view)
	}
}

func TestTerminalManageInitialLoadFailureDoesNotClaimTheRegistryIsEmpty(t *testing.T) {
	ops := &terminalManageTestOperations{
		listErr:   errors.New("activity state is unreadable"),
		snapshots: [][]terminalInventoryResult{{terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Migrated", sessionstate.ContextActive)}},
	}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))
	view := model.View().Content
	if !strings.Contains(view, "Unable to load managed terminals") || !strings.Contains(view, "Activity state is unreadable") {
		t.Fatalf("initial load failure is not actionable:\n%s", view)
	}
	if strings.Contains(view, "No managed terminals") || strings.Contains(view, "sway-session terminal --new") {
		t.Fatalf("initial load failure was misrepresented as an empty registry:\n%s", view)
	}
	if !strings.Contains(view, "[m] Migrate") {
		t.Fatalf("load failure hid the migration recovery action:\n%s", view)
	}

	ops.listErr = nil
	model = terminalManageSend(t, model, terminalManageKey("m"))
	if ops.migrateCalls != 1 || !strings.Contains(model.View().Content, "Migrated") {
		t.Fatalf("migration did not reload the imported inventory: calls=%d\n%s", ops.migrateCalls, model.View().Content)
	}
}

func TestTerminalManageFooterGroupsDiscoverableActions(t *testing.T) {
	item := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Help", sessionstate.ContextActive)
	model := terminalManageRunInit(t, newTerminalManageModel(&terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{item}}}))
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 100, Height: 24})
	view := model.View().Content
	for _, want := range []string{"Navigate", "Selected", "System", "[Enter/o] Open", "[m] Migrate", "[?] Help"} {
		if !strings.Contains(view, want) {
			t.Fatalf("grouped footer is missing %q:\n%s", want, view)
		}
	}
}

func TestTerminalManageNarrowFootersKeepEveryActionVisible(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	items := []terminalInventoryResult{
		terminalManageTestItem("11111111-1111-4111-8111-111111111111", "First", sessionstate.ContextActive),
		terminalManageTestItem("22222222-2222-4222-8222-222222222222", "Second", sessionstate.ContextActive),
		terminalManageTestItem("33333333-3333-4333-8333-333333333333", "Third", sessionstate.ContextActive),
		terminalManageTestItem("44444444-4444-4444-8444-444444444444", "Fourth", sessionstate.ContextActive),
	}
	model := terminalManageRunInit(t, newTerminalManageModel(&terminalManageTestOperations{snapshots: [][]terminalInventoryResult{items}}))
	for _, width := range []int{58, 64, 71, 73, 74, 80} {
		model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: width, Height: 24})
		view := model.View().Content
		for _, want := range []string{"[Enter] Open", "[e] Rename", "[a] Archive/activate", "[d] Delete", "[m] Migrate", "[r] Refresh", "[?] Help", "[q] Quit"} {
			if !strings.Contains(view, want) {
				t.Fatalf("width %d clipped action %q:\n%s", width, want, view)
			}
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("width %d rendered %d-column line %q", width, ansi.StringWidth(line), line)
			}
		}
	}
}

func TestTerminalManageWrapsCompleteMigrationResultAtEightyColumns(t *testing.T) {
	item := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Migrated", sessionstate.ContextActive)
	model := terminalManageRunInit(t, newTerminalManageModel(&terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{item}}}))
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 80, Height: 24})
	model.status = "Migrated 300 contexts, 275 terminal activity records, layout=true, application session=true; legacy JSON kept; skipped 25 stale runtime records; commit acknowledgement was lost, migrated state verified"

	plain := strings.ReplaceAll(ansi.Strip(model.View().Content), "│", " ")
	normalized := strings.Join(strings.Fields(plain), " ")
	for _, want := range []string{"legacy JSON kept", "skipped 25 stale runtime records", "commit acknowledgement was lost", "migrated state verified"} {
		if !strings.Contains(normalized, want) {
			t.Fatalf("80x24 migration result hid %q:\n%s", want, plain)
		}
	}
}

func TestTerminalManageNavigatesAndOpensOnlyTheSelectedActiveTerminal(t *testing.T) {
	first := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "First", sessionstate.ContextActive)
	second := terminalManageTestItem("22222222-2222-4222-8222-222222222222", "Second", sessionstate.ContextActive)
	ops := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{first, second}}}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))

	model = terminalManageUpdate(t, model, terminalManageKey("j"))
	_ = terminalManageSend(t, model, terminalManageKey("o"))
	if got, want := ops.opens, []sessionstate.ContextID{second.ContextID}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("open target = %v, want %v", got, want)
	}

	archived := terminalManageTestItem("33333333-3333-4333-8333-333333333333", "Closed", sessionstate.ContextArchived)
	blockedOps := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{archived}}}
	blocked := terminalManageRunInit(t, newTerminalManageModel(blockedOps))
	blocked = terminalManageSend(t, blocked, terminalManageKey("enter"))
	if len(blockedOps.opens) != 0 {
		t.Fatalf("archived context was opened: %v", blockedOps.opens)
	}
	if view := blocked.View().Content; !strings.Contains(view, "Activate") {
		t.Fatalf("archived open did not explain the recovery action:\n%s", view)
	}
}

func TestTerminalManageFiltersWithoutLosingTheUnderlyingSelection(t *testing.T) {
	alpha := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Alpha build", sessionstate.ContextActive)
	beta := terminalManageTestItem("22222222-2222-4222-8222-222222222222", "Beta review", sessionstate.ContextActive)
	ops := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{alpha, beta}}}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))

	model = terminalManageUpdate(t, model, terminalManageKey("/"))
	for _, key := range []string{"b", "e", "t", "a"} {
		model = terminalManageUpdate(t, model, terminalManageKey(key))
	}
	view := model.View().Content
	if !strings.Contains(view, "Beta review") || strings.Contains(view, "Alpha build") {
		t.Fatalf("filter did not constrain the list:\n%s", view)
	}

	model = terminalManageUpdate(t, model, terminalManageKey("esc"))
	_ = terminalManageSend(t, model, terminalManageKey("o"))
	if got := ops.opens; len(got) != 1 || got[0] != beta.ContextID {
		t.Fatalf("opening after filter selected %v, want %s", got, beta.ContextID)
	}
}

func TestTerminalManageRenamesWithEditableFriendlyTitle(t *testing.T) {
	item := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Old title", sessionstate.ContextActive)
	ops := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{item}}}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))

	model = terminalManageUpdate(t, model, terminalManageKey("e"))
	if view := model.View().Content; !strings.Contains(view, "Rename") || !strings.Contains(view, "Old title") {
		t.Fatalf("rename editor is not labelled or prefilled:\n%s", view)
	}
	model = terminalManageUpdate(t, model, tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	for _, key := range []string{"Night shift"} {
		model = terminalManageUpdate(t, model, terminalManageKey(key))
	}
	_ = terminalManageSend(t, model, terminalManageKey("enter"))

	if len(ops.renames) != 1 || ops.renames[0].id != item.ContextID || ops.renames[0].title != "Night shift" {
		t.Fatalf("rename calls = %+v, want exact ID and friendly title", ops.renames)
	}
}

func TestTerminalManageRenamePreservesExistingLongValidTitle(t *testing.T) {
	title := strings.Repeat("a", 200)
	item := terminalManageTestItem("11111111-1111-4111-8111-111111111111", title, sessionstate.ContextActive)
	ops := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{item}}}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))

	model = terminalManageUpdate(t, model, terminalManageKey("e"))
	_ = terminalManageSend(t, model, terminalManageKey("enter"))
	if len(ops.renames) != 1 || ops.renames[0].title != title {
		t.Fatalf("unchanged long title was truncated: got %d bytes, want %d", len(ops.renames[0].title), len(title))
	}
}

func TestTerminalManageArchivesAndActivatesWithExactContextID(t *testing.T) {
	active := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Active", sessionstate.ContextActive)
	archived := terminalManageTestItem("22222222-2222-4222-8222-222222222222", "Archived", sessionstate.ContextArchived)
	ops := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{active, archived}, {active, archived}, {active, archived}}}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))

	model = terminalManageSend(t, model, terminalManageKey("a"))
	model = terminalManageUpdate(t, model, terminalManageKey("j"))
	_ = terminalManageSend(t, model, terminalManageKey("a"))

	if len(ops.stateChanges) != 2 {
		t.Fatalf("state changes = %+v, want archive then activate", ops.stateChanges)
	}
	if got := ops.stateChanges[0]; got.id != active.ContextID || got.state != sessionstate.ContextArchived {
		t.Fatalf("archive call = %+v", got)
	}
	if got := ops.stateChanges[1]; got.id != archived.ContextID || got.state != sessionstate.ContextActive {
		t.Fatalf("activate call = %+v", got)
	}
}

func TestTerminalManageArchiveKeepsCursorPositionForRepeatedArchive(t *testing.T) {
	first := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Alpha", sessionstate.ContextActive)
	selected := terminalManageTestItem("22222222-2222-4222-8222-222222222222", "Bravo", sessionstate.ContextActive)
	next := terminalManageTestItem("33333333-3333-4333-8333-333333333333", "Charlie", sessionstate.ContextActive)
	archived := selected
	archived.State = sessionstate.ContextArchived
	ops := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{
		{first, selected, next},
		{first, next, archived},
	}}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 80, Height: 24})
	model = terminalManageUpdate(t, model, terminalManageKey("j"))

	model = terminalManageSend(t, model, terminalManageKey("a"))
	item, ok := model.selected()
	if !ok || item.ContextID != next.ContextID || model.cursor != 1 {
		t.Fatalf("archive followed moved item instead of retaining row 1: selected=%+v cursor=%d", item, model.cursor)
	}

	_ = terminalManageSend(t, model, terminalManageKey("a"))
	if got := ops.stateChanges; len(got) != 2 || got[1].id != next.ContextID || got[1].state != sessionstate.ContextArchived {
		t.Fatalf("repeated archive targeted %+v, want Charlie archived", got)
	}
}

func TestTerminalManageActivateFollowsMovedContext(t *testing.T) {
	active := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Zulu", sessionstate.ContextActive)
	archived := terminalManageTestItem("22222222-2222-4222-8222-222222222222", "Alpha", sessionstate.ContextArchived)
	activated := archived
	activated.State = sessionstate.ContextActive
	ops := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{
		{active, archived},
		{active, activated},
	}}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))
	model = terminalManageUpdate(t, model, terminalManageKey("j"))

	model = terminalManageSend(t, model, terminalManageKey("a"))
	item, ok := model.selected()
	if !ok || item.ContextID != archived.ContextID || model.cursor != 0 {
		t.Fatalf("activate did not follow moved context: selected=%+v cursor=%d", item, model.cursor)
	}
}

func TestTerminalManageDeleteKeepsCursorPosition(t *testing.T) {
	first := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Alpha", sessionstate.ContextActive)
	deleted := terminalManageTestItem("22222222-2222-4222-8222-222222222222", "Bravo", sessionstate.ContextArchived)
	next := terminalManageTestItem("33333333-3333-4333-8333-333333333333", "Charlie", sessionstate.ContextActive)
	ops := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{
		{first, next, deleted},
		{first, next},
	}}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))
	model = terminalManageUpdate(t, model, terminalManageKey("j"))

	model = terminalManageUpdate(t, model, terminalManageKey("d"))
	model = terminalManageSend(t, model, terminalManageKey("y"))
	item, ok := model.selected()
	if !ok || item.ContextID != next.ContextID || model.cursor != 1 {
		t.Fatalf("delete did not retain row 1: selected=%+v cursor=%d", item, model.cursor)
	}
}

func TestTerminalManageRenameFollowsContextAcrossReordering(t *testing.T) {
	first := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Alpha", sessionstate.ContextActive)
	renamed := terminalManageTestItem("22222222-2222-4222-8222-222222222222", "Bravo", sessionstate.ContextActive)
	next := terminalManageTestItem("33333333-3333-4333-8333-333333333333", "Charlie", sessionstate.ContextActive)
	updated := renamed
	updated.Label = "Zulu"
	updated.Identity.Project = "Zulu"
	ops := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{
		{first, renamed, next},
		{first, next, updated},
	}}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))
	model = terminalManageUpdate(t, model, terminalManageKey("j"))
	model = terminalManageUpdate(t, model, terminalManageKey("e"))
	model = terminalManageUpdate(t, model, tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	model = terminalManageUpdate(t, model, terminalManageKey("Zulu"))

	model = terminalManageSend(t, model, terminalManageKey("enter"))
	item, ok := model.selected()
	if !ok || item.ContextID != renamed.ContextID || model.cursor != 2 {
		t.Fatalf("rename did not follow reordered context: selected=%+v cursor=%d", item, model.cursor)
	}
}

func TestTerminalManageArchivePreservesFilterViewport(t *testing.T) {
	first := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Alpha", sessionstate.ContextActive)
	selected := terminalManageTestItem("22222222-2222-4222-8222-222222222222", "Bravo", sessionstate.ContextActive)
	next := terminalManageTestItem("33333333-3333-4333-8333-333333333333", "Charlie", sessionstate.ContextActive)
	archived := selected
	archived.State = sessionstate.ContextArchived
	ops := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{
		{first, selected, next},
		{first, next, archived},
	}}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 80, Height: 24})
	model = terminalManageUpdate(t, model, terminalManageKey("j"))
	model = terminalManageUpdate(t, model, terminalManageKey("/"))
	model = terminalManageUpdate(t, model, terminalManageKey("a"))
	model = terminalManageUpdate(t, model, terminalManageKey("enter"))

	model = terminalManageSend(t, model, terminalManageKey("a"))
	item, ok := model.selected()
	if model.filter != "a" || len(model.visible) != 3 || !ok || item.ContextID != next.ContextID || model.cursor != 1 {
		t.Fatalf("archive lost filtered viewport: filter=%q visible=%v selected=%+v cursor=%d", model.filter, model.visible, item, model.cursor)
	}
	if view := model.View().Content; !strings.Contains(view, "Filter: a  [/] Edit or clear") {
		t.Fatalf("80x24 filtered viewport did not show its active filter:\n%s", view)
	}
}

func TestTerminalManagePurgeDialogIsolatesKeysAndRequiresExplicitYes(t *testing.T) {
	item := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Disposable terminal", sessionstate.ContextArchived)
	item.Cwd = "/work/disposable"
	ops := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{item}, {}}}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 80, Height: 24})

	model = terminalManageUpdate(t, model, terminalManageKey("d"))
	view := model.View().Content
	for _, want := range []string{"Delete Disposable terminal", "/work/disposable", "permanently", "[y] Delete"} {
		if !strings.Contains(view, want) {
			t.Fatalf("purge confirmation is missing %q:\n%s", want, view)
		}
	}
	model = terminalManageUpdate(t, model, terminalManageKey("j"))
	model = terminalManageUpdate(t, model, terminalManageKey("r"))
	if ops.listCalls != 1 || len(ops.purges) != 0 {
		t.Fatalf("modal leaked list/global keys: lists=%d purges=%v", ops.listCalls, ops.purges)
	}
	model = terminalManageUpdate(t, model, terminalManageKey("n"))
	if len(ops.purges) != 0 || strings.Contains(model.View().Content, "Delete Disposable terminal") {
		t.Fatalf("no did not cancel purge")
	}

	model = terminalManageUpdate(t, model, terminalManageKey("d"))
	_ = terminalManageSend(t, model, terminalManageKey("y"))
	if got := ops.purges; len(got) != 1 || got[0] != item.ContextID {
		t.Fatalf("purge calls = %v, want %s", got, item.ContextID)
	}
}

func TestTerminalManageBlocksQuitWhileOperationIsPending(t *testing.T) {
	model := newTerminalManageModel(&terminalManageTestOperations{})
	model.pending = true

	for _, key := range []tea.KeyPressMsg{
		{Code: 'c', Mod: tea.ModCtrl},
		terminalManageKey("q"),
	} {
		updated, command := terminalManageUpdateWithCommand(t, model, key)
		if command != nil || !updated.pending {
			t.Fatalf("pending operation accepted quit key %q: pending=%t command=%v", key.String(), updated.pending, command)
		}
	}
}

func TestTerminalManageShowsSuccessfulPurgeCleanupWarning(t *testing.T) {
	item := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Disposable terminal", sessionstate.ContextArchived)
	warning := "Context was purged, but stale presentation activity could not be removed"
	ops := &terminalManageTestOperations{
		snapshots:    [][]terminalInventoryResult{{item}, {}},
		purgeMessage: warning,
	}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))
	model = terminalManageUpdate(t, model, terminalManageKey("d"))
	model = terminalManageSend(t, model, terminalManageKey("y"))

	if view := model.View().Content; !strings.Contains(view, warning) {
		t.Fatalf("successful purge warning was discarded:\n%s", view)
	}
}

func TestTerminalManageCommandAdapterPurgesActivityAtomically(t *testing.T) {
	deps := testDependencies(t)
	registered := registerTestContext(t, deps)
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	message, err := (commandTerminalManageOperations{deps: deps}).Purge(t.Context(), registered.ID)
	if err != nil {
		t.Fatalf("purge terminal: %v", err)
	}
	if strings.Contains(message, "stale presentation activity") {
		t.Fatalf("atomic purge reported obsolete cleanup warning: %q", message)
	}
	if registry := loadTestRegistry(t, deps); len(registry.Contexts) != 0 {
		t.Fatalf("successful purge retained registry context: %+v", registry.Contexts)
	}
	activity, err := sessionstate.ReadTerminalActivitySnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := sessionstate.FindTerminalActivity(activity, registered.ID); exists {
		t.Fatalf("successful purge retained activity: %+v", activity)
	}
}

func TestTerminalManageErrorsRemainVisibleAndSuccessfulReloadKeepsSelection(t *testing.T) {
	first := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "First", sessionstate.ContextActive)
	second := terminalManageTestItem("22222222-2222-4222-8222-222222222222", "Second", sessionstate.ContextActive)
	ops := &terminalManageTestOperations{
		snapshots: [][]terminalInventoryResult{{first, second}, {second, first}},
		openErr:   errors.New("Sway socket is unavailable"),
	}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))
	model = terminalManageUpdate(t, model, terminalManageKey("j"))
	model = terminalManageSend(t, model, terminalManageKey("o"))
	if view := model.View().Content; !strings.Contains(view, "Sway socket is unavailable") {
		t.Fatalf("async operation error is not visible:\n%s", view)
	}

	model = terminalManageSend(t, model, terminalManageKey("a"))
	model = terminalManageUpdate(t, model, terminalManageKey("e"))
	model = terminalManageUpdate(t, model, tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	model = terminalManageUpdate(t, model, terminalManageKey("Retained selection"))
	_ = terminalManageSend(t, model, terminalManageKey("enter"))
	if len(ops.renames) != 1 || ops.renames[0].id != second.ContextID {
		t.Fatalf("selection drifted across reload: renames=%+v", ops.renames)
	}
}

func TestTerminalManageIgnoresObsoleteLoadsAndBlocksActionsWhileLoading(t *testing.T) {
	current := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Current", sessionstate.ContextActive)
	stale := terminalManageTestItem("22222222-2222-4222-8222-222222222222", "Stale", sessionstate.ContextActive)
	ops := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{current}}}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))

	updated, command := terminalManageUpdateWithCommand(t, model, terminalManageKey("r"))
	if command == nil || !updated.loading {
		t.Fatal("refresh did not begin an asynchronous load")
	}
	updated = terminalManageUpdate(t, updated, terminalManageKey("o"))
	if len(ops.opens) != 0 {
		t.Fatalf("open ran while inventory load was pending: %v", ops.opens)
	}
	updated = terminalManageUpdate(t, updated, terminalManageLoadedMsg{
		generation: updated.loadID - 1,
		snapshot:   terminalManageSnapshot{items: []terminalInventoryResult{stale}},
	})
	if !updated.loading || strings.Contains(updated.View().Content, "Stale") {
		t.Fatalf("obsolete load replaced current state:\n%s", updated.View().Content)
	}
	updated = terminalManageRunCommand(t, updated, command)
	if updated.loading || !strings.Contains(updated.View().Content, "Current") {
		t.Fatalf("current load was not applied:\n%s", updated.View().Content)
	}
}

func TestTerminalManageResponsiveRenderingAndNoColorNeverPanics(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	item := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "A very long friendly terminal title", sessionstate.ContextActive)
	item.Cwd = "/a/path/with/a/very/long/component/that/must/not/overlap/the/quit/help/path"
	item.ArchivedAt = terminalManageTestArchivedAt()
	model := terminalManageRunInit(t, newTerminalManageModel(&terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{item}}}))
	interactive := terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 80, Height: 24})
	interactive = terminalManageUpdate(t, interactive, terminalManageKey("e"))
	if view := interactive.View().Content; strings.Contains(view, "\x1b[") {
		t.Fatalf("NO_COLOR rename editor contains ANSI styling: %q", view)
	}

	for _, size := range []tea.WindowSizeMsg{{Width: 120, Height: 30}, {Width: 80, Height: 24}, {Width: 45, Height: 8}, {Width: 1, Height: 1}} {
		model = terminalManageUpdate(t, model, size)
		view := model.View().Content
		if strings.Contains(view, "\x1b[") {
			t.Fatalf("NO_COLOR render contains ANSI styling at %+v: %q", size, view)
		}
		if !strings.Contains(view, "q") {
			t.Fatalf("render at %+v lost its quit recovery path:\n%s", size, view)
		}
	}
}

func TestTerminalManageRendersGradientFrameAndPulsingSelection(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	item := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Color pulse", sessionstate.ContextActive)
	model := terminalManageRunInit(t, newTerminalManageModel(&terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{item}}}))
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 100, Height: 24})
	first := model.View().Content
	if !strings.Contains(first, "╭") || !strings.Contains(first, "╯") || !strings.Contains(first, "\x1b[") {
		t.Fatalf("color TUI is missing its styled frame:\n%s", first)
	}

	model.animate = true
	updated, next := terminalManageUpdateWithCommand(t, model, terminalManagePulseMsg{})
	if next == nil || updated.pulsePhase == model.pulsePhase {
		t.Fatalf("pulse did not advance or schedule its next frame: before=%d after=%d command=%v", model.pulsePhase, updated.pulsePhase, next)
	}
	if second := updated.View().Content; second == first || !strings.Contains(second, "Color pulse") {
		t.Fatalf("pulse did not recolor while preserving selection content:\n%s", second)
	}
}

func TestTerminalManageLongInventoryKeepsTheSelectionVisible(t *testing.T) {
	items := make([]terminalInventoryResult, 20)
	for index := range items {
		id := sessionstate.ContextID(fmt.Sprintf("%08d-1111-4111-8111-111111111111", index+1))
		items[index] = terminalManageTestItem(id, fmt.Sprintf("Terminal %02d", index+1), sessionstate.ContextActive)
	}
	ops := &terminalManageTestOperations{snapshots: [][]terminalInventoryResult{items}}
	model := terminalManageRunInit(t, newTerminalManageModel(ops))
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 80, Height: 24})
	for range 15 {
		model = terminalManageUpdate(t, model, terminalManageKey("j"))
	}
	view := model.View().Content
	if !strings.Contains(view, "Terminal 16") || !strings.Contains(view, "↑") || !strings.Contains(view, "↓") {
		t.Fatalf("long inventory did not keep selected row and scroll affordances visible:\n%s", view)
	}
	model = terminalManageUpdate(t, model, terminalManageKey("d"))
	if view := model.View().Content; !strings.Contains(view, "[y] Delete") {
		t.Fatalf("long inventory hid the destructive confirmation at 80x24:\n%s", view)
	}
	model = terminalManageUpdate(t, model, terminalManageKey("n"))
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 100, Height: 24})
	model = terminalManageUpdate(t, model, terminalManageKey("d"))
	if view := model.View().Content; !strings.Contains(view, "[y] Delete") {
		t.Fatalf("wide long inventory hid the destructive confirmation at 100x24:\n%s", view)
	}
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 45, Height: 8})
	_ = terminalManageSend(t, model, terminalManageKey("y"))
	if len(ops.purges) != 0 {
		t.Fatalf("hidden destructive confirmation still purged: %v", ops.purges)
	}
}

func TestTerminalManagePurgePromptRemainsActionableAtMinimumSupportedSize(t *testing.T) {
	item := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Minimum size", sessionstate.ContextArchived)
	model := terminalManageRunInit(t, newTerminalManageModel(&terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{item}}}))
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 48, Height: 16})
	model = terminalManageUpdate(t, model, terminalManageKey("d"))
	view := model.View().Content
	for _, want := range []string{"Delete Minimum size", "permanently", "[y] Delete"} {
		if !strings.Contains(view, want) {
			t.Fatalf("minimum-size purge confirmation is missing %q:\n%s", want, view)
		}
	}
}

func TestTerminalManageMinimumHeightWideTerminalKeepsHelpAndFeedbackVisible(t *testing.T) {
	item := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Wide minimum", sessionstate.ContextActive)
	model := terminalManageRunInit(t, newTerminalManageModel(&terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{item}}}))
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 100, Height: 16})

	help := terminalManageUpdate(t, model, terminalManageKey("?"))
	if view := help.View().Content; !strings.Contains(view, "[d] Delete permanently") || !strings.Contains(view, "[m] Migrate old state") {
		t.Fatalf("100x16 help lost its final shortcut row:\n%s", view)
	}

	model.err = errors.New("state unavailable")
	if view := model.View().Content; !strings.Contains(view, "Error: State unavailable") {
		t.Fatalf("100x16 render hid operation error:\n%s", view)
	}
	model.err = nil
	model.status = "Terminal opened"
	if view := model.View().Content; !strings.Contains(view, "Terminal opened") {
		t.Fatalf("100x16 render hid operation status:\n%s", view)
	}
}

func TestTerminalManageHelpKeysMatchVisibleBehavior(t *testing.T) {
	item := terminalManageTestItem("11111111-1111-4111-8111-111111111111", "Help", sessionstate.ContextActive)
	model := terminalManageRunInit(t, newTerminalManageModel(&terminalManageTestOperations{snapshots: [][]terminalInventoryResult{{item}}}))
	model = terminalManageUpdate(t, model, terminalManageKey("?"))
	if view := model.View().Content; !strings.Contains(view, "[Esc/?] Close help") || !strings.Contains(view, "[q] Quit") {
		t.Fatalf("help does not describe its modal keys:\n%s", view)
	}
	updated, command := terminalManageUpdateWithCommand(t, model, terminalManageKey("q"))
	if command == nil || updated.mode != terminalManageHelpMode {
		t.Fatalf("q did not quit directly from help: mode=%v command=%v", updated.mode, command)
	}
}

func TestTerminalManageCommandPassesStreamsConfigAndSocketToInjectedRunner(t *testing.T) {
	deps := testDependencies(t)
	input := strings.NewReader("input")
	var output bytes.Buffer
	configPath := "/tmp/sway-session-test-config.toml"
	called := 0
	deps.runTerminalManage = func(
		ctx context.Context,
		stdin io.Reader,
		stdout io.Writer,
		config string,
		socket string,
		_ dependencies,
	) error {
		called++
		if ctx == nil || stdin != input || stdout != &output || config != configPath || socket != "/run/user/1000/sway.sock" {
			t.Fatalf("runner received ctx=%v stdin=%T stdout=%T config=%q socket=%q", ctx, stdin, stdout, config, socket)
		}
		return nil
	}
	var stderr bytes.Buffer
	code := runWith([]string{
		"--config", configPath, "terminal", "manage", "--socket", "/run/user/1000/sway.sock",
	}, input, &output, &stderr, deps)
	if code != exitSuccess || called != 1 || output.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("manage dispatch code=%d called=%d stdout=%q stderr=%q", code, called, output.String(), stderr.String())
	}
}

func TestTerminalManageCommandRejectsJSONAndUnexpectedArgumentsBeforeRunning(t *testing.T) {
	deps := testDependencies(t)
	deps.runTerminalManage = func(context.Context, io.Reader, io.Writer, string, string, dependencies) error {
		t.Fatal("invalid manage command reached the TUI runner")
		return nil
	}
	for _, arguments := range [][]string{
		{"--json", "terminal", "manage"},
		{"terminal", "manage", "unexpected"},
		{"terminal", "manage", "--socket", "relative.sock"},
	} {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if code := runWith(arguments, strings.NewReader(""), &stdout, &stderr, deps); code != exitUsage && code != exitOperation {
			t.Fatalf("arguments %v returned %d, stderr=%q", arguments, code, stderr.String())
		}
	}
}

func TestTerminalManagerRejectsNonTTYStreamsWithoutControlOutput(t *testing.T) {
	var output bytes.Buffer
	err := runTerminalManager(t.Context(), strings.NewReader(""), &output, "", "", testDependencies(t))
	if err == nil || !strings.Contains(err.Error(), "stdin") {
		t.Fatalf("non-TTY input error = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("non-TTY rejection wrote terminal controls: %q", output.String())
	}
}

func terminalManageRunInit(t *testing.T, model terminalManageModel) terminalManageModel {
	t.Helper()
	return terminalManageRunCommand(t, model, model.Init())
}

func terminalManageRunCommand(t *testing.T, model terminalManageModel, command tea.Cmd) terminalManageModel {
	t.Helper()
	for command != nil {
		model, command = terminalManageUpdateWithCommand(t, model, command())
	}
	return model
}

func terminalManageSend(t *testing.T, model terminalManageModel, message tea.Msg) terminalManageModel {
	t.Helper()
	updated, command := terminalManageUpdateWithCommand(t, model, message)
	return terminalManageRunCommand(t, updated, command)
}

func terminalManageUpdate(t *testing.T, model terminalManageModel, message tea.Msg) terminalManageModel {
	t.Helper()
	updated, _ := terminalManageUpdateWithCommand(t, model, message)
	return updated
}

func terminalManageUpdateWithCommand(t *testing.T, model terminalManageModel, message tea.Msg) (terminalManageModel, tea.Cmd) {
	t.Helper()
	updated, command := model.Update(message)
	managed, ok := updated.(terminalManageModel)
	if !ok {
		t.Fatalf("Update returned %T, want terminalManageModel", updated)
	}
	return managed, command
}

func terminalManageKey(text string) tea.KeyPressMsg {
	switch text {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	default:
		return tea.KeyPressMsg{Text: text, Code: rune(text[0])}
	}
}

func terminalManageTestItem(id sessionstate.ContextID, title string, state sessionstate.ContextState) terminalInventoryResult {
	return terminalInventoryResult{
		ContextID: id,
		Label:     title,
		Identity:  terminalIdentityResult{Kind: sessionstate.TerminalIdentityProject, Project: title},
		Adapter:   sessionstate.TerminalAdapterAlacritty,
		Manager:   sessionstate.TerminalSessionManagerHerdr,
		State:     state,
		Session:   "terminal-" + string(id)[:8],
		Cwd:       "/work/" + strings.ToLower(strings.ReplaceAll(title, " ", "-")),
	}
}

func terminalManageTestArchivedAt() *time.Time {
	value := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	return &value
}

type terminalManageTestOperations struct {
	snapshots    [][]terminalInventoryResult
	windows      []map[sessionstate.ContextID]terminalWindowPresence
	windowErrors []error
	listCalls    int
	listErr      error
	openErr      error
	setStateErr  error
	renameErr    error
	purgeErr     error
	purgeMessage string
	migrateErr   error
	migrateCalls int
	opens        []sessionstate.ContextID
	stateChanges []terminalManageTestStateChange
	renames      []terminalManageTestRename
	purges       []sessionstate.ContextID
}

type terminalManageTestStateChange struct {
	id    sessionstate.ContextID
	state sessionstate.ContextState
}

type terminalManageTestRename struct {
	id    sessionstate.ContextID
	title string
}

func (operations *terminalManageTestOperations) Load(context.Context, string) (terminalManageSnapshot, error) {
	operations.listCalls++
	if operations.listErr != nil {
		return terminalManageSnapshot{}, operations.listErr
	}
	if len(operations.snapshots) == 0 {
		return terminalManageSnapshot{}, nil
	}
	index := min(operations.listCalls-1, len(operations.snapshots)-1)
	items := append([]terminalInventoryResult(nil), operations.snapshots[index]...)
	windows := terminalManageUnknownWindows(items)
	if len(operations.windows) != 0 {
		windowIndex := min(operations.listCalls-1, len(operations.windows)-1)
		for id, presence := range operations.windows[windowIndex] {
			windows[id] = presence
		}
	}
	var windowError error
	if len(operations.windowErrors) != 0 {
		windowError = operations.windowErrors[min(operations.listCalls-1, len(operations.windowErrors)-1)]
	}
	return terminalManageSnapshot{items: items, windows: windows, windowError: windowError}, nil
}

func (operations *terminalManageTestOperations) Open(_ context.Context, id sessionstate.ContextID, _ string) error {
	operations.opens = append(operations.opens, id)
	return operations.openErr
}

func (operations *terminalManageTestOperations) SetState(_ context.Context, id sessionstate.ContextID, state sessionstate.ContextState) error {
	operations.stateChanges = append(operations.stateChanges, terminalManageTestStateChange{id: id, state: state})
	return operations.setStateErr
}

func (operations *terminalManageTestOperations) Rename(_ context.Context, id sessionstate.ContextID, title string) error {
	operations.renames = append(operations.renames, terminalManageTestRename{id: id, title: title})
	return operations.renameErr
}

func (operations *terminalManageTestOperations) Purge(_ context.Context, id sessionstate.ContextID) (string, error) {
	operations.purges = append(operations.purges, id)
	return operations.purgeMessage, operations.purgeErr
}

func (operations *terminalManageTestOperations) Migrate(context.Context) (string, error) {
	operations.migrateCalls++
	return "Migrated legacy state to SQLite", operations.migrateErr
}
