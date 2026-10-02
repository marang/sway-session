package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/swayipc"
	"golang.org/x/sys/unix"
)

// The fake compositor's lifetime is independent of an operation process. Its
// tree and effect history are committed together before sending any reply.
// Reopening this fixture therefore observes effects even when their caller
// died before receiving an acknowledgement.
type lifecycleFixtureSway struct {
	root        string
	boundary    func(string)
	loseAck     bool
	reject      bool
	unavailable bool
}

type lifecycleFixtureEffect struct {
	ContainerID int64  `json:"container_id"`
	Kind        string `json:"kind"`
	Mark        string `json:"mark"`
	Changed     bool   `json:"changed"`
}

type lifecycleFixtureWorld struct {
	Compositor string                   `json:"compositor"`
	Tree       *swayipc.TreeNode        `json:"tree"`
	Effects    []lifecycleFixtureEffect `json:"effects"`
	Events     []swayipc.Event          `json:"events"`
}

func lifecycleFixtureDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func lifecycleFixtureNewSway(t *testing.T, root string, windows ...*swayipc.TreeNode) *lifecycleFixtureSway {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	world := lifecycleFixtureWorld{
		Compositor: strings.Repeat("a", 64),
		Tree: &swayipc.TreeNode{ID: 1, Type: "root", Nodes: []*swayipc.TreeNode{{
			ID: 2, Type: "output", Name: "HEADLESS-1", Nodes: []*swayipc.TreeNode{{
				ID: 3, Type: "workspace", Name: "98", Nodes: windows,
			}},
		}}},
	}
	if err := lifecycleFixturePublish(filepath.Join(root, "world.json"), world); err != nil {
		t.Fatal(err)
	}
	return &lifecycleFixtureSway{root: root}
}

func (client *lifecycleFixtureSway) LifecycleCompositorID(ctx context.Context) (string, error) {
	var identity string
	err := client.access(ctx, func(world *lifecycleFixtureWorld) (bool, error) {
		identity = world.Compositor
		return false, nil
	})
	return identity, err
}

var lifecycleFixtureMarkCommand = regexp.MustCompile(`^\[con_id=([0-9]+)\] (mark --add|unmark) ([^\s;]+)$`)

func (client *lifecycleFixtureSway) RequestContext(ctx context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if client.unavailable {
		return swayipc.Message{}, errors.New("fixture compositor unavailable")
	}
	if kind == swayipc.GetTree {
		var response []byte
		err := client.access(ctx, func(world *lifecycleFixtureWorld) (bool, error) {
			var err error
			response, err = json.Marshal(world.Tree)
			return false, err
		})
		return swayipc.Message{Type: kind, Payload: response}, err
	}
	if kind != swayipc.RunCommand {
		return swayipc.Message{}, fmt.Errorf("unexpected fake Sway request %d", kind)
	}
	parts := lifecycleFixtureMarkCommand.FindStringSubmatch(string(payload))
	if parts == nil {
		return swayipc.Message{}, fmt.Errorf("fixture refuses non-mark command %q", payload)
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return swayipc.Message{}, err
	}
	verb := "mark"
	if parts[2] == "unmark" {
		verb = "unmark"
	}
	mark := strings.Trim(parts[3], `"`)
	if client.boundary != nil {
		client.boundary("before-" + verb)
	}
	if client.reject {
		return swayipc.Message{Type: kind, Payload: []byte(`[{"success":false,"error":"fixture rejection"}]`)}, nil
	}
	err = client.access(ctx, func(world *lifecycleFixtureWorld) (bool, error) {
		node := lifecycleFixtureNode(world.Tree, id)
		if node == nil {
			return false, fmt.Errorf("fixture container %d does not exist", id)
		}
		before := slices.Contains(node.Marks, mark)
		if verb == "mark" && !before {
			node.Marks = append(node.Marks, mark)
		} else if verb == "unmark" {
			node.Marks = slices.DeleteFunc(node.Marks, func(value string) bool { return value == mark })
		}
		changed := before != slices.Contains(node.Marks, mark)
		world.Effects = append(world.Effects, lifecycleFixtureEffect{ContainerID: id, Kind: verb, Mark: mark, Changed: changed})
		if changed {
			// Like real Sway, mark changes emit a window event but do not
			// move focus. Store a snapshot, not a pointer into the mutable tree.
			copy := *node
			copy.Marks = slices.Clone(node.Marks)
			world.Events = append(world.Events, swayipc.Event{Type: swayipc.EventWindow, Change: "mark", Container: &copy})
		}
		return true, nil
	})
	if err != nil {
		return swayipc.Message{}, err
	}
	if client.boundary != nil {
		client.boundary("after-" + verb)
	}
	if client.loseAck {
		return swayipc.Message{}, &swayipc.CommandOutcomeUnknownError{Cause: errors.New("fixture lost acknowledgement after durable effect")}
	}
	return swayipc.Message{Type: kind, Payload: []byte(`[{"success":true}]`)}, nil
}

func lifecycleFixtureNode(root *swayipc.TreeNode, id int64) *swayipc.TreeNode {
	if root == nil || root.ID == id {
		return root
	}
	for _, children := range [][]*swayipc.TreeNode{root.Nodes, root.FloatingNodes} {
		for _, child := range children {
			if found := lifecycleFixtureNode(child, id); found != nil {
				return found
			}
		}
	}
	return nil
}

func (client *lifecycleFixtureSway) access(ctx context.Context, action func(*lifecycleFixtureWorld) (bool, error)) error {
	return lifecycleFixtureWithLock(ctx, filepath.Join(client.root, "world.lock"), func() error {
		data, err := os.ReadFile(filepath.Join(client.root, "world.json"))
		if err != nil {
			return err
		}
		var world lifecycleFixtureWorld
		if err := json.Unmarshal(data, &world); err != nil {
			return err
		}
		changed, err := action(&world)
		if err != nil || !changed {
			return err
		}
		return lifecycleFixturePublish(filepath.Join(client.root, "world.json"), world)
	})
}

func lifecycleFixtureWithLock(ctx context.Context, path string, action func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return action()
}

func lifecycleFixturePublish(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".lifecycle-fixture-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func lifecycleFixtureReadWorld(t *testing.T, client *lifecycleFixtureSway) lifecycleFixtureWorld {
	t.Helper()
	var result lifecycleFixtureWorld
	if err := client.access(t.Context(), func(world *lifecycleFixtureWorld) (bool, error) {
		result = *world
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}
