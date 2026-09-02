package swayipc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// EventStreamState publishes subscription lifecycle changes before their
// corresponding events reach a possibly busy consumer. Odd epochs are
// connected and even epochs are disconnected.
type EventStreamState struct {
	epoch atomic.Uint64
}

// Snapshot returns the current lifecycle epoch and connection state.
func (state *EventStreamState) Snapshot() (uint64, bool) {
	if state == nil {
		return 0, false
	}
	epoch := state.epoch.Load()
	return epoch, epoch%2 == 1
}

func (state *EventStreamState) transition() uint64 {
	return state.epoch.Add(1)
}

// StreamEvents reconnects one bounded subscription to the tree events needed
// by title animation and persistent session observation. Delivery is
// coalescing: consumers must re-read GET_TREE instead of treating events as a
// complete state log.
func StreamEvents(socket string, events chan<- Event, done <-chan struct{}) {
	streamEvents(socket, []byte(`["window","workspace","shutdown"]`), false, events, done)
}

// StreamSessionEvents adds binding activity needed to keep persistent restore
// work subordinate to live user intent. The title animator deliberately uses
// the narrower StreamEvents subscription.
func StreamSessionEvents(socket string, events chan<- Event, done <-chan struct{}) {
	StreamSessionEventsWithState(socket, events, done, &EventStreamState{})
}

// StreamSessionEventsWithState also exposes synchronous stream lifecycle
// state so in-flight restore work can stop before the event loop catches up.
func StreamSessionEventsWithState(socket string, events chan<- Event, done <-chan struct{}, state *EventStreamState) {
	if state == nil {
		state = &EventStreamState{}
	}
	streamEvents(socket, []byte(`["window","workspace","binding","shutdown","tick"]`), true, events, done, state)
}

func streamEvents(socket string, subscription []byte, preserveUserIntent bool, events chan<- Event, done <-chan struct{}, states ...*EventStreamState) {
	var state *EventStreamState
	if len(states) != 0 {
		state = states[0]
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
		}

		connection, err := OpenSubscriptionContext(ctx, socket, subscription, defaultRequestTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if endpointGone(socket) {
				emitShutdown(events, done)
				return
			}
			if waitForDone(done, time.Second) {
				return
			}
			continue
		}
		epoch := uint64(0)
		if preserveUserIntent {
			epoch = state.transition()
		}
		if preserveUserIntent && !emitEventStreamReady(events, done, epoch) {
			_ = connection.Close()
			return
		}

		for {
			message, err := connection.ReadContext(ctx)
			if err != nil {
				_ = connection.Close()
				if ctx.Err() != nil {
					return
				}
				epoch := uint64(0)
				if preserveUserIntent {
					epoch = state.transition()
				}
				if endpointGone(socket) {
					emitShutdown(events, done)
					return
				}
				if preserveUserIntent && !emitEventStreamDisconnected(events, done, epoch) {
					return
				}
				break
			}
			event, err := DecodeEvent(message)
			if err != nil {
				_ = connection.Close()
				epoch := uint64(0)
				if preserveUserIntent {
					epoch = state.transition()
				}
				if preserveUserIntent && !emitEventStreamDisconnected(events, done, epoch) {
					return
				}
				break
			}
			if event.Type == EventShutdown {
				select {
				case events <- event:
				case <-done:
				}
				_ = connection.Close()
				return
			}
			if !deliverEvent(events, done, event, preserveUserIntent) {
				_ = connection.Close()
				return
			}
		}
		if waitForDone(done, time.Second) {
			return
		}
	}
}

// OpenSubscriptionContext bounds connection establishment and subscription as
// one setup operation. The returned connection is no longer tied to the setup
// deadline and remains usable for event reads.
func OpenSubscriptionContext(ctx context.Context, socket string, subscription []byte, timeout time.Duration) (*Conn, error) {
	if ctx == nil {
		return nil, errors.New("sway subscription context is nil")
	}
	if timeout <= 0 {
		return nil, errors.New("sway subscription timeout must be positive")
	}
	setupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	connection, err := DialContext(setupCtx, socket)
	if err != nil {
		return nil, fmt.Errorf("connect to Sway events: %w", err)
	}
	response, err := connection.RequestContext(setupCtx, Subscribe, subscription)
	if err == nil {
		err = CheckSubscribeResponse(response)
	}
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("subscribe to Sway events: %w", err)
	}
	return connection, nil
}

func emitEventStreamReady(events chan<- Event, done <-chan struct{}, epoch uint64) bool {
	select {
	case events <- Event{Type: EventStream, Change: "ready", StreamEpoch: epoch}:
		return true
	case <-done:
		return false
	}
}

func emitEventStreamDisconnected(events chan<- Event, done <-chan struct{}, epoch uint64) bool {
	select {
	case events <- Event{Type: EventStream, Change: "disconnected", StreamEpoch: epoch}:
		return true
	case <-done:
		return false
	}
}

func deliverEvent(events chan<- Event, done <-chan struct{}, event Event, preserveUserIntent bool) bool {
	if preserveUserIntent && eventNeedsLosslessSessionDelivery(event) {
		select {
		case events <- event:
			return true
		case <-done:
			return false
		}
	}
	select {
	case events <- event:
	default:
	}
	return true
}

func eventNeedsLosslessSessionDelivery(event Event) bool {
	return event.Type == EventTick || eventSupersedesSessionRestore(event)
}

func eventSupersedesSessionRestore(event Event) bool {
	return event.Type == EventBinding ||
		event.Type == EventWindow && (event.Change == "focus" || event.Change == "move" || event.Change == "close") ||
		event.Type == EventWorkspace && event.Change == "focus"
}

func endpointGone(socket string) bool {
	_, err := os.Lstat(socket)
	return errors.Is(err, os.ErrNotExist)
}

func emitShutdown(events chan<- Event, done <-chan struct{}) {
	select {
	case events <- Event{Type: EventShutdown, Change: "endpoint-gone"}:
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
