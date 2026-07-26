package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeAudioCaptureBackend struct {
	availableErr error
	capture      func(context.Context, func(io.Reader) error) error
}

func (backend fakeAudioCaptureBackend) Name() string {
	return "fake"
}

func (backend fakeAudioCaptureBackend) Available() error {
	return backend.availableErr
}

func (backend fakeAudioCaptureBackend) Capture(ctx context.Context, consume func(io.Reader) error) error {
	return backend.capture(ctx, consume)
}

func TestAudioBandBinsAreOrderedAndDoNotOverlap(t *testing.T) {
	previousLast := 0
	distinctRanges := map[[2]int]bool{}
	for band, binRange := range audioBandRanges() {
		first, last := binRange[0], binRange[1]
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

func TestDefaultAudioBackendUsesConfiguredDevice(t *testing.T) {
	original := audioSettings
	t.Cleanup(func() {
		audioSettings = original
	})
	device := "  alsa_output.test.monitor  "
	applyConfig(Config{Audio: ConfigAudio{Device: &device}})

	backend, ok := defaultAudioCaptureBackend().(parecCaptureBackend)
	if !ok {
		t.Fatal("expected parec to remain the production capture backend")
	}
	if backend.device != "alsa_output.test.monitor" {
		t.Fatalf("expected configured device, got %q", backend.device)
	}
	if !slices.Contains(backend.arguments(), "--device=alsa_output.test.monitor") {
		t.Fatalf("expected configured device in parec arguments: %v", backend.arguments())
	}
	if !slices.Contains(backend.arguments(), "--rate=48000") ||
		!slices.Contains(backend.arguments(), "--channels=2") {
		t.Fatalf("expected 48 kHz stereo parec arguments: %v", backend.arguments())
	}
}

func TestAudioMonitorReportsUnavailableBackendOnceWithoutCapture(t *testing.T) {
	original := audioSettings
	audioSettings.Sensitivity = 1
	t.Cleanup(func() {
		audioSettings = original
	})

	captures := 0
	backend := fakeAudioCaptureBackend{
		availableErr: errors.New("missing"),
		capture: func(context.Context, func(io.Reader) error) error {
			captures++
			return nil
		},
	}
	var diagnostics bytes.Buffer
	stop := startAudioMonitor(backend, &audioMeter{}, &diagnostics)
	stop()
	stop()

	if captures != 0 {
		t.Fatalf("unavailable backend must not capture, attempts=%d", captures)
	}
	if count := strings.Count(diagnostics.String(), "is unavailable"); count != 1 {
		t.Fatalf("expected one unavailable diagnostic, got %q", diagnostics.String())
	}
}

func TestAudioMonitorCancelsInjectedBackend(t *testing.T) {
	original := audioSettings
	audioSettings.Sensitivity = 1
	t.Cleanup(func() {
		audioSettings = original
	})

	started := make(chan struct{})
	canceled := make(chan struct{})
	backend := fakeAudioCaptureBackend{
		capture: func(ctx context.Context, _ func(io.Reader) error) error {
			close(started)
			<-ctx.Done()
			close(canceled)
			return ctx.Err()
		},
	}
	stop := startAudioMonitor(backend, &audioMeter{}, io.Discard)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("fake backend did not start")
	}
	stop()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("fake backend did not observe cancellation")
	}
}

func TestAudioMeterReportsInjectedCaptureFailureOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	backend := fakeAudioCaptureBackend{
		capture: func(context.Context, func(io.Reader) error) error {
			attempts++
			if attempts == 2 {
				cancel()
			}
			return errors.New("capture failed")
		},
	}
	var diagnostics bytes.Buffer
	var meter audioMeter
	meter.run(ctx, backend, &diagnostics, time.Millisecond)

	if attempts != 2 {
		t.Fatalf("expected two capture attempts, got %d", attempts)
	}
	if count := strings.Count(diagnostics.String(), "capture unavailable"); count != 1 {
		t.Fatalf("expected one capture diagnostic, got %q", diagnostics.String())
	}
}

func TestAudioConsumeBuildsOverlappingAnalysisWindow(t *testing.T) {
	samples := sineSamples(880, 0.45)
	encoded := bytes.NewReader(encodeStereoSamples(samples, samples))

	var meter audioMeter
	if err := meter.consume(&limitedReader{reader: encoded, limit: 7}); !errors.Is(err, io.EOF) {
		t.Fatalf("expected end of finite test stream, got %v", err)
	}
	if snapshot := meter.snapshot(); !snapshot.Active || snapshot.Level <= 0.1 {
		t.Fatalf("expected consumed audio to update the meter, got %+v", snapshot)
	}
}

type limitedReader struct {
	reader io.Reader
	limit  int
}

func (reader *limitedReader) Read(buffer []byte) (int, error) {
	if len(buffer) > reader.limit {
		buffer = buffer[:reader.limit]
	}
	return reader.reader.Read(buffer)
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

func TestAnalyzeStereoAudioExposesRegionsAndBalance(t *testing.T) {
	silence := make([]int16, audioBlockSize)
	low := sineSamples(120, 0.35)
	high := sineSamples(6000, 0.35)

	lowAnalysis := analyzeStereoAudioBlock(low, low)
	if lowAnalysis.Bass <= lowAnalysis.HighMid || lowAnalysis.Bass <= lowAnalysis.Treble {
		t.Fatalf("expected bass-heavy analysis, got %+v", lowAnalysis)
	}
	if math.Abs(lowAnalysis.Balance) > 0.01 {
		t.Fatalf("expected centered stereo balance, got %.3f", lowAnalysis.Balance)
	}

	highAnalysis := analyzeStereoAudioBlock(high, high)
	if highAnalysis.Treble <= highAnalysis.Bass || highAnalysis.Centroid <= lowAnalysis.Centroid {
		t.Fatalf("expected bright treble-heavy analysis, low=%+v high=%+v", lowAnalysis, highAnalysis)
	}

	leftAnalysis := analyzeStereoAudioBlock(low, silence)
	if leftAnalysis.LeftLevel <= 0.1 || leftAnalysis.RightLevel != 0 || leftAnalysis.Balance > -0.9 {
		t.Fatalf("expected strongly left-panned analysis, got %+v", leftAnalysis)
	}
	rightAnalysis := analyzeStereoAudioBlock(silence, low)
	if rightAnalysis.RightLevel <= 0.1 || rightAnalysis.LeftLevel != 0 || rightAnalysis.Balance < 0.9 {
		t.Fatalf("expected strongly right-panned analysis, got %+v", rightAnalysis)
	}

	inverted := make([]int16, len(low))
	for index, sample := range low {
		inverted[index] = -sample
	}
	antiPhase := analyzeStereoAudioBlock(low, inverted)
	if antiPhase.Level <= 0.1 || antiPhase.Bass <= 0 {
		t.Fatalf("opposite channel phase must not cancel shared energy: %+v", antiPhase)
	}
}

func TestAudioMeterAttacksQuicklyAndReleasesSmoothly(t *testing.T) {
	var meter audioMeter
	now := time.Now()
	meter.update(sineSamples(880, 0.4), now)
	active := meter.snapshot()
	if !active.Active || active.Level <= 0.05 || active.Revision != 1 {
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

func TestAudioSmoothingDependsOnElapsedTimeNotUpdateCount(t *testing.T) {
	silence := make([]int16, audioBlockSize)
	tone := sineSamples(880, 0.25)
	start := time.Now()
	var frequent audioMeter
	var sparse audioMeter
	frequent.update(silence, start)
	sparse.update(silence, start)

	for step := 1; step <= 10; step++ {
		frequent.update(tone, start.Add(time.Duration(step)*10*time.Millisecond))
	}
	sparse.update(tone, start.Add(100*time.Millisecond))

	if difference := math.Abs(frequent.snapshot().Level - sparse.snapshot().Level); difference > 0.001 {
		t.Fatalf("expected elapsed-time-equivalent smoothing, difference=%.6f", difference)
	}
}

func TestAudioRevisionChangesOnlyForMaterialVisualState(t *testing.T) {
	silence := make([]int16, audioBlockSize)
	now := time.Now()
	var meter audioMeter
	meter.update(silence, now)
	first := meter.snapshot().Revision
	meter.update(silence, now.Add(20*time.Millisecond))
	if second := meter.snapshot().Revision; second != first {
		t.Fatalf("steady silence must not advance visual revision: first=%d second=%d", first, second)
	}

	meter.update(sineSamples(440, 0.3), now.Add(40*time.Millisecond))
	active := meter.snapshot()
	if active.Revision <= first || active.Bass == 0 || active.LowMid == 0 {
		t.Fatalf("material audio must advance revision and aggregate features: %+v", active)
	}
}

func TestAudioSensitivityScalesAnalyzedEnergy(t *testing.T) {
	samples := sineSamples(880, 0.04)
	now := time.Now()
	var normal audioMeter
	normal.setSensitivity(1)
	normal.update(samples, now)
	var boosted audioMeter
	boosted.setSensitivity(2)
	boosted.update(samples, now)

	normalSnapshot := normal.snapshot()
	boostedSnapshot := boosted.snapshot()
	if boostedSnapshot.Level <= normalSnapshot.Level {
		t.Fatalf("expected sensitivity to raise level, normal=%.3f boosted=%.3f",
			normalSnapshot.Level, boostedSnapshot.Level)
	}
	if boostedSnapshot.Bands[strongestAudioBand(normalSnapshot.Bands)] <=
		normalSnapshot.Bands[strongestAudioBand(normalSnapshot.Bands)] {
		t.Fatal("expected sensitivity to raise band energy")
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

func TestAudioMotionScalesVisualSnapshot(t *testing.T) {
	snapshot := audioSnapshot{Active: true, Level: 0.4}
	snapshot.Bands[5] = 0.45

	quiet := scaleAudioSnapshot(snapshot, 0.5)
	strong := scaleAudioSnapshot(snapshot, 2)
	if quiet.Level >= snapshot.Level || quiet.Bands[5] >= snapshot.Bands[5] {
		t.Fatalf("expected reduced motion response, got %+v", quiet)
	}
	if strong.Level <= snapshot.Level || strong.Bands[5] <= snapshot.Bands[5] {
		t.Fatalf("expected increased motion response, got %+v", strong)
	}
}

func TestAudioPresetActivationIsScoped(t *testing.T) {
	originalRotation := rotationPresets
	t.Cleanup(func() {
		rotationPresets = originalRotation
	})

	if !presetUsesAudio("aurora_sound") || presetUsesAudio("aurora") {
		t.Fatal("expected only aurora_sound to request audio directly")
	}
	rotationPresets = []string{"aurora", "aurora_sound"}
	if !presetUsesAudio(rotationSelection) {
		t.Fatal("expected a rotation containing aurora_sound to request audio")
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

func encodeStereoSamples(left []int16, right []int16) []byte {
	var encoded bytes.Buffer
	for index := range min(len(left), len(right)) {
		_ = binary.Write(&encoded, binary.LittleEndian, left[index])
		_ = binary.Write(&encoded, binary.LittleEndian, right[index])
	}
	return encoded.Bytes()
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
