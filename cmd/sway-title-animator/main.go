package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const (
	defaultFPS             = 25.0
	defaultMotion          = 0.22
	defaultApproxCharWidth = 8.5
	defaultMaxArtColumns   = 220
	defaultTitleReserve    = 18
	defaultShowcaseHold    = 260
	defaultShowcaseBlend   = 75

	ipcRunCommand = 0
	ipcSubscribe  = 2
	ipcGetTree    = 4
)

const defaultConfigContents = `[settings]
fps = 25
motion = 0.22
approx_char_width = 8.5
max_art_columns = 220
title_reserve_columns = 18
showcase_hold_frames = 260
showcase_blend_frames = 75
detect_child_process = true

[showcase]
presets = [
  "loom",
  "aurora",
  "spectrum",
  "radar",
  "constellation",
  "circuit",
  "braid",
  "comet",
]

[glyphs]
aurora_bars = "▁▂▃▄▅▆▇█"
aurora_dots = "·∙•"
aurora_sparkles = "✦✧"
shade_ramp = " ·░▒▓█"
spectrum_bars = "▁▂▃▄▅▆▇█"
spectrum_left = "⟨([{<"
spectrum_right = ">}])⟩"
radar_levels = " ·┄─═●"
radar_sweep = "◜◠◝◞◡◟"
constellation_stars = "✦✧✶✷"
circuit_tiles = "─╴╶═╡╞╪┄╍╾╼"
comet_trail = "·░▒▓"

[icons]
alacritty = "▣"
firefox = "🌐"
riotbox = "♪"

[animation.ribbon]
fill = true
frames = [
  "··░░▒▒▓▓▒▒░░··  ",
  "·░░▒▒▓▓▒▒░░··  ·",
  "░░▒▒▓▓▒▒░░··  ··",
  "░▒▒▓▓▒▒░░··  ··░",
  "▒▒▓▓▒▒░░··  ··░░",
  "▒▓▓▒▒░░··  ··░░▒",
  "▓▓▒▒░░··  ··░░▒▒",
  "▓▒▒░░··  ··░░▒▒▓",
]

[animation.shutter]
fill = true
frames = [
  "░░▒▒▓▓██▓▓▒▒░░··",
  "·░░▒▒▓▓██▓▓▒▒░░·",
  "··░░▒▒▓▓██▓▓▒▒░░",
  "░··░░▒▒▓▓██▓▓▒▒░",
]
`

var (
	animationPreset = "showcase"
	settings        = Settings{
		FPS:                 defaultFPS,
		Motion:              defaultMotion,
		ApproxCharWidth:     defaultApproxCharWidth,
		MaxArtColumns:       defaultMaxArtColumns,
		TitleReserveColumns: defaultTitleReserve,
		ShowcaseHoldFrames:  defaultShowcaseHold,
		ShowcaseBlendFrames: defaultShowcaseBlend,
		DetectChildProcess:  true,
	}

	auroraBars        = []rune("▁▂▃▄▅▆▇█")
	auroraDots        = []rune("·∙•")
	auroraSparkles    = []rune("✦✧")
	shadeRamp         = []rune(" ·░▒▓█")
	spectrumBars      = []rune("▁▂▃▄▅▆▇█")
	spectrumLeft      = []rune("⟨([{<")
	spectrumRight     = []rune(">}])⟩")
	radarLevels       = []rune(" ·┄─═●")
	radarSweep        = []rune("◜◠◝◞◡◟")
	constellationStar = []rune("✦✧✶✷")
	circuitTiles      = []rune("─╴╶═╡╞╪┄╍╾╼")
	cometTrail        = []rune("·░▒▓")

	iconRules = []struct {
		needle string
		icon   string
	}{
		{"firefox", "🌐"},
		{"librewolf", "🌐"},
		{"chromium", "🌐"},
		{"chrome", "🌐"},
		{"browser", "🌐"},
		{"alacritty", "▣"},
		{"foot", "▣"},
		{"kitty", "▣"},
		{"wezterm", "▣"},
		{"terminal", "▣"},
		{"codium", "⌘"},
		{"code", "⌘"},
		{"emacs", "λ"},
		{"vim", "λ"},
		{"thunar", "📁"},
		{"nautilus", "📁"},
		{"pcmanfm", "📁"},
		{"dolphin", "📁"},
		{"pavucontrol", "♪"},
		{"helvum", "♪"},
		{"qpwgraph", "♪"},
		{"ardour", "♪"},
		{"reaper", "♪"},
		{"vlc", "▶"},
		{"mpv", "▶"},
		{"celluloid", "▶"},
		{"spotify", "♪"},
		{"signal", "💬"},
		{"telegram", "💬"},
		{"discord", "💬"},
		{"element", "💬"},
		{"slack", "💬"},
		{"steam", "🎮"},
		{"lutris", "🎮"},
		{"gimp", "✎"},
		{"inkscape", "✎"},
		{"krita", "✎"},
		{"libreoffice", "▤"},
		{"zathura", "▤"},
		{"evince", "▤"},
	}
)

type Settings struct {
	FPS                 float64
	Motion              float64
	ApproxCharWidth     float64
	MaxArtColumns       int
	TitleReserveColumns int
	ShowcaseHoldFrames  int
	ShowcaseBlendFrames int
	DetectChildProcess  bool
}

type Config struct {
	Settings  ConfigSettings            `toml:"settings"`
	Showcase  ConfigShowcase            `toml:"showcase"`
	Glyphs    ConfigGlyphs              `toml:"glyphs"`
	Icons     map[string]string         `toml:"icons"`
	Animation map[string]FrameAnimation `toml:"animation"`
}

type ConfigSettings struct {
	FPS                 *float64 `toml:"fps"`
	Motion              *float64 `toml:"motion"`
	ApproxCharWidth     *float64 `toml:"approx_char_width"`
	MaxArtColumns       *int     `toml:"max_art_columns"`
	TitleReserveColumns *int     `toml:"title_reserve_columns"`
	ShowcaseHoldFrames  *int     `toml:"showcase_hold_frames"`
	ShowcaseBlendFrames *int     `toml:"showcase_blend_frames"`
	DetectChildProcess  *bool    `toml:"detect_child_process"`
}

type ConfigShowcase struct {
	Presets []string `toml:"presets"`
}

type ConfigGlyphs struct {
	AuroraBars        string `toml:"aurora_bars"`
	AuroraDots        string `toml:"aurora_dots"`
	AuroraSparkles    string `toml:"aurora_sparkles"`
	ShadeRamp         string `toml:"shade_ramp"`
	SpectrumBars      string `toml:"spectrum_bars"`
	SpectrumLeft      string `toml:"spectrum_left"`
	SpectrumRight     string `toml:"spectrum_right"`
	RadarLevels       string `toml:"radar_levels"`
	RadarSweep        string `toml:"radar_sweep"`
	ConstellationStar string `toml:"constellation_stars"`
	CircuitTiles      string `toml:"circuit_tiles"`
	CometTrail        string `toml:"comet_trail"`
}

type FrameAnimation struct {
	Frames []string `toml:"frames"`
	Fill   bool     `toml:"fill"`
}

type Rect struct {
	Width int `json:"width"`
}

type WindowProperties struct {
	Class    string `json:"class"`
	Instance string `json:"instance"`
}

type Node struct {
	ID               int64            `json:"id"`
	Name             string           `json:"name"`
	Type             string           `json:"type"`
	PID              int              `json:"pid"`
	Layout           string           `json:"layout"`
	AppID            *string          `json:"app_id"`
	Window           *int64           `json:"window"`
	Focused          bool             `json:"focused"`
	Urgent           bool             `json:"urgent"`
	Shell            string           `json:"shell"`
	InhibitIdle      bool             `json:"inhibit_idle"`
	SandboxEngine    *string          `json:"sandbox_engine"`
	SandboxAppID     *string          `json:"sandbox_app_id"`
	SandboxInstance  *string          `json:"sandbox_instance_id"`
	Marks            []string         `json:"marks"`
	Rect             Rect             `json:"rect"`
	WindowProperties WindowProperties `json:"window_properties"`
	Nodes            []*Node          `json:"nodes"`
	FloatingNodes    []*Node          `json:"floating_nodes"`
	Parent           *Node            `json:"-"`
}

type nodeWithParent struct {
	node   *Node
	parent *Node
}

type cachedProcessLabel struct {
	label     string
	checkedAt time.Time
}

type IPC struct {
	socket string
	conn   net.Conn
}

func (ipc *IPC) Close() {
	if ipc.conn != nil {
		_ = ipc.conn.Close()
		ipc.conn = nil
	}
}

func (ipc *IPC) ensure() error {
	if ipc.conn != nil {
		return nil
	}
	conn, err := net.Dial("unix", ipc.socket)
	if err != nil {
		return err
	}
	ipc.conn = conn
	return nil
}

func (ipc *IPC) Request(messageType uint32, payload string) ([]byte, uint32, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if err := ipc.ensure(); err != nil {
			return nil, 0, err
		}
		body, responseType, err := ipc.requestOnce(messageType, payload)
		if err == nil {
			return body, responseType, nil
		}
		ipc.Close()
	}
	return nil, 0, errors.New("ipc request failed after reconnect")
}

func (ipc *IPC) requestOnce(messageType uint32, payload string) ([]byte, uint32, error) {
	if _, err := ipc.conn.Write(ipcHeader(messageType, len([]byte(payload)))); err != nil {
		return nil, 0, err
	}
	if payload != "" {
		if _, err := ipc.conn.Write([]byte(payload)); err != nil {
			return nil, 0, err
		}
	}
	return readIPCMessage(ipc.conn)
}

func ipcHeader(messageType uint32, length int) []byte {
	header := make([]byte, 14)
	copy(header, []byte("i3-ipc"))
	binary.LittleEndian.PutUint32(header[6:10], uint32(length))
	binary.LittleEndian.PutUint32(header[10:14], messageType)
	return header
}

func readIPCMessage(reader io.Reader) ([]byte, uint32, error) {
	header := make([]byte, 14)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, 0, err
	}
	if string(header[:6]) != "i3-ipc" {
		return nil, 0, errors.New("invalid ipc magic")
	}
	length := binary.LittleEndian.Uint32(header[6:10])
	messageType := binary.LittleEndian.Uint32(header[10:14])
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, 0, err
	}
	return body, messageType, nil
}

func runtimeFile() string {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = "/tmp"
	}
	return filepath.Join(runtimeDir, "sway-tab-title-daemon.pid")
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func replaceExisting(pidFile string) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	oldPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || oldPID == os.Getpid() || !processExists(oldPID) {
		return
	}
	_ = syscall.Kill(oldPID, syscall.SIGTERM)
	for i := 0; i < 20; i++ {
		if !processExists(oldPID) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func writePIDFile(pidFile string) error {
	return os.WriteFile(pidFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)
}

func cleanupPIDFile(pidFile string) {
	data, err := os.ReadFile(pidFile)
	if err == nil && strings.TrimSpace(string(data)) == strconv.Itoa(os.Getpid()) {
		_ = os.Remove(pidFile)
	}
}

func walk(node *Node, parent *Node, out *[]nodeWithParent) {
	if node == nil {
		return
	}
	node.Parent = parent
	*out = append(*out, nodeWithParent{node: node, parent: parent})
	for _, child := range node.Nodes {
		walk(child, node, out)
	}
	for _, child := range node.FloatingNodes {
		walk(child, node, out)
	}
}

func isWindow(node *Node) bool {
	if node == nil || node.Type != "con" || node.Name == "" {
		return false
	}
	return node.AppID != nil || node.Window != nil || node.WindowProperties.Class != ""
}

func identifiers(node *Node) []string {
	values := []string{}
	if node.AppID != nil {
		values = append(values, strings.ToLower(*node.AppID))
	}
	if node.WindowProperties.Class != "" {
		values = append(values, strings.ToLower(node.WindowProperties.Class))
	}
	if node.WindowProperties.Instance != "" {
		values = append(values, strings.ToLower(node.WindowProperties.Instance))
	}
	if node.Name != "" {
		values = append(values, strings.ToLower(node.Name))
	}
	return values
}

func iconFor(node *Node) string {
	ids := identifiers(node)
	for _, rule := range iconRules {
		for _, value := range ids {
			if strings.Contains(value, rule.needle) {
				return rule.icon
			}
		}
	}
	return "◆"
}

func appLabel(node *Node) string {
	label := "app"
	if node.AppID != nil && *node.AppID != "" {
		label = *node.AppID
	} else if node.WindowProperties.Class != "" {
		label = node.WindowProperties.Class
	} else if node.WindowProperties.Instance != "" {
		label = node.WindowProperties.Instance
	}
	parts := strings.Split(label, ".")
	label = strings.TrimSpace(parts[len(parts)-1])
	if label == "" {
		return "app"
	}
	runes := []rune(label)
	if len(runes) > 24 {
		runes = runes[:24]
	}
	return string(runes)
}

func textColumns(value string) int {
	columns := 0
	for _, r := range value {
		if r >= 0x1100 {
			columns += 2
		} else {
			columns++
		}
	}
	return columns
}

func truncateColumns(value string, maxColumns int) string {
	if maxColumns <= 0 {
		return ""
	}
	if textColumns(value) <= maxColumns {
		return value
	}
	if maxColumns <= 1 {
		return "…"
	}
	used := 0
	out := []rune{}
	for _, r := range value {
		width := 1
		if r >= 0x1100 {
			width = 2
		}
		if used+width > maxColumns-1 {
			break
		}
		out = append(out, r)
		used += width
	}
	return string(out) + "…"
}

func tabWidthPX(node *Node, parent *Node) int {
	parentWidth := 0
	if parent != nil {
		parentWidth = parent.Rect.Width
	}
	nodeWidth := node.Rect.Width
	if parent != nil && (parent.Layout == "tabbed" || parent.Layout == "stacked") {
		siblings := 0
		for _, child := range parent.Nodes {
			if isWindow(child) {
				siblings++
			}
		}
		if siblings > 0 && parentWidth > 0 {
			return max(1, parentWidth/siblings)
		}
	}
	return max(1, max(nodeWidth, max(parentWidth, 240)))
}

func artWidth(width int) int {
	return max(0, min(width, settings.MaxArtColumns))
}

func shortFrame(width int, phase int, frames []string) string {
	runes := []rune(frames[phase%len(frames)])
	if len(runes) > width {
		runes = runes[:width]
	}
	return string(runes)
}

func pseudoRandom(index int, phase int, salt float64) float64 {
	value := math.Sin(float64(index+1)*12.9898+(float64(phase)+salt)*78.233) * 43758.5453
	return value - math.Floor(value)
}

func rampPick(ramp []rune, level float64) rune {
	level = math.Max(0.0, math.Min(1.0, level))
	return ramp[min(len(ramp)-1, int(level*float64(len(ramp))))]
}

func auroraArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"▁▂▃", "▂▃▄", "▃▄▅", "▄▅▆", "▅▆▇", "▆▇█", "▇█▇", "█▇▆"})
	}

	chars := make([]rune, 0, width)
	timeBase := float64(phase) * 0.022

	for index := range width {
		offset := pseudoRandom(index, 0, 2.8)
		rise := math.Mod(timeBase+offset, 1.0)
		// Grow each column upward, then let it settle softly before the next lift.
		lift := 0.0
		if rise < 0.74 {
			lift = smoothstep(rise / 0.74)
		} else {
			lift = 1.0 - smoothstep((rise-0.74)/0.26)*0.82
		}
		swell := (math.Sin(float64(index)*0.19+float64(phase)*0.018) + 1.0) * 0.5
		breath := (math.Sin(float64(phase)*0.011+float64(index)*0.041) + 1.0) * 0.5
		level := 0.12 + 0.64*lift + 0.16*swell + 0.08*breath
		level = math.Max(0.0, math.Min(1.0, level))
		chars = append(chars, rampPick(auroraBars, level))
	}
	return string(chars)
}

func spectrumArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"⟨█⟩", "(▓)", "[▒]", "{░}", "<·>"})
	}

	chars := make([]rune, width)
	for index := range chars {
		chars[index] = rampPick(shadeRamp, 0.15+0.08*math.Sin(float64(index)*0.55+float64(phase)*0.03))
	}
	center := width / 2
	if width%2 == 0 {
		chars[center-1] = '┃'
		chars[center] = '┃'
	} else {
		chars[center] = '┃'
	}

	maxRadius := max(1, width/2-1)
	outerPulse := 0.52 + 0.48*math.Sin(float64(phase)*0.051)
	innerPulse := 0.50 + 0.50*math.Sin(float64(phase)*0.079+1.7)
	outerRadius := int(float64(maxRadius) * (0.36 + 0.56*outerPulse))
	innerRadius := int(float64(maxRadius) * (0.10 + 0.42*innerPulse))

	for radius := 1; radius <= maxRadius; radius++ {
		left := center - radius - 1
		right := center + radius
		if width%2 != 0 {
			left = center - radius
			right = center + radius
		}
		if left < 0 || right >= width {
			continue
		}

		level := (math.Sin(float64(radius)*0.48+float64(phase)*0.11) + 1.0) * 0.5
		level = math.Max(level*0.58, math.Exp(-math.Pow(float64(radius-outerRadius)/math.Max(2.0, float64(width)*0.045), 2)))
		level = math.Max(level, math.Exp(-math.Pow(float64(radius-innerRadius)/math.Max(2.0, float64(width)*0.032), 2))*0.92)

		switch {
		case radius == outerRadius || radius == innerRadius:
			pair := (phase/9 + radius) % min(len(spectrumLeft), len(spectrumRight))
			chars[left] = spectrumLeft[pair]
			chars[right] = spectrumRight[pair]
		case level > 0.76:
			chars[left] = rampPick(spectrumBars, level)
			chars[right] = rampPick(spectrumBars, level)
		case level > 0.58:
			chars[left] = '━'
			chars[right] = '━'
		case level > 0.42:
			chars[left] = '─'
			chars[right] = '─'
		default:
			chars[left] = rampPick(shadeRamp, level*0.42)
			chars[right] = rampPick(shadeRamp, level*0.42)
		}
	}
	return string(chars)
}

func radarArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"╶◜╴", "─◠─", "╶◝╴", "╶◞╴", "─◡─", "╶◟╴"})
	}

	head := math.Mod(float64(phase)*1.05, float64(width))
	echo := math.Mod(float64(phase)*0.48+float64(width)*0.57, float64(width))
	secondary := math.Mod(float64(width)-float64(phase)*0.36, float64(width))
	chars := make([]rune, 0, width)
	for index := range width {
		pulse := math.Max(0.0, 1.0-math.Abs(float64(index)-head)/6.0)
		echoPulse := math.Max(0.0, 1.0-math.Abs(float64(index)-echo)/9.0) * 0.64
		secondaryPulse := math.Max(0.0, 1.0-math.Abs(float64(index)-secondary)/13.0) * 0.42
		scanline := math.Sin(float64(index)*0.29 - float64(phase)*0.10)
		level := math.Max(pulse, math.Max(echoPulse, secondaryPulse))
		switch {
		case math.Abs(float64(index)-head) < 0.55:
			chars = append(chars, radarSweep[(phase/3)%len(radarSweep)])
		case index%11 == 0:
			chars = append(chars, '╋')
		case level > 0.72:
			chars = append(chars, rampPick(radarLevels, level))
		case level > 0.38:
			chars = append(chars, '═')
		case scanline > 0.72:
			chars = append(chars, '─')
		case scanline > 0.35:
			chars = append(chars, '┄')
		default:
			chars = append(chars, ' ')
		}
	}
	return string(chars)
}

func constellationArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{" ✦ ", "  ✧", "·  ", " ✶ ", "  ·"})
	}

	chars := make([]rune, 0, width)
	drift := math.Sin(float64(phase)*0.025) * float64(width) * 0.08
	for index := range width {
		n := pseudoRandom(index, phase/12, 4.2)
		shimmer := pseudoRandom(index, phase/4, 9.7)
		lane := math.Sin((float64(index)+drift)*0.11) + math.Sin(float64(index)*0.037-float64(phase)*0.041)
		switch {
		case shimmer > 0.982 && lane > 0.15:
			chars = append(chars, constellationStar[(index+phase)%len(constellationStar)])
		case n > 0.955 && lane > -0.15:
			chars = append(chars, '•')
		case n > 0.915 && lane > 0.35:
			chars = append(chars, '·')
		case index > 0 && index < width-1 && n > 0.985:
			chars = append(chars, '╴')
		default:
			chars = append(chars, ' ')
		}
	}
	return string(chars)
}

func circuitArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"╍╾╼", "─╪═", "═●═", "╼╾╍"})
	}

	chars := make([]rune, 0, width)
	gateA := int(math.Mod(float64(phase)*0.9, float64(max(1, width))))
	gateB := int(float64(width-1) - math.Mod(float64(phase)*0.56, float64(max(1, width))))
	gateC := int(math.Mod(float64(phase)*0.31+float64(width)*0.38, float64(max(1, width))))
	for index := range width {
		nearGate := min(abs(index-gateA), min(abs(index-gateB), abs(index-gateC)))
		switch {
		case nearGate == 0:
			chars = append(chars, '●')
		case nearGate <= 2:
			chars = append(chars, circuitTiles[(phase+index)%len(circuitTiles)])
		case (index+phase/4)%19 == 0:
			chars = append(chars, '╪')
		case (index-phase/3)%13 == 0:
			chars = append(chars, '═')
		case (index+phase)%7 == 0:
			chars = append(chars, '╍')
		case (index+phase)%4 == 0:
			chars = append(chars, '┄')
		default:
			chars = append(chars, '─')
		}
	}
	return string(chars)
}

func braidArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"╱╲╱", "╲╱╲", "╱╳╲", "╲╳╱"})
	}

	chars := make([]rune, 0, width)
	for index := range width {
		waveA := math.Sin(float64(index)*0.58 + float64(phase)*0.12)
		waveB := math.Sin(float64(index)*0.58 - float64(phase)*0.10 + math.Pi)
		cross := math.Abs(waveA - waveB)
		switch {
		case cross < 0.16:
			chars = append(chars, '╳')
		case waveA > waveB:
			chars = append(chars, '╱')
		default:
			chars = append(chars, '╲')
		}
	}
	return string(chars)
}

func loomArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"≈⌁░", "░≋▒", "▒⌁▓", "▓✦▒", "▒≋░", "░⌁≈"})
	}

	chars := make([]rune, 0, width)
	t := float64(phase) * 0.041
	knotA := float64(width) * (0.24 + 0.13*math.Sin(t*0.71))
	knotB := float64(width) * (0.52 + 0.17*math.Sin(t*0.53+1.9))
	knotC := float64(width) * (0.79 + 0.10*math.Sin(t*0.67+4.1))

	for index := range width {
		x := float64(index)
		warp := math.Sin(x*0.17+t) + math.Sin(x*0.043-t*0.82+math.Sin(t*0.31)*2.2)
		weft := math.Sin(x*0.31-t*1.17) * math.Cos(x*0.071+t*0.47)
		moire := (math.Sin(warp*1.7+weft*1.1) + 1.0) * 0.5
		softGrain := pseudoRandom(index, phase/9, 12.4) * 0.11
		level := 0.12 + moire*0.54 + softGrain

		knot := math.Exp(-math.Pow((x-knotA)/4.8, 2))
		knot = math.Max(knot, math.Exp(-math.Pow((x-knotB)/5.8, 2))*0.94)
		knot = math.Max(knot, math.Exp(-math.Pow((x-knotC)/4.2, 2))*0.86)
		level = math.Max(level, knot)

		crossing := math.Abs(math.Sin(warp+t*0.4)) < 0.075 && math.Abs(weft) > 0.38
		switch {
		case knot > 0.93 && (phase/8+index)%3 == 0:
			chars = append(chars, '✦')
		case knot > 0.78:
			chars = append(chars, '⌁')
		case crossing:
			chars = append(chars, '≋')
		case level > 0.74:
			chars = append(chars, '≈')
		default:
			chars = append(chars, rampPick(shadeRamp, level))
		}
	}
	return string(chars)
}

func cometArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"░▒☄", "▒▓✦", "▓█✶", "▒▓✧", "░▒·"})
	}

	chars := make([]rune, width)
	for i := range chars {
		nebula := 0.11 + 0.18*(math.Sin(float64(i)*0.19+float64(phase)*0.027)+1.0)*0.5
		chars[i] = rampPick(shadeRamp, nebula)
	}
	heads := []struct {
		pos    int
		dir    int
		length int
		head   rune
	}{
		{int(math.Mod(float64(phase)*0.92, float64(width+26))) - 13, 1, 19, '☄'},
		{width - 1 - (int(math.Mod(float64(phase)*0.52+float64(width)*0.44, float64(width+30))) - 15), -1, 15, '✦'},
		{int(math.Mod(float64(phase)*0.31+float64(width)*0.68, float64(width+40))) - 20, 1, 23, '✧'},
	}
	for cometIndex, comet := range heads {
		for tail := 0; tail < comet.length; tail++ {
			index := comet.pos - tail*comet.dir
			if index < 0 || index >= width {
				continue
			}
			if tail == 0 {
				chars[index] = comet.head
			} else {
				level := math.Max(0.0, 1.0-float64(tail)/float64(comet.length))
				if cometIndex == 1 && tail%5 == 0 {
					chars[index] = '·'
				} else {
					chars[index] = rampPick(cometTrail, level)
				}
			}
			if tail == 2 && index+1 < width {
				chars[index+1] = '✶'
			}
		}
	}
	return string(chars)
}

type animationFunc func(width int, phase int) string

var animationPresets = map[string]animationFunc{
	"aurora":        auroraArt,
	"spectrum":      spectrumArt,
	"radar":         radarArt,
	"constellation": constellationArt,
	"circuit":       circuitArt,
	"braid":         braidArt,
	"loom":          loomArt,
	"comet":         cometArt,
}

var showcasePresets = []string{
	"loom",
	"aurora",
	"spectrum",
	"radar",
	"constellation",
	"circuit",
	"braid",
	"comet",
}

func showcaseArt(width int, phase int) string {
	cycleFrames := settings.ShowcaseHoldFrames + settings.ShowcaseBlendFrames
	presetIndex := (phase / cycleFrames) % len(showcasePresets)
	offset := phase % cycleFrames
	preset := showcasePresets[presetIndex]
	motion := motionPhase(phase)

	if offset < settings.ShowcaseHoldFrames {
		return animationPresets[preset](width, motion)
	}

	nextPreset := showcasePresets[(presetIndex+1)%len(showcasePresets)]
	from := animationPresets[preset](width, motion)
	to := animationPresets[nextPreset](width, motion)
	progress := float64(offset-settings.ShowcaseHoldFrames) / float64(settings.ShowcaseBlendFrames)
	return blendArt(from, to, width, phase, smoothstep(progress))
}

func activityArt(width int, phase int) string {
	if animationPreset == "showcase" {
		return showcaseArt(width, phase)
	}
	return animationFuncFor(animationPreset)(width, motionPhase(phase))
}

func animationFrameKey(phase int) int {
	if animationPreset != "showcase" {
		return motionPhase(phase)
	}
	cycleFrames := settings.ShowcaseHoldFrames + settings.ShowcaseBlendFrames
	presetIndex := (phase / cycleFrames) % len(showcasePresets)
	offset := phase % cycleFrames
	if offset < settings.ShowcaseHoldFrames {
		return presetIndex*1_000_000 + motionPhase(phase)
	}
	return 10_000_000 + phase
}

func blendArt(from string, to string, width int, phase int, progress float64) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	fromRunes := fitRunes(from, width)
	toRunes := fitRunes(to, width)
	out := make([]rune, 0, width)

	for index := range width {
		x := float64(index) / float64(max(1, width-1))
		wave := 0.13 * math.Sin(float64(index)*0.42+float64(phase)*0.055)
		dither := 0.10 * (pseudoRandom(index, phase/5, 17.0) - 0.5)
		threshold := x + wave + dither
		if progress >= threshold {
			out = append(out, toRunes[index])
		} else {
			out = append(out, fromRunes[index])
		}
	}
	return string(out)
}

func fitRunes(value string, width int) []rune {
	runes := []rune(value)
	if len(runes) > width {
		return runes[:width]
	}
	if len(runes) < width {
		padding := make([]rune, width-len(runes))
		for index := range padding {
			padding[index] = ' '
		}
		runes = append(runes, padding...)
	}
	return runes
}

func smoothstep(value float64) float64 {
	value = math.Max(0.0, math.Min(1.0, value))
	return value * value * (3.0 - 2.0*value)
}

func motionPhase(phase int) int {
	return int(math.Floor(float64(phase) * settings.Motion))
}

func animationFuncFor(name string) animationFunc {
	if name == "showcase" {
		return showcaseArt
	}
	return animationPresets[name]
}

func frameAnimationArt(animation FrameAnimation) animationFunc {
	return func(width int, phase int) string {
		width = artWidth(width)
		if width == 0 || len(animation.Frames) == 0 {
			return ""
		}
		frame := []rune(animation.Frames[phase%len(animation.Frames)])
		if animation.Fill && len(frame) > 0 {
			out := make([]rune, 0, width)
			for len(out) < width {
				remaining := width - len(out)
				if len(frame) <= remaining {
					out = append(out, frame...)
				} else {
					out = append(out, frame[:remaining]...)
				}
			}
			return string(out)
		}
		if len(frame) > width {
			frame = frame[:width]
		}
		if len(frame) < width {
			padding := make([]rune, width-len(frame))
			for index := range padding {
				padding[index] = ' '
			}
			frame = append(frame, padding...)
		}
		return string(frame)
	}
}

func loadConfig(path string) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var config Config
	if err := toml.Unmarshal(data, &config); err != nil {
		return err
	}
	applyConfig(config)
	return nil
}

func initConfig(path string) error {
	if path == "" {
		return errors.New("config path is empty")
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(defaultConfigContents), 0o644)
}

func applyConfig(config Config) {
	if config.Settings.FPS != nil {
		settings.FPS = *config.Settings.FPS
	}
	if config.Settings.Motion != nil {
		settings.Motion = *config.Settings.Motion
	}
	if config.Settings.ApproxCharWidth != nil {
		settings.ApproxCharWidth = *config.Settings.ApproxCharWidth
	}
	if config.Settings.MaxArtColumns != nil {
		settings.MaxArtColumns = *config.Settings.MaxArtColumns
	}
	if config.Settings.TitleReserveColumns != nil {
		settings.TitleReserveColumns = *config.Settings.TitleReserveColumns
	}
	if config.Settings.ShowcaseHoldFrames != nil {
		settings.ShowcaseHoldFrames = *config.Settings.ShowcaseHoldFrames
	}
	if config.Settings.ShowcaseBlendFrames != nil {
		settings.ShowcaseBlendFrames = *config.Settings.ShowcaseBlendFrames
	}
	if config.Settings.DetectChildProcess != nil {
		settings.DetectChildProcess = *config.Settings.DetectChildProcess
	}
	applyGlyphConfig(config.Glyphs)
	for needle, icon := range config.Icons {
		iconRules = append([]struct {
			needle string
			icon   string
		}{{strings.ToLower(needle), icon}}, iconRules...)
	}
	for name, animation := range config.Animation {
		if len(animation.Frames) == 0 {
			continue
		}
		animationPresets[name] = frameAnimationArt(animation)
	}
	if len(config.Showcase.Presets) > 0 {
		showcasePresets = filterKnownPresets(config.Showcase.Presets)
	}
}

func applyGlyphConfig(glyphs ConfigGlyphs) {
	assignRunes := func(value string, target *[]rune) {
		if value != "" {
			*target = []rune(value)
		}
	}
	assignRunes(glyphs.AuroraBars, &auroraBars)
	assignRunes(glyphs.AuroraDots, &auroraDots)
	assignRunes(glyphs.AuroraSparkles, &auroraSparkles)
	assignRunes(glyphs.ShadeRamp, &shadeRamp)
	assignRunes(glyphs.SpectrumBars, &spectrumBars)
	assignRunes(glyphs.SpectrumLeft, &spectrumLeft)
	assignRunes(glyphs.SpectrumRight, &spectrumRight)
	assignRunes(glyphs.RadarLevels, &radarLevels)
	assignRunes(glyphs.RadarSweep, &radarSweep)
	assignRunes(glyphs.ConstellationStar, &constellationStar)
	assignRunes(glyphs.CircuitTiles, &circuitTiles)
	assignRunes(glyphs.CometTrail, &cometTrail)
}

func filterKnownPresets(names []string) []string {
	filtered := []string{}
	for _, name := range names {
		if name == "showcase" {
			continue
		}
		if _, ok := animationPresets[name]; ok {
			filtered = append(filtered, name)
		}
	}
	if len(filtered) == 0 {
		return showcasePresets
	}
	return filtered
}

type TitleAnimator struct {
	ipc                  *IPC
	lastFormats          map[int64]string
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
}

func NewTitleAnimator(ipc *IPC) *TitleAnimator {
	return &TitleAnimator{
		ipc:           ipc,
		lastFormats:   map[int64]string{},
		processLabels: map[int]cachedProcessLabel{},
		windowsByID:   map[int64]nodeWithParent{},
	}
}

func (animator *TitleAnimator) RefreshTree(phase int) {
	body, _, err := animator.ipc.Request(ipcGetTree, "")
	if err != nil {
		return
	}
	var root Node
	if err := json.Unmarshal(body, &root); err != nil {
		return
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
	if animator.lastFormats[node.ID] == value {
		return
	}
	animator.SetTitleFormat(node.ID, value)
	animator.lastFormats[node.ID] = value
}

func (animator *TitleAnimator) NodeFrameParts(node *Node, parent *Node) (string, int) {
	icon := iconFor(node)
	label := animator.WindowLabel(node)
	statusText := visibleStatusText(node)
	visiblePrefix := fmt.Sprintf("%s %s%s: ", icon, label, statusText)
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

func (animator *TitleAnimator) ApplyFocusedFrame(phase int) {
	key := animationFrameKey(phase)
	if animator.focusedCacheIsActive && animator.focusedAnimationKey == key {
		return
	}
	animator.focusedAnimationKey = key

	value := animator.focusedBase
	if animator.focusedArtColumns > 0 {
		value += " " + activityArt(animator.focusedArtColumns, phase)
	}
	if animator.lastFormats[animator.focusedID] == value {
		return
	}
	animator.SetTitleFormat(animator.focusedID, value)
	animator.lastFormats[animator.focusedID] = value
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

func (animator *TitleAnimator) ResetAll() {
	for conID := range animator.lastFormats {
		animator.SetTitleFormat(conID, "%title")
	}
	animator.lastFormats = map[int64]string{}
}

func (animator *TitleAnimator) SetTitleFormat(conID int64, value string) {
	command := fmt.Sprintf("[con_id=%d] title_format %s", conID, quoteSwayString(value))
	_, _, _ = animator.ipc.Request(ipcRunCommand, command)
}

type statusBadge struct {
	text   string
	color  string
	weight string
}

func statusBadges(node *Node) []statusBadge {
	badges := []statusBadge{}
	if node.Shell != "" {
		shell := node.Shell
		switch shell {
		case "xdg_shell":
			shell = "wl"
		case "xwayland":
			shell = "x11"
		}
		badges = append(badges, statusBadge{text: shell, color: "#666666"})
	}
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
		if mark != "" && !strings.HasPrefix(mark, "_") {
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

func subscribe(socket string, events chan<- struct{}, shutdown chan<- struct{}, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
		}

		conn, err := net.Dial("unix", socket)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		if _, err := conn.Write(ipcHeader(ipcSubscribe, len([]byte(`["window","workspace","shutdown"]`)))); err != nil {
			_ = conn.Close()
			time.Sleep(time.Second)
			continue
		}
		if _, err := conn.Write([]byte(`["window","workspace","shutdown"]`)); err != nil {
			_ = conn.Close()
			time.Sleep(time.Second)
			continue
		}
		if _, _, err := readIPCMessage(conn); err != nil {
			_ = conn.Close()
			time.Sleep(time.Second)
			continue
		}

		reader := bufio.NewReader(conn)
		for {
			body, _, err := readIPCMessage(reader)
			if err != nil {
				_ = conn.Close()
				break
			}
			var event struct {
				Change string `json:"change"`
			}
			_ = json.Unmarshal(body, &event)
			if event.Change == "shutdown" {
				select {
				case shutdown <- struct{}{}:
				default:
				}
				_ = conn.Close()
				return
			}
			select {
			case events <- struct{}{}:
			default:
			}
		}
		time.Sleep(time.Second)
	}
}

func runLoop(socket string) int {
	return runLoopWithFPS(socket, settings.FPS)
}

func runLoopWithFPS(socket string, fps float64) int {
	control := &IPC{socket: socket}
	defer control.Close()

	animator := NewTitleAnimator(control)
	events := make(chan struct{}, 16)
	shutdown := make(chan struct{}, 1)
	done := make(chan struct{})
	defer close(done)
	go subscribe(socket, events, shutdown, done)

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	phase := 0
	animator.RefreshTree(phase)
	ticker := time.NewTicker(time.Duration(float64(time.Second) / fps))
	defer ticker.Stop()

	for {
		select {
		case <-events:
			animator.RefreshTree(phase)
		case <-shutdown:
			animator.ResetAll()
			return 0
		case <-signals:
			animator.ResetAll()
			return 0
		case <-ticker.C:
			phase++
			animator.Tick(phase)
		}
	}
}

func listPresets() {
	names := make([]string, 0, len(animationPresets)+1)
	for name := range animationPresets {
		names = append(names, name)
	}
	names = append(names, "showcase")
	sort.Strings(names)
	for _, name := range names {
		fmt.Println(name)
	}
}

func main() {
	replace := flag.Bool("replace", false, "replace an existing tab title daemon")
	preset := flag.String("preset", envDefault("SWAY_TAB_ANIMATION", animationPreset), "animation preset")
	fps := flag.Float64("fps", 0, "animation frames per second")
	configPath := flag.String("config", envDefault("SWAY_TITLE_ANIMATOR_CONFIG", defaultConfigPath()), "TOML config path")
	initConfigFlag := flag.Bool("init-config", false, "write an example config if it does not exist")
	list := flag.Bool("list-presets", false, "list animation presets")
	socket := flag.String("socket", os.Getenv("SWAYSOCK"), "sway IPC socket")
	flag.Parse()

	if *initConfigFlag {
		if err := initConfig(*configPath); err != nil {
			fmt.Fprintf(os.Stderr, "Unable to initialize config %s: %v\n", *configPath, err)
			os.Exit(1)
		}
		fmt.Printf("Created %s\n", *configPath)
		return
	}

	if err := loadConfig(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "Unable to load config %s: %v\n", *configPath, err)
		os.Exit(2)
	}
	if *fps > 0 {
		settings.FPS = *fps
	} else if envFPS := os.Getenv("SWAY_TAB_FPS"); envFPS != "" {
		if parsed, err := strconv.ParseFloat(envFPS, 64); err == nil {
			settings.FPS = parsed
		}
	}

	if *list {
		listPresets()
		return
	}
	if *preset != "showcase" {
		if _, ok := animationPresets[*preset]; !ok {
			fmt.Fprintf(os.Stderr, "Unknown animation preset: %s\n", *preset)
			fmt.Fprintf(os.Stderr, "Available:\n")
			listPresets()
			os.Exit(2)
		}
	}
	animationPreset = *preset
	if settings.FPS < 1 || settings.FPS > 60 {
		fmt.Fprintln(os.Stderr, "FPS must be between 1 and 60")
		os.Exit(2)
	}
	if settings.Motion <= 0 {
		fmt.Fprintln(os.Stderr, "Motion must be greater than 0")
		os.Exit(2)
	}
	if settings.ShowcaseHoldFrames < 1 || settings.ShowcaseBlendFrames < 1 {
		fmt.Fprintln(os.Stderr, "Showcase hold/blend frames must be greater than 0")
		os.Exit(2)
	}
	if *socket == "" {
		fmt.Fprintln(os.Stderr, "SWAYSOCK is not set; pass --socket or start from Sway")
		os.Exit(2)
	}

	pidFile := runtimeFile()
	if *replace {
		replaceExisting(pidFile)
	} else if data, err := os.ReadFile(pidFile); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 && processExists(pid) {
			return
		}
	}
	if err := writePIDFile(pidFile); err != nil {
		fmt.Fprintf(os.Stderr, "Unable to write pid file: %v\n", err)
		os.Exit(1)
	}

	exitCode := runLoopWithFPS(*socket, settings.FPS)
	cleanupPIDFile(pidFile)
	os.Exit(exitCode)
}

func defaultConfigPath() string {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "sway-title-animator", "config.toml")
}

func envDefault(name string, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envFloatDefault(name string, fallback float64) float64 {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
