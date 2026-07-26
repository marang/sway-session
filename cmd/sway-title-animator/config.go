package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

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
	if err := validateConfigCompatibility(data); err != nil {
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
	if config.Settings.RotationHoldFrames != nil {
		settings.RotationHoldFrames = *config.Settings.RotationHoldFrames
	}
	if config.Settings.RotationBlendFrames != nil {
		settings.RotationBlendFrames = *config.Settings.RotationBlendFrames
	}
	if config.Settings.DetectChildProcess != nil {
		settings.DetectChildProcess = *config.Settings.DetectChildProcess
	}
	if config.Audio.Device != nil {
		audioSettings.Device = strings.TrimSpace(*config.Audio.Device)
	}
	if config.Audio.Sensitivity != nil {
		audioSettings.Sensitivity = *config.Audio.Sensitivity
	}
	if config.Audio.Motion != nil {
		audioSettings.Motion = *config.Audio.Motion
	}
	applyGlyphConfig(config.Glyphs)
	iconRules = append(configuredIconRules(config.Icons), iconRules...)
	for name, animation := range config.Animation {
		if len(animation.Frames) == 0 {
			continue
		}
		if isLegacyBundledFrameAnimation(name, animation) {
			continue
		}
		animationPresets[name] = frameAnimationArt(animation)
	}
	if len(config.Rotation.Presets) > 0 {
		rotationPresets = filterKnownPresets(config.Rotation.Presets)
	}
}

func validateConfigCompatibility(data []byte) error {
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return err
	}
	if _, exists := raw["showcase"]; exists {
		return errors.New("obsolete [showcase] config section; rename it to [rotation]")
	}
	if rawSettings, ok := raw["settings"].(map[string]any); ok {
		for _, name := range []string{"showcase_hold_frames", "showcase_blend_frames"} {
			if _, exists := rawSettings[name]; exists {
				replacement := strings.Replace(name, "showcase_", "rotation_", 1)
				return fmt.Errorf("obsolete settings.%s option; rename it to settings.%s", name, replacement)
			}
		}
	}
	if rawAudio, ok := raw["audio"].(map[string]any); ok {
		if _, exists := rawAudio["backend"]; exists {
			return errors.New("audio.backend is not supported; parec is the only production audio backend")
		}
	}
	return nil
}

func configuredIconRules(icons map[string]string) []iconRule {
	rules := make([]iconRule, 0, len(icons))
	for needle, icon := range icons {
		rules = append(rules, iconRule{needle: strings.ToLower(needle), icon: icon})
	}
	sort.Slice(rules, func(first int, second int) bool {
		if len(rules[first].needle) != len(rules[second].needle) {
			return len(rules[first].needle) > len(rules[second].needle)
		}
		if rules[first].needle != rules[second].needle {
			return rules[first].needle < rules[second].needle
		}
		return rules[first].icon < rules[second].icon
	})
	return rules
}

func validateSettings(settings Settings) error {
	if settings.FPS < 1 || settings.FPS > 60 {
		return errors.New("frames per second must be between 1 and 60")
	}
	if settings.Motion <= 0 {
		return errors.New("motion must be greater than 0")
	}
	if settings.ApproxCharWidth <= 0 {
		return errors.New("approximate character width must be greater than 0")
	}
	if settings.MaxArtColumns < 0 {
		return errors.New("maximum art columns must be greater than or equal to 0")
	}
	if settings.TitleReserveColumns < 0 {
		return errors.New("title reserve columns must be greater than or equal to 0")
	}
	if settings.RotationHoldFrames < 1 || settings.RotationBlendFrames < 1 {
		return errors.New("rotation hold/blend frames must be greater than 0")
	}
	return nil
}

func validateAudioSettings(settings AudioSettings) error {
	device := strings.TrimSpace(settings.Device)
	if device == "" {
		return errors.New("audio device must not be empty")
	}
	if len(device) > 512 || strings.IndexFunc(device, func(character rune) bool {
		return character < 0x20 || character == 0x7f
	}) >= 0 {
		return errors.New("audio device must be at most 512 characters and contain no control characters")
	}
	if math.IsNaN(settings.Sensitivity) || math.IsInf(settings.Sensitivity, 0) ||
		settings.Sensitivity <= 0 || settings.Sensitivity > 10 {
		return errors.New("audio sensitivity must be greater than 0 and at most 10")
	}
	if math.IsNaN(settings.Motion) || math.IsInf(settings.Motion, 0) ||
		settings.Motion <= 0 || settings.Motion > 10 {
		return errors.New("audio motion must be greater than 0 and at most 10")
	}
	return nil
}

func validateRuntimeSettings() error {
	if err := validateSettings(settings); err != nil {
		return err
	}
	return validateAudioSettings(audioSettings)
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
		if _, ok := animationPresets[name]; ok {
			filtered = append(filtered, name)
		}
	}
	if len(filtered) == 0 {
		return rotationPresets
	}
	return filtered
}
