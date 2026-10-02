package session

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/marang/sway-session/internal/swayipc"
)

func TestForgetCompensationRevalidatesWindowOwnership(t *testing.T) {
	for _, change := range []string{"unchanged", "process_replaced", "foreign_mark", "mark_moved", "closed", "compositor_replaced"} {
		t.Run(change, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			registered := flatpakApplicationContext("org.example.App", "org.example.App")
			registered.ID = testContextID
			original := emptyRegistry()
			original.Contexts = []Context{registered}
			if err := RegistryStoreFor(root).SaveContext(t.Context(), original); err != nil {
				t.Fatal(err)
			}
			mark, _ := registered.ID.Mark()
			window := appWindow(42, true, "org.example.App", "", "", "org.example.App")
			window.PID = 123
			window.Marks = []string{mark}
			base := &mutationSwayClient{tree: applicationTree(window), honorContext: true}
			client := &forgetEpochClient{mutationSwayClient: base, epoch: "original"}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			base.cancelAfterCommand = func() {
				node, err := findContainer(base.tree, 42)
				if err != nil {
					t.Fatal(err)
				}
				switch change {
				case "process_replaced":
					node.PID++
				case "foreign_mark":
					foreign, _ := ContextID("11111111-1111-4111-8111-111111111111").Mark()
					node.Marks = []string{foreign}
				case "mark_moved":
					other := appWindow(43, false, "org.example.App", "", "", "org.example.App")
					other.Marks = []string{mark}
					base.tree.Nodes[0].Nodes[0].Nodes = append(base.tree.Nodes[0].Nodes[0].Nodes, other)
				case "closed":
					base.tree = &swayipc.TreeNode{ID: 1, Type: "root"}
				case "compositor_replaced":
					client.epoch = "replacement"
				}
				cancel()
			}
			_, err := ForgetApplicationContext(ctx, root, client, string(registered.ID))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("failed forget must retain cancellation: %v", err)
			}
			var actual Registry
			if err := RegistryStoreFor(root).LoadInto(&actual); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, original) {
				t.Fatalf("canceled forget changed registry: got %+v want %+v", actual, original)
			}
			wantCommands := 1
			if change == "unchanged" {
				wantCommands = 2
				node, _ := findContainer(base.tree, 42)
				if !containsMark(node.Marks, mark) {
					t.Fatal("failed forget did not restore unchanged window mark")
				}
			}
			if base.commandCalls != wantCommands {
				t.Fatalf("compensation acted on changed window: commands=%d want=%d err=%v", base.commandCalls, wantCommands, err)
			}
			if err := WithTerminalLifecycleLockContext(t.Context(), root, func() error { return nil }); err != nil {
				t.Fatalf("forget leaked lifecycle lock: %v", err)
			}
		})
	}
}

func TestForgetReconcilesUncertainCommit(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "not_committed", true: "committed_ack_lost"}[committed], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			registered := flatpakApplicationContext("org.example.App", "org.example.App")
			registered.ID = testContextID
			registry := emptyRegistry()
			registry.Contexts = []Context{registered}
			if err := RegistryStoreFor(root).Save(registry); err != nil {
				t.Fatal(err)
			}
			window := appWindow(42, true, "org.example.App", "", "", "org.example.App")
			mark, _ := registered.ID.Mark()
			window.Marks = []string{mark}
			client := &mutationSwayClient{tree: applicationTree(window)}
			originalCommit := executeStateCommit
			t.Cleanup(func() { executeStateCommit = originalCommit })
			injected := false
			failure := errors.New("injected uncertain forget commit")
			executeStateCommit = func(tx *stateWriteTransaction) error {
				if !injected && client.commandCalls == 1 {
					injected = true
					if committed {
						if err := originalCommit(tx); err != nil {
							return err
						}
					}
					return failure
				}
				return originalCommit(tx)
			}
			removed, err := ForgetApplicationContext(t.Context(), root, client, string(registered.ID))
			if !injected {
				t.Fatal("commit fault not exercised")
			}
			if committed && (err != nil || !reflect.DeepEqual(removed, registered)) {
				t.Fatalf("committed forget must report confirmed removal: %+v %v", removed, err)
			}
			if !committed && !errors.Is(err, failure) {
				t.Fatalf("failed commit must remain an error: %v", err)
			}
			if err := RegistryStoreFor(root).LoadInto(&registry); err != nil {
				t.Fatal(err)
			}
			wantContexts, wantCommands := 0, 1
			if !committed {
				wantContexts, wantCommands = 1, 2
			}
			if len(registry.Contexts) != wantContexts || client.commandCalls != wantCommands || containsMark(window.Marks, mark) == committed {
				t.Fatalf("incorrect reconciliation: contexts=%d commands=%d marks=%v", len(registry.Contexts), client.commandCalls, window.Marks)
			}
		})
	}
}

type forgetEpochClient struct {
	*mutationSwayClient
	epoch string
}

func (client *forgetEpochClient) LifecycleCompositorID(context.Context) (string, error) {
	return client.epoch, nil
}

var _ SwayRequestClient = (*forgetEpochClient)(nil)
