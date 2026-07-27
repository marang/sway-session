package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDominoBuildsACompleteSeededChainReaction(t *testing.T) {
	originalSeed := animationSeed
	animationSeed = 0x5eed
	t.Cleanup(func() {
		animationSeed = originalSeed
	})

	frames := map[string]bool{}
	vocabulary := ""
	for phase := 0; phase < dominoCycleFrames; phase += 12 {
		frame := dominoArt(80, phase)
		if len([]rune(frame)) != 80 {
			t.Fatalf("phase %d rendered wrong width: %q", phase, frame)
		}
		frames[frame] = true
		vocabulary += frame
	}
	for cycle := 0; cycle < 24; cycle++ {
		state := dominoCycleAt(cycle * dominoCycleFrames)
		phase := cycle*dominoCycleFrames + state.holdFrames + state.fallFrames/2
		vocabulary += dominoArt(80, phase)
	}
	if len(frames) < 6 {
		t.Fatalf("domino choreography is too static: %d unique frames", len(frames))
	}
	for _, glyph := range "▮━╱╲" {
		if !strings.ContainsRune(vocabulary, glyph) {
			t.Fatalf("domino choreography never rendered %q: %q", glyph, vocabulary)
		}
	}

	first := dominoArt(80, 96)
	animationSeed = 0x5eed
	if second := dominoArt(80, 96); second != first {
		t.Fatalf("fixed seed is not deterministic:\nfirst:  %q\nsecond: %q", first, second)
	}
}

func TestDominoSupportsTinyAndNegativeWidths(t *testing.T) {
	for _, width := range []int{-3, 0, 1, 2, 7, 8, 80, 220} {
		for _, phase := range []int{-193, -1, 0, 191, 512} {
			frame := dominoArt(width, phase)
			if got, want := len([]rune(frame)), artWidth(width); got != want {
				t.Fatalf("width=%d phase=%d rendered %d runes, want %d", width, phase, got, want)
			}
		}
	}
}

func TestDominoKeepsLayoutStableWhileDirectionAndTempoVary(t *testing.T) {
	originalSeed := animationSeed
	animationSeed = 0x1ab53
	t.Cleanup(func() {
		animationSeed = originalSeed
	})

	directions := map[bool]bool{}
	fallFrames := map[int]bool{}
	layout := dominoPositions(80)
	for cycle := int64(0); cycle < 24; cycle++ {
		state := dominoCycleAt(int(cycle) * dominoCycleFrames)
		directions[state.leftToRight] = true
		fallFrames[state.fallFrames] = true
		if positions := dominoPositions(80); !slices.Equal(positions, layout) {
			t.Fatalf("cycle %d changed the standing-stone layout: %v != %v",
				cycle, positions, layout)
		}
	}
	if len(directions) != 2 || len(fallFrames) < 6 {
		t.Fatalf("expected organic cycle variety, directions=%v fallFrames=%v",
			directions, fallFrames)
	}
	if before, after := dominoArt(80, dominoCycleFrames-1), dominoArt(80, dominoCycleFrames); before != after {
		t.Fatalf("cycle boundary jumped instead of returning smoothly to the stable layout:\nbefore: %q\nafter:  %q",
			before, after)
	}
}

func TestDominoSoundBeatStartsLocalOutwardCascade(t *testing.T) {
	audio := audioSnapshot{
		Active:     true,
		Bass:       0.72,
		LowMid:     0.64,
		Treble:     0.78,
		OnsetCount: 1,
	}
	audio.Onsets[0] = audioOnset{
		ID: 7, Sequence: 2, Age: 280 * time.Millisecond, Strength: 0.92,
		Region: audioRegionGeneral, Position: 0.15,
	}
	base := dominoArt(100, 0)
	sound := dominoSoundArtWithSnapshot(100, 0, audio)
	if sound == base {
		t.Fatal("beat did not alter the domino chain")
	}
	if !strings.ContainsAny(sound, "╱╲") {
		t.Fatalf("beat did not expose outward falling fronts: %q", sound)
	}
	if !strings.ContainsAny(sound, "•✦") {
		t.Fatalf("treble did not add restrained collision sparks: %q", sound)
	}
	if difference := frameDifferenceRatio(base, sound); difference < 0.04 || difference > 0.48 {
		t.Fatalf("beat response should remain local and legible, difference %.3f", difference)
	}
}

func TestDominoSoundBassControlsCascadeReachAndDecaysToBase(t *testing.T) {
	quiet := audioSnapshot{Active: true, Bass: 0.05, LowMid: 0.35, OnsetCount: 1}
	quiet.Onsets[0] = audioOnset{
		ID: 1, Age: 430 * time.Millisecond, Strength: 0.9,
		Region: audioRegionGeneral, Position: -0.2,
	}
	heavy := quiet
	heavy.Bass = 1
	base := dominoArt(120, 0)
	quietFrame := dominoSoundArtWithSnapshot(120, 0, quiet)
	heavyFrame := dominoSoundArtWithSnapshot(120, 0, heavy)
	quietDifference := frameDifferenceRatio(base, quietFrame)
	heavyDifference := frameDifferenceRatio(base, heavyFrame)
	if heavyDifference <= quietDifference {
		t.Fatalf("bass did not expand cascade reach: quiet=%.3f heavy=%.3f",
			quietDifference, heavyDifference)
	}

	heavy.Onsets[0].Age = dominoSoundCascadeLifetime
	if decayed := dominoSoundArtWithSnapshot(120, 0, heavy); decayed != base {
		t.Fatalf("expired cascade did not return to base:\ngot:  %q\nwant: %q", decayed, base)
	}
}

func TestDominoSoundInactiveAndSilentCapturePreserveBase(t *testing.T) {
	for _, audio := range []audioSnapshot{
		{},
		{CaptureAvailable: true},
		{CaptureAvailable: true, Active: true},
	} {
		for _, phase := range []int{0, 47, 113, 191, 257} {
			got := dominoSoundArtWithSnapshot(80, phase, audio)
			want := dominoArt(80, phase)
			if got != want {
				t.Fatalf("audio=%+v phase=%d changed calm base:\ngot:  %q\nwant: %q",
					audio, phase, got, want)
			}
		}
	}
}
