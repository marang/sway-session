package main

import (
	"math"
)

func artWidth(width int) int {
	return max(0, min(width, settings.MaxArtColumns))
}

func shortFrame(width int, phase int, frames []string) string {
	key := "short:" + frames[0]
	drift := int(math.Round(signedOrganicNoise(key, 1, float64(phase)/17) * float64(len(frames)) * 1.6))
	offset := int(animationRandom(key, 2, 0) * float64(len(frames)))
	frameIndex := (phase + drift + offset) % len(frames)
	if frameIndex < 0 {
		frameIndex += len(frames)
	}
	runes := []rune(frames[frameIndex])
	if len(runes) > width {
		runes = runes[:width]
	}
	return string(runes)
}

func pseudoRandom(index int, phase int, salt float64) float64 {
	value := math.Sin(float64(index+1)*12.9898+(float64(phase)+salt)*78.233) * 43758.5453
	return value - math.Floor(value)
}

func rampPick(ramp []rune, level float64) rune {
	level = math.Max(0.0, math.Min(1.0, level))
	return ramp[min(len(ramp)-1, int(level*float64(len(ramp))))]
}

func auroraArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"▁▂▃", "▂▃▄", "▃▄▅", "▄▅▆", "▅▆▇", "▆▇█", "▇█▇", "█▇▆"})
	}

	chars := make([]rune, 0, width)
	timeBase := float64(phase)*0.022 + signedOrganicNoise("aurora", 1, float64(phase)/86)*0.72
	sparkEvents := organicEvents("aurora", 2, float64(phase), 41, 10, 22)

	for index := range width {
		offset := animationRandom("aurora", 3, int64(index))
		rise := math.Mod(timeBase+offset, 1.0)
		// Grow each column upward, then let it settle softly before the next lift.
		lift := 0.0
		if rise < 0.74 {
			lift = smoothstep(rise / 0.74)
		} else {
			lift = 1.0 - smoothstep((rise-0.74)/0.26)*0.82
		}
		swellPhase := float64(phase)*0.018 + signedOrganicNoise("aurora", uint64(20+index%7), float64(phase)/63)*0.9
		swell := (math.Sin(float64(index)*0.19+swellPhase) + 1.0) * 0.5
		breath := organicNoise("aurora", uint64(40+index%13), float64(phase)/92+float64(index)*0.03)
		level := 0.12 + 0.64*lift + 0.16*swell + 0.08*breath
		level = math.Max(0.0, math.Min(1.0, level))
		glyph := rampPick(auroraBars, level)
		for _, event := range sparkEvents {
			center := eventRandom("aurora", 2, event.Index, 4) * float64(width-1)
			distance := math.Abs(float64(index) - center)
			if distance < 0.65 && event.Envelope > 0.62 {
				glyph = auroraSparkles[int(eventRandom("aurora", 2, event.Index, 5)*float64(len(auroraSparkles)))%len(auroraSparkles)]
			} else if distance < 2.2 && event.Envelope > 0.34 {
				glyph = auroraDots[abs(index+int(event.Index))%len(auroraDots)]
			}
		}
		chars = append(chars, glyph)
	}
	return string(chars)
}

func spectrumArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"⟨█⟩", "(▓)", "[▒]", "{░}", "<·>"})
	}

	chars := make([]rune, width)
	for index := range chars {
		chars[index] = rampPick(shadeRamp, 0.15+0.08*math.Sin(float64(index)*0.55+float64(phase)*0.03))
	}
	center := width / 2
	if width%2 == 0 {
		chars[center-1] = '┃'
		chars[center] = '┃'
	} else {
		chars[center] = '┃'
	}

	maxRadius := max(1, width/2-1)
	warpedPhase := float64(phase) + signedOrganicNoise("spectrum", 1, float64(phase)/72)*17
	outerPulse := 0.52 + 0.48*math.Sin(warpedPhase*0.051)
	innerPulse := 0.50 + 0.50*math.Sin(warpedPhase*0.079+1.7)
	for _, event := range organicEvents("spectrum", 2, float64(phase), 31, 14, 26) {
		if eventRandom("spectrum", 2, event.Index, 2) > 0.5 {
			outerPulse = math.Max(outerPulse, event.Envelope)
		} else {
			innerPulse = math.Max(innerPulse, event.Envelope)
		}
	}
	outerRadius := int(float64(maxRadius) * (0.36 + 0.56*outerPulse))
	innerRadius := int(float64(maxRadius) * (0.10 + 0.42*innerPulse))

	for radius := 1; radius <= maxRadius; radius++ {
		left := center - radius - 1
		right := center + radius
		if width%2 != 0 {
			left = center - radius
			right = center + radius
		}
		if left < 0 || right >= width {
			continue
		}

		level := (math.Sin(float64(radius)*0.48+warpedPhase*0.11) + 1.0) * 0.5
		level = math.Max(level*0.58, math.Exp(-math.Pow(float64(radius-outerRadius)/math.Max(2.0, float64(width)*0.045), 2)))
		level = math.Max(level, math.Exp(-math.Pow(float64(radius-innerRadius)/math.Max(2.0, float64(width)*0.032), 2))*0.92)

		switch {
		case radius == outerRadius || radius == innerRadius:
			pairOffset := int(organicNoise("spectrum", 3, float64(phase)/13) * float64(min(len(spectrumLeft), len(spectrumRight))))
			pair := (pairOffset + radius) % min(len(spectrumLeft), len(spectrumRight))
			chars[left] = spectrumLeft[pair]
			chars[right] = spectrumRight[pair]
		case level > 0.76:
			chars[left] = rampPick(spectrumBars, level)
			chars[right] = rampPick(spectrumBars, level)
		case level > 0.58:
			chars[left] = '━'
			chars[right] = '━'
		case level > 0.42:
			chars[left] = '─'
			chars[right] = '─'
		default:
			chars[left] = rampPick(shadeRamp, level*0.42)
			chars[right] = rampPick(shadeRamp, level*0.42)
		}
	}
	return string(chars)
}

func radarArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"╶◜╴", "─◠─", "╶◝╴", "╶◞╴", "─◡─", "╶◟╴"})
	}

	span := float64(width)
	headClock := float64(phase)*1.05 + signedOrganicNoise("radar", 1, float64(phase)/54)*7
	echoClock := float64(phase)*0.48 + signedOrganicNoise("radar", 2, float64(phase)/71)*11
	secondaryClock := float64(phase)*0.36 + signedOrganicNoise("radar", 3, float64(phase)/93)*9
	head := math.Mod(headClock+span*100, span)
	echo := math.Mod(echoClock+span*0.57+span*100, span)
	secondary := math.Mod(span-1-secondaryClock+span*100, span)
	contacts := organicEvents("radar", 4, float64(phase), 29, 18, 34)
	chars := make([]rune, 0, width)
	for index := range width {
		pulse := math.Max(0.0, 1.0-wrappedDistance(float64(index), head, span)/6.0)
		echoPulse := math.Max(0.0, 1.0-wrappedDistance(float64(index), echo, span)/9.0) * 0.64
		secondaryPulse := math.Max(0.0, 1.0-wrappedDistance(float64(index), secondary, span)/13.0) * 0.42
		scanline := math.Sin(float64(index)*0.29 - float64(phase)*0.10)
		level := math.Max(pulse, math.Max(echoPulse, secondaryPulse))
		contact := false
		for _, event := range contacts {
			position := eventRandom("radar", 4, event.Index, 2) * span
			if wrappedDistance(float64(index), position, span) < 0.65 && event.Envelope > 0.42 {
				contact = true
				level = 1
			}
		}
		switch {
		case contact:
			chars = append(chars, '◆')
		case wrappedDistance(float64(index), head, span) < 0.55:
			chars = append(chars, radarSweep[(phase/3)%len(radarSweep)])
		case index%11 == 0:
			chars = append(chars, '╋')
		case level > 0.72:
			chars = append(chars, rampPick(radarLevels, level))
		case level > 0.38:
			chars = append(chars, '═')
		case scanline > 0.72:
			chars = append(chars, '─')
		case scanline > 0.35:
			chars = append(chars, '┄')
		default:
			chars = append(chars, ' ')
		}
	}
	return string(chars)
}

func constellationArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{" ✦ ", "  ✧", "·  ", " ✶ ", "  ·"})
	}

	chars := make([]rune, 0, width)
	drift := signedOrganicNoise("constellation", 1, float64(phase)/68) * float64(width) * 0.10
	clusters := organicEvents("constellation", 2, float64(phase), 47, 16, 31)
	for index := range width {
		n := animationRandom("constellation", 10, int64(index))
		shimmer := organicNoise("constellation", uint64(100+index), float64(phase)/6.5)
		lane := math.Sin((float64(index)+drift)*0.11) +
			math.Sin(float64(index)*0.037-float64(phase)*0.041+signedOrganicNoise("constellation", 3, float64(phase)/81))
		clusterStrength := 0.0
		for _, event := range clusters {
			center := eventRandom("constellation", 2, event.Index, 3) * float64(width-1)
			clusterStrength = math.Max(clusterStrength, event.Envelope*math.Exp(-math.Pow((float64(index)-center)/6.5, 2)))
		}
		switch {
		case (n > 0.82 && shimmer > 0.89 && lane > 0.08) || clusterStrength > 0.72:
			starIndex := int(animationRandom("constellation", 11, int64(index))*float64(len(constellationStar))) % len(constellationStar)
			chars = append(chars, constellationStar[starIndex])
		case (n > 0.90 || clusterStrength > 0.46) && lane > -0.28:
			chars = append(chars, '•')
		case n > 0.82 && shimmer > 0.46 && lane > 0.22:
			chars = append(chars, '·')
		case index > 0 && index < width-1 && clusterStrength > 0.22:
			chars = append(chars, '╴')
		default:
			chars = append(chars, ' ')
		}
	}
	return string(chars)
}

func circuitArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"╍╾╼", "─╪═", "═●═", "╼╾╍"})
	}

	chars := make([]rune, 0, width)
	span := float64(max(1, width))
	gateA := int(math.Mod(float64(phase)*0.9+signedOrganicNoise("circuit", 1, float64(phase)/47)*8+span*100, span))
	gateB := width - 1 - int(math.Mod(float64(phase)*0.56+signedOrganicNoise("circuit", 2, float64(phase)/63)*12+span*100, span))
	gateC := int(math.Mod(float64(phase)*0.31+span*0.38+signedOrganicNoise("circuit", 3, float64(phase)/79)*15+span*100, span))
	surges := organicEvents("circuit", 4, float64(phase), 36, 12, 24)
	for index := range width {
		nearGate := min(abs(index-gateA), min(abs(index-gateB), abs(index-gateC)))
		surge := 0.0
		for _, event := range surges {
			center := eventRandom("circuit", 4, event.Index, 4) * float64(width-1)
			surge = math.Max(surge, event.Envelope*math.Exp(-math.Pow((float64(index)-center)/5.5, 2)))
		}
		switch {
		case nearGate == 0:
			chars = append(chars, '●')
		case nearGate <= 2:
			tile := int(organicNoise("circuit", uint64(20+index), float64(phase)/9)*float64(len(circuitTiles))) % len(circuitTiles)
			chars = append(chars, circuitTiles[tile])
		case surge > 0.76:
			chars = append(chars, '╪')
		case surge > 0.42:
			chars = append(chars, '═')
		case (index+phase/4)%19 == 0:
			chars = append(chars, '╪')
		case (index-phase/3)%13 == 0:
			chars = append(chars, '═')
		case (index+phase)%7 == 0:
			chars = append(chars, '╍')
		case (index+phase)%4 == 0:
			chars = append(chars, '┄')
		default:
			chars = append(chars, '─')
		}
	}
	return string(chars)
}

func braidArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"╱╲╱", "╲╱╲", "╱╳╲", "╲╳╱"})
	}

	chars := make([]rune, 0, width)
	tightness := 0.49 + organicNoise("braid", 1, float64(phase)/74)*0.18
	timeA := float64(phase)*0.12 + signedOrganicNoise("braid", 2, float64(phase)/53)*1.8
	timeB := float64(phase)*0.10 + signedOrganicNoise("braid", 3, float64(phase)/67)*2.1
	knots := organicEvents("braid", 4, float64(phase), 43, 16, 30)
	for index := range width {
		waveA := math.Sin(float64(index)*tightness + timeA)
		waveB := math.Sin(float64(index)*tightness - timeB + math.Pi)
		cross := math.Abs(waveA - waveB)
		for _, event := range knots {
			center := eventRandom("braid", 4, event.Index, 2) * float64(width-1)
			if math.Abs(float64(index)-center) < 2.5+event.Envelope*4 {
				cross *= 1 - event.Envelope*0.72
			}
		}
		switch {
		case cross < 0.16:
			chars = append(chars, '╳')
		case waveA > waveB:
			chars = append(chars, '╱')
		default:
			chars = append(chars, '╲')
		}
	}
	return string(chars)
}

func loomArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"≈⌁░", "░≋▒", "▒⌁▓", "▓✦▒", "▒≋░", "░⌁≈"})
	}

	chars := make([]rune, 0, width)
	t := float64(phase)*0.041 + signedOrganicNoise("loom", 1, float64(phase)/83)*1.3
	knotA := float64(width) * (0.12 + 0.28*organicNoise("loom", 2, float64(phase)/69))
	knotB := float64(width) * (0.39 + 0.28*organicNoise("loom", 3, float64(phase)/91))
	knotC := float64(width) * (0.68 + 0.25*organicNoise("loom", 4, float64(phase)/77))

	for index := range width {
		x := float64(index)
		warp := math.Sin(x*0.17+t) + math.Sin(x*0.043-t*0.82+math.Sin(t*0.31)*2.2)
		weft := math.Sin(x*0.31-t*1.17) * math.Cos(x*0.071+t*0.47)
		moire := (math.Sin(warp*1.7+weft*1.1) + 1.0) * 0.5
		softGrain := organicNoise("loom", uint64(100+index), float64(phase)/8.5) * 0.11
		level := 0.12 + moire*0.54 + softGrain

		knot := math.Exp(-math.Pow((x-knotA)/4.8, 2))
		knot = math.Max(knot, math.Exp(-math.Pow((x-knotB)/5.8, 2))*0.94)
		knot = math.Max(knot, math.Exp(-math.Pow((x-knotC)/4.2, 2))*0.86)
		level = math.Max(level, knot)

		crossing := math.Abs(math.Sin(warp+t*0.4)) < 0.075 && math.Abs(weft) > 0.38
		switch {
		case knot > 0.91 && organicNoise("loom", uint64(300+index), float64(phase)/11) > 0.58:
			chars = append(chars, '✦')
		case knot > 0.78:
			chars = append(chars, '⌁')
		case crossing:
			chars = append(chars, '≋')
		case level > 0.74:
			chars = append(chars, '≈')
		default:
			chars = append(chars, rampPick(shadeRamp, level))
		}
	}
	return string(chars)
}

func cometArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"░▒☄", "▒▓✦", "▓█✶", "▒▓✧", "░▒·"})
	}

	chars := make([]rune, width)
	for i := range chars {
		nebula := 0.08 + 0.20*organicNoise("comet", uint64(10+i%17), float64(phase)/31+float64(i)*0.04)
		chars[i] = rampPick(shadeRamp, nebula)
	}

	type cometHead struct {
		pos    int
		dir    int
		length int
		head   rune
		event  organicEvent
		stream uint64
	}
	heads := []cometHead{}
	for stream, timing := range []struct {
		spacing float64
		minimum float64
		maximum float64
	}{
		{34, 22, 34},
		{53, 30, 46},
		{79, 38, 58},
	} {
		for _, event := range organicEvents("comet", uint64(stream+1), float64(phase), timing.spacing, timing.minimum, timing.maximum) {
			length := 10 + int(eventRandom("comet", uint64(stream+1), event.Index, 2)*17)
			direction := 1
			if eventRandom("comet", uint64(stream+1), event.Index, 3) < 0.38 {
				direction = -1
			}
			travel := float64(width + length*2)
			pos := -length + int(event.Progress*travel)
			if direction < 0 {
				pos = width + length - int(event.Progress*travel)
			}
			headRunes := []rune{'☄', '✦', '✧', '✶'}
			head := headRunes[int(eventRandom("comet", uint64(stream+1), event.Index, 4)*float64(len(headRunes)))%len(headRunes)]
			heads = append(heads, cometHead{
				pos:    pos,
				dir:    direction,
				length: length,
				head:   head,
				event:  event,
				stream: uint64(stream + 1),
			})
		}
	}
	for cometIndex, comet := range heads {
		for tail := 0; tail < comet.length; tail++ {
			index := comet.pos - tail*comet.dir
			if index < 0 || index >= width {
				continue
			}
			if tail == 0 {
				chars[index] = comet.head
			} else {
				level := math.Max(0.0, 1.0-float64(tail)/float64(comet.length)) * math.Min(1, comet.event.Envelope*1.7)
				if cometIndex == 1 && tail%5 == 0 {
					chars[index] = '·'
				} else {
					chars[index] = rampPick(cometTrail, level)
				}
			}
			if tail == 2 && index+1 < width && eventRandom("comet", comet.stream, comet.event.Index, uint64(20+tail)) > 0.46 {
				chars[index+1] = '✶'
			}
		}
	}
	return string(chars)
}

func smileysArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"ಠ_ಠ", ">_<", "^_^", "ʕ•ᴥ", "☞ﾟヮ"})
	}

	chars := make([]rune, width)
	for index := range chars {
		sparkle := organicNoise("smileys", uint64(10+index), float64(phase)/8)
		switch {
		case sparkle > 0.985:
			chars[index] = '✧'
		case sparkle > 0.955:
			chars[index] = '·'
		case sparkle < 0.025:
			chars[index] = '｡'
		default:
			chars[index] = ' '
		}
	}

	streamCount := max(1, min(4, width/30))
	for faceIndex := range streamCount {
		stream := uint64(faceIndex + 1)
		phaseOffset := float64(phase + faceIndex*23)
		for _, event := range organicEvents("smileys", stream, phaseOffset, 61+float64(faceIndex*13), 42, 76) {
			faceChoice := int(eventRandom("smileys", stream, event.Index, 2) * float64(len(smileyFaces)))
			face := []rune(smileyFaces[faceChoice%len(smileyFaces)])
			direction := 1
			if eventRandom("smileys", stream, event.Index, 3) < 0.46 {
				direction = -1
			}
			travel := float64(width + len(face)*2)
			pos := -len(face) + int(smoothstep(event.Progress)*travel)
			if direction < 0 {
				pos = width + len(face) - int(smoothstep(event.Progress)*travel)
			}
			pos += int(math.Round(signedOrganicNoise("smileys", 20+stream, float64(phase)/9) * 2))
			for offset, r := range face {
				target := pos + offset
				if target >= 0 && target < width {
					chars[target] = r
				}
			}
			if pos > 1 && pos < width && eventRandom("smileys", stream, event.Index, 4) > 0.42 {
				chars[pos-2] = '｡'
				chars[pos-1] = '･'
			}
			tail := pos + len(face)
			if tail >= 0 && tail+1 < width && eventRandom("smileys", stream, event.Index, 5) > 0.58 {
				chars[tail] = '･'
				chars[tail+1] = 'ﾟ'
			}
		}
	}
	return string(chars)
}

func waveArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return shortFrame(width, phase, []string{"▁▃▅", "▂▅▇", "▃▆◟", "▅█╲", "▇▅▂"})
	}

	chars := make([]rune, 0, width)
	warpedPhase := float64(phase) + signedOrganicNoise("wave", 1, float64(phase)/82)*22
	crestA := math.Mod(float64(width)*0.82-warpedPhase*0.34+float64(width)*100, float64(width))
	crestB := math.Mod(float64(width)*0.33-warpedPhase*0.18+
		signedOrganicNoise("wave", 2, float64(phase)/59)*float64(width)*0.23+float64(width)*100, float64(width))
	energy := 0.72 + organicNoise("wave", 3, float64(phase)/73)*0.38
	breakers := organicEvents("wave", 4, float64(phase), 46, 16, 29)
	for index := range width {
		x := float64(index)
		swell := (math.Sin(x*0.115-warpedPhase*0.073) + 1.0) * 0.5
		backwash := (math.Sin(x*0.041+warpedPhase*0.029+1.4) + 1.0) * 0.5
		curl := math.Exp(-math.Pow((x-crestA)/6.5, 2))
		outer := math.Exp(-math.Pow((x-crestB)/10.0, 2)) * 0.58
		for _, event := range breakers {
			center := eventRandom("wave", 4, event.Index, 2) * float64(width-1)
			curl = math.Max(curl, event.Envelope*math.Exp(-math.Pow((x-center)/5.2, 2)))
		}
		level := (0.10 + 0.46*swell + 0.18*backwash + 0.30*math.Max(curl, outer)) * energy

		switch {
		case curl > 0.91:
			chars = append(chars, []rune("◜◝◞◟")[(phase/3+index)%4])
		case curl > 0.70:
			chars = append(chars, []rune("▇█▇")[(phase/3+index)%3])
		case curl > 0.46:
			if (index+phase)%2 == 0 {
				chars = append(chars, '╱')
			} else {
				chars = append(chars, '╲')
			}
		case outer > 0.36 && (index+phase/2)%3 == 0:
			if (index+phase)%2 == 0 {
				chars = append(chars, '▂')
			} else {
				chars = append(chars, '▃')
			}
		case level > 0.76:
			chars = append(chars, '█')
		case level > 0.64:
			chars = append(chars, '▇')
		case level > 0.52:
			chars = append(chars, '▅')
		case level > 0.40:
			chars = append(chars, '▃')
		case level > 0.28:
			chars = append(chars, '▁')
		default:
			chars = append(chars, ' ')
		}
	}
	return string(chars)
}

func splineArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 12 {
		return shortFrame(width, phase, []string{"⢀⣠⠤", "⠉⠢⣀", "⣀⠔⠉", "⠤⣄⡀"})
	}

	masks := make([]int, width)
	dotMask := func(row int, col int) int {
		if col == 0 {
			return []int{0x01, 0x02, 0x04, 0x40}[row]
		}
		return []int{0x08, 0x10, 0x20, 0x80}[row]
	}
	plot := func(pixelX int, row int) {
		if pixelX < 0 || pixelX >= width*2 || row < 0 || row > 3 {
			return
		}
		masks[pixelX/2] |= dotMask(row, pixelX%2)
	}
	plotBrush := func(pixelX int, y float64) {
		center := int(math.Round(y))
		plot(pixelX, center)
		if y-float64(center) > 0.28 {
			plot(pixelX, center+1)
		}
		if float64(center)-y > 0.28 {
			plot(pixelX, center-1)
		}
	}

	pixelWidth := width * 2
	segments := max(2, min(5, width/18))
	segmentWidth := max(10, pixelWidth/segments)
	for segment := range segments {
		left := segment * segmentWidth
		right := min(pixelWidth-1, (segment+1)*segmentWidth)
		if segment == segments-1 {
			right = pixelWidth - 1
		}
		if right <= left {
			continue
		}

		noiseTime := float64(phase) / (49 + float64(segment)*7)
		y0 := 0.12 + organicNoise("spline", uint64(10+segment), noiseTime)*2.76
		y1 := 0.12 + organicNoise("spline", uint64(11+segment), noiseTime)*2.76
		c0 := 1.5 + signedOrganicNoise("spline", uint64(40+segment*2), float64(phase)/37)*2.05
		c1 := 1.5 + signedOrganicNoise("spline", uint64(41+segment*2), float64(phase)/43)*2.05

		for sample := 0; sample <= right-left; sample++ {
			u := float64(sample) / float64(max(1, right-left))
			a := math.Pow(1-u, 3)
			b := 3 * math.Pow(1-u, 2) * u
			c := 3 * (1 - u) * u * u
			d := u * u * u
			y := a*y0 + b*c0 + c*c1 + d*y1
			plotBrush(left+sample, math.Max(0, math.Min(3, y)))
		}

		for _, point := range []struct {
			x int
			y float64
		}{
			{left, y0},
			{left + (right-left)/3, c0},
			{left + 2*(right-left)/3, c1},
			{right, y1},
		} {
			row := int(math.Round(math.Max(0, math.Min(3, point.y))))
			plot(point.x, row)
			plot(point.x-1, row)
			plot(point.x+1, row)
			plot(point.x, row-1)
			plot(point.x, row+1)
		}
	}

	chars := make([]rune, width)
	for index, mask := range masks {
		if mask == 0 {
			switch {
			case (index+phase/4)%37 == 0:
				chars[index] = '·'
			case (index-phase/5)%53 == 0:
				chars[index] = '⋅'
			default:
				chars[index] = ' '
			}
			continue
		}
		chars[index] = rune(0x2800 + mask)
	}

	tracerClock := float64(phase)*0.42 + signedOrganicNoise("spline", 90, float64(phase)/61)*float64(width)*0.18
	tracer := int(math.Mod(tracerClock+float64(width)*100, float64(width)))
	tracerGlyph := int(organicNoise("spline", 91, float64(phase)/8)*4) % 4
	chars[tracer] = []rune{'◆', '✦', '◇', '✧'}[tracerGlyph]

	return string(chars)
}

type animationFunc func(width int, phase int) string

var animationPresets = map[string]animationFunc{
	"aurora":         auroraArt,
	"aurora_sound":   auroraSoundArt,
	"spectrum":       spectrumArt,
	"spectrum_sound": spectrumSoundArt,
	"radar":          radarArt,
	"radar_sound":    radarSoundArt,
	"constellation":  constellationArt,
	"circuit":        circuitArt,
	"braid":          braidArt,
	"loom":           loomArt,
	"comet":          cometArt,
	"comet_sound":    cometSoundArt,
	"smileys":        smileysArt,
	"wave":           waveArt,
	"wave_sound":     waveSoundArt,
	"spline":         splineArt,
	"square":         squareArt,
	"square_sound":   squareSoundArt,
	"ripples":        ripplesArt,
	"ripples_sound":  ripplesSoundArt,
	"bloom":          bloomArt,
	"bloom_sound":    bloomSoundArt,
	"glitch":         glitchArt,
	"ribbon":         ribbonArt,
	"shutter":        shutterArt,
	"shutter_sound":  shutterSoundArt,
}

var soundPresetNames = map[string]bool{
	"aurora_sound":   true,
	"bloom_sound":    true,
	"comet_sound":    true,
	"spectrum_sound": true,
	"square_sound":   true,
	"ripples_sound":  true,
	"radar_sound":    true,
	"shutter_sound":  true,
	"wave_sound":     true,
}

func isSoundPreset(name string) bool {
	return soundPresetNames[name]
}

var rotationPresets = []string{
	"loom",
	"aurora",
	"bloom",
	"spectrum",
	"square",
	"ripples",
	"radar",
	"constellation",
	"circuit",
	"glitch",
	"braid",
	"comet",
	"smileys",
	"wave",
	"spline",
}

func rotationArt(width int, phase int) string {
	cycleFrames := settings.RotationHoldFrames + settings.RotationBlendFrames
	presetIndex := (phase / cycleFrames) % len(rotationPresets)
	offset := phase % cycleFrames
	preset := rotationPresets[presetIndex]
	motion := motionPhase(phase)

	if offset < settings.RotationHoldFrames {
		return animationPresets[preset](width, motion)
	}

	nextPreset := rotationPresets[(presetIndex+1)%len(rotationPresets)]
	from := animationPresets[preset](width, motion)
	to := animationPresets[nextPreset](width, motion)
	progress := float64(offset-settings.RotationHoldFrames) / float64(settings.RotationBlendFrames)
	return blendArt(from, to, width, phase, smoothstep(progress))
}

func activityArt(width int, phase int) string {
	if animationPreset == rotationSelection {
		return rotationArt(width, phase)
	}
	return animationFuncFor(animationPreset)(width, motionPhase(phase))
}

func animationFrameKey(phase int) int {
	if animationPreset != rotationSelection {
		if isSoundPreset(animationPreset) {
			return phase
		}
		return motionPhase(phase)
	}
	cycleFrames := settings.RotationHoldFrames + settings.RotationBlendFrames
	presetIndex := (phase / cycleFrames) % len(rotationPresets)
	offset := phase % cycleFrames
	preset := rotationPresets[presetIndex]
	if isSoundPreset(preset) ||
		(offset >= settings.RotationHoldFrames &&
			isSoundPreset(rotationPresets[(presetIndex+1)%len(rotationPresets)])) {
		return 20_000_000 + phase
	}
	if offset < settings.RotationHoldFrames {
		return presetIndex*1_000_000 + motionPhase(phase)
	}
	return 10_000_000 + phase
}

func framesUntilNextAnimationKey(phase int) int {
	current := animationFrameKey(phase)
	for frames := 1; frames <= 240; frames++ {
		if animationFrameKey(phase+frames) != current {
			return frames
		}
	}
	return 1
}

func blendArt(from string, to string, width int, phase int, progress float64) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	fromRunes := fitRunes(from, width)
	toRunes := fitRunes(to, width)
	out := make([]rune, 0, width)

	for index := range width {
		x := float64(index) / float64(max(1, width-1))
		wave := 0.13 * math.Sin(float64(index)*0.42+float64(phase)*0.055)
		dither := 0.10 * (pseudoRandom(index, phase/5, 17.0) - 0.5)
		threshold := x + wave + dither
		if progress >= threshold {
			out = append(out, toRunes[index])
		} else {
			out = append(out, fromRunes[index])
		}
	}
	return string(out)
}

func fitRunes(value string, width int) []rune {
	runes := []rune(value)
	if len(runes) > width {
		return runes[:width]
	}
	if len(runes) < width {
		padding := make([]rune, width-len(runes))
		for index := range padding {
			padding[index] = ' '
		}
		runes = append(runes, padding...)
	}
	return runes
}

func smoothstep(value float64) float64 {
	value = math.Max(0.0, math.Min(1.0, value))
	return value * value * (3.0 - 2.0*value)
}

func motionPhase(phase int) int {
	return int(math.Floor(float64(phase) * settings.Motion))
}

func animationFuncFor(name string) animationFunc {
	return animationPresets[name]
}

func frameAnimationArt(animation FrameAnimation) animationFunc {
	return func(width int, phase int) string {
		width = artWidth(width)
		if width == 0 || len(animation.Frames) == 0 {
			return ""
		}
		frame := []rune(animation.Frames[phase%len(animation.Frames)])
		if animation.Fill && len(frame) > 0 {
			out := make([]rune, 0, width)
			for len(out) < width {
				remaining := width - len(out)
				if len(frame) <= remaining {
					out = append(out, frame...)
				} else {
					out = append(out, frame[:remaining]...)
				}
			}
			return string(out)
		}
		if len(frame) > width {
			frame = frame[:width]
		}
		if len(frame) < width {
			padding := make([]rune, width-len(frame))
			for index := range padding {
				padding[index] = ' '
			}
			frame = append(frame, padding...)
		}
		return string(frame)
	}
}
