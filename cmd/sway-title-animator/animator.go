package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/marang/sway-title-animator/internal/swayipc"
	"github.com/marang/sway-title-animator/internal/titleindicator"
)

type TitleAnimator struct {
	ipc                  *swayipc.Client
	titleSetter          func(int64, string) error
	lastFormats          map[int64]string
	lastFormatSetAt      map[int64]time.Time
	processLabels        map[int]cachedProcessLabel
	windowsByID          map[int64]nodeWithParent
	focusedID            int64
	focusedBase          string
	focusedArtColumns    int
	focusedAnimationKey  int
	focusedBaseCheckedAt time.Time
	focusedNeedsRefresh  bool
	focusedCacheIsActive bool
	hasFocus             bool
	lastSetError         string
	lastSetErrorAt       time.Time
	errorReporter        func(error)
}

func NewTitleAnimator(ipc *swayipc.Client) *TitleAnimator {
	return &TitleAnimator{
		ipc:             ipc,
		lastFormats:     map[int64]string{},
		lastFormatSetAt: map[int64]time.Time{},
		processLabels:   map[int]cachedProcessLabel{},
		windowsByID:     map[int64]nodeWithParent{},
		errorReporter: func(err error) {
			fmt.Fprintf(os.Stderr, "Unable to update title format: %v\n", err)
		},
	}
}

func (animator *TitleAnimator) RefreshTree(phase int) (*Node, error) {
	if animator == nil || animator.ipc == nil {
		return nil, errors.New("title animator has no Sway IPC client")
	}
	message, err := animator.ipc.Request(swayipc.GetTree, nil)
	if err != nil {
		return nil, err
	}
	if message.Type != swayipc.GetTree {
		return nil, fmt.Errorf("unexpected Sway tree response type %d", message.Type)
	}
	var root Node
	if err := json.Unmarshal(message.Payload, &root); err != nil {
		return nil, fmt.Errorf("decode Sway tree: %w", err)
	}

	all := []nodeWithParent{}
	walk(&root, nil, &all)

	windows := []nodeWithParent{}
	newWindows := map[int64]nodeWithParent{}
	livePIDs := map[int]bool{}
	animator.focusedID = 0
	animator.hasFocus = false
	for _, item := range all {
		if !isWindow(item.node) {
			continue
		}
		windows = append(windows, item)
		newWindows[item.node.ID] = item
		if item.node.PID > 0 {
			livePIDs[item.node.PID] = true
		}
		if item.node.Focused {
			animator.focusedID = item.node.ID
			animator.hasFocus = true
		}
	}
	animator.windowsByID = newWindows
	animator.focusedCacheIsActive = false

	for id := range animator.lastFormats {
		if _, ok := newWindows[id]; !ok {
			delete(animator.lastFormats, id)
			delete(animator.lastFormatSetAt, id)
		}
	}
	for pid := range animator.processLabels {
		if !livePIDs[pid] {
			delete(animator.processLabels, pid)
		}
	}
	for _, item := range windows {
		animator.ApplyNode(item.node, item.parent, phase)
	}
	return &root, nil
}

func (animator *TitleAnimator) WindowLabel(node *Node) string {
	label := appLabel(node)
	if !settings.DetectChildProcess || node.PID <= 0 || !isTerminalWindow(node) {
		return label
	}
	child := animator.CachedChildProcessLabel(node.PID)
	if child == "" {
		return label
	}
	return label + " › " + truncateColumns(child, 18)
}

func (animator *TitleAnimator) CachedChildProcessLabel(pid int) string {
	if cached, ok := animator.processLabels[pid]; ok && time.Since(cached.checkedAt) < 750*time.Millisecond {
		return cached.label
	}
	label := childProcessLabel(pid)
	animator.processLabels[pid] = cachedProcessLabel{
		label:     label,
		checkedAt: time.Now(),
	}
	return label
}

func (animator *TitleAnimator) ApplyNode(node *Node, parent *Node, phase int) {
	base, artColumns := animator.NodeFrameParts(node, parent)
	value := base
	if artColumns > 0 {
		value += " " + activityArt(artColumns, phase)
	}
	if animator.hasFocus && node.ID == animator.focusedID {
		animator.focusedBase = base
		animator.focusedArtColumns = artColumns
		animator.focusedAnimationKey = animationFrameKey(phase)
		animator.focusedBaseCheckedAt = time.Now()
		animator.focusedNeedsRefresh = settings.DetectChildProcess && node.PID > 0 && isTerminalWindow(node)
		animator.focusedCacheIsActive = true
	}
	if !animator.shouldSetTitleFormat(node.ID, value) {
		return
	}
	if err := animator.SetTitleFormat(node.ID, value); err == nil {
		animator.rememberTitleFormat(node.ID, value)
	} else {
		animator.reportSetError(err)
	}
}

func (animator *TitleAnimator) NodeFrameParts(node *Node, parent *Node) (string, int) {
	icon := iconFor(node)
	label := animator.WindowLabel(node)
	statusText := visibleStatusText(node)
	indicator := applicationIndicator(node.Marks, node.ID)
	visiblePrefix := fmt.Sprintf("%s%s %s%s: ", indicator, icon, label, statusText)
	title := node.Name
	if animator.hasFocus && node.ID == animator.focusedID {
		tabColumns := int(float64(tabWidthPX(node, parent)) / settings.ApproxCharWidth)
		prefixColumns := textColumns(visiblePrefix)
		maxTitleColumns := min(textColumns(node.Name), max(settings.TitleReserveColumns, tabColumns-prefixColumns-24))
		title = truncateColumns(node.Name, maxTitleColumns)
		fixedColumns := prefixColumns + textColumns(title) + 1
		return visiblePrefix + title, max(0, tabColumns-fixedColumns+2)
	}
	return visiblePrefix + title, 0
}

func applicationIndicator(marks []string, containerID int64) string {
	state, ok := titleindicator.FromMarks(marks, containerID)
	if !ok {
		return ""
	}
	var glyph string
	switch state {
	case titleindicator.Unregistered:
		glyph = applicationIndicatorUnregistered
	case titleindicator.Pending:
		glyph = applicationIndicatorPending
	case titleindicator.Registered:
		glyph = applicationIndicatorRegistered
	case titleindicator.Pinned:
		glyph = applicationIndicatorPinned
	}
	if glyph == "" {
		return ""
	}
	return glyph + " "
}

func (animator *TitleAnimator) ApplyFocusedFrame(phase int) {
	key := animationFrameKey(phase)
	if animator.focusedCacheIsActive && animator.focusedAnimationKey == key {
		if value, ok := animator.lastFormats[animator.focusedID]; ok && animator.shouldSetTitleFormat(animator.focusedID, value) {
			if err := animator.SetTitleFormat(animator.focusedID, value); err == nil {
				animator.rememberTitleFormat(animator.focusedID, value)
			} else {
				animator.reportSetError(err)
			}
		}
		return
	}
	animator.focusedAnimationKey = key

	value := animator.focusedBase
	if animator.focusedArtColumns > 0 {
		value += " " + activityArt(animator.focusedArtColumns, phase)
	}
	if !animator.shouldSetTitleFormat(animator.focusedID, value) {
		return
	}
	if err := animator.SetTitleFormat(animator.focusedID, value); err == nil {
		animator.rememberTitleFormat(animator.focusedID, value)
	} else {
		animator.reportSetError(err)
	}
}

func (animator *TitleAnimator) Tick(phase int) {
	if !animator.hasFocus || !animator.focusedCacheIsActive {
		animator.RefreshTree(phase)
		return
	}
	if _, ok := animator.windowsByID[animator.focusedID]; !ok {
		animator.RefreshTree(phase)
		return
	}
	if animator.focusedNeedsRefresh && time.Since(animator.focusedBaseCheckedAt) >= 750*time.Millisecond {
		item := animator.windowsByID[animator.focusedID]
		animator.ApplyNode(item.node, item.parent, phase)
		return
	}
	animator.ApplyFocusedFrame(phase)
}

func (animator *TitleAnimator) FramesUntilNextWake(phase int) int {
	frames := framesUntilNextAnimationKey(phase)
	if !animator.focusedNeedsRefresh || !animator.focusedCacheIsActive {
		return frames
	}
	elapsed := time.Since(animator.focusedBaseCheckedAt)
	if elapsed >= 750*time.Millisecond {
		return 1
	}
	refreshFrames := int(math.Ceil(float64((750*time.Millisecond)-elapsed) / float64(frameDuration(settings.FPS))))
	return max(1, min(frames, refreshFrames))
}

func (animator *TitleAnimator) ResetAll() error {
	var resetErrors []error
	for conID := range animator.lastFormats {
		if err := animator.SetTitleFormat(conID, "%title"); err != nil {
			resetErrors = append(resetErrors, fmt.Errorf("reset container %d: %w", conID, err))
		}
	}
	animator.lastFormats = map[int64]string{}
	animator.lastFormatSetAt = map[int64]time.Time{}
	return errors.Join(resetErrors...)
}

func (animator *TitleAnimator) SetTitleFormat(conID int64, value string) error {
	if animator.titleSetter != nil {
		return animator.titleSetter(conID, value)
	}
	command := fmt.Sprintf("[con_id=%d] title_format %s", conID, quoteSwayString(value))
	message, err := animator.ipc.Request(swayipc.RunCommand, []byte(command))
	if err != nil {
		return err
	}
	return swayipc.CheckRunCommandResponse(message)
}

func (animator *TitleAnimator) shouldSetTitleFormat(conID int64, value string) bool {
	if animator.lastFormats[conID] != value {
		return true
	}
	lastSetAt, ok := animator.lastFormatSetAt[conID]
	return !ok || time.Since(lastSetAt) >= titleReassertInterval
}

func (animator *TitleAnimator) rememberTitleFormat(conID int64, value string) {
	animator.lastFormats[conID] = value
	animator.lastFormatSetAt[conID] = time.Now()
}

func (animator *TitleAnimator) reportSetError(err error) {
	if err == nil || animator.errorReporter == nil {
		return
	}
	message := err.Error()
	if message == animator.lastSetError && time.Since(animator.lastSetErrorAt) < 5*time.Second {
		return
	}
	animator.lastSetError = message
	animator.lastSetErrorAt = time.Now()
	animator.errorReporter(err)
}

type statusBadge struct {
	text   string
	color  string
	weight string
}

func statusBadges(node *Node) []statusBadge {
	badges := []statusBadge{}
	if node.Urgent {
		badges = append(badges, statusBadge{text: "!", color: "#cd2d2d", weight: "bold"})
	}
	if node.InhibitIdle {
		badges = append(badges, statusBadge{text: "idle", color: "#666666"})
	}
	if node.SandboxEngine != nil || node.SandboxAppID != nil || node.SandboxInstance != nil {
		badges = append(badges, statusBadge{text: "sbx", color: "#666666"})
	}
	visibleMarks := visibleMarkCount(node.Marks)
	if visibleMarks > 0 {
		badges = append(badges, statusBadge{text: fmt.Sprintf("◇%d", visibleMarks), color: "#666666"})
	}
	return badges
}

func visibleMarkCount(marks []string) int {
	count := 0
	for _, mark := range marks {
		// Project-owned control marks are compositor metadata, not user badges.
		// Filtering the stable mark prefix is presentation-only: the animator
		// never interprets its identity or opens session state.
		if mark != "" && !strings.HasPrefix(mark, "_") && !strings.HasPrefix(mark, "persist:") {
			count++
		}
	}
	return count
}

func visibleStatusText(node *Node) string {
	badges := statusBadges(node)
	if len(badges) == 0 {
		return ""
	}
	parts := make([]string, 0, len(badges))
	for _, badge := range badges {
		parts = append(parts, badge.text)
	}
	return " " + strings.Join(parts, " ")
}

func quoteSwayString(value string) string {
	escaped := strings.ReplaceAll(value, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
	return "\"" + escaped + "\""
}
