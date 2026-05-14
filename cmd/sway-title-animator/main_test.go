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

func TestSelectChildProcessLabelPrefersNearestUserProcess(t *testing.T) {
	children := map[int][]int{
		1: {2},
		2: {3},
		3: {4},
		4: {5},
	}
	names := map[int]string{
		1: "alacritty",
		2: "zsh",
		3: "editor",
		4: "language-server",
		5: "compiler",
	}

	label := selectChildProcessLabel(
		1,
		func(pid int) []int { return children[pid] },
		func(pid int) string { return names[pid] },
	)

	if label != "editor" {
		t.Fatalf("expected nearest user process label, got %q", label)
	}
}

func TestSelectChildProcessLabelSkipsShellWrappers(t *testing.T) {
	children := map[int][]int{
		1: {2},
		2: {3},
		3: {4},
	}
	names := map[int]string{
		1: "foot",
		2: "bash",
		3: "sudo",
		4: "nvim",
	}

	label := selectChildProcessLabel(
		1,
		func(pid int) []int { return children[pid] },
		func(pid int) string { return names[pid] },
	)

	if label != "nvim" {
		t.Fatalf("expected shell wrappers to be skipped, got %q", label)
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
