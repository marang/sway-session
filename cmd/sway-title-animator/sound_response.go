package main

import (
	"cmp"
	"math"
	"slices"
)

const soundBandBlurRadius = 2

func currentSoundSnapshot() audioSnapshot {
	return calmSoundSnapshot(
		scaleAudioSnapshot(currentAudioSnapshot(), audioSettings.Motion),
	)
}

func calmSoundSnapshot(snapshot audioSnapshot) audioSnapshot {
	rawBands := snapshot.Bands
	for index := range snapshot.Bands {
		total := 0.0
		weight := 0.0
		for offset := -soundBandBlurRadius; offset <= soundBandBlurRadius; offset++ {
			source := max(0, min(audioBandCount-1, index+offset))
			localWeight := float64(soundBandBlurRadius + 1 - abs(offset))
			total += rawBands[source] * localWeight
			weight += localWeight
		}
		snapshot.Bands[index] = calmSoundEnergy(total / weight)
	}

	snapshot.Level = calmSoundEnergy(snapshot.Level)
	snapshot.Bass = calmSoundEnergy(snapshot.Bass)
	snapshot.LowMid = calmSoundEnergy(snapshot.LowMid)
	snapshot.HighMid = calmSoundEnergy(snapshot.HighMid)
	snapshot.Treble = calmSoundEnergy(snapshot.Treble)
	snapshot.LeftLevel = calmSoundEnergy(snapshot.LeftLevel)
	snapshot.RightLevel = calmSoundEnergy(snapshot.RightLevel)
	snapshot.SpectralFlux = calmSoundTransient(snapshot.SpectralFlux) * 0.68
	snapshot.Peak = calmSoundTransient(snapshot.Peak)
	snapshot.Balance = math.Max(-1, math.Min(1, snapshot.Balance)) * 0.55
	if snapshot.Active {
		snapshot.Centroid = 0.5 +
			(math.Max(0, math.Min(1, snapshot.Centroid))-0.5)*0.68
	}

	snapshot.Onsets, snapshot.OnsetCount = calmSoundOnsets(snapshot)
	return snapshot
}

func calmSoundEnergy(value float64) float64 {
	value = (math.Max(0, math.Min(1, value)) - 0.04) / 0.96
	return smoothstep(value) * 0.90
}

func calmSoundTransient(value float64) float64 {
	value = (math.Max(0, math.Min(1, value)) - 0.12) / 0.88
	return smoothstep(value)
}

func calmSoundOnsets(snapshot audioSnapshot) ([audioEventCapacity]audioOnset, int) {
	var newest [3]audioOnset
	var found [3]bool
	for index := 0; index < min(snapshot.OnsetCount, len(snapshot.Onsets)); index++ {
		onset := snapshot.Onsets[index]
		region := int(onset.Region)
		if region < 0 || region >= len(newest) || onset.Strength < 0.38 {
			continue
		}
		if !found[region] || onset.ID > newest[region].ID {
			onset.Strength = math.Min(
				0.90,
				0.34+calmSoundTransient(onset.Strength)*0.56,
			)
			newest[region] = onset
			found[region] = true
		}
	}

	selected := make([]audioOnset, 0, len(newest))
	for region, onset := range newest {
		if found[region] {
			selected = append(selected, onset)
		}
	}
	slices.SortFunc(selected, func(first audioOnset, second audioOnset) int {
		return cmp.Compare(first.ID, second.ID)
	})

	var onsets [audioEventCapacity]audioOnset
	copy(onsets[:], selected)
	return onsets, len(selected)
}
