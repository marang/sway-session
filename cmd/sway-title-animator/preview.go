package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"golang.org/x/term"
)

type previewLayout struct {
	width        int
	height       int
	labelColumns int
	artColumns   int
}

type previewTickMsg time.Time

type previewModel struct {
	names       []string
	viewport    viewport.Model
	layout      previewLayout
	fps         float64
	phase       int
	lastMotion  int
	lastAudio   audioSnapshot
	hasAudio    bool
	ready       bool
	layoutError error
}

func previewPresetNames() []string {
	names := make([]string, 0, len(animationPresets))
	seen := map[string]bool{}
	for _, name := range rotationPresets {
		if seen[name] {
			continue
		}
		if _, ok := animationPresets[name]; !ok {
			continue
		}
		names = append(names, name)
		seen[name] = true
	}

	remaining := make([]string, 0, len(animationPresets)-len(names))
	for name := range animationPresets {
		if !seen[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	return append(names, remaining...)
}

func calculatePreviewLayout(names []string, width int, height int) (previewLayout, error) {
	if len(names) == 0 {
		return previewLayout{}, errors.New("no animation presets are available to preview")
	}
	labelColumns := 0
	for _, name := range names {
		labelColumns = max(labelColumns, terminalColumns(name))
	}
	minimumWidth := labelColumns + 2 + 8
	if width < minimumWidth {
		return previewLayout{}, fmt.Errorf("preview needs at least %d terminal columns (currently %d)", minimumWidth, width)
	}
	if height < 2 {
		return previewLayout{}, fmt.Errorf("preview needs at least 2 terminal rows (currently %d)", height)
	}

	availableArtColumns := width - labelColumns - 2
	artColumns := min(settings.MaxArtColumns, availableArtColumns)
	return previewLayout{
		width:        width,
		height:       height,
		labelColumns: labelColumns,
		artColumns:   max(0, artColumns),
	}, nil
}

func previewLines(names []string, layout previewLayout, phase int) []string {
	lines := make([]string, 0, len(names)*2-1)
	motion := motionPhase(phase)
	for index, name := range names {
		label := name + strings.Repeat(" ", layout.labelColumns-terminalColumns(name))
		art := animationPresets[name](layout.artColumns, motion)
		art = truncateTerminalColumns(art, layout.artColumns)
		lines = append(lines, label+"  "+art)
		if index < len(names)-1 {
			lines = append(lines, "")
		}
	}
	return lines
}

func newPreviewModel(names []string, fps float64) previewModel {
	model := previewModel{
		names:      names,
		viewport:   viewport.New(),
		fps:        fps,
		lastMotion: motionPhase(0),
		lastAudio:  currentAudioSnapshot(),
		hasAudio:   presetListUsesAudio(names),
	}
	model.viewport.MouseWheelEnabled = true
	model.viewport.MouseWheelDelta = 3
	return model
}

func (model previewModel) Init() tea.Cmd {
	return previewTick(model.fps)
}

func (model previewModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.KeyPressMsg:
		switch message.String() {
		case "q", "ctrl+c":
			return model, tea.Quit
		case "up", "k":
			model.viewport.ScrollUp(1)
			return model, nil
		case "down", "j":
			model.viewport.ScrollDown(1)
			return model, nil
		case "pgup":
			model.viewport.PageUp()
			return model, nil
		case "pgdown":
			model.viewport.PageDown()
			return model, nil
		case "home":
			model.viewport.GotoTop()
			return model, nil
		case "end":
			model.viewport.GotoBottom()
			return model, nil
		}
	case tea.WindowSizeMsg:
		model.resize(message.Width, message.Height)
		return model, nil
	case previewTickMsg:
		model.phase++
		motion := motionPhase(model.phase)
		audio := model.lastAudio
		if model.hasAudio {
			audio = currentAudioSnapshot()
		}
		if motion != model.lastMotion || !sameAudioVisual(audio, model.lastAudio) {
			model.lastMotion = motion
			model.lastAudio = audio
			model.refreshContent()
		}
		return model, previewTick(model.fps)
	}

	var command tea.Cmd
	model.viewport, command = model.viewport.Update(message)
	return model, command
}

func (model *previewModel) resize(width int, height int) {
	layout, err := calculatePreviewLayout(model.names, width, height)
	model.layoutError = err
	model.ready = true
	if err != nil {
		return
	}
	model.layout = layout
	model.viewport.SetWidth(width)
	model.viewport.SetHeight(height - 1)
	model.refreshContent()
}

func (model *previewModel) refreshContent() {
	if model.layoutError != nil || model.layout.width == 0 {
		return
	}
	model.viewport.SetContent(strings.Join(previewLines(model.names, model.layout, model.phase), "\n"))
}

func (model previewModel) View() tea.View {
	var content string
	if !model.ready {
		content = "Preparing animation preview…"
	} else if model.layoutError != nil {
		content = model.layoutError.Error()
	} else {
		position := fmt.Sprintf("%3.0f%%", model.viewport.ScrollPercent()*100)
		help := truncateTerminalColumns(
			fmt.Sprintf("↑/↓ scroll  PgUp/PgDn page  Home/End jump  q quit  %s", position),
			model.layout.width,
		)
		content = model.viewport.View() + "\n" + help
	}
	view := tea.NewView(content)
	view.AltScreen = true
	return view
}

func previewTick(fps float64) tea.Cmd {
	return tea.Tick(frameDuration(fps), func(now time.Time) tea.Msg {
		return previewTickMsg(now)
	})
}

func runPreview(output *os.File, fps float64) error {
	if !term.IsTerminal(int(output.Fd())) {
		return errors.New("preview requires an interactive terminal on stdout")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("preview requires an interactive terminal on stdin")
	}

	names := previewPresetNames()
	if len(names) == 0 {
		return errors.New("no animation presets are available to preview")
	}
	stopAudio := func() {}
	if presetListUsesAudio(names) {
		stopAudio = startDefaultAudioMonitor()
	}
	defer stopAudio()

	program := tea.NewProgram(
		newPreviewModel(names, fps),
		tea.WithInput(os.Stdin),
		tea.WithOutput(output),
	)
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("run terminal preview: %w", err)
	}
	return nil
}

func sameAudioVisual(first audioSnapshot, second audioSnapshot) bool {
	return first.Bands == second.Bands &&
		first.Level == second.Level &&
		first.Active == second.Active
}

func terminalColumns(value string) int {
	columns := 0
	for _, character := range value {
		switch {
		case character == '\t':
			columns += 4
		case character < 0x20 || (character >= 0x7f && character < 0xa0):
			continue
		case unicode.Is(unicode.Mn, character), unicode.Is(unicode.Me, character):
			continue
		case isWideTerminalRune(character):
			columns += 2
		default:
			columns++
		}
	}
	return columns
}

func truncateTerminalColumns(value string, maxColumns int) string {
	if maxColumns <= 0 {
		return ""
	}
	used := 0
	output := make([]rune, 0, len([]rune(value)))
	for _, character := range value {
		width := terminalColumns(string(character))
		if used+width > maxColumns {
			break
		}
		output = append(output, character)
		used += width
	}
	return string(output)
}

func isWideTerminalRune(character rune) bool {
	return character >= 0x1100 && (character <= 0x115f ||
		character == 0x2329 || character == 0x232a ||
		(character >= 0x2e80 && character <= 0xa4cf && character != 0x303f) ||
		(character >= 0xac00 && character <= 0xd7a3) ||
		(character >= 0xf900 && character <= 0xfaff) ||
		(character >= 0xfe10 && character <= 0xfe19) ||
		(character >= 0xfe30 && character <= 0xfe6f) ||
		(character >= 0xff00 && character <= 0xff60) ||
		(character >= 0xffe0 && character <= 0xffe6) ||
		(character >= 0x1f300 && character <= 0x1faff) ||
		(character >= 0x20000 && character <= 0x3fffd))
}
