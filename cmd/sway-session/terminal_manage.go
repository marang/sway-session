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
	sessionstate "github.com/marang/sway-title-animator/internal/session"
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
	items      []terminalInventoryResult
	err        error
}

type terminalManageActionMsg struct {
	id     sessionstate.ContextID
	action string
	err    error
}

type terminalManageModel struct {
	ctx        context.Context
	operations terminalManageOperations
	socket     string
	items      []terminalInventoryResult
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
	noColor    bool
}

func newTerminalManageModel(operations terminalManageOperations) terminalManageModel {
	noColor := os.Getenv("NO_COLOR") != ""
	input := textinput.New()
	input.CharLimit = 128
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
			model.items = sortTerminalManageItems(message.items)
			model.rebuildVisible()
			model.restoreSelection()
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
	styles := newTerminalManageStyles(model.noColor)
	width := model.width
	if width <= 0 {
		width = 80
	}
	var output strings.Builder
	output.WriteString(styles.title.Render("Persistent terminals"))
	output.WriteString(styles.muted.Render(fmt.Sprintf("  %d saved", len(model.items))))
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
		if width >= 100 {
			listWidth = width/2 - 2
		}
		list := model.renderList(styles, listWidth)
		if width >= 100 {
			detail := model.renderDetails(styles, width-listWidth-3)
			output.WriteString("\n" + lipgloss.JoinHorizontal(lipgloss.Top, list, "   ", detail))
		} else if model.mode == terminalManagePurgeMode {
			output.WriteString("\n" + list)
		} else {
			output.WriteString("\n" + list + "\n\n" + model.renderDetails(styles, width))
		}
		output.WriteByte('\n')
	}

	switch model.mode {
	case terminalManageFilterMode:
		output.WriteString("\nFilter  " + model.inputView())
	case terminalManageRenameMode:
		output.WriteString("\nRename  " + model.inputView())
	case terminalManagePurgeMode:
		if item, ok := model.selected(); ok {
			output.WriteString("\n" + styles.danger.Render("Delete "+terminalManageName(item)+" permanently?"))
			output.WriteString("\nThis removes the sway-session entry and its Herdr state.")
			output.WriteString("\n" + styles.muted.Render(item.Cwd))
			output.WriteString("\n[y/n] delete or cancel  [Esc] cancel")
		}
	case terminalManageHelpMode:
		output.WriteString("\n" + styles.accent.Render("Keyboard help"))
		output.WriteString("\nEsc/? close help   q quit   ↑/↓ or j/k select")
		output.WriteString("\nEnter/o open   e rename   a archive/activate   / filter   d delete   r refresh")
	default:
		if model.pending {
			output.WriteString("\nWorking…")
		} else {
			output.WriteString("\nq quit  ↑/↓ or j/k select  Enter open  e rename  a archive/activate  d delete  / filter  ? help")
		}
	}
	if model.err != nil {
		output.WriteString("\n" + styles.danger.Render("Error: "+terminalManageSentence(model.err.Error())))
	} else if model.status != "" {
		output.WriteString("\n" + styles.success.Render(model.status))
	}
	return terminalManageFit(output.String(), width, model.height)
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

func newTerminalManageStyles(noColor bool) terminalManageStyles {
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
	styles.selected = styles.selected.Foreground(lipgloss.Color("#FDE68A"))
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
		state := "● active"
		if item.State == sessionstate.ContextArchived {
			state = "○ archived"
		}
		line := fmt.Sprintf("%s%s  %s", cursor, state, terminalManageName(item))
		line = ansi.Truncate(line, max(width, 1), "…")
		if row == model.cursor {
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
	if model.height > 0 {
		if model.width >= 100 {
			reserved := 7
			switch model.mode {
			case terminalManagePurgeMode:
				reserved += 3
			case terminalManageHelpMode:
				reserved += 2
			case terminalManageFilterMode, terminalManageRenameMode:
				reserved++
			}
			rows = max(model.height-reserved, 1)
		} else {
			reserved := 15
			switch model.mode {
			case terminalManagePurgeMode:
				reserved += 3
			case terminalManageHelpMode:
				reserved += 2
			case terminalManageFilterMode, terminalManageRenameMode:
				reserved++
			}
			rows = max(model.height-reserved, 1)
		}
	}
	rows = min(rows, len(model.visible))
	start := max(model.cursor-rows/2, 0)
	if start+rows > len(model.visible) {
		start = max(len(model.visible)-rows, 0)
	}
	return start, start + rows
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

func (model terminalManageModel) renderDetails(styles terminalManageStyles, width int) string {
	item, ok := model.selected()
	if !ok {
		return ""
	}
	lines := []string{
		styles.accent.Render(terminalManageName(item)),
		"State       " + string(item.State),
		"Last active " + terminalManageTime(item.LastFocusedAt),
		"Created     " + terminalManageTime(item.CreatedAt),
		"Project     " + terminalManageProject(item),
		"Directory   " + item.Cwd,
		"Session     " + item.Session,
		"Context     " + string(item.ContextID),
	}
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], max(width, 1), "…")
	}
	return strings.Join(lines, "\n")
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
	model.loadID++
	return model.loadCommand(model.loadID)
}

func (model terminalManageModel) loadCommand(generation uint64) tea.Cmd {
	return func() tea.Msg {
		items, err := model.operations.List(model.ctx)
		return terminalManageLoadedMsg{generation: generation, items: items, err: err}
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
	program := tea.NewProgram(model, options...)
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("run terminal manager: %w", err)
	}
	return nil
}
