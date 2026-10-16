package session

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestRemoteBindingSurvivesBackupAndRecovery(t *testing.T) {
	for name, binding := range map[string]TerminalRemoteBinding{
		"direct":        {Target: "ssh://builder@example.test:2222", Session: "remote-work"},
		"saved_machine": {Target: "build-alias", Session: "remote-work", MachineID: "0123456789abcdef0123456789abcdef"},
	} {
		t.Run(name, func(t *testing.T) {
			root := backupTestRoot(t)
			registry := validRegistry()
			registry.Contexts[0].Launcher.Terminal.Remote = &binding
			if err := RegistryStoreFor(root).Save(registry); err != nil {
				t.Fatal(err)
			}
			output := backupTestOutput(t)
			if _, err := BackupState(t.Context(), root, output); err != nil {
				t.Fatal(err)
			}
			if _, err := ValidateStateBackup(t.Context(), output); err != nil {
				t.Fatal(err)
			}
			// The backup must keep the original pinned route independently of
			// later writes to the source registry.
			if _, err := UpdateRegistry(root, func(current *Registry) error {
				current.Contexts[0].Launcher.Terminal.Remote.Target = "later-host"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "recovered")
			result, err := RecoverState(t.Context(), target, output, true)
			if err != nil || !result.Applied {
				t.Fatalf("recover remote backup: %+v %v", result, err)
			}
			var recovered Registry
			if err := RegistryStoreFor(target).LoadInto(&recovered); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(recovered, registry) {
				t.Fatalf("recovered registry lost or changed remote binding: got %+v, want %+v", recovered.Contexts, registry.Contexts)
			}
			// An unrelated ordinary mutation after recovery must preserve the
			// route rather than dropping an optional field during resave.
			if _, err := UpdateRegistry(target, func(current *Registry) error {
				current.Contexts[0].Label = "After recovery"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := RegistryStoreFor(target).LoadInto(&recovered); err != nil {
				t.Fatal(err)
			}
			if got := recovered.Contexts[0].Launcher.Terminal.Remote; got == nil || *got != binding {
				t.Fatalf("ordinary recovered-store write dropped remote binding: %+v", got)
			}
		})
	}
}
