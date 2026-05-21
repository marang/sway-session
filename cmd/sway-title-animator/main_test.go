package main

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestIPCDialUnixSocketRoundTrip(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "sway.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("unix sockets are not permitted in this sandbox: %v", err)
		}
		t.Fatalf("listen unix socket: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})

	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()

		body, messageType, err := readIPCMessage(conn)
		if err != nil {
			serverDone <- err
			return
		}
		if messageType != 99 || string(body) != "hello" {
			serverDone <- &testError{message: "unexpected request"}
			return
		}
		if err := writeFull(conn, ipcHeader(100, len([]byte("ok")))); err != nil {
			serverDone <- err
			return
		}
		if err := writeFull(conn, []byte("ok")); err != nil {
			serverDone <- err
			return
		}
		serverDone <- nil
	}()

	ipc := &IPC{socket: socket}
	t.Cleanup(ipc.Close)
	body, messageType, err := ipc.Request(99, "hello")
	if err != nil {
		t.Fatalf("ipc request: %v", err)
	}
	if messageType != 100 || string(body) != "ok" {
		t.Fatalf("unexpected response type=%d body=%q", messageType, body)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server: %v", err)
	}
}

type testError struct {
	message string
}

func (err *testError) Error() string {
	return err.message
}

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

func TestValidateSettingsRejectsLayoutBreakingValues(t *testing.T) {
	valid := Settings{
		FPS:                 25,
		Motion:              0.22,
		ApproxCharWidth:     8.5,
		MaxArtColumns:       220,
		TitleReserveColumns: 18,
		ShowcaseHoldFrames:  260,
		ShowcaseBlendFrames: 75,
	}
	tests := []struct {
		name   string
		mutate func(*Settings)
	}{
		{"fps too low", func(settings *Settings) { settings.FPS = 0 }},
		{"fps too high", func(settings *Settings) { settings.FPS = 61 }},
		{"motion zero", func(settings *Settings) { settings.Motion = 0 }},
		{"char width zero", func(settings *Settings) { settings.ApproxCharWidth = 0 }},
		{"negative art columns", func(settings *Settings) { settings.MaxArtColumns = -1 }},
		{"negative title reserve", func(settings *Settings) { settings.TitleReserveColumns = -1 }},
		{"showcase hold zero", func(settings *Settings) { settings.ShowcaseHoldFrames = 0 }},
		{"showcase blend zero", func(settings *Settings) { settings.ShowcaseBlendFrames = 0 }},
	}

	if err := validateSettings(valid); err != nil {
		t.Fatalf("expected valid settings, got %v", err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := valid
			test.mutate(&settings)
			if err := validateSettings(settings); err == nil {
				t.Fatalf("expected invalid settings")
			}
		})
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

func TestCommandLineLabelPrefersScriptCommandOverRuntime(t *testing.T) {
	label := commandLineLabel([]string{"/usr/bin/node", "/opt/tools/codex.js"})

	if label != "codex" {
		t.Fatalf("expected script command label, got %q", label)
	}
}

func TestCommandLineLabelKeepsRuntimeForInlineCommand(t *testing.T) {
	label := commandLineLabel([]string{"/usr/bin/node", "-e", "console.log('inline')"})

	if label != "node" {
		t.Fatalf("expected runtime label, got %q", label)
	}
}

func TestProcessLabelPrefersKernelNameOverRuntimeCommandline(t *testing.T) {
	label := processLabel([]string{"/usr/bin/node", "/opt/tools/cli.js"}, "codex\n")

	if label != "codex" {
		t.Fatalf("expected kernel process name, got %q", label)
	}
}

func TestProcessLabelKeepsScriptCommandWhenKernelNameMatchesRuntime(t *testing.T) {
	label := processLabel([]string{"/usr/bin/node", "/opt/tools/codex.js"}, "node\n")

	if label != "codex" {
		t.Fatalf("expected script command label, got %q", label)
	}
}

func TestProcessLabelIgnoresTruncatedKernelName(t *testing.T) {
	label := processLabel([]string{"/usr/bin/verylongprocessname", "/opt/tools/codex.js"}, "verylongprocess\n")

	if label != "codex" {
		t.Fatalf("expected script command label, got %q", label)
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

func TestFramesUntilNextAnimationKeySkipsStillMotionFrames(t *testing.T) {
	originalPreset := animationPreset
	t.Cleanup(func() {
		animationPreset = originalPreset
	})

	animationPreset = "aurora"

	if frames := framesUntilNextAnimationKey(1); frames <= 1 {
		t.Fatalf("expected still motion frames to be skipped, got %d", frames)
	}
}

func TestFramesUntilNextAnimationKeyKeepsShowcaseBlendAtFullFPS(t *testing.T) {
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

	if frames := framesUntilNextAnimationKey(2); frames != 1 {
		t.Fatalf("expected blend frames to run at full fps, got %d", frames)
	}
}

func TestNewAnimationPresetsRenderMotion(t *testing.T) {
	for _, name := range []string{"smileys", "wave", "spline"} {
		t.Run(name, func(t *testing.T) {
			fn := animationPresets[name]
			first := fn(80, 1)
			later := fn(80, 12)
			if first == "" {
				t.Fatalf("expected nonempty frame")
			}
			if first == later {
				t.Fatalf("expected preset to move, got identical frames %q", first)
			}
		})
	}
}

func TestApplyFocusedFrameReassertsCachedFrame(t *testing.T) {
	var setID int64
	var setValue string
	setCount := 0
	animator := NewTitleAnimator(nil)
	animator.titleSetter = func(conID int64, value string) {
		setID = conID
		setValue = value
		setCount++
	}
	animator.focusedID = 42
	animator.focusedBase = "base"
	animator.focusedAnimationKey = animationFrameKey(1)
	animator.focusedCacheIsActive = true
	animator.lastFormats[42] = "base"
	animator.lastFormatSetAt[42] = time.Now().Add(-titleReassertInterval - time.Second)

	animator.ApplyFocusedFrame(1)

	if setCount != 1 || setID != 42 || setValue != "base" {
		t.Fatalf("expected cached frame to be reasserted once, got count=%d id=%d value=%q", setCount, setID, setValue)
	}
}
