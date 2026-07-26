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
	chars := fitRunes(cometArt(width, phase), width)
	if !audio.Active {
		return string(chars)
	}

	for index := range chars {
		position := float64(index) / float64(max(1, width-1))
		energy := interpolatedAudioBand(
			audio.Bands,
			position*float64(audioBandCount-1),
		)
		switch {
		case chars[index] == '·' && energy > 0.34:
			chars[index] = '░'
		case chars[index] == '░' && energy > 0.50:
			chars[index] = '▒'
		case chars[index] == '▒' && energy > 0.66:
			chars[index] = '▓'
		}
	}
	addCometSoundParticles(chars, phase, audio)
	addCometSoundPointPulse(chars, phase, soundBeatPulse(audio))
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
		case chars[index] == ' ':
			chars[index] = '·'
		}
	}
	chars[headIndex] = '☄'
	return string(chars)
}

func addCometSoundPointPulse(chars []rune, phase int, beatPulse float64) {
	if beatPulse <= 0 {
		return
	}
	for index := range chars {
		pulse := organicNoise(
			"comet_sound_beat",
			uint64(100+index),
			float64(phase)/24,
		)
		switch {
		case (chars[index] == '·' || chars[index] == '∙') &&
			beatPulse > 0.68 && pulse > 0.28:
			chars[index] = '●'
		case (chars[index] == '·' || chars[index] == '∙') &&
			beatPulse > 0.30 && pulse > 0.42:
			chars[index] = '•'
		case chars[index] == ' ' && pulse > 0.965-beatPulse*0.14:
			if beatPulse > 0.68 {
				chars[index] = '●'
			} else {
				chars[index] = '•'
			}
		}
	}
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

func addCometSoundParticles(chars []rune, phase int, audio audioSnapshot) {
	if len(chars) == 0 {
		return
	}
	count := max(3, int(math.Round(
		float64(len(chars))*(0.025+audio.Level*0.14),
	)))
	count = min(max(3, len(chars)/6), count)
	for particle := range count {
		origin := organicNoise("comet_sound", uint64(10+particle), 0.29) * float64(len(chars))
		speed := 0.007 + organicNoise("comet_sound", uint64(30+particle), 0.73)*0.012
		position := math.Mod(origin+float64(phase)*speed, float64(len(chars)))
		index := max(0, min(len(chars)-1, int(position)))
		bandPosition := float64(index) / float64(max(1, len(chars)-1))
		energy := 0.45*audio.Level + 0.55*interpolatedAudioBand(
			audio.Bands, bandPosition*float64(audioBandCount-1),
		)
		if chars[index] != ' ' && chars[index] != '·' && chars[index] != '∙' {
			continue
		}
		switch {
		case audio.Treble > 0.52 && particle%4 == 0:
			chars[index] = '✧'
		case energy > 0.58:
			chars[index] = '░'
		case particle%3 == 0:
			chars[index] = '∙'
		default:
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

	openness, onset := bloomSoundOpenness(phase, audio)
	mids := (audio.LowMid + audio.HighMid) / 2
	for index := range chars {
		switch {
		case chars[index] == '─' && audio.Bass > 0.40:
			chars[index] = '━'
		case chars[index] == '╴' && mids > 0.38 &&
			(index+phase/9)%3 == 0:
			chars[index] = '⌁'
		case chars[index] == '⌁' && mids > 0.52 &&
			(index+phase/11)%4 == 0:
			chars[index] = '❧'
		case chars[index] == '·' && audio.Treble > 0.38:
			chars[index] = '•'
		}
	}

	center := width / 2
	left, right := center, center
	if onset.ID != 0 {
		center = int(math.Round((0.5 + onset.Position*0.40) * float64(width-1)))
		center = max(0, min(width-1, center))
		span := 2 + int(math.Round(openness*(2+mids*math.Min(8, float64(width)*0.10))))
		left = max(0, center-span)
		right = min(width-1, center+span)
		for index := left; index <= right; index++ {
			distance := math.Abs(float64(index-center)) / float64(max(1, span))
			switch {
			case index == center:
				chars[index] = '✦'
			case distance > 0.72:
				chars[index] = '⌁'
			case chars[index] == ' ':
				chars[index] = '─'
			}
		}
	}
	addBloomSoundPollen(chars, phase, audio, onset, left, right)
	return string(chars)
}

func bloomSoundOpenness(phase int, audio audioSnapshot) (float64, audioOnset) {
	breath := 0.5 + 0.5*math.Sin(
		float64(phase)*0.018+signedOrganicNoise("bloom_sound", 1, 0.47)*0.8,
	)
	openness := 0.12 + breath*0.20 + audio.Level*0.56
	onset, ok := newestSoundOnset(audio, audioRegionGeneral)
	if !ok || onset.Strength < 0.48 || onset.Age < 0 {
		return math.Min(1, openness), audioOnset{}
	}
	const lifetime = 1500 * time.Millisecond
	if onset.Age >= lifetime {
		return math.Min(1, openness), audioOnset{}
	}
	progress := float64(onset.Age) / float64(lifetime)
	envelope := 1 - smoothstep(progress)
	return math.Min(1, math.Max(openness, onset.Strength*envelope)), onset
}

func addBloomSoundPollen(
	chars []rune,
	phase int,
	audio audioSnapshot,
	onset audioOnset,
	left int,
	right int,
) {
	if audio.Treble < 0.36 {
		return
	}
	count := 1 + int(math.Round(audio.Treble*3))
	for particle := range count {
		distance := 2 + particle*3 + phase/12%4
		index := left - distance
		if particle%2 == 1 {
			index = right + distance
		}
		if onset.ID != 0 {
			index += int(onset.ID%3) - 1
		}
		if index >= 0 && index < len(chars) {
			chars[index] = '·'
		}
	}
}
