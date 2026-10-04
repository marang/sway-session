package sessionrequest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func TestServiceAllowsOccupiedWorkspaceAndFocusesRequestedWindow(t *testing.T) {
	service, request, client, runner := testService(t)
	request.Workspace = 98
	client.workspace = request.Workspace
	client.occupied = true
	client.floatingOccupied = true

	response, err := service.Handle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !response.Created || response.Context == nil || response.Context.ID != testContextID || response.Workspace != 98 {
		t.Fatalf("unexpected response: %+v", response)
	}
	if !reflect.DeepEqual(client.commands, []string{"workspace number 98", "[con_id=6] focus"}) ||
		!reflect.DeepEqual(runner.calls, []sessionstate.ContextID{testContextID}) {
		t.Fatalf("wrong start/focus effects: commands=%v restores=%v", client.commands, runner.calls)
	}
	if !client.occupied || !client.floatingOccupied {
		t.Fatal("unrelated windows were removed")
	}
}

type sharedWorkspaceSway struct {
	root     *swayipc.TreeNode
	commands []string
}

func (client *sharedWorkspaceSway) Close() {}

func (client *sharedWorkspaceSway) RequestContext(_ context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if kind == swayipc.GetTree {
		encoded, err := json.Marshal(client.root)
		return swayipc.Message{Type: kind, Payload: encoded}, err
	}
	if kind != swayipc.RunCommand {
		return swayipc.Message{}, errors.New("unexpected Sway request")
	}
	command := string(payload)
	client.commands = append(client.commands, command)
	var container int64
	if command == "workspace number 98" {
		return swayipc.Message{Type: kind, Payload: []byte(`[{"success":true}]`)}, nil
	}
	if _, err := fmt.Sscanf(command, "[con_id=%d] focus", &container); err != nil {
		return swayipc.Message{}, fmt.Errorf("broker affected other windows: %s", command)
	}
	visitServiceTree(client.root, func(node *swayipc.TreeNode) { node.Focused = node.ID == container })
	return swayipc.Message{Type: kind, Payload: []byte(`[{"success":true}]`)}, nil
}

type sharedWorkspaceRestore struct {
	client *sharedWorkspaceSway
	starts map[sessionstate.ContextID]int
}

type requestRestoreFunc func(context.Context, sessionstate.ContextID) error

func (restore requestRestoreFunc) Restore(ctx context.Context, id sessionstate.ContextID) error {
	return restore(ctx, id)
}

func (runner *sharedWorkspaceRestore) Restore(_ context.Context, id sessionstate.ContextID) error {
	runner.starts[id]++
	appID, err := id.AppID()
	if err != nil {
		return err
	}
	workspace := runner.client.root.Nodes[0].Nodes[1]
	workspace.Nodes = append(workspace.Nodes, &swayipc.TreeNode{
		ID: int64(100 + len(runner.starts)), Type: "con", AppID: &appID,
	})
	return nil
}

func sharedWorkspaceService(t *testing.T) (*Service, *sharedWorkspaceSway, *sharedWorkspaceRestore, []Request) {
	t.Helper()
	client := &sharedWorkspaceSway{root: serviceTree(98, "", true, true)}
	runner := &sharedWorkspaceRestore{client: client, starts: make(map[sessionstate.ContextID]int)}
	service := &Service{
		StateRoot: filepath.Join(t.TempDir(), "state"), NewContextID: sessionstate.NewContextID,
		NewSway: func() SwayRequester { return client }, Restore: runner,
	}
	requests := make([]Request, 3)
	for index := range requests {
		requests[index] = Request{
			Version: ProtocolVersion, Session: fmt.Sprintf("shared-%d", index),
			Label: fmt.Sprintf("Shared %d", index), Cwd: t.TempDir(), Workspace: 98,
		}
	}
	return service, client, runner, requests
}

func TestServiceThreeIndependentContextsShareOneWorkspace(t *testing.T) {
	service, client, runner, requests := sharedWorkspaceService(t)
	ids := make([]sessionstate.ContextID, 3)
	for index, request := range requests {
		response, err := service.Handle(t.Context(), request)
		if err != nil || !response.Created || response.Context == nil {
			t.Fatalf("start %d: response=%+v err=%v", index, response, err)
		}
		ids[index] = response.Context.ID
	}
	for _, index := range []int{2, 0, 1} {
		// The neighbor holds focus before each reopen. Selecting a workspace
		// alone cannot satisfy this user-visible context selection.
		visitServiceTree(client.root, func(node *swayipc.TreeNode) { node.Focused = node.ID == 5 })
		response, err := service.Handle(t.Context(), requests[index])
		if err != nil || response.Created || response.Context == nil || response.Context.ID != ids[index] {
			t.Fatalf("reopen %d: response=%+v err=%v", index, response, err)
		}
		var focusedID sessionstate.ContextID
		visitServiceTree(client.root, func(node *swayipc.TreeNode) {
			if node.Focused && node.AppID != nil {
				focusedID, _ = sessionstate.ParseAppID(*node.AppID)
			}
		})
		if focusedID != ids[index] {
			t.Fatalf("reopen focused %s instead of %s", focusedID, ids[index])
		}
	}
	registry, err := loadRegistry(service.StateRoot)
	if err != nil || len(registry.Contexts) != 3 || len(runner.starts) != 3 {
		t.Fatalf("registry/launch count: contexts=%d starts=%v err=%v", len(registry.Contexts), runner.starts, err)
	}
	for _, id := range ids {
		if runner.starts[id] != 1 {
			t.Fatalf("context %s started %d times", id, runner.starts[id])
		}
	}
	workspace := client.root.Nodes[0].Nodes[1]
	if len(workspace.Nodes) != 4 || len(workspace.FloatingNodes) != 1 || workspace.Nodes[0].ID != 5 || workspace.FloatingNodes[0].ID != 7 {
		t.Fatalf("unrelated occupants or window membership changed: %+v", workspace)
	}
}

func TestServicePartialStartDoesNotBlockOtherContextsAndExactRetryReusesIdentity(t *testing.T) {
	service, _, runner, requests := sharedWorkspaceService(t)
	if _, err := service.Handle(t.Context(), requests[0]); err != nil {
		t.Fatal(err)
	}
	failedID := sessionstate.ContextID("")
	service.Restore = requestRestoreFunc(func(_ context.Context, id sessionstate.ContextID) error {
		failedID = id
		return errors.New("temporary launch failure")
	})
	if _, err := service.Handle(t.Context(), requests[1]); err == nil {
		t.Fatal("partial start failure was hidden")
	}
	service.Restore = runner
	if _, err := service.Handle(t.Context(), requests[2]); err != nil {
		t.Fatalf("partial neighboring start blocked independent context: %v", err)
	}
	response, err := service.Handle(t.Context(), requests[1])
	if err != nil || response.Created || response.Context == nil || response.Context.ID != failedID {
		t.Fatalf("exact retry lost its identity: response=%+v err=%v", response, err)
	}
	if len(runner.starts) != 3 || runner.starts[failedID] != 1 {
		t.Fatalf("retry duplicated or omitted a window: %v", runner.starts)
	}
	registry, err := loadRegistry(service.StateRoot)
	if err != nil || len(registry.Contexts) != 3 {
		t.Fatalf("partial start leaked registrations: registry=%+v err=%v", registry, err)
	}
}

func TestServiceConcurrentRequestsShareWorkspaceWithoutDuplicateContextsOrWindows(t *testing.T) {
	service, client, runner, requests := sharedWorkspaceService(t)
	type result struct {
		index    int
		response Response
		err      error
	}
	results := make(chan result, 9)
	start := make(chan struct{})
	for repetition := 0; repetition < 3; repetition++ {
		for index, request := range requests {
			go func() {
				<-start
				response, err := service.Handle(t.Context(), request)
				results <- result{index: index, response: response, err: err}
			}()
		}
	}
	close(start)
	ids := make(map[int]sessionstate.ContextID)
	created := 0
	for iteration := 0; iteration < 9; iteration++ {
		result := <-results
		if result.err != nil || result.response.Context == nil {
			t.Fatalf("concurrent request: response=%+v err=%v", result.response, result.err)
		}
		id := result.response.Context.ID
		if previous, exists := ids[result.index]; exists && previous != id {
			t.Fatalf("same request acquired two identities: %s and %s", previous, id)
		}
		ids[result.index] = id
		if result.response.Created {
			created++
		}
	}
	registry, err := loadRegistry(service.StateRoot)
	if err != nil || len(registry.Contexts) != 3 || len(ids) != 3 || created != 3 || len(runner.starts) != 3 {
		t.Fatalf("concurrent requests duplicated contexts: registry=%+v ids=%v created=%d starts=%v err=%v", registry, ids, created, runner.starts, err)
	}
	for id, count := range runner.starts {
		if count != 1 {
			t.Fatalf("context %s has %d windows", id, count)
		}
	}
	if len(client.root.Nodes[0].Nodes[1].Nodes) != 4 {
		t.Fatal("concurrent requests lost a neighbor or duplicated a window")
	}
}

func TestServiceDoesNotMoveContextFromAnotherWorkspace(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("already-mapped=%t", mapped), func(t *testing.T) {
			service, request, client, runner := testService(t)
			request.Workspace = 98
			if mapped {
				contextValue := registeredContext(request)
				saveServiceTestRegistry(t, service.StateRoot, sessionstate.Registry{
					Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue},
				})
				client.workspace, client.mapped = 99, contextValue.ID
			} else {
				runner.afterRestore = func() error { client.workspace = 99; return nil }
			}
			response, err := service.Handle(t.Context(), request)
			if err == nil || response.Context != nil {
				t.Fatalf("wrong placement accepted: response=%+v err=%v", response, err)
			}
			for _, command := range client.commands {
				if command != "workspace number 98" {
					t.Fatalf("conflicting placement caused window effects: %s", command)
				}
			}
			registry, err := loadRegistry(service.StateRoot)
			if err != nil || len(registry.Contexts) != 1 {
				t.Fatalf("retry identity was lost: registry=%+v err=%v", registry, err)
			}
		})
	}
}

func TestServiceRejectsDuplicateRequestedWindowOnOccupiedWorkspace(t *testing.T) {
	service, request, client, runner := testService(t)
	contextValue := registeredContext(request)
	saveServiceTestRegistry(t, service.StateRoot, sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue},
	})
	client.workspace, client.mapped, client.occupied = request.Workspace, contextValue.ID, true
	client.transformTree = func(root *swayipc.TreeNode) {
		appID, _ := contextValue.ID.AppID()
		root.Nodes[0].Nodes[1].FloatingNodes = append(root.Nodes[0].Nodes[1].FloatingNodes,
			&swayipc.TreeNode{ID: 8, Type: "con", AppID: &appID})
	}
	if _, err := service.Handle(t.Context(), request); err == nil {
		t.Fatal("duplicate identity was accepted")
	}
	if len(client.commands) != 0 || len(runner.calls) != 0 {
		t.Fatalf("ambiguous windows caused effects: commands=%v restores=%v", client.commands, runner.calls)
	}
}

func TestServiceReusesNestedAndFloatingContextWithoutChangingLayout(t *testing.T) {
	for _, floating := range []bool{false, true} {
		t.Run(fmt.Sprintf("floating=%t", floating), func(t *testing.T) {
			service, request, client, runner := testService(t)
			contextValue := registeredContext(request)
			saveServiceTestRegistry(t, service.StateRoot, sessionstate.Registry{
				Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue},
			})
			client.workspace, client.mapped, client.occupied = request.Workspace, contextValue.ID, true
			client.transformTree = func(root *swayipc.TreeNode) {
				workspace := root.Nodes[0].Nodes[1]
				window := workspace.Nodes[1]
				workspace.Nodes = workspace.Nodes[:1]
				group := &swayipc.TreeNode{ID: 8, Type: "con", Layout: "tabbed", Nodes: []*swayipc.TreeNode{window}}
				if floating {
					workspace.FloatingNodes = append(workspace.FloatingNodes, group)
				} else {
					workspace.Nodes = append(workspace.Nodes, group)
				}
			}
			response, err := service.Handle(t.Context(), request)
			if err != nil || response.Created || response.Context == nil || response.Context.ID != contextValue.ID || client.focusedContainer != 6 {
				t.Fatalf("nested/floating context not reused: response=%+v err=%v", response, err)
			}
			if len(runner.calls) != 0 || !reflect.DeepEqual(client.commands, []string{"[con_id=6] focus"}) {
				t.Fatalf("existing layout changed: commands=%v starts=%v", client.commands, runner.calls)
			}
		})
	}
}

func TestServiceRejectionReasonsAndRecoveryIdentityReachTheClientSafely(t *testing.T) {
	for _, code := range []string{DiagnosticWorkspaceAmbiguous, DiagnosticWorkspaceConflict, DiagnosticWindowAmbiguous,
		DiagnosticMappingPending, DiagnosticContextChanged, DiagnosticInitializationFailed, DiagnosticRestoreFailed} {
		t.Run(code, func(t *testing.T) {
			service, request, client, runner := testService(t)
			request.Workspace = 98
			client.workspace, client.occupied = 98, true
			wantID := testContextID
			switch code {
			case DiagnosticWorkspaceAmbiguous:
				client.transformTree = duplicateRequestedWorkspace(98)
				wantID = ""
			case DiagnosticWorkspaceConflict:
				runner.afterRestore = func() error { client.workspace = 99; return nil }
			case DiagnosticWindowAmbiguous:
				runner.afterRestore = func() error {
					client.transformTree = func(root *swayipc.TreeNode) {
						appID, _ := testContextID.AppID()
						root.Nodes[0].Nodes[1].Nodes = append(root.Nodes[0].Nodes[1].Nodes,
							&swayipc.TreeNode{ID: 66, Type: "con", AppID: &appID})
					}
					return nil
				}
			case DiagnosticMappingPending:
				runner.mapped = nil
			case DiagnosticContextChanged:
				runner.afterRestore = func() error {
					_, err := sessionstate.UpdateRegistry(service.StateRoot, func(registry *sessionstate.Registry) error {
						_, err := sessionstate.SetContextState(registry, string(testContextID), sessionstate.ContextArchived)
						return err
					})
					return err
				}
			case DiagnosticInitializationFailed:
				service.Initializer = &fakeSessionInitializer{err: fmt.Errorf("private initialization in %s", request.Cwd)}
			case DiagnosticRestoreFailed:
				runner.err = fmt.Errorf("private launch from %s", request.Cwd)
			}
			socket := diagnosticTestSocket(t)
			server, err := StartServer(socket, service.Handle, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			_, err = Send(t.Context(), socket, request)
			var diagnostic *RequestDiagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Code != code || diagnostic.ContextID != wantID || diagnostic.Workspace != 98 {
				t.Fatalf("actionable service rejection was lost: %v", err)
			}
			if diagnostic.Cause != nil || strings.Contains(err.Error(), request.Cwd) || strings.Contains(err.Error(), request.Session) {
				t.Fatalf("service rejection exposed private context metadata: %v", err)
			}
			registry, err := loadRegistry(service.StateRoot)
			if err != nil {
				t.Fatal(err)
			}
			if wantID == "" && len(registry.Contexts) != 0 || wantID != "" && (len(registry.Contexts) != 1 || registry.Contexts[0].ID != wantID) {
				t.Fatalf("rejection lost recovery identity: %+v", registry)
			}
		})
	}
}

func TestServiceWorkspaceAmbiguityRetainsExistingRecoveryIdentityAcrossSocket(t *testing.T) {
	for _, phase := range []string{"existing-unmapped", "before-restore", "before-focus", "after-restore", "during-focus"} {
		t.Run(phase, func(t *testing.T) {
			service, request, client, runner := testService(t)
			request.Workspace = 98
			client.workspace, client.occupied = 98, true
			if phase == "existing-unmapped" || phase == "before-restore" || phase == "before-focus" {
				saveServiceTestRegistry(t, service.StateRoot, sessionstate.Registry{
					Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{registeredContext(request)},
				})
			}
			switch phase {
			case "existing-unmapped":
				client.transformTree = duplicateRequestedWorkspace(98)
			case "before-restore", "before-focus":
				if phase == "before-focus" {
					client.mapped = testContextID
				}
				client.onTree = func(number int) error {
					if number == 2 {
						client.transformTree = duplicateRequestedWorkspace(98)
					}
					return nil
				}
			case "after-restore":
				runner.afterRestore = func() error { client.transformTree = duplicateRequestedWorkspace(98); return nil }
			case "during-focus":
				client.onCommand = func(command string) error {
					if strings.HasPrefix(command, "[con_id=") {
						client.transformTree = duplicateRequestedWorkspace(98)
					}
					return nil
				}
			}
			socket := diagnosticTestSocket(t)
			server, err := StartServer(socket, service.Handle, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			_, err = Send(t.Context(), socket, request)
			var diagnostic *RequestDiagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Code != DiagnosticWorkspaceAmbiguous || diagnostic.ContextID != testContextID {
				t.Fatalf("surviving registration lost its recovery identity: %v", err)
			}
			registry, err := loadRegistry(service.StateRoot)
			if err != nil || len(registry.Contexts) != 1 || registry.Contexts[0].ID != testContextID {
				t.Fatalf("ambiguous placement removed the existing registration: registry=%+v err=%v", registry, err)
			}
		})
	}
}

func TestServiceRejectsWindowReplacementDuringFocus(t *testing.T) {
	service, request, client, runner := testService(t)
	contextValue := registeredContext(request)
	saveServiceTestRegistry(t, service.StateRoot, sessionstate.Registry{
		Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue},
	})
	client.workspace = request.Workspace
	client.mapped = contextValue.ID
	client.occupied = true
	client.onCommand = func(command string) error {
		if strings.HasPrefix(command, "[con_id=") {
			client.transformTree = func(root *swayipc.TreeNode) {
				visitServiceTree(root, func(node *swayipc.TreeNode) {
					if node.ID == 6 {
						node.ID = 66
					}
				})
			}
		}
		return nil
	}

	response, err := service.Handle(context.Background(), request)
	if err == nil || response.Context != nil {
		t.Fatalf("replaced window was accepted: response=%+v err=%v", response, err)
	}
	if len(runner.calls) != 0 || !reflect.DeepEqual(client.commands, []string{"[con_id=6] focus"}) {
		t.Fatalf("replacement retried or changed other windows: commands=%v restores=%v", client.commands, runner.calls)
	}
}

func TestServiceHonorsPlacementAndFocusChangesDuringItsFocusCommand(t *testing.T) {
	for _, moved := range []bool{false, true} {
		t.Run(fmt.Sprintf("moved=%t", moved), func(t *testing.T) {
			service, request, client, runner := testService(t)
			contextValue := registeredContext(request)
			saveServiceTestRegistry(t, service.StateRoot, sessionstate.Registry{
				Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{contextValue},
			})
			client.workspace, client.mapped, client.occupied = request.Workspace, contextValue.ID, true
			client.onCommand = func(_ string) error {
				if moved {
					client.workspace = 99
				} else {
					client.focusedContainer = 5
				}
				return nil
			}
			response, err := service.Handle(t.Context(), request)
			if err == nil || response.Context != nil {
				t.Fatalf("concurrent intent was overridden: response=%+v err=%v", response, err)
			}
			if len(runner.calls) != 0 || !reflect.DeepEqual(client.commands, []string{"[con_id=6] focus"}) {
				t.Fatalf("broker retried against changed focus/placement: commands=%v restores=%v", client.commands, runner.calls)
			}
		})
	}
}
