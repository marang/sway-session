package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func TestStatusLiveApplicationPlacementsAreReadOnly(t *testing.T) {
	for _, scenario := range []string{"workspace", "hidden scratchpad", "shown scratchpad", "absent", "ambiguous windows", "duplicate anchors", "changed identity", "marked anchor and sibling", "archived", "xwayland"} {
		t.Run(scenario, func(t *testing.T) {
			deps := testDependencies(t)
			registered := statusTestApplication()
			appID := registered.App.Identity.WaylandAppID
			window := &swayipc.TreeNode{ID: 42, Type: "con", AppID: &appID, Name: "private title https://secret.example/?token=private"}
			workspace := &swayipc.TreeNode{ID: 3, Type: "workspace", Name: "98:web", Nodes: []*swayipc.TreeNode{window}}
			tree := &swayipc.TreeNode{ID: 1, Type: "root", Nodes: []*swayipc.TreeNode{{ID: 2, Type: "output", Nodes: []*swayipc.TreeNode{workspace}}}}
			wantPresence, wantWorkspace, wantCount := "present", workspace.Name, 1
			wantScratchpad, wantVisible := false, true
			switch scenario {
			case "hidden scratchpad":
				workspace.Name = "__i3_scratch"
				wantWorkspace, wantScratchpad, wantVisible = "", true, false
			case "shown scratchpad":
				workspace.Nodes = nil
				workspace.FloatingNodes = []*swayipc.TreeNode{{ID: 41, Type: "floating_con", ScratchpadState: "changed", Nodes: []*swayipc.TreeNode{window}}}
				wantScratchpad = true
			case "absent":
				otherID := "org.example.Unregistered"
				window.AppID = &otherID
				wantPresence, wantWorkspace, wantCount = "absent", "", 0
			case "ambiguous windows", "duplicate anchors":
				other := &swayipc.TreeNode{ID: 43, Type: "con", AppID: &appID}
				if scenario == "duplicate anchors" {
					mark, _ := registered.ID.Mark()
					window.Marks, other.Marks = []string{mark}, []string{mark}
				}
				tree.Nodes[0].Nodes = append(tree.Nodes[0].Nodes, &swayipc.TreeNode{ID: 4, Type: "workspace", Name: "__i3_scratch", FloatingNodes: []*swayipc.TreeNode{other}})
				wantPresence, wantWorkspace, wantCount = "ambiguous", "", 2
			case "changed identity":
				mark, _ := registered.ID.Mark()
				window.Marks = []string{mark}
				otherID := "org.example.Other"
				window.AppID = &otherID
				wantPresence, wantWorkspace = "ambiguous", ""
			case "marked anchor and sibling":
				mark, _ := registered.ID.Mark()
				window.Marks = []string{mark}
				tree.Nodes[0].Nodes = append(tree.Nodes[0].Nodes, &swayipc.TreeNode{ID: 4, Type: "workspace", Name: "99", Nodes: []*swayipc.TreeNode{{ID: 43, Type: "con", AppID: &appID}}})
				wantCount = 2
			case "archived":
				at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
				registered.State = sessionstate.ContextArchived
				registered.ArchivedAt = &at
				registered.Lifecycle = &sessionstate.LifecycleTransition{Reason: sessionstate.LifecycleReasonExplicitArchive, At: at}
			case "xwayland":
				registered.App.Identity = sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowXWayland, X11Class: "Example", X11Instance: "example"}
				window.AppID = nil
				window.WindowProperties = swayipc.WindowProperties{Class: "Example", Instance: "example"}
			}
			root, _ := deps.stateRoot()
			saveTestRegistry(t, root, sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{registered}})
			before := loadTestRegistry(t, deps)
			databaseBefore, err := os.ReadFile(filepath.Join(root, "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			client := &fakeSwayClient{trees: []*swayipc.TreeNode{tree}}
			t.Setenv("SWAYSOCK", "/fixture/other.sock")
			deps.newSwayClient = func(socket string) swayRequester {
				if socket != "/fixture/sway.sock" {
					t.Fatalf("unexpected socket %q", socket)
				}
				return client
			}
			deps.desktopCatalog = func() (sessionstate.DesktopCatalog, error) {
				t.Fatal("status loaded desktop catalog")
				return sessionstate.DesktopCatalog{}, nil
			}
			var stdout, stderr bytes.Buffer
			code := runWith([]string{"--json", "status", "--socket", "/fixture/sway.sock"}, strings.NewReader(""), &stdout, &stderr, deps)
			if code != exitSuccess || stderr.Len() != 0 {
				t.Fatalf("status failed code=%d stderr=%q", code, stderr.String())
			}
			var result commandResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Version != commandResultVersion || result.Command != "status" || !reflect.DeepEqual(result.Contexts, before.Contexts) || result.ApplicationPlacements == nil || len(*result.ApplicationPlacements) != 1 {
				t.Fatalf("unexpected status: %+v", result)
			}
			placement := (*result.ApplicationPlacements)[0]
			if placement.ContextID != registered.ID || placement.Presence != wantPresence || placement.Workspace != wantWorkspace || placement.WindowCount != wantCount {
				t.Fatalf("unexpected placement: %+v", placement)
			}
			if wantPresence == "present" {
				if placement.ContainerID != 42 || placement.Scratchpad == nil || *placement.Scratchpad != wantScratchpad || placement.Visible == nil || *placement.Visible != wantVisible {
					t.Fatalf("incorrect exact placement: %+v", placement)
				}
			} else if placement.ContainerID != 0 || placement.Scratchpad != nil || placement.Visible != nil || !strings.Contains(stdout.String(), `"scratchpad":null`) || !strings.Contains(stdout.String(), `"visible":null`) {
				t.Fatalf("unknown placement was guessed: %+v JSON=%s", placement, stdout.String())
			}
			if strings.Contains(stdout.String(), window.Name) || strings.Contains(stdout.String(), "__i3_scratch") {
				t.Fatalf("status exposed window title or hidden workspace: %s", stdout.String())
			}
			var human bytes.Buffer
			if err := writeResult(&human, false, result); err != nil {
				t.Fatal(err)
			}
			for _, expected := range []string{registered.Label, string(registered.ID), string(registered.State), "application=" + wantPresence} {
				if !strings.Contains(human.String(), expected) {
					t.Fatalf("human output missing %q: %s", expected, human.String())
				}
			}
			if wantPresence != "present" && (strings.Contains(human.String(), "scratchpad=") || strings.Contains(human.String(), "visible=") || strings.Contains(human.String(), "workspace=")) {
				t.Fatalf("human output guessed placement: %s", human.String())
			}
			if wantPresence == "present" {
				for _, expected := range []string{map[bool]string{false: "scratchpad=false", true: "scratchpad=true"}[wantScratchpad], map[bool]string{false: "visible=false", true: "visible=true"}[wantVisible]} {
					if !strings.Contains(human.String(), expected) {
						t.Fatalf("human output omitted %q: %s", expected, human.String())
					}
				}
			}
			if strings.Contains(human.String(), window.Name) || strings.Contains(human.String(), "__i3_scratch") || strings.Contains(human.String(), "\x1b") {
				t.Fatalf("human output exposed private title, hidden workspace, or ANSI styling: %q", human.String())
			}
			databaseAfter, err := os.ReadFile(filepath.Join(root, "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(databaseBefore, databaseAfter) || !reflect.DeepEqual(before, loadTestRegistry(t, deps)) {
				t.Fatal("status changed persistent state")
			}
			if client.index != 1 || !client.closed || len(deps.processStarter.(*recordingStarter).calls) != 0 || len(deps.herdrRunner.(*recordingHerdrRunner).calls) != 0 {
				t.Fatalf("status exceeded one read-only observation or leaked its client: %+v", client)
			}
		})
	}
}

func TestStatusEmptyApplicationsRemainExplicit(t *testing.T) {
	for _, withTerminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing registry", true: "terminal only"}[withTerminal], func(t *testing.T) {
			deps := testDependencies(t)
			var terminal sessionstate.Context
			if withTerminal {
				terminal = registerTestContext(t, deps)
			}
			client := &fakeSwayClient{trees: []*swayipc.TreeNode{{Type: "root"}}}
			deps.newSwayClient = func(socket string) swayRequester {
				if socket != "/fixture/environment.sock" {
					t.Fatalf("status ignored SWAYSOCK: %q", socket)
				}
				return client
			}
			t.Setenv("SWAYSOCK", "/fixture/environment.sock")
			var stdout, stderr bytes.Buffer
			code := runWith([]string{"status", "--json"}, strings.NewReader(""), &stdout, &stderr, deps)
			if code != exitSuccess || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"application_placements":[]`) {
				t.Fatalf("missing explicit empty applications code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if withTerminal {
				var result commandResult
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				var human bytes.Buffer
				if err := writeResult(&human, false, result); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(human.String(), terminal.Label) || !strings.Contains(human.String(), string(terminal.ID)) || !strings.Contains(human.String(), "active\therdr") {
					t.Fatalf("status omitted terminal registration: %q", human.String())
				}
			} else {
				root, _ := deps.stateRoot()
				if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("status created missing state root: %v", err)
				}
			}
			stdout.Reset()
			if code := runWith([]string{"--json", "list"}, strings.NewReader(""), &stdout, &stderr, deps); code != exitSuccess || strings.Contains(stdout.String(), "application_placements") {
				t.Fatalf("status changed list output: code=%d stdout=%q", code, stdout.String())
			}
		})
	}
}

func TestStatusRejectsArgumentsBeforeReadingState(t *testing.T) {
	for _, arguments := range [][]string{{"context"}, {"--socket"}, {"--yes"}, {"--socket", "/fixture/sway.sock", "context"}, {"--config", "/fixture/config"}} {
		t.Run(strings.Join(arguments, " "), func(t *testing.T) {
			deps := testDependencies(t)
			deps.stateRoot = func() (string, error) { t.Fatal("invalid status read state"); return "", nil }
			var stdout, stderr bytes.Buffer
			code := runWith(append([]string{"status"}, arguments...), strings.NewReader(""), &stdout, &stderr, deps)
			if code != exitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), "Usage: sway-session status [--socket <path>]") {
				t.Fatalf("invalid arguments result code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestStatusReportsOnlyExplicitApplicationsAcrossAllContexts(t *testing.T) {
	deps := testDependencies(t)
	terminal := registerTestContext(t, deps)
	application := statusTestApplication()
	application.ID = "22222222-2222-4222-8222-222222222222"
	flatpak := statusTestApplication()
	flatpak.ID = "33333333-3333-4333-8333-333333333333"
	flatpak.Label = "Archived Flatpak"
	flatpak.State = sessionstate.ContextArchived
	flatpak.Launcher = sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.Flatpak", FlatpakInstallation: sessionstate.FlatpakUser}
	flatpak.App.Identity = sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.Flatpak", SandboxAppID: "org.example.Flatpak"}
	root, _ := deps.stateRoot()
	saveTestRegistry(t, root, sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{terminal, application, flatpak}})
	unregisteredID := "org.example.Unregistered"
	tree := &swayipc.TreeNode{Type: "root", Nodes: []*swayipc.TreeNode{{Type: "workspace", Name: "98", Nodes: []*swayipc.TreeNode{
		{ID: 42, Type: "con", AppID: &flatpak.App.Identity.WaylandAppID, SandboxAppID: &flatpak.App.Identity.SandboxAppID},
		{ID: 43, Type: "con", AppID: &unregisteredID},
	}}}}
	client := &fakeSwayClient{trees: []*swayipc.TreeNode{tree}}
	deps.newSwayClient = func(string) swayRequester { return client }
	result, failure := executeStatus(t.Context(), []string{"--socket", "/fixture/sway.sock"}, deps)
	if failure != nil || len(result.Contexts) != 3 || result.ApplicationPlacements == nil || len(*result.ApplicationPlacements) != 2 {
		t.Fatalf("unexpected global status result=%+v failure=%+v", result, failure)
	}
	placements := *result.ApplicationPlacements
	if placements[0].ContextID != application.ID || placements[0].Presence != "absent" || placements[1].ContextID != flatpak.ID || placements[1].Presence != "present" || placements[1].ContainerID != 42 {
		t.Fatalf("status included unregistered windows or omitted an explicit application: %+v", placements)
	}
	if result.Contexts[2].State != sessionstate.ContextArchived {
		t.Fatal("status changed archived policy in its output")
	}
}

func TestStatusObservationFailureNeverClaimsAbsence(t *testing.T) {
	for _, scenario := range []string{"disconnected", "invalid identity", "nil node", "invalid root", "null tree", "nil client", "missing dependency", "missing socket", "relative socket", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			deps := testDependencies(t)
			root, _ := deps.stateRoot()
			saveTestRegistry(t, root, sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{statusTestApplication()}})
			client := &fakeSwayClient{trees: []*swayipc.TreeNode{{Type: "root"}}}
			deps.newSwayClient = func(string) swayRequester { return client }
			arguments := []string{"--json", "status", "--socket", "/fixture/sway.sock"}
			ctx := t.Context()
			switch scenario {
			case "disconnected":
				client.err = errors.New("connection refused")
			case "invalid identity":
				appID := "invalid\nidentity"
				client.trees = []*swayipc.TreeNode{applicationCommandTree(&swayipc.TreeNode{ID: 42, Type: "con", AppID: &appID})}
			case "nil node":
				client.trees = []*swayipc.TreeNode{{Type: "root", Nodes: []*swayipc.TreeNode{nil}}}
			case "invalid root":
				client.trees = []*swayipc.TreeNode{{}}
			case "null tree":
				client.trees = []*swayipc.TreeNode{nil}
			case "nil client":
				deps.newSwayClient = func(string) swayRequester { return nil }
			case "missing dependency":
				deps.newSwayClient = nil
			case "missing socket":
				t.Setenv("SWAYSOCK", "")
				arguments = []string{"--json", "status"}
			case "relative socket":
				arguments[len(arguments)-1] = "relative.sock"
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var stdout, stderr bytes.Buffer
			code := runWithContext(ctx, arguments, strings.NewReader(""), &stdout, &stderr, deps)
			if code != exitOperation || stdout.Len() != 0 || stderr.Len() == 0 || strings.Contains(stderr.String(), `"presence":"absent"`) {
				t.Fatalf("observation failure claimed a result code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if scenario == "disconnected" && (!client.closed || !strings.Contains(stderr.String(), "connection refused") || !strings.Contains(stderr.String(), "--socket PATH")) {
				t.Fatalf("Sway failure lacked actionable diagnostics or leaked client: closed=%t stderr=%q", client.closed, stderr.String())
			}
		})
	}
}

func TestStatusHelpDoesNotObserveState(t *testing.T) {
	deps := testDependencies(t)
	deps.stateRoot = func() (string, error) { t.Fatal("help read state"); return "", nil }
	for _, arguments := range [][]string{{"--help"}, {"help", "status"}, {"status", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := runWith(arguments, strings.NewReader(""), &stdout, &stderr, deps); code != exitSuccess || stderr.Len() != 0 || !strings.Contains(stdout.String(), "status [--socket <path>]") {
			t.Fatalf("status help result code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	}
}

func statusTestApplication() sessionstate.Context {
	return sessionstate.Context{
		ID: testContextID, Label: "Example App", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherDesktop, DesktopID: "org.example.App.desktop", DesktopOrigin: sessionstate.DesktopEntrySystem, DesktopPath: "/usr/share/applications/org.example.App.desktop"},
		App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App"}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow},
	}
}

func TestStatusCompletions(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			program, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s unavailable; completion runtime check requires it", shell)
			}
			completion := filepath.Join("..", "..", "contrib", "completions", shell, map[string]string{"bash": "sway-session", "zsh": "_sway-session", "fish": "sway-session.fish"}[shell])
			var script string
			switch shell {
			case "bash":
				script = `source "$STATUS_COMPLETION"
_sway_session_contexts() { echo 'status completion read registry' >&2; exit 1; }
_sway_session_files() { COMPREPLY+=(file-completion); }
check() { _sway_session; printf '%s\n' "${COMPREPLY[@]}"; }
COMP_WORDS=(sway-session sta); COMP_CWORD=1; check
COMP_WORDS=(sway-session status --s); COMP_CWORD=2; check
COMP_WORDS=(sway-session status --socket --json ''); COMP_CWORD=4; check
COMP_WORDS=(sway-session status -- ''); COMP_CWORD=3; _sway_session
test "${#COMPREPLY[@]}" -eq 0 || exit 1
`
			case "zsh":
				script = `source "$STATUS_COMPLETION"
compadd() { while (( $# )); do if [[ $1 == -- ]]; then shift; print -rl -- "$@"; return 0; fi; shift; done; }
_files() { print -r -- file-completion; }
_sway_session_contexts() { print -u2 -- 'status completion read registry'; exit 1; }
words=(sway-session sta); CURRENT=2; _sway-session
words=(sway-session status --s); CURRENT=3; _sway-session
words=(sway-session status --socket --json ''); CURRENT=5; _sway-session
words=(sway-session status -- ''); CURRENT=4
test -z "$(_sway-session)" || exit 1
`
			case "fish":
				script = `source "$STATUS_COMPLETION"
function __sway_session_contexts; echo 'status completion read registry' >&2; exit 1; end
complete -C 'sway-session sta'
complete -C 'sway-session status --s'
complete -C "sway-session status --socket --json $STATUS_FIXTURE/"
set -l terminated (complete -C 'sway-session status -- ')
test (count $terminated) -eq 0; or exit 1
`
			}
			fixture := t.TempDir()
			if err := os.WriteFile(filepath.Join(fixture, "file-completion"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), program, "-c", script)
			command.Env = append(os.Environ(), "STATUS_COMPLETION="+completion, "STATUS_FIXTURE="+fixture)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("%s completion failed: %v\n%s", shell, err, output)
			}
			for _, expected := range []string{"status", "--socket", "file-completion"} {
				if !strings.Contains(string(output), expected) {
					t.Fatalf("%s completion omitted %q: %s", shell, expected, output)
				}
			}
		})
	}
}
