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
	audioBandCount  = 32
	audioBlockSize  = 2048
	audioHopSize    = 512
	audioSampleRate = 24_000
	audioStaleAfter = 300 * time.Millisecond
	audioRetryDelay = 1500 * time.Millisecond
)

type audioSnapshot struct {
	Bands    [audioBandCount]float64
	Level    float64
	Active   bool
	Sequence uint64
}

type audioMeter struct {
	mu          sync.RWMutex
	bands       [audioBandCount]float64
	level       float64
	sensitivity float64
	lastUpdate  time.Time
	sequence    uint64
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
		"--rate=24000",
		"--channels=1",
		"--latency-msec=60",
		"--process-time-msec=20",
		"--client-name=sway-title-animator",
		"--stream-name=sway-title-animator-meter",
	}
}

func (meter *audioMeter) consume(reader io.Reader) error {
	buffer := make([]byte, audioHopSize*2)
	window := make([]int16, audioBlockSize)
	filled := 0
	for {
		if _, err := io.ReadFull(reader, buffer); err != nil {
			return err
		}
		if filled < audioBlockSize {
			for index := range audioHopSize {
				window[filled+index] = int16(binary.LittleEndian.Uint16(buffer[index*2:]))
			}
			filled += audioHopSize
			if filled < audioBlockSize {
				continue
			}
		} else {
			copy(window, window[audioHopSize:])
			for index := range audioHopSize {
				window[audioBlockSize-audioHopSize+index] =
					int16(binary.LittleEndian.Uint16(buffer[index*2:]))
			}
		}
		meter.update(window, time.Now())
	}
}

func (meter *audioMeter) update(samples []int16, now time.Time) {
	targetBands, targetLevel := analyzeAudioBlock(samples)
	meter.mu.Lock()
	defer meter.mu.Unlock()

	sensitivity := meter.sensitivity
	if sensitivity <= 0 {
		sensitivity = defaultAudioSensitivity
	}
	for index, target := range targetBands {
		target = math.Min(1, target*sensitivity)
		factor := 0.16
		if target > meter.bands[index] {
			factor = 0.68
		}
		meter.bands[index] += (target - meter.bands[index]) * factor
	}
	targetLevel = math.Min(1, targetLevel*sensitivity)
	levelFactor := 0.14
	if targetLevel > meter.level {
		levelFactor = 0.72
	}
	meter.level += (targetLevel - meter.level) * levelFactor
	meter.lastUpdate = now
	meter.sequence++
}

func (meter *audioMeter) setSensitivity(sensitivity float64) {
	meter.mu.Lock()
	defer meter.mu.Unlock()
	meter.sensitivity = sensitivity
}

func (meter *audioMeter) snapshot() audioSnapshot {
	meter.mu.RLock()
	defer meter.mu.RUnlock()

	if meter.lastUpdate.IsZero() || time.Since(meter.lastUpdate) > audioStaleAfter {
		return audioSnapshot{Sequence: meter.sequence}
	}
	return audioSnapshot{
		Bands:    meter.bands,
		Level:    meter.level,
		Active:   meter.level > 0.025,
		Sequence: meter.sequence,
	}
}

func (meter *audioMeter) clear() {
	meter.mu.Lock()
	defer meter.mu.Unlock()
	clear(meter.bands[:])
	meter.level = 0
	meter.lastUpdate = time.Time{}
	meter.sequence++
}

func analyzeAudioBlock(samples []int16) ([audioBandCount]float64, float64) {
	var bands [audioBandCount]float64
	if len(samples) != audioBlockSize {
		return bands, 0
	}

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
		return bands, 0
	}

	fft(realValues, imaginaryValues)
	const (
		minFrequency = 55.0
		maxFrequency = 11_000.0
	)
	for band := range audioBandCount {
		lowFrequency := minFrequency * math.Pow(maxFrequency/minFrequency, float64(band)/audioBandCount)
		highFrequency := minFrequency * math.Pow(maxFrequency/minFrequency, float64(band+1)/audioBandCount)
		firstBin, lastBin := audioBandBinRange(lowFrequency, highFrequency)
		peak := 0.0
		for bin := firstBin; bin <= lastBin; bin++ {
			magnitude := math.Hypot(realValues[bin], imaginaryValues[bin]) / audioBlockSize
			peak = math.Max(peak, magnitude)
		}
		bands[band] = math.Min(1, math.Sqrt(peak*8))
	}
	return bands, math.Min(1, math.Sqrt(rms*4))
}

func audioBandBinRange(lowFrequency float64, highFrequency float64) (int, int) {
	firstBin := max(1, int(math.Ceil(lowFrequency*audioBlockSize/audioSampleRate)))
	lastBin := min(audioBlockSize/2-1, int(math.Ceil(highFrequency*audioBlockSize/audioSampleRate))-1)
	return firstBin, max(firstBin, lastBin)
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
