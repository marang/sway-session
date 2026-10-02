package session

import (
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
			client := &mutationSwayClient{tree: applicationTree(window), reject: true}
			compensated := false
			client.beforeCommand = func() {
				requireRecoveryBusy(t, directory)
				if client.commandCalls < 2 {
					return
				}
				compensated = true
				// Compensation is outside the registry callback and after its
				// SQLite handle closed; only the outer operation guard covers it.
				if err := unix.Flock(int(directory.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatalf("compensation still holds registry lock: %v", err)
				}
				_ = unix.Flock(int(directory.Fd()), unix.LOCK_UN)
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
				_, err = ForgetApplicationContext(t.Context(), root, client, string(registered.ID))
			}
			if err == nil || !compensated {
				t.Fatalf("did not exercise rejected mutation and compensation: compensated=%v err=%v", compensated, err)
			}
			gate, err := acquireStateAccessAt(t.Context(), directory, true)
			if err != nil {
				t.Fatalf("operation leaked its guard: %v", err)
			}
			_ = gate.Close()
		})
	}
}
