package main

import "math"

const dominoCycleFrames = 192

type dominoMotion uint8

const (
	dominoStanding dominoMotion = iota
	dominoFalling
	dominoFallen
	dominoRising
)

type dominoCycle struct {
	index              int64
	position           int
	holdFrames         int
	fallFrames         int
	fallenHoldFrames   int
	leftToRight        bool
	recoverLeftToRight bool
}

func dominoArt(width int, phase int) string {
	width = artWidth(width)
	if width == 0 {
		return ""
	}
	if width < 8 {
		return string(fitRunes(shortFrame(width, phase, []string{
			"▮·▮",
			"━·╲",
			"━·━",
			"╱·━",
			"▮·▮",
		}), width))
	}
	return string(dominoFrame(width, phase))
}

func dominoFrame(width int, phase int) []rune {
	cycle := dominoCycleAt(phase)
	positions := dominoPositions(width)
	chars := make([]rune, width)
	for index := range chars {
		chars[index] = ' '
	}
	for index := 0; index+1 < len(positions); index++ {
		midpoint := (positions[index] + positions[index+1]) / 2
		if midpoint != positions[index] && midpoint != positions[index+1] {
			chars[midpoint] = '·'
		}
	}

	for stone, column := range positions {
		motion, direction := dominoStoneMotion(cycle, stone, len(positions))
		chars[column] = dominoGlyph(motion, direction)
		if motion == dominoFalling {
			contact := column + 1
			if !direction {
				contact = column - 1
			}
			if contact >= 0 && contact < width && chars[contact] == ' ' {
				chars[contact] = '•'
			}
		}
	}
	return chars
}

func dominoCycleAt(phase int) dominoCycle {
	index := phase / dominoCycleFrames
	position := phase % dominoCycleFrames
	if position < 0 {
		position += dominoCycleFrames
		index--
	}
	cycleIndex := int64(index)
	holdFrames := 14 + int(eventRandom("domino", 1, cycleIndex, 1)*17)
	fallFrames := 68 + int(eventRandom("domino", 1, cycleIndex, 2)*31)
	fallenHoldFrames := 12 + int(eventRandom("domino", 1, cycleIndex, 3)*19)
	leftToRight := eventRandom("domino", 1, cycleIndex, 4) < 0.5
	recoverLeftToRight := leftToRight
	if eventRandom("domino", 1, cycleIndex, 5) < 0.65 {
		recoverLeftToRight = !leftToRight
	}
	return dominoCycle{
		index:              cycleIndex,
		position:           position,
		holdFrames:         holdFrames,
		fallFrames:         fallFrames,
		fallenHoldFrames:   fallenHoldFrames,
		leftToRight:        leftToRight,
		recoverLeftToRight: recoverLeftToRight,
	}
}

func dominoPositions(width int) []int {
	if width <= 0 {
		return nil
	}
	spacing := 2 + int(eventRandom("domino-layout", 1, 0, 1)*2)
	offset := int(eventRandom("domino-layout", 1, 0, 2) * float64(spacing))
	positions := make([]int, 0, width/spacing+1)
	for column := offset; column < width; column += spacing {
		positions = append(positions, column)
	}
	if len(positions) == 0 {
		positions = append(positions, min(width-1, offset))
	}
	return positions
}

func dominoStoneMotion(cycle dominoCycle, stone int, stoneCount int) (dominoMotion, bool) {
	if stoneCount <= 0 || cycle.position < cycle.holdFrames {
		return dominoStanding, cycle.leftToRight
	}
	fallEnd := cycle.holdFrames + cycle.fallFrames
	if cycle.position < fallEnd {
		progress := float64(cycle.position-cycle.holdFrames) / float64(cycle.fallFrames)
		front := smoothstep(progress) * float64(stoneCount+1)
		rank := float64(dominoRank(stone, stoneCount, cycle.leftToRight))
		rank += dominoStoneStagger(cycle.index, stone, 1)
		switch distance := front - rank; {
		case distance > 1.25:
			return dominoFallen, cycle.leftToRight
		case distance > 0:
			return dominoFalling, cycle.leftToRight
		default:
			return dominoStanding, cycle.leftToRight
		}
	}

	recoveryStart := fallEnd + cycle.fallenHoldFrames
	if cycle.position < recoveryStart {
		return dominoFallen, cycle.recoverLeftToRight
	}
	recoveryFrames := max(1, dominoCycleFrames-recoveryStart)
	progress := float64(cycle.position-recoveryStart) / float64(recoveryFrames)
	front := smoothstep(progress) * float64(stoneCount+1)
	rank := float64(dominoRank(stone, stoneCount, cycle.recoverLeftToRight))
	rank += dominoStoneStagger(cycle.index, stone, 2)
	switch distance := front - rank; {
	case distance > 1.25:
		return dominoStanding, cycle.recoverLeftToRight
	case distance > 0:
		return dominoRising, cycle.recoverLeftToRight
	default:
		return dominoFallen, cycle.recoverLeftToRight
	}
}

func dominoStoneStagger(cycle int64, stone int, stream uint64) float64 {
	event := cycle*512 + int64(stone)
	return (eventRandom("domino-stagger", stream, event, 1)*2 - 1) * 0.28
}

func dominoRank(stone int, stoneCount int, leftToRight bool) int {
	if leftToRight {
		return stone
	}
	return stoneCount - 1 - stone
}

func dominoGlyph(motion dominoMotion, leftToRight bool) rune {
	switch motion {
	case dominoStanding:
		return '▮'
	case dominoFallen:
		return '━'
	case dominoFalling:
		if leftToRight {
			return '╲'
		}
		return '╱'
	case dominoRising:
		if leftToRight {
			return '╱'
		}
		return '╲'
	default:
		return ' '
	}
}

func nearestDominoPosition(positions []int, target float64) int {
	if len(positions) == 0 {
		return -1
	}
	nearest := 0
	distance := math.Abs(float64(positions[0]) - target)
	for index := 1; index < len(positions); index++ {
		candidate := math.Abs(float64(positions[index]) - target)
		if candidate < distance {
			nearest = index
			distance = candidate
		}
	}
	return nearest
}
