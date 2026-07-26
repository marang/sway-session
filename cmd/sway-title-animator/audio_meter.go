package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"
)

const (
	audioBandCount      = 32
	audioBlockSize      = 2048
	audioHopSize        = 1024
	audioSampleRate     = 48_000
	audioChannelCount   = 2
	audioAttack         = 100 * time.Millisecond
	audioRelease        = 320 * time.Millisecond
	audioWarmup         = 400 * time.Millisecond
	audioWarmupBlend    = 200 * time.Millisecond
	audioNormalizeUp    = 2 * time.Second
	audioNormalizeDown  = 8 * time.Second
	audioPeakDecay      = 650 * time.Millisecond
	audioEventLifetime  = 1500 * time.Millisecond
	audioOnsetCooldown  = 140 * time.Millisecond
	audioRegionCooldown = 180 * time.Millisecond
	audioEventCapacity  = 8
	audioFluxThreshold  = 0.08
	audioBassThreshold  = 0.16
	audioHighThreshold  = 0.16
	audioStaleAfter     = 300 * time.Millisecond
	audioRetryDelay     = 1500 * time.Millisecond
)

type audioRegion uint8

const (
	audioRegionGeneral audioRegion = iota
	audioRegionBass
	audioRegionHigh
)

type audioOnset struct {
	ID       uint64
	Age      time.Duration
	Strength float64
	Region   audioRegion
	Position float64
}

type audioSnapshot struct {
	Bands        [audioBandCount]float64
	Level        float64
	Bass         float64
	LowMid       float64
	HighMid      float64
	Treble       float64
	Centroid     float64
	LeftLevel    float64
	RightLevel   float64
	Balance      float64
	SpectralFlux float64
	Peak         float64
	Onsets       [audioEventCapacity]audioOnset
	OnsetCount   int
	Active       bool
	Revision     uint64
}

type audioAnalysis struct {
	Bands      [audioBandCount]float64
	Level      float64
	Bass       float64
	LowMid     float64
	HighMid    float64
	Treble     float64
	Centroid   float64
	LeftLevel  float64
	RightLevel float64
	Balance    float64
}

type audioOnsetState struct {
	id         uint64
	occurredAt time.Time
	strength   float64
	region     audioRegion
	position   float64
}

type audioMeter struct {
	mu                sync.RWMutex
	bands             [audioBandCount]float64
	level             float64
	bass              float64
	lowMid            float64
	highMid           float64
	treble            float64
	centroid          float64
	leftLevel         float64
	rightLevel        float64
	balance           float64
	spectralFlux      float64
	peak              float64
	previousBands     [audioBandCount]float64
	hasPreviousBands  bool
	onsets            []audioOnsetState
	nextOnsetID       uint64
	lastGeneralOnset  time.Time
	lastBassOnset     time.Time
	lastHighOnset     time.Time
	sensitivity       float64
	normalizationRef  float64
	captureStarted    time.Time
	lastUpdate        time.Time
	revision          uint64
	visualFingerprint uint64
}

type audioCaptureBackend interface {
	Name() string
	Available() error
	Capture(context.Context, func(io.Reader) error) error
}

type parecCaptureBackend struct {
	executable string
	device     string
}

var (
	defaultAudioMeter    audioMeter
	currentAudioSnapshot = defaultAudioMeter.snapshot
)

func startDefaultAudioMonitor() func() {
	return startAudioMonitor(defaultAudioCaptureBackend(), &defaultAudioMeter, os.Stderr)
}

func defaultAudioCaptureBackend() audioCaptureBackend {
	return parecCaptureBackend{executable: "parec", device: audioSettings.Device}
}

func startAudioMonitor(backend audioCaptureBackend, meter *audioMeter, diagnostics io.Writer) func() {
	meter.setSensitivity(audioSettings.Sensitivity)
	if err := backend.Available(); err != nil {
		fmt.Fprintf(diagnostics, "sound-reactive presets: %s is unavailable: %v; install the PulseAudio command-line utilities or check the configured playback monitor\n", backend.Name(), err)
		meter.clear()
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		meter.run(ctx, backend, diagnostics, audioRetryDelay)
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(750 * time.Millisecond):
			}
		})
	}
}

func (meter *audioMeter) run(
	ctx context.Context,
	backend audioCaptureBackend,
	diagnostics io.Writer,
	retryDelay time.Duration,
) {
	reported := false
	report := func(err error) {
		if err == nil || ctx.Err() != nil || reported {
			return
		}
		reported = true
		fmt.Fprintf(diagnostics, "sound-reactive presets: %s capture unavailable: %v; retrying\n", backend.Name(), err)
	}
	meter.runCaptureLoop(ctx, func(ctx context.Context) error {
		return backend.Capture(ctx, meter.consume)
	}, retryDelay, report)
}

func (meter *audioMeter) runCaptureLoop(
	ctx context.Context,
	capture func(context.Context) error,
	retryDelay time.Duration,
	report func(error),
) {
	for {
		if ctx.Err() != nil {
			return
		}
		err := capture(ctx)
		meter.clear()
		if ctx.Err() != nil {
			return
		}
		if report != nil {
			report(err)
		}

		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (backend parecCaptureBackend) Name() string {
	return "parec"
}

func (backend parecCaptureBackend) Available() error {
	_, err := exec.LookPath(backend.executable)
	return err
}

func (backend parecCaptureBackend) Capture(ctx context.Context, consume func(io.Reader) error) error {
	command := exec.CommandContext(ctx, backend.executable, backend.arguments()...)
	output, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return err
	}

	readErr := consume(output)
	if readErr != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return readErr
	}
	return command.Wait()
}

func (backend parecCaptureBackend) arguments() []string {
	return []string{
		"--record",
		"--device=" + backend.device,
		"--raw",
		"--format=s16le",
		"--rate=48000",
		"--channels=2",
		"--latency-msec=60",
		"--process-time-msec=20",
		"--client-name=sway-title-animator",
		"--stream-name=sway-title-animator-meter",
	}
}

func (meter *audioMeter) consume(reader io.Reader) error {
	buffer := make([]byte, audioHopSize*audioChannelCount*2)
	leftWindow := make([]int16, audioBlockSize)
	rightWindow := make([]int16, audioBlockSize)
	filled := 0
	for {
		if _, err := io.ReadFull(reader, buffer); err != nil {
			return err
		}
		if filled < audioBlockSize {
			for index := range audioHopSize {
				offset := index * audioChannelCount * 2
				leftWindow[filled+index] = int16(binary.LittleEndian.Uint16(buffer[offset:]))
				rightWindow[filled+index] = int16(binary.LittleEndian.Uint16(buffer[offset+2:]))
			}
			filled += audioHopSize
			if filled < audioBlockSize {
				continue
			}
		} else {
			copy(leftWindow, leftWindow[audioHopSize:])
			copy(rightWindow, rightWindow[audioHopSize:])
			for index := range audioHopSize {
				offset := index * audioChannelCount * 2
				target := audioBlockSize - audioHopSize + index
				leftWindow[target] = int16(binary.LittleEndian.Uint16(buffer[offset:]))
				rightWindow[target] = int16(binary.LittleEndian.Uint16(buffer[offset+2:]))
			}
		}
		meter.updateStereo(leftWindow, rightWindow, time.Now())
	}
}

func (meter *audioMeter) update(samples []int16, now time.Time) {
	meter.updateStereo(samples, samples, now)
}

func (meter *audioMeter) updateStereo(left []int16, right []int16, now time.Time) {
	target := analyzeStereoAudioBlock(left, right)
	meter.mu.Lock()
	defer meter.mu.Unlock()

	sensitivity := meter.sensitivity
	if sensitivity <= 0 {
		sensitivity = defaultAudioSensitivity
	}
	elapsed := time.Second * audioHopSize / audioSampleRate
	if !meter.lastUpdate.IsZero() && now.After(meter.lastUpdate) {
		elapsed = now.Sub(meter.lastUpdate)
	}
	if meter.captureStarted.IsZero() {
		meter.captureStarted = now
	}
	meter.updateNormalization(target.Level, elapsed)
	for index, value := range target.Bands {
		target.Bands[index] = math.Min(1, value*sensitivity)
	}
	target.Level = math.Min(1, target.Level*sensitivity)
	target.Bass = math.Min(1, target.Bass*sensitivity)
	target.LowMid = math.Min(1, target.LowMid*sensitivity)
	target.HighMid = math.Min(1, target.HighMid*sensitivity)
	target.Treble = math.Min(1, target.Treble*sensitivity)
	target.LeftLevel = math.Min(1, target.LeftLevel*sensitivity)
	target.RightLevel = math.Min(1, target.RightLevel*sensitivity)
	gain := meter.normalizationGain()
	scaleAudioEnergy(&target, gain)
	flux, bassRise, highRise := meter.spectrumChanges(target.Bands)
	meter.pruneOnsets(now)
	warmed := now.Sub(meter.captureStarted) >= audioWarmup+audioWarmupBlend
	if warmed && target.Level > 0.025 {
		meter.detectOnsets(now, flux, bassRise, highRise, target.Balance)
	}
	warmupScale := math.Max(0, math.Min(1,
		float64(now.Sub(meter.captureStarted)-audioWarmup)/float64(audioWarmupBlend),
	))
	scaleAudioAnalysis(&target, warmupScale)
	for index, value := range target.Bands {
		meter.bands[index] = smoothAudioValue(meter.bands[index], value, elapsed)
	}
	meter.level = smoothAudioValue(meter.level, target.Level, elapsed)
	meter.bass = smoothAudioValue(meter.bass, target.Bass, elapsed)
	meter.lowMid = smoothAudioValue(meter.lowMid, target.LowMid, elapsed)
	meter.highMid = smoothAudioValue(meter.highMid, target.HighMid, elapsed)
	meter.treble = smoothAudioValue(meter.treble, target.Treble, elapsed)
	meter.centroid = smoothAudioValue(meter.centroid, target.Centroid, elapsed)
	meter.leftLevel = smoothAudioValue(meter.leftLevel, target.LeftLevel, elapsed)
	meter.rightLevel = smoothAudioValue(meter.rightLevel, target.RightLevel, elapsed)
	meter.balance = smoothSignedAudioValue(meter.balance, target.Balance, elapsed)
	meter.spectralFlux = smoothAudioValue(meter.spectralFlux, flux*warmupScale, elapsed)
	meter.peak *= math.Exp(-float64(elapsed) / float64(audioPeakDecay))
	meter.peak = math.Max(meter.peak, target.Level)
	meter.lastUpdate = now
	fingerprint := meter.fingerprint()
	if fingerprint != meter.visualFingerprint {
		meter.visualFingerprint = fingerprint
		meter.revision++
	}
}

func (meter *audioMeter) setSensitivity(sensitivity float64) {
	meter.mu.Lock()
	defer meter.mu.Unlock()
	meter.sensitivity = sensitivity
}

func (meter *audioMeter) snapshot() audioSnapshot {
	return meter.snapshotAt(time.Now())
}

func (meter *audioMeter) snapshotAt(now time.Time) audioSnapshot {
	meter.mu.RLock()
	defer meter.mu.RUnlock()

	if meter.lastUpdate.IsZero() || now.Sub(meter.lastUpdate) > audioStaleAfter {
		return audioSnapshot{Revision: meter.revision}
	}
	snapshot := audioSnapshot{
		Bands:        meter.bands,
		Level:        meter.level,
		Bass:         meter.bass,
		LowMid:       meter.lowMid,
		HighMid:      meter.highMid,
		Treble:       meter.treble,
		Centroid:     meter.centroid,
		LeftLevel:    meter.leftLevel,
		RightLevel:   meter.rightLevel,
		Balance:      meter.balance,
		SpectralFlux: meter.spectralFlux,
		Peak:         meter.peak,
		Active:       meter.level > 0.025,
		Revision:     meter.revision,
	}
	for _, onset := range meter.onsets {
		age := max(time.Duration(0), now.Sub(onset.occurredAt))
		if age >= audioEventLifetime || snapshot.OnsetCount == audioEventCapacity {
			continue
		}
		snapshot.Onsets[snapshot.OnsetCount] = audioOnset{
			ID:       onset.id,
			Age:      age,
			Strength: onset.strength,
			Region:   onset.region,
			Position: onset.position,
		}
		snapshot.OnsetCount++
	}
	return snapshot
}

func (meter *audioMeter) clear() {
	meter.mu.Lock()
	defer meter.mu.Unlock()
	clear(meter.bands[:])
	meter.level = 0
	meter.bass = 0
	meter.lowMid = 0
	meter.highMid = 0
	meter.treble = 0
	meter.centroid = 0
	meter.leftLevel = 0
	meter.rightLevel = 0
	meter.balance = 0
	meter.spectralFlux = 0
	meter.peak = 0
	clear(meter.previousBands[:])
	meter.hasPreviousBands = false
	meter.onsets = meter.onsets[:0]
	meter.lastGeneralOnset = time.Time{}
	meter.lastBassOnset = time.Time{}
	meter.lastHighOnset = time.Time{}
	meter.normalizationRef = 0
	meter.captureStarted = time.Time{}
	meter.lastUpdate = time.Time{}
	if meter.visualFingerprint != 0 {
		meter.visualFingerprint = 0
		meter.revision++
	}
}

func (meter *audioMeter) spectrumChanges(bands [audioBandCount]float64) (float64, float64, float64) {
	if !meter.hasPreviousBands {
		meter.previousBands = bands
		meter.hasPreviousBands = true
		return 0, 0, 0
	}

	positiveSum := 0.0
	bassRise := 0.0
	highRise := 0.0
	for band, value := range bands {
		rise := math.Max(0, value-meter.previousBands[band])
		positiveSum += rise
		frequency := audioBandCenter(band)
		switch {
		case frequency < 250:
			bassRise = math.Max(bassRise, rise)
		case frequency >= 4000:
			highRise = math.Max(highRise, rise)
		}
	}
	meter.previousBands = bands
	return math.Min(1, positiveSum/4), bassRise, highRise
}

func (meter *audioMeter) detectOnsets(
	now time.Time,
	flux float64,
	bassRise float64,
	highRise float64,
	position float64,
) {
	if flux >= audioFluxThreshold && cooldownElapsed(now, meter.lastGeneralOnset, audioOnsetCooldown) {
		meter.addOnset(now, onsetStrength(flux, audioFluxThreshold), audioRegionGeneral, position)
		meter.lastGeneralOnset = now
	}
	if bassRise >= audioBassThreshold && cooldownElapsed(now, meter.lastBassOnset, audioRegionCooldown) {
		meter.addOnset(now, onsetStrength(bassRise, audioBassThreshold), audioRegionBass, position)
		meter.lastBassOnset = now
	}
	if highRise >= audioHighThreshold && cooldownElapsed(now, meter.lastHighOnset, audioRegionCooldown) {
		meter.addOnset(now, onsetStrength(highRise, audioHighThreshold), audioRegionHigh, position)
		meter.lastHighOnset = now
	}
}

func cooldownElapsed(now time.Time, previous time.Time, cooldown time.Duration) bool {
	return previous.IsZero() || now.Sub(previous) >= cooldown
}

func onsetStrength(value float64, threshold float64) float64 {
	progress := (value - threshold) / (1 - threshold)
	return math.Max(0.25, math.Min(1, 0.25+progress*0.75))
}

func (meter *audioMeter) addOnset(
	now time.Time,
	strength float64,
	region audioRegion,
	position float64,
) {
	meter.nextOnsetID++
	onset := audioOnsetState{
		id:         meter.nextOnsetID,
		occurredAt: now,
		strength:   strength,
		region:     region,
		position:   math.Max(-1, math.Min(1, position)),
	}
	if len(meter.onsets) == audioEventCapacity {
		copy(meter.onsets, meter.onsets[1:])
		meter.onsets[len(meter.onsets)-1] = onset
		return
	}
	meter.onsets = append(meter.onsets, onset)
}

func (meter *audioMeter) pruneOnsets(now time.Time) {
	firstLive := 0
	for firstLive < len(meter.onsets) &&
		now.Sub(meter.onsets[firstLive].occurredAt) >= audioEventLifetime {
		firstLive++
	}
	if firstLive == 0 {
		return
	}
	copy(meter.onsets, meter.onsets[firstLive:])
	meter.onsets = meter.onsets[:len(meter.onsets)-firstLive]
}

func (meter *audioMeter) updateNormalization(level float64, elapsed time.Duration) {
	if level < 0.005 {
		return
	}
	if meter.normalizationRef == 0 {
		meter.normalizationRef = level
		return
	}
	tau := audioNormalizeDown
	if level > meter.normalizationRef {
		tau = audioNormalizeUp
	}
	factor := 1 - math.Exp(-float64(max(elapsed, time.Millisecond))/float64(tau))
	meter.normalizationRef += (level - meter.normalizationRef) * factor
}

func (meter *audioMeter) normalizationGain() float64 {
	if meter.normalizationRef <= 0 {
		return 1
	}
	return math.Max(0.5, math.Min(4, 0.55/meter.normalizationRef))
}

func scaleAudioAnalysis(analysis *audioAnalysis, scale float64) {
	scaleAudioEnergy(analysis, scale)
	analysis.Centroid *= scale
	analysis.Balance *= scale
}

func scaleAudioEnergy(analysis *audioAnalysis, scale float64) {
	for index := range analysis.Bands {
		analysis.Bands[index] = math.Min(1, analysis.Bands[index]*scale)
	}
	analysis.Level = math.Min(1, analysis.Level*scale)
	analysis.Bass = math.Min(1, analysis.Bass*scale)
	analysis.LowMid = math.Min(1, analysis.LowMid*scale)
	analysis.HighMid = math.Min(1, analysis.HighMid*scale)
	analysis.Treble = math.Min(1, analysis.Treble*scale)
	analysis.LeftLevel = math.Min(1, analysis.LeftLevel*scale)
	analysis.RightLevel = math.Min(1, analysis.RightLevel*scale)
}

func analyzeAudioBlock(samples []int16) ([audioBandCount]float64, float64) {
	analysis := analyzeStereoAudioBlock(samples, samples)
	return analysis.Bands, analysis.Level
}

func analyzeStereoAudioBlock(left []int16, right []int16) audioAnalysis {
	if len(left) != audioBlockSize || len(right) != audioBlockSize {
		return audioAnalysis{}
	}
	leftBands, leftLevel, leftCentroid := analyzeMonoSpectrum(left)
	rightBands, rightLevel, rightCentroid := analyzeMonoSpectrum(right)
	var bands [audioBandCount]float64
	for index := range bands {
		bands[index] = (leftBands[index] + rightBands[index]) / 2
	}
	level := (leftLevel + rightLevel) / 2
	centroid := 0.0
	if level > 0 {
		centroid = (leftCentroid*leftLevel + rightCentroid*rightLevel) /
			(leftLevel + rightLevel)
	}
	balance := 0.0
	if total := leftLevel + rightLevel; total > 0.0001 {
		balance = (rightLevel - leftLevel) / total
	}
	return audioAnalysis{
		Bands:      bands,
		Level:      level,
		Bass:       aggregateBandEnergy(bands, 55, 250),
		LowMid:     aggregateBandEnergy(bands, 250, 1000),
		HighMid:    aggregateBandEnergy(bands, 1000, 4000),
		Treble:     aggregateBandEnergy(bands, 4000, 11_000),
		Centroid:   centroid,
		LeftLevel:  leftLevel,
		RightLevel: rightLevel,
		Balance:    balance,
	}
}

func analyzeMonoSpectrum(samples []int16) ([audioBandCount]float64, float64, float64) {
	var bands [audioBandCount]float64
	realValues := make([]float64, audioBlockSize)
	imaginaryValues := make([]float64, audioBlockSize)
	sumSquares := 0.0
	mean := 0.0
	for _, sample := range samples {
		mean += float64(sample) / 32768
	}
	mean /= audioBlockSize
	for index, sample := range samples {
		value := float64(sample)/32768 - mean
		sumSquares += value * value
		window := 0.5 - 0.5*math.Cos(2*math.Pi*float64(index)/float64(audioBlockSize-1))
		realValues[index] = value * window
	}
	rms := math.Sqrt(sumSquares / audioBlockSize)
	if rms < 0.0008 {
		return bands, 0, 0
	}

	fft(realValues, imaginaryValues)
	const (
		minFrequency = 55.0
		maxFrequency = 11_000.0
	)
	ranges := audioBandRanges()
	weightedFrequency := 0.0
	totalMagnitude := 0.0
	for band, binRange := range ranges {
		peak := 0.0
		for bin := binRange[0]; bin <= binRange[1]; bin++ {
			magnitude := math.Hypot(realValues[bin], imaginaryValues[bin]) / audioBlockSize
			peak = math.Max(peak, magnitude)
			frequency := float64(bin) * audioSampleRate / audioBlockSize
			weightedFrequency += frequency * magnitude
			totalMagnitude += magnitude
		}
		bands[band] = math.Min(1, math.Sqrt(peak*8))
	}
	centroid := 0.0
	if totalMagnitude > 0 {
		frequency := math.Max(minFrequency, math.Min(maxFrequency, weightedFrequency/totalMagnitude))
		centroid = math.Log(frequency/minFrequency) / math.Log(maxFrequency/minFrequency)
	}
	return bands, math.Min(1, math.Sqrt(rms*4)), centroid
}

func audioBandRanges() [audioBandCount][2]int {
	const (
		minFrequency = 55.0
		maxFrequency = 11_000.0
	)
	var ranges [audioBandCount][2]int
	previousLast := int(math.Ceil(minFrequency*audioBlockSize/audioSampleRate)) - 1
	for band := range audioBandCount {
		highFrequency := minFrequency * math.Pow(maxFrequency/minFrequency, float64(band+1)/audioBandCount)
		first := previousLast + 1
		last := min(audioBlockSize/2-1, int(math.Ceil(highFrequency*audioBlockSize/audioSampleRate))-1)
		last = max(first, last)
		ranges[band] = [2]int{first, last}
		previousLast = last
	}
	return ranges
}

func aggregateBandEnergy(bands [audioBandCount]float64, lowFrequency float64, highFrequency float64) float64 {
	total := 0.0
	count := 0
	for band, energy := range bands {
		frequency := audioBandCenter(band)
		if frequency >= lowFrequency && frequency < highFrequency {
			total += energy
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return total / float64(count)
}

func audioBandCenter(band int) float64 {
	const (
		minFrequency = 55.0
		maxFrequency = 11_000.0
	)
	position := (float64(band) + 0.5) / audioBandCount
	return minFrequency * math.Pow(maxFrequency/minFrequency, position)
}

func smoothAudioValue(current float64, target float64, elapsed time.Duration) float64 {
	const minimumElapsed = time.Millisecond
	if elapsed < minimumElapsed {
		elapsed = minimumElapsed
	}
	tau := audioRelease
	if target > current {
		tau = audioAttack
	}
	factor := 1 - math.Exp(-float64(elapsed)/float64(tau))
	return current + (target-current)*factor
}

func smoothSignedAudioValue(current float64, target float64, elapsed time.Duration) float64 {
	if math.Abs(target) > math.Abs(current) {
		factor := 1 - math.Exp(-float64(max(elapsed, time.Millisecond))/float64(audioAttack))
		return current + (target-current)*factor
	}
	factor := 1 - math.Exp(-float64(max(elapsed, time.Millisecond))/float64(audioRelease))
	return current + (target-current)*factor
}

func (meter *audioMeter) fingerprint() uint64 {
	visible := meter.level > 1.0/256 ||
		meter.bass > 1.0/256 ||
		meter.lowMid > 1.0/256 ||
		meter.highMid > 1.0/256 ||
		meter.treble > 1.0/256 ||
		meter.centroid > 1.0/256 ||
		meter.leftLevel > 1.0/256 ||
		meter.rightLevel > 1.0/256 ||
		math.Abs(meter.balance) > 1.0/256 ||
		meter.spectralFlux > 1.0/256 ||
		meter.peak > 1.0/256 ||
		len(meter.onsets) > 0
	if !visible {
		for _, value := range meter.bands {
			if value > 1.0/256 {
				visible = true
				break
			}
		}
	}
	if !visible {
		return 0
	}
	hash := uint64(1469598103934665603)
	add := func(value float64, signed bool) {
		if signed {
			value = (math.Max(-1, math.Min(1, value)) + 1) / 2
		} else {
			value = math.Max(0, math.Min(1, value))
		}
		quantized := uint64(math.Round(value * 128))
		hash ^= quantized
		hash *= 1099511628211
	}
	for _, value := range meter.bands {
		add(value, false)
	}
	for _, value := range []float64{
		meter.level,
		meter.bass,
		meter.lowMid,
		meter.highMid,
		meter.treble,
		meter.centroid,
		meter.leftLevel,
		meter.rightLevel,
		meter.spectralFlux,
		meter.peak,
	} {
		add(value, false)
	}
	add(meter.balance, true)
	for _, onset := range meter.onsets {
		hash ^= onset.id
		hash *= 1099511628211
		hash ^= uint64(onset.region) + 1
		hash *= 1099511628211
		add(onset.strength, false)
		add(onset.position, true)
	}
	if meter.level > 0.025 {
		hash ^= 1
		hash *= 1099511628211
	}
	return hash
}

func fft(realValues []float64, imaginaryValues []float64) {
	size := len(realValues)
	for index, reversed := 1, 0; index < size; index++ {
		bit := size >> 1
		for reversed&bit != 0 {
			reversed ^= bit
			bit >>= 1
		}
		reversed ^= bit
		if index < reversed {
			realValues[index], realValues[reversed] = realValues[reversed], realValues[index]
			imaginaryValues[index], imaginaryValues[reversed] = imaginaryValues[reversed], imaginaryValues[index]
		}
	}

	for length := 2; length <= size; length <<= 1 {
		angle := -2 * math.Pi / float64(length)
		stepReal, stepImaginary := math.Cos(angle), math.Sin(angle)
		for start := 0; start < size; start += length {
			twiddleReal, twiddleImaginary := 1.0, 0.0
			for offset := 0; offset < length/2; offset++ {
				even := start + offset
				odd := even + length/2
				oddReal := realValues[odd]*twiddleReal - imaginaryValues[odd]*twiddleImaginary
				oddImaginary := realValues[odd]*twiddleImaginary + imaginaryValues[odd]*twiddleReal
				realValues[odd] = realValues[even] - oddReal
				imaginaryValues[odd] = imaginaryValues[even] - oddImaginary
				realValues[even] += oddReal
				imaginaryValues[even] += oddImaginary
				twiddleReal, twiddleImaginary =
					twiddleReal*stepReal-twiddleImaginary*stepImaginary,
					twiddleReal*stepImaginary+twiddleImaginary*stepReal
			}
		}
	}
}

func presetUsesAudio(name string) bool {
	if name == "aurora_sound" {
		return true
	}
	if name != rotationSelection {
		return false
	}
	return slices.Contains(rotationPresets, "aurora_sound")
}

func presetListUsesAudio(names []string) bool {
	return slices.Contains(names, "aurora_sound")
}
