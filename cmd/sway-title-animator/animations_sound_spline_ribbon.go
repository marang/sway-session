package main

import (
	"math"
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
	chars := make([]rune, width)
	clock := float64(phase)*0.012 +
		signedOrganicNoise("spline_sound", 1, float64(phase)/222)*0.28
	seedLift := signedOrganicNoise("spline_sound", 2, 0.31) * 0.34
	for index := range width {
		position := float64(index) / float64(max(1, width-1))
		energy := 0.18
		if audio.Active {
			energy = interpolatedAudioBand(audio.Bands, position*float64(audioBandCount-1))
		}
		y := 1.5 + seedLift +
			math.Sin(position*math.Pi*2+clock)*0.24 + (energy-0.5)*2.20
		chars[index] = splineSoundCurveGlyph(y)
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
	if !audio.Active {
		return string(fitRunes(ribbonArt(width, phase), width))
	}
	clock := float64(phase)*0.026 +
		signedOrganicNoise("ribbon_sound", 1, float64(phase)/173)*0.62
	frequency := 0.11 + mids*0.12
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
		level := 0.06 + fold*(0.24+bass*0.36) + curve*mids*0.20 + band*0.42
		if twistLive {
			distance := math.Abs(float64(index) - twistCenter)
			twist := math.Exp(-math.Pow(distance/3.4, 2)) * twistEnvelope
			level = level*(1-twist) + (1-level)*twist
		}
		if treble > 0.46 &&
			(index+phase/7)%max(9, 19-int(math.Round(treble*7))) == 0 {
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
	const lifetime = 1700 * time.Millisecond
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
