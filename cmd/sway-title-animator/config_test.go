package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marang/sway-title-animator/internal/titleindicator"
	"github.com/pelletier/go-toml/v2"
)

func TestValidateConfigCompatibilityRejectsObsoleteShowcaseConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		message string
	}{
		{
			name:    "section",
			config:  "[showcase]\npresets = [\"aurora\"]\n",
			message: "rename it to [rotation]",
		},
		{
			name:    "hold setting",
			config:  "[settings]\nshowcase_hold_frames = 20\n",
			message: "settings.rotation_hold_frames",
		},
		{
			name:    "blend setting",
			config:  "[settings]\nshowcase_blend_frames = 10\n",
			message: "settings.rotation_blend_frames",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateConfigCompatibility([]byte(test.config))
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("expected migration error containing %q, got %v", test.message, err)
			}
		})
	}
}

func TestLoadConfigRejectsObsoleteShowcaseSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[showcase]\npresets = [\"aurora\"]\n"), 0o600); err != nil {
		t.Fatalf("write obsolete config: %v", err)
	}

	err := loadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "rename it to [rotation]") {
		t.Fatalf("expected actionable load error, got %v", err)
	}
}

func TestRotationConfigUsesNewNames(t *testing.T) {
	data := []byte(`[settings]
rotation_hold_frames = 20
rotation_blend_frames = 10

[rotation]
presets = ["aurora", "square"]
`)
	if err := validateConfigCompatibility(data); err != nil {
		t.Fatalf("reject current config: %v", err)
	}

	var config Config
	if err := toml.Unmarshal(data, &config); err != nil {
		t.Fatalf("decode current config: %v", err)
	}
	if config.Settings.RotationHoldFrames == nil || *config.Settings.RotationHoldFrames != 20 {
		t.Fatalf("unexpected rotation hold setting: %#v", config.Settings.RotationHoldFrames)
	}
	if config.Settings.RotationBlendFrames == nil || *config.Settings.RotationBlendFrames != 10 {
		t.Fatalf("unexpected rotation blend setting: %#v", config.Settings.RotationBlendFrames)
	}
	if got := strings.Join(config.Rotation.Presets, ","); got != "aurora,square" {
		t.Fatalf("unexpected rotation presets %q", got)
	}
}

func TestValidateConfigCompatibilityRejectsUnsupportedAudioBackend(t *testing.T) {
	data := []byte("[audio]\nbackend = \"pipewire\"\n")
	err := validateConfigCompatibility(data)
	if err == nil || !strings.Contains(err.Error(), "audio.backend is not supported") {
		t.Fatalf("expected unsupported backend error, got %v", err)
	}
}

func TestAudioConfigUsesAcceptedStartupFields(t *testing.T) {
	data := []byte(`[audio]
device = "alsa_output.test.monitor"
sensitivity = 1.5
motion = 0.75
`)
	var config Config
	if err := toml.Unmarshal(data, &config); err != nil {
		t.Fatalf("decode audio config: %v", err)
	}
	if config.Audio.Device == nil || *config.Audio.Device != "alsa_output.test.monitor" {
		t.Fatalf("unexpected audio device: %#v", config.Audio.Device)
	}
	if config.Audio.Sensitivity == nil || *config.Audio.Sensitivity != 1.5 {
		t.Fatalf("unexpected audio sensitivity: %#v", config.Audio.Sensitivity)
	}
	if config.Audio.Motion == nil || *config.Audio.Motion != 0.75 {
		t.Fatalf("unexpected audio motion: %#v", config.Audio.Motion)
	}
}

func TestApplicationIndicatorGlyphsAreConfigurableAsOneEqualWidthSet(t *testing.T) {
	original := []string{
		applicationIndicatorUnregistered,
		applicationIndicatorPending,
		applicationIndicatorRegistered,
		applicationIndicatorPinned,
	}
	t.Cleanup(func() {
		applicationIndicatorUnregistered = original[0]
		applicationIndicatorPending = original[1]
		applicationIndicatorRegistered = original[2]
		applicationIndicatorPinned = original[3]
	})
	path := filepath.Join(t.TempDir(), "config.toml")
	data := []byte(`[indicators]
unregistered = "u"
pending = "w"
registered = "r"
pinned = "p"
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfig(path); err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := validateRuntimeSettings(); err != nil {
		t.Fatalf("validate configured glyphs: %v", err)
	}

	states := []titleindicator.State{
		titleindicator.Unregistered,
		titleindicator.Pending,
		titleindicator.Registered,
		titleindicator.Pinned,
	}
	for index, state := range states {
		mark, err := titleindicator.Mark(state, 42)
		if err != nil {
			t.Fatal(err)
		}
		if got := applicationIndicator([]string{mark}, 42); got != []string{"u ", "w ", "r ", "p "}[index] {
			t.Fatalf("indicator %q = %q", state, got)
		}
	}
}

func TestApplicationIndicatorGlyphsRejectAmbiguousOrUnequalSets(t *testing.T) {
	tests := []struct {
		name   string
		glyphs [4]string
	}{
		{name: "duplicate", glyphs: [4]string{"○", "◔", "○", "◆"}},
		{name: "multiple characters", glyphs: [4]string{"○", "◔", "rr", "◆"}},
		{name: "unequal columns", glyphs: [4]string{"○", "◔", "界", "◆"}},
		{name: "control character", glyphs: [4]string{"○", "\n", "●", "◆"}},
		{name: "invisible format character", glyphs: [4]string{"○", "\u200d", "●", "◆"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := [4]string{
				applicationIndicatorUnregistered,
				applicationIndicatorPending,
				applicationIndicatorRegistered,
				applicationIndicatorPinned,
			}
			applicationIndicatorUnregistered = test.glyphs[0]
			applicationIndicatorPending = test.glyphs[1]
			applicationIndicatorRegistered = test.glyphs[2]
			applicationIndicatorPinned = test.glyphs[3]
			t.Cleanup(func() {
				applicationIndicatorUnregistered = original[0]
				applicationIndicatorPending = original[1]
				applicationIndicatorRegistered = original[2]
				applicationIndicatorPinned = original[3]
			})

			if err := validateRuntimeSettings(); err == nil || !strings.Contains(err.Error(), "application indicator glyphs") {
				t.Fatalf("expected indicator glyph validation error, got %v", err)
			}
		})
	}
}

func TestApplicationIndicatorConfigRejectsExplicitlyEmptyGlyph(t *testing.T) {
	original := applicationIndicatorPinned
	t.Cleanup(func() { applicationIndicatorPinned = original })
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[indicators]\npinned = \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfig(path); err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := validateRuntimeSettings(); err == nil || !strings.Contains(err.Error(), "application indicator glyphs") {
		t.Fatalf("expected explicit empty glyph to fail validation, got %v", err)
	}
}

func TestValidateAudioSettings(t *testing.T) {
	valid := AudioSettings{
		Device:      defaultAudioDevice,
		Sensitivity: defaultAudioSensitivity,
		Motion:      defaultAudioMotion,
	}
	if err := validateAudioSettings(valid); err != nil {
		t.Fatalf("expected defaults to be valid: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*AudioSettings)
	}{
		{"empty device", func(settings *AudioSettings) { settings.Device = " " }},
		{"control character", func(settings *AudioSettings) { settings.Device = "monitor\nname" }},
		{"sensitivity zero", func(settings *AudioSettings) { settings.Sensitivity = 0 }},
		{"sensitivity too high", func(settings *AudioSettings) { settings.Sensitivity = 11 }},
		{"sensitivity NaN", func(settings *AudioSettings) { settings.Sensitivity = math.NaN() }},
		{"motion zero", func(settings *AudioSettings) { settings.Motion = 0 }},
		{"motion too high", func(settings *AudioSettings) { settings.Motion = 11 }},
		{"motion infinite", func(settings *AudioSettings) { settings.Motion = math.Inf(1) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := valid
			test.mutate(&settings)
			if err := validateAudioSettings(settings); err == nil {
				t.Fatal("expected invalid audio settings")
			}
		})
	}
}
