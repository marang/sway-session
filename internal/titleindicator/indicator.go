// Package titleindicator defines sway-session's versioned presentation-only
// Sway mark contract. A separate title renderer may consume these marks, but
// they contain no session identity or application-private data.
package titleindicator

import (
	"fmt"
	"strconv"
	"strings"
)

const MarkPrefix = "_sway_session_app_indicator_v1_"

type State string

const (
	Unregistered State = "unregistered"
	Pending      State = "pending"
	Registered   State = "registered"
	Pinned       State = "pinned"
)

type ObservedMark struct {
	Raw         string
	State       State
	ContainerID int64
}

func Mark(state State, containerID int64) (string, error) {
	if !state.valid() {
		return "", fmt.Errorf("unsupported application indicator state %q", state)
	}
	if containerID <= 0 {
		return "", fmt.Errorf("application indicator container ID must be positive")
	}
	return MarkPrefix + string(state) + "_" + strconv.FormatInt(containerID, 10), nil
}

func FromMarks(marks []string, containerID int64) (State, bool) {
	var found State
	for _, mark := range marks {
		observed, ok := parse(mark)
		if !ok || observed.ContainerID != containerID {
			continue
		}
		if found != "" && found != observed.State {
			return "", false
		}
		found = observed.State
	}
	return found, found != ""
}

// KnownMarks returns valid v1 marks in observed order. Unknown future marker
// versions are deliberately ignored so each owner can clean up only its own
// wire version.
func KnownMarks(marks []string) []ObservedMark {
	known := make([]ObservedMark, 0, len(marks))
	seen := make(map[string]struct{}, len(marks))
	for _, mark := range marks {
		if _, duplicate := seen[mark]; duplicate {
			continue
		}
		observed, ok := parse(mark)
		if ok {
			known = append(known, observed)
			seen[mark] = struct{}{}
		}
	}
	return known
}

func parse(mark string) (ObservedMark, bool) {
	rest, ok := strings.CutPrefix(mark, MarkPrefix)
	if !ok {
		return ObservedMark{}, false
	}
	separator := strings.LastIndexByte(rest, '_')
	if separator <= 0 || separator == len(rest)-1 {
		return ObservedMark{}, false
	}
	state := State(rest[:separator])
	containerID, err := strconv.ParseInt(rest[separator+1:], 10, 64)
	if err != nil || containerID <= 0 || !state.valid() {
		return ObservedMark{}, false
	}
	canonical, err := Mark(state, containerID)
	if err != nil || canonical != mark {
		return ObservedMark{}, false
	}
	return ObservedMark{Raw: mark, State: state, ContainerID: containerID}, true
}

func (state State) valid() bool {
	switch state {
	case Unregistered, Pending, Registered, Pinned:
		return true
	default:
		return false
	}
}
