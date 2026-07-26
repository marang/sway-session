package main

import (
	"strings"
	"testing"
	"time"
)

func TestSmileysSoundAudioControlsFaceTravelAndReaction(t *testing.T) {
	quietFace := string(smileysSoundFace(0.1, 0.1, false))
	loudFace := string(smileysSoundFace(1, 1, false))
	if quietFace == loudFace || !strings.ContainsAny(loudFace, "●▽") {
		t.Fatalf("bass and mids should change local face weight/expression: %q %q", quietFace, loudFace)
	}
	audio := audioSnapshot{Active: true, Level: 1, Bass: 1, LowMid: 1, Treble: 1}
	active := smileysSoundArtWithSnapshot(120, 40, audio)
	if strings.Count(active, "ʕ") < 2 || !strings.ContainsRune(active, '✦') {
		t.Fatalf("level and treble should add spaced faces and accents: %q", active)
	}
	audio.OnsetCount = 1
	audio.Onsets[0] = audioOnset{
		ID: 1, Age: 100 * time.Millisecond, Strength: 1, Region: audioRegionGeneral,
	}
	if reaction := smileysSoundArtWithSnapshot(120, 40, audio); !strings.Contains(reaction, ">_<") {
		t.Fatalf("strong onset should synchronize one reaction: %q", reaction)
	}
}

func TestSmileysSoundStereoBiasAndSilentSingleFace(t *testing.T) {
	right := audioSnapshot{Active: true, Level: 0.2, Balance: 1}
	left := right
	left.Balance = -1
	rightFrame := smileysSoundArtWithSnapshot(100, 40, right)
	leftFrame := smileysSoundArtWithSnapshot(100, 40, left)
	rightPosition := firstIndexRune(rightFrame, 'ʕ')
	leftPosition := firstIndexRune(leftFrame, 'ʕ')
	if rightPosition == leftPosition || rightPosition+leftPosition != 95 {
		t.Fatalf("entry side should follow stereo: right=%q left=%q", rightFrame, leftFrame)
	}
	silent := smileysSoundArtWithSnapshot(100, 80, audioSnapshot{})
	if strings.Count(silent, "ʕ") != 1 {
		t.Fatalf("silence should keep one relaxed face: %q", silent)
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
	if strings.Count(tear, "═") < 80 {
		t.Fatalf("very strong onset should create one bounded tear: %q", tear)
	}
}

func TestGlitchSoundSilenceIsAlmostCleanAndMoving(t *testing.T) {
	first := glitchSoundArtWithSnapshot(80, 0, audioSnapshot{})
	second := glitchSoundArtWithSnapshot(80, 220, audioSnapshot{})
	if first == second || countNotRune(first, '─') != 1 || countNotRune(second, '─') != 1 {
		t.Fatalf("silence should keep one moving defect: first=%q second=%q", first, second)
	}
	if strings.ContainsAny(first, "█▓▒") {
		t.Fatalf("silent glitch must remain distinct from ribbon: %q", first)
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

func firstIndexRune(frame string, target rune) int {
	for index, glyph := range []rune(frame) {
		if glyph == target {
			return index
		}
	}
	return -1
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
