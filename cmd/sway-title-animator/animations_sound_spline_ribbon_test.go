package main

import (
	"strings"
	"testing"
	"time"
)

func TestSplineSoundBandsDisplaceContinuousCurve(t *testing.T) {
	low, high := audioSnapshot{Active: true}, audioSnapshot{Active: true}
	for index := range low.Bands {
		low.Bands[index], high.Bands[index] = 0.1, 0.9
	}
	lowFrame := splineSoundArtWithSnapshot(100, 20, low)
	highFrame := splineSoundArtWithSnapshot(100, 20, high)
	if strings.Count(highFrame, "⣀") <= strings.Count(lowFrame, "⣀") {
		t.Fatalf("band energy should displace curve: low=%q high=%q", lowFrame, highFrame)
	}
	for _, frame := range []string{lowFrame, highFrame} {
		for _, glyph := range frame {
			if !strings.ContainsRune("⠉⠒⠤⣀◇◆", glyph) {
				t.Fatalf("spline lost continuous vocabulary: %q", frame)
			}
		}
	}
}

func TestSplineSoundTracerUsesCentroidStereoAndOnset(t *testing.T) {
	left := audioSnapshot{Active: true, Centroid: 0.2, Balance: 1}
	right := audioSnapshot{Active: true, Centroid: 0.8, Balance: 1}
	if splineSoundTracer(100, 0, left) >= splineSoundTracer(100, 0, right) {
		t.Fatal("centroid should place tracer across curve")
	}
	forward := splineSoundTracer(100, 20, right)
	right.Balance = -1
	reverse := splineSoundTracer(100, 20, right)
	if forward <= reverse {
		t.Fatalf("balance should bias tracer direction: forward=%d reverse=%d", forward, reverse)
	}
	right.OnsetCount = 1
	right.Onsets[0] = audioOnset{
		ID: 1, Age: 100 * time.Millisecond, Strength: 1, Region: audioRegionGeneral,
	}
	if !strings.ContainsRune(splineSoundArtWithSnapshot(100, 20, right), '◆') {
		t.Fatal("strong onset should brighten tracer")
	}
}

func TestRibbonSoundBandsBassMidsTrebleAndDirection(t *testing.T) {
	calm := ribbonSoundArtWithSnapshot(120, 20, audioSnapshot{
		Active: true, Bass: 0.1, LowMid: 0.1, HighMid: 0.1,
	})
	active := audioSnapshot{
		Active: true, Bass: 1, LowMid: 1, HighMid: 1, Treble: 1,
		Centroid: 1, Balance: 1,
	}
	for index := range active.Bands {
		active.Bands[index] = float64(index%5) / 4
	}
	energized := ribbonSoundArtWithSnapshot(120, 20, active)
	if calm == energized || !strings.ContainsRune(energized, '█') ||
		!strings.ContainsRune(energized, '✦') {
		t.Fatalf("audio should shape ribbon and highlights: %q", energized)
	}
	active.Balance = -1
	if reversed := ribbonSoundArtWithSnapshot(120, 20, active); reversed == energized {
		t.Fatal("stereo balance should reverse ribbon drift")
	}
}

func TestRibbonSoundTwistPropagatesWithoutGlitchVocabulary(t *testing.T) {
	audio := audioSnapshot{Active: true, Level: 0.8, OnsetCount: 1}
	audio.Onsets[0] = audioOnset{
		ID: 4, Age: 280 * time.Millisecond, Strength: 1,
		Region: audioRegionGeneral, Position: 1,
	}
	center, _, live := ribbonSoundTwist(100, audio)
	if !live || center <= 0 || center >= 50 {
		t.Fatalf("twist should propagate from selected edge: %.2f", center)
	}
	frame := ribbonSoundArtWithSnapshot(100, 20, audio)
	if strings.ContainsAny(frame, "╳╪┄╍") {
		t.Fatalf("ribbon twist must not become a glitch tear: %q", frame)
	}
}

func TestSplineAndRibbonSoundStayBoundedDeterministicAndMoveAtRest(t *testing.T) {
	for name, animation := range map[string]func(int, int, audioSnapshot) string{
		"spline_sound": splineSoundArtWithSnapshot,
		"ribbon_sound": ribbonSoundArtWithSnapshot,
	} {
		t.Run(name, func(t *testing.T) {
			frames := map[string]bool{}
			for _, width := range []int{0, 1, 7, 8, 80, 220} {
				for _, phase := range []int{0, 47, 113, 229} {
					frame := animation(width, phase, audioSnapshot{})
					if len([]rune(frame)) != artWidth(width) {
						t.Fatalf("width=%d rendered %d runes", width, len([]rune(frame)))
					}
					if repeated := animation(width, phase, audioSnapshot{}); repeated != frame {
						t.Fatalf("fixed input must be deterministic: %q != %q", frame, repeated)
					}
					if width == 80 {
						frames[frame] = true
					}
				}
			}
			if len(frames) < 2 {
				t.Fatalf("silent form should keep slow motion: %v", frames)
			}
		})
	}
}
