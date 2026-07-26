package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestSquareSoundBassAndLevelShapeConnectedTrace(t *testing.T) {
	lowBass := audioSnapshot{Active: true, Level: 0.35, Bass: 0.1}
	highBass := audioSnapshot{Active: true, Level: 0.35, Bass: 0.95}
	lowLevels := squareSoundLevels(120, 73, lowBass)
	highLevels := squareSoundLevels(120, 73, highBass)
	if transitions(highLevels) >= transitions(lowLevels) {
		t.Fatalf("higher bass should lengthen plateaus: low=%d high=%d",
			transitions(lowLevels), transitions(highLevels))
	}

	quiet := squareSoundLevels(120, 73, audioSnapshot{Active: true, Level: 0.1, Bass: 0.5})
	loud := squareSoundLevels(120, 73, audioSnapshot{Active: true, Level: 0.9, Bass: 0.5})
	if countHigh(loud) <= countHigh(quiet) {
		t.Fatalf("overall level should increase the high duty cycle: quiet=%d loud=%d",
			countHigh(quiet), countHigh(loud))
	}

	for _, frame := range []string{
		squareSoundArtWithSnapshot(80, 73, lowBass),
		squareSoundArtWithSnapshot(80, 73, highBass),
		squareSoundArtWithSnapshot(80, 73, audioSnapshot{}),
	} {
		if frameContainsBraille(frame) {
			t.Fatalf("square_sound must never use Braille: %q", frame)
		}
		for _, glyph := range frame {
			if glyph != ' ' && !strings.ContainsRune("⎺⎽⎡⎤", glyph) {
				t.Fatalf("square_sound used disconnected glyph %q in %q", glyph, frame)
			}
		}
	}
}

func TestSquareSoundBuildAndRunnerFollowStereoDirection(t *testing.T) {
	right := audioSnapshot{Active: true, Level: 0.5, Bass: 0.5, Balance: 0.8}
	left := right
	left.Balance = -0.8
	rightFrame := []rune(squareSoundArtWithSnapshot(80, 10, right))
	leftFrame := []rune(squareSoundArtWithSnapshot(80, 10, left))
	if rightFrame[0] == ' ' || rightFrame[len(rightFrame)-1] != ' ' {
		t.Fatalf("positive balance should build from the left: %q", string(rightFrame))
	}
	if leftFrame[0] != ' ' || leftFrame[len(leftFrame)-1] == ' ' {
		t.Fatalf("negative balance should build from the right: %q", string(leftFrame))
	}

	base := squareSoundLevels(80, 61, right)
	rightRunner := append([]bool(nil), base...)
	leftRunner := append([]bool(nil), base...)
	onset := audioOnset{
		Age:      300 * time.Millisecond,
		Strength: 1,
		Region:   audioRegionGeneral,
		Position: 0.8,
	}
	applySquareSoundRunner(rightRunner, onset)
	onset.Position = -0.8
	applySquareSoundRunner(leftRunner, onset)
	if changedCenter(base, rightRunner) <= changedCenter(base, leftRunner) {
		t.Fatalf("runner directions did not separate: right=%.1f left=%.1f",
			changedCenter(base, rightRunner), changedCenter(base, leftRunner))
	}
}

func TestSquareSoundRunnerStrengthControlsLengthAndSpeed(t *testing.T) {
	onset := audioOnset{
		Age:      220 * time.Millisecond,
		Strength: 0.3,
		Region:   audioRegionGeneral,
		Position: 1,
	}
	weak, weakOK := squareSoundRunner(100, onset)
	onset.Strength = 1
	strong, strongOK := squareSoundRunner(100, onset)
	if !weakOK || !strongOK {
		t.Fatal("expected both runners to be live")
	}
	if strong.barLength <= weak.barLength {
		t.Fatalf("strong runner should overwrite more trace: weak=%d strong=%d",
			weak.barLength, strong.barLength)
	}
	if strong.left <= weak.left {
		t.Fatalf("strong runner should travel faster: weak=%d strong=%d",
			weak.left, strong.left)
	}
}

func TestRipplesSoundRegionsControlPositionSpeedAndWidth(t *testing.T) {
	bass := audioOnset{
		Age:      300 * time.Millisecond,
		Strength: 0.8,
		Region:   audioRegionBass,
		Position: 0.8,
	}
	high := bass
	high.Region = audioRegionHigh
	bassCenter, bassRadius, bassThickness, _, bassLive := soundRipple(100, bass)
	highCenter, highRadius, highThickness, _, highLive := soundRipple(100, high)
	if !bassLive || !highLive {
		t.Fatal("expected both regional ripples to be live")
	}
	if bassThickness <= highThickness || bassRadius >= highRadius {
		t.Fatalf("expected broad slow bass and narrow fast highs: bass radius=%.2f width=%.2f high radius=%.2f width=%.2f",
			bassRadius, bassThickness, highRadius, highThickness)
	}
	if bassCenter <= 50 || highCenter <= bassCenter {
		t.Fatalf("frequency region and positive stereo should bias positions right: bass=%.2f high=%.2f",
			bassCenter, highCenter)
	}
}

func TestRipplesSoundUsesBoundedImmutableOnsetHistory(t *testing.T) {
	audio := audioSnapshot{Active: true, OnsetCount: 2}
	audio.Onsets[0] = audioOnset{
		ID:       1,
		Age:      180 * time.Millisecond,
		Strength: 1,
		Region:   audioRegionBass,
		Position: -0.7,
	}
	audio.Onsets[1] = audioOnset{
		ID:       2,
		Age:      120 * time.Millisecond,
		Strength: 0.8,
		Region:   audioRegionHigh,
		Position: 0.7,
	}
	frame := ripplesSoundArtWithSnapshot(100, 31, audio)
	if !strings.ContainsAny(frame, "●═─╴╶") {
		t.Fatalf("expected audio-driven ripple rings, got %q", frame)
	}
	if repeated := ripplesSoundArtWithSnapshot(100, 31, audio); repeated != frame {
		t.Fatal("fixed onset history must render deterministically")
	}

	expired := audio
	expired.Onsets[0].Age = 2 * time.Second
	expired.Onsets[1].Age = 2 * time.Second
	if idle := ripplesSoundArtWithSnapshot(100, 31, expired); idle == frame {
		t.Fatal("expired onsets must fall back to the subtle idle pulse")
	}
	if audio.Onsets[0].Age != 180*time.Millisecond {
		t.Fatal("renderer must not mutate the immutable onset snapshot")
	}
}

func TestSoundPairSilentFormsMoveAndStayBounded(t *testing.T) {
	for name, animation := range map[string]func(int, int, audioSnapshot) string{
		"square_sound":  squareSoundArtWithSnapshot,
		"ripples_sound": ripplesSoundArtWithSnapshot,
	} {
		t.Run(name, func(t *testing.T) {
			frames := map[string]bool{}
			for _, width := range []int{0, 1, 7, 8, 80, 220} {
				for _, phase := range []int{0, 47, 113, 229} {
					frame := animation(width, phase, audioSnapshot{})
					if len([]rune(frame)) != artWidth(width) {
						t.Fatalf("width=%d phase=%d rendered %d runes", width, phase, len([]rune(frame)))
					}
					if width == 80 {
						frames[frame] = true
					}
				}
			}
			if len(frames) < 2 {
				t.Fatalf("silent form should retain slow organic motion, got %v", frames)
			}
		})
	}
}

func transitions(levels []bool) int {
	count := 0
	for index := 1; index < len(levels); index++ {
		if levels[index] != levels[index-1] {
			count++
		}
	}
	return count
}

func countHigh(levels []bool) int {
	count := 0
	for _, high := range levels {
		if high {
			count++
		}
	}
	return count
}

func changedCenter(first []bool, second []bool) float64 {
	sum := 0.0
	count := 0
	for index := range min(len(first), len(second)) {
		if first[index] != second[index] {
			sum += float64(index)
			count++
		}
	}
	if count == 0 {
		return math.NaN()
	}
	return sum / float64(count)
}
