package main

import "time"

const (
	defaultFPS              = 25.0
	defaultMotion           = 0.22
	defaultApproxCharWidth  = 8.5
	defaultMaxArtColumns    = 220
	defaultTitleReserve     = 18
	defaultRotationHold     = 260
	defaultRotationBlend    = 75
	defaultAudioDevice      = "@DEFAULT_MONITOR@"
	defaultAudioSensitivity = 1.0
	defaultAudioMotion      = 1.0
	titleReassertInterval   = 2 * time.Second
	rotationSelection       = ""
)

const defaultConfigContents = `[settings]
fps = 25
motion = 0.22
approx_char_width = 8.5
max_art_columns = 220
title_reserve_columns = 18
rotation_hold_frames = 260
rotation_blend_frames = 75
detect_child_process = true

[audio]
# device = "@DEFAULT_MONITOR@"
sensitivity = 1.0
motion = 1.0

[rotation]
presets = [
  "loom",
  "aurora",
  "bloom",
  "spectrum",
  "square",
  "ripples",
  "radar",
  "constellation",
  "circuit",
  "glitch",
  "braid",
  "comet",
  "smileys",
  "wave",
  "spline",
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
`

type iconRule struct {
	needle string
	icon   string
}

var (
	animationPreset = rotationSelection
	settings        = Settings{
		FPS:                 defaultFPS,
		Motion:              defaultMotion,
		ApproxCharWidth:     defaultApproxCharWidth,
		MaxArtColumns:       defaultMaxArtColumns,
		TitleReserveColumns: defaultTitleReserve,
		RotationHoldFrames:  defaultRotationHold,
		RotationBlendFrames: defaultRotationBlend,
		DetectChildProcess:  true,
	}
	audioSettings = AudioSettings{
		Device:      defaultAudioDevice,
		Sensitivity: defaultAudioSensitivity,
		Motion:      defaultAudioMotion,
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
	iconRules         = []iconRule{
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
	RotationHoldFrames  int
	RotationBlendFrames int
	DetectChildProcess  bool
}

type AudioSettings struct {
	Device      string
	Sensitivity float64
	Motion      float64
}

type Config struct {
	Settings  ConfigSettings            `toml:"settings"`
	Audio     ConfigAudio               `toml:"audio"`
	Rotation  ConfigRotation            `toml:"rotation"`
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
	RotationHoldFrames  *int     `toml:"rotation_hold_frames"`
	RotationBlendFrames *int     `toml:"rotation_blend_frames"`
	DetectChildProcess  *bool    `toml:"detect_child_process"`
}

type ConfigAudio struct {
	Device      *string  `toml:"device"`
	Sensitivity *float64 `toml:"sensitivity"`
	Motion      *float64 `toml:"motion"`
}

type ConfigRotation struct {
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
