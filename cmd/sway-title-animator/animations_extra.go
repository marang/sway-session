package main

import (
	"math"
	"slices"
	"strings"
)

type squareSegment uint8

const (
	squareHidden squareSegment = iota
	squareHigh
	squareFalling
	squareLow
	squareRising
)

const (
	squareCycleFrames      = 64
	squareDrawingFrames    = 40
	squareRunnerStartFrame = 44
	squareRunnerChance     = 0.42
)

type squareRunner struct {
	left      int
	barLength int
}

func squareArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}

	segments := squareSegments(width, phase)
	chars := make([]rune, width)
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

func squareSegments(width int, phase int) []squareSegment {
	cycle, position, revealed, leftToRight := squareBuildState(width, phase)
	levels := squareBaseLevels(width, cycle)
	if revealed == width {
		if runner, ok := squareRunnerState(width, cycle, position); ok {
			applySquareRunner(levels, runner)
		}
	}
	segments := renderSquareLevels(levels)
	revealSquareSegments(segments, revealed, leftToRight)
	return segments
}

func squareBuildState(width int, phase int) (int64, int, int, bool) {
	cycle := phase / squareCycleFrames
	position := phase % squareCycleFrames
	if position < 0 {
		position += squareCycleFrames
		cycle--
	}

	progress := math.Min(1, float64(position)/squareDrawingFrames)
	revealed := int(math.Ceil(smoothstep(progress) * float64(width)))
	leftToRight := eventRandom("square", 8, int64(cycle), 1) < 0.5
	return int64(cycle), position, min(width, revealed), leftToRight
}

func squareBaseLevels(width int, cycle int64) []bool {
	levels := make([]bool, width)
	high := eventRandom("square", 1, cycle, 1) < 0.5
	lengthScale := 0.62 + eventRandom("square", 1, cycle, 2)*0.86
	cursor := 0
	previousLength := -1

	for run := int64(0); cursor < width; run++ {
		event := cycle*512 + run
		minLength, lengthRange := 3, 13
		if !high {
			minLength, lengthRange = 4, 17
		}
		runLength := max(2, int(math.Round(float64(minLength+
			int(eventRandom("square", 2, event, 1)*float64(lengthRange)))*lengthScale)))
		if runLength == previousLength {
			runLength += 1 + int(eventRandom("square", 2, event, 2)*3)
		}
		previousLength = runLength

		for range runLength {
			if cursor >= width {
				break
			}
			levels[cursor] = high
			cursor++
		}
		high = !high
	}
	return levels
}

func squareBaseSegments(width int, cycle int64) []squareSegment {
	return renderSquareLevels(squareBaseLevels(width, cycle))
}

func renderSquareLevels(levels []bool) []squareSegment {
	segments := make([]squareSegment, len(levels))
	for index, high := range levels {
		if index > 0 && high != levels[index-1] {
			if high {
				segments[index] = squareRising
			} else {
				segments[index] = squareFalling
			}
		} else if high {
			segments[index] = squareHigh
		} else {
			segments[index] = squareLow
		}
	}
	return segments
}

func squareRunnerState(width int, cycle int64, position int) (squareRunner, bool) {
	if width < 12 || position < squareRunnerStartFrame ||
		eventRandom("square-runner", 1, cycle, 1) >= squareRunnerChance {
		return squareRunner{}, false
	}

	barLength := 4 + int(eventRandom("square-runner", 1, cycle, 2)*9)
	packetWidth := barLength + 4
	first := max(0, width/10)
	last := max(first, width-packetWidth-max(1, width/14))
	progress := float64(position-squareRunnerStartFrame) /
		float64(squareCycleFrames-squareRunnerStartFrame-1)
	progress = smoothstep(math.Max(0, math.Min(1, progress)))
	left := first + int(math.Round(progress*float64(last-first)))
	return squareRunner{left: left, barLength: barLength}, true
}

func applySquareRunner(levels []bool, runner squareRunner) {
	packetWidth := runner.barLength + 4
	for index := range packetWidth {
		target := runner.left + index
		if target >= 0 && target < len(levels) {
			levels[target] = index >= 2 && index < runner.barLength+2
		}
	}
}

func revealSquareSegments(segments []squareSegment, revealed int, leftToRight bool) {
	if revealed >= len(segments) {
		return
	}
	if revealed < 0 {
		revealed = 0
	}
	if leftToRight {
		clear(segments[revealed:])
	} else {
		clear(segments[:len(segments)-revealed])
	}
}

func auroraSoundArt(width int, phase int) string {
	return auroraSoundArtWithSnapshot(
		width,
		phase,
		scaleAudioSnapshot(currentAudioSnapshot(), audioSettings.Motion),
	)
}

const (
	auroraSoundNeedleThreshold     = 0.82
	auroraSoundHardNeedleThreshold = 0.94
)

func auroraSoundArtWithSnapshot(width int, _ int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if !audio.Active {
		return strings.Repeat(string(auroraBars[0]), width)
	}

	chars := make([]rune, width)
	for index := range width {
		position := 0.0
		if width > 1 {
			position = float64(index) / float64(width-1)
		}
		bandPosition := math.Pow(position, 0.72) * float64(audioBandCount-1)
		energy := interpolatedAudioBand(audio.Bands, bandPosition)
		level := energy*0.92 + audio.Level*0.08
		level = math.Max(0, math.Min(1, level))

		switch {
		case energy >= auroraSoundHardNeedleThreshold:
			chars[index] = '┃'
		case energy >= auroraSoundNeedleThreshold:
			chars[index] = '╿'
		default:
			chars[index] = rampPick(auroraBars, level)
		}
	}
	return string(chars)
}

func scaleAudioSnapshot(snapshot audioSnapshot, scale float64) audioSnapshot {
	snapshot.Level = math.Min(1, snapshot.Level*scale)
	for index := range snapshot.Bands {
		snapshot.Bands[index] = math.Min(1, snapshot.Bands[index]*scale)
	}
	return snapshot
}

func interpolatedAudioBand(bands [audioBandCount]float64, position float64) float64 {
	position = math.Max(0, math.Min(float64(audioBandCount-1), position))
	left := int(math.Floor(position))
	right := min(audioBandCount-1, left+1)
	progress := position - float64(left)
	return bands[left] + (bands[right]-bands[left])*progress
}

func ripplesArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{" · ", "╴●╶", "─ ·", "   "})
	}

	chars := make([]rune, width)
	levels := make([]float64, width)
	for index := range chars {
		chars[index] = ' '
	}

	events := organicEvents("ripples", 1, float64(phase), 24, 26, 42)
	events = append(events, organicEvents("ripples", 2, float64(phase), 39, 30, 52)...)
	for _, event := range events {
		center := eventRandom("ripples", 5, event.Index, 1) * float64(width-1)
		speed := 0.42 + eventRandom("ripples", 5, event.Index, 2)*0.52
		radius := event.Progress * speed * float64(width) * 0.72
		thickness := 1.1 + eventRandom("ripples", 5, event.Index, 3)*1.9
		for index := range width {
			distance := math.Abs(float64(index) - center)
			ring := math.Exp(-math.Pow((distance-radius)/thickness, 2)) * event.Envelope
			if event.Progress < 0.12 {
				impact := math.Exp(-math.Pow(distance/2.2, 2)) * (1 - event.Progress/0.12)
				ring = math.Max(ring, impact)
			}
			levels[index] = math.Max(levels[index], ring)
		}
	}

	for index, level := range levels {
		switch {
		case level > 0.88:
			chars[index] = '●'
		case level > 0.68:
			chars[index] = '═'
		case level > 0.43:
			chars[index] = '─'
		case level > 0.20:
			if index%2 == 0 {
				chars[index] = '╴'
			} else {
				chars[index] = '╶'
			}
		case organicNoise("ripples", uint64(100+index), float64(phase)/19) > 0.975:
			chars[index] = '·'
		}
	}
	return string(chars)
}

func bloomArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{" · ", "╴⌁ ", "⌁•╶", "❧⌁✦", " ⌁ "})
	}

	chars := make([]rune, width)
	for index := range chars {
		chars[index] = ' '
	}

	events := organicEvents("bloom", 1, float64(phase), 58, 72, 108)
	events = append(events, organicEvents("bloom", 2, float64(phase)+31, 83, 82, 126)...)
	for _, event := range events {
		origin := int(eventRandom("bloom", 4, event.Index, 1) * float64(width-1))
		direction := 1
		if eventRandom("bloom", 4, event.Index, 2) < 0.5 {
			direction = -1
		}
		reach := int((0.16 + eventRandom("bloom", 4, event.Index, 3)*0.38) * float64(width))
		growth := smoothstep(math.Min(1, event.Progress/0.68))
		if event.Progress > 0.78 {
			growth *= 1 - smoothstep((event.Progress-0.78)/0.22)
		}
		tip := max(1, int(float64(reach)*growth))
		for step := 0; step <= tip; step++ {
			index := origin + step*direction
			if index < 0 || index >= width {
				break
			}
			position := float64(step) / float64(max(1, tip))
			switch {
			case step == tip && event.Progress > 0.38 && event.Envelope > 0.48:
				chars[index] = []rune{'✦', '✧', '•'}[int(eventRandom("bloom", 5, event.Index, 4)*3)%3]
			case step > 2 && (step+int(event.Index))%9 == 0 && event.Envelope > 0.36:
				chars[index] = '❧'
			case step > 1 && (step+int(event.Index))%5 == 0:
				chars[index] = '⌁'
			case position > 0.82:
				chars[index] = '╴'
			default:
				chars[index] = '─'
			}
		}
	}

	for index := range chars {
		if chars[index] == ' ' && organicNoise("bloom", uint64(200+index), float64(phase)/35) > 0.988 {
			chars[index] = '·'
		}
	}
	return string(chars)
}

func ribbonArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"░▒▓", "▒▓█", "▓█▓", "█▓▒", "▓▒░"})
	}

	chars := make([]rune, width)
	timeBase := float64(phase)*0.083 + signedOrganicNoise("ribbon", 1, float64(phase)/67)*1.5
	frequency := 0.14 + organicNoise("ribbon", 2, float64(phase)/91)*0.07
	sheens := organicEvents("ribbon", 3, float64(phase), 43, 16, 28)
	ramp := []rune("·░▒▓█")
	for index := range chars {
		x := float64(index)
		fold := math.Sin(x*frequency+timeBase) + 0.36*math.Sin(x*frequency*0.43-timeBase*0.72)
		crossFold := math.Sin(x*frequency*1.83 - timeBase*0.41 +
			signedOrganicNoise("ribbon", 4, float64(phase)/79)*0.8)
		level := 0.12 + 0.58*(fold+1.36)/2.72 + 0.18*(crossFold+1)/2
		glyph := rampPick(ramp, level)
		for _, event := range sheens {
			center := eventRandom("ribbon", 3, event.Index, 4) * float64(width-1)
			if math.Abs(x-center) < 0.7 && event.Envelope > 0.48 {
				glyph = '✦'
			} else if math.Abs(x-center) < 2.4 && event.Envelope > 0.34 {
				glyph = '█'
			}
		}
		chars[index] = glyph
	}
	return string(chars)
}

func shutterArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"███", "▶▓◀", "▶░◀", "▶ ◀", " │ "})
	}

	chars := make([]rune, width)
	center := float64(width-1) / 2
	aperture := 0.10 + organicNoise("shutter", 1, float64(phase)/24)*0.72
	for _, event := range organicEvents("shutter", 2, float64(phase), 39, 12, 24) {
		if eventRandom("shutter", 2, event.Index, 2) > 0.5 {
			aperture = math.Max(aperture, event.Envelope*0.92)
		} else {
			aperture = math.Min(aperture, 1-event.Envelope*0.94)
		}
	}
	gapRadius := aperture * center
	leftEdge := int(math.Floor(center - gapRadius))
	rightEdge := int(math.Ceil(center + gapRadius))
	for index := range chars {
		distance := math.Abs(float64(index) - center)
		switch {
		case index == leftEdge:
			chars[index] = '▶'
		case index == rightEdge:
			chars[index] = '◀'
		case distance < gapRadius:
			if (index+phase/5)%9 == 0 {
				chars[index] = '│'
			} else {
				chars[index] = ' '
			}
		default:
			panelDepth := math.Min(1, (distance-gapRadius)/math.Max(2, center*0.34))
			if (index+phase/7)%7 == 0 {
				chars[index] = '│'
			} else {
				chars[index] = rampPick([]rune("░▒▓█"), 1-panelDepth*0.68)
			}
		}
	}
	return string(chars)
}

func isLegacyBundledFrameAnimation(name string, animation FrameAnimation) bool {
	if !animation.Fill {
		return false
	}
	var legacyFrames []string
	switch name {
	case "ribbon":
		legacyFrames = []string{
			"··░░▒▒▓▓▒▒░░··  ",
			"·░░▒▒▓▓▒▒░░··  ·",
			"░░▒▒▓▓▒▒░░··  ··",
			"░▒▒▓▓▒▒░░··  ··░",
			"▒▒▓▓▒▒░░··  ··░░",
			"▒▓▓▒▒░░··  ··░░▒",
			"▓▓▒▒░░··  ··░░▒▒",
			"▓▒▒░░··  ··░░▒▒▓",
		}
	case "shutter":
		legacyFrames = []string{
			"░░▒▒▓▓██▓▓▒▒░░··",
			"·░░▒▒▓▓██▓▓▒▒░░·",
			"··░░▒▒▓▓██▓▓▒▒░░",
			"░··░░▒▒▓▓██▓▓▒▒░",
		}
	default:
		return false
	}
	return slices.Equal(animation.Frames, legacyFrames)
}

func glitchArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"───", "╍╪─", "░╳▓", "─┄╴"})
	}

	chars := make([]rune, width)
	for index := range chars {
		switch {
		case (index+phase/7)%29 == 0:
			chars[index] = '╍'
		case (index-phase/11)%17 == 0:
			chars[index] = '┄'
		default:
			chars[index] = '─'
		}
	}

	tiles := []rune{'╴', '╶', '╪', '╳', '░', '▒', '▓'}
	events := organicEvents("glitch", 1, float64(phase), 33, 5, 13)
	events = append(events, organicEvents("glitch", 2, float64(phase)+17, 57, 4, 9)...)
	for _, event := range events {
		center := int(eventRandom("glitch", 4, event.Index, 1) * float64(width-1))
		radius := 2 + int(eventRandom("glitch", 4, event.Index, 2)*float64(max(3, width/8)))
		shift := int(eventRandom("glitch", 4, event.Index, 3)*7) - 3
		for index := max(0, center-radius); index <= min(width-1, center+radius); index++ {
			edge := math.Abs(float64(index-center)) / float64(max(1, radius))
			if event.Envelope*(1-edge*0.62) < animationRandom("glitch", uint64(100+event.Index), int64(index)) {
				continue
			}
			tileIndex := abs(index+shift+int(event.Index)) % len(tiles)
			chars[index] = tiles[tileIndex]
		}
	}
	return string(chars)
}
