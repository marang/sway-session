package main

import (
	"strings"
	"testing"
	"time"
)

func TestEveryBuiltInBasePresetHasRegisteredSoundCompanion(t *testing.T) {
	for name := range animationPresets {
		if strings.HasSuffix(name, "_sound") {
			continue
		}
		companion := name + "_sound"
		if _, ok := animationPresets[companion]; !ok {
			t.Errorf("base preset %q has no registered companion %q", name, companion)
		}
		if !isSoundPreset(companion) {
			t.Errorf("companion %q is missing from audio activation registry", companion)
		}
	}
}

func TestUnavailableCaptureFallsBackForEverySoundCompanion(t *testing.T) {
	originalSnapshot := currentAudioSnapshot
	t.Cleanup(func() {
		currentAudioSnapshot = originalSnapshot
	})
	currentAudioSnapshot = func() audioSnapshot {
		return audioSnapshot{}
	}

	for name := range soundPresetNames {
		baseName := strings.TrimSuffix(name, "_sound")
		got := animationFuncFor(name)(80, 37)
		want := animationPresets[baseName](80, 37)
		if got != want {
			t.Errorf("%s did not fall back to %s: got=%q want=%q",
				name, baseName, got, want)
		}
	}
}

func BenchmarkAllSoundPresets(b *testing.B) {
	originalSnapshot := currentAudioSnapshot
	b.Cleanup(func() {
		currentAudioSnapshot = originalSnapshot
	})
	audio := audioSnapshot{
		CaptureAvailable: true,
		Active:           true,
		Level:            0.72,
		Bass:             0.68,
		LowMid:           0.55,
		HighMid:          0.63,
		Treble:           0.58,
		Centroid:         0.61,
		Balance:          0.24,
		SpectralFlux:     0.42,
		Peak:             0.81,
		OnsetCount:       2,
	}
	for index := range audio.Bands {
		audio.Bands[index] = float64(index%9) / 8
	}
	audio.Onsets[0] = audioOnset{
		ID: 1, Age: 180 * time.Millisecond, Strength: 0.92,
		Region: audioRegionGeneral, Position: 0.4,
	}
	audio.Onsets[1] = audioOnset{
		ID: 2, Age: 240 * time.Millisecond, Strength: 0.84,
		Region: audioRegionBass, Position: -0.3,
	}
	currentAudioSnapshot = func() audioSnapshot {
		return audio
	}
	names := make([]string, 0, len(soundPresetNames))
	for name := range soundPresetNames {
		names = append(names, name)
	}

	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		for _, name := range names {
			_ = animationPresets[name](120, index)
		}
	}
}
