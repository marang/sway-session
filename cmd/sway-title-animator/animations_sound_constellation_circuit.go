package main

import (
	"math"
	"time"
)

func constellationSoundArt(width int, phase int) string {
	return constellationSoundArtWithSnapshot(
		width,
		phase,
		scaleAudioSnapshot(currentAudioSnapshot(), audioSettings.Motion),
	)
}

func constellationSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	chars := make([]rune, width)
	drift := int(math.Round(signedOrganicNoise(
		"constellation_sound", 1, float64(phase)/96,
	) * 2))
	for index := range width {
		source := index - drift
		density := animationRandom("constellation_sound", 10, int64(source))
		if density < 0.84 {
			chars[index] = ' '
			continue
		}
		energy := 0.12
		if audio.Active {
			position := float64(max(0, min(width-1, source))) /
				float64(max(1, width-1))
			energy = interpolatedAudioBand(
				audio.Bands,
				position*float64(audioBandCount-1),
			)
		}
		chars[index] = constellationSoundStar(energy, density)
	}

	addConstellationSupernova(chars, audio)
	addConstellationShootingStar(chars, phase, audio)
	return string(chars)
}

func constellationSoundStar(energy float64, density float64) rune {
	switch {
	case energy > 0.78:
		return '✦'
	case energy > 0.52:
		return '✧'
	case energy > 0.26:
		return '•'
	case density > 0.94:
		return '•'
	default:
		return '·'
	}
}

func addConstellationSupernova(chars []rune, audio audioSnapshot) {
	onset, ok := newestSoundOnset(audio, audioRegionGeneral)
	if !ok || onset.Strength < 0.62 || onset.Age < 0 {
		return
	}
	const lifetime = 720 * time.Millisecond
	if onset.Age >= lifetime || len(chars) == 0 {
		return
	}
	progress := float64(onset.Age) / float64(lifetime)
	stereo := math.Max(-1, math.Min(1, onset.Position))
	center := int(math.Round((0.5 + stereo*0.38) * float64(len(chars)-1)))
	radius := 1 + int(math.Round(onset.Strength*(1-progress)*3))
	for offset := -radius; offset <= radius; offset++ {
		index := center + offset
		if index < 0 || index >= len(chars) {
			continue
		}
		switch abs(offset) {
		case 0:
			chars[index] = '✦'
		case 1:
			chars[index] = '✧'
		default:
			chars[index] = '·'
		}
	}
}

func addConstellationShootingStar(chars []rune, phase int, audio audioSnapshot) {
	if !audio.Active || audio.SpectralFlux < 0.16 || len(chars) < 5 {
		return
	}
	direction := 1
	if audio.Balance < 0 {
		direction = -1
	}
	position := int(math.Mod(
		float64(phase)*(0.35+audio.SpectralFlux)+float64(len(chars))*100,
		float64(len(chars)),
	))
	for distance, glyph := range []rune{'✧', '─', '╴'} {
		index := position - direction*distance
		if index >= 0 && index < len(chars) {
			chars[index] = glyph
		}
	}
}

func circuitSoundArt(width int, phase int) string {
	return circuitSoundArtWithSnapshot(
		width,
		phase,
		scaleAudioSnapshot(currentAudioSnapshot(), audioSettings.Motion),
	)
}

func circuitSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	chars := make([]rune, width)
	for index := range width {
		switch {
		case index%17 == 8:
			chars[index] = '╪'
		case index%17 == 7:
			chars[index] = '╾'
		case index%17 == 9:
			chars[index] = '╼'
		default:
			chars[index] = '─'
		}
	}

	if audio.Active {
		for index := range width {
			position := float64(index) / float64(max(1, width-1))
			energy := interpolatedAudioBand(
				audio.Bands,
				position*float64(audioBandCount-1),
			)
			switch {
			case energy > 0.78 && index%17 == 8:
				chars[index] = '●'
			case energy > 0.58:
				chars[index] = '═'
			case energy > 0.32 && chars[index] == '─':
				chars[index] = '╍'
			}
		}
		addCircuitSoundCurrent(chars, audio)
		addCircuitSoundSparks(chars, phase, audio)
	} else {
		addCircuitDiagnostic(chars, phase)
	}
	return string(chars)
}

func addCircuitSoundCurrent(chars []rune, audio audioSnapshot) {
	onset, ok := newestSoundOnset(audio, audioRegionBass)
	if !ok || len(chars) == 0 {
		return
	}
	center, radius, strength, live := circuitSoundCurrent(len(chars), onset, audio)
	if !live {
		return
	}
	for offset := -radius; offset <= radius; offset++ {
		index := center + offset
		if index < 0 || index >= len(chars) {
			continue
		}
		switch {
		case offset == 0 && strength > 0.62:
			chars[index] = '●'
		case math.Abs(float64(offset)) <= float64(radius)*0.55:
			chars[index] = '═'
		default:
			chars[index] = '╍'
		}
	}
}

func circuitSoundCurrent(width int, onset audioOnset, audio audioSnapshot) (int, int, float64, bool) {
	const lifetime = 1050 * time.Millisecond
	if width <= 0 || onset.Region != audioRegionBass ||
		onset.Age < 0 || onset.Age >= lifetime {
		return 0, 0, 0, false
	}
	direction := 1
	if onset.Position < 0 {
		direction = -1
	}
	progress := smoothstep(float64(onset.Age) / float64(lifetime))
	center := int(math.Round(progress * float64(width-1)))
	if direction < 0 {
		center = width - 1 - center
	}
	mids := (audio.LowMid + audio.HighMid) / 2
	radius := 2 + int(math.Round(mids*math.Min(9, float64(width)*0.10)))
	strength := onset.Strength * (1 - 0.45*progress)
	return center, radius, strength, true
}

func addCircuitSoundSparks(chars []rune, phase int, audio audioSnapshot) {
	if audio.Treble < 0.42 {
		return
	}
	for index := 8; index < len(chars); index += 17 {
		if (index+phase)%max(3, 8-int(math.Round(audio.Treble*4))) == 0 {
			chars[index] = '✦'
		}
	}
}

func addCircuitDiagnostic(chars []rune, phase int) {
	if len(chars) == 0 {
		return
	}
	seedOffset := organicNoise("circuit_sound", 1, 0.39) * float64(len(chars))
	position := int(math.Mod(seedOffset+float64(phase)*0.08, float64(len(chars))))
	chars[position] = '●'
	if position > 0 {
		chars[position-1] = '═'
	}
	if position+1 < len(chars) {
		chars[position+1] = '═'
	}
}
