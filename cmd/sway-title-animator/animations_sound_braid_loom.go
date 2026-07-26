package main

import (
	"math"
	"time"
)

func braidSoundArt(width int, phase int) string {
	return braidSoundArtWithSnapshot(width, phase,
		currentSoundSnapshot())
}

func braidSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	chars := fitRunes(braidArt(width, phase), width)
	if !audio.Active {
		return string(chars)
	}
	mids := (audio.LowMid + audio.HighMid) / 2
	bassCrossingSpacing := max(11, 23-int(math.Round(audio.Bass*8)))
	for index := range chars {
		switch {
		case chars[index] == '╳' && audio.Bass > 0.42:
			chars[index] = '╬'
		case (chars[index] == '╱' || chars[index] == '╲') &&
			audio.Bass > 0.42 &&
			(index+phase/13)%bassCrossingSpacing == 0:
			chars[index] = '╬'
		case (chars[index] == '╱' || chars[index] == '╲') &&
			(index+phase/11)%max(7, 18-int(math.Round(mids*12))) == 0:
			chars[index] = '╳'
		}
	}
	addBraidSoundCrossing(chars, audio)
	addBraidSoundHighlight(chars, phase, audio.Treble, audio.Balance)
	return string(chars)
}

func addBraidSoundCrossing(chars []rune, audio audioSnapshot) {
	onset, ok := newestSoundOnset(audio, audioRegionGeneral)
	if !ok || onset.Strength < 0.58 || onset.Age < 0 {
		return
	}
	const lifetime = 1200 * time.Millisecond
	if onset.Age >= lifetime || len(chars) == 0 {
		return
	}
	stereo := math.Max(-1, math.Min(1, onset.Position))
	center := int(math.Round((0.5 + stereo*0.40) * float64(len(chars)-1)))
	radius := 1 + int(math.Round(onset.Strength*2))
	for offset := -radius; offset <= radius; offset++ {
		index := center + offset
		if index < 0 || index >= len(chars) {
			continue
		}
		switch {
		case offset == 0:
			chars[index] = '╳'
		case offset < 0:
			chars[index] = '╱'
		default:
			chars[index] = '╲'
		}
	}
}

func addBraidSoundHighlight(chars []rune, phase int, treble float64, balance float64) {
	if treble < 0.34 || len(chars) == 0 {
		return
	}
	direction := 1
	if balance < 0 {
		direction = -1
	}
	position := int(math.Mod(
		float64(phase)*0.16+float64(len(chars))*100,
		float64(len(chars)),
	))
	if direction < 0 {
		position = len(chars) - 1 - position
	}
	chars[position] = '✦'
}

func loomSoundArt(width int, phase int) string {
	return loomSoundArtWithSnapshot(width, phase,
		currentSoundSnapshot())
}

func loomSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	bass, mids, treble := audio.Bass, (audio.LowMid+audio.HighMid)/2, audio.Treble
	if !audio.Active {
		bass, mids, treble = 0.16, 0.18, 0
	}
	warpSpacing := max(5, 13-int(math.Round(bass*5)))
	weftFrequency := 0.13 + mids*0.24
	clock := float64(phase)*0.014 +
		signedOrganicNoise("loom_sound", 1, float64(phase)/206)*0.65
	chars := make([]rune, width)
	for index := range width {
		weft := 0.5 + 0.5*math.Sin(float64(index)*weftFrequency+clock)
		switch {
		case index%warpSpacing == 0 && bass > 0.55:
			chars[index] = '▓'
		case index%warpSpacing == 0:
			chars[index] = '▒'
		case weft > 0.78:
			chars[index] = '≈'
		case weft > 0.52:
			chars[index] = '≋'
		case weft > 0.28:
			chars[index] = '⌁'
		default:
			chars[index] = '░'
		}
	}
	addLoomSoundShuttle(chars, audio)
	addLoomSoundGlints(chars, phase, treble)
	return string(chars)
}

func addLoomSoundShuttle(chars []rune, audio audioSnapshot) {
	onset, ok := newestSoundOnset(audio, audioRegionGeneral)
	if !ok || len(chars) == 0 {
		return
	}
	center, radius, live := loomSoundShuttle(len(chars), onset, audio)
	if !live {
		return
	}
	for offset := -radius; offset <= radius; offset++ {
		index := center + offset
		if index < 0 || index >= len(chars) {
			continue
		}
		switch {
		case offset == 0:
			chars[index] = '✦'
		case abs(offset)%2 == 0:
			chars[index] = '≋'
		default:
			chars[index] = '⌁'
		}
	}
}

func loomSoundShuttle(width int, onset audioOnset, audio audioSnapshot) (int, int, bool) {
	const lifetime = 1600 * time.Millisecond
	if width <= 0 || onset.Region != audioRegionGeneral ||
		onset.Age < 0 || onset.Age >= lifetime {
		return 0, 0, false
	}
	progress := smoothstep(float64(onset.Age) / float64(lifetime))
	center := int(math.Round(progress * float64(width-1)))
	if onset.Position < 0 {
		center = width - 1 - center
	}
	mids := (audio.LowMid + audio.HighMid) / 2
	radius := 2 + int(math.Round(mids*math.Min(7, float64(width)*0.08)))
	return center, radius, true
}

func addLoomSoundGlints(chars []rune, phase int, treble float64) {
	if treble < 0.52 {
		return
	}
	spacing := max(11, 22-int(math.Round(treble*7)))
	for index := range chars {
		if (index+phase/6)%spacing == 0 && chars[index] != '✦' {
			chars[index] = '·'
		}
	}
}
