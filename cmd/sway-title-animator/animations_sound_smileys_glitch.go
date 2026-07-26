package main

import (
	"math"
	"time"
)

func smileysSoundArt(width int, phase int) string {
	return smileysSoundArtWithSnapshot(width, phase,
		currentSoundSnapshot())
}

func smileysSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	chars := fitRunes(smileysArt(width, phase), width)
	if !audio.Active {
		return string(chars)
	}
	if audio.Treble > 0.58 {
		accented := false
		for index := range chars {
			if (chars[index] == '·' || chars[index] == '｡') &&
				(index+phase/11)%5 == 0 {
				chars[index] = '✦'
				accented = true
			}
		}
		if !accented {
			for index := range chars {
				if chars[index] == ' ' {
					chars[index] = '✦'
					break
				}
			}
		}
	}
	if smileysSoundReaction(audio) {
		reaction := []rune("(ﾉ◕ヮ◕)ﾉ✦")
		position := max(0, (width-len(reaction))/2)
		if onset, ok := newestSoundOnset(audio, audioRegionGeneral); ok {
			position = int(math.Round((0.5 + onset.Position*0.35) * float64(max(0, width-len(reaction)))))
		}
		position = max(0, min(max(0, width-len(reaction)), position))
		copy(chars[position:min(width, position+len(reaction))], reaction)
	}
	return string(chars)
}

func smileysSoundReaction(audio audioSnapshot) bool {
	onset, ok := newestSoundOnset(audio, audioRegionGeneral)
	return ok && onset.Strength >= 0.72 && onset.Age >= 0 &&
		onset.Age < 700*time.Millisecond
}

func glitchSoundArt(width int, phase int) string {
	return glitchSoundArtWithSnapshot(width, phase,
		currentSoundSnapshot())
}

func glitchSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	chars := fitRunes(glitchArt(width, phase), width)
	if !audio.Active {
		return string(chars)
	}

	density := math.Max(0, math.Min(1, audio.SpectralFlux*0.72))
	spacing := max(8, 23-int(math.Round(density*12)))
	motionPhase := phase / 5
	for index := range chars {
		if (index+motionPhase)%spacing == 0 && chars[index] == '─' {
			chars[index] = '┄'
		}
		if audio.Treble > 0.48 &&
			(index*3+motionPhase)%max(11, 23-int(math.Round(audio.Treble*8))) == 0 {
			chars[index] = []rune{'╴', '╶', '╪'}[(index+motionPhase)%3]
		}
	}
	addGlitchSoundDisplacement(chars, audio)
	return string(chars)
}

func addGlitchSoundDisplacement(chars []rune, audio audioSnapshot) {
	onset, ok := newestSoundOnset(audio, audioRegionBass)
	if !ok || len(chars) == 0 {
		return
	}
	center, radius, live := glitchSoundBlock(len(chars), onset)
	if !live {
		return
	}
	for offset := -radius; offset <= radius; offset++ {
		index := center + offset
		if index < 0 || index >= len(chars) {
			continue
		}
		chars[index] = []rune{'░', '▒', '▓', '╳'}[abs(offset+int(onset.ID))%4]
	}
}

func glitchSoundBlock(width int, onset audioOnset) (int, int, bool) {
	const lifetime = 1100 * time.Millisecond
	if width <= 0 || onset.Region != audioRegionBass ||
		onset.Age < 0 || onset.Age >= lifetime {
		return 0, 0, false
	}
	stereo := math.Max(-1, math.Min(1, onset.Position))
	center := int(math.Round((0.5 + stereo*0.40) * float64(width-1)))
	progress := float64(onset.Age) / float64(lifetime)
	center += int(math.Copysign(progress*float64(width)*0.10, stereo))
	center = max(0, min(width-1, center))
	radius := 1 + int(math.Round(onset.Strength*math.Min(5, float64(width)*0.06)))
	return center, radius, true
}
