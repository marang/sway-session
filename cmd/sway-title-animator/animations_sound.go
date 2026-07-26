package main

import (
	"math"
	"strings"
	"time"
)

func squareSoundArt(width int, phase int) string {
	return squareSoundArtWithSnapshot(
		width,
		phase,
		currentSoundSnapshot(),
	)
}

func squareSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}

	levels := squareSoundLevels(width, phase, audio)
	segments := renderSquareLevels(levels)
	if audio.Active {
		if onset, ok := newestSoundOnset(audio, audioRegionGeneral); ok {
			revealSquareSoundBeat(segments, levels, onset)
		}
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
	drift := signedOrganicNoise("square_sound", 1, float64(phase)/156)
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
			float64(phase)/181+float64(run)*0.17,
		) * 1.4))
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

func revealSquareSoundBeat(
	segments []squareSegment,
	levels []bool,
	onset audioOnset,
) {
	if len(segments) == 0 || onset.Age < 0 {
		return
	}
	runCount := 1
	for index := 1; index < len(levels); index++ {
		if levels[index] != levels[index-1] {
			runCount++
		}
	}

	sequence := onset.Sequence
	if sequence == 0 {
		sequence = max(uint64(1), onset.ID)
	}
	zeroBased := sequence - 1
	runIndex := int(zeroBased % uint64(runCount))
	buildCycle := int64(zeroBased / uint64(runCount))
	leftToRight := eventRandom(
		"square_sound_direction",
		1,
		buildCycle,
		1,
	) < 0.5

	runEnds := squareSoundRunEnds(levels, leftToRight)
	previous := 0
	if runIndex > 0 {
		previous = runEnds[runIndex-1]
	}
	target := runEnds[runIndex]
	duration := 160*time.Millisecond +
		time.Duration((1-onset.Strength)*float64(140*time.Millisecond))
	progress := 1.0
	if onset.Age < duration {
		progress = smoothstep(float64(onset.Age) / float64(duration))
	}
	revealed := previous + max(1, int(math.Ceil(progress*float64(target-previous))))
	revealSquareSegments(segments, revealed, leftToRight)
}

func squareSoundRunEnds(levels []bool, leftToRight bool) []int {
	if len(levels) == 0 {
		return nil
	}
	ends := make([]int, 0, len(levels))
	for revealed := 1; revealed < len(levels); revealed++ {
		previous := revealed - 1
		current := revealed
		if !leftToRight {
			previous = len(levels) - revealed
			current = previous - 1
		}
		if levels[previous] != levels[current] {
			ends = append(ends, revealed)
		}
	}
	return append(ends, len(levels))
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
		currentSoundSnapshot(),
	)
}

func ripplesSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return string(fitRunes(ripplesArt(width, phase), width))
	}
	if !audio.Active {
		return string(fitRunes(ripplesArt(width, phase), width))
	}

	levels := ripplesBaseLevels(width, phase)
	audioLevels := make([]float64, width)
	firstOnset := max(0, min(audio.OnsetCount, len(audio.Onsets))-2)
	for onsetIndex := firstOnset; onsetIndex < min(audio.OnsetCount, len(audio.Onsets)); onsetIndex++ {
		onset := audio.Onsets[onsetIndex]
		center, radius, thickness, envelope, ok := soundRipple(width, onset)
		if !ok {
			continue
		}
		for index := range width {
			distance := math.Abs(float64(index) - center)
			ring := math.Exp(-math.Pow((distance-radius)/thickness, 2)) * envelope
			if radius < thickness*0.8 {
				impact := math.Exp(-math.Pow(distance/math.Max(1, thickness), 2)) *
					envelope * (1 - radius/(thickness*0.8))
				ring = math.Max(ring, impact)
			}
			levels[index] = math.Max(levels[index], ring)
			audioLevels[index] = math.Max(audioLevels[index], ring)
		}
	}
	chars := []rune(ripplesLevelsArt(levels, phase))
	for index, level := range audioLevels {
		switch {
		case level > 0.68:
			chars[index] = '◉'
		case level > 0.35:
			chars[index] = '◎'
		}
	}
	return string(chars)
}

func soundRipple(width int, onset audioOnset) (float64, float64, float64, float64, bool) {
	if width <= 0 || onset.Age < 0 {
		return 0, 0, 0, 0, false
	}
	lifetime := 1550 * time.Millisecond
	speed := 0.58
	thickness := 1.8
	switch onset.Region {
	case audioRegionBass:
		lifetime = 2100 * time.Millisecond
		speed = 0.34
		thickness = 3.2
	case audioRegionHigh:
		lifetime = 1200 * time.Millisecond
		speed = 0.58
		thickness = 1.05
	}
	if onset.Age >= lifetime {
		return 0, 0, 0, 0, false
	}

	stereo := math.Max(-1, math.Min(1, onset.Position))
	centerPosition := 0.12 + eventRandom(
		"ripples_sound_center",
		1,
		int64(onset.ID),
		1,
	)*0.76
	switch onset.Region {
	case audioRegionBass:
		centerPosition = centerPosition*0.85 + 0.5*0.15 + stereo*0.05
	case audioRegionHigh:
		centerPosition += stereo * 0.14
	default:
		centerPosition += stereo * 0.10
	}
	centerPosition = math.Max(0.06, math.Min(0.94, centerPosition))
	progress := float64(onset.Age) / float64(lifetime)
	center := centerPosition * float64(max(0, width-1))
	radius := progress * speed * float64(width)
	envelope := onset.Strength * (1 - smoothstep(progress))
	thickness *= 0.65 + onset.Strength*0.55
	return center, radius, thickness, envelope, true
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

func radarSoundArt(width int, phase int) string {
	return radarSoundArtWithSnapshot(
		width,
		phase,
		currentSoundSnapshot(),
	)
}

func radarSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	chars := fitRunes(radarArt(width, phase), width)
	if !audio.Active {
		return string(chars)
	}

	contacts := make([]float64, width)
	center := float64(width-1) / 2
	bass := audio.Bass
	mids := (audio.LowMid + audio.HighMid) / 2
	treble := audio.Treble

	targetWidth := 0.65 + mids*3.2
	targetWeight := 0.22 + bass*0.72
	for index := range width {
		distance := math.Abs(float64(index) - center)
		if distance <= targetWidth {
			contacts[index] = math.Max(contacts[index], targetWeight*(1-distance/(targetWidth+0.5)))
		}
	}

	firstOnset := max(0, min(audio.OnsetCount, len(audio.Onsets))-2)
	for onsetIndex := firstOnset; onsetIndex < min(audio.OnsetCount, len(audio.Onsets)); onsetIndex++ {
		echoCenter, echoWidth, envelope, ok := radarSoundEcho(width, audio.Onsets[onsetIndex], mids)
		if !ok {
			continue
		}
		for index := range width {
			distance := math.Abs(float64(index) - echoCenter)
			if distance <= echoWidth {
				contacts[index] = math.Max(
					contacts[index],
					envelope*(1-distance/(echoWidth+0.4)),
				)
			}
		}
	}

	head := radarHeadPosition(width, phase)
	sweepWidth := 6.0 + bass*8.0
	for index := range width {
		distance := wrappedDistance(float64(index), head, float64(width))
		sweepLevel := math.Max(0, 1-distance/sweepWidth)
		switch {
		case contacts[index] > 0.78:
			chars[index] = '◆'
		case contacts[index] > 0.48:
			chars[index] = '●'
		case distance < 0.55:
			chars[index] = radarSweep[(phase/3)%len(radarSweep)]
		case sweepLevel > 0.72 &&
			strings.ContainsRune(" ┄─═", chars[index]):
			chars[index] = '═'
		case sweepLevel > 0.38 && strings.ContainsRune(" ┄─", chars[index]):
			chars[index] = '─'
		case sweepLevel > 0.14 && chars[index] == ' ':
			chars[index] = '┄'
		case treble > 0.32 && chars[index] == ' ' &&
			(index+phase/4)%max(11, 25-int(math.Round(treble*8))) == 0:
			chars[index] = '·'
		}
	}
	return string(chars)
}

func radarSoundEcho(width int, onset audioOnset, mids float64) (float64, float64, float64, bool) {
	const lifetime = 1650 * time.Millisecond
	if width <= 0 || onset.Age < 0 || onset.Age >= lifetime {
		return 0, 0, 0, false
	}
	progress := float64(onset.Age) / float64(lifetime)
	stereo := math.Max(-1, math.Min(1, onset.Position))
	regionReach := 0.58
	switch onset.Region {
	case audioRegionBass:
		regionReach = 0.30
	case audioRegionHigh:
		regionReach = 0.86
	}
	center := float64(width-1) / 2
	position := center + stereo*center*regionReach
	drift := math.Copysign(progress*center*0.10, stereo)
	if stereo == 0 {
		drift = progress * center * 0.06
	}
	position = math.Max(0, math.Min(float64(width-1), position+drift))
	echoWidth := 0.65 + mids*3.4
	if onset.Region == audioRegionBass {
		echoWidth += 0.8
	}
	envelope := onset.Strength * (1 - smoothstep(progress))
	return position, echoWidth, envelope, true
}

func shutterSoundArt(width int, phase int) string {
	return shutterSoundArtWithSnapshot(
		width,
		phase,
		currentSoundSnapshot(),
	)
}

func shutterSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}

	center, gapRadius, closure := shutterSoundGeometry(width, phase, audio)
	treble, peak := 0.0, 0.0
	if audio.Active {
		treble, peak = audio.Treble, audio.Peak
	}
	return shutterFrame(width, phase, center, gapRadius, closure, treble, peak)
}

func shutterSoundGeometry(width int, phase int, audio audioSnapshot) (float64, float64, float64) {
	center := float64(max(0, width-1)) / 2
	breathClock := float64(phase)*0.024 + signedOrganicNoise("shutter_sound", 1, 0.41)*0.65
	breath := 0.5 + 0.5*math.Sin(breathClock)
	openness := 0.22 + breath*0.66
	closure := 1 - openness
	center += signedOrganicNoise("shutter", 5, float64(phase)/118) *
		math.Min(4, float64(width)*0.035)
	if audio.Active {
		audioClosure := 0.08 + audio.LowMid*0.76
		closure = closure*0.55 + audioClosure*0.45
		if onset, ok := newestSoundOnset(audio, audioRegionBass); ok {
			const lifetime = 1350 * time.Millisecond
			if onset.Age >= 0 && onset.Age < lifetime {
				progress := float64(onset.Age) / float64(lifetime)
				onsetClosure := onset.Strength * (1 - smoothstep(progress))
				closure = math.Max(closure, onsetClosure)
			}
		}
		closure = math.Max(0.06, math.Min(0.92, closure))
	}
	openness = 1 - closure
	gapRadius := math.Max(1, openness*float64(max(1, width-1))/2)
	return center, gapRadius, closure
}
