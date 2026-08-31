package swayipc

import (
	"errors"
	"os"
	"time"
)

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
	streamEvents(socket, []byte(`["window","workspace","binding","shutdown"]`), true, events, done)
}

func streamEvents(socket string, subscription []byte, preserveUserIntent bool, events chan<- Event, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
		}

		connection, err := Dial(socket)
		if err != nil {
			if endpointGone(socket) {
				emitShutdown(events, done)
				return
			}
			if waitForDone(done, time.Second) {
				return
			}
			continue
		}
		response, err := connection.Request(Subscribe, subscription)
		if err == nil {
			err = CheckSubscribeResponse(response)
		}
		if err != nil {
			_ = connection.Close()
			if waitForDone(done, time.Second) {
				return
			}
			continue
		}

		for {
			message, err := connection.Read()
			if err != nil {
				_ = connection.Close()
				if endpointGone(socket) {
					emitShutdown(events, done)
					return
				}
				break
			}
			event, err := DecodeEvent(message)
			if err != nil {
				_ = connection.Close()
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

func deliverEvent(events chan<- Event, done <-chan struct{}, event Event, preserveUserIntent bool) bool {
	if preserveUserIntent && eventSupersedesSessionRestore(event) {
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
