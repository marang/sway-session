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
	skipAudioWarmup(&meter)
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
	meter.captureStarted = now.Add(-audioWarmup - audioWarmupBlend)
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
	frequent.captureStarted = start.Add(-audioWarmup - audioWarmupBlend)
	sparse.captureStarted = start.Add(-audioWarmup - audioWarmupBlend)
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
	meter.captureStarted = now.Add(-audioWarmup - audioWarmupBlend)
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

func TestAudioWarmupStaysCalmThenBlendsIn(t *testing.T) {
	tone := sineSamples(440, 0.3)
	start := time.Now()
	var meter audioMeter
	meter.update(tone, start)
	if snapshot := meter.snapshotAt(start); snapshot.Active || snapshot.Level != 0 ||
		!snapshot.CaptureAvailable {
		t.Fatalf("warm-up must expose a calm snapshot: %+v", snapshot)
	}

	midpoint := start.Add(audioWarmup + audioWarmupBlend/2)
	meter.update(tone, midpoint)
	mid := meter.snapshotAt(midpoint)
	if mid.Level <= 0 {
		t.Fatalf("warm-up blend should begin gradually: %+v", mid)
	}
	end := start.Add(audioWarmup + audioWarmupBlend)
	meter.update(tone, end)
	if full := meter.snapshotAt(end); full.Level <= mid.Level || !full.Active {
		t.Fatalf("expected completed warm-up response, mid=%+v full=%+v", mid, full)
	}
}

func TestAudioSnapshotDistinguishesSilenceFromUnavailableCapture(t *testing.T) {
	now := time.Now()
	var meter audioMeter
	meter.update(make([]int16, audioBlockSize), now)
	silent := meter.snapshotAt(now)
	if !silent.CaptureAvailable || silent.Active {
		t.Fatalf("captured silence should remain available but inactive: %+v", silent)
	}
	stale := meter.snapshotAt(now.Add(audioStaleAfter + time.Millisecond))
	if stale.CaptureAvailable || stale.Active {
		t.Fatalf("stale capture should be unavailable: %+v", stale)
	}
}

func TestAudioNormalizationIsBoundedAndResetsWithCapture(t *testing.T) {
	var meter audioMeter
	meter.normalizationRef = 0.01
	if gain := meter.normalizationGain(); gain != 4 {
		t.Fatalf("expected bounded maximum gain, got %.3f", gain)
	}
	meter.normalizationRef = 2
	if gain := meter.normalizationGain(); gain != 0.5 {
		t.Fatalf("expected bounded minimum gain, got %.3f", gain)
	}
	meter.captureStarted = time.Now()
	meter.clear()
	if meter.normalizationRef != 0 || !meter.captureStarted.IsZero() {
		t.Fatalf("capture reset must clear normalization and warm-up state")
	}
}

func TestAudioNormalizationAdaptsSlowerThanVisualRelease(t *testing.T) {
	var meter audioMeter
	meter.normalizationRef = 0.8
	elapsed := audioRelease
	meter.updateNormalization(0.2, elapsed)
	if meter.normalizationRef <= 0.7 {
		t.Fatalf("normalization adapted too quickly: %.3f", meter.normalizationRef)
	}
}

func TestAudioSpectralFluxUsesOnlyPositiveBandChanges(t *testing.T) {
	var meter audioMeter
	var baseline [audioBandCount]float64
	bassBand := audioBandForFrequency(120)
	highBand := audioBandForFrequency(6000)
	baseline[bassBand] = 0.2
	baseline[highBand] = 0.6
	if flux, bass, high := meter.spectrumChanges(baseline); flux != 0 || bass != 0 || high != 0 {
		t.Fatalf("first spectrum must establish the baseline, got flux=%.3f bass=%.3f high=%.3f",
			flux, bass, high)
	}

	changed := baseline
	changed[bassBand] = 0.7
	changed[highBand] = 0.1
	flux, bass, high := meter.spectrumChanges(changed)
	if math.Abs(flux-0.125) > 0.0001 || math.Abs(bass-0.5) > 0.0001 || high != 0 {
		t.Fatalf("expected only the bass rise to contribute, got flux=%.3f bass=%.3f high=%.3f",
			flux, bass, high)
	}

	if flux, bass, high := meter.spectrumChanges([audioBandCount]float64{}); flux != 0 || bass != 0 || high != 0 {
		t.Fatalf("falling energy must not produce flux, got flux=%.3f bass=%.3f high=%.3f",
			flux, bass, high)
	}
}

func TestAudioOnsetsAreSuppressedUntilWarmupCompletes(t *testing.T) {
	start := time.Now()
	silence := make([]int16, audioBlockSize)
	tone := sineSamples(120, 0.5)
	var meter audioMeter

	meter.update(silence, start)
	meter.update(tone, start.Add(audioWarmup/2))
	if snapshot := meter.snapshotAt(start.Add(audioWarmup / 2)); snapshot.OnsetCount != 0 {
		t.Fatalf("warm-up must suppress onset events: %+v", snapshot)
	}

	ready := start.Add(audioWarmup + audioWarmupBlend)
	meter.update(silence, ready)
	meter.update(tone, ready.Add(20*time.Millisecond))
	if snapshot := meter.snapshotAt(ready.Add(20 * time.Millisecond)); snapshot.OnsetCount == 0 {
		t.Fatalf("expected a post-warm-up onset from an abrupt tone: %+v", snapshot)
	}
}

func TestAudioImpulseCreatesOnsetsAndAdvancesVisualRevision(t *testing.T) {
	start := time.Now()
	var meter audioMeter
	meter.captureStarted = start.Add(-audioWarmup - audioWarmupBlend)
	meter.update(make([]int16, audioBlockSize), start)
	before := meter.snapshotAt(start)

	at := start.Add(20 * time.Millisecond)
	meter.update(impulseSamples(0.8), at)
	after := meter.snapshotAt(at)
	if after.OnsetCount == 0 || after.SpectralFlux <= 0 || after.Peak <= 0 {
		t.Fatalf("expected an impulse to create shared transient features: %+v", after)
	}
	if after.Revision <= before.Revision {
		t.Fatalf("new transient state must advance the visual revision: before=%d after=%d",
			before.Revision, after.Revision)
	}

	meter.update(make([]int16, audioBlockSize), at.Add(130*time.Millisecond))
	if decayed := meter.snapshotAt(at.Add(130 * time.Millisecond)); decayed.Revision <= after.Revision {
		t.Fatalf("material transient decay must advance the visual revision: before=%d after=%d",
			after.Revision, decayed.Revision)
	}
}

func TestAudioOnsetClassesUseIndependentCooldowns(t *testing.T) {
	var meter audioMeter
	now := time.Now()
	meter.detectOnsets(now, 0.8, 0.7, 0.6, 0.75)
	if len(meter.onsets) != 3 {
		t.Fatalf("expected general, bass, and high events, got %+v", meter.onsets)
	}
	for index, region := range []audioRegion{audioRegionGeneral, audioRegionBass, audioRegionHigh} {
		onset := meter.onsets[index]
		if onset.region != region || onset.id != uint64(index+1) || onset.position != 0.75 {
			t.Fatalf("unexpected onset %d: %+v", index, onset)
		}
	}

	meter.detectOnsets(now.Add(audioOnsetCooldown), 0.8, 0.7, 0.6, -0.5)
	if len(meter.onsets) != 4 || meter.onsets[3].region != audioRegionGeneral {
		t.Fatalf("general cooldown should elapse before region cooldowns: %+v", meter.onsets)
	}
	meter.detectOnsets(now.Add(audioRegionCooldown), 0, 0.7, 0.6, -0.5)
	if len(meter.onsets) != 6 ||
		meter.onsets[4].region != audioRegionBass ||
		meter.onsets[5].region != audioRegionHigh {
		t.Fatalf("expected independent bass and high events after their cooldown: %+v", meter.onsets)
	}
}

func TestAudioOnsetHistoryIsBoundedOrderedAndImmutable(t *testing.T) {
	var meter audioMeter
	start := time.Now()
	for index := range audioEventCapacity + 2 {
		meter.addOnset(
			start.Add(time.Duration(index)*time.Millisecond),
			0.4+float64(index)/100,
			audioRegion(index%3),
			float64(index)/10,
		)
	}
	meter.lastUpdate = start.Add(20 * time.Millisecond)

	snapshot := meter.snapshotAt(start.Add(20 * time.Millisecond))
	if snapshot.OnsetCount != audioEventCapacity {
		t.Fatalf("expected a fixed %d-event history, got %d", audioEventCapacity, snapshot.OnsetCount)
	}
	if capacity := cap(meter.onsets); capacity > audioEventCapacity {
		t.Fatalf("event storage must remain bounded, capacity=%d", capacity)
	}
	if snapshot.Onsets[0].ID != 3 || snapshot.Onsets[audioEventCapacity-1].ID != 10 {
		t.Fatalf("expected oldest events to be discarded in order: %+v", snapshot.Onsets)
	}
	if snapshot.Onsets[0].Age != 18*time.Millisecond {
		t.Fatalf("expected deterministic event age, got %s", snapshot.Onsets[0].Age)
	}

	snapshot.Onsets[0].Strength = 0
	again := meter.snapshotAt(start.Add(20 * time.Millisecond))
	if again.Onsets[0].Strength == 0 {
		t.Fatal("mutating a snapshot must not mutate the meter history")
	}
}

func TestAudioOnsetHistoryExpiresAndCaptureResetClearsDetector(t *testing.T) {
	var meter audioMeter
	start := time.Now()
	meter.addOnset(start, 0.8, audioRegionBass, -0.25)
	firstID := meter.onsets[0].id
	meter.lastUpdate = start.Add(audioEventLifetime)
	if snapshot := meter.snapshotAt(start.Add(audioEventLifetime)); snapshot.OnsetCount != 0 {
		t.Fatalf("expired events must not be exposed: %+v", snapshot)
	}
	meter.pruneOnsets(start.Add(audioEventLifetime))
	if len(meter.onsets) != 0 {
		t.Fatalf("expired events must be pruned, got %+v", meter.onsets)
	}

	meter.spectralFlux = 0.5
	meter.peak = 0.7
	meter.hasPreviousBands = true
	meter.lastBassOnset = start
	meter.clear()
	if meter.spectralFlux != 0 || meter.peak != 0 || meter.hasPreviousBands ||
		len(meter.onsets) != 0 || !meter.lastBassOnset.IsZero() {
		t.Fatalf(
			"capture reset left detector state: flux=%.3f peak=%.3f previous=%t onsets=%d cooldown=%s",
			meter.spectralFlux,
			meter.peak,
			meter.hasPreviousBands,
			len(meter.onsets),
			meter.lastBassOnset,
		)
	}
	meter.addOnset(start.Add(2*audioEventLifetime), 0.5, audioRegionGeneral, 0)
	if meter.onsets[0].id <= firstID {
		t.Fatalf("event IDs must remain monotonic across reconnects: first=%d next=%d",
			firstID, meter.onsets[0].id)
	}
}

func TestAudioPeakHoldDecaysWithElapsedTime(t *testing.T) {
	now := time.Now()
	var meter audioMeter
	meter.captureStarted = now.Add(-audioWarmup - audioWarmupBlend)
	meter.update(sineSamples(880, 0.4), now)
	initial := meter.snapshotAt(now).Peak
	if initial <= 0 {
		t.Fatal("expected active audio to establish a held peak")
	}

	elapsed := 130 * time.Millisecond
	meter.update(make([]int16, audioBlockSize), now.Add(elapsed))
	decayed := meter.snapshotAt(now.Add(elapsed)).Peak
	expected := initial * math.Exp(-float64(elapsed)/float64(audioPeakDecay))
	if math.Abs(decayed-expected) > 0.0001 {
		t.Fatalf("expected time-based peak decay %.6f, got %.6f", expected, decayed)
	}
}

func TestAudioSensitivityScalesAnalyzedEnergy(t *testing.T) {
	samples := sineSamples(880, 0.04)
	now := time.Now()
	var normal audioMeter
	normal.captureStarted = now.Add(-audioWarmup - audioWarmupBlend)
	normal.setSensitivity(1)
	normal.update(samples, now)
	var boosted audioMeter
	boosted.captureStarted = now.Add(-audioWarmup - audioWarmupBlend)
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

func TestSpectrumSoundMapsBassOutsideAndTrebleInside(t *testing.T) {
	bass := audioSnapshot{Active: true, Level: 0.4}
	treble := audioSnapshot{Active: true, Level: 0.4}
	for band := range bass.Bands {
		switch {
		case audioBandCenter(band) < 250:
			bass.Bands[band] = 1
		case audioBandCenter(band) >= 4000:
			treble.Bands[band] = 1
		}
	}

	bassRunes := []rune(spectrumSoundArtWithSnapshot(41, 17, bass))
	trebleRunes := []rune(spectrumSoundArtWithSnapshot(41, 17, treble))
	if !strings.ContainsRune("▅▆▇█", bassRunes[1]) ||
		!strings.ContainsRune("·─━", bassRunes[19]) {
		t.Fatalf("expected bass energy outside the mirrored display, got %q", string(bassRunes))
	}
	if !strings.ContainsRune("▅▆▇█", trebleRunes[19]) ||
		!strings.ContainsRune("·─━", trebleRunes[1]) {
		t.Fatalf("expected treble energy near the center, got %q", string(trebleRunes))
	}
	if bassRunes[1] != bassRunes[39] || trebleRunes[19] != trebleRunes[21] {
		t.Fatal("spectrum_sound must preserve mirrored pairs")
	}
}

func TestSpectrumSoundPeakAndSilenceRemainRecognizable(t *testing.T) {
	originalSeed := animationSeed
	animationSeed = 0x5eed
	t.Cleanup(func() {
		animationSeed = originalSeed
	})

	quietFrames := map[string]bool{}
	for _, phase := range []int{0, 47, 113, 229} {
		frame := spectrumSoundArtWithSnapshot(41, phase, audioSnapshot{})
		runes := []rune(frame)
		if runes[0] != '⟨' || runes[len(runes)-1] != '⟩' || runes[len(runes)/2] != '┃' {
			t.Fatalf("expected a bracketed symmetric idle pulse, got %q", frame)
		}
		for index := 1; index < len(runes)/2; index++ {
			if runes[index] != runes[len(runes)-1-index] {
				t.Fatalf("idle spectrum lost symmetry at %d: %q", index, frame)
			}
		}
		quietFrames[frame] = true
	}
	if len(quietFrames) < 2 {
		t.Fatalf("expected a slowly breathing silent pulse, got %v", quietFrames)
	}

	peak := audioSnapshot{Active: true, Level: 0.4, Peak: 0.8, Centroid: 0.75}
	for index := range peak.Bands {
		peak.Bands[index] = 0.4
	}
	frame := []rune(spectrumSoundArtWithSnapshot(41, 23, peak))
	focusRadius := 1 + int(math.Round((1-peak.Centroid)*18))
	left, right := spectrumPairPositions(41, 20, focusRadius)
	if frame[left] != '┃' || frame[right] != '┃' {
		t.Fatalf("expected peak-hold accents at the centroid focus, got %q", string(frame))
	}
}

func TestWaveSoundUsesAudioFeaturesAndOnsetBreakers(t *testing.T) {
	audio := audioSnapshot{
		Active:     true,
		Level:      0.65,
		Bass:       0.8,
		LowMid:     0.55,
		HighMid:    1,
		Treble:     1,
		OnsetCount: 1,
	}
	audio.Onsets[0] = audioOnset{
		ID:       7,
		Age:      150 * time.Millisecond,
		Strength: 1,
		Region:   audioRegionGeneral,
		Position: 0.8,
	}
	frame := waveSoundArtWithSnapshot(80, 31, audio)
	if !strings.ContainsAny(frame, "◜◝◞◟╱╲") {
		t.Fatalf("expected a breaker from the recent onset, got %q", frame)
	}
	if !strings.ContainsRune(frame, '•') {
		t.Fatalf("expected bounded treble spray over high-mid foam, got %q", frame)
	}

	withoutOnset := audio
	withoutOnset.OnsetCount = 0
	withoutOnset.Onsets = [audioEventCapacity]audioOnset{}
	if calm := waveSoundArtWithSnapshot(80, 31, withoutOnset); calm == frame {
		t.Fatal("recent onset must change the wave without renderer-owned state")
	}
	if repeated := waveSoundArtWithSnapshot(80, 31, audio); repeated != frame {
		t.Fatal("fixed phase and audio snapshot must render deterministically")
	}
}

func TestWaveSoundBreakerDirectionFollowsStereoPosition(t *testing.T) {
	rightward := audioOnset{
		ID:       1,
		Strength: 1,
		Region:   audioRegionGeneral,
		Position: 0.8,
	}
	leftward := rightward
	leftward.Position = -0.8

	rightward.Age = 150 * time.Millisecond
	rightStart, _, ok := waveSoundBreaker(80, rightward)
	if !ok {
		t.Fatal("expected a live rightward breaker")
	}
	rightward.Age = 600 * time.Millisecond
	rightEnd, _, _ := waveSoundBreaker(80, rightward)
	if rightEnd <= rightStart {
		t.Fatalf("positive stereo position must travel right: start=%.2f end=%.2f", rightStart, rightEnd)
	}

	leftward.Age = 150 * time.Millisecond
	leftStart, _, ok := waveSoundBreaker(80, leftward)
	if !ok {
		t.Fatal("expected a live leftward breaker")
	}
	leftward.Age = 600 * time.Millisecond
	leftEnd, _, _ := waveSoundBreaker(80, leftward)
	if leftEnd >= leftStart {
		t.Fatalf("negative stereo position must travel left: start=%.2f end=%.2f", leftStart, leftEnd)
	}
	if _, _, live := waveSoundBreaker(80, audioOnset{
		Age:      time.Second,
		Strength: 1,
		Region:   audioRegionGeneral,
	}); live {
		t.Fatal("expired breaker motion must not survive in the renderer")
	}
}

func TestWaveSoundSilenceIsASmallContinuousTide(t *testing.T) {
	frames := map[string]bool{}
	for _, phase := range []int{0, 47, 113, 229} {
		frame := waveSoundArtWithSnapshot(80, phase, audioSnapshot{})
		if !strings.ContainsAny(frame, "▁▂▃") || !strings.ContainsAny(frame, "◜╲") {
			t.Fatalf("expected a recognizable low-energy tide, got %q", frame)
		}
		frames[frame] = true
	}
	if len(frames) < 2 {
		t.Fatalf("silent tide must keep slow organic motion, got %v", frames)
	}
}

func TestAudioMotionScalesVisualSnapshot(t *testing.T) {
	snapshot := audioSnapshot{
		Active:       true,
		Level:        0.4,
		SpectralFlux: 0.35,
		Peak:         0.6,
		OnsetCount:   1,
	}
	snapshot.Bands[5] = 0.45
	snapshot.Onsets[0] = audioOnset{ID: 9, Age: 20 * time.Millisecond, Strength: 0.5}

	quiet := scaleAudioSnapshot(snapshot, 0.5)
	strong := scaleAudioSnapshot(snapshot, 2)
	if quiet.Level >= snapshot.Level ||
		quiet.Bands[5] >= snapshot.Bands[5] ||
		quiet.SpectralFlux >= snapshot.SpectralFlux ||
		quiet.Peak >= snapshot.Peak ||
		quiet.Onsets[0].Strength >= snapshot.Onsets[0].Strength {
		t.Fatalf("expected reduced motion response, got %+v", quiet)
	}
	if strong.Level <= snapshot.Level ||
		strong.Bands[5] <= snapshot.Bands[5] ||
		strong.SpectralFlux <= snapshot.SpectralFlux ||
		strong.Peak <= snapshot.Peak ||
		strong.Onsets[0].Strength <= snapshot.Onsets[0].Strength {
		t.Fatalf("expected increased motion response, got %+v", strong)
	}
	if quiet.Onsets[0].ID != snapshot.Onsets[0].ID ||
		quiet.Onsets[0].Age != snapshot.Onsets[0].Age {
		t.Fatal("motion scaling must preserve event identity and age")
	}
}

func TestAudioPresetActivationIsScoped(t *testing.T) {
	originalRotation := rotationPresets
	t.Cleanup(func() {
		rotationPresets = originalRotation
	})

	for name := range soundPresetNames {
		if !presetUsesAudio(name) {
			t.Fatalf("expected %s to request audio directly", name)
		}
	}
	for _, name := range []string{"aurora", "spectrum", "wave"} {
		if presetUsesAudio(name) {
			t.Fatalf("base preset %s must not request audio", name)
		}
	}
	rotationPresets = []string{"aurora", "ripples_sound"}
	if !presetUsesAudio(rotationSelection) {
		t.Fatal("expected a rotation containing a sound companion to request audio")
	}
	if !presetListUsesAudio([]string{"aurora", "square_sound"}) ||
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

func impulseSamples(amplitude float64) []int16 {
	samples := make([]int16, audioBlockSize)
	samples[audioBlockSize/2] = int16(math.Min(1, amplitude) * 32767)
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

func skipAudioWarmup(meter *audioMeter) {
	meter.captureStarted = time.Now().Add(-audioWarmup - audioWarmupBlend)
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

func audioBandForFrequency(target float64) int {
	closest := 0
	for band := 1; band < audioBandCount; band++ {
		if math.Abs(audioBandCenter(band)-target) < math.Abs(audioBandCenter(closest)-target) {
			closest = band
		}
	}
	return closest
}
