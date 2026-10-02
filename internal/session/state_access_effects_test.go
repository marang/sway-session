package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStateAccessSpansApplicationCompensation(t *testing.T) {
	for _, operation := range []string{"register", "rebind", "forget"} {
		t.Run(operation, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			registered := flatpakApplicationContext("org.example.App", "org.example.App")
			registered.ID = testContextID
			registry := emptyRegistry()
			if operation != "register" {
				registry.Contexts = []Context{registered}
			}
			if err := RegistryStoreFor(root).SaveContext(t.Context(), registry); err != nil {
				t.Fatal(err)
			}
			directory := accessTestDirectory(t, root)
			window := appWindow(42, true, "org.example.App", "", "", "org.example.App")
			if operation == "forget" {
				mark, _ := registered.ID.Mark()
				window.Marks = []string{mark}
			}
			if operation == "rebind" {
				sandbox := "org.example.Rebound"
				window.SandboxAppID = &sandbox
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := &mutationSwayClient{tree: applicationTree(window)}
			if operation == "forget" {
				client.honorContext = true
				client.cancelAfterCommand = cancel
			} else {
				client.unknownAfterApply = true
				client.observeFailuresAfterCommand = 2
			}
			compensated := false
			client.beforeCommand = func() {
				requireRecoveryBusy(t, directory)
				if client.commandCalls < 2 {
					return
				}
				compensated = true
				// Compensation keeps registry observation and Sway effects serialized.
				lockErr := unix.Flock(int(directory.Fd()), unix.LOCK_EX|unix.LOCK_NB)
				if lockErr == nil {
					_ = unix.Flock(int(directory.Fd()), unix.LOCK_UN)
					t.Fatal("compensation released its registry lock")
				}
				if !errors.Is(lockErr, unix.EWOULDBLOCK) {
					t.Fatalf("probe registry lock: %v", lockErr)
				}
			}
			var err error
			switch operation {
			case "register":
				err = RegisterApplicationContext(t.Context(), root, client, registered, 42)
			case "rebind":
				replacement := flatpakApplicationContext("org.example.Rebound", "org.example.App")
				replacement.ID = registered.ID
				_, _, err = RebindApplicationContext(t.Context(), root, client, registered, replacement, 42)
			case "forget":
				_, err = ForgetApplicationContext(ctx, root, client, string(registered.ID))
			}
			if err == nil || !compensated {
				t.Fatalf("did not exercise failed mutation and compensation: compensated=%v err=%v", compensated, err)
			}
			gate, err := acquireStateAccessAt(t.Context(), directory, true)
			if err != nil {
				t.Fatalf("operation leaked its guard: %v", err)
			}
			_ = gate.Close()
		})
	}
}
