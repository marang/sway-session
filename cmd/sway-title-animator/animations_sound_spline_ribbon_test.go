package main

import (
	"math/bits"
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
	lowDots, highDots := 0, 0
	for _, glyph := range lowFrame {
		if glyph >= 0x2800 && glyph <= 0x28ff {
			lowDots += bits.OnesCount(uint(glyph - 0x2800))
		}
	}
	for _, glyph := range highFrame {
		if glyph >= 0x2800 && glyph <= 0x28ff {
			highDots += bits.OnesCount(uint(glyph - 0x2800))
		}
	}
	if highDots <= lowDots || lowFrame == highFrame {
		t.Fatalf("band energy should thicken the preserved spline: low=%q high=%q",
			lowFrame, highFrame)
	}
	if !strings.ContainsAny(lowFrame, "◇◆✦✧") ||
		!strings.ContainsAny(highFrame, "◇◆✦✧") {
		t.Fatalf("spline lost its tracer choreography: low=%q high=%q",
			lowFrame, highFrame)
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
	if reversed := ribbonSoundArtWithSnapshot(120, 20, active); reversed != energized {
		t.Fatal("instantaneous stereo balance must not reverse or jump the ribbon")
	}
}

func TestRibbonSoundBeatPulseReshapesDistributedFolds(t *testing.T) {
	audio := audioSnapshot{Active: true, Level: 0.8, OnsetCount: 1}
	audio.Onsets[0] = audioOnset{
		ID: 4, Age: 280 * time.Millisecond, Strength: 1,
		Region: audioRegionGeneral, Position: 1,
	}
	if pulse := ribbonSoundPulse(audio); pulse <= 0.45 {
		t.Fatalf("expected a visible beat pulse, got %.2f", pulse)
	}
	frame := ribbonSoundArtWithSnapshot(100, 20, audio)
	if strings.ContainsAny(frame, "╳╪┄╍╱╲") {
		t.Fatalf("ribbon pulse must not become a glitch tear: %q", frame)
	}
	withoutOnset := audio
	withoutOnset.OnsetCount = 0
	withoutOnset.Onsets = [audioEventCapacity]audioOnset{}
	calm := []rune(ribbonSoundArtWithSnapshot(100, 20, withoutOnset))
	pulsed := []rune(frame)
	changedQuarters := 0
	for quarter := range 4 {
		start := quarter * 25
		changed := false
		for index := start; index < start+25; index++ {
			if calm[index] != pulsed[index] {
				changed = true
				break
			}
		}
		if changed {
			changedQuarters++
		}
	}
	if changedQuarters < 3 {
		t.Fatalf("beat should reshape folds across the ribbon, changed quarters=%d",
			changedQuarters)
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
