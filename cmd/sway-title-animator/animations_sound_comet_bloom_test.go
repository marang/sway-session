package main

import (
	"strings"
	"testing"
	"time"
)

func TestCometSoundLaunchesOnlyFromBassOnsets(t *testing.T) {
	audio := audioSnapshot{Active: true, Level: 0.8, Centroid: 0.5, OnsetCount: 1}
	audio.Onsets[0] = audioOnset{
		ID: 3, Age: 240 * time.Millisecond, Strength: 1,
		Region: audioRegionGeneral, Position: 1,
	}
	withoutBass := cometSoundArtWithSnapshot(80, 20, audio)
	if strings.ContainsRune(withoutBass, '☄') {
		t.Fatalf("general onset must not launch a comet: %q", withoutBass)
	}
	audio.Onsets[0].Region = audioRegionBass
	withBass := cometSoundArtWithSnapshot(80, 20, audio)
	if !strings.ContainsRune(withBass, '☄') || !strings.ContainsAny(withBass, "░▒▓") {
		t.Fatalf("bass onset should launch a comet with a tail: %q", withBass)
	}
}

func TestCometSoundCentroidVelocityStereoAndTailFeatures(t *testing.T) {
	onset := audioOnset{
		ID: 5, Age: 350 * time.Millisecond, Strength: 0.9,
		Region: audioRegionBass, Position: 0.8,
	}
	darkHead, darkDirection, _, darkLive := cometSoundFlight(100, onset, 0.05)
	brightHead, brightDirection, _, brightLive := cometSoundFlight(100, onset, 0.95)
	if !darkLive || !brightLive || darkDirection != 1 || brightDirection != 1 {
		t.Fatal("expected live right-moving flights")
	}
	if brightHead <= darkHead {
		t.Fatalf("bright centroid should move faster: dark=%.2f bright=%.2f", darkHead, brightHead)
	}
	onset.Position = -0.8
	leftHead, leftDirection, _, leftLive := cometSoundFlight(100, onset, 0.95)
	if !leftLive || leftDirection != -1 || leftHead <= 50 {
		t.Fatalf("negative balance should launch from the right: head=%.2f direction=%d",
			leftHead, leftDirection)
	}

	audio := audioSnapshot{
		Active: true, Level: 1, Centroid: 0.5, Treble: 1, OnsetCount: 1,
	}
	audio.Onsets[0] = onset
	frame := cometSoundArtWithSnapshot(120, 13, audio)
	if !strings.ContainsRune(frame, '✦') {
		t.Fatalf("treble should add bounded tail sparkles: %q", frame)
	}
}

func TestCometSoundSilenceUsesCompleteBaseAnimation(t *testing.T) {
	frames := map[string]bool{}
	for _, phase := range []int{0, 47, 113, 229} {
		frame := cometSoundArtWithSnapshot(80, phase, audioSnapshot{})
		if want := cometArt(80, phase); frame != want {
			t.Fatalf("silence should render the complete base comet:\nwant: %q\ngot:  %q",
				want, frame)
		}
		frames[frame] = true
	}
	if len(frames) < 2 {
		t.Fatalf("base comet should keep moving, got %v", frames)
	}
}

func TestBloomSoundMidsBassBalanceAndPollenPreserveForm(t *testing.T) {
	base := audioSnapshot{
		Active: true, Level: 0.8, Bass: 0.2, LowMid: 0.1, HighMid: 0.1,
		Treble: 1, Balance: -1, OnsetCount: 1,
	}
	base.Onsets[0] = audioOnset{
		ID: 9, Age: 520 * time.Millisecond, Strength: 1,
		Region: audioRegionGeneral,
	}
	narrow := bloomSoundArtWithSnapshot(100, 20, base)
	wideAudio := base
	wideAudio.LowMid = 1
	wideAudio.HighMid = 1
	wideAudio.Bass = 1
	wide := bloomSoundArtWithSnapshot(100, 20, wideAudio)
	if strings.Count(wide, "⌁") <= strings.Count(narrow, "⌁") {
		t.Fatalf("mids should emphasize existing petal tips: narrow=%q wide=%q", narrow, wide)
	}
	if !strings.ContainsRune(wide, '━') {
		t.Fatalf("bass should strengthen the stem: %q", wide)
	}
	if !strings.ContainsRune(wide, '·') {
		t.Fatalf("treble should add post-open pollen: %q", wide)
	}

	if silent := bloomSoundArtWithSnapshot(100, 20, audioSnapshot{}); silent != bloomArt(100, 20) {
		t.Fatalf("silence should preserve the complete base bloom: %q", silent)
	}
}

func TestCometAndBloomSoundStayBoundedAndDeterministic(t *testing.T) {
	active := audioSnapshot{
		Active: true, Level: 0.7, Bass: 0.8, LowMid: 0.6, HighMid: 0.5,
		Treble: 0.7, Centroid: 0.6, OnsetCount: 2,
	}
	active.Onsets[0] = audioOnset{
		ID: 1, Age: 220 * time.Millisecond, Strength: 1,
		Region: audioRegionBass, Position: -0.7,
	}
	active.Onsets[1] = audioOnset{
		ID: 2, Age: 420 * time.Millisecond, Strength: 0.9,
		Region: audioRegionGeneral, Position: 0.7,
	}
	for name, animation := range map[string]func(int, int, audioSnapshot) string{
		"comet_sound": cometSoundArtWithSnapshot,
		"bloom_sound": bloomSoundArtWithSnapshot,
	} {
		t.Run(name, func(t *testing.T) {
			for _, width := range []int{0, 1, 7, 8, 80, 220} {
				frame := animation(width, 31, active)
				if len([]rune(frame)) != artWidth(width) {
					t.Fatalf("width=%d rendered %d runes", width, len([]rune(frame)))
				}
				if repeated := animation(width, 31, active); repeated != frame {
					t.Fatalf("fixed input must be deterministic: %q != %q", frame, repeated)
				}
			}
		})
	}
}
