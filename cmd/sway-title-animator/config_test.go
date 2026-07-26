package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestRejectObsoleteShowcaseConfig(t *testing.T) {
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
			err := rejectObsoleteShowcaseConfig([]byte(test.config))
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
	if err := rejectObsoleteShowcaseConfig(data); err != nil {
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
