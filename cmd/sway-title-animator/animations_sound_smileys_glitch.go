package main

import (
	"math"
	"time"
)

func smileysSoundArt(width int, phase int) string {
	return smileysSoundArtWithSnapshot(width, phase,
		scaleAudioSnapshot(currentAudioSnapshot(), audioSettings.Motion))
}

func smileysSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	chars := make([]rune, width)
	for index := range chars {
		chars[index] = ' '
	}
	level, bass, mids, treble := audio.Level, audio.Bass,
		(audio.LowMid+audio.HighMid)/2, audio.Treble
	count := 1
	speed := 0.035
	if audio.Active {
		count = 1 + int(math.Round(level*2))
		speed += level * 0.22
	}
	reaction := smileysSoundReaction(audio)
	for faceIndex := range count {
		face := smileysSoundFace(bass, mids, reaction)
		if !audio.Active {
			face = []rune("｡･ﾟﾟ･ʕ•ᴥ•ʔっ･ﾟﾟ･｡")
		}
		travel := max(1, width-len(face)+1)
		seedOffset := organicNoise(
			"smileys_sound", uint64(10+faceIndex), 0.37,
		) * float64(travel)
		offset := float64(faceIndex) * float64(travel) / float64(count)
		position := int(math.Mod(
			float64(phase)*speed+offset+seedOffset+float64(travel)*100,
			float64(travel),
		))
		if audio.Balance < 0 {
			position = width - position - len(face)
		}
		for glyphIndex, glyph := range face {
			index := position + glyphIndex
			if index >= 0 && index < width {
				chars[index] = glyph
			}
		}
		if treble > 0.38 {
			accent := position - 2
			if faceIndex%2 == 1 {
				accent = position + len(face) + 1
			}
			if accent >= 0 && accent < width {
				chars[accent] = '✦'
			}
		}
	}
	return string(chars)
}

func smileysSoundFace(bass float64, mids float64, reaction bool) []rune {
	if reaction {
		return []rune("ʕ>_<ʔ")
	}
	eyes := '•'
	if bass > 0.66 {
		eyes = '●'
	} else if bass > 0.34 {
		eyes = '◉'
	}
	mouth := 'ᴥ'
	if mids > 0.66 {
		mouth = '▽'
	} else if mids > 0.34 {
		mouth = 'ω'
	}
	return []rune{'ʕ', eyes, mouth, eyes, 'ʔ'}
}

func smileysSoundReaction(audio audioSnapshot) bool {
	onset, ok := newestSoundOnset(audio, audioRegionGeneral)
	return ok && onset.Strength >= 0.72 && onset.Age >= 0 &&
		onset.Age < 460*time.Millisecond
}

func glitchSoundArt(width int, phase int) string {
	return glitchSoundArtWithSnapshot(width, phase,
		scaleAudioSnapshot(currentAudioSnapshot(), audioSettings.Motion))
}

func glitchSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	chars := make([]rune, width)
	for index := range chars {
		chars[index] = '─'
	}
	if !audio.Active {
		defect := int(math.Mod(
			organicNoise("glitch_sound", 1, 0.43)*float64(width)+
				float64(phase)*0.06,
			float64(width),
		))
		chars[defect] = '╍'
		return string(chars)
	}

	if onset, ok := newestSoundOnset(audio, audioRegionGeneral); ok &&
		onset.Strength > 0.92 && onset.Age >= 0 && onset.Age < 130*time.Millisecond {
		for index := range chars {
			if index%11 == 0 {
				chars[index] = '╪'
			} else {
				chars[index] = '═'
			}
		}
		return string(chars)
	}

	density := math.Max(0, math.Min(1, audio.SpectralFlux))
	spacing := max(3, 19-int(math.Round(density*14)))
	for index := range chars {
		if (index+phase)%spacing == 0 {
			chars[index] = '┄'
		}
		if audio.Treble > 0.30 &&
			(index*3+phase)%max(5, 17-int(math.Round(audio.Treble*9))) == 0 {
			chars[index] = []rune{'╴', '╶', '░'}[(index+phase)%3]
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
	const lifetime = 620 * time.Millisecond
	if width <= 0 || onset.Region != audioRegionBass ||
		onset.Age < 0 || onset.Age >= lifetime {
		return 0, 0, false
	}
	stereo := math.Max(-1, math.Min(1, onset.Position))
	center := int(math.Round((0.5 + stereo*0.40) * float64(width-1)))
	progress := float64(onset.Age) / float64(lifetime)
	center += int(math.Copysign(progress*float64(width)*0.10, stereo))
	center = max(0, min(width-1, center))
	radius := 1 + int(math.Round(onset.Strength*math.Min(7, float64(width)*0.08)))
	return center, radius, true
}
