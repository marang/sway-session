package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// Desktop errors are fixed reason codes. Canonical API errors can include
// registry values, paths or tree payloads and must not cross this boundary.
type desktopError string

func (e desktopError) Error() string { return string(e) }

func redactDesktopError(err error) error {
	var code desktopError
	if errors.As(err, &code) {
		return code
	}
	return desktopError("observation_failed")
}

// Flag parsing may stop before --scenario, so select error redaction without
// depending on successfully parsed arguments.
func desktopRequested(args []string) bool {
	for index, value := range args {
		if value == "--scenario=desktop" || value == "-scenario=desktop" ||
			(value == "--scenario" || value == "-scenario") && index+1 < len(args) && args[index+1] == "desktop" {
			return true
		}
	}
	return false
}

func runDesktop(ctx context.Context, root, socket, ids string, probe bool, stdout io.Writer, fail func(error) int) int {
	expected := []session.ContextID{}
	if ids != "" {
		for _, value := range strings.Split(ids, ",") {
			id, err := session.ParseContextID(value)
			if err != nil {
				return fail(desktopError("invalid_expected_ids"))
			}
			expected = append(expected, id)
		}
	}
	if len(expected) > 2 || !probe && len(expected) != 2 || len(expected) == 2 && expected[0] == expected[1] {
		return fail(desktopError("invalid_expected_ids"))
	}
	registry := session.Registry{Version: session.ContextsSchemaVersion, Contexts: []session.Context{}}
	if err := session.RegistryStoreFor(root).LoadIntoContext(ctx, &registry); err != nil && !(probe && errors.Is(err, os.ErrNotExist)) {
		return fail(desktopError("registry_unavailable"))
	}
	var stored session.LayoutSnapshot
	if !probe {
		if err := session.LayoutStoreFor(root).LoadIntoContext(ctx, &stored); err != nil {
			return fail(desktopError("stored_layout_unavailable"))
		}
	}
	client := swayipc.NewClient(socket)
	defer client.Close()
	message, err := client.RequestContext(ctx, swayipc.GetTree, nil)
	if err != nil {
		return fail(desktopError("tree_unavailable"))
	}
	if message.Type != swayipc.GetTree {
		return fail(desktopError("unexpected_tree_message"))
	}
	var tree swayipc.TreeNode
	if err := json.Unmarshal(message.Payload, &tree); err != nil {
		return fail(desktopError("invalid_tree_json"))
	}
	var result any
	if probe {
		result, err = ProbeDesktop(registry, &tree, expected)
	} else {
		result, err = ObserveDesktop(registry, stored, &tree, expected)
	}
	if err != nil {
		return fail(err)
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fail(desktopError("output_failed"))
	}
	return 0
}

type desktopIdentity struct {
	Protocol               session.WindowProtocol `json:"protocol"`
	AppID                  string                 `json:"app_id"`
	X11Class               string                 `json:"x11_class"`
	X11Instance            string                 `json:"x11_instance"`
	SandboxAppID           string                 `json:"sandbox_app_id"`
	SandboxEngine          string                 `json:"sandbox_engine"`
	SandboxInstancePresent bool                   `json:"sandbox_instance_present"`
}

type desktopApplication struct {
	Role        string                           `json:"role"`
	ContextID   session.ContextID                `json:"context_id"`
	DesiredOpen bool                             `json:"desired_open"`
	Policy      session.ApplicationRestorePolicy `json:"policy"`
	desktopIdentity
	Presence     string `json:"presence"`
	WindowCount  int    `json:"window_count"`
	AnchorMarked bool   `json:"anchor_marked"`
}

type DesktopObservation struct {
	Version               int                  `json:"version"`
	Scenario              string               `json:"scenario"`
	Workspace             string               `json:"workspace"`
	Contexts              []contextState       `json:"contexts"`
	Applications          []desktopApplication `json:"applications"`
	Live                  semanticSnapshot     `json:"live"`
	Stored                semanticSnapshot     `json:"stored"`
	ApplicationCount      int                  `json:"application_count"`
	StagingManagedWindows int                  `json:"staging_managed_windows"`
	MatchesFixture        bool                 `json:"matches_fixture"`
	Exclusions            []string             `json:"exclusions"`
}

// ObserveDesktop compares both canonical captures with the original fixed
// two-role fixture. Equal but damaged live/stored state cannot become a pass.
func ObserveDesktop(registry session.Registry, stored session.LayoutSnapshot, tree *swayipc.TreeNode, expected []session.ContextID) (DesktopObservation, error) {
	fail := func(code desktopError) (DesktopObservation, error) { return DesktopObservation{}, code }
	if len(expected) != 2 || expected[0] == expected[1] {
		return fail("invalid_expected_ids")
	}
	windows, err := desktopTree(tree, true)
	if err != nil {
		return DesktopObservation{}, err
	}
	if err := validateDesktopRegistry(registry, expected); err != nil || len(registry.Contexts) != 2 {
		return fail("invalid_fixture_registry")
	}
	groups, err := session.ObserveRegisteredApplicationGroups(tree, registry)
	if err != nil {
		return fail("application_groups_invalid")
	}
	states := make([]contextState, 0, 2)
	applications := make([]desktopApplication, 0, 2)
	for index, id := range expected {
		var c session.Context
		for _, registered := range registry.Contexts {
			if registered.ID == id {
				c = registered
			}
		}
		if c.State != session.ContextActive || !c.App.DesiredOpen || c.App.RestorePolicy != session.ApplicationRestoreFollow {
			return fail("fixture_policy_changed")
		}
		if c.App.Identity.Protocol != session.WindowWayland {
			return fail("fixture_protocol_changed")
		}
		role := "chrome"
		if index == 0 {
			if c.Launcher.Kind != session.LauncherDesktop || c.Launcher.DesktopID != "lab253-chrome.desktop" || c.Launcher.DesktopOrigin != session.DesktopEntryUser || c.App.Identity.SandboxAppID != "" {
				return fail("fixture_role_changed")
			}
		} else {
			role = "slack"
			if c.Launcher.Kind != session.LauncherFlatpak || c.Launcher.FlatpakID != "com.slack.Slack" || c.Launcher.FlatpakInstallation != session.FlatpakUser || c.App.Identity.SandboxAppID != "com.slack.Slack" {
				return fail("fixture_role_changed")
			}
		}
		group := groups[id]
		presence := session.ApplicationGroupPresence(group, time.Time{})
		if presence.State != "present" || group.Ambiguous || len(group.Windows) != 1 || group.Anchor == nil || !group.AnchorMarked || group.Anchor.Workspace != "98" || group.Anchor.Scratchpad {
			return fail("fixture_presence_changed")
		}
		// Group overlap is intentionally broader than exact restoration evidence.
		// Check every stable observed identity field against the saved identity.
		identity := group.Anchor.Identity
		if identity.Protocol != c.App.Identity.Protocol || identity.WaylandAppID != c.App.Identity.WaylandAppID || identity.X11Class != c.App.Identity.X11Class || identity.X11Instance != c.App.Identity.X11Instance || identity.SandboxAppID != c.App.Identity.SandboxAppID {
			return fail("fixture_identity_changed")
		}
		node := windows[group.Anchor.ContainerID]
		if node == nil {
			return fail("fixture_window_missing")
		}
		projection, err := projectDesktopIdentity(identity, node)
		if err != nil {
			return DesktopObservation{}, err
		}
		states = append(states, contextState{ID: id, State: c.State})
		applications = append(applications, desktopApplication{Role: role, ContextID: id, DesiredOpen: true, Policy: c.App.RestorePolicy, desktopIdentity: projection, Presence: presence.State, WindowCount: 1, AnchorMarked: true})
	}
	if len(windows) != 2 {
		return fail("fixture_window_count_changed")
	}
	if err := stored.Validate(); err != nil || len(stored.Scratchpad) != 0 {
		return fail("stored_layout_invalid")
	}
	live, err := session.CaptureLayout(tree, registry)
	if err != nil || len(live.Scratchpad) != 0 {
		return fail("capture_invalid")
	}
	liveTopology, err := fixtureTopology(live)
	if err != nil {
		return fail("live_layout_changed")
	}
	storedTopology, err := fixtureTopology(stored)
	if err != nil {
		return fail("stored_layout_changed")
	}
	want := topology{Tiling: &semanticNode{Layout: session.LayoutTabbed, Children: []semanticNode{{ContextID: &expected[0]}, {ContextID: &expected[1]}}}, Floating: []semanticNode{}}
	if !reflect.DeepEqual(liveTopology, want) {
		return fail("live_topology_changed")
	}
	if !reflect.DeepEqual(storedTopology, want) {
		return fail("stored_topology_changed")
	}
	sort.Slice(states, func(i, j int) bool { return states[i].ID < states[j].ID })
	return DesktopObservation{Version: 1, Scenario: "desktop", Workspace: "98", Contexts: states, Applications: applications,
		Live:             semanticSnapshot{Workspaces: []semanticWorkspace{{Name: "98", RestoreMode: session.WorkspaceRestoreLayout, topology: liveTopology}}},
		Stored:           semanticSnapshot{Workspaces: []semanticWorkspace{{Name: "98", RestoreMode: session.WorkspaceRestoreLayout, topology: storedTopology}}},
		ApplicationCount: 2, MatchesFixture: true, Exclusions: []string{"proportions", "geometry", "focus"}}, nil
}

type desktopWindow struct {
	ContainerID int64  `json:"container_id"`
	Workspace   string `json:"workspace"`
}

type desktopFocused struct {
	desktopWindow
	desktopIdentity
	ContextID *session.ContextID `json:"context_id,omitempty"`
}

type desktopGroup struct {
	ContextID    session.ContextID                `json:"context_id"`
	State        session.ContextState             `json:"state"`
	DesiredOpen  bool                             `json:"desired_open"`
	Policy       session.ApplicationRestorePolicy `json:"policy"`
	Presence     string                           `json:"presence"`
	WindowCount  int                              `json:"window_count"`
	AnchorMarked bool                             `json:"anchor_marked"`
	Anchor       *desktopWindow                   `json:"anchor"`
	Windows      []desktopWindow                  `json:"windows"`
}

type desktopCaptureMode struct {
	Workspace   string                       `json:"workspace"`
	RestoreMode session.WorkspaceRestoreMode `json:"restore_mode"`
}

type DesktopProbe struct {
	Version      int                  `json:"version"`
	Scenario     string               `json:"scenario"`
	TotalWindows int                  `json:"total_windows"`
	Focused      *desktopFocused      `json:"focused"`
	Groups       []desktopGroup       `json:"groups"`
	CaptureModes []desktopCaptureMode `json:"capture_modes"`
}

// ProbeDesktop exposes canonical lifecycle evidence during fixture phases,
// including absence and ambiguity. It never chooses an anchor itself.
func ProbeDesktop(registry session.Registry, tree *swayipc.TreeNode, expected []session.ContextID) (DesktopProbe, error) {
	windows, err := desktopTree(tree, false)
	if err != nil {
		return DesktopProbe{}, err
	}
	if err := validateDesktopRegistry(registry, expected); err != nil {
		return DesktopProbe{}, err
	}
	result := DesktopProbe{Version: 1, Scenario: "desktop_probe", TotalWindows: len(windows), Groups: []desktopGroup{}, CaptureModes: []desktopCaptureMode{}}
	groups, err := session.ObserveRegisteredApplicationGroups(tree, registry)
	if err != nil {
		return DesktopProbe{}, desktopError("application_groups_invalid")
	}
	for _, c := range registry.Contexts {
		group := groups[c.ID]
		presence := session.ApplicationGroupPresence(group, time.Time{})
		item := desktopGroup{ContextID: c.ID, State: c.State, DesiredOpen: c.App.DesiredOpen, Policy: c.App.RestorePolicy, Presence: presence.State, WindowCount: len(group.Windows), AnchorMarked: group.AnchorMarked, Windows: []desktopWindow{}}
		for _, window := range group.Windows {
			item.Windows = append(item.Windows, desktopWindow{ContainerID: window.ContainerID, Workspace: window.Workspace})
		}
		sort.Slice(item.Windows, func(i, j int) bool { return item.Windows[i].ContainerID < item.Windows[j].ContainerID })
		if !group.Ambiguous && group.Anchor != nil {
			item.Anchor = &desktopWindow{ContainerID: group.Anchor.ContainerID, Workspace: group.Anchor.Workspace}
		}
		result.Groups = append(result.Groups, item)
	}
	sort.Slice(result.Groups, func(i, j int) bool { return result.Groups[i].ContextID < result.Groups[j].ContextID })
	focused, err := session.ResolveFocusedApplication(tree, session.DesktopCatalog{}, registry)
	if err == nil {
		node := windows[focused.Window.ContainerID]
		if node != nil {
			identity, err := projectDesktopIdentity(focused.Window.Identity, node)
			if err != nil {
				return DesktopProbe{}, err
			}
			result.Focused = &desktopFocused{desktopWindow: desktopWindow{ContainerID: focused.Window.ContainerID, Workspace: focused.Window.Workspace}, desktopIdentity: identity}
			if focused.Registered != nil {
				id := focused.Registered.ID
				result.Focused.ContextID = &id
			}
		}
	}
	capture, err := session.CaptureLayout(tree, registry)
	if err != nil {
		return DesktopProbe{}, desktopError("capture_invalid")
	}
	for _, workspace := range capture.Workspaces {
		result.CaptureModes = append(result.CaptureModes, desktopCaptureMode{Workspace: workspace.Name, RestoreMode: workspace.RestoreMode})
	}
	return result, nil
}

func validateDesktopRegistry(registry session.Registry, expected []session.ContextID) error {
	if len(registry.Contexts) > 2 || len(expected) > 2 || len(expected) == 2 && expected[0] == expected[1] || registry.Validate() != nil {
		return desktopError("invalid_fixture_registry")
	}
	for _, c := range registry.Contexts {
		if c.App == nil {
			return desktopError("non_application_context")
		}
	}
	for _, id := range expected {
		if _, err := session.ParseContextID(string(id)); err != nil {
			return desktopError("invalid_expected_ids")
		}
		found := false
		for _, c := range registry.Contexts {
			found = found || c.ID == id
		}
		if !found {
			return desktopError("expected_context_missing")
		}
	}
	return nil
}

func projectDesktopIdentity(identity session.ApplicationIdentity, node *swayipc.TreeNode) (desktopIdentity, error) {
	engine := ""
	if node.SandboxEngine != nil {
		engine = *node.SandboxEngine
	}
	if engine != "" && engine != "org.flatpak" {
		return desktopIdentity{}, desktopError("unsupported_sandbox_engine")
	}
	return desktopIdentity{Protocol: identity.Protocol, AppID: identity.WaylandAppID, X11Class: identity.X11Class, X11Instance: identity.X11Instance, SandboxAppID: identity.SandboxAppID, SandboxEngine: engine, SandboxInstancePresent: node.SandboxInstance != nil && *node.SandboxInstance != ""}, nil
}

// Bound before calling recursive canonical observers. Workspace names exported
// by this harness are fixture numbers or the two fixed transient workspaces.
func desktopTree(tree *swayipc.TreeNode, strict bool) (map[int64]*swayipc.TreeNode, error) {
	windows := map[int64]*swayipc.TreeNode{}
	visited := 0
	var walk func(*swayipc.TreeNode, string, int) error
	walk = func(node *swayipc.TreeNode, workspace string, depth int) error {
		visited++
		if node == nil || depth > 64 || visited > 4096 {
			return desktopError("tree_budget_exceeded")
		}
		if node.Type == "workspace" {
			workspace = node.Name
			if !desktopWorkspace(workspace) {
				return desktopError("non_fixture_workspace")
			}
			if strict && len(node.Nodes) == 1 && (node.Layout == "tabbed" || node.Layout == "stacked") {
				return desktopError("fixture_hierarchy_changed")
			}
		}
		window := node.AppID != nil || node.Window != nil || node.WindowProperties.Class != "" || node.WindowProperties.Instance != ""
		if strict && !window {
			for _, mark := range node.Marks {
				if strings.HasPrefix(mark, session.MarkPrefix) {
					return desktopError("invalid_window_structure")
				}
			}
		}
		if window {
			if workspace == "" || strict && workspace != "98" {
				return desktopError("fixture_window_misplaced")
			}
			if len(node.Nodes) != 0 || len(node.FloatingNodes) != 0 || node.ID <= 0 || windows[node.ID] != nil {
				return desktopError("invalid_window_structure")
			}
			windows[node.ID] = node
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
		return nil, err
	}
	return windows, nil
}

func desktopWorkspace(name string) bool {
	if name == session.RestoreStagingWorkspace || name == "__i3_scratch" {
		return true
	}
	value, err := strconv.Atoi(name)
	return err == nil && value >= 98 && value <= 999999 && strconv.Itoa(value) == name
}
