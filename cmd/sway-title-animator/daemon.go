package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"
)

func subscribe(socket string, events chan<- struct{}, shutdown chan<- struct{}, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
		}

		conn, err := dialUnixSocket(socket)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		if err := writeFull(conn, ipcHeader(ipcSubscribe, len([]byte(`["window","workspace","shutdown"]`)))); err != nil {
			_ = conn.Close()
			time.Sleep(time.Second)
			continue
		}
		if err := writeFull(conn, []byte(`["window","workspace","shutdown"]`)); err != nil {
			_ = conn.Close()
			time.Sleep(time.Second)
			continue
		}
		if _, _, err := readIPCMessage(conn); err != nil {
			_ = conn.Close()
			time.Sleep(time.Second)
			continue
		}

		reader := bufio.NewReader(conn)
		for {
			body, _, err := readIPCMessage(reader)
			if err != nil {
				_ = conn.Close()
				break
			}
			var event struct {
				Change string `json:"change"`
			}
			_ = json.Unmarshal(body, &event)
			if event.Change == "shutdown" {
				select {
				case shutdown <- struct{}{}:
				default:
				}
				_ = conn.Close()
				return
			}
			select {
			case events <- struct{}{}:
			default:
			}
		}
		time.Sleep(time.Second)
	}
}

func runLoopWithFPS(socket string, fps float64) int {
	stopAudio := func() {}
	if presetUsesAudio(animationPreset) {
		stopAudio = startDefaultAudioMonitor()
	}
	defer stopAudio()

	control := &IPC{socket: socket}
	defer control.Close()

	animator := NewTitleAnimator(control)
	events := make(chan struct{}, 16)
	shutdown := make(chan struct{}, 1)
	done := make(chan struct{})
	defer close(done)
	go subscribe(socket, events, shutdown, done)

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	phase := 0
	animator.RefreshTree(phase)
	initialDuration := frameDuration(fps)
	timer := time.NewTimer(initialDuration)
	defer timer.Stop()
	nextFrames := 1
	nextWakeAt := time.Now().Add(initialDuration)

	for {
		select {
		case <-events:
			animator.RefreshTree(phase)
			candidateFrames := animator.FramesUntilNextWake(phase)
			candidateDuration := time.Duration(candidateFrames) * frameDuration(fps)
			if time.Now().Add(candidateDuration).Before(nextWakeAt) {
				nextFrames = candidateFrames
				nextWakeAt = resetTimer(timer, candidateDuration)
			}
		case <-shutdown:
			if err := animator.ResetAll(); err != nil {
				fmt.Fprintf(os.Stderr, "Unable to reset all title formats: %v\n", err)
			}
			return 0
		case <-signals:
			if err := animator.ResetAll(); err != nil {
				fmt.Fprintf(os.Stderr, "Unable to reset all title formats: %v\n", err)
			}
			return 0
		case <-timer.C:
			phase += nextFrames
			animator.Tick(phase)
			nextFrames = animator.FramesUntilNextWake(phase)
			nextWakeAt = resetTimer(timer, time.Duration(nextFrames)*frameDuration(fps))
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
	names := make([]string, 0, len(animationPresets)+1)
	for name := range animationPresets {
		names = append(names, name)
	}
	names = append(names, "showcase")
	sort.Strings(names)
	for _, name := range names {
		fmt.Println(name)
	}
}
