// The VM observer reads canonical state and Sway capture without restoring it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("observe", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("state-root", "/home/ss-test/.local/state/sway-session", "disposable guest state root")
	socket := flags.String("socket", "", "exact guest Sway IPC socket")
	ids := flags.String("expected", "", "two comma-separated context UUIDs in tab order")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "VM observer: %v\n", err)
		return 1
	}
	if flags.NArg() != 0 || !filepath.IsAbs(*root) || filepath.Clean(*root) != *root ||
		!filepath.IsAbs(*socket) || filepath.Clean(*socket) != *socket {
		return fail(errors.New("use --state-root ABSOLUTE_PATH --socket ABSOLUTE_PATH --expected UUID,UUID"))
	}
	expected := []session.ContextID{}
	for _, value := range strings.Split(*ids, ",") {
		id, err := session.ParseContextID(value)
		if err != nil {
			return fail(fmt.Errorf("expected identity: %w", err))
		}
		expected = append(expected, id)
	}
	if len(expected) != 2 || expected[0] == expected[1] {
		return fail(errors.New("expected exactly two distinct context UUIDs"))
	}
	var registry session.Registry
	if err := session.RegistryStoreFor(*root).LoadIntoContext(ctx, &registry); err != nil {
		return fail(fmt.Errorf("read registry: %w", err))
	}
	var stored session.LayoutSnapshot
	if err := session.LayoutStoreFor(*root).LoadIntoContext(ctx, &stored); err != nil {
		return fail(fmt.Errorf("read stored layout: %w", err))
	}
	client := swayipc.NewClient(*socket)
	defer client.Close()
	message, err := client.RequestContext(ctx, swayipc.GetTree, nil)
	if err != nil {
		return fail(fmt.Errorf("read Sway tree: %w", err))
	}
	if message.Type != swayipc.GetTree {
		return fail(errors.New("unexpected Sway tree message type"))
	}
	var tree swayipc.TreeNode
	if err := json.Unmarshal(message.Payload, &tree); err != nil {
		return fail(fmt.Errorf("decode Sway tree: %w", err))
	}
	observation, err := Observe(registry, stored, &tree, expected)
	if err != nil {
		return fail(err)
	}
	if err := json.NewEncoder(stdout).Encode(observation); err != nil {
		return fail(fmt.Errorf("write JSON: %w", err))
	}
	return 0
}

type semanticNode struct {
	ContextID  *session.ContextID     `json:"context_id,omitempty"`
	Layout     session.LayoutKind     `json:"layout,omitempty"`
	Children   []semanticNode         `json:"children,omitempty"`
	Fullscreen session.FullscreenMode `json:"fullscreen,omitempty"`
}

type topology struct {
	Tiling   *semanticNode  `json:"tiling"`
	Floating []semanticNode `json:"floating"`
}

type semanticWorkspace struct {
	Name        string                       `json:"name"`
	RestoreMode session.WorkspaceRestoreMode `json:"restore_mode"`
	topology
}

type semanticSnapshot struct {
	Workspaces []semanticWorkspace `json:"workspaces"`
}

type contextState struct {
	ID    session.ContextID    `json:"id"`
	State session.ContextState `json:"state"`
}

type Observation struct {
	Version               int              `json:"version"`
	Workspace             string           `json:"workspace"`
	Contexts              []contextState   `json:"contexts"`
	Live                  semanticSnapshot `json:"live"`
	Stored                semanticSnapshot `json:"stored"`
	TerminalCount         int              `json:"terminal_count"`
	StagingManagedWindows int              `json:"staging_managed_windows"`
	MatchesFixture        bool             `json:"matches_fixture"`
	Exclusions            []string         `json:"exclusions"`
}

// Observe checks observations against the fixed fixture, rather than trusting
// agreement between a damaged live layout and a newly overwritten stored one.
func Observe(registry session.Registry, stored session.LayoutSnapshot, tree *swayipc.TreeNode, expected []session.ContextID) (Observation, error) {
	if len(expected) != 2 || expected[0] == expected[1] {
		return Observation{}, errors.New("expected exactly two distinct context UUIDs in tab order")
	}
	if err := registry.Validate(); err != nil {
		return Observation{}, fmt.Errorf("registry: %w", err)
	}
	if len(registry.Contexts) != 2 {
		return Observation{}, errors.New("registry must contain exactly two fixture contexts")
	}
	states := make([]contextState, 0, 2)
	for _, id := range expected {
		found := false
		for _, c := range registry.Contexts {
			if c.ID != id {
				continue
			}
			if c.State != session.ContextActive || !session.IsTerminalInstanceContext(c) ||
				c.Launcher.Cwd != "/home/ss-test/project" || c.Launcher.Terminal.Adapter != session.TerminalAdapterAlacritty {
				return Observation{}, fmt.Errorf("context %s must be an active fixture Herdr/Alacritty terminal", id)
			}
			states = append(states, contextState{ID: id, State: c.State})
			found = true
		}
		if !found {
			return Observation{}, fmt.Errorf("expected context %s is absent", id)
		}
	}
	if err := stored.Validate(); err != nil {
		return Observation{}, fmt.Errorf("stored layout: %w", err)
	}
	if err := fixtureWindows(tree, expected); err != nil {
		return Observation{}, err
	}
	live, err := session.CaptureLayout(tree, registry)
	if err != nil {
		return Observation{}, fmt.Errorf("capture live layout: %w", err)
	}
	liveTopology, err := fixtureTopology(live)
	if err != nil {
		return Observation{}, fmt.Errorf("live layout: %w", err)
	}
	storedTopology, err := fixtureTopology(stored)
	if err != nil {
		return Observation{}, fmt.Errorf("stored layout: %w", err)
	}
	want := topology{Tiling: &semanticNode{Layout: session.LayoutTabbed, Children: []semanticNode{
		{ContextID: &expected[0]}, {ContextID: &expected[1]},
	}}, Floating: []semanticNode{}}
	if !reflect.DeepEqual(liveTopology, want) {
		return Observation{}, errors.New("live topology must be workspace 98 with the two expected terminals in tab order")
	}
	if !reflect.DeepEqual(storedTopology, want) {
		return Observation{}, errors.New("stored topology must be workspace 98 with the two expected terminals in tab order")
	}
	sort.Slice(states, func(i, j int) bool { return states[i].ID < states[j].ID })
	return Observation{
		Version: 1, Workspace: "98", Contexts: states,
		Live:          semanticSnapshot{Workspaces: []semanticWorkspace{{Name: "98", RestoreMode: session.WorkspaceRestoreLayout, topology: liveTopology}}},
		Stored:        semanticSnapshot{Workspaces: []semanticWorkspace{{Name: "98", RestoreMode: session.WorkspaceRestoreLayout, topology: storedTopology}}},
		TerminalCount: 2, StagingManagedWindows: 0, MatchesFixture: true,
		Exclusions: []string{"proportions", "geometry", "focus"},
	}, nil
}

func fixtureWindows(tree *swayipc.TreeNode, expected []session.ContextID) error {
	counts := map[session.ContextID]int{}
	visited := 0
	var walk func(*swayipc.TreeNode, string, int) error
	walk = func(node *swayipc.TreeNode, workspace string, depth int) error {
		visited++
		if node == nil || depth > 64 || visited > 4096 {
			return errors.New("sway observation exceeds the fixture node/depth budget or contains a nil node")
		}
		if node.Type == "workspace" {
			workspace = node.Name
			// CaptureLayout selects the sole child as the tiling root. For this
			// fixture, a surrounding tabbed/stacked workspace is still meaningful
			// and must not disappear through that canonical capture convention.
			if workspace == "98" && len(node.Nodes) == 1 && (node.Layout == "tabbed" || node.Layout == "stacked") {
				return errors.New("extra singleton tabbed/stacked workspace hierarchy differs from the fixture")
			}
		}
		managed := node.AppID != nil && strings.HasPrefix(*node.AppID, session.AppIDPrefix)
		for _, mark := range node.Marks {
			managed = managed || strings.HasPrefix(mark, session.MarkPrefix)
		}
		if managed && workspace == session.RestoreStagingWorkspace {
			return errors.New("managed window remains in the restore staging workspace")
		}
		window := node.AppID != nil || node.Window != nil || node.WindowProperties.Class != ""
		if managed || window {
			if len(node.Nodes) != 0 || node.AppID == nil || workspace != "98" {
				return errors.New("expected only fixture terminal leaves on workspace 98")
			}
			id, err := session.ParseAppID(*node.AppID)
			if err != nil || id != expected[0] && id != expected[1] {
				return errors.New("unexpected window identity in the disposable compositor")
			}
			mark, _ := id.Mark()
			marked := false
			for _, value := range node.Marks {
				marked = marked || value == mark
			}
			if !marked {
				return fmt.Errorf("fixture terminal %s is not marked by the daemon yet", id)
			}
			counts[id]++
		}
		for _, children := range [][]*swayipc.TreeNode{node.Nodes, node.FloatingNodes} {
			for _, child := range children {
				if err := walk(child, workspace, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(tree, "", 0); err != nil {
		return err
	}
	for _, id := range expected {
		if counts[id] != 1 {
			return fmt.Errorf("fixture terminal %s has %d live windows; expected exactly one", id, counts[id])
		}
	}
	return nil
}

func fixtureTopology(snapshot session.LayoutSnapshot) (topology, error) {
	if len(snapshot.Workspaces) != 1 || snapshot.Workspaces[0].Name != "98" ||
		snapshot.Workspaces[0].RestoreMode != session.WorkspaceRestoreLayout {
		return topology{}, errors.New("expected one complete layout on workspace 98")
	}
	w := snapshot.Workspaces[0]
	result := topology{Floating: []semanticNode{}}
	if w.Tiling != nil {
		node := semantic(*w.Tiling)
		result.Tiling = &node
	}
	for _, node := range w.Floating {
		result.Floating = append(result.Floating, semantic(node))
	}
	return result, nil
}

func semantic(node session.LayoutNode) semanticNode {
	// A plain split with one child has no relative orientation. Keep singleton
	// tabbed/stacked groups, floating roots and fullscreen boundaries intact.
	if (node.Layout == session.LayoutSplitHorizontal || node.Layout == session.LayoutSplitVertical) &&
		len(node.Children) == 1 && node.Geometry == nil && node.Fullscreen == session.FullscreenNone {
		return semantic(node.Children[0])
	}
	result := semanticNode{ContextID: node.ContextID, Layout: node.Layout, Fullscreen: node.Fullscreen}
	for _, child := range node.Children {
		result.Children = append(result.Children, semantic(child))
	}
	return result
}
