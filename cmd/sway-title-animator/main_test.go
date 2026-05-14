package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestChildProcessLabelFindsDescendant(t *testing.T) {
	cmd := exec.Command("sleep", "2")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	for range 20 {
		if label := childProcessLabel(os.Getpid()); label == "sleep" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("expected to find sleep child process")
}

func TestAnimationFrameKeyCoalescesStillMotionFrames(t *testing.T) {
	originalPreset := animationPreset
	t.Cleanup(func() {
		animationPreset = originalPreset
	})

	animationPreset = "aurora"

	if first, second := animationFrameKey(1), animationFrameKey(2); first != second {
		t.Fatalf("expected adjacent low-motion frames to share key, got %d and %d", first, second)
	}
	if first, later := animationFrameKey(1), animationFrameKey(6); first == later {
		t.Fatalf("expected later frame to advance key, got %d and %d", first, later)
	}
}

func TestVisibleStatusTextDoesNotExposeWaylandShell(t *testing.T) {
	node := &Node{Shell: "xdg_shell"}

	if status := visibleStatusText(node); status != "" {
		t.Fatalf("expected shell protocol to stay hidden, got %q", status)
	}
}

func TestSelectChildProcessLabelPrefersForegroundProcessGroupLeader(t *testing.T) {
	children := map[int][]int{
		1: {2},
		2: {3},
		3: {4},
		4: {5},
	}
	names := map[int]string{
		1: "alacritty",
		2: "launcher",
		3: "editor",
		4: "language-server",
		5: "compiler",
	}
	stats := map[int]processStat{
		2: {pgrp: 2, ttyNr: 34818, tpgid: 3},
		3: {pgrp: 3, ttyNr: 34818, tpgid: 3},
		4: {pgrp: 3, ttyNr: 34818, tpgid: 3},
		5: {pgrp: 3, ttyNr: 34818, tpgid: 3},
	}

	label := selectChildProcessLabel(
		1,
		func(pid int) []int { return children[pid] },
		func(pid int) string { return names[pid] },
		func(pid int) (processStat, bool) {
			stat, ok := stats[pid]
			return stat, ok
		},
	)

	if label != "editor" {
		t.Fatalf("expected foreground process group leader, got %q", label)
	}
}

func TestSelectChildProcessLabelFallsBackToFirstDescendantWithoutTTY(t *testing.T) {
	children := map[int][]int{
		1: {2},
		2: {3},
		3: {4},
	}
	names := map[int]string{
		1: "foot",
		2: "launcher",
		3: "tool",
		4: "helper",
	}

	label := selectChildProcessLabel(
		1,
		func(pid int) []int { return children[pid] },
		func(pid int) string { return names[pid] },
		func(pid int) (processStat, bool) { return processStat{}, false },
	)

	if label != "launcher" {
		t.Fatalf("expected first descendant fallback, got %q", label)
	}
}

func TestAnimationFrameKeyKeepsShowcaseBlendFramesDistinct(t *testing.T) {
	originalPreset := animationPreset
	originalHold := settings.ShowcaseHoldFrames
	originalBlend := settings.ShowcaseBlendFrames
	t.Cleanup(func() {
		animationPreset = originalPreset
		settings.ShowcaseHoldFrames = originalHold
		settings.ShowcaseBlendFrames = originalBlend
	})

	animationPreset = "showcase"
	settings.ShowcaseHoldFrames = 2
	settings.ShowcaseBlendFrames = 3

	if first, second := animationFrameKey(2), animationFrameKey(3); first == second {
		t.Fatalf("expected blend frames to stay distinct, got %d and %d", first, second)
	}
}
