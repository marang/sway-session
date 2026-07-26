package main

import (
	"math"
	"slices"
	"strings"
	"time"
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
		currentSoundSnapshot(),
	)
}

const auroraSoundOnsetLifetime = 420 * time.Millisecond

func auroraSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if !audio.Active {
		return string(fitRunes(auroraArt(width, phase), width))
	}

	chars := make([]rune, width)
	onset, hasOnset := newestSoundOnset(audio, audioRegionGeneral)
	onsetEnvelope := 0.0
	onsetCenter := 0.0
	if hasOnset && onset.Age >= 0 && onset.Age < auroraSoundOnsetLifetime {
		progress := float64(onset.Age) / float64(auroraSoundOnsetLifetime)
		onsetEnvelope = onset.Strength * (1 - smoothstep(progress))
		onsetCenter = (math.Max(-1, math.Min(1, onset.Position)) + 1) *
			0.5 * float64(width-1)
	}

	for index := range width {
		position := 0.0
		if width > 1 {
			position = float64(index) / float64(width-1)
		}
		bandPosition := math.Pow(position, 0.72) * float64(audioBandCount-1)
		energy := interpolatedAudioBand(audio.Bands, bandPosition)
		motion := signedOrganicNoise(
			"aurora_sound",
			uint64(100+index/3),
			float64(phase)/22+position*0.7,
		)
		level := energy*0.92 + audio.Level*0.08 +
			motion*(0.045+energy*0.09)
		if onsetEnvelope > 0 {
			distance := math.Abs(float64(index) - onsetCenter)
			spread := math.Max(2.0, float64(width)*0.045)
			localLift := math.Exp(-math.Pow(distance/spread, 2)) *
				onsetEnvelope * 0.20
			level += localLift
		}
		level = math.Max(0, math.Min(1, level))
		chars[index] = rampPick(auroraBars, level)
	}

	return string(chars)
}

func spectrumSoundArt(width int, phase int) string {
	return spectrumSoundArtWithSnapshot(
		width,
		phase,
		currentSoundSnapshot(),
	)
}

func spectrumSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return spectrumSoundSmallArt(width, phase, audio)
	}

	chars := make([]rune, width)
	for index := range chars {
		chars[index] = '·'
	}
	chars[0] = '⟨'
	chars[width-1] = '⟩'
	center := width / 2
	centerLeft := center
	if width%2 == 0 {
		centerLeft--
	}
	for index := centerLeft; index <= center; index++ {
		chars[index] = '┃'
	}

	maxRadius := max(1, min(centerLeft-1, width-center-2))
	if !audio.Active {
		idlePhase := float64(phase)*0.025 +
			signedOrganicNoise("spectrum_sound", 2, float64(phase)/167)*2.2 +
			signedOrganicNoise("spectrum_sound", 3, 0.29)*math.Pi
		breath := 0.5 + 0.5*math.Sin(idlePhase)
		pulseRadius := 1 + int(math.Round(breath*float64(min(3, maxRadius-1))))
		for radius := 1; radius <= maxRadius; radius++ {
			left, right := spectrumPairPositions(width, center, radius)
			if radius == pulseRadius {
				chars[left], chars[right] = '━', '━'
			} else if math.Abs(float64(radius-pulseRadius)) <= 1 {
				chars[left], chars[right] = '─', '─'
			}
		}
		return string(chars)
	}

	focusRadius := 1 + int(math.Round((1-audio.Centroid)*float64(max(1, maxRadius-1))))
	organicPhase := float64(phase)*0.012 +
		signedOrganicNoise("spectrum_sound", 1, float64(phase)/181)*0.35
	for radius := 1; radius <= maxRadius; radius++ {
		left, right := spectrumPairPositions(width, center, radius)
		radialPosition := float64(radius-1) / float64(max(1, maxRadius-1))
		bandPosition := (1 - radialPosition) * float64(audioBandCount-1)
		energy := interpolatedAudioBand(audio.Bands, bandPosition)
		organic := 0.5 + 0.5*math.Sin(float64(radius)*0.31+organicPhase)
		focus := math.Exp(-math.Pow(float64(radius-focusRadius)/math.Max(1.5, float64(maxRadius)*0.08), 2))
		level := energy*0.75 + audio.Level*0.10 + organic*0.08 + focus*audio.Peak*0.07
		level = math.Max(0, math.Min(1, level))

		glyph := rampPick(spectrumBars, level)
		switch {
		case level < 0.16:
			glyph = '·'
		case level < 0.30:
			glyph = '─'
		case level < 0.48:
			glyph = '━'
		}
		chars[left], chars[right] = glyph, glyph
	}
	if audio.Peak >= 0.70 {
		left, right := spectrumPairPositions(width, center, min(maxRadius, focusRadius))
		chars[left], chars[right] = '┃', '┃'
	}
	return string(chars)
}

func spectrumSoundSmallArt(width int, phase int, audio audioSnapshot) string {
	chars := make([]rune, width)
	level := audio.Level
	if !audio.Active {
		level = 0.18 + 0.08*(0.5+0.5*math.Sin(float64(phase)*0.025))
	}
	for index := range chars {
		chars[index] = rampPick(spectrumBars, level)
	}
	center := width / 2
	chars[center] = '┃'
	if width%2 == 0 {
		chars[center-1] = '┃'
	}
	if width >= 3 {
		chars[0], chars[width-1] = '⟨', '⟩'
	}
	return string(chars)
}

func spectrumPairPositions(width int, center int, radius int) (int, int) {
	left := center - radius
	right := center + radius
	if width%2 == 0 {
		left--
	}
	return left, right
}

func waveSoundArt(width int, phase int) string {
	return waveSoundArtWithSnapshot(
		width,
		phase,
		currentSoundSnapshot(),
	)
}

func waveSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	chars := fitRunes(waveArt(width, phase), width)
	if !audio.Active {
		return string(chars)
	}

	waveRamp := []rune("▁▂▃▅▇█")
	surgeOnset, hasSurge := newestSoundOnset(audio, audioRegionGeneral)
	surgeCenter, surgeSpread, surgeEnvelope, surgeLive := waveSoundSurge(width, surgeOnset)
	hasSurge = hasSurge && surgeLive
	for index := range chars {
		x := float64(index)
		position := float64(index) / float64(max(1, width-1))
		band := interpolatedAudioBand(
			audio.Bands,
			position*float64(audioBandCount-1),
		)
		if rampIndex := slices.Index(waveRamp, chars[index]); rampIndex >= 0 {
			lift := audio.Bass*0.34 + audio.LowMid*0.18 + band*0.48
			if hasSurge {
				lift += surgeEnvelope *
					math.Exp(-math.Pow((x-surgeCenter)/surgeSpread, 2))
			}
			rampIndex = min(len(waveRamp)-1, rampIndex+int(math.Round(lift*2)))
			chars[index] = waveRamp[rampIndex]
		}
		if audio.HighMid > 0.34 &&
			strings.ContainsRune("▇█◜◝◞◟", chars[index]) &&
			(index+phase/7)%max(5, 11-int(math.Round(audio.HighMid*6))) == 0 {
			chars[index] = '≈'
		}
		if audio.Treble > 0.30 && chars[index] != ' ' &&
			(index+phase/9)%max(11, 23-int(math.Round(audio.Treble*10))) == 0 {
			chars[index] = '·'
		}
		if hasSurge {
			surge := surgeEnvelope *
				math.Exp(-math.Pow((x-surgeCenter)/surgeSpread, 2))
			if chars[index] == ' ' && surge > 0.28 {
				chars[index] = rampPick(waveRamp, (surge-0.28)*0.82)
			} else if surge > 0.68 && strings.ContainsRune("▇█", chars[index]) &&
				(index+phase/11)%5 == 0 {
				chars[index] = '≈'
			}
		}
	}
	if hasSurge && surgeEnvelope > 0.50 {
		centerIndex := max(0, min(width-1, int(math.Round(surgeCenter))))
		chars[centerIndex] = '≋'
	}
	return string(chars)
}

func waveSoundSurge(width int, onset audioOnset) (float64, float64, float64, bool) {
	if width <= 0 || onset.Region != audioRegionGeneral || onset.Age < 0 {
		return 0, 0, 0, false
	}
	const lifetime = 720 * time.Millisecond
	if onset.Age >= lifetime {
		return 0, 0, 0, false
	}
	progress := float64(onset.Age) / float64(lifetime)
	stereo := math.Max(-1, math.Min(1, onset.Position))
	center := (0.5 + stereo*0.34) * float64(max(0, width-1))
	spread := math.Max(3.0, float64(width)*0.045) * (1 + progress*0.55)
	envelope := onset.Strength * (1 - smoothstep(progress))
	return center, spread, envelope, true
}

func scaleAudioSnapshot(snapshot audioSnapshot, scale float64) audioSnapshot {
	snapshot.Level = math.Min(1, snapshot.Level*scale)
	snapshot.Bass = math.Min(1, snapshot.Bass*scale)
	snapshot.LowMid = math.Min(1, snapshot.LowMid*scale)
	snapshot.HighMid = math.Min(1, snapshot.HighMid*scale)
	snapshot.Treble = math.Min(1, snapshot.Treble*scale)
	snapshot.LeftLevel = math.Min(1, snapshot.LeftLevel*scale)
	snapshot.RightLevel = math.Min(1, snapshot.RightLevel*scale)
	snapshot.SpectralFlux = math.Min(1, snapshot.SpectralFlux*scale)
	snapshot.Peak = math.Min(1, snapshot.Peak*scale)
	for index := range snapshot.Bands {
		snapshot.Bands[index] = math.Min(1, snapshot.Bands[index]*scale)
	}
	for index := range min(snapshot.OnsetCount, len(snapshot.Onsets)) {
		snapshot.Onsets[index].Strength = math.Min(1, snapshot.Onsets[index].Strength*scale)
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

	levels := ripplesBaseLevels(width, phase)
	return ripplesLevelsArt(levels, phase)
}

func ripplesBaseLevels(width int, phase int) []float64 {
	levels := make([]float64, width)
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
	return levels
}

func ripplesLevelsArt(levels []float64, phase int) string {
	chars := make([]rune, len(levels))
	for index := range chars {
		chars[index] = ' '
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
	timeBase := float64(phase)*0.068 +
		signedOrganicNoise("ribbon", 1, float64(phase)/67)*1.35
	frequency := 0.105 + organicNoise("ribbon", 2, float64(phase)/91)*0.055
	sheens := organicEvents("ribbon", 3, float64(phase), 43, 16, 28)
	ramp := []rune("·░▒▓█")
	for index := range chars {
		x := float64(index)
		warp := signedOrganicNoise(
			"ribbon",
			uint64(10+index%9),
			float64(phase)/83+float64(index)*0.012,
		) * 0.55
		foldAngle := x*frequency + timeBase + warp
		fold := math.Sin(foldAngle) +
			0.42*math.Sin(x*frequency*0.47-timeBase*0.61)
		crossFold := math.Sin(x*frequency*1.83 - timeBase*0.41 +
			signedOrganicNoise("ribbon", 4, float64(phase)/79)*0.8)
		level := 0.08 + 0.66*(fold+1.42)/2.84 + 0.20*(crossFold+1)/2
		facing := math.Abs(math.Sin(foldAngle))
		glyph := rampPick(ramp, level)
		if facing < 0.105 {
			if math.Cos(foldAngle) >= 0 {
				glyph = '◐'
			} else {
				glyph = '◑'
			}
		} else if facing < 0.20 {
			glyph = '◒'
		}
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

	center := float64(width-1) / 2
	aperture := 0.10 + organicNoise("shutter", 1, float64(phase)/24)*0.72
	for _, event := range organicEvents("shutter", 2, float64(phase), 39, 12, 24) {
		if eventRandom("shutter", 2, event.Index, 2) > 0.5 {
			aperture = math.Max(aperture, event.Envelope*0.92)
		} else {
			aperture = math.Min(aperture, 1-event.Envelope*0.94)
		}
	}
	center += signedOrganicNoise("shutter", 5, float64(phase)/118) *
		math.Min(4, float64(width)*0.035)
	gapRadius := aperture * center
	return shutterFrame(width, phase, center, gapRadius, 1-aperture, 0, 0)
}

func shutterFrame(
	width int,
	phase int,
	center float64,
	gapRadius float64,
	closure float64,
	treble float64,
	peak float64,
) string {
	chars := make([]rune, width)
	leftEdge := max(0, int(math.Floor(center-gapRadius)))
	rightEdge := min(width-1, int(math.Ceil(center+gapRadius)))
	seamIndex := max(0, min(width-1, int(math.Round(center))))
	scanOffset := math.Sin(float64(phase)*0.041) * gapRadius * 0.62
	scanIndex := max(leftEdge+1, min(rightEdge-1, int(math.Round(center+scanOffset))))
	bladeSpan := max(4, width/17)
	heavy := peak >= 0.68
	edgeLeft, edgeRight, bladeLeft, bladeRight := '›', '‹', '▷', '◁'
	seam := '┆'
	if heavy {
		edgeLeft, edgeRight, bladeLeft, bladeRight = '▶', '◀', '▶', '◀'
		seam = '┃'
	}

	for index := range chars {
		switch {
		case index == leftEdge:
			chars[index] = edgeLeft
		case index == rightEdge:
			chars[index] = edgeRight
		case index == seamIndex:
			chars[index] = seam
		case index > leftEdge && index < rightEdge:
			focusDistance := math.Abs(math.Abs(float64(index)-center) - gapRadius*0.46)
			switch {
			case index == scanIndex:
				chars[index] = '│'
			case focusDistance < 0.55:
				chars[index] = '·'
			default:
				chars[index] = ' '
			}
		default:
			distanceFromEdge := leftEdge - index
			leftPanel := true
			if index > rightEdge {
				distanceFromEdge = index - rightEdge
				leftPanel = false
			}
			bladePosition := distanceFromEdge % bladeSpan
			switch {
			case bladePosition == 0:
				if leftPanel {
					chars[index] = bladeLeft
				} else {
					chars[index] = bladeRight
				}
			case bladePosition == bladeSpan/2:
				chars[index] = '│'
			case treble > 0.48 &&
				(index+phase/6)%max(7, 18-int(math.Round(treble*8))) == 0:
				chars[index] = '✦'
			default:
				bladeWave := 0.5 + 0.5*math.Cos(
					float64(bladePosition)/float64(bladeSpan)*2*math.Pi,
				)
				level := 0.22 + closure*0.34 + bladeWave*0.42
				chars[index] = rampPick([]rune("░▒▓█"), level)
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
