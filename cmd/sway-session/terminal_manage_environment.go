package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/statefile"
)

// The manager consumes typed domain operations, never shell commands or a saved
// preview. Revision is optimistic concurrency authority for definition edits.
type terminalManageEnvironmentOperations interface {
	LoadEnvironments(context.Context, string) (terminalManageEnvironmentSnapshot, error)
	SaveEnvironment(context.Context, sessionstate.WorkEnvironment) (sessionstate.WorkEnvironment, error)
	DeleteEnvironment(context.Context, sessionstate.EnvironmentID, int64) error
	PreviewEnvironment(context.Context, sessionstate.EnvironmentID, sessionstate.ContextID, string) (sessionstate.ReachPreview, error)
	ReachEnvironment(context.Context, sessionstate.EnvironmentID, sessionstate.ContextID, string) (sessionstate.ReachResult, error)
}

// Saved definitions are usable before optional live observations finish.
type terminalManageEnvironmentObserver interface {
	ObserveEnvironments(context.Context, string, terminalManageEnvironmentSnapshot) (terminalManageEnvironmentSnapshot, error)
}

type terminalManageEnvironmentSnapshot struct {
	environments []sessionstate.WorkEnvironment
	contexts     []sessionstate.Context
	terminals    []terminalInventoryResult
	applications []sessionstate.ApplicationInventoryItem
}

type terminalManageEnvironmentMode uint8

const (
	environmentList terminalManageEnvironmentMode = iota
	environmentFilter
	environmentName
	environmentMembers
	environmentAdd
	environmentOverride
	environmentDetails
	environmentPreview
	environmentResults
	environmentDelete
	environmentHelp
	environmentUncertain
)

type terminalManageEnvironmentState struct {
	active          bool
	loaded          bool
	loading         bool
	loadID          uint64
	snapshot        terminalManageEnvironmentSnapshot
	names           map[sessionstate.ContextID]string
	searchFields    map[sessionstate.ContextID]string
	selectedID      sessionstate.EnvironmentID
	cursor          int
	filter          string
	originalFilter  string
	mode            terminalManageEnvironmentMode
	returnMode      terminalManageEnvironmentMode
	saveReturn      terminalManageEnvironmentMode
	input           textinput.Model
	draft           sessionstate.WorkEnvironment
	memberBase      sessionstate.WorkEnvironment
	memberCursor    int
	pickerCursor    int
	scroll          int
	busy            bool
	writeID         uint64
	err             error
	status          string
	report          []string
	results         []string
	reachID         uint64
	reachPending    bool
	reachStop       context.CancelFunc
	previewID       uint64
	previewTarget   sessionstate.EnvironmentID
	observationID   uint64
	observing       bool
	observationStop context.CancelFunc
	observationErr  error
	uncertainLoaded bool
}

type terminalManageEnvironmentLoadedMsg struct {
	generation uint64
	snapshot   terminalManageEnvironmentSnapshot
	err        error
}
type terminalManageEnvironmentObservedMsg struct {
	generation    uint64
	observationID uint64
	snapshot      terminalManageEnvironmentSnapshot
	err           error
}
type terminalManageEnvironmentSavedMsg struct {
	generation uint64
	definition sessionstate.WorkEnvironment
	deleted    bool
	err        error
}
type terminalManageEnvironmentPreviewMsg struct {
	generation uint64
	id         sessionstate.EnvironmentID
	result     sessionstate.ReachPreview
	err        error
}
type terminalManageEnvironmentReachedMsg struct {
	generation uint64
	id         sessionstate.EnvironmentID
	result     sessionstate.ReachResult
	err        error
}

func (model *terminalManageModel) environmentOperations() (terminalManageEnvironmentOperations, bool) {
	operations, ok := model.operations.(terminalManageEnvironmentOperations)
	return operations, ok
}

func (model *terminalManageModel) environmentLoad() tea.Cmd {
	operations, ok := model.environmentOperations()
	if !ok {
		return nil
	}
	state := &model.environment
	model.environmentStopObservation()
	state.observationErr = nil
	state.previewID++
	state.snapshot = resetEnvironmentObservations(state.snapshot)
	state.loading = true
	state.loadID++
	generation, ctx, socket := state.loadID, model.ctx, model.socket
	return func() tea.Msg {
		snapshot, err := operations.LoadEnvironments(ctx, socket)
		return terminalManageEnvironmentLoadedMsg{generation, snapshot, err}
	}
}

func (model *terminalManageModel) environmentStopObservation() {
	state := &model.environment
	if state.observationStop != nil {
		state.observationStop()
		state.observationStop = nil
	}
	state.observing = false
	state.observationID++
}

func (model *terminalManageModel) environmentObserve() tea.Cmd {
	model.environmentStopObservation()
	state := &model.environment
	observer, ok := model.operations.(terminalManageEnvironmentObserver)
	if !ok || !state.active || !state.loaded {
		return nil
	}
	ctx, cancel := context.WithTimeout(model.ctx, terminalObservationTimeout)
	state.observationStop = cancel
	state.observing = true
	state.snapshot = resetEnvironmentObservations(state.snapshot)
	generation, observationID, socket, snapshot := state.loadID, state.observationID, model.socket, cloneEnvironmentObservations(state.snapshot)
	return func() tea.Msg {
		observed, err := observer.ObserveEnvironments(ctx, socket, snapshot)
		return terminalManageEnvironmentObservedMsg{generation, observationID, observed, err}
	}
}

func resetEnvironmentObservations(snapshot terminalManageEnvironmentSnapshot) terminalManageEnvironmentSnapshot {
	snapshot = cloneEnvironmentObservations(snapshot)
	for index := range snapshot.terminals {
		item := &snapshot.terminals[index]
		item.WindowPresence = "unknown"
		item.WindowObservedAt = nil
		item.WindowReason = "refresh_pending"
		item.Activity = sessionstate.UnknownTerminalSessionObservation("refresh_pending")
	}
	for index := range snapshot.applications {
		item := &snapshot.applications[index]
		item.Presence = sessionstate.ApplicationPresence{State: "unknown", Reason: "refresh_pending"}
		item.LauncherValidity = sessionstate.ApplicationLauncherObservation{State: sessionstate.ApplicationLauncherUnknown, Reason: "refresh_pending"}
		item.LauncherObservedAt = nil
		if item.LaunchGuard != nil {
			item.LaunchGuard.CompositorMatches = nil
		}
	}
	return snapshot
}

func cloneEnvironmentObservations(snapshot terminalManageEnvironmentSnapshot) terminalManageEnvironmentSnapshot {
	snapshot.terminals = slices.Clone(snapshot.terminals)
	snapshot.applications = slices.Clone(snapshot.applications)
	for index := range snapshot.applications {
		item := &snapshot.applications[index]
		if item.LaunchGuard != nil {
			guard := *item.LaunchGuard
			item.LaunchGuard = &guard
		}
	}
	return snapshot
}

func (model *terminalManageModel) environmentCancelReach() {
	if model.environment.reachStop != nil {
		model.environment.reachStop()
		model.environment.reachStop = nil
	}
	model.environment.reachID++
	model.environment.reachPending = false
}

// Environment messages remain routable after switching back to terminals. Old
// generations cannot replace a new definition, editor target, or Reach result.
func (model *terminalManageModel) updateEnvironment(message tea.Msg) (tea.Cmd, bool) {
	state := &model.environment
	switch message := message.(type) {
	case terminalManageEnvironmentLoadedMsg:
		if message.generation != state.loadID {
			return nil, true
		}
		state.loading = false
		state.err = message.err
		if state.mode == environmentUncertain && message.err == nil {
			state.uncertainLoaded = true
			state.err = errors.New("save outcome uncertain; inspect the exact identity before editing")
		}
		if message.err == nil {
			state.snapshot = resetEnvironmentObservations(message.snapshot)
			state.indexContexts()
			state.loaded = true
			state.restoreSelection()
			return model.environmentObserve(), true
		}
		return nil, true
	case terminalManageEnvironmentObservedMsg:
		if message.generation != state.loadID || message.observationID != state.observationID || !state.observing || !state.active {
			return nil, true
		}
		model.environmentStopObservation()
		state.observationErr = message.err
		// Observation failures are localized facts; they never replace definitions.
		if message.snapshot.terminals != nil {
			state.snapshot.terminals = message.snapshot.terminals
		}
		if message.snapshot.applications != nil {
			state.snapshot.applications = message.snapshot.applications
		}
		state.indexContexts()
		state.restoreSelection()
		return nil, true
	case terminalManageEnvironmentSavedMsg:
		if message.generation != state.writeID {
			return nil, true
		}
		state.busy = false
		state.err = message.err
		if message.err != nil {
			var uncertain *statefile.CommitOutcomeUnknownError
			if errors.As(message.err, &uncertain) {
				if message.definition.ID != "" {
					state.draft = message.definition
					state.draft.Members = slices.Clone(message.definition.Members)
					state.selectedID = message.definition.ID
				}
				state.mode = environmentUncertain
				state.uncertainLoaded = false
				state.err = errors.New("save outcome uncertain; refresh/inspect before retry")
				model.environmentCancelReach()
				return model.environmentLoad(), true
			}
			if state.mode == environmentMembers {
				state.draft = state.memberBase
				state.draft.Members = slices.Clone(state.memberBase.Members)
				state.memberCursor = min(state.memberCursor, max(len(state.draft.Members)-1, 0))
			}
			return nil, true
		}
		state.mode = state.saveReturn
		if !message.deleted {
			state.selectedID = message.definition.ID
			state.draft = message.definition
			state.draft.Members = slices.Clone(message.definition.Members)
			state.memberBase = message.definition
			state.memberCursor = min(state.memberCursor, max(len(state.draft.Members)-1, 0))
		}
		state.status = "Definition saved; context lifecycle unchanged."
		if message.deleted {
			state.status = "Definition deleted; contexts remain registered."
		}
		model.environmentCancelReach()
		return model.environmentLoad(), true
	case terminalManageEnvironmentPreviewMsg:
		if message.generation != state.previewID || message.id != state.selectedID || state.mode != environmentList {
			return nil, true
		}
		state.err = message.err
		if message.err == nil {
			state.report = terminalManageEnvironmentPreviewLines(message.result, state.names)
			state.previewTarget = message.id
			state.scroll = 0
			state.mode = environmentPreview
		}
		return nil, true
	case terminalManageEnvironmentReachedMsg:
		if message.generation != state.reachID {
			return nil, true
		}
		state.reachPending = false
		if state.reachStop != nil {
			state.reachStop()
			state.reachStop = nil
		}
		state.err = message.err
		if message.err == nil || len(message.result.Members) > 0 {
			state.results = terminalManageEnvironmentResultLines(message.result, state.names)
			state.status = "Reach finished; u shows per-member outcomes."
			if state.mode == environmentList && state.selectedID == message.id {
				state.report = state.results
				state.scroll = 0
				state.mode = environmentResults
			}
		}
		return nil, true
	case tea.WindowSizeMsg:
		state.input.SetWidth(max(message.Width-8, 1))
		return nil, false
	case terminalManageLoadedMsg, terminalManageActivityMsg, terminalManageDirectoryMsg, terminalManageActionMsg, terminalManageMigrationMsg, terminalManagePulseMsg:
		// Focused editors own input events, not the other view's asynchronous
		// domain messages. Its saved load must still finish before returning.
		return nil, false
	}
	if !state.active {
		return nil, false
	}
	if key, ok := message.(tea.KeyPressMsg); ok {
		return model.environmentKey(key), true
	}
	if state.textMode() {
		var command tea.Cmd
		state.input, command = state.input.Update(message)
		if state.mode == environmentFilter {
			state.filter = state.input.Value()
			state.restoreSelection()
		}
		return command, true
	}
	return nil, false
}

func (state *terminalManageEnvironmentState) textMode() bool {
	return state.mode == environmentFilter || state.mode == environmentName || state.mode == environmentAdd
}
func (model *terminalManageModel) environmentBeginInput(value, placeholder string) tea.Cmd {
	state := &model.environment
	state.input = textinput.New()
	state.input.CharLimit = 256
	state.input.Prompt = "> "
	state.input.Placeholder = placeholder
	state.input.SetValue(value)
	state.input.CursorEnd()
	width := model.width
	if width <= 0 {
		width = 80
	}
	state.input.SetWidth(max(width-8, 1))
	if model.noColor {
		state.input.SetStyles(textinput.Styles{})
	}
	return state.input.Focus()
}
func (state *terminalManageEnvironmentState) visible() []sessionstate.WorkEnvironment {
	result := make([]sessionstate.WorkEnvironment, 0, len(state.snapshot.environments))
	query := strings.ToLower(strings.TrimSpace(state.filter))
	for _, environment := range state.snapshot.environments {
		fields := []string{environment.Name, string(environment.ID)}
		for _, id := range environment.Members {
			fields = append(fields, state.contextSearch(id))
		}
		if query == "" || strings.Contains(strings.ToLower(strings.Join(fields, "\n")), query) {
			result = append(result, environment)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].ID < result[j].ID
	})
	return result
}
func (state *terminalManageEnvironmentState) restoreSelection() {
	visible := state.visible()
	for index, definition := range visible {
		if definition.ID == state.selectedID {
			state.cursor = index
			return
		}
	}
	state.cursor = min(max(state.cursor, 0), max(len(visible)-1, 0))
	if len(visible) > 0 {
		state.selectedID = visible[state.cursor].ID
	}
}
func (state *terminalManageEnvironmentState) selected() (sessionstate.WorkEnvironment, bool) {
	for _, definition := range state.visible() {
		if definition.ID == state.selectedID {
			return definition, true
		}
	}
	return sessionstate.WorkEnvironment{}, false
}

// Build presentation indexes once per snapshot. Searching a large group must
// not repeatedly scan the whole context catalog for each member.
func (state *terminalManageEnvironmentState) indexContexts() {
	state.names = make(map[sessionstate.ContextID]string, len(state.snapshot.contexts))
	state.searchFields = make(map[sessionstate.ContextID]string, len(state.snapshot.contexts))
	for _, item := range state.snapshot.contexts {
		name := item.Label
		if name == "" {
			name = string(item.ID)
		}
		state.names[item.ID] = name
		state.searchFields[item.ID] = strings.ToLower(name + "\n" + string(item.ID))
	}
	for _, item := range state.snapshot.applications {
		name := item.Label
		if name == "" {
			name = item.Launcher.DesktopID
		}
		if name == "" {
			name = item.Launcher.FlatpakID
		}
		if name == "" {
			name = string(item.ContextID)
		}
		state.names[item.ContextID] = name
		state.searchFields[item.ContextID] = strings.ToLower(strings.Join([]string{item.Label, item.Launcher.DesktopID, item.Launcher.FlatpakID, string(item.ContextID)}, "\n"))
	}
	for _, item := range state.snapshot.terminals {
		state.names[item.ContextID] = terminalManageName(item)
		state.searchFields[item.ContextID] = strings.ToLower(strings.Join([]string{terminalManageName(item), item.Label, item.Identity.Project, item.Cwd, item.PaneDirectory, item.Session, string(item.ContextID)}, "\n"))
	}
}
func (state *terminalManageEnvironmentState) contextSearch(id sessionstate.ContextID) string {
	if fields, ok := state.searchFields[id]; ok {
		return fields
	}
	return strings.ToLower(string(id))
}
func (state *terminalManageEnvironmentState) contextName(id sessionstate.ContextID) string {
	if name, ok := state.names[id]; ok {
		return name
	}
	return string(id) + " · unresolved"
}
func (state *terminalManageEnvironmentState) picker() []sessionstate.Context {
	query := strings.ToLower(strings.TrimSpace(state.input.Value()))
	var result []sessionstate.Context
	for _, item := range state.snapshot.contexts {
		if slices.Contains(state.draft.Members, item.ID) {
			continue
		}
		if query == "" || strings.Contains(strings.ToLower(state.contextSearch(item.ID)), query) {
			result = append(result, item)
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return state.contextName(result[i].ID) < state.contextName(result[j].ID) })
	return result
}

func (model *terminalManageModel) environmentKey(message tea.KeyPressMsg) tea.Cmd {
	state := &model.environment
	key := message.String()
	if key == "ctrl+c" {
		model.environmentStopObservation()
		model.environmentCancelReach()
		model.stopProbes()
		return tea.Quit
	}
	if model.width > 0 && (model.width < 48 || model.height < 16) {
		if key == "q" && !state.textMode() {
			model.environmentCancelReach()
			return tea.Quit
		}
		return nil
	}
	if state.busy {
		return nil
	}
	if state.mode == environmentUncertain {
		switch key {
		case "r":
			return model.environmentLoad()
		case "esc":
			if state.uncertainLoaded {
				state.mode = environmentList
				state.err = nil
				state.status = "Inspect saved definitions before making another explicit edit."
				state.restoreSelection()
			}
		case "q":
			model.environmentStopObservation()
			model.environmentCancelReach()
			return tea.Quit
		}
		return nil
	}
	if state.textMode() {
		switch key {
		case "esc":
			if state.mode == environmentFilter {
				state.filter = state.originalFilter
				state.restoreSelection()
				state.mode = environmentList
			} else if state.mode == environmentAdd {
				state.mode = environmentMembers
			} else {
				state.mode = environmentList
			}
			state.input.Blur()
			state.err = nil
			return nil
		case "enter":
			switch state.mode {
			case environmentFilter:
				state.filter = state.input.Value()
				state.restoreSelection()
				state.mode = environmentList
				state.input.Blur()
				return nil
			case environmentName:
				name := strings.TrimSpace(state.input.Value())
				if err := sessionstate.ValidateWorkEnvironmentName(name); err != nil {
					state.err = err
					return nil
				}
				state.draft.Name = name
				state.saveReturn = environmentList
				return model.environmentSave()
			case environmentAdd:
				options := state.picker()
				if len(options) > 0 {
					state.pickerCursor = min(state.pickerCursor, len(options)-1)
					state.draft.Members = append(state.draft.Members, options[state.pickerCursor].ID)
					state.memberCursor = len(state.draft.Members) - 1
					state.mode = environmentMembers
					state.input.Blur()
					state.saveReturn = environmentMembers
					return model.environmentSave()
				}
				return nil
			}
		case "up", "down":
			if state.mode == environmentAdd {
				if key == "up" {
					state.pickerCursor = max(state.pickerCursor-1, 0)
				} else {
					state.pickerCursor = min(state.pickerCursor+1, max(len(state.picker())-1, 0))
				}
				return nil
			}
		}
		var command tea.Cmd
		state.input, command = state.input.Update(message)
		if state.mode == environmentFilter {
			state.filter = state.input.Value()
			state.restoreSelection()
		}
		if state.mode == environmentAdd {
			state.pickerCursor = 0
		}
		return command
	}
	if state.mode == environmentDelete {
		if key == "esc" || key == "n" {
			state.mode = environmentList
			return nil
		}
		if key == "y" {
			return model.environmentDeleteDefinition()
		}
		return nil
	}
	if state.mode == environmentMembers {
		switch key {
		case "esc":
			state.mode = environmentList
			state.err = nil
		case "a":
			state.mode = environmentAdd
			state.pickerCursor = 0
			return model.environmentBeginInput("", "find an existing context")
		case "up", "k":
			state.memberCursor = max(state.memberCursor-1, 0)
		case "down", "j":
			state.memberCursor = min(state.memberCursor+1, max(len(state.draft.Members)-1, 0))
		case "d":
			if len(state.draft.Members) > 0 {
				id := state.draft.Members[state.memberCursor]
				state.draft.Members = slices.Delete(state.draft.Members, state.memberCursor, state.memberCursor+1)
				if state.draft.Primary == id {
					state.draft.Primary = ""
				}
				state.memberCursor = min(state.memberCursor, max(len(state.draft.Members)-1, 0))
				state.saveReturn = environmentMembers
				return model.environmentSave()
			}
		case "p":
			if len(state.draft.Members) > 0 {
				state.draft.Primary = state.draft.Members[state.memberCursor]
				state.saveReturn = environmentMembers
				return model.environmentSave()
			}
		case "c":
			if state.draft.Primary == "" {
				return nil
			}
			state.draft.Primary = ""
			state.saveReturn = environmentMembers
			return model.environmentSave()
		case "i":
			state.returnMode = environmentMembers
			state.mode = environmentDetails
			state.scroll = 0
		}
		return nil
	}
	if state.mode == environmentOverride {
		switch key {
		case "esc":
			state.mode = environmentList
		case "up", "k":
			state.memberCursor = max(state.memberCursor-1, 0)
		case "down", "j":
			state.memberCursor = min(state.memberCursor+1, max(len(state.draft.Members)-1, 0))
		case "enter":
			if len(state.draft.Members) > 0 {
				return model.environmentReachTarget(state.draft.ID, state.draft.Members[state.memberCursor])
			}
		}
		return nil
	}
	if state.mode == environmentDetails || state.mode == environmentPreview || state.mode == environmentResults || state.mode == environmentHelp {
		switch key {
		case "esc":
			state.mode = state.returnMode
		case "up", "k":
			state.scroll = max(state.scroll-1, 0)
		case "down", "j":
			state.scroll++
		case "pgup":
			state.scroll = max(state.scroll-model.environmentBodyHeight(), 0)
		case "pgdown":
			state.scroll += model.environmentBodyHeight()
		case "enter":
			if state.mode == environmentPreview {
				return model.environmentReachTarget(state.previewTarget, "")
			}
		}
		if key != "tab" && key != "q" && key != "x" {
			return nil
		}
	}
	switch key {
	case "tab":
		model.environmentStopObservation()
		state.active = false
		return nil
	case "q":
		model.environmentStopObservation()
		model.environmentCancelReach()
		model.stopProbes()
		return tea.Quit
	case "x":
		model.environmentCancelReach()
		state.status = "Pending Reach canceled; already started contexts remain open."
		return nil
	case "up", "k", "down", "j":
		visible := state.visible()
		if key == "up" || key == "k" {
			state.cursor = max(state.cursor-1, 0)
		} else {
			state.cursor = min(state.cursor+1, max(len(visible)-1, 0))
		}
		if len(visible) > 0 {
			state.selectedID = visible[state.cursor].ID
		}
		state.previewID++
		state.err = nil
	case "/":
		state.originalFilter = state.filter
		state.mode = environmentFilter
		return model.environmentBeginInput(state.filter, "environment or member")
	case "r":
		return model.environmentLoad()
	case "?":
		state.mode = environmentHelp
		state.returnMode = environmentList
		state.scroll = 0
	case "n":
		state.draft = sessionstate.WorkEnvironment{}
		state.mode = environmentName
		state.err = nil
		return model.environmentBeginInput("", "environment name")
	case "e", "v", "d", "o", "i":
		definition, ok := state.selected()
		if !ok {
			return nil
		}
		state.draft = definition
		state.draft.Members = slices.Clone(definition.Members)
		state.memberBase = definition
		state.memberCursor = 0
		state.err = nil
		switch key {
		case "e":
			state.mode = environmentName
			return model.environmentBeginInput(definition.Name, "environment name")
		case "v":
			state.mode = environmentMembers
		case "d":
			state.mode = environmentDelete
		case "o":
			state.mode = environmentOverride
		case "i":
			state.returnMode = environmentList
			state.mode = environmentDetails
			state.scroll = 0
		}
	case "p":
		return model.environmentPreview()
	case "u":
		if len(state.results) > 0 {
			state.report = state.results
			state.mode = environmentResults
			state.returnMode = environmentList
			state.scroll = 0
		}
	case "enter":
		return model.environmentReach("")
	}
	return nil
}

func (model *terminalManageModel) environmentSave() tea.Cmd {
	operations, ok := model.environmentOperations()
	if !ok {
		return nil
	}
	state := &model.environment
	state.busy = true
	state.err = nil
	state.writeID++
	generation, definition, ctx := state.writeID, state.draft, model.ctx
	definition.Members = slices.Clone(definition.Members)
	return func() tea.Msg {
		saved, err := operations.SaveEnvironment(ctx, definition)
		return terminalManageEnvironmentSavedMsg{generation: generation, definition: saved, err: err}
	}
}
func (model *terminalManageModel) environmentDeleteDefinition() tea.Cmd {
	operations, ok := model.environmentOperations()
	if !ok {
		return nil
	}
	state := &model.environment
	state.saveReturn = environmentList
	state.busy = true
	state.writeID++
	generation, definition, ctx := state.writeID, state.draft, model.ctx
	return func() tea.Msg {
		err := operations.DeleteEnvironment(ctx, definition.ID, definition.Revision)
		return terminalManageEnvironmentSavedMsg{generation: generation, deleted: true, err: err}
	}
}
func (model *terminalManageModel) environmentPreview() tea.Cmd {
	operations, ok := model.environmentOperations()
	if !ok {
		return nil
	}
	state := &model.environment
	definition, ok := state.selected()
	if !ok {
		return nil
	}
	state.previewID++
	generation, ctx, socket := state.previewID, model.ctx, model.socket
	state.status = "Evaluating fresh Reach preview…"
	state.err = nil
	state.returnMode = environmentList
	return func() tea.Msg {
		result, err := operations.PreviewEnvironment(ctx, definition.ID, "", socket)
		return terminalManageEnvironmentPreviewMsg{generation, definition.ID, result, err}
	}
}
func (model *terminalManageModel) environmentReach(primary sessionstate.ContextID) tea.Cmd {
	definition, ok := model.environment.selected()
	if !ok {
		return nil
	}
	return model.environmentReachTarget(definition.ID, primary)
}

func (model *terminalManageModel) environmentReachTarget(id sessionstate.EnvironmentID, primary sessionstate.ContextID) tea.Cmd {
	// The new explicit intent supersedes old focus authority even when its
	// own preflight blocks before execution (for example, no saved primary).
	model.environmentCancelReach()
	operations, ok := model.environmentOperations()
	if !ok {
		return nil
	}
	state := &model.environment
	var definition sessionstate.WorkEnvironment
	for _, candidate := range state.snapshot.environments {
		if candidate.ID == id {
			definition = candidate
			break
		}
	}
	if definition.ID == "" {
		state.err = errors.New("the displayed environment was removed; return and select work explicitly")
		return nil
	}
	if primary == "" && definition.Primary == "" {
		state.err = errors.New("choose a saved primary in Members or press o to choose for this Reach")
		return nil
	}
	ctx, stop := context.WithCancel(model.ctx)
	state.reachStop = stop
	state.reachPending = true
	generation, socket := state.reachID, model.socket
	state.mode = environmentList
	state.status = "Reaching " + definition.Name + "… navigation remains available; x cancels pending work."
	state.err = nil
	state.returnMode = environmentList
	return func() tea.Msg {
		result, err := operations.ReachEnvironment(ctx, definition.ID, primary, socket)
		return terminalManageEnvironmentReachedMsg{generation, definition.ID, result, err}
	}
}

func (model terminalManageModel) environmentBodyHeight() int {
	height := model.height
	if height <= 0 {
		height = 24
	}
	return max(height-10, 1)
}
func environmentWindow(lines []string, cursor, height int) []string {
	if len(lines) <= height {
		return lines
	}
	start := max(cursor-height/2, 0)
	start = min(start, len(lines)-height)
	return lines[start : start+height]
}
func (model terminalManageModel) renderEnvironment() string {
	state := &model.environment
	width, height := model.width, model.height
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	if width < 48 || height < 16 {
		return terminalManageFit("Ctrl+C to quit or resize: environments need at least 48×16.", width, height)
	}
	var lines []string
	title := "Work environments"
	if state.loading {
		title += " · refreshing"
	}
	lines = append(lines, title)
	definition, selected := state.selected()
	switch state.mode {
	case environmentName, environmentMembers, environmentAdd, environmentOverride, environmentDelete, environmentDetails, environmentUncertain:
		definition = state.draft
		selected = true
	}
	if selected {
		if definition.Name == "" {
			definition.Name = "New environment"
		}
		lines = append(lines, ansi.Truncate(definition.Name+" · Primary: "+state.primaryName(definition.Primary), width, "…"))
	} else {
		lines = append(lines, fmt.Sprintf("%d saved", len(state.snapshot.environments)))
	}
	lines = append(lines, ansi.Truncate("Filter: "+state.filter, width, "…"))
	body, footer := model.environmentBody()
	var wrappedFooter []string
	for _, line := range footer {
		wrappedFooter = append(wrappedFooter, strings.Split(terminalManageWrap(line, width), "\n")...)
	}
	footer = wrappedFooter
	if state.mode == environmentDetails || state.mode == environmentPreview || state.mode == environmentResults || state.mode == environmentHelp {
		var wrapped []string
		for _, line := range body {
			wrapped = append(wrapped, strings.Split(terminalManageWrap(line, width), "\n")...)
		}
		body = wrapped
	}
	available := model.environmentBodyHeight()
	if len(footer) > 3 {
		available = max(available-(len(footer)-3), 1)
	}
	if state.mode == environmentDetails || state.mode == environmentPreview || state.mode == environmentResults || state.mode == environmentHelp {
		offset := min(max(state.scroll, 0), max(len(body)-available, 0))
		body = body[offset:min(offset+available, len(body))]
	} else {
		cursor := state.cursor
		if state.mode == environmentMembers || state.mode == environmentOverride {
			cursor = state.memberCursor
		}
		if state.mode == environmentAdd {
			cursor = state.pickerCursor
		}
		if state.mode == environmentAdd && len(body) > 1 {
			body = append(body[:1], environmentWindow(body[1:], cursor, max(available-1, 1))...)
		} else {
			body = environmentWindow(body, cursor, available)
		}
	}
	lines = append(lines, body...)
	for len(lines) < height-len(footer)-2 {
		lines = append(lines, "")
	}
	feedback := state.status
	if state.observing {
		feedback = "Live evidence pending; saved definitions remain editable."
	}
	if state.observationErr != nil {
		feedback = "Live evidence unavailable; unknown until refreshed."
	}
	if state.reachPending {
		feedback = "Reach pending · navigation available · x cancel"
	}
	if state.err != nil {
		feedback = "Error: " + state.err.Error()
	}
	if state.busy {
		feedback = "Saving definition…"
	}
	lines = append(lines, ansi.Truncate(feedback, width, "…"))
	lines = append(lines, footer...)
	return terminalManageFit(strings.Join(lines, "\n"), width, height)
}
func (state *terminalManageEnvironmentState) primaryName(id sessionstate.ContextID) string {
	if id == "" {
		return "none · choose before Reach"
	}
	return state.contextName(id)
}
func (model terminalManageModel) environmentBody() ([]string, []string) {
	state := &model.environment
	common := []string{"[↑/↓ j/k] Select [/] Search [Tab] Terminals", "[Enter] Reach [p] Preview [o] Choose primary", "[n] New [e] Rename [v] Members [d] Delete", "[i] Details [u] Results [r] Refresh", "[?] Help [q] Quit"}
	switch state.mode {
	case environmentUncertain:
		lines := []string{"Save outcome uncertain; refresh/inspect before retry.", "Candidate: " + string(state.draft.ID), fmt.Sprintf("Candidate revision: %d", state.draft.Revision)}
		observed := false
		for _, definition := range state.snapshot.environments {
			if definition.ID == state.draft.ID {
				observed = true
				lines = append(lines, fmt.Sprintf("Observed exact identity: %s (revision %d)", definition.Name, definition.Revision))
			}
		}
		if !state.uncertainLoaded {
			lines = append(lines, "Awaiting a successful saved-state refresh.")
		} else if !observed {
			lines = append(lines, "Exact identity is absent from refreshed saved state.")
		}
		return lines, []string{"[r] Refresh saved state [q] Quit", "[Esc] Return after successful inspection"}
	case environmentName:
		return []string{"Environment name", state.inputView(model.noColor)}, []string{"[Enter] Save name [Esc] Cancel"}
	case environmentFilter:
		return []string{"Search environments and referenced work", state.inputView(model.noColor)}, []string{"[Enter] Apply [Esc] Keep previous search"}
	case environmentDelete:
		return []string{"Delete environment “" + state.draft.Name + "”?", "Only its definition is removed.", "Contexts and windows remain registered/open."}, []string{"[y] Delete definition [n/Esc] Cancel"}
	case environmentAdd:
		lines := []string{}
		for index, item := range state.picker() {
			prefix := "  "
			if index == state.pickerCursor {
				prefix = "> "
			}
			lines = append(lines, prefix+state.contextName(item.ID)+" · "+string(item.State))
		}
		if len(lines) == 0 {
			lines = append(lines, "No remaining contexts match.")
		}
		return append([]string{state.inputView(model.noColor)}, lines...), []string{"Type to search · [↑/↓] Select", "[Enter] Add and save [Esc] Cancel"}
	case environmentMembers, environmentOverride:
		lines := []string{}
		for index, id := range state.draft.Members {
			prefix := "  "
			if index == state.memberCursor {
				prefix = "> "
			}
			suffix := ""
			if id == state.draft.Primary {
				suffix = " · primary"
			}
			lines = append(lines, prefix+state.contextName(id)+suffix)
		}
		if len(lines) == 0 {
			lines = append(lines, "No members yet; add existing contexts.")
		}
		if state.mode == environmentOverride {
			return lines, []string{"Choose primary for this Reach only", "[↑/↓] Select [Enter] Reach [Esc] Cancel"}
		}
		return lines, []string{"[a] Add [d] Remove [p] Primary [c] Clear primary", "[i] Details [↑/↓] Select", "Changes save individually · [Esc] Back"}
	case environmentDetails:
		definition := state.draft
		lines := []string{"Environment " + string(definition.ID), "Name: " + definition.Name, "Primary: " + state.primaryName(definition.Primary)}
		if len(definition.Members) > 0 {
			index := min(state.memberCursor, len(definition.Members)-1)
			lines = append(lines, state.memberDetails(definition.Members[index])...)
		}
		return lines, []string{"[↑/↓ PgUp/PgDn] Scroll [Esc] Back"}
	case environmentPreview, environmentResults:
		footer := []string{"[↑/↓ PgUp/PgDn] Scroll [Esc] Back", "[Tab] Terminals [x] Cancel pending [q] Quit"}
		if state.mode == environmentPreview {
			footer = append([]string{"Preview · [Enter] Reach with fresh checks"}, footer...)
		}
		return state.report, footer
	case environmentHelp:
		return []string{"Environments reference existing contexts; they do not own them.", "Enter reaches the primary; outcomes explain blocked or missing work.", "Existing windows stay where they are. Unknown evidence blocks launches.", "p previews; o chooses a primary for one Reach.", "v edits members: a adds, d removes, p chooses primary.", "Each member action saves individually.", "Deleting an environment never archives or closes a context.", "A new Reach supersedes pending focus/unstarted work.", "x cancels pending Reach without closing already started work.", "Search includes environment names and referenced context fields.", "Applications have window/policy evidence, no Herdr or agent state.", "Tab returns to the unchanged terminal manager."}, []string{"[↑/↓ PgUp/PgDn] Scroll [Esc] Back"}
	}
	if !state.loaded && state.loading {
		return []string{"Loading environment definitions…"}, common
	}
	if !state.loaded && state.err != nil {
		return []string{"Unable to load saved environments. Press r to retry."}, common
	}
	definitions := state.visible()
	lines := []string{}
	for _, definition := range definitions {
		prefix := "  "
		if definition.ID == state.selectedID {
			prefix = "> "
		}
		lines = append(lines, fmt.Sprintf("%s%s · %d members", prefix, definition.Name, len(definition.Members)))
	}
	if len(lines) == 0 {
		if len(state.snapshot.environments) == 0 {
			lines = append(lines, "No work environments yet. Press n to create one.")
		} else {
			lines = append(lines, "No environments match; / edits the search.")
		}
	}
	return lines, common
}
func (state *terminalManageEnvironmentState) inputView(noColor bool) string {
	if !noColor {
		return state.input.View()
	}
	characters := []rune(state.input.Value())
	position := min(max(state.input.Position(), 0), len(characters))
	before, after := string(characters[:position]), string(characters[position:])
	width := max(state.input.Width(), 1)
	leftBudget := width - 1
	if after != "" {
		leftBudget /= 2
	}
	beforeWidth := ansi.StringWidth(before)
	left := ansi.Cut(before, max(beforeWidth-leftBudget, 0), beforeWidth)
	right := ansi.Truncate(after, max(width-1-ansi.StringWidth(left), 0), "")
	return state.input.Prompt + left + "│" + right
}
func (state *terminalManageEnvironmentState) memberDetails(id sessionstate.ContextID) []string {
	lines := []string{"Member: " + state.contextName(id), "Context: " + string(id)}
	for _, item := range state.snapshot.terminals {
		if item.ContextID == id {
			return append(lines, "Lifecycle: "+string(item.State), "Window: "+item.WindowPresence, "Herdr: "+terminalManageSessionEvidence(item.Activity.SessionState), "Agent: "+terminalManageAgentEvidence(item.Activity.AgentState), "Evidence: "+terminalManageEvidenceTime(item.Activity), "Next login: "+terminalManageNextLogin(item), "Last restore: "+terminalManageRestoreResult(item), "Startup path: "+item.Cwd)
		}
	}
	for _, item := range state.snapshot.applications {
		if item.ContextID == id {
			intent := "desired closed"
			if item.DesiredOpen {
				intent = "desired open"
			}
			lines = append(lines, "Lifecycle: "+string(item.State), "Intent: "+intent, "Window: "+item.Presence.State+" · "+item.Presence.Reason, "Next login: "+sessionstate.RestorePolicyExplanation(item.RestorePolicy.Reason), "Launcher: "+string(item.LauncherValidity.State)+" · "+item.LauncherValidity.Reason)
			if item.RestoreOutcome != nil {
				lines = append(lines, "Last restore: "+string(item.RestoreOutcome.Outcome.Status)+" · "+item.RestoreOutcome.Stage)
			}
			return lines
		}
	}
	return append(lines, "Unresolved reference; remove or replace explicitly.")
}

func terminalManageEnvironmentPreviewLines(preview sessionstate.ReachPreview, names map[sessionstate.ContextID]string) []string {
	lines := []string{"Fresh Reach preview", "Environment: " + string(preview.EnvironmentID)}
	if !preview.Ready {
		lines = append(lines, "Primary is not ready; resolve the listed reason before Reach.")
	}
	return append(lines, terminalManageEnvironmentMemberResults(preview.Members, names)...)
}
func terminalManageEnvironmentResultLines(result sessionstate.ReachResult, names map[sessionstate.ContextID]string) []string {
	return append([]string{"Reach outcomes · partial results do not imply all members opened", "Environment: " + string(result.EnvironmentID)}, terminalManageEnvironmentMemberResults(result.Members, names)...)
}
func terminalManageEnvironmentMemberResults(members []sessionstate.ReachMemberResult, names map[sessionstate.ContextID]string) []string {
	lines := make([]string, 0, len(members))
	for _, member := range members {
		primary := ""
		if member.Primary {
			primary = " · primary"
		}
		name := names[member.ContextID]
		if name == "" {
			name = "Unresolved member"
		}
		lines = append(lines, fmt.Sprintf("%s%s [%s]: %s · %s", name, primary, member.ContextID, strings.ReplaceAll(string(member.Status), "_", " "), member.Reason))
	}
	if len(lines) == 0 {
		lines = append(lines, "No member effects reported.")
	}
	return lines
}
