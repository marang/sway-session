package main

import (
	"bytes"
	"errors"
	"math"
	"math/bits"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

type failingEntropyReader struct{}

func (failingEntropyReader) Read([]byte) (int, error) {
	return 0, errors.New("entropy unavailable")
}

func TestAnimationSeedUsesEntropyAndStableFallback(t *testing.T) {
	entropy := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	first := animationSeedFrom(bytes.NewReader(entropy), time.Unix(10, 20), 42)
	second := animationSeedFrom(bytes.NewReader(entropy), time.Unix(99, 88), 7)
	if first == 0 || first != second {
		t.Fatalf("expected nonzero entropy-derived seed, got first=%d second=%d", first, second)
	}

	fallbackTime := time.Unix(1234, 5678)
	fallback := animationSeedFrom(failingEntropyReader{}, fallbackTime, 42)
	repeated := animationSeedFrom(failingEntropyReader{}, fallbackTime, 42)
	if fallback == 0 || fallback != repeated {
		t.Fatalf("expected stable nonzero fallback, got first=%d second=%d", fallback, repeated)
	}
	if fallback == animationSeedFrom(failingEntropyReader{}, fallbackTime, 43) {
		t.Fatal("expected fallback seed to include the process ID")
	}
}

func TestOrganicNoiseIsSeededDeterministicAndBounded(t *testing.T) {
	originalSeed := animationSeed
	t.Cleanup(func() {
		animationSeed = originalSeed
	})

	animationSeed = 100
	first := organicNoise("test", 7, 12.25)
	repeated := organicNoise("test", 7, 12.25)
	if first != repeated {
		t.Fatalf("expected deterministic noise, got %v and %v", first, repeated)
	}
	if first < 0 || first > 1 {
		t.Fatalf("expected bounded noise, got %v", first)
	}
	left := organicNoise("test", 7, 12.99999)
	right := organicNoise("test", 7, 13.00001)
	if difference := left - right; difference > 0.0001 || difference < -0.0001 {
		t.Fatalf("expected smooth noise at epoch boundary, left=%v right=%v", left, right)
	}

	animationSeed = 200
	if changed := organicNoise("test", 7, 12.25); changed == first {
		t.Fatalf("expected a different seed to change noise, got %v", changed)
	}
}

func TestDefaultRotationOrderIncludesNewPresets(t *testing.T) {
	want := []string{
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
		"domino",
	}
	if !slices.Equal(rotationPresets, want) {
		t.Fatalf("expected default rotation %v, got %v", want, rotationPresets)
	}
}

func TestSoundPresetRegistryMatchesRegisteredCompanions(t *testing.T) {
	for name := range soundPresetNames {
		if !strings.HasSuffix(name, "_sound") {
			t.Fatalf("sound preset %q must use the _sound suffix", name)
		}
		if _, ok := animationPresets[name]; !ok {
			t.Fatalf("sound preset %q is not registered", name)
		}
		if slices.Contains(rotationPresets, name) {
			t.Fatalf("sound preset %q must remain opt-in", name)
		}
	}
	for name := range animationPresets {
		if strings.HasSuffix(name, "_sound") && !isSoundPreset(name) {
			t.Fatalf("registered sound companion %q is missing from the audio registry", name)
		}
	}
}

func TestAllBuiltInAnimationsAreBoundedAndMove(t *testing.T) {
	originalSeed := animationSeed
	animationSeed = 0x123456789abcdef
	t.Cleanup(func() {
		animationSeed = originalSeed
	})

	widths := []int{0, 1, 3, 7, 8, 11, 12, 80, 220}
	phases := []int{1, 12, 53, 137, 311, 701}
	for name, animation := range animationPresets {
		t.Run(name, func(t *testing.T) {
			for _, width := range widths {
				for _, phase := range phases {
					frame := animation(width, phase)
					if runes := len([]rune(frame)); runes > artWidth(width) {
						t.Fatalf("width=%d phase=%d rendered %d runes: %q", width, phase, runes, frame)
					}
				}
			}

			frames := map[string]bool{}
			for _, phase := range phases {
				frames[animation(80, phase)] = true
			}
			if len(frames) < 2 {
				t.Fatalf("expected motion across sampled phases, got %q", animation(80, phases[0]))
			}
		})
	}
}

func TestAnimationSeedChangesRepresentativeGalleryFrames(t *testing.T) {
	originalSeed := animationSeed
	t.Cleanup(func() {
		animationSeed = originalSeed
	})

	animationSeed = 111
	first := map[string]string{}
	for name, animation := range animationPresets {
		first[name] = animation(96, 173)
	}
	animationSeed = 222
	changed := 0
	unchanged := make([]string, 0)
	for name, animation := range animationPresets {
		if animation(96, 173) != first[name] {
			changed++
		} else {
			unchanged = append(unchanged, name)
		}
	}
	if changed < len(animationPresets)-2 {
		sort.Strings(unchanged)
		t.Fatalf("expected nearly every preset to respond to the run seed, changed=%d total=%d unchanged=%v",
			changed, len(animationPresets), unchanged)
	}
}

func TestOscilloscopePresetsVaryGeometryOverTime(t *testing.T) {
	originalSeed := animationSeed
	animationSeed = 0x5eed
	t.Cleanup(func() {
		animationSeed = originalSeed
	})

	for _, test := range []struct {
		name          string
		animation     animationFunc
		expectedRunes string
	}{
		{"square", squareArt, "⎺⎽⎡⎤"},
	} {
		t.Run(test.name, func(t *testing.T) {
			frames := map[string]bool{}
			for _, phase := range []int{0, 37, 83, 149, 233, 359} {
				frame := test.animation(80, phase)
				frames[frame] = true
				if frameContainsBraille(frame) {
					t.Fatalf("expected a line-based oscilloscope trace without braille, got %q", frame)
				}
				for _, glyph := range frame {
					if glyph == ' ' {
						continue
					}
					if !strings.ContainsRune(test.expectedRunes, glyph) {
						t.Fatalf("unexpected %s glyph %q in %q", test.name, glyph, frame)
					}
				}
			}
			if len(frames) < 4 {
				t.Fatalf("expected changing period, amplitude, or duty cycle; got %d unique frames", len(frames))
			}
		})
	}
}

func TestOscilloscopePresetsBuildWithoutScrolling(t *testing.T) {
	originalSeed := animationSeed
	animationSeed = 0x5eed
	t.Cleanup(func() {
		animationSeed = originalSeed
	})

	for _, test := range []struct {
		name        string
		animation   animationFunc
		firstPhase  int
		secondPhase int
	}{
		{"square", squareArt, 7, 18},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := []rune(test.animation(80, test.firstPhase))
			second := []rune(test.animation(80, test.secondPhase))
			firstActive := 0
			secondActive := 0
			for index := range first {
				if first[index] != ' ' {
					firstActive++
					if second[index] != first[index] {
						t.Fatalf("already-drawn glyph moved at column %d: %q became %q", index, first[index], second[index])
					}
				}
				if second[index] != ' ' {
					secondActive++
				}
			}
			if secondActive <= firstActive {
				t.Fatalf("expected trace to grow, active columns %d then %d", firstActive, secondActive)
			}
		})
	}
}

func TestSquareUsesMatchedContinuousLineGlyphs(t *testing.T) {
	originalSeed := animationSeed
	animationSeed = 0x5eed
	t.Cleanup(func() {
		animationSeed = originalSeed
	})

	frame := []rune(squareArt(80, 38))
	if strings.ContainsAny(string(frame), "_─━▔└┗┐┓") {
		t.Fatalf("square regressed to line and corner glyphs with mismatched baselines: %q", string(frame))
	}
	if !strings.ContainsRune(string(frame), '⎤') || !strings.ContainsRune(string(frame), '⎡') {
		t.Fatalf("expected both falling and rising edges in %q", string(frame))
	}
	for index, glyph := range frame {
		if index == 0 || index == len(frame)-1 {
			continue
		}
		left := frame[index-1]
		right := frame[index+1]
		switch glyph {
		case '⎤':
			if left != '⎺' || right != '⎽' {
				t.Fatalf("falling edge at column %d does not join high to low: %q", index, string(frame))
			}
		case '⎡':
			if left != '⎽' || right != '⎺' {
				t.Fatalf("rising edge at column %d does not join low to high: %q", index, string(frame))
			}
		}
	}
}

func TestSquareVariesFrequencyAndPlateauLengthsBetweenBuilds(t *testing.T) {
	originalSeed := animationSeed
	animationSeed = 0x5eed
	t.Cleanup(func() {
		animationSeed = originalSeed
	})

	frequencies := map[int]bool{}
	longestHighRuns := map[int]bool{}
	for cycle := range 6 {
		frame := squareBaseSegments(160, int64(cycle))
		frequency := 0
		longestHigh := 0
		currentHigh := 0
		plateauLengths := map[int]bool{}
		currentPlateau := 0
		for _, segment := range frame {
			if segment == squareRising {
				frequency++
			}
			if segment == squareHigh {
				currentHigh++
				longestHigh = max(longestHigh, currentHigh)
			} else {
				currentHigh = 0
			}
			if segment == squareHigh || segment == squareLow {
				currentPlateau++
			} else if currentPlateau > 0 {
				plateauLengths[currentPlateau] = true
				currentPlateau = 0
			}
		}
		if currentPlateau > 0 {
			plateauLengths[currentPlateau] = true
		}
		if len(plateauLengths) < 4 {
			t.Fatalf("cycle %d reused too few plateau lengths within one build: %v", cycle, plateauLengths)
		}
		frequencies[frequency] = true
		longestHighRuns[longestHigh] = true
	}
	if len(frequencies) < 3 {
		t.Fatalf("expected square frequency to vary between builds, got %v", frequencies)
	}
	if len(longestHighRuns) < 3 {
		t.Fatalf("expected square plateau lengths to vary between builds, got %v", longestHighRuns)
	}
}

func TestSquareRunnerOccasionallyMovesRightAndOverwritesTrace(t *testing.T) {
	originalSeed := animationSeed
	animationSeed = 0x5eed
	t.Cleanup(func() {
		animationSeed = originalSeed
	})

	const width = 100
	activeCycles := 0
	inactiveCycles := 0
	verifiedMovement := false
	for cycle := range int64(32) {
		earlyPosition := squareRunnerStartFrame + 2
		earlyRunner, active := squareRunnerState(width, cycle, earlyPosition)
		if !active {
			inactiveCycles++
			continue
		}
		activeCycles++
		if verifiedMovement {
			continue
		}

		latePosition := squareCycleFrames - 3
		lateRunner, lateActive := squareRunnerState(width, cycle, latePosition)
		if !lateActive || lateRunner.left <= earlyRunner.left {
			t.Fatalf("cycle %d runner did not move right: early=%+v late=%+v", cycle, earlyRunner, lateRunner)
		}

		base := squareBaseSegments(width, cycle)
		phase := int(cycle)*squareCycleFrames + earlyPosition
		overwritten := squareSegments(width, phase)
		changed := 0
		packetEnd := earlyRunner.left + earlyRunner.barLength + 4
		for index := range base {
			if overwritten[index] != base[index] {
				changed++
				if index < earlyRunner.left || index > packetEnd {
					t.Fatalf("runner changed column %d outside [%d,%d]", index, earlyRunner.left, packetEnd)
				}
			}
		}
		if changed == 0 {
			t.Fatalf("cycle %d runner did not overwrite the existing trace", cycle)
		}
		verifiedMovement = true
	}

	if activeCycles == 0 || inactiveCycles == 0 {
		t.Fatalf("runner should be occasional, got active=%d inactive=%d", activeCycles, inactiveCycles)
	}
	if !verifiedMovement {
		t.Fatal("did not find a moving runner to verify")
	}
}

func TestDominoHasDistinctChainReactionSilhouette(t *testing.T) {
	originalSeed := animationSeed
	animationSeed = 0x5eed
	t.Cleanup(func() {
		animationSeed = originalSeed
	})

	for _, phase := range []int{0, 19, 47, 103, 211} {
		ribbon := ribbonArt(80, phase)
		domino := dominoArt(80, phase)
		if ribbon == domino {
			t.Fatalf("phase %d rendered identical frames %q", phase, ribbon)
		}
		if !strings.ContainsAny(ribbon, "·░▒▓█✦") {
			t.Fatalf("phase %d ribbon lost woven-light vocabulary: %q", phase, ribbon)
		}
		if strings.ContainsAny(ribbon, "╱╲━═─╍┄") {
			t.Fatalf("phase %d ribbon drifted back into glitch line vocabulary: %q", phase, ribbon)
		}
		if !strings.ContainsAny(domino, "▮━╱╲") {
			t.Fatalf("phase %d domino lost chain-reaction vocabulary: %q", phase, domino)
		}
	}
}

func TestLegacyBundledRibbonFramesYieldToBuiltInRenderer(t *testing.T) {
	legacyRibbon := FrameAnimation{
		Fill: true,
		Frames: []string{
			"··░░▒▒▓▓▒▒░░··  ",
			"·░░▒▒▓▓▒▒░░··  ·",
			"░░▒▒▓▓▒▒░░··  ··",
			"░▒▒▓▓▒▒░░··  ··░",
			"▒▒▓▓▒▒░░··  ··░░",
			"▒▓▓▒▒░░··  ··░░▒",
			"▓▓▒▒░░··  ··░░▒▒",
			"▓▒▒░░··  ··░░▒▒▓",
		},
	}
	if !isLegacyBundledFrameAnimation("ribbon", legacyRibbon) {
		t.Fatal("expected original ribbon frames to be recognized")
	}

	customized := legacyRibbon
	customized.Frames = slices.Clone(legacyRibbon.Frames)
	customized.Frames[0] = "custom"
	if isLegacyBundledFrameAnimation("ribbon", customized) {
		t.Fatal("expected customized ribbon frames to remain user-controlled")
	}
}

func TestRetiredBundledApertureFramesDoNotReturnThroughOldConfigs(t *testing.T) {
	legacy := FrameAnimation{
		Fill: true,
		Frames: []string{
			"░░▒▒▓▓██▓▓▒▒░░··",
			"·░░▒▒▓▓██▓▓▒▒░░·",
			"··░░▒▒▓▓██▓▓▒▒░░",
			"░··░░▒▒▓▓██▓▓▒▒░",
		},
	}
	if !isLegacyBundledFrameAnimation("shutter", legacy) {
		t.Fatal("old bundled aperture frames would be re-registered during config loading")
	}

	customized := legacy
	customized.Frames = slices.Clone(legacy.Frames)
	customized.Frames[0] = "custom"
	if isLegacyBundledFrameAnimation("shutter", customized) {
		t.Fatal("custom user animation should remain user-controlled")
	}
}

func TestBuiltInPresetsMaintainDistinctVisualLanguages(t *testing.T) {
	originalSeed := animationSeed
	animationSeed = 0x5eed
	t.Cleanup(func() {
		animationSeed = originalSeed
	})

	const (
		universalMinimumDistance = 0.10
		strictMinimumDistance    = 0.55
	)

	allowedSimilarity := map[string]bool{
		"aurora|aurora_sound":               true,
		"bloom|bloom_sound":                 true,
		"braid|braid_sound":                 true,
		"comet|comet_sound":                 true,
		"constellation|constellation_sound": true,
		"circuit|circuit_sound":             true,
		"domino|domino_sound":               true,
		"loom|loom_sound":                   true,
		"glitch|glitch_sound":               true,
		"radar|radar_sound":                 true,
		"ripples|ripples_sound":             true,
		"ribbon|ribbon_sound":               true,
		"smileys|smileys_sound":             true,
		"square|square_sound":               true,
		"spectrum|spectrum_sound":           true,
		"spline|spline_sound":               true,
		"wave|wave_sound":                   true,
	}
	strictPairs := map[string]bool{
		"braid|square":  true,
		"domino|glitch": true,
		"domino|ribbon": true,
		"domino|square": true,
		"glitch|ribbon": true,
		"glitch|square": true,
		"ribbon|square": true,
		"spline|square": true,
	}
	names := make([]string, 0, len(animationPresets))
	for name := range animationPresets {
		names = append(names, name)
	}
	sort.Strings(names)
	for firstIndex, first := range names {
		for _, second := range names[firstIndex+1:] {
			pairKey := first + "|" + second
			if allowedSimilarity[pairKey] {
				continue
			}
			t.Run(first+"_vs_"+second, func(t *testing.T) {
				distance := animationVisualDistance(animationPresets[first], animationPresets[second])
				minimumDistance := universalMinimumDistance
				if strictPairs[pairKey] {
					minimumDistance = strictMinimumDistance
				}
				if distance < minimumDistance {
					t.Fatalf("%s and %s are visually too similar: distance %.3f, minimum %.3f",
						first, second, distance, minimumDistance)
				}
			})
		}
	}
}

func TestActiveSoundPresetsMaintainDistinctVisualLanguages(t *testing.T) {
	originalSeed := animationSeed
	t.Cleanup(func() {
		animationSeed = originalSeed
	})

	steady := steadySoundTestSnapshot()
	eventful := steady
	eventful.OnsetCount = 3
	eventful.Onsets[0] = audioOnset{
		ID: 1, Sequence: 1, Age: 180 * time.Millisecond, Strength: 0.9,
		Region: audioRegionGeneral, Position: 0.35,
	}
	eventful.Onsets[1] = audioOnset{
		ID: 2, Sequence: 1, Age: 220 * time.Millisecond, Strength: 0.86,
		Region: audioRegionBass, Position: -0.4,
	}
	eventful.Onsets[2] = audioOnset{
		ID: 3, Sequence: 1, Age: 140 * time.Millisecond, Strength: 0.82,
		Region: audioRegionHigh, Position: 0.2,
	}

	renderers := soundSnapshotRenderers()
	names := make([]string, 0, len(animationPresets))
	for name := range animationPresets {
		names = append(names, name)
	}
	sort.Strings(names)

	const minimumDistance = 0.10
	phases := []int{0, 11, 29, 53, 89, 137, 211, 307}
	snapshots := []audioSnapshot{steady, eventful}
	seeds := []uint64{0x5eed, 0x1ab53}
	widths := []int{48, 80, 120}
	sampleCount := float64(len(snapshots) * len(seeds) * len(widths))
	signatures := make(map[string][]float64, len(names))
	for _, name := range names {
		signature := make([]float64, 19)
		for _, audio := range snapshots {
			for _, seed := range seeds {
				animationSeed = seed
				for _, width := range widths {
					animation := activeSimilarityAnimation(name, audio, renderers)
					sample := animationVisualSignatureForSamples(
						animation,
						width,
						phases,
					)
					for index, value := range sample {
						signature[index] += value / sampleCount
					}
				}
			}
		}
		signatures[name] = signature
	}

	for firstIndex, first := range names {
		for _, second := range names[firstIndex+1:] {
			if !isSoundPreset(first) && !isSoundPreset(second) {
				continue
			}
			if strings.TrimSuffix(first, "_sound") ==
				strings.TrimSuffix(second, "_sound") {
				continue
			}

			distance := visualSignatureDistance(
				signatures[first],
				signatures[second],
			)
			if distance < minimumDistance {
				t.Errorf(
					"%s and %s are too similar with active audio: aggregate distance %.3f, minimum %.3f",
					first,
					second,
					distance,
					minimumDistance,
				)
			}
		}
	}
}

func visualSignatureDistance(first []float64, second []float64) float64 {
	sum := 0.0
	for index := range min(len(first), len(second)) {
		difference := first[index] - second[index]
		sum += difference * difference
	}
	return math.Sqrt(sum)
}

func activeSimilarityAnimation(
	name string,
	audio audioSnapshot,
	renderers map[string]func(int, int, audioSnapshot) string,
) animationFunc {
	if renderer, ok := renderers[name]; ok {
		return func(width int, phase int) string {
			return renderer(width, phase, audio)
		}
	}
	return animationPresets[name]
}

func animationVisualDistance(first animationFunc, second animationFunc) float64 {
	return animationVisualDistanceForSamples(
		first,
		second,
		80,
		[]int{0, 11, 29, 53, 89, 137, 211, 307},
	)
}

func animationVisualDistanceForSamples(
	first animationFunc,
	second animationFunc,
	width int,
	phases []int,
) float64 {
	firstSignature := animationVisualSignatureForSamples(first, width, phases)
	secondSignature := animationVisualSignatureForSamples(second, width, phases)
	sum := 0.0
	for index := range firstSignature {
		difference := firstSignature[index] - secondSignature[index]
		sum += difference * difference
	}
	return math.Sqrt(sum)
}

func animationVisualSignatureForSamples(
	animation animationFunc,
	width int,
	phases []int,
) []float64 {
	const classCount = 16
	signature := make([]float64, classCount+3)
	for _, phase := range phases {
		frame := fitRunes(animation(width, phase), width)
		classes := make([]int, width)
		active := 0
		for index, glyph := range frame {
			class := animationGlyphClass(glyph)
			classes[index] = class
			signature[class] += 1 / float64(width*len(phases))
			if class != 0 {
				active++
			}
		}

		symmetry := 0
		transitions := 0
		for index := range width {
			if classes[index] == classes[width-1-index] {
				symmetry++
			}
			if index > 0 && classes[index] != classes[index-1] {
				transitions++
			}
		}
		signature[classCount] += float64(active) / float64(width*len(phases))
		signature[classCount+1] += float64(symmetry) / float64(width*len(phases))
		signature[classCount+2] += float64(transitions) / float64((width-1)*len(phases))
	}
	return signature
}

func animationGlyphClass(glyph rune) int {
	switch glyph {
	case ' ':
		return 0
	case '·', '∙', '•', '｡', '･', '⋅':
		return 1
	case '░':
		return 2
	case '▒':
		return 3
	case '▓':
		return 4
	case '█', '▇', '▆', '▅', '▄', '▃', '▂', '▁':
		return 5
	case '─', '━', '═', '┄', '╍', '╴', '╶', '⌁', '≋', '≈', '⎺', '⎻', '⎼', '⎽':
		return 6
	case '│', '┃', '╋', '╪', '╡', '╞', '╾', '╼', '▶', '◀', '⎡', '⎤':
		return 7
	case '╱', '╲', '╳', '⟋', '/', '\\':
		return 8
	case '✦', '✧', '✶', '✷', '◆', '◇', '●', '☄', '❧':
		return 9
	case '◎', '◉':
		return 15
	}
	if glyph >= 0x2800 && glyph <= 0x28ff {
		mask := uint8(glyph - 0x2800)
		dots := bits.OnesCount8(mask)
		if dots >= 5 {
			return 14
		}
		weightedRows := bits.OnesCount8(mask&0x09)*0 +
			bits.OnesCount8(mask&0x12)*1 +
			bits.OnesCount8(mask&0x24)*2 +
			bits.OnesCount8(mask&0xc0)*3
		averageRow := float64(weightedRows) / float64(max(1, dots))
		switch {
		case averageRow < 0.8:
			return 10
		case averageRow < 1.7:
			return 11
		case averageRow < 2.5:
			return 12
		default:
			return 13
		}
	}
	return 15
}

func frameContainsBraille(frame string) bool {
	for _, glyph := range frame {
		if glyph >= 0x2800 && glyph <= 0x28ff {
			return true
		}
	}
	return false
}

func TestPreviewPresetNamesPairSoundCompanionsWithinRotationThenSortedRemainder(t *testing.T) {
	originalPresets := animationPresets
	originalRotation := rotationPresets
	originalSoundNames := soundPresetNames
	animationPresets = map[string]animationFunc{
		"aurora":       func(int, int) string { return "a" },
		"aurora_sound": func(int, int) string { return "A" },
		"bloom":        func(int, int) string { return "b" },
		"bloom_sound":  func(int, int) string { return "B" },
		"alpha":        func(int, int) string { return "c" },
		"zebra":        func(int, int) string { return "d" },
	}
	soundPresetNames = map[string]bool{
		"aurora_sound": true,
		"bloom_sound":  true,
	}
	rotationPresets = []string{"bloom_sound", "aurora", "bloom", "missing"}
	t.Cleanup(func() {
		animationPresets = originalPresets
		rotationPresets = originalRotation
		soundPresetNames = originalSoundNames
	})

	got := previewPresetNames()
	want := []string{
		"bloom", "bloom_sound",
		"aurora", "aurora_sound",
		"alpha", "zebra",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestPreviewPresetNamesPlaceEveryBuiltInSoundPresetAfterItsBase(t *testing.T) {
	names := previewPresetNames()
	for index, name := range names {
		if !isSoundPreset(name) {
			continue
		}
		base := strings.TrimSuffix(name, "_sound")
		if index == 0 || names[index-1] != base {
			t.Fatalf("expected %q immediately after %q, got %v", name, base, names)
		}
	}
}

func TestCalculatePreviewLayoutSupportsScrollableHeightsAndErrors(t *testing.T) {
	originalMax := settings.MaxArtColumns
	settings.MaxArtColumns = 220
	t.Cleanup(func() {
		settings.MaxArtColumns = originalMax
	})
	names := []string{"aurora", "constellation", "wave"}

	tall, err := calculatePreviewLayout(names, 80, 5)
	if err != nil {
		t.Fatalf("tall layout: %v", err)
	}
	if tall.artColumns != 65 {
		t.Fatalf("unexpected art width: %+v", tall)
	}

	short, err := calculatePreviewLayout(names, 80, 2)
	if err != nil {
		t.Fatalf("scrollable short layout: %v", err)
	}
	if short.height != 2 {
		t.Fatalf("unexpected short layout: %+v", short)
	}

	if _, err := calculatePreviewLayout(names, 80, 1); err == nil || !strings.Contains(err.Error(), "at least 2 terminal rows") {
		t.Fatalf("expected minimum-height error, got %v", err)
	}
	if _, err := calculatePreviewLayout(names, 20, 5); err == nil || !strings.Contains(err.Error(), "terminal columns") {
		t.Fatalf("expected minimum-width error, got %v", err)
	}
}

func TestPreviewLinesShareMotionPhaseAndUseSpacers(t *testing.T) {
	originalPresets := animationPresets
	originalMotion := settings.Motion
	animationPresets = map[string]animationFunc{}
	settings.Motion = 0.5
	seen := map[string]int{}
	for _, name := range []string{"first", "second"} {
		presetName := name
		animationPresets[name] = func(width int, phase int) string {
			seen[presetName] = phase
			return strings.Repeat(presetName[:1], width)
		}
	}
	t.Cleanup(func() {
		animationPresets = originalPresets
		settings.Motion = originalMotion
	})

	layout := previewLayout{width: 40, height: 3, labelColumns: 6, artColumns: 8}
	lines := previewLines([]string{"first", "second"}, layout, 10)
	if len(lines) != 3 || lines[1] != "" {
		t.Fatalf("expected one spacer line, got %#v", lines)
	}
	if seen["first"] != 5 || seen["second"] != 5 {
		t.Fatalf("expected shared motion phase 5, got %#v", seen)
	}
}

func TestTerminalColumnsHandleBrailleWideAndCombiningRunes(t *testing.T) {
	if got := terminalColumns("⠿"); got != 1 {
		t.Fatalf("expected braille width 1, got %d", got)
	}
	if got := terminalColumns("🌐"); got != 2 {
		t.Fatalf("expected emoji width 2, got %d", got)
	}
	if got := terminalColumns("e\u0301"); got != 1 {
		t.Fatalf("expected combining sequence width 1, got %d", got)
	}
	if got := truncateTerminalColumns("ab🌐cd", 4); got != "ab🌐" {
		t.Fatalf("unexpected display-width truncation %q", got)
	}
}

func TestPreviewModelRenderingAndNonTTYFailure(t *testing.T) {
	originalPresets := animationPresets
	originalRotation := rotationPresets
	animationPresets = map[string]animationFunc{
		"demo": func(width int, phase int) string { return strings.Repeat("x", width) },
	}
	rotationPresets = []string{"demo"}
	t.Cleanup(func() {
		animationPresets = originalPresets
		rotationPresets = originalRotation
	})

	model := newPreviewModel([]string{"demo"}, 25)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 30, Height: 3})
	rendered := updated.(previewModel).View().Content
	if !strings.Contains(rendered, "demo  xxxxxxxx") || !strings.Contains(rendered, "PgUp/PgDn") {
		t.Fatalf("unexpected Bubble Tea preview %q", rendered)
	}

	file, err := os.CreateTemp(t.TempDir(), "preview-output")
	if err != nil {
		t.Fatalf("create output file: %v", err)
	}
	defer file.Close()
	if err := runPreview(file, 25); err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("expected non-TTY rejection, got %v", err)
	}
}

func TestPreviewModelScrollsAndQuits(t *testing.T) {
	originalPresets := animationPresets
	animationPresets = map[string]animationFunc{}
	names := []string{"first", "second", "third", "fourth"}
	for _, name := range names {
		animationPresets[name] = func(width int, phase int) string {
			return strings.Repeat("x", width)
		}
	}
	t.Cleanup(func() {
		animationPresets = originalPresets
	})

	model := newPreviewModel(names, 25)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 40, Height: 4})
	model = updated.(previewModel)
	if model.viewport.YOffset() != 0 {
		t.Fatalf("expected preview at top, offset=%d", model.viewport.YOffset())
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	model = updated.(previewModel)
	if model.viewport.YOffset() == 0 || !model.viewport.AtBottom() {
		t.Fatalf("expected End to scroll to bottom, offset=%d", model.viewport.YOffset())
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	model = updated.(previewModel)
	if model.viewport.YOffset() != 0 || !model.viewport.AtTop() {
		t.Fatalf("expected Home to scroll to top, offset=%d", model.viewport.YOffset())
	}
	_, command := model.Update(tea.KeyPressMsg{Text: "q", Code: 'q'})
	if command == nil {
		t.Fatal("expected q to quit")
	}
}

func TestAudioRevisionControlsPreviewRedraw(t *testing.T) {
	first := audioSnapshot{Revision: 1}
	second := audioSnapshot{Revision: 1}
	if !sameAudioVisual(first, second) {
		t.Fatal("matching audio revisions should not trigger a visual redraw")
	}
	second.Revision = 2
	if sameAudioVisual(first, second) {
		t.Fatal("a new visible audio revision must trigger a redraw")
	}
	second.Revision = first.Revision
	second.Active = true
	if sameAudioVisual(first, second) {
		t.Fatal("stale/active state changes must trigger a redraw")
	}
	second.Active = first.Active
	second.CaptureAvailable = true
	if sameAudioVisual(first, second) {
		t.Fatal("capture availability changes must trigger a redraw")
	}
}
