package main

import (
	"math"
	"time"
)

func cometSoundArt(width int, phase int) string {
	return cometSoundArtWithSnapshot(
		width,
		phase,
		currentSoundSnapshot(),
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
	tailLength := 3 + int(math.Round(audio.Level*float64(width)*0.22))
	tailLength = max(3, min(width-1, tailLength))
	for distance := tailLength; distance >= 1; distance-- {
		index := headIndex - direction*distance
		if index < 0 || index >= width {
			continue
		}
		density := envelope * (1 - float64(distance)/float64(tailLength+1))
		switch {
		case audio.Treble > 0.35 &&
			(index+phase/5+int(onset.ID))%max(5, 11-int(math.Round(audio.Treble*4))) == 0:
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
	lifetime := 2400*time.Millisecond -
		time.Duration(centroid*float64(800*time.Millisecond))
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
	count := max(4, len(chars)/14)
	if active {
		count = max(3, len(chars)/24)
	}
	for particle := range count {
		origin := organicNoise("comet_sound", uint64(10+particle), 0.29) * float64(len(chars))
		speed := 0.007 + organicNoise("comet_sound", uint64(30+particle), 0.73)*0.012
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
		currentSoundSnapshot(),
	)
}

func bloomSoundArtWithSnapshot(width int, phase int, audio audioSnapshot) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	chars := fitRunes(bloomArt(width, phase), width)
	if !audio.Active {
		return string(chars)
	}
	for index := range chars {
		if audio.Bass > 0.58 && chars[index] == '─' {
			chars[index] = '━'
		}
		if audio.HighMid > 0.55 &&
			(chars[index] == '╴' || chars[index] == '─' || chars[index] == '━') &&
			(index+phase/11)%7 == 0 {
			chars[index] = '⌁'
		}
	}
	onset, ok := newestSoundOnset(audio, audioRegionGeneral)
	if ok && onset.Strength > 0.52 && onset.Age >= 0 && onset.Age < 1200*time.Millisecond {
		center := int(math.Round((0.5 + onset.Position*0.4) * float64(width-1)))
		center = max(0, min(width-1, center))
		chars[center] = '✦'
		if audio.Treble > 0.4 && center+2 < width {
			chars[center+2] = '·'
		}
	}
	return string(chars)
}
