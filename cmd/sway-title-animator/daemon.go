package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/marang/sway-title-animator/internal/swayipc"
)

func subscribe(socket string, events chan<- swayipc.Event, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
		}

		conn, err := swayipc.Dial(socket)
		if err != nil {
			if swayEndpointGone(socket) {
				emitSwayShutdown(events, done)
				return
			}
			if waitForDone(done, time.Second) {
				return
			}
			continue
		}
		response, err := conn.Request(swayipc.Subscribe, []byte(`["window","workspace","shutdown"]`))
		if err == nil {
			err = swayipc.CheckSubscribeResponse(response)
		}
		if err != nil {
			_ = conn.Close()
			if waitForDone(done, time.Second) {
				return
			}
			continue
		}

		for {
			message, err := conn.Read()
			if err != nil {
				_ = conn.Close()
				if swayEndpointGone(socket) {
					emitSwayShutdown(events, done)
					return
				}
				break
			}
			event, err := swayipc.DecodeEvent(message)
			if err != nil {
				_ = conn.Close()
				break
			}
			if event.Type == swayipc.EventShutdown {
				select {
				case events <- event:
				case <-done:
				}
				_ = conn.Close()
				return
			}
			select {
			case events <- event:
			default:
			}
		}
		if waitForDone(done, time.Second) {
			return
		}
	}
}

func swayEndpointGone(socket string) bool {
	_, err := os.Lstat(socket)
	return errors.Is(err, os.ErrNotExist)
}

func emitSwayShutdown(events chan<- swayipc.Event, done <-chan struct{}) {
	select {
	case events <- swayipc.Event{Type: swayipc.EventShutdown, Change: "endpoint-gone"}:
	case <-done:
	}
}

func waitForDone(done <-chan struct{}, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func runLoopWithFPS(socket string, fps float64) int {
	stopAudio := func() {}
	if presetUsesAudio(animationPreset) {
		stopAudio = startDefaultAudioMonitor()
	}
	defer stopAudio()

	control := swayipc.NewClient(socket)
	defer control.Close()

	animator := NewTitleAnimator(control)
	events := make(chan swayipc.Event, 16)
	done := make(chan struct{})
	defer close(done)
	go subscribe(socket, events, done)

	reporter := &sessionErrorReporter{}
	codexBroker, err := startCodexReportBroker(reporter.Report)
	if err != nil {
		reporter.Report(fmt.Errorf("start secure Codex session reporter: %w", err))
	}
	if codexBroker != nil {
		defer func() {
			if err := codexBroker.Close(); err != nil {
				reporter.Report(fmt.Errorf("stop secure Codex session reporter: %w", err))
			}
		}()
	}
	sessionRuntime, err := newSessionRuntime(control)
	if err != nil {
		reporter.Report(err)
		sessionRuntime = nil
	}
	snapshotTimer := time.NewTimer(time.Hour)
	if !snapshotTimer.Stop() {
		<-snapshotTimer.C
	}
	defer snapshotTimer.Stop()
	var snapshotTimerChannel <-chan time.Time
	resetSnapshotTimer := func() {
		if !snapshotTimer.Stop() {
			select {
			case <-snapshotTimer.C:
			default:
			}
		}
		if sessionRuntime == nil {
			snapshotTimerChannel = nil
			return
		}
		deadline, scheduled := sessionRuntime.Deadline()
		if !scheduled {
			snapshotTimerChannel = nil
			return
		}
		duration := time.Until(deadline)
		if duration < 0 {
			duration = 0
		}
		snapshotTimer.Reset(duration)
		snapshotTimerChannel = snapshotTimer.C
	}

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	phase := 0
	reconcilePersistentSession(animator, sessionRuntime, phase, reporter.Report)
	resetSnapshotTimer()
	initialDuration := frameDuration(fps)
	timer := time.NewTimer(initialDuration)
	defer timer.Stop()
	nextFrames := 1
	nextWakeAt := time.Now().Add(initialDuration)

	for {
		select {
		case event := <-events:
			if event.Type == swayipc.EventShutdown {
				if sessionRuntime != nil {
					sessionRuntime.Shutdown()
				}
				if err := animator.ResetAll(); err != nil {
					fmt.Fprintf(os.Stderr, "Unable to reset all title formats: %v\n", err)
				}
				return 0
			}
			if event.AffectsSessionLayout() {
				reconcilePersistentSession(animator, sessionRuntime, phase, reporter.Report)
				resetSnapshotTimer()
			} else {
				_, _ = animator.RefreshTree(phase)
			}
			candidateFrames := animator.FramesUntilNextWake(phase)
			candidateDuration := time.Duration(candidateFrames) * frameDuration(fps)
			if time.Now().Add(candidateDuration).Before(nextWakeAt) {
				nextFrames = candidateFrames
				nextWakeAt = resetTimer(timer, candidateDuration)
			}
		case <-signals:
			if sessionRuntime != nil {
				sessionRuntime.Shutdown()
			}
			if err := animator.ResetAll(); err != nil {
				fmt.Fprintf(os.Stderr, "Unable to reset all title formats: %v\n", err)
			}
			return 0
		case <-timer.C:
			phase += nextFrames
			animator.Tick(phase)
			nextFrames = animator.FramesUntilNextWake(phase)
			nextWakeAt = resetTimer(timer, time.Duration(nextFrames)*frameDuration(fps))
		case now := <-snapshotTimerChannel:
			if sessionRuntime.ObservationDue(now) {
				// Advance the deadline before I/O so a disconnected Sway socket
				// cannot turn a due observation into a tight retry loop.
				sessionRuntime.PostponeObservation(now)
				reconcilePersistentSession(animator, sessionRuntime, phase, reporter.Report)
			}
			if sessionRuntime.StartupDue(now) {
				reconcilePersistentSession(animator, sessionRuntime, phase, reporter.Report)
				if sessionRuntime.StartupDue(time.Now()) {
					sessionRuntime.PostponeStartup(time.Now())
				}
			}
			if err := sessionRuntime.Flush(now); err != nil {
				reporter.Report(err)
			}
			resetSnapshotTimer()
		}
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
