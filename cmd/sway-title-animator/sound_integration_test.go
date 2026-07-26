package main

import (
	"math"
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

func TestInactiveAudioPreservesEveryCompleteBaseChoreography(t *testing.T) {
	originalSnapshot := currentAudioSnapshot
	t.Cleanup(func() {
		currentAudioSnapshot = originalSnapshot
	})
	for _, captureAvailable := range []bool{false, true} {
		currentAudioSnapshot = func() audioSnapshot {
			return audioSnapshot{CaptureAvailable: captureAvailable}
		}
		for name := range soundPresetNames {
			baseName := strings.TrimSuffix(name, "_sound")
			got := animationFuncFor(name)(80, 37)
			want := animationPresets[baseName](80, 37)
			if got != want {
				t.Errorf("%s did not preserve %s with capture=%t: got=%q want=%q",
					name, baseName, captureAvailable, got, want)
			}
		}
	}
}

func TestEverySoundCompanionKeepsMovingWithSteadyAudio(t *testing.T) {
	renderers := soundSnapshotRenderers()
	audio := steadySoundTestSnapshot()

	for name, render := range renderers {
		t.Run(name, func(t *testing.T) {
			frames := map[string]bool{}
			for _, phase := range []int{0, 73, 149, 227} {
				frames[render(100, phase, audio)] = true
			}
			if len(frames) < 2 {
				t.Fatalf("steady audio froze the animation across its sampled cycle")
			}
		})
	}
}

func TestReworkedSoundCompanionsPreserveBaseTemporalChoreography(t *testing.T) {
	originalSeed := animationSeed
	animationSeed = 0x1ab53
	t.Cleanup(func() {
		animationSeed = originalSeed
	})

	audio := steadySoundTestSnapshot()
	audio.OnsetCount = 0
	audio.Onsets = [audioEventCapacity]audioOnset{}
	renderers := map[string]func(int, int, audioSnapshot) string{
		"bloom_sound":  bloomSoundArtWithSnapshot,
		"braid_sound":  braidSoundArtWithSnapshot,
		"comet_sound":  cometSoundArtWithSnapshot,
		"ribbon_sound": ribbonSoundArtWithSnapshot,
		"spline_sound": splineSoundArtWithSnapshot,
		"wave_sound":   waveSoundArtWithSnapshot,
	}
	phases := []int{0, 6, 12, 18, 24, 30, 36, 42}

	for name, render := range renderers {
		t.Run(name, func(t *testing.T) {
			baseName := strings.TrimSuffix(name, "_sound")
			baseMotion := 0.0
			soundMotion := 0.0
			uniqueSoundFrames := map[string]bool{}
			for index, phase := range phases {
				base := animationPresets[baseName](100, phase)
				sound := render(100, phase, audio)
				uniqueSoundFrames[sound] = true
				if index == 0 {
					continue
				}
				previousPhase := phases[index-1]
				baseMotion += frameDifferenceRatio(
					animationPresets[baseName](100, previousPhase),
					base,
				)
				soundMotion += frameDifferenceRatio(
					render(100, previousPhase, audio),
					sound,
				)
			}
			if len(uniqueSoundFrames) < len(phases)-1 {
				t.Fatalf("sound companion dropped base motion: %d/%d unique frames",
					len(uniqueSoundFrames), len(phases))
			}
			minimumMotion := math.Max(0.05, baseMotion*0.35)
			if soundMotion < minimumMotion {
				t.Fatalf("sound motion %.3f lost too much base choreography %.3f",
					soundMotion, baseMotion)
			}
		})
	}
}

func TestEverySoundCompanionRespondsClearlyToMusic(t *testing.T) {
	renderers := soundSnapshotRenderers()
	quiet := steadySoundTestSnapshot()
	quiet.Level, quiet.Bass, quiet.LowMid, quiet.HighMid, quiet.Treble = 0.16, 0.14, 0.12, 0.15, 0.10
	for index := range quiet.Bands {
		quiet.Bands[index] = 0.10 + float64(index%3)*0.025
	}
	normal := steadySoundTestSnapshot()
	normal.OnsetCount = 3
	normal.Onsets[0] = audioOnset{
		ID: 1, Age: 220 * time.Millisecond, Strength: 0.78,
		Region: audioRegionGeneral, Position: 0.35,
	}
	normal.Onsets[1] = audioOnset{
		ID: 2, Age: 180 * time.Millisecond, Strength: 0.82,
		Region: audioRegionBass, Position: -0.25,
	}
	normal.Onsets[2] = audioOnset{
		ID: 3, Age: 140 * time.Millisecond, Strength: 0.74,
		Region: audioRegionHigh, Position: 0.15,
	}

	const (
		width                  = 100
		minimumBaseDifference  = 0.04
		minimumAudioDifference = 0.08
	)
	phases := []int{37, 83, 149}
	for name, render := range renderers {
		t.Run(name, func(t *testing.T) {
			baseName := strings.TrimSuffix(name, "_sound")
			maximumBaseDifference := 0.0
			maximumAudioDifference := 0.0
			for _, phase := range phases {
				base := string(fitRunes(animationPresets[baseName](width, phase), width))
				quietFrame := render(width, phase, quiet)
				normalFrame := render(width, phase, normal)
				maximumBaseDifference = math.Max(
					maximumBaseDifference,
					frameDifferenceRatio(base, normalFrame),
				)
				maximumAudioDifference = math.Max(
					maximumAudioDifference,
					frameDifferenceRatio(quietFrame, normalFrame),
				)
			}
			if maximumBaseDifference < minimumBaseDifference {
				t.Fatalf("normal music response is too close to %s across sampled phases: maximum difference %.3f",
					baseName, maximumBaseDifference)
			}
			if maximumAudioDifference < minimumAudioDifference {
				t.Fatalf("quiet-to-normal music response is too small across sampled phases: maximum difference %.3f",
					maximumAudioDifference)
			}
		})
	}
}

func TestSoundCompanionsDoNotJumpForSmallAudioChange(t *testing.T) {
	audio := steadySoundTestSnapshot()
	nearby := audio
	nearby.Level += 0.015
	nearby.Bass += 0.015
	nearby.LowMid += 0.015
	nearby.HighMid += 0.015
	nearby.Treble += 0.015
	nearby.Centroid += 0.015
	nearby.Balance += 0.015
	nearby.SpectralFlux += 0.015
	for index := range nearby.Bands {
		nearby.Bands[index] = math.Min(1, nearby.Bands[index]+0.015)
	}

	for name, render := range soundSnapshotRenderers() {
		t.Run(name, func(t *testing.T) {
			if name == "square_sound" {
				// Plateau lengths are the audio signal; a one-column change
				// intentionally reflows the connected waveform.
				return
			}
			first := render(100, 83, audio)
			second := render(100, 83, nearby)
			if difference := frameDifferenceRatio(first, second); difference > 0.45 {
				t.Fatalf("small audio change replaced too much of the frame: difference %.3f\nfirst:  %q\nsecond: %q",
					difference, first, second)
			}
		})
	}
}

func soundSnapshotRenderers() map[string]func(int, int, audioSnapshot) string {
	return map[string]func(int, int, audioSnapshot) string{
		"aurora_sound":        auroraSoundArtWithSnapshot,
		"bloom_sound":         bloomSoundArtWithSnapshot,
		"braid_sound":         braidSoundArtWithSnapshot,
		"circuit_sound":       circuitSoundArtWithSnapshot,
		"comet_sound":         cometSoundArtWithSnapshot,
		"constellation_sound": constellationSoundArtWithSnapshot,
		"glitch_sound":        glitchSoundArtWithSnapshot,
		"loom_sound":          loomSoundArtWithSnapshot,
		"radar_sound":         radarSoundArtWithSnapshot,
		"ribbon_sound":        ribbonSoundArtWithSnapshot,
		"ripples_sound":       ripplesSoundArtWithSnapshot,
		"shutter_sound":       shutterSoundArtWithSnapshot,
		"smileys_sound":       smileysSoundArtWithSnapshot,
		"spectrum_sound":      spectrumSoundArtWithSnapshot,
		"spline_sound":        splineSoundArtWithSnapshot,
		"square_sound":        squareSoundArtWithSnapshot,
		"wave_sound":          waveSoundArtWithSnapshot,
	}
}

func steadySoundTestSnapshot() audioSnapshot {
	audio := audioSnapshot{
		CaptureAvailable: true,
		Active:           true,
		Level:            0.62,
		Bass:             0.58,
		LowMid:           0.51,
		HighMid:          0.66,
		Treble:           0.47,
		Centroid:         0.56,
		Balance:          0.18,
		SpectralFlux:     0.31,
		Peak:             0.64,
	}
	for index := range audio.Bands {
		audio.Bands[index] = 0.28 + float64(index%7)*0.08
	}
	return audio
}

func frameDifferenceRatio(first string, second string) float64 {
	firstRunes := []rune(first)
	secondRunes := []rune(second)
	width := max(len(firstRunes), len(secondRunes))
	if width == 0 {
		return 0
	}
	different := 0
	for index := range width {
		var firstRune, secondRune rune
		if index < len(firstRunes) {
			firstRune = firstRunes[index]
		}
		if index < len(secondRunes) {
			secondRune = secondRunes[index]
		}
		if firstRune != secondRune {
			different++
		}
	}
	return float64(different) / float64(width)
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
