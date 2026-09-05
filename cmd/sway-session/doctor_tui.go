package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/marang/sway-session/internal/doctor"
)

type doctorCheckedMsg struct{ report doctor.Report }
type doctorPlannedMsg struct {
	plan doctor.Plan
	err  error
}
type doctorAppliedMsg struct {
	result doctor.FixResult
	err    error
}

type doctorModel struct {
	ctx                           context.Context
	operations                    doctorOperations
	report                        doctor.Report
	visible                       []int
	cursor, offset, width, height int
	filter                        textinput.Model
	filtering, help, noColor      bool
	busy                          string
	message                       string
	plan                          *doctor.Plan
}

func newDoctorModel(ctx context.Context, operations doctorOperations) doctorModel {
	filter := textinput.New()
	filter.Placeholder = "Filter checks"
	filter.CharLimit = 128
	return doctorModel{ctx: ctx, operations: operations, width: 80, height: 24, filter: filter, busy: "Checking setup…"}
}

func (model doctorModel) Init() tea.Cmd { return model.check() }
func (model doctorModel) check() tea.Cmd {
	return func() tea.Msg { return doctorCheckedMsg{model.operations.Check(model.ctx)} }
}
func (model doctorModel) selected() (doctor.Check, bool) {
	if model.cursor < 0 || model.cursor >= len(model.visible) {
		return doctor.Check{}, false
	}
	return model.report.Checks[model.visible[model.cursor]], true
}
func (model *doctorModel) refilter(id string) {
	model.visible = nil
	query := strings.ToLower(model.filter.Value())
	for index, check := range model.report.Checks {
		if strings.Contains(strings.ToLower(check.ID+" "+check.Title+" "+string(check.Status)+" "+check.Detail), query) {
			model.visible = append(model.visible, index)
		}
	}
	model.cursor = min(model.cursor, max(0, len(model.visible)-1))
	for row, index := range model.visible {
		if model.report.Checks[index].ID == id {
			model.cursor = row
			break
		}
	}
}

func (model doctorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = msg.Width, msg.Height
		model.offset = 0
		model.filter.SetWidth(max(1, msg.Width-8))
		return model, nil
	case doctorCheckedMsg:
		selected, _ := model.selected()
		model.report, model.busy = msg.report, ""
		model.refilter(selected.ID)
		return model, nil
	case doctorPlannedMsg:
		model.busy = ""
		if msg.err != nil {
			model.message = "Cannot prepare repair: " + doctorText(msg.err.Error())
			return model, nil
		}
		model.plan, model.offset = &msg.plan, 0
		return model, nil
	case doctorAppliedMsg:
		model.plan, model.offset = nil, 0
		if msg.err != nil {
			model.busy = ""
			model.message = "Repair failed: " + doctorText(msg.err.Error())
			return model, nil
		}
		model.message = doctorText(msg.result.Message)
		for _, path := range msg.result.Backups {
			model.message += " Backup: " + doctorText(path)
		}
		model.busy = "Rechecking setup…"
		return model, model.check()
	case tea.KeyPressMsg:
		key := msg.String()
		// Do not abandon a file transaction in flight. The repair engine still
		// observes cancellation from the parent context and performs rollback.
		if model.busy == "Applying repair…" {
			return model, nil
		}
		if key == "ctrl+c" {
			return model, tea.Quit
		}
		if model.filtering {
			if key == "enter" || key == "esc" {
				model.filtering = false
				model.filter.Blur()
				return model, nil
			}
			var command tea.Cmd
			model.filter, command = model.filter.Update(msg)
			model.cursor, model.offset = 0, 0
			model.refilter("")
			return model, command
		}
		if model.help {
			if key == "esc" || key == "?" || key == "q" {
				model.help = false
			}
			return model, nil
		}
		if key == "q" {
			return model, tea.Quit
		}
		if model.width < 48 || model.height < 16 || model.busy != "" {
			return model, nil
		}
		if model.plan != nil {
			switch key {
			case "esc", "n":
				model.plan, model.offset, model.message = nil, 0, "Repair cancelled; no files changed."
			case "y":
				plan := *model.plan
				model.busy = "Applying repair…"
				return model, func() tea.Msg {
					result, err := model.operations.Apply(model.ctx, plan)
					return doctorAppliedMsg{result, err}
				}
			case "down", "j":
				model.scroll(1)
			case "up", "k":
				model.scroll(-1)
			case "pgdown":
				model.scroll(model.bodyHeight())
			case "pgup":
				model.scroll(-model.bodyHeight())
			}
			return model, nil
		}
		switch key {
		case "down", "j":
			model.cursor = min(model.cursor+1, max(0, len(model.visible)-1))
			model.offset = 0
		case "up", "k":
			model.cursor = max(model.cursor-1, 0)
			model.offset = 0
		case "pgdown":
			model.scroll(model.bodyHeight())
		case "pgup":
			model.scroll(-model.bodyHeight())
		case "/":
			model.filtering = true
			return model, model.filter.Focus()
		case "esc":
			model.filter.SetValue("")
			model.refilter("")
			model.offset = 0
		case "?":
			model.help = true
		case "r":
			model.busy = "Checking setup…"
			model.message = ""
			return model, model.check()
		case "f":
			if check, ok := model.selected(); ok && check.FixID != "" {
				model.busy = "Preparing preview…"
				return model, func() tea.Msg {
					plan, err := model.operations.Plan(model.ctx, check.FixID)
					return doctorPlannedMsg{plan, err}
				}
			}
		}
	}
	return model, nil
}

func (model doctorModel) bodyHeight() int { return max(1, model.height-9) }
func (model doctorModel) detailWidth() int {
	if model.plan == nil && model.width >= 100 {
		return model.width - model.width*2/5 - 5
	}
	return max(1, model.width-4)
}
func (model doctorModel) detailLines() []string {
	var lines []string
	if model.plan != nil {
		lines = append(lines, "Repair: "+doctorText(model.plan.ID), doctorText(model.plan.Summary), "")
		for _, change := range model.plan.Changes {
			lines = append(lines, doctorText(change.Path))
			for _, line := range strings.Split(change.Preview, "\n") {
				lines = append(lines, doctorText(line))
			}
			lines = append(lines, "")
		}
		lines = append(lines, "Only these configuration edits will be applied.", "Existing files receive private backups. No reload is performed.")
	} else if check, ok := model.selected(); ok {
		lines = append(lines, doctorText(check.Title), "["+string(check.Status)+"] "+doctorText(check.ID), "", doctorText(check.Detail))
		if check.Hint != "" {
			lines = append(lines, "", "Next: "+doctorText(check.Hint))
		}
		for _, evidence := range check.Evidence {
			lines = append(lines, "Evidence: "+doctorText(evidence))
		}
		if check.FixID != "" {
			lines = append(lines, "", "[f] Preview fix: "+doctorText(check.FixID))
		}
	} else {
		lines = append(lines, "No matching checks. [Esc] Clear filter.")
	}
	return strings.Split(ansi.Hardwrap(strings.Join(lines, "\n"), model.detailWidth(), true), "\n")
}
func (model *doctorModel) scroll(delta int) {
	model.offset = max(0, min(model.offset+delta, max(0, len(model.detailLines())-model.detailHeight())))
}
func (model doctorModel) detailHeight() int {
	if model.plan == nil && model.width < 100 {
		return max(1, model.bodyHeight()-min(5, model.bodyHeight()/2)-1)
	}
	return model.bodyHeight()
}
func (model doctorModel) renderList(width, height int, styles terminalManageStyles) []string {
	start := max(0, min(model.cursor-height+1, max(0, len(model.visible)-height)))
	lines := make([]string, height)
	for row := range height {
		index := start + row
		if index >= len(model.visible) {
			break
		}
		check := model.report.Checks[model.visible[index]]
		prefix := "  "
		if index == model.cursor {
			prefix = "› "
		}
		line := ansi.Truncate(prefix+"["+string(check.Status)+"] "+doctorText(check.Title), width, "…")
		if index == model.cursor {
			line = styles.selected.Render(terminalManagePad(line, width))
		}
		lines[row] = line
	}
	return lines
}
func (model doctorModel) View() tea.View {
	if model.width < 48 || model.height < 16 {
		view := tea.NewView("Resize to at least 48×16. [q] Quit")
		view.AltScreen = true
		return view
	}
	styles := newTerminalManageStyles(model.noColor, 0)
	width := model.width - 4
	header := styles.title.Render("Setup doctor") + fmt.Sprintf(" · %d checks", len(model.report.Checks))
	if model.busy != "" {
		header += " · " + model.busy
	}
	filter := "[/] Filter · " + doctorText(model.filter.Value())
	if model.filtering {
		filter = model.filter.View()
	}
	body := make([]string, model.bodyHeight())
	if model.help {
		copy(body, []string{"Doctor inspects setup without starting sessions or services.", "[↑/↓ or j/k] Select check    [PgUp/PgDn] Scroll details", "[/] Filter   [Esc] Clear filter   [r] Refresh", "[f] Prepare a repair preview; no file changes yet.", "In preview: [y] Apply with backups, [n/Esc] Cancel.", "Unknown or conflicting configuration requires manual edits.", "Optional checks can be unavailable without blocking use.", "[? / Esc] Back"})
	} else {
		details := model.detailLines()
		offset := min(model.offset, max(0, len(details)-model.detailHeight()))
		visible := details[offset:min(len(details), offset+model.detailHeight())]
		if model.plan != nil {
			copy(body, visible)
		} else if model.width >= 100 {
			listWidth := model.width * 2 / 5
			list := model.renderList(listWidth, len(body), styles)
			for i := range body {
				body[i] = terminalManagePad(list[i], listWidth) + " │ "
				if i < len(visible) {
					body[i] += visible[i]
				}
			}
		} else {
			listHeight := min(5, model.bodyHeight()/2)
			copy(body, model.renderList(width, listHeight, styles))
			copy(body[listHeight+1:], visible)
		}
	}
	footer := "[↑/↓ j/k] Select  [/] Filter  [PgUp/PgDn] Details"
	actions := "[r] Refresh  [?] Help  [q] Quit"
	if check, ok := model.selected(); ok && check.FixID != "" {
		actions = "[f] Preview fix  " + actions
	}
	if model.plan != nil {
		footer = "[↑/↓ PgUp/PgDn] Scroll preview"
		actions = "[y] Apply + backup  [n/Esc] Cancel  [q] Quit"
	}
	if model.width < 70 && model.plan == nil {
		footer = "[↑/↓ j/k] Select  [/] Filter  [PgUp/Dn] More"
		actions = "[f] Fix preview  [r] Check  [?] Help  [q] Quit"
	}
	message := model.message
	if model.busy != "" {
		message = model.busy
	}
	lines := []string{header, filter, ""}
	lines = append(lines, body...)
	lines = append(lines, styles.accent.Render(ansi.Truncate(message, width, "…")), footer, actions)
	view := tea.NewView(terminalManageFrame(strings.Join(lines, "\n"), model.width, 0, model.noColor))
	view.AltScreen = true
	return view
}

func runDoctorUI(ctx context.Context, stdin io.Reader, stdout io.Writer, operations doctorOperations) error {
	model := newDoctorModel(ctx, operations)
	model.noColor = os.Getenv("NO_COLOR") != ""
	options := []tea.ProgramOption{tea.WithContext(ctx), tea.WithInput(stdin), tea.WithOutput(stdout), tea.WithEnvironment(os.Environ())}
	if model.noColor {
		options = append(options, tea.WithColorProfile(colorprofile.ASCII))
	}
	_, err := tea.NewProgram(model, options...).Run()
	return err
}
