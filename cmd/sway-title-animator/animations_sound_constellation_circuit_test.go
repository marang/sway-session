package main

import (
	"strings"
	"testing"
	"time"
)

func TestConstellationSoundMapsBandsToFixedStarRegions(t *testing.T) {
	audio := audioSnapshot{Active: true}
	for index := range audio.Bands {
		if index < audioBandCount/2 {
			audio.Bands[index] = 1
		}
	}
	first := constellationSoundArtWithSnapshot(120, 20, audio)
	audio.Bands = [audioBandCount]float64{}
	for index := audioBandCount / 2; index < audioBandCount; index++ {
		audio.Bands[index] = 1
	}
	second := constellationSoundArtWithSnapshot(120, 20, audio)
	if brightStarCenter(first) >= brightStarCenter(second) {
		t.Fatalf("frequency regions should remain ordered: low=%q high=%q", first, second)
	}
	if strings.Count(first, "✦") == 0 || strings.Count(second, "✦") == 0 {
		t.Fatal("energized regions should contain bright stars")
	}
}

func TestConstellationSoundSupernovaAndFluxFollowStereo(t *testing.T) {
	audio := audioSnapshot{
		Active: true, SpectralFlux: 0.9, Balance: 1, OnsetCount: 1,
	}
	audio.Onsets[0] = audioOnset{
		ID: 1, Age: 120 * time.Millisecond, Strength: 1,
		Region: audioRegionGeneral, Position: 0.9,
	}
	right := constellationSoundArtWithSnapshot(100, 20, audio)
	rightSupernova := make([]rune, 100)
	addConstellationSupernova(rightSupernova, audio)
	audio.Onsets[0].Position = -0.9
	audio.Balance = -1
	left := constellationSoundArtWithSnapshot(100, 20, audio)
	leftSupernova := make([]rune, 100)
	addConstellationSupernova(leftSupernova, audio)
	if lastIndexRune(string(rightSupernova), '✦') <= 50 ||
		lastIndexRune(string(leftSupernova), '✦') >= 50 {
		t.Fatalf("supernova should follow stereo: left=%q right=%q", left, right)
	}
	if !strings.ContainsAny(right, "─╴") || !strings.ContainsAny(left, "─╴") {
		t.Fatal("spectral flux should create a bounded shooting star")
	}
}

func TestCircuitSoundBandsCurrentMidsAndTreble(t *testing.T) {
	audio := audioSnapshot{
		Active: true, LowMid: 0.1, HighMid: 0.1, Treble: 1, OnsetCount: 1,
	}
	for index := range audio.Bands {
		audio.Bands[index] = 0.8
	}
	audio.Onsets[0] = audioOnset{
		ID: 3, Age: 300 * time.Millisecond, Strength: 1,
		Region: audioRegionBass, Position: 0.8,
	}
	narrowCenter, narrowRadius, _, live := circuitSoundCurrent(100, audio.Onsets[0], audio)
	if !live || narrowCenter >= 50 {
		t.Fatalf("positive stereo should launch current from left: center=%d", narrowCenter)
	}
	audio.LowMid = 1
	audio.HighMid = 1
	_, wideRadius, _, _ := circuitSoundCurrent(100, audio.Onsets[0], audio)
	if wideRadius <= narrowRadius {
		t.Fatalf("mids should lengthen route: narrow=%d wide=%d", narrowRadius, wideRadius)
	}
	frame := circuitSoundArtWithSnapshot(100, 20, audio)
	if !strings.ContainsAny(frame, "═●") || !strings.ContainsRune(frame, '✦') {
		t.Fatalf("active circuit should show current and junction sparks: %q", frame)
	}
}

func TestCircuitSoundStereoDirectionAndSilentDiagnostic(t *testing.T) {
	onset := audioOnset{
		ID: 1, Age: 240 * time.Millisecond, Strength: 1,
		Region: audioRegionBass, Position: 1,
	}
	right, _, _, _ := circuitSoundCurrent(100, onset, audioSnapshot{})
	onset.Position = -1
	left, _, _, _ := circuitSoundCurrent(100, onset, audioSnapshot{})
	if right >= 50 || left <= 50 {
		t.Fatalf("current launch direction should follow stereo: rightward=%d leftward=%d",
			right, left)
	}
	first := circuitSoundArtWithSnapshot(80, 0, audioSnapshot{})
	second := circuitSoundArtWithSnapshot(80, 220, audioSnapshot{})
	if first == second || first != circuitArt(80, 0) || second != circuitArt(80, 220) {
		t.Fatalf("silence should preserve the complete moving circuit: first=%q second=%q",
			first, second)
	}
}

func TestConstellationAndCircuitSoundStayBoundedAndDeterministic(t *testing.T) {
	audio := audioSnapshot{
		Active: true, Bass: 0.8, LowMid: 0.6, HighMid: 0.5,
		Treble: 0.7, SpectralFlux: 0.6, OnsetCount: 2,
	}
	for index := range audio.Bands {
		audio.Bands[index] = float64(index%7) / 6
	}
	audio.Onsets[0] = audioOnset{
		ID: 1, Age: 220 * time.Millisecond, Strength: 1,
		Region: audioRegionBass, Position: -0.7,
	}
	audio.Onsets[1] = audioOnset{
		ID: 2, Age: 180 * time.Millisecond, Strength: 0.9,
		Region: audioRegionGeneral, Position: 0.7,
	}
	for name, animation := range map[string]func(int, int, audioSnapshot) string{
		"constellation_sound": constellationSoundArtWithSnapshot,
		"circuit_sound":       circuitSoundArtWithSnapshot,
	} {
		t.Run(name, func(t *testing.T) {
			for _, width := range []int{0, 1, 7, 8, 80, 220} {
				frame := animation(width, 31, audio)
				if len([]rune(frame)) != artWidth(width) {
					t.Fatalf("width=%d rendered %d runes", width, len([]rune(frame)))
				}
				if repeated := animation(width, 31, audio); repeated != frame {
					t.Fatalf("fixed input must be deterministic: %q != %q", frame, repeated)
				}
			}
		})
	}
}

func brightStarCenter(frame string) float64 {
	sum := 0
	count := 0
	for index, glyph := range []rune(frame) {
		if glyph == '✦' {
			sum += index
			count++
		}
	}
	if count == 0 {
		return -1
	}
	return float64(sum) / float64(count)
}

func lastIndexRune(frame string, target rune) int {
	last := -1
	for index, glyph := range []rune(frame) {
		if glyph == target {
			last = index
		}
	}
	return last
}
