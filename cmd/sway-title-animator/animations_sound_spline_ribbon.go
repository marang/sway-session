package main

import (
	"math"
	"time"
)

func splineSoundArt(width int, phase int) string {
	return splineSoundArtWithSnapshot(width, phase,
		scaleAudioSnapshot(currentAudioSnapshot(), audioSettings.Motion))
}

func splineSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	chars := make([]rune, width)
	clock := float64(phase)*0.025 +
		signedOrganicNoise("spline_sound", 1, float64(phase)/111)*0.35
	for index := range width {
		position := float64(index) / float64(max(1, width-1))
		energy := 0.18
		if audio.Active {
			energy = interpolatedAudioBand(audio.Bands, position*float64(audioBandCount-1))
		}
		y := 1.5 + math.Sin(position*math.Pi*2+clock)*0.28 + (energy-0.5)*2.2
		chars[index] = splineSoundCurveGlyph(y)
	}
	tracer := splineSoundTracer(width, phase, audio)
	strong := false
	if onset, ok := newestSoundOnset(audio, audioRegionGeneral); ok {
		strong = onset.Strength > 0.62 && onset.Age >= 0 && onset.Age < 620*time.Millisecond
	}
	if strong {
		chars[tracer] = '◆'
	} else {
		chars[tracer] = '◇'
	}
	return string(chars)
}

func splineSoundCurveGlyph(y float64) rune {
	switch {
	case y < 0.65:
		return '⠉'
	case y < 1.35:
		return '⠒'
	case y < 2.10:
		return '⠤'
	default:
		return '⣀'
	}
}

func splineSoundTracer(width int, phase int, audio audioSnapshot) int {
	if width <= 0 {
		return 0
	}
	centroid, direction := 0.5, 1.0
	if audio.Active {
		centroid = math.Max(0, math.Min(1, audio.Centroid))
		if audio.Balance < 0 {
			direction = -1
		}
	}
	position := centroid*float64(width-1) + direction*float64(phase)*0.10
	return int(math.Mod(position+float64(width)*100, float64(width)))
}

func ribbonSoundArt(width int, phase int) string {
	return ribbonSoundArtWithSnapshot(width, phase,
		scaleAudioSnapshot(currentAudioSnapshot(), audioSettings.Motion))
}

func ribbonSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	bass, mids, treble, centroid := audio.Bass, (audio.LowMid+audio.HighMid)/2,
		audio.Treble, audio.Centroid
	if !audio.Active {
		bass, mids, treble, centroid = 0.18, 0.18, 0, 0.22
	}
	direction := 1.0
	if audio.Balance < 0 {
		direction = -1
	}
	speed := 0.025 + centroid*0.12
	clock := direction*float64(phase)*speed +
		signedOrganicNoise("ribbon_sound", 1, float64(phase)/109)*0.45
	frequency := 0.10 + mids*0.18
	ramp := []rune("·░▒▓█")
	chars := make([]rune, width)
	twistCenter, twistEnvelope, twistLive := ribbonSoundTwist(width, audio)
	for index := range width {
		position := float64(index) / float64(max(1, width-1))
		band := 0.0
		if audio.Active {
			band = interpolatedAudioBand(audio.Bands, position*float64(audioBandCount-1))
		}
		fold := 0.5 + 0.5*math.Sin(float64(index)*frequency+clock)
		curve := 0.5 + 0.5*math.Sin(float64(index)*frequency*0.47-clock*0.63)
		level := 0.10 + fold*(0.30+bass*0.30) + curve*mids*0.18 + band*0.28
		if twistLive {
			distance := math.Abs(float64(index) - twistCenter)
			twist := math.Exp(-math.Pow(distance/3.4, 2)) * twistEnvelope
			level = level*(1-twist) + (1-level)*twist
		}
		if treble > 0.38 &&
			(index+phase)%max(6, 14-int(math.Round(treble*7))) == 0 {
			chars[index] = '✦'
		} else {
			chars[index] = rampPick(ramp, math.Max(0, math.Min(1, level)))
		}
	}
	return string(chars)
}

func ribbonSoundTwist(width int, audio audioSnapshot) (float64, float64, bool) {
	onset, ok := newestSoundOnset(audio, audioRegionGeneral)
	if !ok || onset.Strength < 0.60 || width <= 0 || onset.Age < 0 {
		return 0, 0, false
	}
	const lifetime = 980 * time.Millisecond
	if onset.Age >= lifetime {
		return 0, 0, false
	}
	progress := smoothstep(float64(onset.Age) / float64(lifetime))
	center := progress * float64(width-1)
	if onset.Position < 0 {
		center = float64(width-1) - center
	}
	return center, onset.Strength * (1 - 0.45*progress), true
}
