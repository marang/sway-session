package main

import (
	"math"
	"time"
)

func squareSoundArt(width int, phase int) string {
	return squareSoundArtWithSnapshot(
		width,
		phase,
		scaleAudioSnapshot(currentAudioSnapshot(), audioSettings.Motion),
	)
}

func squareSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}

	levels := squareSoundLevels(width, phase, audio)
	if onset, ok := newestSoundOnset(audio, audioRegionGeneral); ok {
		applySquareSoundRunner(levels, onset)
	}
	segments := renderSquareLevels(levels)
	if audio.Active {
		const buildFrames = 36
		position := phase % 104
		if position < 0 {
			position += 104
		}
		revealed := width
		if position < buildFrames {
			progress := smoothstep(float64(position) / buildFrames)
			revealed = int(math.Ceil(progress * float64(width)))
		}
		revealSquareSegments(segments, revealed, audio.Balance >= 0)
	}
	return squareSegmentsArt(segments)
}

func squareSoundLevels(width int, phase int, audio audioSnapshot) []bool {
	levels := make([]bool, width)
	bass := audio.Bass
	level := audio.Level
	if !audio.Active {
		bass = 0.82
		level = 0.34
	}
	drift := signedOrganicNoise("square_sound", 1, float64(phase)/88)
	baseLength := 4 + int(math.Round(bass*13+drift*2.4))
	baseLength = max(3, baseLength)
	duty := math.Max(0.22, math.Min(0.78, 0.30+level*0.48))
	highLength := max(2, int(math.Round(float64(baseLength)*duty*2)))
	lowLength := max(2, int(math.Round(float64(baseLength)*(1-duty)*2)))

	high := true
	cursor := 0
	run := 0
	for cursor < width {
		runLength := lowLength
		if high {
			runLength = highLength
		}
		variation := int(math.Round(signedOrganicNoise(
			"square_sound",
			uint64(10+run%7),
			float64(phase)/113+float64(run)*0.17,
		) * 2))
		runLength = max(2, runLength+variation)
		for range runLength {
			if cursor >= width {
				break
			}
			levels[cursor] = high
			cursor++
		}
		high = !high
		run++
	}
	return levels
}

func applySquareSoundRunner(levels []bool, onset audioOnset) {
	runner, ok := squareSoundRunner(len(levels), onset)
	if !ok {
		return
	}
	applySquareRunner(levels, runner)
}

func squareSoundRunner(width int, onset audioOnset) (squareRunner, bool) {
	const (
		minimumDuration = 480 * time.Millisecond
		durationRange   = 520 * time.Millisecond
	)
	duration := minimumDuration + time.Duration((1-onset.Strength)*float64(durationRange))
	if onset.Age < 0 || onset.Age >= duration || width < 6 {
		return squareRunner{}, false
	}
	progress := smoothstep(float64(onset.Age) / float64(duration))
	barLength := 4 + int(math.Round(onset.Strength*12))
	packetWidth := min(width, barLength+4)
	last := max(0, width-packetWidth)
	left := int(math.Round(progress * float64(last)))
	if onset.Position < 0 {
		left = last - left
	}
	return squareRunner{
		left:      left,
		barLength: min(barLength, max(2, packetWidth-4)),
	}, true
}

func squareSegmentsArt(segments []squareSegment) string {
	chars := make([]rune, len(segments))
	for index, segment := range segments {
		switch segment {
		case squareHigh:
			chars[index] = '⎺'
		case squareFalling:
			chars[index] = '⎤'
		case squareLow:
			chars[index] = '⎽'
		case squareRising:
			chars[index] = '⎡'
		default:
			chars[index] = ' '
		}
	}
	return string(chars)
}

func ripplesSoundArt(width int, phase int) string {
	return ripplesSoundArtWithSnapshot(
		width,
		phase,
		scaleAudioSnapshot(currentAudioSnapshot(), audioSettings.Motion),
	)
}

func ripplesSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}

	levels := make([]float64, width)
	live := false
	for onsetIndex := 0; onsetIndex < min(audio.OnsetCount, len(audio.Onsets)); onsetIndex++ {
		onset := audio.Onsets[onsetIndex]
		center, radius, thickness, envelope, ok := soundRipple(width, onset)
		if !ok {
			continue
		}
		live = true
		for index := range width {
			distance := math.Abs(float64(index) - center)
			ring := math.Exp(-math.Pow((distance-radius)/thickness, 2)) * envelope
			if radius < thickness*0.8 {
				impact := math.Exp(-math.Pow(distance/math.Max(1, thickness), 2)) *
					envelope * (1 - radius/(thickness*0.8))
				ring = math.Max(ring, impact)
			}
			levels[index] = math.Max(levels[index], ring)
		}
	}
	if !live {
		addSoundRippleIdle(levels, phase)
	}
	return soundRippleLevelsArt(levels)
}

func soundRipple(width int, onset audioOnset) (float64, float64, float64, float64, bool) {
	if width <= 0 || onset.Age < 0 {
		return 0, 0, 0, 0, false
	}
	lifetime := 1050 * time.Millisecond
	speed := 0.58
	thickness := 1.8
	switch onset.Region {
	case audioRegionBass:
		lifetime = 1450 * time.Millisecond
		speed = 0.42
		thickness = 3.2
	case audioRegionHigh:
		lifetime = 760 * time.Millisecond
		speed = 0.78
		thickness = 1.05
	}
	if onset.Age >= lifetime {
		return 0, 0, 0, 0, false
	}

	stereo := (math.Max(-1, math.Min(1, onset.Position)) + 1) / 2
	centerPosition := stereo
	switch onset.Region {
	case audioRegionBass:
		centerPosition = 0.5 + (stereo-0.5)*0.45
	case audioRegionHigh:
		if stereo < 0.5 {
			centerPosition = 0.18 + stereo*0.34
		} else {
			centerPosition = 0.65 + (stereo-0.5)*0.34
		}
	}
	progress := float64(onset.Age) / float64(lifetime)
	center := centerPosition * float64(max(0, width-1))
	radius := progress * speed * float64(width)
	envelope := onset.Strength * (1 - smoothstep(progress))
	thickness *= 0.65 + onset.Strength*0.55
	return center, radius, thickness, envelope, true
}

func addSoundRippleIdle(levels []float64, phase int) {
	if len(levels) == 0 {
		return
	}
	breath := 0.5 + 0.5*math.Sin(
		float64(phase)*0.045+signedOrganicNoise("ripples_sound", 1, float64(phase)/97),
	)
	center := float64(len(levels)-1) / 2
	radius := 1.5 + breath*math.Min(5, float64(len(levels))/8)
	for index := range levels {
		distance := math.Abs(float64(index) - center)
		levels[index] = math.Exp(-math.Pow((distance-radius)/1.35, 2)) * (0.24 + breath*0.16)
	}
}

func soundRippleLevelsArt(levels []float64) string {
	chars := make([]rune, len(levels))
	for index, level := range levels {
		switch {
		case level > 0.84:
			chars[index] = '●'
		case level > 0.62:
			chars[index] = '═'
		case level > 0.38:
			chars[index] = '─'
		case level > 0.16:
			if index%2 == 0 {
				chars[index] = '╴'
			} else {
				chars[index] = '╶'
			}
		default:
			chars[index] = ' '
		}
	}
	return string(chars)
}

func newestSoundOnset(audio audioSnapshot, region audioRegion) (audioOnset, bool) {
	var newest audioOnset
	found := false
	for index := 0; index < min(audio.OnsetCount, len(audio.Onsets)); index++ {
		onset := audio.Onsets[index]
		if onset.Region != region || (found && onset.ID <= newest.ID) {
			continue
		}
		newest = onset
		found = true
	}
	return newest, found
}
