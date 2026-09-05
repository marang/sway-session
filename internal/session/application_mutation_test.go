package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/swayipc"
)

func TestRegisterApplicationContextMarksAndEnablesIndicatorsInOneTransaction(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	context := flatpakApplicationContext("org.example.App", "org.example.App")
	context.ID = testContextID
	client := &mutationSwayClient{tree: applicationTree(appWindow(42, true, "org.example.App", "", "", "org.example.App"))}
	if err := RegisterApplicationContext(t.Context(), root, client, context, 42); err != nil {
		t.Fatal(err)
	}
	var registry Registry
	if err := RegistryStoreFor(root).LoadInto(&registry); err != nil {
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
	if err := RegisterApplicationContext(t.Context(), root, client, context, 42); err == nil {
		t.Fatal("Sway mark rejection was accepted")
	}
	var registry Registry
	if err := RegistryStoreFor(root).LoadInto(&registry); err == nil && len(registry.Contexts) != 0 {
		t.Fatalf("rejected mark left registry mutation: %+v", registry)
	}
}

func TestSetContextMarkReobservesUnknownCommandOutcome(t *testing.T) {
	window := appWindow(42, true, "org.example.App", "", "", "")
	client := &mutationSwayClient{tree: applicationTree(window), unknownAfterApply: true}
	if err := SetContextMark(t.Context(), client, 42, testContextID, true); err != nil {
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
	if err := RegisterApplicationContext(t.Context(), root, client, context, 42); err == nil {
		t.Fatal("unobservable mark outcome was accepted")
	}
	mark, _ := context.ID.Mark()
	if containsMark(window.Marks, mark) {
		t.Fatalf("attempted mark was orphaned after registry rollback: %q", window.Marks)
	}
}

func TestRegisterDoesNotRollBackMarkWhenCommittedRegistryCannotBeReconciled(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	applicationContext := flatpakApplicationContext("org.example.App", "org.example.App")
	applicationContext.ID = testContextID
	committed := Registry{
		Version:     ContextsSchemaVersion,
		Preferences: RegistryPreferences{DesktopIndicators: true},
		Contexts:    []Context{applicationContext},
	}
	window := appWindow(42, true, "org.example.App", "", "", "org.example.App")
	wroteCommittedRegistry := false
	client := &mutationSwayClient{
		tree:              applicationTree(window),
		unknownAfterApply: true,
		observeFailures:   1,
		beforeCommand: func() {
			if wroteCommittedRegistry {
				return
			}
			wroteCommittedRegistry = true
			writeRegistryBypassingLifecycleLock(t, root, committed)
			if err := os.Chmod(filepath.Join(root, StateDatabaseFilename), 0); err != nil {
				t.Fatalf("make reconciliation load fail: %v", err)
			}
		},
	}

	err := RegisterApplicationContext(t.Context(), root, client, applicationContext, 42)
	if err == nil {
		t.Fatal("unknown registry reconciliation was accepted")
	}
	mark, _ := applicationContext.ID.Mark()
	if !containsMark(window.Marks, mark) {
		t.Fatalf("unknown registry reconciliation destructively removed committed mark: %q", window.Marks)
	}
	if !strings.Contains(err.Error(), "daemon reconciliation") {
		t.Fatalf("reconciliation failure was not actionable: %v", err)
	}

	path := filepath.Join(root, StateDatabaseFilename)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("restore committed registry permissions: %v", err)
	}
	var visible Registry
	if err := RegistryStoreFor(root).LoadInto(&visible); err != nil {
		t.Fatalf("load committed registry: %v", err)
	}
	if len(visible.Contexts) != 1 || !reflect.DeepEqual(visible.Contexts[0], applicationContext) {
		t.Fatalf("unexpected committed registry: %+v", visible)
	}
}

func TestRegisterDoesNotRollBackCommittedMarkAfterConcurrentLifecycleChange(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	applicationContext := flatpakApplicationContext("org.example.App", "org.example.App")
	applicationContext.ID = testContextID
	committedContext := applicationContext
	committedContext.App = &Application{
		Identity:      applicationContext.App.Identity,
		DesiredOpen:   true,
		RestorePolicy: ApplicationRestorePinned,
	}
	committed := Registry{
		Version:     ContextsSchemaVersion,
		Preferences: RegistryPreferences{DesktopIndicators: true},
		Contexts:    []Context{committedContext},
	}
	window := appWindow(42, true, "org.example.App", "", "", "org.example.App")
	wroteCommittedRegistry := false
	client := &mutationSwayClient{
		tree:                        applicationTree(window),
		unknownAfterApply:           true,
		observeFailuresAfterCommand: 1,
		beforeCommand: func() {
			if wroteCommittedRegistry {
				return
			}
			wroteCommittedRegistry = true
			writeRegistryBypassingLifecycleLock(t, root, committed)
		},
	}

	if err := RegisterApplicationContext(t.Context(), root, client, applicationContext, 42); err != nil {
		t.Fatalf("committed registration was mistaken for rollback after lifecycle change: %v", err)
	}
	mark, _ := applicationContext.ID.Mark()
	if !containsMark(window.Marks, mark) {
		t.Fatalf("concurrent lifecycle change caused committed mark rollback: %q", window.Marks)
	}
	var visible Registry
	if err := RegistryStoreFor(root).LoadInto(&visible); err != nil {
		t.Fatal(err)
	}
	if len(visible.Contexts) != 1 || visible.Contexts[0].App.RestorePolicy != ApplicationRestorePinned {
		t.Fatalf("concurrent lifecycle state was not preserved: %+v", visible)
	}
}

func TestApplicationMutationReconciliationIgnoresOnlyLifecycleFields(t *testing.T) {
	expected := flatpakApplicationContext("org.example.App", "org.example.App")
	expected.ID = testContextID
	current := expected
	current.State = ContextArchived
	archivedAt := time.Now().UTC()
	current.ArchivedAt = &archivedAt
	current.App = &Application{
		Identity:      expected.App.Identity,
		DesiredOpen:   true,
		RestorePolicy: ApplicationRestorePinned,
	}
	if !sameApplicationMutation(current, expected) {
		t.Fatal("authoritative lifecycle change hid a committed application mutation")
	}

	differentLauncher := current
	differentLauncher.Launcher.FlatpakID = "org.example.Other"
	if sameApplicationMutation(differentLauncher, expected) {
		t.Fatal("different launcher was mistaken for the committed mutation")
	}
	differentIdentity := current
	differentIdentity.App = &Application{
		Identity: ApplicationIdentity{Protocol: WindowWayland, WaylandAppID: "org.example.Other"},
	}
	if sameApplicationMutation(differentIdentity, expected) {
		t.Fatal("different window identity was mistaken for the committed mutation")
	}
}

func TestRegisterUsesFreshBoundedContextForRollbackAfterCancellation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	applicationContext := flatpakApplicationContext("org.example.App", "org.example.App")
	applicationContext.ID = testContextID
	window := appWindow(42, true, "org.example.App", "", "", "org.example.App")
	ctx, cancel := context.WithCancel(context.Background())
	client := &mutationSwayClient{
		tree:               applicationTree(window),
		honorContext:       true,
		cancelAfterCommand: cancel,
	}
	if err := RegisterApplicationContext(ctx, root, client, applicationContext, 42); err == nil {
		t.Fatal("canceled registration was accepted")
	}
	mark, _ := applicationContext.ID.Mark()
	if containsMark(window.Marks, mark) {
		t.Fatalf("canceled registration left an orphaned Sway mark: %q", window.Marks)
	}
}

func TestRepairApplicationMarkHoldsRegistryLockAcrossSwayMutation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	registered := flatpakApplicationContext("org.example.App", "org.example.App")
	registered.ID = testContextID
	if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{registered}}); err != nil {
		t.Fatal(err)
	}
	commandStarted := make(chan struct{})
	releaseCommand := make(chan struct{})
	client := &mutationSwayClient{
		tree: applicationTree(appWindow(42, true, "org.example.App", "", "", "org.example.App")),
		beforeCommand: func() {
			close(commandStarted)
			<-releaseCommand
		},
	}
	repairDone := make(chan error, 1)
	go func() {
		repairDone <- RepairApplicationMark(t.Context(), root, client, 42, registered)
	}()
	<-commandStarted

	updateAttempted := make(chan struct{})
	updateEntered := make(chan struct{})
	updateDone := make(chan error, 1)
	releaseCheck := make(chan bool, 1)
	go func() {
		<-updateAttempted
		timer := time.NewTimer(25 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-updateEntered:
			releaseCheck <- true
		case <-timer.C:
			releaseCheck <- false
		}
		close(releaseCommand)
	}()
	go func() {
		close(updateAttempted)
		_, err := UpdateRegistry(root, func(registry *Registry) error {
			close(updateEntered)
			registry.Contexts[0].Label = "Concurrent update"
			return registry.Validate()
		})
		updateDone <- err
	}()
	if <-releaseCheck {
		<-repairDone
		t.Fatal("concurrent registry mutation crossed the in-flight Sway repair boundary")
	}

	if err := <-repairDone; err != nil {
		t.Fatalf("repair failed: %v", err)
	}
	if err := <-updateDone; err != nil {
		t.Fatalf("concurrent registry mutation failed: %v", err)
	}
	var updated Registry
	if err := RegistryStoreFor(root).LoadInto(&updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Contexts) != 1 || updated.Contexts[0].Label != "Concurrent update" {
		t.Fatalf("registry mutation after repair was not persisted: %+v", updated.Contexts)
	}
}

func TestRebindPreservesLifecycleChangedAfterApprovalWasReviewed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	expected := flatpakApplicationContext("org.example.Old", "org.example.Old")
	expected.ID = testContextID
	expected.App.DesiredOpen = true
	if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{expected}}); err != nil {
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

	if _, _, err := RebindApplicationContext(t.Context(), root, client, expected, replacement, 42); err != nil {
		t.Fatalf("lifecycle-only change invalidated rebind approval: %v", err)
	}
	var registry Registry
	if err := RegistryStoreFor(root).LoadInto(&registry); err != nil {
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
	if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{expected}}); err != nil {
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

	if _, _, err := RebindApplicationContext(t.Context(), root, client, expected, replacement, 42); err == nil {
		t.Fatal("rebind accepted a launcher changed after approval")
	}
	if client.commandCalls != 0 {
		t.Fatalf("stale rebind crossed the Sway mutation boundary: %d commands", client.commandCalls)
	}
}

func TestRebindDoesNotRollBackMarkWhenCommittedRegistryCannotBeReconciled(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	expected := flatpakApplicationContext("org.example.Old", "org.example.Old")
	expected.ID = testContextID
	if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{expected}}); err != nil {
		t.Fatal(err)
	}
	replacement := flatpakApplicationContext("org.example.New", "org.example.New")
	replacement.ID = expected.ID
	committed := Registry{Version: ContextsSchemaVersion, Contexts: []Context{replacement}}
	window := appWindow(42, true, "org.example.New", "", "", "org.example.New")
	wroteCommittedRegistry := false
	client := &mutationSwayClient{
		tree:                        applicationTree(window),
		unknownAfterApply:           true,
		observeFailuresAfterCommand: 1,
		beforeCommand: func() {
			if wroteCommittedRegistry {
				return
			}
			wroteCommittedRegistry = true
			writeRegistryBypassingLifecycleLock(t, root, committed)
			if err := os.Chmod(filepath.Join(root, StateDatabaseFilename), 0); err != nil {
				t.Fatalf("make reconciliation load fail: %v", err)
			}
		},
	}

	_, _, err := RebindApplicationContext(t.Context(), root, client, expected, replacement, 42)
	if err == nil {
		t.Fatal("unknown registry reconciliation was accepted")
	}
	mark, _ := replacement.ID.Mark()
	if !containsMark(window.Marks, mark) {
		t.Fatalf("unknown registry reconciliation destructively removed committed mark: %q", window.Marks)
	}
	if !strings.Contains(err.Error(), "daemon reconciliation") {
		t.Fatalf("reconciliation failure was not actionable: %v", err)
	}

	path := filepath.Join(root, StateDatabaseFilename)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("restore committed registry permissions: %v", err)
	}
	var visible Registry
	if err := RegistryStoreFor(root).LoadInto(&visible); err != nil {
		t.Fatalf("load committed registry: %v", err)
	}
	if len(visible.Contexts) != 1 || !reflect.DeepEqual(visible.Contexts[0], replacement) {
		t.Fatalf("unexpected committed registry: %+v", visible)
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
	if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{expected}}); err != nil {
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

	_, replacement, err := ReapproveApplicationContext(t.Context(), root, expected.ID, revision, launcher)
	if err != nil {
		t.Fatalf("lifecycle-only change invalidated reapproval: %v", err)
	}
	if replacement.Launcher.DesktopEntrySHA256 != strings.Repeat("b", 64) || replacement.App.RestorePolicy != ApplicationRestorePinned {
		t.Fatalf("reapproval did not merge reviewed launcher with current lifecycle: %+v", replacement)
	}
}

func TestReapproveReportsUnknownRegistryReconciliation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	expected := flatpakApplicationContext("org.example.App", "org.example.App")
	expected.ID = testContextID
	if err := RegistryStoreFor(root).Save(Registry{Version: ContextsSchemaVersion, Contexts: []Context{expected}}); err != nil {
		t.Fatal(err)
	}
	revision, err := ApplicationOperationContextRevision(expected)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, StateDatabaseFilename)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}

	_, _, err = ReapproveApplicationContext(t.Context(), root, expected.ID, revision, expected.Launcher)
	if err == nil || !strings.Contains(err.Error(), "cannot reconcile registry state") {
		t.Fatalf("reapproval did not report unknown reconciliation: %v", err)
	}
}

func writeRegistryBypassingLifecycleLock(t *testing.T, root string, registry Registry) {
	t.Helper()
	database, err := openStateDatabase(t.Context(), root, false)
	if err != nil {
		t.Fatalf("open state database for concurrent write: %v", err)
	}
	defer database.Close()
	_, revision, err := loadRegistrySnapshotDatabase(t.Context(), database)
	if errors.Is(err, os.ErrNotExist) {
		revision = 0
	} else if err != nil {
		t.Fatalf("load registry revision for concurrent write: %v", err)
	}
	if err := saveRegistryDatabase(t.Context(), database, registry, revision); err != nil {
		t.Fatalf("write concurrent registry: %v", err)
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
	tree                        *swayipc.TreeNode
	reject                      bool
	unknownAfterApply           bool
	observeFailures             int
	observeFailuresAfterCommand int
	commandCalls                int
	beforeCommand               func()
	honorContext                bool
	cancelAfterCommand          func()
}

func (client *mutationSwayClient) RequestContext(ctx context.Context, messageType swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if client.honorContext && ctx.Err() != nil {
		return swayipc.Message{}, ctx.Err()
	}
	if messageType == swayipc.GetTree {
		if client.observeFailures > 0 {
			client.observeFailures--
			return swayipc.Message{}, errors.New("tree unavailable")
		}
		if client.commandCalls > 0 && client.observeFailuresAfterCommand > 0 {
			client.observeFailuresAfterCommand--
			return swayipc.Message{}, errors.New("tree unavailable")
		}
		data, err := json.Marshal(client.tree)
		return swayipc.Message{Type: swayipc.GetTree, Payload: data}, err
	}
	if messageType != swayipc.RunCommand {
		return swayipc.Message{}, errors.New("unexpected request")
	}
	client.commandCalls++
	if client.beforeCommand != nil {
		client.beforeCommand()
	}
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
	if client.cancelAfterCommand != nil {
		client.cancelAfterCommand()
		client.cancelAfterCommand = nil
		return swayipc.Message{}, context.Canceled
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
