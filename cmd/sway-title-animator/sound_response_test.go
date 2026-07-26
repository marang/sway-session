package main

import (
	"math"
	"testing"
	"time"
)

func TestCalmSoundSnapshotBroadensBandsWithoutDestroyingResponse(t *testing.T) {
	snapshot := audioSnapshot{
		Active:       true,
		Level:        0.8,
		Bass:         0.8,
		LowMid:       0.8,
		HighMid:      0.8,
		Treble:       0.8,
		Balance:      1,
		SpectralFlux: 0.8,
		Peak:         0.8,
	}
	for index := range snapshot.Bands {
		if index%2 == 0 {
			snapshot.Bands[index] = 1
		}
	}

	calm := calmSoundSnapshot(snapshot)
	if calm.Level <= snapshot.Level ||
		calm.Bass <= snapshot.Bass ||
		calm.SpectralFlux <= snapshot.SpectralFlux ||
		calm.Balance >= snapshot.Balance {
		t.Fatalf("expected lifted visual energy with bounded balance, got %+v", calm)
	}

	rawVariation := maximumAdjacentBandDifference(snapshot.Bands)
	calmVariation := maximumAdjacentBandDifference(calm.Bands)
	if calmVariation >= rawVariation*0.60 {
		t.Fatalf("expected broad frequency regions, raw variation %.3f calm variation %.3f",
			rawVariation, calmVariation)
	}
}

func TestCalmSoundSnapshotKeepsOnlyNewestOnsetPerRegion(t *testing.T) {
	snapshot := audioSnapshot{Active: true, OnsetCount: 5}
	snapshot.Onsets[0] = audioOnset{
		ID: 1, Age: 500 * time.Millisecond, Strength: 0.8, Region: audioRegionGeneral,
	}
	snapshot.Onsets[1] = audioOnset{
		ID: 2, Age: 400 * time.Millisecond, Strength: 0.7, Region: audioRegionBass,
	}
	snapshot.Onsets[2] = audioOnset{
		ID: 3, Age: 300 * time.Millisecond, Strength: 0.9, Region: audioRegionHigh,
	}
	snapshot.Onsets[3] = audioOnset{
		ID: 4, Age: 200 * time.Millisecond, Strength: 1, Region: audioRegionGeneral,
	}
	snapshot.Onsets[4] = audioOnset{
		ID: 5, Age: 100 * time.Millisecond, Strength: 0.2, Region: audioRegionBass,
	}

	calm := calmSoundSnapshot(snapshot)
	if calm.OnsetCount != 3 {
		t.Fatalf("expected one meaningful onset per region, got %+v", calm.Onsets)
	}
	for index, id := range []uint64{2, 3, 4} {
		if calm.Onsets[index].ID != id {
			t.Fatalf("unexpected onset selection: %+v", calm.Onsets)
		}
		if calm.Onsets[index].Strength > 0.95 {
			t.Fatalf("visual onset strength must remain bounded: %+v", calm.Onsets[index])
		}
	}
}

func TestCurrentSoundSnapshotAppliesMotionBeforeCalming(t *testing.T) {
	originalSnapshot := currentAudioSnapshot
	originalMotion := audioSettings.Motion
	t.Cleanup(func() {
		currentAudioSnapshot = originalSnapshot
		audioSettings.Motion = originalMotion
	})

	snapshot := audioSnapshot{Active: true, Level: 0.4, Bass: 0.6}
	snapshot.Bands[7] = 0.8
	currentAudioSnapshot = func() audioSnapshot {
		return snapshot
	}
	audioSettings.Motion = 0.5

	want := calmSoundSnapshot(scaleAudioSnapshot(snapshot, 0.5))
	if got := currentSoundSnapshot(); got != want {
		t.Fatalf("unexpected runtime sound snapshot:\n got: %+v\nwant: %+v", got, want)
	}
}

func maximumAdjacentBandDifference(bands [audioBandCount]float64) float64 {
	maximum := 0.0
	for index := 1; index < len(bands); index++ {
		maximum = math.Max(maximum, math.Abs(bands[index]-bands[index-1]))
	}
	return maximum
}
