package main

import (
	"strings"
	"testing"
	"time"
)

func TestBraidSoundBassMidsOnsetAndTreble(t *testing.T) {
	lowBass := braidSoundArtWithSnapshot(120, 30, audioSnapshot{
		Active: true, Bass: 0.1, LowMid: 0.4, HighMid: 0.4,
	})
	highBass := braidSoundArtWithSnapshot(120, 30, audioSnapshot{
		Active: true, Bass: 1, LowMid: 0.4, HighMid: 0.4,
	})
	lowMids := braidSoundArtWithSnapshot(120, 30, audioSnapshot{
		Active: true, Bass: 0.4, LowMid: 0.1, HighMid: 0.1,
	})
	highMids := braidSoundArtWithSnapshot(120, 30, audioSnapshot{
		Active: true, Bass: 0.4, LowMid: 1, HighMid: 1,
	})
	energized := audioSnapshot{
		Active: true, Bass: 1, LowMid: 1, HighMid: 1, Treble: 1, OnsetCount: 1,
	}
	energized.Onsets[0] = audioOnset{
		ID: 2, Age: 180 * time.Millisecond, Strength: 1,
		Region: audioRegionGeneral, Position: 0.8,
	}
	active := braidSoundArtWithSnapshot(120, 30, energized)
	if lowBass == highBass {
		t.Fatalf("bass should change strand amplitude: low=%q high=%q", lowBass, highBass)
	}
	if highMids == lowMids {
		t.Fatalf("mids should increase crossing density: low=%q high=%q", lowMids, highMids)
	}
	if !strings.ContainsRune(active, '✦') || lastIndexRune(active, '╳') <= 60 {
		t.Fatalf("onset and treble should emphasize the selected strand: %q", active)
	}
}

func TestBraidSoundHighlightFollowsStereo(t *testing.T) {
	right, left := []rune(strings.Repeat("╱", 80)), []rune(strings.Repeat("╱", 80))
	addBraidSoundHighlight(right, 30, 1, 1)
	addBraidSoundHighlight(left, 30, 1, -1)
	if lastIndexRune(string(right), '✦') >= lastIndexRune(string(left), '✦') {
		t.Fatalf("opposite strands should travel oppositely: right=%q left=%q", string(right), string(left))
	}

	quiet, loud := []rune(strings.Repeat("╱", 80)), []rune(strings.Repeat("╱", 80))
	addBraidSoundHighlight(quiet, 30, 0.4, 1)
	addBraidSoundHighlight(loud, 30, 1, 1)
	if lastIndexRune(string(quiet), '✦') != lastIndexRune(string(loud), '✦') {
		t.Fatalf("instantaneous treble must not relocate the moving highlight: quiet=%q loud=%q",
			string(quiet), string(loud))
	}
}

func TestBraidNeverCollapsesIntoAStaticRowOfCrossings(t *testing.T) {
	originalSeed := animationSeed
	t.Cleanup(func() {
		animationSeed = originalSeed
	})

	for seed := uint64(1); seed <= 64; seed++ {
		animationSeed = seed
		for _, phase := range []int{0, 30, 90, 180, 270} {
			frame := braidArt(120, phase)
			if !strings.ContainsAny(frame, "╱╲") {
				t.Fatalf("seed=%d phase=%d collapsed braid: %q", seed, phase, frame)
			}
		}
	}
}

func TestLoomSoundWarpWeftShuttleAndGlints(t *testing.T) {
	loose := loomSoundArtWithSnapshot(120, 20, audioSnapshot{
		Active: true, Bass: 0.1, LowMid: 0.1, HighMid: 0.1,
	})
	active := audioSnapshot{
		Active: true, Bass: 1, LowMid: 1, HighMid: 1, Treble: 1, OnsetCount: 1,
	}
	active.Onsets[0] = audioOnset{
		ID: 4, Age: 260 * time.Millisecond, Strength: 1,
		Region: audioRegionGeneral, Position: 1,
	}
	tight := loomSoundArtWithSnapshot(120, 20, active)
	if strings.Count(tight, "▓") <= strings.Count(loose, "▓") ||
		!strings.ContainsRune(tight, '✦') || !strings.ContainsRune(tight, '·') {
		t.Fatalf("audio should tighten and glint the textile: loose=%q tight=%q", loose, tight)
	}
	_, looseRadius, _ := loomSoundShuttle(120, active.Onsets[0], audioSnapshot{})
	_, tightRadius, _ := loomSoundShuttle(120, active.Onsets[0], active)
	if tightRadius <= looseRadius {
		t.Fatalf("mids should widen shuttle section: loose=%d tight=%d", looseRadius, tightRadius)
	}
}

func TestLoomSoundShuttleDirectionFollowsStereo(t *testing.T) {
	onset := audioOnset{
		ID: 1, Age: 250 * time.Millisecond, Strength: 1,
		Region: audioRegionGeneral, Position: 1,
	}
	right, _, rightLive := loomSoundShuttle(100, onset, audioSnapshot{})
	onset.Position = -1
	left, _, leftLive := loomSoundShuttle(100, onset, audioSnapshot{})
	if !rightLive || !leftLive || right >= 50 || left <= 50 {
		t.Fatalf("shuttle should enter from selected side: right=%d left=%d", right, left)
	}
}

func TestBraidAndLoomSoundStayBoundedDeterministicAndMoveAtRest(t *testing.T) {
	for name, animation := range map[string]func(int, int, audioSnapshot) string{
		"braid_sound": braidSoundArtWithSnapshot,
		"loom_sound":  loomSoundArtWithSnapshot,
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
				t.Fatalf("silent textile should keep slow motion: %v", frames)
			}
		})
	}
}
