package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/statefile"
)

type environmentTestOperations struct {
	terminalManageTestOperations
	snapshot      terminalManageEnvironmentSnapshot
	saves         []sessionstate.WorkEnvironment
	deleted       []sessionstate.EnvironmentID
	saveErr       error
	saveCandidate sessionstate.WorkEnvironment
	requests      []sessionstate.EnvironmentID
	overrides     []sessionstate.ContextID
	canceled      int
}

func (operations *environmentTestOperations) LoadEnvironments(context.Context, string) (terminalManageEnvironmentSnapshot, error) {
	return operations.snapshot, nil
}
func (operations *environmentTestOperations) SaveEnvironment(_ context.Context, definition sessionstate.WorkEnvironment) (sessionstate.WorkEnvironment, error) {
	operations.saves = append(operations.saves, definition)
	if operations.saveErr != nil {
		return operations.saveCandidate, operations.saveErr
	}
	if definition.ID == "" {
		definition.ID = sessionstate.EnvironmentID("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	}
	definition.Revision++
	found := false
	for index, current := range operations.snapshot.environments {
		if current.ID == definition.ID {
			operations.snapshot.environments[index] = definition
			found = true
		}
	}
	if !found {
		operations.snapshot.environments = append(operations.snapshot.environments, definition)
	}
	return definition, nil
}
func (operations *environmentTestOperations) DeleteEnvironment(_ context.Context, id sessionstate.EnvironmentID, _ int64) error {
	operations.deleted = append(operations.deleted, id)
	operations.snapshot.environments = slices.DeleteFunc(operations.snapshot.environments, func(item sessionstate.WorkEnvironment) bool { return item.ID == id })
	return nil
}
func (operations *environmentTestOperations) PreviewEnvironment(_ context.Context, id sessionstate.EnvironmentID, primary sessionstate.ContextID, _ string) (sessionstate.ReachPreview, error) {
	return sessionstate.ReachPreview{EnvironmentID: id, Primary: primary, Ready: true, Members: []sessionstate.ReachMemberResult{{ContextID: environmentContext1, Status: sessionstate.ReachStatus("would_focus"), Reason: "existing exact anchor"}}}, nil
}
func (operations *environmentTestOperations) ReachEnvironment(ctx context.Context, id sessionstate.EnvironmentID, primary sessionstate.ContextID, _ string) (sessionstate.ReachResult, error) {
	if ctx.Err() != nil {
		operations.canceled++
		return sessionstate.ReachResult{}, ctx.Err()
	}
	operations.requests = append(operations.requests, id)
	operations.overrides = append(operations.overrides, primary)
	return sessionstate.ReachResult{EnvironmentID: id, Primary: primary, Members: []sessionstate.ReachMemberResult{{ContextID: environmentContext1, Status: sessionstate.ReachStatus("blocked"), Reason: "safe background launch unavailable"}}}, nil
}

const environmentContext1 = sessionstate.ContextID("11111111-1111-4111-8111-111111111111")
const environmentContext2 = sessionstate.ContextID("22222222-2222-4222-8222-222222222222")

func environmentFixture(t *testing.T) (terminalManageModel, *environmentTestOperations) {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	operations := &environmentTestOperations{snapshot: terminalManageEnvironmentSnapshot{
		environments: []sessionstate.WorkEnvironment{{ID: sessionstate.EnvironmentID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"), Name: "Daily work", Revision: 1, Members: []sessionstate.ContextID{environmentContext1}, Primary: environmentContext1}, {ID: sessionstate.EnvironmentID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), Name: "Other work", Revision: 1, Members: []sessionstate.ContextID{environmentContext2}, Primary: environmentContext2}},
		contexts:     []sessionstate.Context{{ID: environmentContext1, Label: "Agent and shell", State: sessionstate.ContextActive}, {ID: environmentContext2, Label: "Shared browser", State: sessionstate.ContextArchived}},
		terminals:    []terminalInventoryResult{terminalManageTestItem(environmentContext1, "Agent and shell", sessionstate.ContextActive)},
		applications: []sessionstate.ApplicationInventoryItem{{ContextID: environmentContext2, Label: "Shared browser", State: sessionstate.ContextArchived, Presence: sessionstate.ApplicationPresence{State: "unknown"}}},
	}}
	model := newTerminalManageModel(operations)
	model = terminalManageRunInit(t, model)
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 80, Height: 24})
	if !strings.Contains(model.render(), "[Tab] Environments") {
		t.Fatal("empty terminal view has no environment entry")
	}
	model = terminalManageSend(t, model, tea.KeyPressMsg{Code: tea.KeyTab})
	if !model.environment.active || !model.environment.loaded {
		t.Fatal("environment view did not load")
	}
	return model, operations
}
func TestEnvironmentSearchCancelAndModalTyping(t *testing.T) {
	model, operations := environmentFixture(t)
	model.environment.filter = "Daily"
	model.environment.restoreSelection()
	model = terminalManageUpdate(t, model, terminalManageKey("/"))
	model.environment.input.SetValue("Shared browser")
	model = terminalManageUpdate(t, model, terminalManageKey("esc"))
	if model.environment.filter != "Daily" {
		t.Fatal("cancel discarded prior search")
	}
	model = terminalManageUpdate(t, model, terminalManageKey("e"))
	id := model.environment.draft.ID
	for _, key := range []string{"q", "d", "a", "m", "p", "v", "n", "t"} {
		model = terminalManageUpdate(t, model, terminalManageKey(key))
	}
	if model.environment.mode != environmentName || model.environment.draft.ID != id || len(operations.deleted) != 0 || len(operations.saves) != 0 {
		t.Fatal("text input routed into an action")
	}
	if !strings.HasSuffix(model.environment.input.Value(), "qdampvnt") {
		t.Fatalf("ordinary action letters lost: %q", model.environment.input.Value())
	}
	model = terminalManageUpdate(t, model, terminalManageKey("esc"))
	model = terminalManageUpdate(t, model, tea.KeyPressMsg{Code: tea.KeyTab})
	if model.environment.active || model.filter != "" {
		t.Fatal("view switch lost terminal state")
	}
	model = terminalManageSend(t, model, tea.KeyPressMsg{Code: tea.KeyTab})
	if model.environment.filter != "Daily" {
		t.Fatal("view switch lost environment filter")
	}
}
func TestEnvironmentMembershipSavesSingleActionsAndPrimaryRemoval(t *testing.T) {
	model, operations := environmentFixture(t)
	model = terminalManageUpdate(t, model, terminalManageKey("v"))
	original := model.environment.draft.ID
	model = terminalManageUpdate(t, model, terminalManageKey("a"))
	model = terminalManageUpdate(t, model, terminalManageKey("esc"))
	if len(operations.saves) != 0 {
		t.Fatal("cancel picker wrote state")
	}
	model = terminalManageUpdate(t, model, terminalManageKey("a"))
	model = terminalManageSend(t, model, terminalManageKey("enter"))
	if len(operations.saves) != 1 || !slices.Equal(operations.saves[0].Members, []sessionstate.ContextID{environmentContext1, environmentContext2}) || model.environment.draft.Revision != 2 || model.environment.draft.ID != original {
		t.Fatal("add did not save exact membership/revision")
	}
	model = terminalManageSend(t, model, terminalManageKey("p"))
	if len(operations.saves) != 2 || operations.saves[1].Primary != environmentContext2 || operations.saves[1].Revision != 2 {
		t.Fatal("primary update not a distinct CAS action")
	}
	model = terminalManageSend(t, model, terminalManageKey("d"))
	if len(operations.saves) != 3 || operations.saves[2].Primary != "" || len(model.environment.draft.Members) != 1 {
		t.Fatal("primary removal must clear designation")
	}
	if len(operations.opens) != 0 || len(operations.stateChanges) != 0 || len(operations.purges) != 0 || len(operations.deleted) > 0 {
		t.Fatal("membership changed lifecycle")
	}
	model = terminalManageUpdate(t, model, terminalManageKey("esc"))
	model = terminalManageUpdate(t, model, terminalManageKey("d"))
	if !strings.Contains(model.render(), "Contexts and windows remain") {
		t.Fatal("environment delete reused terminal purge wording")
	}
	model = terminalManageSend(t, model, terminalManageKey("y"))
	if len(operations.deleted) != 1 {
		t.Fatal("definition not deleted")
	}
}
func TestEnvironmentCreateNameAndConflictKeepTargets(t *testing.T) {
	model, operations := environmentFixture(t)
	model = terminalManageUpdate(t, model, terminalManageKey("n"))
	model.environment.input.SetValue("New environment")
	model = terminalManageSend(t, model, terminalManageKey("enter"))
	if len(operations.saves) != 1 || len(operations.saves[0].Members) != 0 || operations.saves[0].Name != "New environment" {
		t.Fatal("create must save an empty definition")
	}
	model = terminalManageUpdate(t, model, terminalManageKey("v"))
	id := model.environment.draft.ID
	revision := model.environment.draft.Revision
	operations.saveErr = errors.New("definition revision changed; refresh before editing")
	model = terminalManageUpdate(t, model, terminalManageKey("a"))
	model = terminalManageSend(t, model, terminalManageKey("enter"))
	if model.environment.err == nil || model.environment.mode != environmentMembers || model.environment.draft.ID != id || model.environment.draft.Revision != revision || len(model.environment.draft.Members) != 0 {
		t.Fatal("failed CAS changed target or retained unsaved membership")
	}
}
func TestEnvironmentReachSupersedesPendingAndDoesNotHijackEditor(t *testing.T) {
	model, operations := environmentFixture(t)
	model, first := terminalManageUpdateWithCommand(t, model, terminalManageKey("enter"))
	firstID := model.environment.reachID
	if !model.environment.reachPending || model.pending {
		t.Fatal("Reach reused blocking terminal pending")
	}
	model = terminalManageUpdate(t, model, terminalManageKey("j"))
	selected := model.environment.selectedID
	model, second := terminalManageUpdateWithCommand(t, model, terminalManageKey("enter"))
	if model.environment.reachID == firstID {
		t.Fatal("new Reach failed to supersede")
	}
	model = terminalManageRunCommand(t, model, first)
	if operations.canceled != 1 || !model.environment.reachPending {
		t.Fatal("superseded response changed active operation")
	}
	model = terminalManageUpdate(t, model, terminalManageKey("e"))
	model.environment.input.SetValue("Unsubmitted name")
	model = terminalManageRunCommand(t, model, second)
	if model.environment.mode != environmentName || model.environment.input.Value() != "Unsubmitted name" || model.environment.selectedID != selected {
		t.Fatal("late Reach overwrote editor/selection")
	}
	if len(operations.requests) != 1 || operations.requests[0] != selected || operations.overrides[0] != "" {
		t.Fatal("Reach did not use exact identity")
	}
	model = terminalManageUpdate(t, model, terminalManageKey("esc"))
	model = terminalManageUpdate(t, model, terminalManageKey("u"))
	if !strings.Contains(model.render(), "blocked") {
		t.Fatal("backend unsupported launch result hidden")
	}
}
func TestEnvironmentPreviewIsFreshIntentAndStaleResponseIsIgnored(t *testing.T) {
	model, operations := environmentFixture(t)
	model, preview := terminalManageUpdateWithCommand(t, model, terminalManageKey("p"))
	model = terminalManageUpdate(t, model, terminalManageKey("e"))
	model = terminalManageRunCommand(t, model, preview)
	if model.environment.mode != environmentName {
		t.Fatal("preview overwrote active name editor")
	}
	model = terminalManageUpdate(t, model, terminalManageKey("esc"))
	model = terminalManageSend(t, model, terminalManageKey("p"))
	if model.environment.mode != environmentPreview {
		t.Fatal("preview unavailable")
	}
	model = terminalManageSend(t, model, terminalManageKey("enter"))
	if len(operations.requests) != 1 {
		t.Fatal("preview Enter did not request fresh Reach")
	}
}
func TestEnvironmentNarrowReportsScrollableAndApplicationsOmitTerminalFacts(t *testing.T) {
	model, _ := environmentFixture(t)
	model = terminalManageUpdate(t, model, terminalManageKey("j"))
	model = terminalManageUpdate(t, model, terminalManageKey("i"))
	if strings.Contains(model.render(), "Herdr:") || strings.Contains(model.render(), "Agent:") {
		t.Fatal("application shown as unknown terminal")
	}
	for _, size := range []tea.WindowSizeMsg{{Width: 48, Height: 16}, {Width: 58, Height: 18}, {Width: 80, Height: 24}, {Width: 100, Height: 22}, {Width: 120, Height: 30}} {
		model = terminalManageUpdate(t, model, size)
		model.environment.mode = environmentResults
		model.environment.report = []string{"begin " + strings.Repeat("长 label ", 60) + " FINAL REASON"}
		model.environment.scroll = 0
		var seen strings.Builder
		for range 100 {
			view := model.render()
			seen.WriteString(view)
			if len(strings.Split(view, "\n")) > size.Height {
				t.Fatal("view exceeded height")
			}
			for _, line := range strings.Split(view, "\n") {
				if ansi.StringWidth(line) > size.Width {
					t.Fatal("view exceeded width")
				}
			}
			if strings.Contains(view, "\x1b[") {
				t.Fatal("NO_COLOR styled output")
			}
			model = terminalManageUpdate(t, model, terminalManageKey("j"))
		}
		if !strings.Contains(seen.String(), "FINAL REASON") {
			t.Fatalf("final outcome inaccessible at %+v", size)
		}
		model.environment.mode = environmentList
		view := model.render()
		if !strings.Contains(view, "[Tab] Terminals") || !strings.Contains(view, "[q] Quit") {
			t.Fatalf("recovery actions clipped at %+v: %s", size, view)
		}
	}
}

type environmentObservingTestOperations struct {
	*environmentTestOperations
	observe func(context.Context, terminalManageEnvironmentSnapshot) (terminalManageEnvironmentSnapshot, error)
}

func (operations *environmentObservingTestOperations) ObserveEnvironments(ctx context.Context, _ string, snapshot terminalManageEnvironmentSnapshot) (terminalManageEnvironmentSnapshot, error) {
	return operations.observe(ctx, snapshot)
}

func TestEnvironmentTwoPhaseObservationRetainsTypingAndRejectsOldGeneration(t *testing.T) {
	model, base := environmentFixture(t)
	calls := 0
	operations := &environmentObservingTestOperations{environmentTestOperations: base, observe: func(ctx context.Context, snapshot terminalManageEnvironmentSnapshot) (terminalManageEnvironmentSnapshot, error) {
		calls++
		if ctx.Err() != nil {
			return snapshot, ctx.Err()
		}
		snapshot.terminals[0].WindowPresence = "present"
		return snapshot, nil
	}}
	model.operations = operations
	old := model.environmentObserve()
	if !model.environment.observing {
		t.Fatal("live probe not started")
	}
	model = terminalManageUpdate(t, model, terminalManageKey("e"))
	model.environment.input.SetValue("Unsubmitted name")
	generation := model.environment.loadID
	observationID := model.environment.observationID
	snapshot := cloneEnvironmentObservations(model.environment.snapshot)
	snapshot.terminals[0].WindowPresence = "present"
	model = terminalManageUpdate(t, model, terminalManageEnvironmentObservedMsg{generation, observationID, snapshot, nil})
	if model.environment.mode != environmentName || model.environment.input.Value() != "Unsubmitted name" {
		t.Fatal("live probe stole text input")
	}
	model = terminalManageUpdate(t, model, terminalManageKey("esc"))
	model = terminalManageSend(t, model, terminalManageKey("r"))
	if model.environment.snapshot.terminals[0].WindowPresence != "present" {
		t.Fatal("new live probe failed")
	}
	model = terminalManageRunCommand(t, model, old)
	if calls != 2 || model.environment.observationErr != nil || model.environment.snapshot.terminals[0].WindowPresence != "present" {
		t.Fatal("obsolete canceled probe changed new observations")
	}
	model.environmentObserve()
	currentID := model.environment.observationID
	model = terminalManageUpdate(t, model, terminalManageEnvironmentObservedMsg{model.environment.loadID, currentID, terminalManageEnvironmentSnapshot{}, errors.New("offline")})
	if model.environment.err != nil || model.environment.observationErr == nil || len(model.environment.snapshot.environments) != 2 {
		t.Fatal("live failure became saved-state failure")
	}
	model.environmentObserve()
	model = terminalManageUpdate(t, model, tea.KeyPressMsg{Code: tea.KeyTab})
	if model.environment.observing {
		t.Fatal("leaving environment view retained probe")
	}
}

func TestEnvironmentRefreshMarksRetainedObservationsUnknown(t *testing.T) {
	model, _ := environmentFixture(t)
	model.environment.snapshot.terminals[0].WindowPresence = "present"
	model.environment.snapshot.applications[0].Presence = sessionstate.ApplicationPresence{State: "present", Anchor: &sessionstate.ApplicationPresenceAnchor{ContainerID: 42}}
	command := model.environmentLoad()
	if command == nil || model.environment.snapshot.terminals[0].WindowPresence != "unknown" || model.environment.snapshot.applications[0].Presence.Anchor != nil {
		t.Fatal("refresh retained actionable old window evidence")
	}
}

func TestEnvironmentPartialErrorRetainsOutcomesAndTerminalTypingDoesNotCancelReach(t *testing.T) {
	model, _ := environmentFixture(t)
	model.environment.reachID = 3
	model.environment.reachPending = true
	model = terminalManageUpdate(t, model, terminalManageEnvironmentReachedMsg{generation: 3, id: model.environment.selectedID, result: sessionstate.ReachResult{Members: []sessionstate.ReachMemberResult{{ContextID: environmentContext1, Status: sessionstate.ReachFocused, Reason: "already focused"}, {ContextID: environmentContext2, Status: sessionstate.ReachFailed, Reason: "compositor disconnected"}}}, err: errors.New("partial failure")})
	if len(model.environment.results) < 3 || model.environment.err == nil {
		t.Fatal("partial error discarded per-member evidence")
	}
	model.environment.active = false
	model.environment.reachPending = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model.environment.reachStop = cancel
	model.mode = terminalManageRenameMode
	model.renameID = environmentContext1
	model.input.SetValue("draft")
	model = terminalManageUpdate(t, model, terminalManageKey("q"))
	if ctx.Err() != nil || !model.environment.reachPending {
		t.Fatal("terminal name input canceled background Reach")
	}
}

func TestEnvironmentUnknownCommitPinsCandidateAndRequiresInspection(t *testing.T) {
	model, operations := environmentFixture(t)
	candidate := sessionstate.WorkEnvironment{ID: sessionstate.EnvironmentID("cccccccc-cccc-4ccc-8ccc-cccccccccccc"), Name: "Uncertain save", Revision: 1, Members: []sessionstate.ContextID{}}
	operations.saveCandidate = candidate
	operations.saveErr = &statefile.CommitOutcomeUnknownError{Cause: errors.New("lost commit acknowledgement")}
	operations.snapshot.environments = append(operations.snapshot.environments, candidate)
	model = terminalManageUpdate(t, model, terminalManageKey("n"))
	model.environment.input.SetValue(candidate.Name)
	model = terminalManageSend(t, model, terminalManageKey("enter"))
	if model.environment.mode != environmentUncertain || model.environment.draft.ID != candidate.ID || model.environment.draft.Revision != 1 || !model.environment.uncertainLoaded {
		t.Fatal("unknown commit lost candidate or failed to reconcile exact identity")
	}
	for _, key := range []string{"enter", "n", "a", "d", "p"} {
		model = terminalManageUpdate(t, model, terminalManageKey(key))
	}
	if len(operations.saves) != 1 || len(operations.deleted) != 0 {
		t.Fatal("uncertain outcome allowed a blind mutation retry")
	}
	view := model.render()
	if !strings.Contains(view, "uncertain") || !strings.Contains(view, string(candidate.ID)) || strings.Contains(view, "Definition saved;") {
		t.Fatalf("uncertain save misreported: %s", view)
	}
	model = terminalManageUpdate(t, model, terminalManageKey("esc"))
	if model.environment.mode != environmentList || model.environment.selectedID != candidate.ID {
		t.Fatal("inspection did not return to the exact candidate")
	}
	model = terminalManageUpdate(t, model, terminalManageKey("e"))
	if model.environment.draft.ID != candidate.ID {
		t.Fatal("subsequent edit became another creation")
	}
	model = terminalManageUpdate(t, model, terminalManageKey("esc"))
	if len(operations.saves) != 1 {
		t.Fatal("canceling reconciliation editor wrote again")
	}
}

func TestEnvironmentLongPickerKeepsInputAndPinnedEditHeader(t *testing.T) {
	model, operations := environmentFixture(t)
	for index := range 20 {
		operations.snapshot.contexts = append(operations.snapshot.contexts, sessionstate.Context{ID: sessionstate.ContextID(fmt.Sprintf("%08d-3333-4333-8333-333333333333", index+100)), Label: "Shared browser", State: sessionstate.ContextArchived})
	}
	model.environment.snapshot = operations.snapshot
	model.environment.indexContexts()
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 48, Height: 16})
	model = terminalManageUpdate(t, model, terminalManageKey("v"))
	model = terminalManageUpdate(t, model, terminalManageKey("a"))
	for range 15 {
		model = terminalManageUpdate(t, model, terminalManageKey("down"))
	}
	if !strings.Contains(model.render(), "> │") {
		t.Fatalf("scrolling picker hid its text input: %s", model.render())
	}
	model = terminalManageUpdate(t, model, terminalManageKey("esc"))
	id := model.environment.draft.ID
	snapshot := operations.snapshot
	snapshot.environments = snapshot.environments[1:]
	model = terminalManageUpdate(t, model, terminalManageEnvironmentLoadedMsg{generation: model.environment.loadID, snapshot: snapshot})
	if model.environment.draft.ID != id || !strings.Contains(model.render(), "Daily work") {
		t.Fatal("refresh replaced pinned member editor header/target")
	}
}

func TestEnvironmentPreviewAndOverrideNeverRetargetAfterRefresh(t *testing.T) {
	for _, mode := range []terminalManageEnvironmentMode{environmentPreview, environmentOverride} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			model, operations := environmentFixture(t)
			oldID := model.environment.selectedID
			if mode == environmentPreview {
				model = terminalManageSend(t, model, terminalManageKey("p"))
			} else {
				model = terminalManageUpdate(t, model, terminalManageKey("o"))
			}
			snapshot := operations.snapshot
			snapshot.environments = slices.Clone(snapshot.environments[1:])
			snapshot.environments[0].Members = []sessionstate.ContextID{environmentContext1}
			snapshot.environments[0].Primary = environmentContext1
			model = terminalManageUpdate(t, model, terminalManageEnvironmentLoadedMsg{generation: model.environment.loadID, snapshot: snapshot})
			if model.environment.selectedID == oldID {
				t.Fatal("fixture failed to move underlying selection")
			}
			model = terminalManageSend(t, model, terminalManageKey("enter"))
			if len(operations.requests) != 0 || model.environment.err == nil {
				t.Fatal("stale modal reached another environment")
			}
		})
	}
}

func TestEnvironmentEditorDoesNotConsumeTerminalLoadMessages(t *testing.T) {
	model, _ := environmentFixture(t)
	model = terminalManageUpdate(t, model, terminalManageKey("n"))
	model.environment.input.SetValue("Unsubmitted name")
	model.loading = true
	snapshot := terminalManageSnapshot{items: []terminalInventoryResult{terminalManageTestItem(environmentContext1, "Loaded terminal", sessionstate.ContextActive)}, windows: map[sessionstate.ContextID]terminalWindowPresence{environmentContext1: terminalWindowOpen}}
	model = terminalManageUpdate(t, model, terminalManageLoadedMsg{generation: model.loadID, snapshot: snapshot})
	if model.loading || len(model.items) != 1 || model.environment.mode != environmentName || model.environment.input.Value() != "Unsubmitted name" {
		t.Fatal("focused environment input consumed asynchronous terminal loading or lost its draft")
	}
}

func TestEnvironmentNoColorNameViewportTracksInsertionPoint(t *testing.T) {
	model, _ := environmentFixture(t)
	model = terminalManageUpdate(t, model, tea.WindowSizeMsg{Width: 48, Height: 16})
	model = terminalManageUpdate(t, model, terminalManageKey("n"))
	for _, prefix := range []string{strings.Repeat("x", 200), strings.Repeat("界", 100)} {
		model.environment.input.SetValue(prefix + "TAIL")
		model.environment.input.CursorEnd()
		view := model.render()
		if !strings.Contains(view, "TAIL│") {
			t.Fatalf("typed tail/caret hidden: %s", view)
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > 48 {
				t.Fatal("name viewport exceeded constrained width")
			}
		}
	}
	model.environment.input.SetValue(strings.Repeat("l", 80) + "LEFT" + "RIGHT" + strings.Repeat("r", 80))
	model.environment.input.SetCursor(84)
	view := model.render()
	if !strings.Contains(view, "LEFT│RIGHT") {
		t.Fatalf("caret does not reflect middle insertion point: %s", view)
	}
	if strings.Contains(view, "\x1b[") {
		t.Fatal("NO_COLOR viewport emitted ANSI styling")
	}
}

func TestEnvironmentBlockedNewReachStillSupersedesOlderRequest(t *testing.T) {
	model, operations := environmentFixture(t)
	model, old := terminalManageUpdateWithCommand(t, model, terminalManageKey("enter"))
	oldID := model.environment.reachID
	model = terminalManageUpdate(t, model, terminalManageKey("j"))
	for index := range model.environment.snapshot.environments {
		if model.environment.snapshot.environments[index].ID == model.environment.selectedID {
			model.environment.snapshot.environments[index].Primary = ""
		}
	}
	model, next := terminalManageUpdateWithCommand(t, model, terminalManageKey("enter"))
	if next != nil || model.environment.err == nil || model.environment.reachPending || model.environment.reachID == oldID {
		t.Fatal("blocked new primary left older focus authority active")
	}
	model = terminalManageRunCommand(t, model, old)
	if operations.canceled != 1 || len(operations.requests) != 0 || model.environment.reachPending {
		t.Fatal("older Reach executed after superseding blocked intent")
	}
}
