package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	sessionstate "github.com/marang/sway-session/internal/session"
	"golang.org/x/term"
)

type terminalManageRunner func(context.Context, io.Reader, io.Writer, string, string, dependencies) error

type terminalManageMode uint8

const (
	terminalManageListMode terminalManageMode = iota
	terminalManageFilterMode
	terminalManageRenameMode
	terminalManagePurgeMode
	terminalManageHelpMode
)

type terminalManageLoadedMsg struct {
	generation uint64
	snapshot   terminalManageSnapshot
	err        error
}

type terminalWindowPresence uint8

const (
	terminalWindowUnknown terminalWindowPresence = iota
	terminalWindowOpen
	terminalWindowClosed
)

func (presence terminalWindowPresence) String() string {
	switch presence {
	case terminalWindowOpen:
		return "open"
	case terminalWindowClosed:
		return "closed"
	default:
		return "unknown"
	}
}

type terminalManageActionMsg struct {
	id     sessionstate.ContextID
	action string
	err    error
}

type terminalManageMigrationMsg struct {
	action string
	err    error
}

type terminalManagePulseMsg struct{}

const terminalManagePulseInterval = 180 * time.Millisecond

type terminalManageModel struct {
	ctx        context.Context
	operations terminalManageOperations
	socket     string
	items      []terminalInventoryResult
	windows    map[sessionstate.ContextID]terminalWindowPresence
	visible    []int
	selectedID sessionstate.ContextID
	cursor     int
	width      int
	height     int
	mode       terminalManageMode
	input      textinput.Model
	loading    bool
	loadID     uint64
	pending    bool
	status     string
	err        error
	windowErr  error
	noColor    bool
	animate    bool
	pulsePhase int
}

func newTerminalManageModel(operations terminalManageOperations) terminalManageModel {
	noColor := os.Getenv("NO_COLOR") != ""
	input := textinput.New()
	input.CharLimit = 256
	input.Prompt = "> "
	input.Placeholder = "type to filter"
	if noColor {
		input.SetStyles(textinput.Styles{})
	}
	return terminalManageModel{
		ctx: context.Background(), operations: operations, input: input, loading: true, loadID: 1, noColor: noColor,
	}
}

func (model terminalManageModel) Init() tea.Cmd {
	if model.animate {
		return tea.Batch(model.loadCommand(model.loadID), terminalManagePulseCommand())
	}
	return model.loadCommand(model.loadID)
}

func (model terminalManageModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = message.Width, message.Height
		model.input.SetWidth(max(message.Width-12, 8))
		return model, nil
	case terminalManageLoadedMsg:
		if message.generation != model.loadID {
			return model, nil
		}
		model.loading = false
		model.pending = false
		model.err = message.err
		if message.err == nil {
			model.items = sortTerminalManageItems(message.snapshot.items)
			model.windows = message.snapshot.windows
			model.windowErr = message.snapshot.windowError
			model.rebuildVisible()
			model.restoreSelection()
		} else {
			model.windows = terminalManageUnknownWindows(model.items)
			model.windowErr = message.err
		}
		return model, nil
	case terminalManageActionMsg:
		model.pending = false
		model.err = message.err
		if message.err != nil {
			return model, nil
		}
		model.selectedID = message.id
		model.status = message.action
		return model, model.beginLoad()
	case terminalManageMigrationMsg:
		model.pending = false
		model.err = message.err
		if message.err != nil {
			return model, nil
		}
		model.status = message.action
		return model, model.beginLoad()
	case terminalManagePulseMsg:
		if !model.animate || model.noColor {
			return model, nil
		}
		model.pulsePhase = (model.pulsePhase + 1) % 120
		return model, terminalManagePulseCommand()
	case tea.KeyPressMsg:
		return model.handleKey(message)
	}
	if model.mode == terminalManageFilterMode || model.mode == terminalManageRenameMode {
		var command tea.Cmd
		model.input, command = model.input.Update(message)
		if model.mode == terminalManageFilterMode {
			model.rebuildVisible()
			model.clampCursor()
		}
		return model, command
	}
	return model, nil
}

func (model terminalManageModel) handleKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	if model.pending {
		return model, nil
	}
	if key == "ctrl+c" {
		return model, tea.Quit
	}
	if model.width > 0 && (model.width < 48 || model.height < 16) {
		if key == "q" {
			return model, tea.Quit
		}
		return model, nil
	}
	if model.loading {
		if key == "q" {
			return model, tea.Quit
		}
		return model, nil
	}
	switch model.mode {
	case terminalManageFilterMode:
		switch key {
		case "esc":
			model.mode = terminalManageListMode
			model.input.Blur()
			model.input.Reset()
			model.rebuildVisible()
			model.restoreSelection()
			return model, nil
		case "enter":
			model.mode = terminalManageListMode
			model.input.Blur()
			model.rememberSelected()
			return model, nil
		}
		var command tea.Cmd
		model.input, command = model.input.Update(message)
		model.rebuildVisible()
		model.clampCursor()
		model.rememberSelected()
		return model, command
	case terminalManageRenameMode:
		switch key {
		case "esc":
			model.mode = terminalManageListMode
			model.input.Blur()
			return model, nil
		case "ctrl+a":
			model.input.Reset()
			return model, nil
		case "enter":
			item, ok := model.selected()
			if !ok {
				return model, nil
			}
			label := strings.TrimSpace(model.input.Value())
			if err := sessionstate.ValidateContextLabel(label); err != nil || label == "" {
				if err == nil {
					err = errors.New("title must not be empty")
				}
				model.err = err
				return model, nil
			}
			model.mode = terminalManageListMode
			model.input.Blur()
			model.pending = true
			model.err = nil
			return model, model.renameCommand(item.ContextID, label)
		}
		var command tea.Cmd
		model.input, command = model.input.Update(message)
		return model, command
	case terminalManagePurgeMode:
		switch key {
		case "esc", "n":
			model.mode = terminalManageListMode
			return model, nil
		case "y":
			item, ok := model.selected()
			if !ok {
				model.mode = terminalManageListMode
				return model, nil
			}
			model.mode = terminalManageListMode
			model.pending = true
			model.err = nil
			return model, model.purgeCommand(item.ContextID)
		default:
			return model, nil
		}
	case terminalManageHelpMode:
		if key == "q" {
			return model, tea.Quit
		}
		if key == "esc" || key == "?" {
			model.mode = terminalManageListMode
		}
		return model, nil
	}

	switch key {
	case "q":
		return model, tea.Quit
	case "?":
		model.mode = terminalManageHelpMode
		return model, nil
	case "up", "k":
		if model.cursor > 0 {
			model.cursor--
		}
		model.rememberSelected()
		return model, nil
	case "down", "j":
		if model.cursor+1 < len(model.visible) {
			model.cursor++
		}
		model.rememberSelected()
		return model, nil
	case "/":
		model.mode = terminalManageFilterMode
		model.input.Placeholder = "filter by title, project, path, or ID"
		model.input.Reset()
		return model, model.input.Focus()
	case "e":
		item, ok := model.selected()
		if !ok {
			return model, nil
		}
		model.mode = terminalManageRenameMode
		model.input.Placeholder = "human-readable title"
		model.input.SetValue(item.Label)
		model.input.CursorEnd()
		model.err = nil
		return model, model.input.Focus()
	case "r":
		model.err = nil
		return model, model.beginLoad()
	case "m":
		model.pending = true
		model.err = nil
		return model, model.migrateCommand()
	case "enter", "o":
		item, ok := model.selected()
		if !ok {
			return model, nil
		}
		if item.State != sessionstate.ContextActive {
			model.err = errors.New("activate this terminal before opening it")
			return model, nil
		}
		model.pending = true
		model.err = nil
		return model, model.openCommand(item.ContextID)
	case "a":
		item, ok := model.selected()
		if !ok {
			return model, nil
		}
		state := sessionstate.ContextArchived
		action := "Archived " + terminalManageName(item)
		if item.State == sessionstate.ContextArchived {
			state = sessionstate.ContextActive
			action = "Activated " + terminalManageName(item)
		}
		model.pending = true
		model.err = nil
		return model, model.stateCommand(item.ContextID, state, action)
	case "d":
		if _, ok := model.selected(); ok {
			model.mode = terminalManagePurgeMode
			model.err = nil
		}
		return model, nil
	}
	return model, nil
}

func (model terminalManageModel) View() tea.View {
	content := model.render()
	view := tea.NewView(content)
	view.AltScreen = true
	return view
}

func (model terminalManageModel) render() string {
	if model.width > 0 && (model.width < 48 || model.height < 16) {
		if model.width == 1 {
			return "q"
		}
		return ansi.Truncate("Press q to quit or resize: persistent terminals need at least 48×16.", max(model.width, 1), "…")
	}
	styles := newTerminalManageStyles(model.noColor, model.pulsePhase)
	width := model.width
	if width <= 0 {
		width = 80
	}
	height := model.height
	wideLayout := model.wideLayout()
	framed := width >= 48 && (height == 0 || height >= 16)
	if framed {
		width -= 2
		if height > 0 {
			height -= 2
		}
	}
	var output strings.Builder
	hasFeedback := model.err != nil || model.status != "" || model.windowErr != nil
	output.WriteString(styles.title.Render("Persistent terminals"))
	open, unknown := model.windowCounts()
	counts := fmt.Sprintf("%d saved · %d open", len(model.items), open)
	if unknown != 0 {
		counts += fmt.Sprintf(" · %d unknown", unknown)
	}
	if width >= 72 {
		output.WriteString(styles.muted.Render("  " + counts + " · Snapshot [r] refresh"))
	} else {
		output.WriteString("\n" + styles.muted.Render(counts))
		output.WriteString("\n" + styles.muted.Render("Snapshot · [r] refresh"))
	}
	output.WriteByte('\n')
	if model.loading {
		output.WriteString("\nLoading terminal sessions…\n")
	} else if model.err != nil && len(model.items) == 0 {
		output.WriteString("\nUnable to load managed terminals.\nFix the state error below, then press r to retry.\n")
	} else if len(model.items) == 0 {
		output.WriteString("\nNo managed terminals yet.\nOpen one with sway-session terminal --new.\n")
	} else if len(model.visible) == 0 {
		output.WriteString("\nNo terminals match this filter.\n")
	} else {
		listWidth := width
		if wideLayout {
			listWidth = width/2 - 2
		}
		list := model.renderList(styles, listWidth)
		if wideLayout {
			detail := model.renderDetails(styles, width-listWidth-3)
			output.WriteString("\n" + lipgloss.JoinHorizontal(lipgloss.Top, list, "   ", detail))
		} else if model.mode == terminalManagePurgeMode || hasFeedback || width < 72 || (model.height > 0 && model.height < 22) {
			output.WriteString("\n" + list)
		} else {
			output.WriteString("\n" + list + "\n\n" + model.renderDetails(styles, width))
		}
		output.WriteByte('\n')
	}

	switch model.mode {
	case terminalManageFilterMode:
		output.WriteString("\nFilter  " + model.inputView())
		output.WriteString("\n[Enter] Apply filter   [Esc] Clear filter")
	case terminalManageRenameMode:
		output.WriteString("\nRename  " + model.inputView())
		output.WriteString("\n[Enter] Save title   [Esc] Cancel   [Ctrl+A] Clear")
	case terminalManagePurgeMode:
		if item, ok := model.selected(); ok {
			output.WriteString("\n" + styles.danger.Render("Delete "+terminalManageName(item)+" permanently?"))
			output.WriteString("\nThis removes the sway-session entry and its Herdr state.")
			output.WriteString("\n" + styles.muted.Render(item.Cwd))
			output.WriteString("\n[y] Delete permanently   [n/Esc] Cancel")
		}
	case terminalManageHelpMode:
		output.WriteString("\n" + styles.accent.Render("Keyboard help"))
		output.WriteString("\n[Esc/?] Close help   [q] Quit   [↑/↓ or j/k] Select")
		output.WriteString("\n[Enter/o] Open   [e] Rename   [a] Archive/activate   [/] Filter")
		output.WriteString("\n[d] Delete permanently   [m] Migrate old state   [r] Refresh")
	default:
		if model.pending {
			output.WriteString("\nWorking…")
		} else {
			output.WriteString("\n" + model.renderFooter(styles, width))
		}
	}
	if model.err != nil {
		feedback := terminalManageWrap("Error: "+terminalManageSentence(model.err.Error()), width)
		output.WriteString("\n" + styles.danger.Render(feedback))
	} else if model.status != "" {
		output.WriteString("\n" + styles.success.Render(terminalManageWrap(model.status, width)))
	}
	if model.err == nil && model.windowErr != nil {
		warning := "Window observation incomplete; unknown entries need [r] Refresh. " + terminalManageSentence(model.windowErr.Error())
		output.WriteString("\n" + styles.danger.Render(terminalManageWrap(warning, width)))
	}
	content := terminalManageFit(output.String(), width, height)
	if framed {
		return terminalManageFrame(content, width+2, model.pulsePhase, model.noColor)
	}
	return content
}

func (model terminalManageModel) renderFooter(styles terminalManageStyles, width int) string {
	global := styles.muted.Render("System") + "   [m] Migrate  [r] Refresh  [?] Help  [q] Quit"
	if len(model.visible) == 0 {
		if width < 64 {
			return styles.muted.Render("System") + "   [m] Migrate  [r] Refresh\n" +
				"         [?] Help  [q] Quit"
		}
		return global
	}
	if width >= 96 {
		return styles.muted.Render("Navigate") + " [↑/↓ or j/k] Select  [/] Filter\n" +
			styles.muted.Render("Selected") + " [Enter/o] Open  [e] Rename  [a] Archive/activate  [d] Delete\n" + global
	}
	if width >= 72 {
		return styles.muted.Render("Navigate") + " [↑/↓ or j/k] Select  [/] Filter\n" +
			styles.muted.Render("Selected") + " [Enter] Open  [e] Rename  [a] Archive/activate  [d] Delete\n" +
			styles.muted.Render("System") + "   [m] Migrate  [r] Refresh  [?] Help  [q] Quit"
	}
	if width >= 56 {
		return styles.muted.Render("Navigate") + " [↑/↓ or j/k] Select  [/] Filter\n" +
			styles.muted.Render("Selected") + " [Enter] Open  [e] Rename\n" +
			"         [a] Archive/activate  [d] Delete\n" +
			styles.muted.Render("System") + "   [m] Migrate  [r] Refresh  [?] Help  [q] Quit"
	}
	return styles.muted.Render("Navigate") + " [↑/↓ or j/k] Select  [/] Filter\n" +
		styles.muted.Render("Selected") + " [Enter] Open  [e] Rename\n" +
		"         [a] Archive/activate  [d] Delete\n" +
		styles.muted.Render("System") + "   [m] Migrate  [r] Refresh\n" +
		"         [?] Help  [q] Quit"
}

func (model terminalManageModel) inputView() string {
	if !model.noColor {
		return model.input.View()
	}
	characters := []rune(model.input.Value())
	position := min(max(model.input.Position(), 0), len(characters))
	characters = append(characters, 0)
	copy(characters[position+1:], characters[position:])
	characters[position] = '│'
	return model.input.Prompt + string(characters)
}

func terminalManageSentence(value string) string {
	characters := []rune(value)
	if len(characters) == 0 {
		return value
	}
	characters[0] = []rune(strings.ToUpper(string(characters[0])))[0]
	return string(characters)
}

type terminalManageStyles struct {
	title, accent, selected, muted, danger, success lipgloss.Style
}

func newTerminalManageStyles(noColor bool, pulsePhase int) terminalManageStyles {
	if noColor {
		return terminalManageStyles{}
	}
	styles := terminalManageStyles{
		title:    lipgloss.NewStyle().Bold(true),
		selected: lipgloss.NewStyle().Bold(true),
		muted:    lipgloss.NewStyle().Faint(true),
		danger:   lipgloss.NewStyle().Bold(true),
		success:  lipgloss.NewStyle().Bold(true),
	}
	styles.title = styles.title.Foreground(lipgloss.Color("#7DD3FC"))
	styles.accent = styles.accent.Foreground(lipgloss.Color("#C4B5FD"))
	selected := terminalManageGradientRGB(pulsePhase, 120, 0)
	styles.selected = styles.selected.
		Foreground(lipgloss.Color(selected.lighten(72).hex())).
		Background(lipgloss.Color(selected.scale(28).hex()))
	styles.muted = styles.muted.Foreground(lipgloss.Color("#94A3B8"))
	styles.danger = styles.danger.Foreground(lipgloss.Color("#FB7185"))
	styles.success = styles.success.Foreground(lipgloss.Color("#86EFAC"))
	return styles
}

func (model terminalManageModel) renderList(styles terminalManageStyles, width int) string {
	start, end := model.listWindow()
	lines := make([]string, 0, end-start+2)
	if start > 0 {
		lines = append(lines, styles.muted.Render(fmt.Sprintf("  ↑ %d more", start)))
	}
	for row := start; row < end; row++ {
		index := model.visible[row]
		item := model.items[index]
		cursor := "  "
		if row == model.cursor {
			cursor = "› "
		}
		presence := model.windowPresence(item.ContextID)
		restore := "restore enabled"
		if item.State == sessionstate.ContextArchived {
			restore = "archived"
		}
		indicator := "?"
		if presence == terminalWindowOpen {
			indicator = "●"
		} else if presence == terminalWindowClosed {
			indicator = "○"
		}
		line := fmt.Sprintf("%s%s %s · %s  %s", cursor, indicator, presence, restore, terminalManageName(item))
		line = ansi.Truncate(line, max(width, 1), "…")
		if row == model.cursor {
			if !model.noColor {
				line = terminalManagePad(line, width)
			}
			line = styles.selected.Render(line)
		}
		lines = append(lines, line)
	}
	if end < len(model.visible) {
		lines = append(lines, styles.muted.Render(fmt.Sprintf("  ↓ %d more", len(model.visible)-end)))
	}
	return strings.Join(lines, "\n")
}

func (model terminalManageModel) listWindow() (int, int) {
	rows := 8
	height := model.height
	if height > 0 && model.width >= 48 && height >= 16 {
		height -= 2
	}
	if height > 0 {
		if model.wideLayout() {
			reserved := 10
			switch model.mode {
			case terminalManagePurgeMode:
				reserved += 3
			case terminalManageHelpMode:
				reserved += 2
			case terminalManageFilterMode, terminalManageRenameMode:
				reserved++
			}
			rows = max(height-reserved, 1)
		} else {
			reserved := 19
			switch model.mode {
			case terminalManagePurgeMode:
				reserved += 3
			case terminalManageHelpMode:
				reserved += 2
			case terminalManageFilterMode, terminalManageRenameMode:
				reserved++
			}
			rows = max(height-reserved, 1)
		}
	}
	rows = min(rows, len(model.visible))
	start := max(model.cursor-rows/2, 0)
	if start+rows > len(model.visible) {
		start = max(len(model.visible)-rows, 0)
	}
	return start, start + rows
}

func (model terminalManageModel) wideLayout() bool {
	return model.width >= 100 && (model.height == 0 || model.height >= 22)
}

func terminalManageFit(content string, width int, height int) string {
	lines := strings.Split(content, "\n")
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], max(width, 1), "…")
	}
	return strings.Join(lines, "\n")
}

func terminalManageWrap(value string, width int) string {
	width = max(width, 1)
	return ansi.Hardwrap(ansi.Wordwrap(value, width, " "), width, false)
}

func (model terminalManageModel) renderDetails(styles terminalManageStyles, width int) string {
	item, ok := model.selected()
	if !ok {
		return ""
	}
	lines := []string{
		styles.accent.Render(terminalManageName(item)),
		terminalManageDetail("Window", model.windowPresence(item.ContextID).String()),
		terminalManageDetail("Restore", terminalManageRestore(item)),
		terminalManageDetail("Last focused", terminalManageTime(item.LastFocusedAt)),
		terminalManageDetail("Created", terminalManageTime(item.CreatedAt)),
		terminalManageDetail("Project", terminalManageProject(item)),
		terminalManageDetail("Directory", item.Cwd),
		terminalManageDetail("Session", item.Session),
		terminalManageDetail("Context", string(item.ContextID)),
	}
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], max(width, 1), "…")
	}
	return strings.Join(lines, "\n")
}

func terminalManageDetail(label string, value string) string {
	return fmt.Sprintf("%-13s%s", label, value)
}

func terminalManageRestore(item terminalInventoryResult) string {
	if item.State == sessionstate.ContextArchived {
		return "archived"
	}
	return "enabled"
}

func terminalManageUnknownWindows(items []terminalInventoryResult) map[sessionstate.ContextID]terminalWindowPresence {
	windows := make(map[sessionstate.ContextID]terminalWindowPresence, len(items))
	for _, item := range items {
		windows[item.ContextID] = terminalWindowUnknown
	}
	return windows
}

func (model terminalManageModel) windowPresence(id sessionstate.ContextID) terminalWindowPresence {
	if presence, exists := model.windows[id]; exists {
		return presence
	}
	return terminalWindowUnknown
}

func (model terminalManageModel) windowCounts() (int, int) {
	open, unknown := 0, 0
	for _, item := range model.items {
		switch model.windowPresence(item.ContextID) {
		case terminalWindowOpen:
			open++
		case terminalWindowUnknown:
			unknown++
		}
	}
	return open, unknown
}

func terminalManageName(item terminalInventoryResult) string {
	if label := strings.TrimSpace(item.Label); label != "" && label != "Terminal" {
		return label
	}
	if item.Identity.Project != "" {
		return item.Identity.Project
	}
	if base := filepath.Base(item.Cwd); base != "." && base != "/" && base != "" {
		return "Terminal · " + base
	}
	if item.CreatedAt != nil {
		return "Terminal · " + item.CreatedAt.Local().Format("02 Jan 15:04")
	}
	return "Terminal"
}

func terminalManageProject(item terminalInventoryResult) string {
	if item.Identity.Project != "" {
		return item.Identity.Project
	}
	return "—"
}

func terminalManageTime(value *time.Time) string {
	if value == nil {
		return "Unknown"
	}
	return value.Local().Format("02 Jan 2006 15:04")
}

func sortTerminalManageItems(items []terminalInventoryResult) []terminalInventoryResult {
	sorted := append([]terminalInventoryResult(nil), items...)
	sort.SliceStable(sorted, func(left, right int) bool {
		if sorted[left].State != sorted[right].State {
			return sorted[left].State == sessionstate.ContextActive
		}
		leftTime := terminalManageSortTime(sorted[left])
		rightTime := terminalManageSortTime(sorted[right])
		if !leftTime.Equal(rightTime) {
			return leftTime.After(rightTime)
		}
		return strings.ToLower(terminalManageName(sorted[left])) < strings.ToLower(terminalManageName(sorted[right]))
	})
	return sorted
}

func terminalManageSortTime(item terminalInventoryResult) time.Time {
	if item.LastFocusedAt != nil {
		return *item.LastFocusedAt
	}
	if item.CreatedAt != nil {
		return *item.CreatedAt
	}
	return time.Time{}
}

func (model *terminalManageModel) rebuildVisible() {
	query := ""
	if model.mode == terminalManageFilterMode {
		query = strings.ToLower(strings.TrimSpace(model.input.Value()))
	}
	model.visible = model.visible[:0]
	for index, item := range model.items {
		haystack := strings.ToLower(strings.Join([]string{
			terminalManageName(item), item.Label, item.Identity.Project, item.Cwd, item.Session, string(item.ContextID),
		}, "\n"))
		if query == "" || strings.Contains(haystack, query) {
			model.visible = append(model.visible, index)
		}
	}
}

func (model *terminalManageModel) restoreSelection() {
	if len(model.visible) == 0 {
		model.cursor = 0
		model.selectedID = ""
		return
	}
	for row, index := range model.visible {
		if model.items[index].ContextID == model.selectedID {
			model.cursor = row
			return
		}
	}
	model.clampCursor()
	model.rememberSelected()
}

func (model *terminalManageModel) clampCursor() {
	if len(model.visible) == 0 {
		model.cursor = 0
		return
	}
	model.cursor = min(max(model.cursor, 0), len(model.visible)-1)
}

func (model *terminalManageModel) rememberSelected() {
	if item, ok := model.selected(); ok {
		model.selectedID = item.ContextID
	}
}

func (model terminalManageModel) selected() (terminalInventoryResult, bool) {
	if model.cursor < 0 || model.cursor >= len(model.visible) {
		return terminalInventoryResult{}, false
	}
	index := model.visible[model.cursor]
	if index < 0 || index >= len(model.items) {
		return terminalInventoryResult{}, false
	}
	return model.items[index], true
}

func (model *terminalManageModel) beginLoad() tea.Cmd {
	model.loading = true
	model.windows = terminalManageUnknownWindows(model.items)
	model.windowErr = nil
	model.loadID++
	return model.loadCommand(model.loadID)
}

func (model terminalManageModel) loadCommand(generation uint64) tea.Cmd {
	return func() tea.Msg {
		snapshot, err := model.operations.Load(model.ctx, model.socket)
		return terminalManageLoadedMsg{generation: generation, snapshot: snapshot, err: err}
	}
}

func (model terminalManageModel) openCommand(id sessionstate.ContextID) tea.Cmd {
	return func() tea.Msg {
		err := model.operations.Open(model.ctx, id, model.socket)
		return terminalManageActionMsg{id: id, action: "Opened " + string(id), err: err}
	}
}

func (model terminalManageModel) stateCommand(id sessionstate.ContextID, state sessionstate.ContextState, action string) tea.Cmd {
	return func() tea.Msg {
		err := model.operations.SetState(model.ctx, id, state)
		return terminalManageActionMsg{id: id, action: action, err: err}
	}
}

func (model terminalManageModel) renameCommand(id sessionstate.ContextID, label string) tea.Cmd {
	return func() tea.Msg {
		err := model.operations.Rename(model.ctx, id, label)
		return terminalManageActionMsg{id: id, action: "Renamed to “" + label + "”", err: err}
	}
}

func (model terminalManageModel) purgeCommand(id sessionstate.ContextID) tea.Cmd {
	return func() tea.Msg {
		message, err := model.operations.Purge(model.ctx, id)
		if message == "" {
			message = "Terminal permanently deleted"
		}
		return terminalManageActionMsg{id: id, action: message, err: err}
	}
}

func (model terminalManageModel) migrateCommand() tea.Cmd {
	return func() tea.Msg {
		message, err := model.operations.Migrate(model.ctx)
		if message == "" {
			message = "Migrated legacy state to SQLite"
		}
		return terminalManageMigrationMsg{action: message, err: err}
	}
}

func executeTerminalManage(
	ctx context.Context,
	arguments []string,
	stdin io.Reader,
	stdout io.Writer,
	structured bool,
	configPath string,
	deps dependencies,
) (commandResult, *commandFailure) {
	set := newFlagSet("terminal manage")
	socket := set.String("socket", "", "Sway IPC socket")
	if err := set.Parse(arguments); err != nil || set.NArg() != 0 {
		return commandResult{}, usageFailure("terminal", "terminal manage accepts only --socket PATH")
	}
	if structured {
		return commandResult{}, usageFailure("terminal", "terminal manage is interactive and does not support --json")
	}
	if *socket != "" && (!filepath.IsAbs(*socket) || filepath.Clean(*socket) != *socket) {
		return commandResult{}, failure("sway_socket", "a valid absolute Sway IPC socket is required", "Run inside Sway or pass --socket PATH.")
	}
	if deps.runTerminalManage == nil {
		return commandResult{}, failure("terminal_manage", "open terminal manager", "terminal manager dependency is unavailable")
	}
	if err := deps.runTerminalManage(ctx, stdin, stdout, configPath, *socket, deps); err != nil {
		return commandResult{}, failure("terminal_manage", "open terminal manager", err.Error())
	}
	return commandResult{Command: "terminal manage", Contexts: []sessionstate.Context{}}, nil
}

func runTerminalManager(ctx context.Context, stdin io.Reader, stdout io.Writer, configPath string, socket string, deps dependencies) error {
	input, inputOK := stdin.(*os.File)
	if !inputOK || !term.IsTerminal(int(input.Fd())) {
		return errors.New("terminal manage requires an interactive terminal on stdin")
	}
	output, outputOK := stdout.(*os.File)
	if !outputOK || !term.IsTerminal(int(output.Fd())) {
		return errors.New("terminal manage requires an interactive terminal on stdout")
	}
	if socket == "" {
		socket = os.Getenv("SWAYSOCK")
	}
	operations := commandTerminalManageOperations{configPath: configPath, deps: deps}
	noColor := os.Getenv("NO_COLOR") != ""
	options := []tea.ProgramOption{tea.WithInput(input), tea.WithOutput(output), tea.WithEnvironment(os.Environ())}
	if noColor {
		options = append(options, tea.WithColorProfile(colorprofile.ASCII))
	}
	model := newTerminalManageModel(operations)
	model.ctx = ctx
	model.socket = socket
	model.noColor = noColor
	model.animate = !noColor
	program := tea.NewProgram(model, options...)
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("run terminal manager: %w", err)
	}
	return nil
}

func terminalManagePulseCommand() tea.Cmd {
	return tea.Tick(terminalManagePulseInterval, func(time.Time) tea.Msg {
		return terminalManagePulseMsg{}
	})
}

type terminalManageRGB struct {
	r, g, b int
}

var terminalManagePalette = [...]terminalManageRGB{
	{r: 34, g: 211, b: 238},
	{r: 96, g: 165, b: 250},
	{r: 167, g: 139, b: 250},
	{r: 244, g: 114, b: 182},
	{r: 251, g: 113, b: 133},
}

func terminalManageGradientRGB(index int, total int, offset int) terminalManageRGB {
	if total < 1 {
		total = 1
	}
	position := (index + offset) % total
	if position < 0 {
		position += total
	}
	scaled := position * len(terminalManagePalette) * 256 / total
	segment := (scaled / 256) % len(terminalManagePalette)
	fraction := scaled % 256
	left := terminalManagePalette[segment]
	right := terminalManagePalette[(segment+1)%len(terminalManagePalette)]
	return terminalManageRGB{
		r: left.r + (right.r-left.r)*fraction/256,
		g: left.g + (right.g-left.g)*fraction/256,
		b: left.b + (right.b-left.b)*fraction/256,
	}
}

func (color terminalManageRGB) scale(percent int) terminalManageRGB {
	return terminalManageRGB{r: color.r * percent / 100, g: color.g * percent / 100, b: color.b * percent / 100}
}

func (color terminalManageRGB) lighten(percent int) terminalManageRGB {
	return terminalManageRGB{
		r: color.r + (255-color.r)*percent/100,
		g: color.g + (255-color.g)*percent/100,
		b: color.b + (255-color.b)*percent/100,
	}
}

func (color terminalManageRGB) hex() string {
	return fmt.Sprintf("#%02X%02X%02X", color.r, color.g, color.b)
}

func terminalManageFrame(content string, width int, phase int, noColor bool) string {
	if width < 2 {
		return content
	}
	innerWidth := width - 2
	lines := strings.Split(content, "\n")
	var output strings.Builder
	output.WriteString(terminalManageFrameEdge('╭', '─', '╮', width, phase, false, noColor))
	for index, line := range lines {
		output.WriteByte('\n')
		output.WriteString(terminalManageFrameGlyph("│", index, len(lines), phase, noColor))
		output.WriteString(terminalManagePad(ansi.Truncate(line, innerWidth, "…"), innerWidth))
		output.WriteString(terminalManageFrameGlyph("│", index, len(lines), phase+40, noColor))
	}
	output.WriteByte('\n')
	output.WriteString(terminalManageFrameEdge('╰', '─', '╯', width, phase+60, true, noColor))
	return output.String()
}

func terminalManageFrameEdge(left rune, fill rune, right rune, width int, phase int, reverse bool, noColor bool) string {
	var output strings.Builder
	for index := range width {
		glyph := fill
		if index == 0 {
			glyph = left
		} else if index == width-1 {
			glyph = right
		}
		colorIndex := index
		if reverse {
			colorIndex = width - index - 1
		}
		output.WriteString(terminalManageFrameGlyph(string(glyph), colorIndex, width, phase, noColor))
	}
	return output.String()
}

func terminalManageFrameGlyph(glyph string, index int, total int, phase int, noColor bool) string {
	if noColor {
		return glyph
	}
	color := terminalManageGradientRGB(index, max(total, 1), phase)
	return lipgloss.NewStyle().Foreground(lipgloss.Color(color.hex())).Render(glyph)
}

func terminalManagePad(value string, width int) string {
	missing := width - ansi.StringWidth(value)
	if missing <= 0 {
		return value
	}
	return value + strings.Repeat(" ", missing)
}
