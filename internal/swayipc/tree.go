package swayipc

import (
	"encoding/json"
	"fmt"
)

const eventBit MessageType = 1 << 31

const (
	workspaceEventMessage MessageType = eventBit
	windowEventMessage    MessageType = eventBit | 3
	bindingEventMessage   MessageType = eventBit | 5
	shutdownEventMessage  MessageType = eventBit | 6
	tickEventMessage      MessageType = eventBit | 7
)

// Rect is an absolute Sway logical-pixel rectangle.
type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type WindowProperties struct {
	Class        string `json:"class"`
	Instance     string `json:"instance"`
	WindowRole   string `json:"window_role"`
	WindowType   string `json:"window_type"`
	TransientFor *int64 `json:"transient_for"`
}

// TreeNode is the bounded GET_TREE subset shared by title animation and
// persistent session capture.
type TreeNode struct {
	ID               int64            `json:"id"`
	Name             string           `json:"name"`
	Type             string           `json:"type"`
	PID              int              `json:"pid"`
	Layout           string           `json:"layout"`
	Percent          *float64         `json:"percent"`
	AppID            *string          `json:"app_id"`
	Window           *int64           `json:"window"`
	Focused          bool             `json:"focused"`
	FullscreenMode   int              `json:"fullscreen_mode"`
	Urgent           bool             `json:"urgent"`
	Shell            string           `json:"shell"`
	InhibitIdle      bool             `json:"inhibit_idle"`
	SandboxEngine    *string          `json:"sandbox_engine"`
	SandboxAppID     *string          `json:"sandbox_app_id"`
	SandboxInstance  *string          `json:"sandbox_instance_id"`
	Marks            []string         `json:"marks"`
	Focus            []int64          `json:"focus"`
	Rect             Rect             `json:"rect"`
	DecoRect         Rect             `json:"deco_rect"`
	WindowProperties WindowProperties `json:"window_properties"`
	Nodes            []*TreeNode      `json:"nodes"`
	FloatingNodes    []*TreeNode      `json:"floating_nodes"`
	Parent           *TreeNode        `json:"-"`
}

type EventType string

const (
	EventWorkspace EventType = "workspace"
	EventWindow    EventType = "window"
	EventBinding   EventType = "binding"
	EventShutdown  EventType = "shutdown"
	EventTick      EventType = "tick"
	// EventStream is an internal lifecycle notification. It is never decoded
	// from Sway; StreamSessionEvents emits it after each successful subscription
	// so stateful consumers can discard assumptions made on an older stream.
	EventStream EventType = "stream"
)

// Event retains the typed Sway event and the relevant tree fragments instead
// of reducing every subscription notification to an empty wake-up.
type Event struct {
	Type        EventType
	Change      string
	StreamEpoch uint64
	Container   *TreeNode
	Current     *TreeNode
	Old         *TreeNode
	Payload     string
	First       bool
}

// AffectsSessionLayout excludes presentation-only notifications which may be
// generated continuously by title animation. Unknown changes remain relevant
// so a newer Sway event cannot silently bypass capture.
func (event Event) AffectsSessionLayout() bool {
	switch event.Type {
	case EventWindow:
		return event.Change != "title" && event.Change != "urgent"
	case EventWorkspace:
		return event.Change != "urgent"
	case EventShutdown:
		return false
	case EventTick:
		return false
	case EventStream:
		return event.Change == "ready"
	case EventBinding:
		return false
	default:
		return true
	}
}

// DecodeEvent decodes the subscribed event types used by the daemon.
func DecodeEvent(message Message) (Event, error) {
	var payload struct {
		Change    string    `json:"change"`
		Container *TreeNode `json:"container"`
		Current   *TreeNode `json:"current"`
		Old       *TreeNode `json:"old"`
		Payload   string    `json:"payload"`
		First     bool      `json:"first"`
	}
	if err := json.Unmarshal(message.Payload, &payload); err != nil {
		return Event{}, fmt.Errorf("decode sway event: %w", err)
	}

	var eventType EventType
	switch message.Type {
	case workspaceEventMessage:
		eventType = EventWorkspace
	case windowEventMessage:
		eventType = EventWindow
	case bindingEventMessage:
		eventType = EventBinding
	case shutdownEventMessage:
		eventType = EventShutdown
	case tickEventMessage:
		eventType = EventTick
	default:
		return Event{}, fmt.Errorf("unsupported sway event message type %#x", uint32(message.Type))
	}
	if eventType == EventTick {
		payload.Change = "tick"
	} else if payload.Change == "" {
		return Event{}, fmt.Errorf("sway %s event has no change type", eventType)
	}
	return Event{
		Type:      eventType,
		Change:    payload.Change,
		Container: payload.Container,
		Current:   payload.Current,
		Old:       payload.Old,
		Payload:   payload.Payload,
		First:     payload.First,
	}, nil
}
