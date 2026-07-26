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

func TestRadarSoundBassControlsSweepSpeedAndTargetWeight(t *testing.T) {
	quietBass := audioSnapshot{Active: true, Bass: 0.05, LowMid: 0.3, HighMid: 0.3}
	loudBass := quietBass
	loudBass.Bass = 0.95

	slowStart := radarSoundSweepPosition(100, 20, 0.16+quietBass.Bass*1.75)
	slowEnd := radarSoundSweepPosition(100, 30, 0.16+quietBass.Bass*1.75)
	fastStart := radarSoundSweepPosition(100, 20, 0.16+loudBass.Bass*1.75)
	fastEnd := radarSoundSweepPosition(100, 30, 0.16+loudBass.Bass*1.75)
	if fastEnd-fastStart <= slowEnd-slowStart {
		t.Fatalf("bass should accelerate sweep: slow %.2f fast %.2f",
			slowEnd-slowStart, fastEnd-fastStart)
	}

	quietFrame := radarSoundArtWithSnapshot(101, 20, quietBass)
	loudFrame := radarSoundArtWithSnapshot(101, 20, loudBass)
	if strings.Count(loudFrame, "◆")+strings.Count(loudFrame, "●") <=
		strings.Count(quietFrame, "◆")+strings.Count(quietFrame, "●") {
		t.Fatalf("bass should strengthen the central target: quiet=%q loud=%q", quietFrame, loudFrame)
	}
}

func TestRadarSoundEchoUsesRegionStereoMidsAndLifetime(t *testing.T) {
	leftBass := audioOnset{
		Age:      160 * time.Millisecond,
		Strength: 0.9,
		Region:   audioRegionBass,
		Position: -0.8,
	}
	rightHigh := leftBass
	rightHigh.Region = audioRegionHigh
	rightHigh.Position = 0.8
	bassPosition, narrowWidth, _, bassLive := radarSoundEcho(100, leftBass, 0.1)
	highPosition, wideWidth, _, highLive := radarSoundEcho(100, rightHigh, 0.9)
	if !bassLive || !highLive {
		t.Fatal("expected fresh radar echoes to be live")
	}
	if bassPosition >= 50 || highPosition <= 50 || highPosition-50 <= 50-bassPosition {
		t.Fatalf("region and stereo placement should separate echoes: bass=%.2f high=%.2f",
			bassPosition, highPosition)
	}
	if wideWidth <= narrowWidth {
		t.Fatalf("mids should widen detected targets: narrow=%.2f wide=%.2f",
			narrowWidth, wideWidth)
	}
	leftBass.Age = 2 * time.Second
	if _, _, _, live := radarSoundEcho(100, leftBass, 0.5); live {
		t.Fatal("expired radar echo should disappear")
	}
}

func TestShutterSoundBassOnsetAndLowMidsCloseAperture(t *testing.T) {
	open := audioSnapshot{Active: true, LowMid: 0.05}
	sustained := audioSnapshot{Active: true, LowMid: 0.95}
	_, openRadius, openClosure := shutterSoundGeometry(100, 20, open)
	_, sustainedRadius, sustainedClosure := shutterSoundGeometry(100, 20, sustained)
	if sustainedRadius >= openRadius || sustainedClosure <= openClosure {
		t.Fatalf("low mids should close aperture: open radius=%.2f closure=%.2f sustained radius=%.2f closure=%.2f",
			openRadius, openClosure, sustainedRadius, sustainedClosure)
	}

	onset := open
	onset.OnsetCount = 1
	onset.Onsets[0] = audioOnset{
		ID:       1,
		Age:      40 * time.Millisecond,
		Strength: 1,
		Region:   audioRegionBass,
	}
	_, struckRadius, struckClosure := shutterSoundGeometry(100, 20, onset)
	onset.Onsets[0].Age = 800 * time.Millisecond
	_, releasedRadius, releasedClosure := shutterSoundGeometry(100, 20, onset)
	if struckRadius >= releasedRadius || struckClosure <= releasedClosure {
		t.Fatalf("bass onset should close then release: struck radius=%.2f closure=%.2f released radius=%.2f closure=%.2f",
			struckRadius, struckClosure, releasedRadius, releasedClosure)
	}
}

func TestShutterSoundBoundsAsymmetryAndPeakVocabulary(t *testing.T) {
	left := audioSnapshot{Active: true, LowMid: 0.4, Balance: -1, Peak: 0.3}
	right := left
	right.Balance = 1
	leftCenter, _, _ := shutterSoundGeometry(100, 9, left)
	rightCenter, _, _ := shutterSoundGeometry(100, 9, right)
	if leftCenter >= 50 || rightCenter <= 50 {
		t.Fatalf("balance should shift closure center: left=%.2f right=%.2f", leftCenter, rightCenter)
	}
	if rightCenter-leftCenter > 16 {
		t.Fatalf("stereo asymmetry exceeded bound: left=%.2f right=%.2f", leftCenter, rightCenter)
	}

	light := shutterSoundArtWithSnapshot(80, 9, left)
	heavy := left
	heavy.Peak = 0.95
	heavyFrame := shutterSoundArtWithSnapshot(80, 9, heavy)
	if strings.ContainsAny(light, "▶◀┃") {
		t.Fatalf("low peak should use light mechanics: %q", light)
	}
	if !strings.ContainsAny(heavyFrame, "▶◀") || !strings.ContainsRune(heavyFrame, '┃') {
		t.Fatalf("high peak should strengthen arrows and seam: %q", heavyFrame)
	}
	for _, frame := range []string{light, heavyFrame} {
		if strings.ContainsAny(frame, "╳╪┄╍") {
			t.Fatalf("shutter_sound must not use glitch fragments: %q", frame)
		}
	}
}

func TestRadarAndShutterSoundSilentFormsMoveAndStayBounded(t *testing.T) {
	for name, animation := range map[string]func(int, int, audioSnapshot) string{
		"radar_sound":   radarSoundArtWithSnapshot,
		"shutter_sound": shutterSoundArtWithSnapshot,
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
				t.Fatalf("silent form should retain slow motion, got %v", frames)
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
