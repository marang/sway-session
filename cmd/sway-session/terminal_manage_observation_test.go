package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func TestCommandTerminalManageLoadObservesWindowsWithoutChangingRestoreState(t *testing.T) {
	deps, contexts := terminalManageObservationFixture(t)
	client := &terminalManageObservationRequester{tree: treeWithContexts(contexts[0].ID, contexts[1].ID)}
	deps.newSwayClient = func(socket string) swayRequester {
		if socket != "/run/user/1000/sway-ipc.sock" {
			t.Fatalf("socket = %q", socket)
		}
		return client
	}

	snapshot, err := (commandTerminalManageOperations{deps: deps}).Load(t.Context(), "/run/user/1000/sway-ipc.sock")
	if err != nil {
		t.Fatalf("load terminal manager: %v", err)
	}
	if snapshot.windowError != nil {
		t.Fatalf("window observation failed: %v", snapshot.windowError)
	}
	terminalManageRequireInventory(t, snapshot, contexts)
	terminalManageRequirePresence(t, snapshot, contexts[0].ID, terminalWindowOpen)
	terminalManageRequirePresence(t, snapshot, contexts[1].ID, terminalWindowOpen)
	terminalManageRequirePresence(t, snapshot, contexts[2].ID, terminalWindowClosed)
	if contexts[1].State != sessionstate.ContextArchived {
		t.Fatal("fixture must prove an archived context may still have an open window")
	}
	terminalManageRequireObservationOnly(t, client)
}

func TestCommandTerminalManageLoadRetainsInventoryWhenWindowObservationIsUnavailable(t *testing.T) {
	for _, test := range []struct {
		name   string
		client func() swayRequester
	}{
		{
			name:   "missing client",
			client: func() swayRequester { return nil },
		},
		{
			name: "tree request failure",
			client: func() swayRequester {
				return &terminalManageObservationRequester{err: errors.New("Sway socket is unavailable")}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps, contexts := terminalManageObservationFixture(t)
			var client swayRequester
			deps.newSwayClient = func(string) swayRequester {
				client = test.client()
				return client
			}

			snapshot, err := (commandTerminalManageOperations{deps: deps}).Load(t.Context(), "")
			if err != nil {
				t.Fatalf("load terminal manager: %v", err)
			}
			if snapshot.windowError == nil {
				t.Fatal("missing observation failure")
			}
			terminalManageRequireInventory(t, snapshot, contexts)
			for _, context := range contexts {
				terminalManageRequirePresence(t, snapshot, context.ID, terminalWindowUnknown)
			}
			if requester, ok := client.(*terminalManageObservationRequester); ok {
				terminalManageRequireObservationOnly(t, requester)
			}
		})
	}
}

func TestCommandTerminalManageLoadTreatsInvalidTreeResponsesAsUnknown(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{name: "null", payload: []byte("null")},
		{name: "empty object", payload: []byte("{}")},
		{name: "error object", payload: []byte(`{"error":"tree unavailable"}`)},
		{name: "malformed JSON", payload: []byte("{")},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps, contexts := terminalManageObservationFixture(t)
			client := &terminalManageObservationRequester{payload: test.payload}
			deps.newSwayClient = func(string) swayRequester { return client }

			snapshot, err := (commandTerminalManageOperations{deps: deps}).Load(t.Context(), "")
			if err != nil {
				t.Fatalf("load terminal manager: %v", err)
			}
			if snapshot.windowError == nil {
				t.Fatal("invalid tree response was accepted")
			}
			terminalManageRequireInventory(t, snapshot, contexts)
			for _, context := range contexts {
				terminalManageRequirePresence(t, snapshot, context.ID, terminalWindowUnknown)
			}
			terminalManageRequireObservationOnly(t, client)
		})
	}
}

func TestCommandTerminalManageLoadIsolatesDuplicateAndConflictingIdentities(t *testing.T) {
	for _, test := range []struct {
		name string
		tree func([]sessionstate.Context) *swayipc.TreeNode
		want [3]terminalWindowPresence
	}{
		{
			name: "duplicate identity",
			tree: func(contexts []sessionstate.Context) *swayipc.TreeNode {
				return treeWithContexts(contexts[0].ID, contexts[0].ID, contexts[1].ID)
			},
			want: [3]terminalWindowPresence{terminalWindowUnknown, terminalWindowOpen, terminalWindowClosed},
		},
		{
			name: "conflicting identities",
			tree: func(contexts []sessionstate.Context) *swayipc.TreeNode {
				firstAppID, err := contexts[0].ID.AppID()
				if err != nil {
					t.Fatal(err)
				}
				thirdMark, err := contexts[2].ID.Mark()
				if err != nil {
					t.Fatal(err)
				}
				root := treeWithContexts(contexts[1].ID)
				workspace := root.Nodes[0].Nodes[0]
				workspace.Nodes = append(workspace.Nodes, &swayipc.TreeNode{
					ID: 99, Type: "con", AppID: &firstAppID, Marks: []string{thirdMark},
				})
				return root
			},
			want: [3]terminalWindowPresence{terminalWindowUnknown, terminalWindowOpen, terminalWindowUnknown},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps, contexts := terminalManageObservationFixture(t)
			client := &terminalManageObservationRequester{tree: test.tree(contexts)}
			deps.newSwayClient = func(string) swayRequester { return client }

			snapshot, err := (commandTerminalManageOperations{deps: deps}).Load(t.Context(), "")
			if err != nil {
				t.Fatalf("load terminal manager: %v", err)
			}
			if snapshot.windowError == nil {
				t.Fatal("ambiguous managed identity was accepted")
			}
			terminalManageRequireInventory(t, snapshot, contexts)
			for index, context := range contexts {
				terminalManageRequirePresence(t, snapshot, context.ID, test.want[index])
			}
			terminalManageRequireObservationOnly(t, client)
		})
	}
}

func terminalManageObservationFixture(t *testing.T) (dependencies, []sessionstate.Context) {
	t.Helper()
	deps := testDependencies(t)
	contexts := []sessionstate.Context{
		terminalInventoryContext("11111111-1111-4111-8111-111111111111", sessionstate.TerminalIdentity{Kind: sessionstate.TerminalIdentityDefault}, sessionstate.ContextActive, nil),
		terminalInventoryContext("22222222-2222-4222-8222-222222222222", sessionstate.TerminalIdentity{Kind: sessionstate.TerminalIdentityProject, Project: "archive"}, sessionstate.ContextArchived, nil),
		terminalInventoryContext("33333333-3333-4333-8333-333333333333", sessionstate.TerminalIdentity{Kind: sessionstate.TerminalIdentityProject, Project: "closed"}, sessionstate.ContextActive, nil),
	}
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	saveTestRegistry(t, root, sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: contexts})
	return deps, contexts
}

func terminalManageRequireInventory(t *testing.T, snapshot terminalManageSnapshot, contexts []sessionstate.Context) {
	t.Helper()
	if got, want := len(snapshot.items), len(contexts); got != want {
		t.Fatalf("terminal inventory count = %d, want %d: %+v", got, want, snapshot.items)
	}
	for _, context := range contexts {
		found := false
		for _, item := range snapshot.items {
			if item.ContextID == context.ID && item.State == context.State {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("terminal inventory omitted context %s in state %q: %+v", context.ID, context.State, snapshot.items)
		}
	}
}

func terminalManageRequirePresence(t *testing.T, snapshot terminalManageSnapshot, id sessionstate.ContextID, want terminalWindowPresence) {
	t.Helper()
	if got, exists := snapshot.windows[id]; !exists || got != want {
		t.Fatalf("window presence for %s = %v (exists=%t), want %v", id, got, exists, want)
	}
}

func terminalManageRequireObservationOnly(t *testing.T, client *terminalManageObservationRequester) {
	t.Helper()
	if !client.closed {
		t.Fatal("window observation client was not closed")
	}
	if len(client.requests) != 1 || client.requests[0] != swayipc.GetTree {
		t.Fatalf("Sway requests = %v, want exactly one GetTree request", client.requests)
	}
}

type terminalManageObservationRequester struct {
	tree     *swayipc.TreeNode
	payload  []byte
	err      error
	requests []swayipc.MessageType
	closed   bool
}

func (client *terminalManageObservationRequester) RequestContext(ctx context.Context, messageType swayipc.MessageType, _ []byte) (swayipc.Message, error) {
	if err := ctx.Err(); err != nil {
		return swayipc.Message{}, err
	}
	client.requests = append(client.requests, messageType)
	if messageType != swayipc.GetTree {
		return swayipc.Message{}, errors.New("unexpected Sway request")
	}
	if client.err != nil {
		return swayipc.Message{}, client.err
	}
	payload := client.payload
	if payload == nil {
		var err error
		payload, err = json.Marshal(client.tree)
		if err != nil {
			return swayipc.Message{}, err
		}
	}
	return swayipc.Message{Type: swayipc.GetTree, Payload: payload}, nil
}

func (client *terminalManageObservationRequester) Close() { client.closed = true }
