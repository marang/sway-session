package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/marang/sway-title-animator/internal/swayipc"
)

func runLoopWithFPS(socket string, fps float64) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runLoopWithContext(ctx, socket, fps)
}

func runLoopWithContext(ctx context.Context, socket string, fps float64) int {
	stopAudio := func() {}
	if presetUsesAudio(animationPreset) {
		stopAudio = startDefaultAudioMonitor()
	}
	defer stopAudio()

	control := swayipc.NewClient(socket)
	defer control.Close()

	animator := NewTitleAnimatorWithContext(ctx, control)
	events := make(chan swayipc.Event, 16)
	done := make(chan struct{})
	defer close(done)
	go swayipc.StreamEvents(socket, events, done)

	phase := 0
	_, _ = animator.RefreshTree(phase)
	initialDuration := frameDuration(fps)
	timer := time.NewTimer(initialDuration)
	defer timer.Stop()
	nextFrames := 1
	nextWakeAt := time.Now().Add(initialDuration)

	for {
		select {
		case event := <-events:
			if event.Type == swayipc.EventShutdown {
				resetAnimatorForShutdown(animator)
				return 0
			}
			_, _ = animator.RefreshTree(phase)
			candidateFrames := animator.FramesUntilNextWake(phase)
			candidateDuration := time.Duration(candidateFrames) * frameDuration(fps)
			if time.Now().Add(candidateDuration).Before(nextWakeAt) {
				nextFrames = candidateFrames
				nextWakeAt = resetTimer(timer, candidateDuration)
			}
		case <-ctx.Done():
			resetAnimatorForShutdown(animator)
			return 0
		case <-timer.C:
			phase += nextFrames
			animator.Tick(phase)
			nextFrames = animator.FramesUntilNextWake(phase)
			nextWakeAt = resetTimer(timer, time.Duration(nextFrames)*frameDuration(fps))
		}
	}
}

func resetAnimatorForShutdown(animator *TitleAnimator) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := animator.ResetAllContext(cleanupCtx); err != nil {
		fmt.Fprintf(os.Stderr, "Unable to reset all title formats: %v\n", err)
	}
}

func frameDuration(fps float64) time.Duration {
	return time.Duration(float64(time.Second) / fps)
}

func resetTimer(timer *time.Timer, duration time.Duration) time.Time {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
	return time.Now().Add(duration)
}

func listPresets() {
	names := make([]string, 0, len(animationPresets))
	for name := range animationPresets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Println(name)
	}
}
