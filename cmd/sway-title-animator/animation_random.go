package main

import (
	"crypto/rand"
	"encoding/binary"
	"io"
	"math"
	"os"
	"time"
)

var animationSeed = freshAnimationSeed()

type organicEvent struct {
	Index    int64
	Progress float64
	Envelope float64
}

func freshAnimationSeed() uint64 {
	return animationSeedFrom(rand.Reader, time.Now(), os.Getpid())
}

func animationSeedFrom(source io.Reader, now time.Time, pid int) uint64 {
	var data [8]byte
	if _, err := io.ReadFull(source, data[:]); err == nil {
		if seed := binary.LittleEndian.Uint64(data[:]); seed != 0 {
			return seed
		}
	}
	return mix64(uint64(now.UnixNano()) ^ uint64(pid)*0x9e3779b97f4a7c15)
}

func mix64(value uint64) uint64 {
	value += 0x9e3779b97f4a7c15
	value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
	value = (value ^ (value >> 27)) * 0x94d049bb133111eb
	return value ^ (value >> 31)
}

func animationNameHash(name string) uint64 {
	hash := uint64(1469598103934665603)
	for index := range len(name) {
		hash ^= uint64(name[index])
		hash *= 1099511628211
	}
	return hash
}

func animationRandom(name string, stream uint64, index int64) float64 {
	value := animationSeed ^ animationNameHash(name)
	value ^= mix64(stream * 0xd6e8feb86659fd93)
	value ^= mix64(uint64(index))
	return float64(mix64(value)>>11) / float64(uint64(1)<<53)
}

func eventRandom(name string, stream uint64, event int64, parameter uint64) float64 {
	return animationRandom(name, stream+parameter*0x9e3779b9, event)
}

func organicNoise(name string, stream uint64, timeValue float64) float64 {
	left := int64(math.Floor(timeValue))
	progress := timeValue - float64(left)
	progress = progress * progress * progress * (progress*(progress*6-15) + 10)
	first := animationRandom(name, stream, left)
	second := animationRandom(name, stream, left+1)
	return first + (second-first)*progress
}

func signedOrganicNoise(name string, stream uint64, timeValue float64) float64 {
	return organicNoise(name, stream, timeValue)*2 - 1
}

func organicEvents(name string, stream uint64, phase float64, spacing float64, minDuration float64, maxDuration float64) []organicEvent {
	if spacing <= 0 || minDuration <= 0 || maxDuration < minDuration {
		return nil
	}
	current := int64(math.Floor(phase / spacing))
	events := make([]organicEvent, 0, 3)
	for index := current - 2; index <= current+1; index++ {
		start := float64(index)*spacing + eventRandom(name, stream, index, 0)*spacing*0.58
		duration := minDuration + eventRandom(name, stream, index, 1)*(maxDuration-minDuration)
		progress := (phase - start) / duration
		if progress < 0 || progress >= 1 {
			continue
		}
		events = append(events, organicEvent{
			Index:    index,
			Progress: progress,
			Envelope: math.Sin(progress * math.Pi),
		})
	}
	return events
}

func wrappedDistance(first float64, second float64, span float64) float64 {
	if span <= 0 {
		return math.Abs(first - second)
	}
	distance := math.Abs(first - second)
	return math.Min(distance, span-distance)
}
