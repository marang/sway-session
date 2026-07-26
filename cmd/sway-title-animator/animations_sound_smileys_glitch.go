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
	mids := (audio.LowMid + audio.HighMid) / 2
	for index := range chars {
		if chars[index] != ' ' {
			continue
		}
		pulse := organicNoise(
			"smileys_sound", uint64(100+index), float64(phase)/24,
		)
		if pulse > 0.92-audio.Level*0.30 {
			switch {
			case audio.Treble > 0.48:
				chars[index] = '✦'
			case audio.Bass > mids:
				chars[index] = '●'
			default:
				chars[index] = '·'
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

	density := math.Max(0, math.Min(1, audio.SpectralFlux*0.82+audio.Level*0.18))
	spacing := max(5, 19-int(math.Round(density*12)))
	motionPhase := phase / 5
	for index := range chars {
		if (index+motionPhase)%spacing == 0 && chars[index] == '─' {
			chars[index] = '┄'
		}
		if audio.Treble > 0.32 &&
			(index*3+motionPhase)%max(7, 19-int(math.Round(audio.Treble*9))) == 0 {
			chars[index] = []rune{'╴', '╶', '╪'}[(index+motionPhase)%3]
		}
	}
	addGlitchSoundWindow(chars, phase, density)
	addGlitchSoundDisplacement(chars, audio)
	return string(chars)
}

func addGlitchSoundWindow(chars []rune, phase int, density float64) {
	if len(chars) == 0 || density < 0.16 {
		return
	}
	center := int(organicNoise(
		"glitch_sound", 8, float64(phase)/58,
	) * float64(len(chars)-1))
	radius := 2 + int(math.Round(density*math.Min(10, float64(len(chars))*0.10)))
	tiles := []rune{'╴', '╶', '╪', '╳', '░', '▒'}
	for index := max(0, center-radius); index <= min(len(chars)-1, center+radius); index++ {
		if animationRandom("glitch_sound", 80, int64(index+phase/9)) > density {
			continue
		}
		chars[index] = tiles[abs(index+phase/7)%len(tiles)]
	}
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
