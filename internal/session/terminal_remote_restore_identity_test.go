package session

import "testing"

func TestRestoreDigestSeparatesRemoteRoutesWithIdenticalLocalMetadata(t *testing.T) {
	original := validRegistry().Contexts[0]
	original.Launcher.Terminal.Remote = &TerminalRemoteBinding{Target: "host-a", Session: "same-name", MachineID: "0123456789abcdef0123456789abcdef"}
	baseline := RestoreContextDigest(original)
	for name, mutate := range map[string]func(*TerminalRemoteBinding){
		"target":  func(binding *TerminalRemoteBinding) { binding.Target = "host-b" },
		"session": func(binding *TerminalRemoteBinding) { binding.Session = "other-name" },
		"profile": func(binding *TerminalRemoteBinding) { binding.MachineID = "1123456789abcdef0123456789abcdef" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := original
			terminal := *original.Launcher.Terminal
			binding := *terminal.Remote
			mutate(&binding)
			terminal.Remote = &binding
			changed.Launcher.Terminal = &terminal
			if err := changed.Validate(); err != nil {
				t.Fatal(err)
			}
			if RestoreContextDigest(changed) == baseline {
				t.Fatal("restore evidence for another remote route retained the same identity digest")
			}
			if RestoreContextDigest(original) != baseline {
				t.Fatal("route comparison changed the original context")
			}
		})
	}
}
