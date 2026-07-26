package main

import (
	"math"
	"slices"
	"time"
)

func splineSoundArt(width int, phase int) string {
	return splineSoundArtWithSnapshot(width, phase,
		currentSoundSnapshot())
}

func splineSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	chars := fitRunes(splineArt(width, phase), width)
	if !audio.Active {
		return string(chars)
	}
	beatLift := 0.0
	if onset, ok := newestSoundOnset(audio, audioRegionGeneral); ok &&
		onset.Age >= 0 && onset.Age < 650*time.Millisecond {
		progress := float64(onset.Age) / float64(650*time.Millisecond)
		beatLift = onset.Strength * (1 - smoothstep(progress))
	}
	for index := range width {
		if chars[index] < 0x2800 || chars[index] > 0x28ff {
			continue
		}
		position := float64(index) / float64(max(1, width-1))
		energy := interpolatedAudioBand(
			audio.Bands,
			position*float64(audioBandCount-1),
		)
		baseMask := int(chars[index] - 0x2800)
		oscillation := math.Sin(
			float64(index)*0.19 - float64(phase)*0.065 +
				audio.Centroid*math.Pi,
		)
		amplitude := 0.30 + energy*1.45 + audio.Level*0.35 + beatLift*0.85
		shift := int(math.Round(oscillation * math.Min(2.4, amplitude)))
		mask := shiftSplineBrailleMask(baseMask, shift)
		if energy+beatLift*0.55 > 0.52 {
			thickenDirection := 1
			if oscillation < 0 {
				thickenDirection = -1
			}
			mask |= baseMask
			mask |= shiftSplineBrailleMask(baseMask, thickenDirection)
		}
		chars[index] = rune(0x2800 + mask)
	}
	tracer := splineSoundTracer(width, phase, audio)
	strong := false
	if onset, ok := newestSoundOnset(audio, audioRegionGeneral); ok {
		strong = onset.Strength > 0.68 && onset.Age >= 0 && onset.Age < 900*time.Millisecond
	}
	if strong {
		chars[tracer] = '◆'
	} else {
		chars[tracer] = '◇'
	}
	return string(chars)
}

func shiftSplineBrailleMask(mask int, shift int) int {
	shifted := 0
	for column := range 2 {
		for row := range 4 {
			dot := splineSoundDotMask(row, column)
			if mask&dot == 0 {
				continue
			}
			targetRow := max(0, min(3, row+shift))
			shifted |= splineSoundDotMask(targetRow, column)
		}
	}
	return shifted
}

func splineSoundDotMask(row int, column int) int {
	if column == 0 {
		return []int{0x01, 0x02, 0x04, 0x40}[row]
	}
	return []int{0x08, 0x10, 0x20, 0x80}[row]
}

func splineSoundTracer(width int, phase int, audio audioSnapshot) int {
	if width <= 0 {
		return 0
	}
	centroid := 0.5
	balance := 0.0
	if audio.Active {
		centroid = math.Max(0, math.Min(1, audio.Centroid))
		balance = math.Max(-1, math.Min(1, audio.Balance))
	}
	centroidOffset := (centroid - 0.5) * float64(width-1) * 0.22
	balanceOffset := balance * float64(width-1) * 0.08
	position := float64(width-1)*0.5 + centroidOffset +
		balanceOffset + float64(phase)*0.050
	return int(math.Mod(position+float64(width)*100, float64(width)))
}

func ribbonSoundArt(width int, phase int) string {
	return ribbonSoundArtWithSnapshot(width, phase,
		currentSoundSnapshot())
}

func ribbonSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	bass, mids, treble := audio.Bass, (audio.LowMid+audio.HighMid)/2,
		audio.Treble
	chars := fitRunes(ribbonArt(width, phase), width)
	if !audio.Active {
		return string(chars)
	}
	ramp := []rune("·░▒▓█")
	pulse := ribbonSoundPulse(audio)
	for index := range width {
		position := float64(index) / float64(max(1, width-1))
		band := interpolatedAudioBand(
			audio.Bands,
			position*float64(audioBandCount-1),
		)
		rampIndex := slices.Index(ramp, chars[index])
		if rampIndex >= 0 {
			lift := band*0.55 + bass*0.25 + mids*0.20
			ribbonWave := 0.5 + 0.5*math.Sin(
				float64(index)*0.21-float64(phase)*0.052,
			)
			lift += pulse * (0.18 + ribbonWave*0.68)
			offset := int(math.Round((lift - 0.38) * 2.6))
			rampIndex = max(0, min(len(ramp)-1, rampIndex+offset))
			chars[index] = ramp[rampIndex]
			if pulse*ribbonWave > 0.62 {
				chars[index] = ramp[len(ramp)-1-rampIndex]
			}
		}
		if treble > 0.46 &&
			(index+phase/7)%max(9, 19-int(math.Round(treble*7))) == 0 ||
			pulse > 0.66 && (index+phase/11)%23 == 0 {
			chars[index] = '✦'
		}
	}
	return string(chars)
}

func ribbonSoundPulse(audio audioSnapshot) float64 {
	onset, ok := newestSoundOnset(audio, audioRegionGeneral)
	if !ok || onset.Age < 0 {
		return 0
	}
	const lifetime = 680 * time.Millisecond
	if onset.Age >= lifetime {
		return 0
	}
	progress := float64(onset.Age) / float64(lifetime)
	return onset.Strength * (1 - smoothstep(progress))
}
