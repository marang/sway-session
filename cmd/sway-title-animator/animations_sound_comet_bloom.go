package main

import (
	"math"
	"time"
)

func cometSoundArt(width int, phase int) string {
	return cometSoundArtWithSnapshot(
		width,
		phase,
		scaleAudioSnapshot(currentAudioSnapshot(), audioSettings.Motion),
	)
}

func cometSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}

	chars := make([]rune, width)
	for index := range chars {
		chars[index] = ' '
	}
	addCometSoundParticles(chars, phase, audio.Active)
	onset, ok := newestSoundOnset(audio, audioRegionBass)
	head, direction, envelope, live := cometSoundFlight(width, onset, audio.Centroid)
	if !ok || !live {
		return string(chars)
	}

	headIndex := max(0, min(width-1, int(math.Round(head))))
	tailLength := 3 + int(math.Round(audio.Level*float64(width)*0.30))
	tailLength = max(3, min(width-1, tailLength))
	for distance := tailLength; distance >= 1; distance-- {
		index := headIndex - direction*distance
		if index < 0 || index >= width {
			continue
		}
		density := envelope * (1 - float64(distance)/float64(tailLength+1))
		switch {
		case audio.Treble > 0.35 &&
			(index+phase+int(onset.ID))%max(3, 8-int(math.Round(audio.Treble*4))) == 0:
			chars[index] = '✦'
		case density > 0.70:
			chars[index] = '▓'
		case density > 0.42:
			chars[index] = '▒'
		case density > 0.18:
			chars[index] = '░'
		case chars[index] == 0:
			chars[index] = '·'
		}
	}
	chars[headIndex] = '☄'
	return string(chars)
}

func cometSoundFlight(width int, onset audioOnset, centroid float64) (float64, int, float64, bool) {
	if width <= 0 || onset.Region != audioRegionBass || onset.Age < 0 {
		return 0, 0, 0, false
	}
	centroid = math.Max(0, math.Min(1, centroid))
	lifetime := 1650*time.Millisecond -
		time.Duration(centroid*float64(950*time.Millisecond))
	if onset.Age >= lifetime {
		return 0, 0, 0, false
	}
	direction := 1
	if onset.Position < -0.05 || (math.Abs(onset.Position) <= 0.05 && onset.ID%2 == 0) {
		direction = -1
	}
	progress := smoothstep(float64(onset.Age) / float64(lifetime))
	head := progress * float64(max(0, width-1))
	if direction < 0 {
		head = float64(width-1) - head
	}
	envelope := onset.Strength * (1 - 0.36*progress)
	return head, direction, envelope, true
}

func addCometSoundParticles(chars []rune, phase int, active bool) {
	if len(chars) == 0 {
		return
	}
	count := max(3, len(chars)/20)
	if active {
		count = max(2, len(chars)/30)
	}
	for particle := range count {
		origin := organicNoise("comet_sound", uint64(10+particle), 0.29) * float64(len(chars))
		speed := 0.018 + organicNoise("comet_sound", uint64(30+particle), 0.73)*0.028
		position := math.Mod(origin+float64(phase)*speed, float64(len(chars)))
		index := max(0, min(len(chars)-1, int(position)))
		if particle%5 == 0 {
			chars[index] = '✧'
		} else if particle%3 == 0 {
			chars[index] = '∙'
		} else {
			chars[index] = '·'
		}
	}
}

func bloomSoundArt(width int, phase int) string {
	return bloomSoundArtWithSnapshot(
		width,
		phase,
		scaleAudioSnapshot(currentAudioSnapshot(), audioSettings.Motion),
	)
}

func bloomSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}

	openness, onset := bloomSoundOpenness(audio)
	if !audio.Active && onset.ID == 0 {
		breathClock := float64(phase)*0.04 +
			signedOrganicNoise("bloom_sound", 1, 0.47)*0.8
		openness = 0.08 + (0.5+0.5*math.Sin(breathClock))*0.20
	}
	center := float64(width-1) / 2
	bend := 0.0
	if audio.Active {
		bend = math.Max(-1, math.Min(1, audio.Balance)) * math.Min(3, float64(width)*0.05)
	}
	anchor := max(0, min(width-1, int(math.Round(center))))
	petalCenter := max(0, min(width-1, int(math.Round(center+bend*openness))))
	mids := (audio.LowMid + audio.HighMid) / 2
	span := 3 + int(math.Round(openness*(4+mids*math.Min(14, float64(width)*0.18))))
	left := max(0, petalCenter-span)
	right := min(width-1, petalCenter+span)
	stem := '─'
	if audio.Active && audio.Bass > 0.58 {
		stem = '━'
	}

	chars := make([]rune, width)
	for index := range chars {
		chars[index] = ' '
	}
	for index := left; index <= right; index++ {
		distance := math.Abs(float64(index-petalCenter)) / float64(max(1, span))
		switch {
		case index == petalCenter:
			if openness > 0.76 {
				chars[index] = '✦'
			} else {
				chars[index] = '❧'
			}
		case distance > 0.78 && openness > 0.42:
			chars[index] = '⌁'
		case openness > 0.30:
			chars[index] = stem
		default:
			chars[index] = '╴'
		}
	}
	if anchor != petalCenter {
		step := 1
		if anchor < petalCenter {
			step = -1
		}
		for index := petalCenter; index != anchor; index += step {
			if chars[index] == 0 {
				chars[index] = stem
			}
		}
		chars[anchor] = '❧'
	}

	addBloomSoundPollen(chars, phase, audio, onset, left, right)
	return string(chars)
}

func bloomSoundOpenness(audio audioSnapshot) (float64, audioOnset) {
	openness := 0.08
	var trigger audioOnset
	if audio.Active {
		openness = math.Max(openness, audio.Level*0.72)
	}
	onset, ok := newestSoundOnset(audio, audioRegionGeneral)
	if !ok || onset.Strength < 0.52 || onset.Age < 0 {
		return openness, trigger
	}
	const lifetime = 1500 * time.Millisecond
	if onset.Age >= lifetime {
		return openness, trigger
	}
	progress := float64(onset.Age) / float64(lifetime)
	eventOpen := smoothstep(math.Min(1, progress/0.18))
	if progress > 0.58 {
		eventOpen *= 1 - smoothstep((progress-0.58)/0.42)
	}
	eventOpen *= onset.Strength
	return math.Max(openness, eventOpen), onset
}

func addBloomSoundPollen(
	chars []rune,
	phase int,
	audio audioSnapshot,
	onset audioOnset,
	left int,
	right int,
) {
	if audio.Treble < 0.28 || onset.ID == 0 ||
		onset.Age < 260*time.Millisecond || onset.Age >= 1200*time.Millisecond {
		return
	}
	count := 1 + int(math.Round(audio.Treble*3))
	for particle := range count {
		side := -1
		if (particle+int(onset.ID))%2 == 0 {
			side = 1
		}
		distance := 2 + particle*2 + int(onset.Age/(240*time.Millisecond))
		index := left - distance
		if side > 0 {
			index = right + distance
		}
		if index >= 0 && index < len(chars) &&
			(index+phase+particle)%2 == 0 {
			chars[index] = '·'
		}
	}
}
