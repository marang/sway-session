package shutdownwatch

import (
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestInlineSignalHandlerDeliversPreparationSynchronously(t *testing.T) {
	handler := &inlineSignalHandler{}
	delivered := false
	handler.setSink(func(received event) {
		delivered = true
		if received.kind != eventPrepareShutdown || !received.active || received.source != ":1.44" {
			t.Fatalf("decoded event = %+v", received)
		}
	})

	handler.DeliverSignal(managerInterface, "PrepareForShutdown", &dbus.Signal{
		Sender: ":1.44",
		Path:   managerPath,
		Body:   []any{true},
	})
	if !delivered {
		t.Fatal("DeliverSignal returned before invoking the monitor")
	}
}

func TestDecodeSessionLifecycleSignals(t *testing.T) {
	t.Run("state changed", func(t *testing.T) {
		received, relevant := decodeSignal(propertiesIface, "PropertiesChanged", &dbus.Signal{
			Sender: ":1.44",
			Path:   dbus.ObjectPath(testSessionPath),
			Body: []any{
				sessionInterface,
				map[string]dbus.Variant{"State": dbus.MakeVariant("closing")},
				[]string{},
			},
		})
		if !relevant || received.kind != eventSessionState || received.state != "closing" ||
			received.path != testSessionPath || received.err != nil {
			t.Fatalf("decodeSignal() = (%+v, %t)", received, relevant)
		}
	})

	t.Run("state invalidated", func(t *testing.T) {
		received, relevant := decodeSignal(propertiesIface, "PropertiesChanged", &dbus.Signal{
			Sender: ":1.44",
			Path:   dbus.ObjectPath(testSessionPath),
			Body: []any{
				sessionInterface,
				map[string]dbus.Variant{},
				[]string{"State"},
			},
		})
		if !relevant || received.kind != eventSessionState || !received.invalidated {
			t.Fatalf("decodeSignal() = (%+v, %t)", received, relevant)
		}
	})

	t.Run("session removed", func(t *testing.T) {
		received, relevant := decodeSignal(managerInterface, "SessionRemoved", &dbus.Signal{
			Sender: ":1.44",
			Path:   managerPath,
			Body:   []any{"31", dbus.ObjectPath(testSessionPath)},
		})
		if !relevant || received.kind != eventSessionRemoved || received.path != testSessionPath {
			t.Fatalf("decodeSignal() = (%+v, %t)", received, relevant)
		}
	})
}

func TestDecodeNameOwnerChange(t *testing.T) {
	received, relevant := decodeSignal(busInterface, "NameOwnerChanged", &dbus.Signal{
		Sender: busInterface,
		Path:   busPath,
		Body:   []any{loginDestination, ":1.44", ":1.52"},
	})
	if !relevant || received.kind != eventNameOwnerChanged || received.err != nil {
		t.Fatalf("decodeSignal() = (%+v, %t)", received, relevant)
	}
}

func TestMalformedRelevantSignalFailsClosed(t *testing.T) {
	received, relevant := decodeSignal(managerInterface, "PrepareForSleep", &dbus.Signal{
		Sender: ":1.44",
		Path:   managerPath,
		Body:   []any{"true"},
	})
	if !relevant || received.kind != eventPrepareSleep || received.err == nil ||
		!strings.Contains(received.err.Error(), "want bool") {
		t.Fatalf("decodeSignal() = (%+v, %t), want malformed relevant event", received, relevant)
	}
}

func TestDecodeIgnoresUnrelatedSignalsAndProperties(t *testing.T) {
	tests := []struct {
		iface  string
		member string
		signal *dbus.Signal
	}{
		{
			iface:  "org.example.Other",
			member: "Changed",
			signal: &dbus.Signal{Path: managerPath},
		},
		{
			iface:  propertiesIface,
			member: "PropertiesChanged",
			signal: &dbus.Signal{
				Path: dbus.ObjectPath(testSessionPath),
				Body: []any{
					sessionInterface,
					map[string]dbus.Variant{"IdleHint": dbus.MakeVariant(true)},
					[]string{},
				},
			},
		},
	}
	for _, test := range tests {
		if received, relevant := decodeSignal(test.iface, test.member, test.signal); relevant {
			t.Fatalf("decodeSignal(%s.%s) = %+v, want unrelated", test.iface, test.member, received)
		}
	}
}
