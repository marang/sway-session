package main

import (
	"strings"
	"testing"
	"time"
)

func TestSmileysSoundAudioControlsFaceTravelAndReaction(t *testing.T) {
	audio := audioSnapshot{Active: true, Level: 1, Bass: 1, LowMid: 1, Treble: 1}
	active := smileysSoundArtWithSnapshot(120, 40, audio)
	if !strings.ContainsRune(active, '✦') {
		t.Fatalf("treble should accent the complete smiley parade: %q", active)
	}
	audio.OnsetCount = 1
	audio.Onsets[0] = audioOnset{
		ID: 1, Age: 100 * time.Millisecond, Strength: 1, Region: audioRegionGeneral,
	}
	if reaction := smileysSoundArtWithSnapshot(120, 40, audio); !strings.Contains(reaction, "(ﾉ◕ヮ◕)ﾉ✦") {
		t.Fatalf("strong onset should synchronize one reaction: %q", reaction)
	}
}

func TestSmileysSoundBalanceDoesNotJitterAndSilencePreservesBase(t *testing.T) {
	right := audioSnapshot{Active: true, Level: 0.2, Balance: 1}
	left := right
	left.Balance = -1
	rightFrame := smileysSoundArtWithSnapshot(100, 40, right)
	leftFrame := smileysSoundArtWithSnapshot(100, 40, left)
	if rightFrame != leftFrame {
		t.Fatalf("instantaneous balance must not mirror or jitter faces: right=%q left=%q",
			rightFrame, leftFrame)
	}
	silent := smileysSoundArtWithSnapshot(100, 80, audioSnapshot{})
	if want := fitRunes(smileysArt(100, 80), 100); silent != string(want) {
		t.Fatalf("silence should preserve the complete base parade: got=%q want=%q",
			silent, string(want))
	}
}

func TestSmileysSoundDotsPulseOnBeat(t *testing.T) {
	calm := audioSnapshot{Active: true, Level: 0.25}
	beat := calm
	beat.OnsetCount = 1
	beat.Onsets[0] = audioOnset{
		ID: 1, Age: 40 * time.Millisecond, Strength: 1, Region: audioRegionGeneral,
	}
	calmFrame := smileysSoundArtWithSnapshot(140, 40, calm)
	beatFrame := smileysSoundArtWithSnapshot(140, 40, beat)
	if strings.Count(beatFrame, "●") <= strings.Count(calmFrame, "●") {
		t.Fatalf("beat should pulse distributed points: calm=%q beat=%q",
			calmFrame, beatFrame)
	}
	if soundBeatPulse(beat) <= 0.8 {
		t.Fatal("fresh strong beat should produce a strong point pulse")
	}
}

func TestGlitchSoundFluxBassTrebleAndTear(t *testing.T) {
	stable := glitchSoundArtWithSnapshot(100, 20, audioSnapshot{Active: true})
	flux := glitchSoundArtWithSnapshot(100, 20, audioSnapshot{
		Active: true, SpectralFlux: 1, Treble: 1,
	})
	if countNotRune(flux, '─') <= countNotRune(stable, '─') {
		t.Fatalf("flux and treble should increase bounded noise: stable=%q flux=%q", stable, flux)
	}
	audio := audioSnapshot{Active: true, OnsetCount: 1}
	audio.Onsets[0] = audioOnset{
		ID: 2, Age: 180 * time.Millisecond, Strength: 1,
		Region: audioRegionBass, Position: 0.8,
	}
	block := glitchSoundArtWithSnapshot(100, 20, audio)
	if !strings.ContainsAny(block, "░▒▓╳") || lastIndexRune(block, '╳') <= 50 {
		t.Fatalf("bass transient should displace selected side: %q", block)
	}
	audio.Onsets[0].Region = audioRegionGeneral
	audio.Onsets[0].Age = 80 * time.Millisecond
	tear := glitchSoundArtWithSnapshot(100, 20, audio)
	if strings.Count(tear, "═") > 20 {
		t.Fatalf("general onset must not replace the complete base glitch with a tear: %q", tear)
	}
}

func TestGlitchSoundSilencePreservesCompleteBaseMotion(t *testing.T) {
	first := glitchSoundArtWithSnapshot(80, 0, audioSnapshot{})
	second := glitchSoundArtWithSnapshot(80, 220, audioSnapshot{})
	if first == second || first != glitchArt(80, 0) || second != glitchArt(80, 220) {
		t.Fatalf("silence should keep the complete moving glitch: first=%q second=%q",
			first, second)
	}
	if strings.ContainsRune(first, '█') {
		t.Fatalf("silent glitch must remain distinct from ribbon: %q", first)
	}
}

func TestGlitchSoundGeneralBeatPulseGrowsInPlace(t *testing.T) {
	onset := audioOnset{
		ID: 8, Age: 100 * time.Millisecond, Strength: 1,
		Region: audioRegionGeneral, Position: 0.6,
	}
	startCenter, startRadius, startIntensity, ok := glitchSoundBeatPulse(100, onset)
	if !ok {
		t.Fatal("expected fresh general beat pulse")
	}
	onset.Age = 540 * time.Millisecond
	endCenter, endRadius, endIntensity, ok := glitchSoundBeatPulse(100, onset)
	if !ok {
		t.Fatal("expected growing general beat pulse")
	}
	if startCenter != endCenter || endRadius <= startRadius {
		t.Fatalf("pulse must grow in place: center %d->%d radius %d->%d",
			startCenter, endCenter, startRadius, endRadius)
	}
	if startIntensity <= 0 || endIntensity <= 0 {
		t.Fatalf("live pulse intensity must remain visible: %.2f -> %.2f",
			startIntensity, endIntensity)
	}
}

func TestSmileysAndGlitchSoundStayBoundedAndDeterministic(t *testing.T) {
	for name, animation := range map[string]func(int, int, audioSnapshot) string{
		"smileys_sound": smileysSoundArtWithSnapshot,
		"glitch_sound":  glitchSoundArtWithSnapshot,
	} {
		t.Run(name, func(t *testing.T) {
			for _, width := range []int{0, 1, 7, 8, 80, 220} {
				frame := animation(width, 31, audioSnapshot{})
				if len([]rune(frame)) != artWidth(width) {
					t.Fatalf("width=%d rendered %d runes", width, len([]rune(frame)))
				}
				if repeated := animation(width, 31, audioSnapshot{}); repeated != frame {
					t.Fatalf("fixed input must be deterministic: %q != %q", frame, repeated)
				}
			}
		})
	}
}

func countNotRune(frame string, target rune) int {
	count := 0
	for _, glyph := range frame {
		if glyph != target {
			count++
		}
	}
	return count
}
