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
