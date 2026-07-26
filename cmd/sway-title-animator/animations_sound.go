package main

import (
	"math"
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
	if onset, ok := newestSoundOnset(audio, audioRegionGeneral); ok {
		applySquareSoundRunner(levels, onset)
	}
	segments := renderSquareLevels(levels)
	if audio.Active {
		const buildFrames = 52
		position := phase % 156
		if position < 0 {
			position += 156
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

func applySquareSoundRunner(levels []bool, onset audioOnset) {
	runner, ok := squareSoundRunner(len(levels), onset)
	if !ok {
		return
	}
	applySquareRunner(levels, runner)
}

func squareSoundRunner(width int, onset audioOnset) (squareRunner, bool) {
	const (
		minimumDuration = 900 * time.Millisecond
		durationRange   = 650 * time.Millisecond
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
		currentSoundSnapshot(),
	)
}

func ripplesSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}

	levels := make([]float64, width)
	live := false
	firstOnset := max(0, min(audio.OnsetCount, len(audio.Onsets))-2)
	for onsetIndex := firstOnset; onsetIndex < min(audio.OnsetCount, len(audio.Onsets)); onsetIndex++ {
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
		float64(phase)*0.022+signedOrganicNoise("ripples_sound", 1, float64(phase)/173),
	)
	center := float64(len(levels)-1) / 2
	center += signedOrganicNoise("ripples_sound", 2, 0.41) *
		math.Min(3, float64(len(levels))/12)
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

	levels := make([]float64, width)
	contacts := make([]float64, width)
	center := float64(width-1) / 2
	bass := audio.Bass
	mids := (audio.LowMid + audio.HighMid) / 2
	treble := audio.Treble
	speed := 0.10
	if audio.Active {
		speed += bass * 0.62
	}
	sweep := radarSoundSweepPosition(width, phase, speed)
	sweepWidth := 3.2 + bass*5.2
	for index := range width {
		distance := wrappedDistance(float64(index), sweep, float64(width))
		levels[index] = math.Max(0, 1-distance/sweepWidth)
	}

	targetWidth := 0.65 + mids*3.2
	targetWeight := 0.38
	if audio.Active {
		targetWeight += bass * 0.62
	}
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

	chars := make([]rune, width)
	sweepIndex := int(math.Round(sweep)) % width
	for index := range width {
		switch {
		case contacts[index] > 0.78:
			chars[index] = '◆'
		case contacts[index] > 0.48:
			chars[index] = '●'
		case index == sweepIndex:
			chars[index] = radarSweep[(phase/3)%len(radarSweep)]
		case audio.Active && treble > 0.32 &&
			(index+phase/4)%max(11, 25-int(math.Round(treble*8))) == 0:
			chars[index] = '·'
		case levels[index] > 0.72:
			chars[index] = '═'
		case levels[index] > 0.38:
			chars[index] = '─'
		case levels[index] > 0.12:
			chars[index] = '┄'
		case index == int(math.Round(center)):
			chars[index] = '╋'
		case !audio.Active && index%6 == 0:
			chars[index] = '·'
		default:
			chars[index] = ' '
		}
	}
	return string(chars)
}

func radarSoundSweepPosition(width int, phase int, speed float64) float64 {
	if width <= 0 {
		return 0
	}
	seedOffset := signedOrganicNoise("radar_sound", 1, 0.37) * float64(width) * 0.14
	return math.Mod(float64(phase)*speed+seedOffset+float64(width)*100, float64(width))
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
	leftEdge := max(0, int(math.Floor(center-gapRadius)))
	rightEdge := min(width-1, int(math.Ceil(center+gapRadius)))
	heavy := audio.Active && audio.Peak >= 0.68
	leftArrow, rightArrow, seam := '›', '‹', '┆'
	if heavy {
		leftArrow, rightArrow, seam = '▶', '◀', '┃'
	}
	seamIndex := max(0, min(width-1, int(math.Round(center))))
	chars := make([]rune, width)
	for index := range width {
		switch {
		case index == leftEdge:
			chars[index] = leftArrow
		case index == rightEdge:
			chars[index] = rightArrow
		case index == seamIndex:
			chars[index] = seam
		case index > leftEdge && index < rightEdge:
			if audio.Active && audio.Treble > 0.38 &&
				(index+phase/5)%max(12, 23-int(math.Round(audio.Treble*6))) == 0 {
				chars[index] = '│'
			} else {
				chars[index] = ' '
			}
		default:
			distanceFromEdge := 0
			if index < leftEdge {
				distanceFromEdge = leftEdge - index
			} else {
				distanceFromEdge = index - rightEdge
			}
			panelLevel := math.Max(0.28, 0.94-float64(distanceFromEdge)/math.Max(3, float64(width)*0.24))
			panelLevel *= 0.72 + closure*0.28
			if audio.Active && audio.Treble > 0.5 && distanceFromEdge == 1 {
				chars[index] = '│'
			} else {
				chars[index] = rampPick([]rune("░▒▓█"), panelLevel)
			}
		}
	}
	return string(chars)
}

func shutterSoundGeometry(width int, phase int, audio audioSnapshot) (float64, float64, float64) {
	center := float64(max(0, width-1)) / 2
	breathClock := float64(phase)*0.018 + signedOrganicNoise("shutter_sound", 1, 0.41)*0.65
	breath := 0.5 + 0.5*math.Sin(breathClock)
	openness := 0.76 + breath*0.10
	closure := 1 - openness
	if audio.Active {
		closure = 0.10 + audio.LowMid*0.44
		if onset, ok := newestSoundOnset(audio, audioRegionBass); ok {
			const lifetime = 1350 * time.Millisecond
			if onset.Age >= 0 && onset.Age < lifetime {
				progress := float64(onset.Age) / float64(lifetime)
				onsetClosure := onset.Strength * (1 - smoothstep(progress))
				closure = math.Max(closure, onsetClosure)
			}
		}
		closure = math.Max(0.06, math.Min(0.92, closure))
		center += math.Max(-1, math.Min(1, audio.Balance)) * float64(width) * 0.045
		center = math.Max(float64(width)*0.12, math.Min(float64(width-1)-float64(width)*0.12, center))
	}
	openness = 1 - closure
	gapRadius := math.Max(1, openness*float64(max(1, width-1))/2)
	return center, gapRadius, closure
}
