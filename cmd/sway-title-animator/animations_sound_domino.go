package main

import (
	"math"
	"time"
)

const dominoSoundCascadeLifetime = 1050 * time.Millisecond

func dominoSoundArt(width int, phase int) string {
	return dominoSoundArtWithSnapshot(
		width,
		phase,
		currentSoundSnapshot(),
	)
}

func dominoSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	base := []rune(dominoArt(width, phase))
	if !audio.Active || width < 8 {
		return string(base)
	}

	positions := dominoPositions(width)
	firstOnset := max(0, min(audio.OnsetCount, len(audio.Onsets))-3)
	for onsetIndex := firstOnset; onsetIndex < min(audio.OnsetCount, len(audio.Onsets)); onsetIndex++ {
		applyDominoSoundCascade(base, positions, audio.Onsets[onsetIndex], audio)
	}
	return string(base)
}

func applyDominoSoundCascade(
	chars []rune,
	positions []int,
	onset audioOnset,
	audio audioSnapshot,
) {
	if len(chars) == 0 || len(positions) == 0 ||
		onset.Age < 0 || onset.Age >= dominoSoundCascadeLifetime ||
		onset.Strength <= 0 {
		return
	}
	progress := float64(onset.Age) / float64(dominoSoundCascadeLifetime)
	envelope := onset.Strength * (1 - smoothstep(progress))
	if envelope < 0.035 {
		return
	}

	centerPosition := 0.5 + math.Max(-1, math.Min(1, onset.Position))*0.34
	if math.Abs(onset.Position) < 0.04 {
		sequence := int64(onset.ID)
		if sequence == 0 {
			sequence = int64(max(uint64(1), onset.Sequence))
		}
		centerPosition = 0.24 + eventRandom("domino_sound", 1, sequence, 1)*0.52
	}
	switch onset.Region {
	case audioRegionBass:
		centerPosition = centerPosition*0.70 + 0.15
	case audioRegionHigh:
		centerPosition = centerPosition*0.82 + 0.09
	}
	centerPosition = math.Max(0.08, math.Min(0.92, centerPosition))
	centerStone := nearestDominoPosition(
		positions,
		centerPosition*float64(len(chars)-1),
	)
	if centerStone < 0 {
		return
	}

	reach := 2.5 + onset.Strength*3.5 +
		audio.Bass*float64(len(positions))*0.42 +
		audio.LowMid*float64(len(positions))*0.14
	if onset.Region == audioRegionHigh {
		reach *= 0.62
	}
	front := smoothstep(math.Min(1, progress*(0.72+audio.LowMid*0.48))) * reach
	for stone, column := range positions {
		distance := math.Abs(float64(stone - centerStone))
		switch {
		case distance < front-0.85:
			chars[column] = '━'
		case math.Abs(distance-front) <= 0.85:
			if stone < centerStone {
				chars[column] = '╱'
			} else {
				chars[column] = '╲'
			}
			addDominoCollisionSpark(chars, column, stone < centerStone, audio.Treble, envelope)
		}
	}

	if progress < 0.17 {
		centerColumn := positions[centerStone]
		if envelope > 0.62 {
			chars[centerColumn] = '▣'
		} else {
			chars[centerColumn] = '▯'
		}
	}
}

func addDominoCollisionSpark(
	chars []rune,
	column int,
	left bool,
	treble float64,
	envelope float64,
) {
	if treble < 0.30 || envelope < 0.18 {
		return
	}
	spark := column + 1
	if left {
		spark = column - 1
	}
	if spark < 0 || spark >= len(chars) {
		return
	}
	if treble*envelope > 0.48 {
		chars[spark] = '✦'
	} else {
		chars[spark] = '•'
	}
}
