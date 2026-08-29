package swayipc

import "testing"

func TestDecodeEventPreservesTypedPayload(t *testing.T) {
	tests := []struct {
		name        string
		messageType MessageType
		payload     string
		wantType    EventType
		wantID      int64
	}{
		{
			name:        "window",
			messageType: windowEventMessage,
			payload:     `{"change":"mark","container":{"id":41}}`,
			wantType:    EventWindow,
			wantID:      41,
		},
		{
			name:        "workspace",
			messageType: workspaceEventMessage,
			payload:     `{"change":"move","current":{"id":42}}`,
			wantType:    EventWorkspace,
			wantID:      42,
		},
		{
			name:        "shutdown",
			messageType: shutdownEventMessage,
			payload:     `{"change":"exit"}`,
			wantType:    EventShutdown,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event, err := DecodeEvent(Message{Type: test.messageType, Payload: []byte(test.payload)})
			if err != nil {
				t.Fatalf("decode event: %v", err)
			}
			if event.Type != test.wantType || event.Change == "" {
				t.Fatalf("unexpected event: %+v", event)
			}
			if event.Container != nil && event.Container.ID != test.wantID {
				t.Fatalf("unexpected container: %+v", event.Container)
			}
			if event.Current != nil && event.Current.ID != test.wantID {
				t.Fatalf("unexpected current workspace: %+v", event.Current)
			}
		})
	}
}

func TestDecodeEventRejectsMalformedOrUnsupportedMessages(t *testing.T) {
	for _, message := range []Message{
		{Type: windowEventMessage, Payload: []byte(`{"change":`)},
		{Type: windowEventMessage, Payload: []byte(`{"container":{"id":1}}`)},
		{Type: eventBit | 99, Payload: []byte(`{"change":"unknown"}`)},
	} {
		if _, err := DecodeEvent(message); err == nil {
			t.Fatalf("expected event to be rejected: %+v", message)
		}
	}
}

func TestEventLayoutRelevanceExcludesPresentationOnlyChanges(t *testing.T) {
	for _, event := range []Event{
		{Type: EventWindow, Change: "title"},
		{Type: EventWindow, Change: "urgent"},
		{Type: EventWorkspace, Change: "urgent"},
		{Type: EventShutdown, Change: "exit"},
	} {
		if event.AffectsSessionLayout() {
			t.Fatalf("presentation-only event affected session layout: %+v", event)
		}
	}
	for _, event := range []Event{
		{Type: EventWindow, Change: "move"},
		{Type: EventWindow, Change: "focus"},
		{Type: EventWorkspace, Change: "rename"},
		{Type: EventWindow, Change: "future-change"},
	} {
		if !event.AffectsSessionLayout() {
			t.Fatalf("layout event was ignored: %+v", event)
		}
	}
}
