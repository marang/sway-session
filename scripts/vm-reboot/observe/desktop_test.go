package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func desktopFixture(t *testing.T) (session.Registry, session.LayoutSnapshot, *swayipc.TreeNode) {
	t.Helper()
	registry, stored, tree := fixture(t)
	for index := range registry.Contexts {
		c := &registry.Contexts[index]
		c.Provider = ""
		c.Label = "PRIVATE REGISTRY LABEL"
		c.App = &session.Application{DesiredOpen: true, RestorePolicy: session.ApplicationRestoreFollow,
			Identity: session.ApplicationIdentity{Protocol: session.WindowWayland, WaylandAppID: "google-chrome"}}
		c.Launcher = session.Launcher{Kind: session.LauncherDesktop, DesktopID: "lab253-chrome.desktop", DesktopOrigin: session.DesktopEntryUser,
			DesktopPath: "/home/ss-test/.local/share/applications/lab253-chrome.desktop", DesktopEntrySHA256: strings.Repeat("a", 64), ApprovedDesktopPath: "/home/ss-test/PRIVATE-APPROVAL.desktop"}
		if index == 1 {
			c.App.Identity.WaylandAppID = "com.slack.Slack"
			c.App.Identity.SandboxAppID = "com.slack.Slack"
			c.Launcher = session.Launcher{Kind: session.LauncherFlatpak, FlatpakID: "com.slack.Slack", FlatpakInstallation: session.FlatpakUser}
		}
		node := tree.Nodes[0].Nodes[index]
		appID := c.App.Identity.WaylandAppID
		node.AppID = &appID
		node.Focused = index == 0
		if index == 1 {
			sandbox, engine, instance := "com.slack.Slack", "flatpak", "PRIVATE-SANDBOX-INSTANCE"
			node.SandboxAppID, node.SandboxEngine, node.SandboxInstance = &sandbox, &engine, &instance
		}
	}
	if err := registry.Validate(); err != nil {
		t.Fatal(err)
	}
	return registry, stored, tree
}

func TestDesktopObservationProjectsCanonicalFixture(t *testing.T) {
	registry, stored, tree := desktopFixture(t)
	got, err := ObserveDesktop(registry, stored, tree, fixtureIDs)
	if err != nil {
		t.Fatal(err)
	}
	if got.Scenario != "desktop" || got.ApplicationCount != 2 || !got.MatchesFixture || got.StagingManagedWindows != 0 || got.Applications[0].Role != "chrome" || got.Applications[1].Role != "slack" || !got.Applications[1].SandboxInstancePresent {
		t.Fatalf("unexpected observation: %+v", got)
	}
	assertDesktopPrivate(t, got)
	// The API is read-only, including policy and approval records.
	beforeRegistry, _ := json.Marshal(registry)
	beforeLayout, _ := json.Marshal(stored)
	_, _ = ObserveDesktop(registry, stored, tree, fixtureIDs)
	afterRegistry, _ := json.Marshal(registry)
	afterLayout, _ := json.Marshal(stored)
	if !bytes.Equal(beforeRegistry, afterRegistry) || !bytes.Equal(beforeLayout, afterLayout) {
		t.Fatal("observer mutated canonical state")
	}
}

func TestDesktopObservationRejectsChangedFixture(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*session.Registry, *session.LayoutSnapshot, *swayipc.TreeNode)
	}{
		{"wrong role", func(reg *session.Registry, _ *session.LayoutSnapshot, _ *swayipc.TreeNode) {
			reg.Contexts[0], reg.Contexts[1] = reg.Contexts[1], reg.Contexts[0]
			reg.Contexts[0].ID, reg.Contexts[1].ID = reg.Contexts[1].ID, reg.Contexts[0].ID
		}},
		{"identity drift with same mark", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			value := "changed-chrome"
			tree.Nodes[0].Nodes[0].AppID = &value
		}},
		{"Slack sandbox missing", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			tree.Nodes[0].Nodes[1].SandboxAppID = nil
		}},
		{"Slack system installation", func(reg *session.Registry, _ *session.LayoutSnapshot, _ *swayipc.TreeNode) {
			reg.Contexts[1].Launcher.FlatpakInstallation = session.FlatpakSystem
		}},
		{"wrong fixture desktop ID", func(reg *session.Registry, _ *session.LayoutSnapshot, _ *swayipc.TreeNode) {
			reg.Contexts[0].Launcher.DesktopID = "other.desktop"
		}},
		{"Chrome stock launcher", func(reg *session.Registry, _ *session.LayoutSnapshot, _ *swayipc.TreeNode) {
			c := &reg.Contexts[0].Launcher
			c.DesktopOrigin = session.DesktopEntrySystem
			c.DesktopEntrySHA256 = ""
			c.ApprovedDesktopPath = ""
		}},
		{"duplicate app window", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			copy := *tree.Nodes[0].Nodes[0]
			copy.ID = 99
			copy.Marks = nil
			tree.Nodes[0].Nodes = append(tree.Nodes[0].Nodes, &copy)
		}},
		{"unrelated transient window", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			app := "unrelated"
			parent := int64(10)
			tree.Nodes[0].FloatingNodes = []*swayipc.TreeNode{{ID: 40, AppID: &app, WindowProperties: swayipc.WindowProperties{TransientFor: &parent}}}
		}},
		{"marked staging parent", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			mark, _ := fixtureIDs[0].Mark()
			tree.Nodes = append(tree.Nodes, &swayipc.TreeNode{Type: "workspace", Name: session.RestoreStagingWorkspace, Nodes: []*swayipc.TreeNode{{ID: 99, Marks: []string{mark}}}})
		}},
		{"staging survivor", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			tree.Nodes = append(tree.Nodes, &swayipc.TreeNode{Type: "workspace", Name: session.RestoreStagingWorkspace, Nodes: []*swayipc.TreeNode{tree.Nodes[0].Nodes[0]}})
			tree.Nodes[0].Nodes = tree.Nodes[0].Nodes[1:]
		}},
		{"unmarked anchor", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			tree.Nodes[0].Nodes[0].Marks = nil
		}},
		{"ambiguous anchors", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			copy := *tree.Nodes[0].Nodes[0]
			copy.ID = 99
			tree.Nodes[0].Nodes = append(tree.Nodes[0].Nodes, &copy)
		}},
		{"both layouts changed", func(_ *session.Registry, stored *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			stored.Workspaces[0].Tiling.Layout = session.LayoutSplitHorizontal
			tree.Nodes[0].Layout = "splith"
		}},
		{"live order", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			tree.Nodes[0].Nodes[0], tree.Nodes[0].Nodes[1] = tree.Nodes[0].Nodes[1], tree.Nodes[0].Nodes[0]
		}},
		{"stored order", func(_ *session.Registry, stored *session.LayoutSnapshot, _ *swayipc.TreeNode) {
			c := stored.Workspaces[0].Tiling.Children
			c[0], c[1] = c[1], c[0]
		}},
		{"live fullscreen", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			tree.Nodes[0].Nodes[0].FullscreenMode = 1
		}},
		{"floating", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			w := tree.Nodes[0]
			w.FloatingNodes = w.Nodes[:1]
			w.Nodes = w.Nodes[1:]
			w.FloatingNodes[0].Rect = swayipc.Rect{Width: 400, Height: 300}
		}},
		{"singleton tab wrapper", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			w := tree.Nodes[0]
			w.Nodes = []*swayipc.TreeNode{{ID: 40, Type: "con", Layout: "tabbed", Nodes: w.Nodes}}
		}},
		{"stored scratchpad", func(_ *session.Registry, stored *session.LayoutSnapshot, _ *swayipc.TreeNode) {
			stored.Scratchpad = []session.ScratchpadPlacement{{ContextID: fixtureIDs[0]}}
		}},
		{"pinned policy", func(reg *session.Registry, _ *session.LayoutSnapshot, _ *swayipc.TreeNode) {
			reg.Contexts[0].App.RestorePolicy = session.ApplicationRestorePinned
		}},
		{"closed intent", func(reg *session.Registry, _ *session.LayoutSnapshot, _ *swayipc.TreeNode) {
			reg.Contexts[0].App.DesiredOpen = false
		}},
		{"low workspace", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) { tree.Nodes[0].Name = "1" }},
		{"unbounded sandbox engine", func(_ *session.Registry, _ *session.LayoutSnapshot, tree *swayipc.TreeNode) {
			value := "PRIVATE ENGINE"
			tree.Nodes[0].Nodes[1].SandboxEngine = &value
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, stored, tree := desktopFixture(t)
			tc.change(&reg, &stored, tree)
			if got, err := ObserveDesktop(reg, stored, tree, fixtureIDs); err == nil {
				t.Fatalf("changed fixture accepted: %+v", got)
			} else {
				assertDesktopTypedError(t, err)
			}
		})
	}
}

func TestDesktopProbeInitialUnregisteredAndProtocolEvidence(t *testing.T) {
	registry, _, tree := desktopFixture(t)
	registry.Contexts = []session.Context{}
	for _, node := range tree.Nodes[0].Nodes {
		node.Marks = nil
	}
	got, err := ProbeDesktop(registry, tree, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalWindows != 2 || got.Focused == nil || got.Focused.ContextID != nil || got.Focused.AppID != "google-chrome" || len(got.Groups) != 0 || len(got.CaptureModes) != 0 {
		t.Fatalf("initial probe = %+v", got)
	}
	// The negative Slack XWayland phase must expose the actual missing sandbox
	// identity without converting class evidence into a Flatpak identity.
	node := tree.Nodes[0].Nodes[0]
	node.AppID = nil
	node.SandboxAppID, node.SandboxEngine, node.SandboxInstance = nil, nil, nil
	node.WindowProperties = swayipc.WindowProperties{Class: "com.slack.Slack", Instance: "com.slack.slack"}
	got, err = ProbeDesktop(registry, tree, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Focused == nil || got.Focused.Protocol != session.WindowXWayland || got.Focused.SandboxAppID != "" || got.Focused.X11Class != "com.slack.Slack" {
		t.Fatalf("actual protocol evidence lost: %+v", got)
	}
	assertDesktopPrivate(t, got)
}

func TestDesktopProbeCanonicalAmbiguityAndAbsence(t *testing.T) {
	registry, _, tree := desktopFixture(t)
	chrome := tree.Nodes[0].Nodes[0]
	chrome.Marks = nil
	second := *chrome
	second.ID, second.Focused = 99, false
	tree.Nodes[0].Nodes = append(tree.Nodes[0].Nodes, &second)
	got, err := ProbeDesktop(registry, tree, fixtureIDs)
	if err != nil {
		t.Fatal(err)
	}
	if got.Groups[0].Presence != "ambiguous" || got.Groups[0].WindowCount != 2 || got.Groups[0].Anchor != nil || got.Groups[0].AnchorMarked || len(got.CaptureModes) != 1 || got.CaptureModes[0].RestoreMode != session.WorkspaceRestorePlacementOnly {
		t.Fatalf("ambiguity was guessed or capture changed: %+v", got)
	}
	if got.Focused == nil || got.Focused.ContextID == nil || *got.Focused.ContextID != fixtureIDs[0] {
		t.Fatalf("focused canonical registration missing: %+v", got)
	}
	// Empty groups are still emitted, and saved policies remain independent
	// from current presence. No artificial anchor fills a last-window gap.
	tree.Nodes[0].Nodes = nil
	registry.Contexts[0].App.DesiredOpen = false
	registry.Contexts[1].App.RestorePolicy = session.ApplicationRestorePinned
	got, err = ProbeDesktop(registry, tree, fixtureIDs)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range got.Groups {
		if group.Presence != "absent" || group.WindowCount != 0 || group.Anchor != nil || group.AnchorMarked || len(group.Windows) != 0 {
			t.Fatalf("absence = %+v", group)
		}
	}
	if got.Focused != nil || got.Groups[0].DesiredOpen || got.Groups[1].Policy != session.ApplicationRestorePinned {
		t.Fatalf("presence changed saved policy: %+v", got)
	}
}

func TestDesktopProbeNeverGuessesFocusedOrGroupAnchor(t *testing.T) {
	registry, _, tree := desktopFixture(t)
	tree.Nodes[0].Nodes[1].Focused = true
	got, err := ProbeDesktop(registry, tree, fixtureIDs)
	if err == nil {
		t.Fatalf("inconsistent multiple-focus tree accepted: %+v", got)
	}
	assertDesktopTypedError(t, err)
	for _, node := range tree.Nodes[0].Nodes {
		node.Focused = false
	}
	got, err = ProbeDesktop(registry, tree, fixtureIDs)
	if err != nil || got.Focused != nil {
		t.Fatalf("no focused window = %+v, %v", got, err)
	}
	tree.Nodes[0].Nodes[0].Marks = nil
	copy := *tree.Nodes[0].Nodes[0]
	copy.ID = 99
	tree.Nodes[0].Nodes = append(tree.Nodes[0].Nodes, &copy)
	got, err = ProbeDesktop(registry, tree, fixtureIDs)
	if err != nil {
		t.Fatal(err)
	}
	if got.Groups[0].Presence != "ambiguous" || got.Groups[0].Anchor != nil {
		t.Fatalf("duplicate anchor guessed: %+v", got.Groups[0])
	}
}

func TestDesktopObserverBoundsBeforeCanonicalRecursion(t *testing.T) {
	for _, tc := range []string{"depth", "count", "nil", "duplicate window", "private workspace"} {
		t.Run(tc, func(t *testing.T) {
			registry, _, tree := desktopFixture(t)
			switch tc {
			case "depth":
				node := tree
				for range 66 {
					child := &swayipc.TreeNode{}
					node.Nodes = append(node.Nodes, child)
					node = child
				}
			case "count":
				for range 4096 {
					tree.Nodes = append(tree.Nodes, &swayipc.TreeNode{})
				}
			case "nil":
				tree.Nodes = append(tree.Nodes, nil)
			case "duplicate window":
				tree.Nodes[0].Nodes = append(tree.Nodes[0].Nodes, tree.Nodes[0].Nodes[0])
			case "private workspace":
				tree.Nodes[0].Name = "PRIVATE WORKSPACE"
			}
			_, err := ProbeDesktop(registry, tree, fixtureIDs)
			if err == nil {
				t.Fatal("invalid/budget-exceeding tree accepted")
			}
			assertDesktopTypedError(t, err)
		})
	}
}

func TestDesktopObserverRejectsTerminalRegistryAndBadExpected(t *testing.T) {
	registry, stored, tree := fixture(t)
	if _, err := ObserveDesktop(registry, stored, tree, fixtureIDs); err == nil {
		t.Fatal("terminal fixture mislabeled desktop")
	}
	registry, stored, tree = desktopFixture(t)
	for _, expected := range [][]session.ContextID{nil, fixtureIDs[:1], {fixtureIDs[0], fixtureIDs[0]}, {fixtureIDs[1], fixtureIDs[0]}, {fixtureIDs[0], "bad-id"}} {
		if _, err := ObserveDesktop(registry, stored, tree, expected); err == nil {
			t.Fatalf("expected IDs accepted: %v", expected)
		}
	}
	for _, expected := range [][]session.ContextID{{fixtureIDs[0], fixtureIDs[0]}, {"bad-id"}, {fixtureIDs[0], fixtureIDs[1], fixtureIDs[0]}} {
		if _, err := ProbeDesktop(registry, tree, expected); err == nil {
			t.Fatalf("probe IDs accepted: %v", expected)
		}
	}
}

func TestDesktopCLIReadsCanonicalStoresAndProbeDoesNotCreateThem(t *testing.T) {
	for _, probe := range []bool{false, true} {
		t.Run(map[bool]string{false: "strict", true: "initial probe"}[probe], func(t *testing.T) {
			registry, stored, tree := desktopFixture(t)
			directory, err := os.MkdirTemp("", "ss-desktop-observer-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(directory) })
			root := filepath.Join(directory, "state")
			args := []string{"--scenario", "desktop", "--state-root", root, "--socket", filepath.Join(directory, "sway.sock")}
			if probe {
				args = append(args, "--probe")
				for _, node := range tree.Nodes[0].Nodes {
					node.Marks = nil
				}
			} else {
				if err := session.RegistryStoreFor(root).Save(registry); err != nil {
					t.Fatal(err)
				}
				if err := session.LayoutStoreFor(root).Save(stored); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--expected", string(fixtureIDs[0])+","+string(fixtureIDs[1]))
			}
			serveDesktopTree(t, filepath.Join(directory, "sway.sock"), tree)
			var stdout, stderr bytes.Buffer
			if code := run(t.Context(), args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			assertDesktopPrivate(t, json.RawMessage(stdout.Bytes()))
			if probe {
				if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("probe initialized an absent store: %v", err)
				}
				var observed DesktopProbe
				if err := json.Unmarshal(stdout.Bytes(), &observed); err != nil || observed.Focused == nil || len(observed.Groups) != 0 {
					t.Fatalf("initial CLI probe = %s err=%v", stdout.String(), err)
				}
			} else {
				var after session.Registry
				if err := session.RegistryStoreFor(root).LoadInto(&after); err != nil || !reflect.DeepEqual(after, registry) {
					t.Fatalf("read-only observer changed registry: %v", err)
				}
			}
		})
	}
}

func TestDesktopCLIErrorOutputIsAllowlisted(t *testing.T) {
	for _, suffix := range [][]string{{"--expected", "PRIVATE UUID"}, {"--expected", string(fixtureIDs[0])}, {"--probe", "--PRIVATE-FLAG"}, {"--state-root", "PRIVATE PATH"}} {
		args := append([]string{"--scenario", "desktop", "--state-root", "/tmp/PRIVATE STATE", "--socket", "/tmp/PRIVATE SOCKET"}, suffix...)
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), args, &stdout, &stderr); code != 1 || stdout.Len() != 0 || strings.Contains(stderr.String(), "PRIVATE") {
			t.Fatalf("private error escaped: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"--PRIVATE-FLAG", "--scenario=desktop"}, &stdout, &stderr); code != 1 || stdout.Len() != 0 || strings.Contains(stderr.String(), "PRIVATE") {
		t.Fatalf("flag parsing escaped desktop redaction: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertDesktopTypedError(t, redactDesktopError(errors.New("PRIVATE PAYLOAD")))
}

func serveDesktopTree(t *testing.T, socket string, tree *swayipc.TreeNode) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	served := make(chan error, 1)
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			served <- err
			return
		}
		defer peer.Close()
		_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
		header := make([]byte, 14)
		if _, err := io.ReadFull(peer, header); err != nil {
			served <- err
			return
		}
		if string(header[:6]) != "i3-ipc" || binary.LittleEndian.Uint32(header[6:10]) != 0 || binary.LittleEndian.Uint32(header[10:]) != uint32(swayipc.GetTree) {
			served <- errors.New("non-GET_TREE request")
			return
		}
		payload, err := json.Marshal(tree)
		if err != nil {
			served <- err
			return
		}
		binary.LittleEndian.PutUint32(header[6:10], uint32(len(payload)))
		_, err = peer.Write(append(header, payload...))
		served <- err
	}()
	t.Cleanup(func() {
		if err := <-served; err != nil {
			t.Error(err)
		}
	})
}

func assertDesktopPrivate(t *testing.T, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"PRIVATE", "launcher", "argv", "environ", "sandbox_instance_id", "desktop_path", "title", "label"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("private field/content %q exported: %s", forbidden, encoded)
		}
	}
}

func assertDesktopTypedError(t *testing.T, err error) {
	t.Helper()
	var code desktopError
	if !errors.As(err, &code) || len(code) == 0 || strings.ContainsAny(string(code), " /\n\t") || strings.Contains(string(code), "PRIVATE") {
		t.Fatalf("untyped error exported: %v", err)
	}
}
