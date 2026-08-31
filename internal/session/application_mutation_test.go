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

func TestRebindPreservesLifecycleChangedAfterApprovalWasReviewed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	expected := flatpakApplicationContext("org.example.Old", "org.example.Old")
	expected.ID = testContextID
	expected.App.DesiredOpen = true
	if err := RegistryFile(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{expected}}); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateRegistry(root, func(registry *Registry) error {
		registry.Contexts[0].App.RestorePolicy = ApplicationRestorePinned
		return registry.Validate()
	}); err != nil {
		t.Fatal(err)
	}
	replacement := flatpakApplicationContext("org.example.New", "org.example.New")
	replacement.ID = expected.ID
	client := &mutationSwayClient{tree: applicationTree(appWindow(42, true, "org.example.New", "", "", "org.example.New"))}

	if _, _, err := RebindApplicationContext(root, client, expected, replacement, 42); err != nil {
		t.Fatalf("lifecycle-only change invalidated rebind approval: %v", err)
	}
	var registry Registry
	if err := RegistryFile(root).LoadInto(&registry); err != nil {
		t.Fatal(err)
	}
	if registry.Contexts[0].Launcher.FlatpakID != "org.example.New" || registry.Contexts[0].App.RestorePolicy != ApplicationRestorePinned {
		t.Fatalf("rebind did not merge reviewed identity with current lifecycle: %+v", registry.Contexts[0])
	}
}

func TestRebindRejectsLauncherChangedAfterApprovalWasReviewed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	expected := flatpakApplicationContext("org.example.Old", "org.example.Old")
	expected.ID = testContextID
	if err := RegistryFile(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{expected}}); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateRegistry(root, func(registry *Registry) error {
		registry.Contexts[0].Launcher.FlatpakID = "org.example.Concurrent"
		registry.Contexts[0].App.Identity.WaylandAppID = "org.example.Concurrent"
		registry.Contexts[0].App.Identity.SandboxAppID = "org.example.Concurrent"
		return registry.Validate()
	}); err != nil {
		t.Fatal(err)
	}
	replacement := flatpakApplicationContext("org.example.New", "org.example.New")
	replacement.ID = expected.ID
	client := &mutationSwayClient{tree: applicationTree(appWindow(42, true, "org.example.New", "", "", "org.example.New"))}

	if _, _, err := RebindApplicationContext(root, client, expected, replacement, 42); err == nil {
		t.Fatal("rebind accepted a launcher changed after approval")
	}
	if client.commandCalls != 0 {
		t.Fatalf("stale rebind crossed the Sway mutation boundary: %d commands", client.commandCalls)
	}
}

func TestReapprovePreservesLifecycleChangedAfterApprovalWasReviewed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	digest := strings.Repeat("a", 64)
	expected := desktopApplicationContext("local.example.desktop", "local.example")
	expected.ID = testContextID
	expected.Launcher.DesktopOrigin = DesktopEntryUser
	expected.Launcher.DesktopPath = "/home/example/.local/share/applications/local.example.desktop"
	expected.Launcher.DesktopEntrySHA256 = digest
	expected.Launcher.ApprovedDesktopPath = "/home/example/.local/state/sway-session/desktop-approvals/local.example.desktop"
	expected.Launcher.ApprovedExecutablePath = "/home/example/.local/bin/example"
	expected.Launcher.ApprovedExecutableSHA256 = digest
	expected.App.DesiredOpen = true
	if err := RegistryFile(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{expected}}); err != nil {
		t.Fatal(err)
	}
	revision, err := ApplicationOperationContextRevision(expected)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateRegistry(root, func(registry *Registry) error {
		registry.Contexts[0].App.RestorePolicy = ApplicationRestorePinned
		return registry.Validate()
	}); err != nil {
		t.Fatal(err)
	}
	launcher := expected.Launcher
	launcher.DesktopEntrySHA256 = strings.Repeat("b", 64)
	launcher.ApprovedExecutableSHA256 = strings.Repeat("b", 64)

	_, replacement, err := ReapproveApplicationContext(root, expected.ID, revision, launcher)
	if err != nil {
		t.Fatalf("lifecycle-only change invalidated reapproval: %v", err)
	}
	if replacement.Launcher.DesktopEntrySHA256 != strings.Repeat("b", 64) || replacement.App.RestorePolicy != ApplicationRestorePinned {
		t.Fatalf("reapproval did not merge reviewed launcher with current lifecycle: %+v", replacement)
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
