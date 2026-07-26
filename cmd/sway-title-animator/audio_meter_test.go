package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

func TestAudioBandBinsAreOrderedAndDoNotOverlap(t *testing.T) {
	const (
		minFrequency = 55.0
		maxFrequency = 11_000.0
	)
	previousLast := 0
	distinctRanges := map[[2]int]bool{}
	for band := range audioBandCount {
		low := minFrequency * math.Pow(maxFrequency/minFrequency, float64(band)/audioBandCount)
		high := minFrequency * math.Pow(maxFrequency/minFrequency, float64(band+1)/audioBandCount)
		first, last := audioBandBinRange(low, high)
		if first <= previousLast {
			t.Fatalf("band %d overlaps the preceding range: first=%d previousLast=%d", band, first, previousLast)
		}
		if last < first {
			t.Fatalf("band %d has invalid bins %d-%d", band, first, last)
		}
		distinctRanges[[2]int{first, last}] = true
		previousLast = last
	}
	if len(distinctRanges) != audioBandCount {
		t.Fatalf("expected %d distinct FFT ranges, got %d", audioBandCount, len(distinctRanges))
	}
}

func TestAudioCaptureLoopReportsFailureAndStopsOnCancellation(t *testing.T) {
	var meter audioMeter
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	reports := 0
	meter.runCaptureLoop(ctx, func(context.Context) error {
		attempts++
		if attempts == 2 {
			cancel()
		}
		return errors.New("capture failed")
	}, time.Millisecond, func(error) {
		reports++
	})
	if attempts != 2 || reports != 1 {
		t.Fatalf("expected one reported retry before cancellation, attempts=%d reports=%d", attempts, reports)
	}
}

func TestAudioConsumeBuildsOverlappingAnalysisWindow(t *testing.T) {
	samples := sineSamples(880, 0.45)
	var encoded bytes.Buffer
	for _, sample := range samples {
		if err := binary.Write(&encoded, binary.LittleEndian, sample); err != nil {
			t.Fatalf("encode samples: %v", err)
		}
	}

	var meter audioMeter
	if err := meter.consume(&encoded); !errors.Is(err, io.EOF) {
		t.Fatalf("expected end of finite test stream, got %v", err)
	}
	if snapshot := meter.snapshot(); !snapshot.Active || snapshot.Level <= 0.1 {
		t.Fatalf("expected consumed audio to update the meter, got %+v", snapshot)
	}
}

func TestAnalyzeAudioBlockSeparatesLowAndHighFrequencies(t *testing.T) {
	lowBands, lowLevel := analyzeAudioBlock(sineSamples(220, 0.45))
	highBands, highLevel := analyzeAudioBlock(sineSamples(4200, 0.45))

	if lowLevel <= 0.1 || highLevel <= 0.1 {
		t.Fatalf("expected both tones to register, levels low=%.3f high=%.3f", lowLevel, highLevel)
	}
	lowPeak := strongestAudioBand(lowBands)
	highPeak := strongestAudioBand(highBands)
	if lowPeak >= highPeak {
		t.Fatalf("expected higher tone to land in a higher band, low=%d high=%d", lowPeak, highPeak)
	}
}

func TestAudioMeterAttacksQuicklyAndReleasesSmoothly(t *testing.T) {
	var meter audioMeter
	now := time.Now()
	meter.update(sineSamples(880, 0.4), now)
	active := meter.snapshot()
	if !active.Active || active.Level <= 0.1 || active.Sequence != 1 {
		t.Fatalf("expected an active first audio frame, got %+v", active)
	}

	silence := make([]int16, audioBlockSize)
	meter.update(silence, now.Add(20*time.Millisecond))
	firstRelease := meter.snapshot().Level
	if firstRelease <= 0 || firstRelease >= active.Level {
		t.Fatalf("expected a smooth partial release, active=%.3f release=%.3f", active.Level, firstRelease)
	}
	for step := 2; step <= 40; step++ {
		meter.update(silence, now.Add(time.Duration(step)*20*time.Millisecond))
	}
	if quiet := meter.snapshot(); quiet.Active || quiet.Level >= 0.025 {
		t.Fatalf("expected sustained silence to settle into the quiet state, got %+v", quiet)
	}
}

func TestAuroraSoundUsesQuietFallbackAndAudioBands(t *testing.T) {
	originalSeed := animationSeed
	animationSeed = 0x5eed
	t.Cleanup(func() {
		animationSeed = originalSeed
	})

	quiet := auroraSoundArtWithSnapshot(80, 37, audioSnapshot{})
	audio := audioSnapshot{Active: true, Level: 0.72}
	for index := range audio.Bands {
		audio.Bands[index] = 0.15 + 0.8*math.Abs(math.Sin(float64(index)*0.41))
	}
	dancing := auroraSoundArtWithSnapshot(80, 37, audio)

	if len([]rune(quiet)) != 80 || len([]rune(dancing)) != 80 {
		t.Fatalf("expected bounded 80-column frames, quiet=%d dancing=%d",
			len([]rune(quiet)), len([]rune(dancing)))
	}
	if quiet == dancing {
		t.Fatalf("expected audio energy to change the quiet frame %q", quiet)
	}
	if quiet != strings.Repeat("▁", 80) {
		t.Fatalf("expected a straight bottom line at rest, got %q", quiet)
	}
	if !strings.ContainsAny(dancing, "▅▆▇█╿┃") {
		t.Fatalf("expected energized bars or peaks, got %q", dancing)
	}
}

func TestAuroraSoundNeedlesRepresentPeakStrength(t *testing.T) {
	normalPeak := audioSnapshot{Active: true, Level: 0.5}
	extremePeak := audioSnapshot{Active: true, Level: 0.5}
	for index := range normalPeak.Bands {
		normalPeak.Bands[index] = 0.86
		extremePeak.Bands[index] = 0.97
	}

	normalFrame := auroraSoundArtWithSnapshot(40, 1, normalPeak)
	if !strings.ContainsRune(normalFrame, '╿') || strings.ContainsRune(normalFrame, '┃') {
		t.Fatalf("expected only light needles for normal peaks, got %q", normalFrame)
	}
	extremeFrame := auroraSoundArtWithSnapshot(40, 1, extremePeak)
	if !strings.ContainsRune(extremeFrame, '┃') {
		t.Fatalf("expected heavy needles for extreme peaks, got %q", extremeFrame)
	}
}

func TestAudioPresetActivationIsScoped(t *testing.T) {
	originalShowcase := showcasePresets
	t.Cleanup(func() {
		showcasePresets = originalShowcase
	})

	if !presetUsesAudio("aurora_sound") || presetUsesAudio("aurora") {
		t.Fatal("expected only aurora_sound to request audio directly")
	}
	showcasePresets = []string{"aurora", "aurora_sound"}
	if !presetUsesAudio("showcase") {
		t.Fatal("expected a showcase containing aurora_sound to request audio")
	}
	if !presetListUsesAudio([]string{"aurora", "aurora_sound"}) ||
		presetListUsesAudio([]string{"aurora", "square"}) {
		t.Fatal("unexpected preview audio activation")
	}
}

func sineSamples(frequency float64, amplitude float64) []int16 {
	samples := make([]int16, audioBlockSize)
	for index := range samples {
		value := math.Sin(2*math.Pi*frequency*float64(index)/audioSampleRate) * amplitude
		samples[index] = int16(value * 32767)
	}
	return samples
}

func strongestAudioBand(bands [audioBandCount]float64) int {
	strongest := 0
	for index := 1; index < len(bands); index++ {
		if bands[index] > bands[strongest] {
			strongest = index
		}
	}
	return strongest
}
