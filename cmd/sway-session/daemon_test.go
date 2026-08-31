package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
	"github.com/marang/sway-title-animator/internal/swayipc"
)

type daemonLoopRequester struct {
	trees    []*swayipc.TreeNode
	commands []string
}

func (requester *daemonLoopRequester) Request(messageType swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	switch messageType {
	case swayipc.GetTree:
		if len(requester.trees) == 0 {
			return swayipc.Message{}, errors.New("no fake Sway tree remains")
		}
		tree := requester.trees[0]
		if len(requester.trees) > 1 {
			requester.trees = requester.trees[1:]
		}
		encoded, err := json.Marshal(tree)
		return swayipc.Message{Type: swayipc.GetTree, Payload: encoded}, err
	case swayipc.RunCommand:
		requester.commands = append(requester.commands, string(payload))
		return swayipc.Message{Type: swayipc.RunCommand, Payload: []byte(`[{"success":true}]`)}, nil
	default:
		return swayipc.Message{}, errors.New("unexpected fake Sway request")
	}
}

func (*daemonLoopRequester) Close() {}

func TestSessionDaemonLoopOwnsPlacementAndCaptureWithoutAnimator(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "sway-session")
	registry := sessionRegistry(testManagedContextID)
	if err := sessionstate.RegistryFile(root).Save(registry); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	desired := placementOnlySnapshot("9: saved", testManagedContextID)
	if err := sessionstate.LayoutFile(root).Save(desired); err != nil {
		t.Fatalf("save desired layout: %v", err)
	}

	appID, _ := testManagedContextID.AppID()
	requester := &daemonLoopRequester{trees: []*swayipc.TreeNode{
		daemonTree("1", &Node{ID: 42, Name: "terminal", Type: "con", AppID: &appID}),
		daemonTree("9: saved", managedDaemonLeaf(t, 42, testManagedContextID)),
	}}
	runtime, err := newSessionRuntime(requester)
	if err != nil {
		t.Fatalf("create session runtime: %v", err)
	}
	events := make(chan swayipc.Event, 1)
	events <- swayipc.Event{Type: swayipc.EventShutdown, Change: "exit"}
	if err := runSessionDaemonLoop(t.Context(), requester, runtime, events, nil); err != nil {
		t.Fatalf("run session daemon loop: %v", err)
	}
	if len(requester.commands) < 2 ||
		!strings.Contains(requester.commands[0], `move container to workspace "9: saved"`) ||
		!strings.Contains(requester.commands[1], `mark --add "persist:`) {
		t.Fatalf("daemon did not perform placement and marking: %v", requester.commands)
	}
	if got := runtime.desired.Workspaces; len(got) != 1 || got[0].Name != "9: saved" {
		t.Fatalf("daemon did not capture the stable workspace: %+v", got)
	}
}
