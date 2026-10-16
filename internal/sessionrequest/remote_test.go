package sessionrequest

import (
	"reflect"
	"strings"
	"testing"

	sessionstate "github.com/marang/sway-session/internal/session"
)

func TestServiceV1CannotAdoptRemoteContextWithMatchingLocalMetadata(t *testing.T) {
	for name, binding := range map[string]sessionstate.TerminalRemoteBinding{
		"direct":        {Target: "host-a", Session: "remote-work"},
		"saved_machine": {Target: "host-a", Session: "remote-work", MachineID: "0123456789abcdef0123456789abcdef"},
	} {
		t.Run(name, func(t *testing.T) {
			service, request, client, runner := testService(t)
			initializer := &fakeSessionInitializer{}
			service.Initializer = initializer
			contextValue := registeredContext(request)
			contextValue.Launcher.Terminal.Remote = &binding
			want := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue}}
			saveServiceTestRegistry(t, service.StateRoot, want)
			if _, err := service.Handle(t.Context(), request); err == nil || !strings.Contains(err.Error(), "conflicting context metadata") {
				t.Fatalf("protocol v1 adopted a remote route without remote identity: %v", err)
			}
			if len(client.commands) != 0 || len(runner.calls) != 0 || len(initializer.calls) != 0 {
				t.Fatalf("rejected remote context caused effects: commands=%v restores=%v initialized=%v", client.commands, runner.calls, initializer.calls)
			}
			var got sessionstate.Registry
			if err := sessionstate.RegistryStoreFor(service.StateRoot).LoadInto(&got); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("rejected v1 request changed remote registration: %+v %v", got, err)
			}
		})
	}
}
