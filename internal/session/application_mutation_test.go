package session

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marang/sway-title-animator/internal/swayipc"
)

func TestRegisterApplicationContextMarksAndEnablesIndicatorsInOneTransaction(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	context := flatpakApplicationContext("org.example.App", "org.example.App")
	context.ID = testContextID
	client := &mutationSwayClient{tree: applicationTree(appWindow(42, true, "org.example.App", "", "", "org.example.App"))}
	if err := RegisterApplicationContext(root, client, context, 42); err != nil {
		t.Fatal(err)
	}
	var registry Registry
	if err := RegistryFile(root).LoadInto(&registry); err != nil {
		t.Fatal(err)
	}
	mark, _ := context.ID.Mark()
	window, _ := findContainer(client.tree, 42)
	if len(registry.Contexts) != 1 || !registry.Preferences.DesktopIndicators || !containsMark(window.Marks, mark) {
		t.Fatalf("registration did not converge atomically: registry=%+v marks=%q", registry, window.Marks)
	}
}

func TestRegisterApplicationContextRollsBackWhenSwayRejectsMark(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	context := flatpakApplicationContext("org.example.App", "org.example.App")
	context.ID = testContextID
	client := &mutationSwayClient{tree: applicationTree(appWindow(42, true, "org.example.App", "", "", "org.example.App")), reject: true}
	if err := RegisterApplicationContext(root, client, context, 42); err == nil {
		t.Fatal("Sway mark rejection was accepted")
	}
	var registry Registry
	if err := RegistryFile(root).LoadInto(&registry); err == nil && len(registry.Contexts) != 0 {
		t.Fatalf("rejected mark left registry mutation: %+v", registry)
	}
}

func TestSetContextMarkReobservesUnknownCommandOutcome(t *testing.T) {
	window := appWindow(42, true, "org.example.App", "", "", "")
	client := &mutationSwayClient{tree: applicationTree(window), unknownAfterApply: true}
	if err := SetContextMark(client, 42, testContextID, true); err != nil {
		t.Fatalf("observably successful unknown outcome failed: %v", err)
	}
	mark, _ := testContextID.Mark()
	if !containsMark(window.Marks, mark) || client.commandCalls != 1 {
		t.Fatalf("unknown command was replayed or not observed: calls=%d marks=%q", client.commandCalls, window.Marks)
	}
}

func TestRegisterRollsBackAttemptedMarkWhenUnknownOutcomeCannotBeObserved(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	context := flatpakApplicationContext("org.example.App", "org.example.App")
	context.ID = testContextID
	window := appWindow(42, true, "org.example.App", "", "", "org.example.App")
	client := &mutationSwayClient{tree: applicationTree(window), unknownAfterApply: true, observeFailures: 1}
	if err := RegisterApplicationContext(root, client, context, 42); err == nil {
		t.Fatal("unobservable mark outcome was accepted")
	}
	mark, _ := context.ID.Mark()
	if containsMark(window.Marks, mark) {
		t.Fatalf("attempted mark was orphaned after registry rollback: %q", window.Marks)
	}
}

func TestPinnedRestorePolicyForcesDesiredOpen(t *testing.T) {
	context := flatpakApplicationContext("org.example.App", "org.example.App")
	context.ID = testContextID
	context.App.DesiredOpen = false
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{context}}
	changed, err := SetApplicationRestorePolicy(&registry, string(context.ID), ApplicationRestorePinned)
	if err != nil {
		t.Fatal(err)
	}
	if !changed.App.DesiredOpen || changed.App.RestorePolicy != ApplicationRestorePinned {
		t.Fatalf("pin did not express an open invariant: %+v", changed.App)
	}
}

func TestSwaynagApprovalPresenterUsesOnlyFixedCommandAndValidatedToken(t *testing.T) {
	starter := &launcherRecordingStarter{}
	presenter := SwaynagApprovalPresenter{Swaynag: "/usr/bin/swaynag", SwaySession: "/usr/bin/sway-session", Starter: starter}
	token := strings.Repeat("a", 32)
	if err := presenter.Present("Register Example?", []ApprovalChoice{{Label: "Example; $(touch nope)", Token: token}}); err != nil {
		t.Fatal(err)
	}
	wantAction := "/usr/bin/sway-session app confirm " + token
	if starter.spec.Name != "/usr/bin/swaynag" || starter.spec.Arguments[len(starter.spec.Arguments)-1] != wantAction {
		t.Fatalf("unexpected swaynag action: %+v", starter.spec)
	}
	if err := presenter.Present("Register?", []ApprovalChoice{{Label: "Unsafe", Token: "a;touch-nope"}}); err == nil {
		t.Fatal("unsafe operation token entered swaynag action")
	}
}

type mutationSwayClient struct {
	tree              *swayipc.TreeNode
	reject            bool
	unknownAfterApply bool
	observeFailures   int
	commandCalls      int
}

func (client *mutationSwayClient) Request(messageType swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if messageType == swayipc.GetTree {
		if client.observeFailures > 0 {
			client.observeFailures--
			return swayipc.Message{}, errors.New("tree unavailable")
		}
		data, err := json.Marshal(client.tree)
		return swayipc.Message{Type: swayipc.GetTree, Payload: data}, err
	}
	if messageType != swayipc.RunCommand {
		return swayipc.Message{}, errors.New("unexpected request")
	}
	client.commandCalls++
	command := string(payload)
	mark := command[strings.LastIndex(command, " ")+1:]
	node, err := findContainer(client.tree, 42)
	if err != nil {
		return swayipc.Message{}, err
	}
	if !client.reject {
		if strings.Contains(command, "mark --add") && !containsMark(node.Marks, mark) {
			node.Marks = append(node.Marks, mark)
		}
		if strings.Contains(command, " unmark ") {
			marks := node.Marks[:0]
			for _, current := range node.Marks {
				if current != mark {
					marks = append(marks, current)
				}
			}
			node.Marks = marks
		}
	}
	if client.unknownAfterApply {
		return swayipc.Message{}, errors.New("connection lost after send")
	}
	if client.reject {
		return swayipc.Message{Type: swayipc.RunCommand, Payload: []byte(`[{"success":false,"error":"rejected"}]`)}, nil
	}
	return swayipc.Message{Type: swayipc.RunCommand, Payload: []byte(`[{"success":true}]`)}, nil
}

func containsMark(marks []string, want string) bool {
	for _, mark := range marks {
		if mark == want {
			return true
		}
	}
	return false
}
