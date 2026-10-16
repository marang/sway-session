package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// Opt in explicitly. Every compositor, application and state root is private;
// only workspaces 98/99 are used. This proves existing-window Reach, not safe
// opening of missing windows or real browser/application session recovery.
func TestReachExistingWindowsHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway Reach integration")
	}
	// Resolve the native fixture programs, never a personal launcher wrapper.
	t.Setenv("PATH", "/usr/bin:/bin")
	for _, name := range []string{"sway", "alacritty", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("explicitly requested Reach integration requires %s: %v", name, err)
		}
	}
	for _, name := range []string{"sway", "alacritty"} {
		version, err := exec.Command(name, "--version").CombinedOutput()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s", strings.TrimSpace(string(version)))
	}
	h := newRestoreCleanupHeadless(t)
	terminalID := testManagedContextID
	missingID := sessionstate.ContextID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	appID := sessionstate.ContextID("33333333-3333-4333-8333-333333333333")
	appIdentity := "diagnostic.LAB375SharedApplication"
	registry := sessionRegistryIDs(terminalID, missingID)
	for index := range registry.Contexts {
		path := filepath.Join(h.root, fmt.Sprintf("worktree-%d", index))
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		registry.Contexts[index].Launcher.Cwd = path
	}
	desktop := filepath.Join(h.root, "data", "LAB375Surrogate.desktop")
	if err := os.WriteFile(desktop, []byte("[Desktop Entry]\nType=Application\nName=LAB-375 private surrogate\nExec=/usr/bin/alacritty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry.Contexts = append(registry.Contexts, sessionstate.Context{
		ID: appID, Label: "Shared application surrogate", Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherDesktop, DesktopID: "LAB375Surrogate.desktop", DesktopOrigin: sessionstate.DesktopEntrySystem, DesktopPath: desktop},
		App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: appIdentity}, DesiredOpen: false, RestorePolicy: sessionstate.ApplicationRestoreFollow},
	})
	if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
		t.Fatal(err)
	}
	h.command("workspace 98")
	terminal := h.terminal(terminalID)
	h.command("workspace 99")
	_, application := startupPromptWindow(t, h, appIdentity)
	store := sessionstate.WorkEnvironmentStoreFor(h.state)
	makeEnvironment := func(name string, primary sessionstate.ContextID) sessionstate.WorkEnvironment {
		t.Helper()
		value, err := store.CreateContext(h.ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range []sessionstate.ContextID{terminalID, appID, missingID} {
			value, err = store.AddMemberContext(h.ctx, value.ID, value.Revision, member)
			if err != nil {
				t.Fatal(err)
			}
		}
		value, err = store.SetPrimaryContext(h.ctx, value.ID, value.Revision, primary)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	development := makeEnvironment("Development worktree", terminalID)
	review := makeEnvironment("Review with shared application", appID)
	definitions, err := store.ListContext(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	requester := &reachHeadlessRequester{Client: h.client}
	launcher := &recordingApplicationLauncher{}
	stream := &swayipc.EventStreamState{}
	compositor, err := compositorIdentity(h.socket)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := newSessionRuntimeWithOptions(requester, sessionRuntimeOptions{
		Context: h.ctx, Root: h.state, EventStreamState: stream, CompositorID: compositor,
		ApplicationLauncher: launcher,
		ApplicationRestore:  sessionstate.ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: time.Second, MaxConcurrent: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime.reach = newReachDispatcher(compositor)
	h.subscribe(runtime, stream)
	h.drain(runtime)
	request := func(value sessionstate.WorkEnvironment, primary sessionstate.ContextID) reachRequest {
		return reachRequest{Version: 1, EnvironmentID: value.ID, Primary: primary, CompositorID: compositor}
	}
	assertUnchanged := func(t *testing.T, placements map[int64]string, before sessionstate.Registry) {
		t.Helper()
		if got := reachHeadlessPlacements(h.tree()); !reflect.DeepEqual(got, placements) {
			t.Fatalf("window IDs/count/placements changed: got=%v want=%v", got, placements)
		}
		var loaded sessionstate.Registry
		if err := sessionstate.RegistryStoreFor(h.state).LoadInto(&loaded); err != nil || !reflect.DeepEqual(loaded, before) {
			t.Fatalf("Reach changed lifecycle/identity/desired-open/paths: %+v %v", loaded, err)
		}
		if loaded, err := store.ListContext(h.ctx); err != nil || !reflect.DeepEqual(loaded, definitions) {
			t.Fatalf("Reach changed definitions: %+v %v", loaded, err)
		}
		if launcher.starts != 0 {
			t.Fatalf("Reach launched an application: %d", launcher.starts)
		}
		for _, command := range requester.commands {
			var id int64
			if _, err := fmt.Sscanf(command, "[con_id=%d] focus", &id); err != nil || command != fmt.Sprintf("[con_id=%d] focus", id) || (id != terminal && id != application) {
				t.Fatalf("Reach issued non-focus/foreign command: %q", command)
			}
		}
	}
	execute := func(t *testing.T, value sessionstate.WorkEnvironment, primary sessionstate.ContextID) sessionstate.ReachResult {
		t.Helper()
		result, err := runtime.executeReach(h.ctx, request(value, primary), h.events)
		if err != nil {
			t.Fatalf("Reach: %+v %v", result, err)
		}
		h.drain(runtime)
		return result
	}
	t.Run("repeat_shared_app_and_explicit_override", func(t *testing.T) {
		placements := reachHeadlessPlacements(h.tree())
		if len(placements) != 2 || placements[terminal] != "98" || placements[application] != "99" {
			t.Fatalf("private fixture escaped high-workspace placement: %v", placements)
		}
		first := execute(t, development, "")
		if reachHeadlessMember(t, first, terminalID).Status != sessionstate.ReachFocused || focusedContainerID(h.tree()) != terminal {
			t.Fatalf("primary not reached: %+v", first)
		}
		if reachHeadlessMember(t, first, appID).Status != sessionstate.ReachAlreadyPresent || reachHeadlessMember(t, first, missingID).Status != sessionstate.ReachBlocked {
			t.Fatalf("shared/missing decisions: %+v", first)
		}
		commands := len(requester.commands)
		for range 3 {
			result := execute(t, development, "")
			if reachHeadlessMember(t, result, terminalID).Status != sessionstate.ReachAlreadyPresent {
				t.Fatalf("repeat not idempotent: %+v", result)
			}
		}
		if len(requester.commands) != commands {
			t.Fatal("repeat issued duplicate focus commands")
		}
		result := execute(t, review, "")
		if reachHeadlessMember(t, result, appID).Status != sessionstate.ReachFocused || focusedContainerID(h.tree()) != application {
			t.Fatalf("shared app not focused in place: %+v", result)
		}
		result = execute(t, review, terminalID)
		if result.Primary != terminalID || focusedContainerID(h.tree()) != terminal {
			t.Fatalf("explicit primary override failed: %+v", result)
		}
		assertUnchanged(t, placements, registry)
		t.Logf("real windows terminal=%d workspace98, application=%d workspace99; repeated/shared/override focus only; missing member deferred; starts=0", terminal, application)
	})
	t.Run("missing_primary_no_start", func(t *testing.T) {
		placements, commands := reachHeadlessPlacements(h.tree()), len(requester.commands)
		result := execute(t, development, missingID)
		if reachHeadlessMember(t, result, missingID).Status != sessionstate.ReachBlocked || len(requester.commands) != commands {
			t.Fatalf("missing primary crossed effect boundary: %+v", result)
		}
		assertUnchanged(t, placements, registry)
	})
	t.Run("archived_primary_no_effect", func(t *testing.T) {
		archived, err := sessionstate.UpdateRegistry(h.state, func(value *sessionstate.Registry) error {
			_, err := sessionstate.SetContextState(value, string(terminalID), sessionstate.ContextArchived)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		placements, commands := reachHeadlessPlacements(h.tree()), len(requester.commands)
		result := execute(t, development, "")
		if reachHeadlessMember(t, result, terminalID).Status != sessionstate.ReachSkipped || len(requester.commands) != commands {
			t.Fatalf("archived primary affected windows: %+v", result)
		}
		assertUnchanged(t, placements, archived)
		if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("real_user_focus_between_observations", func(t *testing.T) {
		h.command(fmt.Sprintf("[con_id=%d] focus", application))
		h.drain(runtime)
		placements, commands := reachHeadlessPlacements(h.tree()), len(requester.commands)
		queued := make(chan swayipc.Event, 256)
		requester.afterTree = func(read int) {
			if read != 1 {
				return
			}
			h.command(fmt.Sprintf("[con_id=%d] focus", terminal))
			// Collect real subscribed events through a compositor tick barrier,
			// then let executeReach consume them at its revalidation boundary.
			barrier := "lab375-user-focus"
			message, err := h.client.RequestContext(h.ctx, swayipc.SendTick, []byte(barrier))
			if err != nil || swayipc.CheckSendTickResponse(message) != nil {
				t.Fatalf("focus barrier: %v", err)
			}
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			focus := false
			for {
				select {
				case event := <-h.events:
					queued <- event
					focus = focus || event.Type == swayipc.EventWindow && event.Change == "focus"
					if event.Type == swayipc.EventTick && event.Payload == barrier {
						if !focus {
							t.Fatal("real user window focus event missing")
						}
						return
					}
				case <-timer.C:
					t.Fatal("real user focus barrier timed out")
				}
			}
		}
		requester.reads = 0
		result, err := runtime.executeReach(h.ctx, request(review, ""), queued)
		requester.afterTree = nil
		if err != nil || reachHeadlessMember(t, result, appID).Status != sessionstate.ReachCanceled || len(requester.commands) != commands || focusedContainerID(h.tree()) != terminal {
			t.Fatalf("user focus did not revoke older Reach: %+v %v commands=%v", result, err, requester.commands[commands:])
		}
		h.drain(runtime)
		assertUnchanged(t, placements, registry)
	})
	t.Run("ambiguous_primary_no_effect", func(t *testing.T) {
		h.command("workspace 98")
		duplicate := h.terminal(terminalID)
		h.drain(runtime)
		placements, commands := reachHeadlessPlacements(h.tree()), len(requester.commands)
		result := execute(t, development, "")
		if reachHeadlessMember(t, result, terminalID).Status != sessionstate.ReachUncertain || len(requester.commands) != commands {
			t.Fatalf("ambiguous primary guessed an anchor: %+v", result)
		}
		assertUnchanged(t, placements, registry)
		t.Logf("duplicate native managed identity windows=%d,%d; no focus command", terminal, duplicate)
	})
}

type reachHeadlessRequester struct {
	*swayipc.Client
	commands  []string
	reads     int
	afterTree func(int)
}

func (client *reachHeadlessRequester) RequestContext(ctx context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	if kind == swayipc.RunCommand {
		client.commands = append(client.commands, string(payload))
	}
	response, err := client.Client.RequestContext(ctx, kind, payload)
	if kind == swayipc.GetTree && err == nil {
		client.reads++
		if client.afterTree != nil {
			client.afterTree(client.reads)
		}
	}
	return response, err
}

func reachHeadlessPlacements(tree *Node) map[int64]string {
	result := make(map[int64]string)
	var visit func(*Node)
	visit = func(node *Node) {
		if node.AppID != nil {
			result[node.ID] = restoreCleanupWorkspace(tree, node.ID)
		}
		for _, child := range append(slices.Clone(node.Nodes), node.FloatingNodes...) {
			visit(child)
		}
	}
	visit(tree)
	return result
}

func reachHeadlessMember(t *testing.T, result sessionstate.ReachResult, id sessionstate.ContextID) sessionstate.ReachMemberResult {
	t.Helper()
	for _, member := range result.Members {
		if member.ContextID == id {
			return member
		}
	}
	t.Fatalf("missing member %s in %+v", id, result)
	return sessionstate.ReachMemberResult{}
}
