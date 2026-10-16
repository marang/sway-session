package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/swayipc"
)

func remoteForgetFixture(t *testing.T, mutate func(*Context)) (string, Registry, TerminalActivityState) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	remote := testValidContext(testContextID)
	remote.State = ContextArchived
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	remote.ArchivedAt = &at
	remote.Launcher.Terminal.Remote = &TerminalRemoteBinding{Target: "fixture", Session: "remote-work", MachineID: "0123456789abcdef0123456789abcdef"}
	if mutate != nil {
		mutate(&remote)
	}
	local := testValidContext(secondContextID)
	local.Label = "Adjacent local work"
	local.Launcher.Session = "adjacent-local"
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{remote, local}}
	if err := RegistryStoreFor(root).Save(registry); err != nil {
		t.Fatal(err)
	}
	activity := TerminalActivityState{Version: TerminalActivitySchemaVersion, Terminals: []TerminalActivity{{ContextID: remote.ID, CreatedAt: &at}, {ContextID: local.ID, CreatedAt: &at}}}
	if err := TerminalActivityStoreFor(root).Save(activity); err != nil {
		t.Fatal(err)
	}
	return root, registry, activity
}

func assertRemoteForgetRetainsState(t *testing.T, root string, want Registry, wantActivity TerminalActivityState) {
	t.Helper()
	var got Registry
	if err := RegistryStoreFor(root).LoadInto(&got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("rejected forget changed registry: %+v %v", got, err)
	}
	activity, err := ReadTerminalActivitySnapshot(root)
	if err != nil || !reflect.DeepEqual(activity, wantActivity) {
		t.Fatalf("rejected forget changed activity: %+v %v", activity, err)
	}
}

func TestForgetRemoteRefusesIneligibleOrStillPresentContext(t *testing.T) {
	for _, name := range []string{"active", "local", "mapped", "mapped_after_process_scan", "pending_process", "process_scan_failure"} {
		t.Run(name, func(t *testing.T) {
			root, want, activity := remoteForgetFixture(t, func(value *Context) {
				if name == "active" {
					value.State = ContextActive
					value.ArchivedAt = nil
				}
				if name == "local" {
					value.Launcher.Terminal.Remote = nil
				}
			})
			client := &sequencedTerminalManagerClient{id: testContextID, mapped: []bool{false, false}}
			if name == "mapped" {
				client.mapped = []bool{true}
			}
			if name == "mapped_after_process_scan" {
				client.mapped = []bool{false, true}
			}
			reconfigurer := TerminalAdapterReconfigurer{StateRoot: root, Client: client, FindProcesses: func(string, ContextID) ([]int, error) {
				if name == "pending_process" {
					return []int{4321}, nil
				}
				if name == "process_scan_failure" {
					return nil, errors.New("fixture process observation failed")
				}
				return nil, nil
			}}
			if _, err := reconfigurer.ForgetRemote(t.Context(), testContextID); err == nil {
				t.Fatal("ineligible or unproven remote context was forgotten")
			}
			assertRemoteForgetRetainsState(t, root, want, activity)
		})
	}
}

func TestForgetRemoteRemovesOnlyArchivedRegistrationAndActivity(t *testing.T) {
	root, want, activity := remoteForgetFixture(t, nil)
	// Synthetic independent Herdr state must survive metadata-only forget.
	configRoot := filepath.Join(t.TempDir(), "config")
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	paths := []string{filepath.Join(configRoot, "herdr", "sessions", want.Contexts[0].Launcher.Session, "session.json"), filepath.Join(t.TempDir(), "remote-owner", "remote-work", "session.json")}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("synthetic Herdr-owned state"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	client := &sequencedTerminalManagerClient{id: testContextID, mapped: []bool{false, false}}
	scans := 0
	manager := TerminalAdapterReconfigurer{StateRoot: root, Client: client, FindProcesses: func(proc string, id ContextID) ([]int, error) {
		scans++
		if proc != "/proc" || id != testContextID {
			t.Fatalf("proof targeted another context: %s %s", proc, id)
		}
		return nil, nil
	}}
	removed, err := manager.ForgetRemote(t.Context(), testContextID)
	if err != nil || !reflect.DeepEqual(removed, want.Contexts[0]) {
		t.Fatalf("forget result %+v %v", removed, err)
	}
	if scans != 1 || client.requests != 2 {
		t.Fatalf("closed proof incomplete: scans=%d trees=%d", scans, client.requests)
	}
	var got Registry
	if err := RegistryStoreFor(root).LoadInto(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Contexts) != 1 || !reflect.DeepEqual(got.Contexts[0], want.Contexts[1]) {
		t.Fatalf("adjacent local context changed: %+v", got.Contexts)
	}
	gotActivity, err := ReadTerminalActivitySnapshot(root)
	if err != nil || len(gotActivity.Terminals) != 1 || !reflect.DeepEqual(gotActivity.Terminals[0], activity.Terminals[1]) {
		t.Fatalf("activity cascade lost adjacent work or retained removed context: %+v %v", gotActivity, err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "synthetic Herdr-owned state" {
			t.Fatalf("forget touched Herdr-owned files: %s %v", path, err)
		}
	}
}

type failedRemoteForgetObserver struct{ cause error }

func (client failedRemoteForgetObserver) RequestContext(context.Context, swayipc.MessageType, []byte) (swayipc.Message, error) {
	return swayipc.Message{}, client.cause
}

func TestForgetRemoteCancellationAndFailedWindowProofRetainState(t *testing.T) {
	for _, name := range []string{"already_canceled", "cancel_during_proof", "failed_sway_proof"} {
		t.Run(name, func(t *testing.T) {
			root, want, activity := remoteForgetFixture(t, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			manager := TerminalAdapterReconfigurer{StateRoot: root, Client: &terminalManagerClient{id: testContextID}, FindProcesses: func(string, ContextID) ([]int, error) {
				if name == "cancel_during_proof" {
					cancel()
				}
				return nil, nil
			}}
			if name == "already_canceled" {
				cancel()
			}
			failure := errors.New("fixture Sway read failure")
			if name == "failed_sway_proof" {
				manager.Client = failedRemoteForgetObserver{cause: failure}
			}
			_, err := manager.ForgetRemote(ctx, testContextID)
			if err == nil {
				t.Fatal("failed or canceled proof removed registration")
			}
			if name == "failed_sway_proof" && !errors.Is(err, failure) {
				t.Fatalf("lost proof failure: %v", err)
			}
			if name != "failed_sway_proof" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			assertRemoteForgetRetainsState(t, root, want, activity)
		})
	}
}
