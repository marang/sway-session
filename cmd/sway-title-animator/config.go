package main

import (
	"errors"
	"fmt"
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
	if len(config.Showcase.Presets) > 0 {
		showcasePresets = filterKnownPresets(config.Showcase.Presets)
	}
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
	if settings.ShowcaseHoldFrames < 1 || settings.ShowcaseBlendFrames < 1 {
		return errors.New("showcase hold/blend frames must be greater than 0")
	}
	return nil
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
