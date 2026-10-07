package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func newScratchpadShowScenario(t *testing.T) (*sessionRuntime, *recordingRequester, *Node, sessionstate.PlacementAction) {
	t.Helper()
	appID := "org.example.App"
	item := sessionstate.Context{
		ID: testManagedContextID, State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: appID, FlatpakInstallation: sessionstate.FlatpakUser},
		App: &sessionstate.Application{
			Identity:    sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: appID, SandboxAppID: appID},
			DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow,
		},
	}
	otherAppID := "org.example.Other"
	root := daemonTree("98", &Node{ID: 41, Type: "con", Focused: true, AppID: &otherAppID})
	root.Nodes[0].Nodes[0].Focus = []int64{41}
	root.Nodes[0].Nodes = append(root.Nodes[0].Nodes, &Node{
		ID: 4, Type: "workspace", Name: "99", Focus: []int64{43},
		Nodes: []*Node{{ID: 43, Type: "con", AppID: &otherAppID}},
	})
	root.Nodes = append(root.Nodes, &Node{ID: 2147483647, Type: "output", Name: "__i3", Nodes: []*Node{{
		ID: 2147483646, Type: "workspace", Name: "__i3_scratch", FloatingNodes: []*Node{{
			ID: 42, Type: "con", AppID: &appID, SandboxAppID: &appID, ScratchpadState: "fresh",
		}},
	}}})
	requester := &recordingRequester{}
	runtime := &sessionRuntime{
		client: requester, registry: sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{item}},
		eventStreamReady: true, eventStreamEpoch: 1,
		desired: sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}, Scratchpad: []sessionstate.ScratchpadPlacement{{
			ContextID: testManagedContextID, Visible: true, Workspace: "99",
		}}},
		restoreProgress: &sessionstate.RestoreProgress{Workspace: "99", Phase: sessionstate.RestoreBuild},
	}
	action := sessionstate.PlacementAction{Kind: sessionstate.PlacementShowScratchpad, ContextID: testManagedContextID, ContainerID: 42, Workspace: "99"}
	return runtime, requester, root, action
}

func scratchpadFocus(container int64) restoreFocusEvent {
	return restoreFocusEvent{kind: swayipc.EventWindow, container: container}
}

func scratchpadWorkspaceFocus(old, next int64) restoreFocusEvent {
	return restoreFocusEvent{kind: swayipc.EventWorkspace, old: old, next: next}
}

func TestRestoredScratchpadCommandPredictsFocusAndPreservesOrigin(t *testing.T) {
	for _, name := range []string{"different-workspace", "same-workspace", "empty-destination", "missing-destination", "empty-original", "same-empty-original", "original-workspace-focus", "original-split-focus"} {
		t.Run(name, func(t *testing.T) {
			_, _, root, action := newScratchpadShowScenario(t)
			original := root.Nodes[0].Nodes[0]
			destination := root.Nodes[0].Nodes[1]
			wantCommand := `[con_id=43] focus; [con_id=42] scratchpad show; [con_id=41] focus`
			wantBefore := []restoreFocusEvent{scratchpadWorkspaceFocus(3, 4), scratchpadFocus(43), scratchpadFocus(42)}
			wantBefore[0].nextName = "99"
			wantAfter := []restoreFocusEvent{scratchpadWorkspaceFocus(4, 3), scratchpadFocus(41)}
			wantAfter[0].oldName, wantAfter[0].nextName = "99", "98"
			switch name {
			case "same-workspace":
				action.Workspace = "98"
				wantCommand = `[con_id=42] scratchpad show; [con_id=41] focus`
				wantBefore, wantAfter = []restoreFocusEvent{scratchpadFocus(42)}, []restoreFocusEvent{scratchpadFocus(41)}
			case "empty-destination":
				destination.Nodes, destination.Focus = nil, nil
				wantCommand = `workspace --no-auto-back-and-forth "99"; [con_id=42] scratchpad show; [con_id=41] focus`
				wantBefore = append(wantBefore[:1], scratchpadFocus(42))
			case "missing-destination":
				root.Nodes[0].Nodes = root.Nodes[0].Nodes[:1]
				wantCommand = `workspace --no-auto-back-and-forth "99"; [con_id=42] scratchpad show; [con_id=41] focus`
				wantBefore = append(wantBefore[:1], scratchpadFocus(42))
				wantBefore[0].next, wantAfter[0].old = 0, 0
			case "empty-original":
				original.Nodes, original.Focus, original.Focused = nil, nil, true
				wantCommand = `[con_id=43] focus; [con_id=42] scratchpad show; workspace --no-auto-back-and-forth "98"`
				wantAfter = wantAfter[:1]
				wantAfter[0].next = 0
			case "same-empty-original":
				original.Nodes, original.Focus, original.Focused = nil, nil, true
				action.Workspace = "98"
				wantCommand = `[con_id=42] scratchpad show; [con_id=42] focus parent`
				wantBefore, wantAfter = []restoreFocusEvent{scratchpadFocus(42)}, nil
			case "original-workspace-focus":
				original.Nodes[0].Focused, original.Focused = false, true
				wantCommand = `[con_id=43] focus; [con_id=42] scratchpad show; [con_id=41] focus parent`
				wantAfter = wantAfter[:1]
			case "original-split-focus":
				original.Nodes[0].Nodes = []*Node{{ID: 44, Type: "con", AppID: original.Nodes[0].AppID}}
				original.Nodes[0].AppID = nil
				wantAfter = wantAfter[:1]
			}
			command, before, after, err := restoredScratchpadCommand(root, action)
			if err != nil {
				t.Fatal(err)
			}
			wantCommand = `[con_id=42] move scratchpad; ` + wantCommand
			if command != wantCommand || !reflect.DeepEqual(before, wantBefore) || !reflect.DeepEqual(after, wantAfter) {
				t.Fatalf("command=%q, before=%+v, after=%+v; want %q, %+v, %+v", command, before, after, wantCommand, wantBefore, wantAfter)
			}
		})
	}
}

func TestSessionRuntimeScratchpadShowAttributesOrderedEvents(t *testing.T) {
	for _, name := range []string{"different-workspace", "same-workspace", "missing-destination", "empty-original", "same-empty-original"} {
		t.Run(name, func(t *testing.T) {
			runtime, requester, root, action := newScratchpadShowScenario(t)
			switch name {
			case "same-workspace":
				action.Workspace = "98"
			case "missing-destination":
				root.Nodes[0].Nodes = root.Nodes[0].Nodes[:1]
			case "empty-original", "same-empty-original":
				ws := root.Nodes[0].Nodes[0]
				ws.Nodes, ws.Focus, ws.Focused = nil, nil, true
				if name == "same-empty-original" {
					action.Workspace = "98"
				}
			}
			runtime.desired.Scratchpad[0].Workspace = action.Workspace
			_, before, after, err := restoredScratchpadCommand(root, action)
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.showRestoredScratchpad(root, action); err != nil {
				t.Fatal(err)
			}
			if len(requester.commands) != 1 || len(requester.barriers) != 1 {
				t.Fatalf("effects were not one command plus barrier: %+v", requester)
			}
			now := time.Now().Add(time.Hour)
			for _, prediction := range before {
				runtime.HandleEvent(observedScratchpadFocus(prediction), now)
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "move", Container: &Node{ID: action.ContainerID}}, now)
			for _, prediction := range after {
				runtime.HandleEvent(observedScratchpadFocus(prediction), now)
			}
			if runtime.restoreCancelled || runtime.restoreProgress == nil || len(runtime.expectedFocus) != 0 || len(runtime.expectedMoves) != 0 {
				t.Fatalf("own events cancelled restore or retained expectations: %+v", runtime.expectedFocus)
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventTick, Payload: requester.barriers[0]}, now)
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: "focus", Container: &Node{ID: 42}}, now)
			if runtime.restoreCancelled {
				t.Fatal("a later window focus cancelled scratchpad restore")
			}
			runtime.HandleEvent(swayipc.Event{Type: swayipc.EventBinding}, now)
			if !runtime.restoreCancelled {
				t.Fatal("a later user binding was swallowed")
			}
		})
	}
}

func observedScratchpadFocus(prediction restoreFocusEvent) swayipc.Event {
	event := swayipc.Event{Type: prediction.kind, Change: "focus", StreamEpoch: 1}
	if prediction.kind == swayipc.EventWindow {
		event.Container = &Node{ID: prediction.container}
	} else {
		event.Old = &Node{ID: prediction.old, Type: "workspace", Name: prediction.oldName}
		event.Current = &Node{ID: prediction.next, Type: "workspace", Name: prediction.nextName}
		if event.Old.ID == 0 {
			event.Old.ID = 104
		}
		if event.Current.ID == 0 {
			if event.Current.Name == "99" {
				event.Current.ID = 104
			} else {
				event.Current.ID = 103
			}
		}
	}
	return event
}

func TestSessionRuntimeScratchpadShowRejectsUnsafeObservations(t *testing.T) {
	for _, name := range []string{"missing-target", "already-visible", "not-scratchpad", "marked", "duplicate-id", "duplicate-identity", "wrong-identity", "terminal-context", "split-target", "tiling-target", "no-focus", "multiple-focus", "destination-duplicate", "destination-case-alias", "unknown-destination-focus", "global-fullscreen", "destination-fullscreen", "saved-hidden", "saved-workspace-changed", "invalid-workspace"} {
		t.Run(name, func(t *testing.T) {
			runtime, requester, root, action := newScratchpadShowScenario(t)
			hidden := root.Nodes[1].Nodes[0]
			target := hidden.FloatingNodes[0]
			switch name {
			case "missing-target":
				hidden.FloatingNodes = nil
			case "already-visible":
				hidden.FloatingNodes = nil
				root.Nodes[0].Nodes[1].FloatingNodes = []*Node{target}
			case "not-scratchpad":
				target.ScratchpadState = "none"
			case "marked":
				mark, _ := action.ContextID.Mark()
				target.Marks = []string{mark}
			case "duplicate-id":
				root.Nodes[0].Nodes[1].Nodes[0].ID = target.ID
			case "duplicate-identity":
				root.Nodes[0].Nodes[1].Nodes[0].AppID = target.AppID
				root.Nodes[0].Nodes[1].Nodes[0].SandboxAppID = target.SandboxAppID
			case "wrong-identity":
				target.AppID = nil
			case "terminal-context":
				runtime.registry = sessionRegistry(action.ContextID)
			case "split-target":
				target.Nodes = []*Node{{ID: 44, Type: "con"}}
			case "tiling-target":
				hidden.Nodes, hidden.FloatingNodes = hidden.FloatingNodes, nil
			case "no-focus":
				root.Nodes[0].Nodes[0].Nodes[0].Focused = false
			case "multiple-focus":
				root.Nodes[0].Nodes[1].Nodes[0].Focused = true
			case "destination-duplicate":
				root.Nodes[0].Nodes = append(root.Nodes[0].Nodes, &Node{ID: 5, Type: "workspace", Name: "99"})
			case "destination-case-alias":
				action.Workspace, runtime.desired.Scratchpad[0].Workspace = "Saved", "Saved"
				root.Nodes[0].Nodes[1].Name = "saved"
			case "unknown-destination-focus":
				root.Nodes[0].Nodes[1].Focus = nil
			case "global-fullscreen":
				root.Nodes[0].Nodes[0].Nodes[0].FullscreenMode = 2
			case "destination-fullscreen":
				root.Nodes[0].Nodes[1].Nodes[0].FullscreenMode = 1
			case "saved-hidden":
				runtime.desired.Scratchpad[0].Visible = false
			case "saved-workspace-changed":
				runtime.desired.Scratchpad[0].Workspace = "100"
			case "invalid-workspace":
				action.Workspace, runtime.desired.Scratchpad[0].Workspace = "next", "next"
			}
			if err := runtime.showRestoredScratchpad(root, action); err == nil {
				t.Fatal("unsafe scratchpad show was accepted")
			}
			if len(requester.commands)+len(requester.barriers)+len(runtime.expectedFocus)+len(runtime.expectedMoves) != 0 {
				t.Fatal("rejected observation issued effects or granted focus attribution")
			}
		})
	}
}

type scratchpadResponseRequester struct {
	recordingRequester
	response []byte
}

func (r *scratchpadResponseRequester) RequestContext(ctx context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	message, err := r.recordingRequester.RequestContext(ctx, kind, payload)
	if kind == swayipc.RunCommand && err == nil {
		message.Payload = r.response
	}
	return message, err
}

func TestSessionRuntimeScratchpadShowUncertainResponseNeedsObservation(t *testing.T) {
	for _, name := range []string{"unknown-outcome", "invalid-reply", "partial-rejection", "barrier-failure"} {
		t.Run(name, func(t *testing.T) {
			runtime, requester, root, action := newScratchpadShowScenario(t)
			switch name {
			case "unknown-outcome":
				requester.failAt, requester.failure = 1, &swayipc.CommandOutcomeUnknownError{Cause: errors.New("reply lost")}
			case "invalid-reply":
				runtime.client = &scratchpadResponseRequester{response: []byte(`{}`)}
			case "partial-rejection":
				runtime.client = &scratchpadResponseRequester{response: []byte(`[{"success":true},{"success":true},{"success":true},{"success":false,"error":"original window closed"}]`)}
			case "barrier-failure":
				runtime.client = &failedFocusBarrierRequester{}
			}
			err := runtime.showRestoredScratchpad(root, action)
			var unknown *swayipc.CommandOutcomeUnknownError
			var invalid *swayipc.CommandResponseInvalidError
			if !errors.As(err, &unknown) && !errors.As(err, &invalid) {
				t.Fatalf("uncertain result did not require observation: %v", err)
			}
			if !runtime.restoreCancelled || len(runtime.expectedFocus)+len(runtime.expectedMoves) != 0 {
				t.Fatal("uncertain result retained active restore or event allowances")
			}
			// A fresh visible observation must reject even a direct retry.
			hidden := root.Nodes[1].Nodes[0]
			root.Nodes[0].Nodes[1].FloatingNodes, hidden.FloatingNodes = hidden.FloatingNodes, nil
			if err := runtime.showRestoredScratchpad(root, action); err == nil {
				t.Fatal("uncertain show was blindly toggled on a visible observation")
			}
		})
	}
}

func TestSessionRuntimeScratchpadShowUserActivityWins(t *testing.T) {
	for _, name := range []string{"binding", "unmatched-workspace", "reordered-workspace", "return-before-move", "wrong-workspace-name", "expired", "epoch-change"} {
		t.Run(name, func(t *testing.T) {
			runtime, requester, root, action := newScratchpadShowScenario(t)
			if name == "wrong-workspace-name" {
				root.Nodes[0].Nodes = root.Nodes[0].Nodes[:1]
			}
			_, before, after, err := restoredScratchpadCommand(root, action)
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.showRestoredScratchpad(root, action); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			event := observedScratchpadFocus(before[0])
			switch name {
			case "binding":
				event = swayipc.Event{Type: swayipc.EventBinding}
			case "unmatched-workspace":
				event = swayipc.Event{Type: swayipc.EventWorkspace, Change: "focus", Old: &Node{ID: 3}, Current: &Node{ID: 5}}
			case "reordered-workspace":
				event = observedScratchpadFocus(after[0])
			case "return-before-move":
				for _, prediction := range before {
					runtime.HandleEvent(observedScratchpadFocus(prediction), now)
				}
				event = observedScratchpadFocus(after[0])
			case "wrong-workspace-name":
				event.Current.Name = "100"
			case "expired":
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventTick, Payload: requester.barriers[0]}, now)
			case "epoch-change":
				runtime.eventStreamState = &mutableEventStreamGuard{epoch: 2, connected: true}
			}
			runtime.HandleEvent(event, now)
			if !runtime.restoreCancelled || len(runtime.expectedFocus) != 0 {
				t.Fatal("user activity retained scratchpad restore or its focus allowances")
			}
		})
	}
}

func TestSessionRuntimeScratchpadOwnershipUsesSavedMembership(t *testing.T) {
	runtime, _, root, action := newScratchpadShowScenario(t)
	owned, err := runtime.observeRestoreWindows(root, runtime.registry)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := owned[action.ContainerID]; !exists || len(owned) != 1 {
		t.Fatalf("scratchpad-only saved context is absent from owned observation: %v", owned)
	}
}
