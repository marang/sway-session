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

func TestProcessLabelScorePrefersInteractiveParent(t *testing.T) {
	codexScore := processLabelScore("codex", 2)
	nodeScore := processLabelScore("node", 4)

	if codexScore <= nodeScore {
		t.Fatalf("expected codex score %d to beat node helper score %d", codexScore, nodeScore)
	}
}

func TestProcessLabelScorePrefersNearbySamePriorityProcess(t *testing.T) {
	near := processLabelScore("node", 2)
	deep := processLabelScore("npm", 4)

	if near <= deep {
		t.Fatalf("expected nearby node score %d to beat deeper npm score %d", near, deep)
	}
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
