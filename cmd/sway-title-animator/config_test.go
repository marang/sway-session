package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
