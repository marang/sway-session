package main

import (
	"context"
	"encoding/json"
	"errors"
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

type lifecycleHeadlessCase struct {
	Root, State, Socket string
	Action, Boundary    string
	Before, After       sessionstate.Context
	Container           int64
	OperationID         string
}

// This integration exercises public registration/rebind and their recovery
// against an owned compositor that survives the operation process. It never
// reads SWAYSOCK or connects to the user's compositor.
func TestLifecycleHeadlessApplicationRecovery(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway/Alacritty integration")
	}
	for _, name := range []string{"sway", "alacritty", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("private compositor integration requires %s: %v", name, err)
		}
	}
	for _, action := range []string{"register", "rebind"} {
		points := []string{"before-mark", "after-mark", "lost-ack"}
		if action == "rebind" {
			points = append(points, "before-unmark", "after-unmark")
		}
		for _, point := range points {
			t.Run(action+"/"+point, func(t *testing.T) {
				h := newRestoreCleanupHeadless(t)
				// Window app IDs and registered application context IDs deliberately
				// differ: no terminal context is registered for these sleep windows.
				oldWindowID := sessionstate.ContextID("11111111-1111-4111-8111-111111111111")
				newWindowID := sessionstate.ContextID("22222222-2222-4222-8222-222222222222")
				otherWindowID := sessionstate.ContextID("33333333-3333-4333-8333-333333333333")
				oldContainer := h.terminal(oldWindowID)
				newContainer := h.terminal(newWindowID)
				otherContainer := h.terminal(otherWindowID)
				for _, id := range []int64{oldContainer, newContainer, otherContainer} {
					h.command(fmt.Sprintf("[con_id=%d] mark --add lab135-preserve-%d", id, id))
				}
				registeredID := sessionstate.ContextID("44444444-4444-4444-8444-444444444444")
				before := lifecycleHeadlessApplication(registeredID, oldWindowID, "old.desktop")
				after := lifecycleHeadlessApplication(registeredID, newWindowID, "new.desktop")
				other := lifecycleHeadlessApplication("55555555-5555-4555-8555-555555555555", otherWindowID, "other.desktop")
				registry := sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{other}}
				otherMark, _ := other.ID.Mark()
				h.command(fmt.Sprintf("[con_id=%d] mark --add %s", otherContainer, otherMark))
				mark, _ := registeredID.Mark()
				if action == "rebind" {
					registry.Contexts = append(registry.Contexts, before)
					h.command(fmt.Sprintf("[con_id=%d] mark --add %s", oldContainer, mark))
				}
				if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
					t.Fatal(err)
				}
				initial := lifecycleHeadlessWindows(h.tree())
				fixture := lifecycleHeadlessCase{Root: h.root, State: h.state, Socket: h.socket, Action: action, Boundary: point, Before: before, After: after, Container: newContainer}
				lifecycleHeadlessRunChild(t, h, fixture)
				operations, _, err := sessionstate.ListLifecycleOperationsContext(h.ctx, h.state, "", 10)
				if err != nil {
					t.Fatal(err)
				}
				if point != "lost-ack" {
					if len(operations) != 1 || operations[0].Kind != sessionstate.LifecycleOperationKind(action) {
						t.Fatalf("process death lost pending intent: %+v", operations)
					}
					operation := operations[0]
					fixture.OperationID = operation.ID
					var stillBefore sessionstate.Registry
					if err := sessionstate.RegistryStoreFor(h.state).LoadInto(&stillBefore); err != nil || !reflect.DeepEqual(stillBefore, registry) {
						t.Fatalf("external effect prematurely committed registry: %+v, %v", stillBefore, err)
					}
					// Inspect actual tree state before restarting, so hitting a named
					// boundary is not the sole evidence of which effect survived.
					windows := lifecycleHeadlessWindows(h.tree())
					wantNew := point == "after-mark"
					wantOld := action == "rebind" && point == "before-unmark"
					if slices.Contains(windows[newContainer].Marks, mark) != wantNew || slices.Contains(windows[oldContainer].Marks, mark) != wantOld {
						t.Fatalf("wrong external state at death: old=%v new=%v", windows[oldContainer].Marks, windows[newContainer].Marks)
					}
				} else if len(operations) != 0 {
					t.Fatalf("lost acknowledgement did not converge synchronously: %+v", operations)
				}
				fixture.Action, fixture.Boundary = "resume", ""
				if fixture.OperationID != "" {
					for range 3 {
						lifecycleHeadlessRunChild(t, h, fixture)
					}
				}
				var actual sessionstate.Registry
				if err := sessionstate.RegistryStoreFor(h.state).LoadInto(&actual); err != nil {
					t.Fatal(err)
				}
				want := registry
				want.Contexts = []sessionstate.Context{other, after}
				want.Preferences.DesktopIndicators = action == "register"
				if !reflect.DeepEqual(actual, want) {
					t.Fatalf("real Sway recovery did not converge registry: got=%+v want=%+v", actual, want)
				}
				operations, _, err = sessionstate.ListLifecycleOperationsContext(h.ctx, h.state, "", 10)
				if err != nil || len(operations) != 0 {
					t.Fatalf("completion retained or resurrected operation: %+v, %v", operations, err)
				}
				windows := lifecycleHeadlessWindows(h.tree())
				if len(windows) != len(initial) {
					t.Fatalf("recovery launched or removed windows: before=%d after=%d", len(initial), len(windows))
				}
				for id, old := range initial {
					node := windows[id]
					if node == nil || node.PID != old.PID || !reflect.DeepEqual(node.AppID, old.AppID) || restoreCleanupWorkspace(h.tree(), id) != "98" {
						t.Fatalf("window identity or workspace changed: %d", id)
					}
					wanted := []string{fmt.Sprintf("lab135-preserve-%d", id)}
					if id == newContainer {
						wanted = append(wanted, mark)
					} else if id == otherContainer {
						wanted = append(wanted, otherMark)
					}
					actualMarks := slices.Clone(node.Marks)
					slices.Sort(actualMarks)
					slices.Sort(wanted)
					if !slices.Equal(actualMarks, wanted) {
						t.Errorf("window %d lost unrelated marks or retained stale ownership: %v", id, actualMarks)
					}
				}
			})
		}
	}
}

func lifecycleHeadlessApplication(id, windowID sessionstate.ContextID, desktopID string) sessionstate.Context {
	appID, _ := windowID.AppID()
	return sessionstate.Context{
		ID: id, Label: desktopID, Provider: "desktop", State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherDesktop, DesktopID: desktopID, DesktopOrigin: sessionstate.DesktopEntrySystem, DesktopPath: "/usr/share/applications/" + desktopID},
		App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: appID}, DesiredOpen: true, RestorePolicy: sessionstate.ApplicationRestoreFollow},
	}
}

func lifecycleHeadlessWindows(tree *Node) map[int64]*Node {
	windows := make(map[int64]*Node)
	var visit func(*Node)
	visit = func(node *Node) {
		if node.PID > 0 && node.AppID != nil {
			windows[node.ID] = node
		}
		for _, children := range [][]*Node{node.Nodes, node.FloatingNodes} {
			for _, child := range children {
				visit(child)
			}
		}
	}
	visit(tree)
	return windows
}

func lifecycleHeadlessRunChild(t *testing.T, h *restoreCleanupHeadless, fixture lifecycleHeadlessCase) {
	t.Helper()
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.root, "lifecycle-case.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(h.ctx, executable, "-test.run=^TestLifecycleHeadlessProcessHelper$", "-test.count=1")
	command.Dir = h.root
	command.Env = append(slices.Clone(h.env), "SWAY_SESSION_LIFECYCLE_HEADLESS_CASE="+path, "GORACE=atexit_sleep_ms=0")
	output, err := command.CombinedOutput()
	if fixture.Boundary != "" && fixture.Boundary != "lost-ack" {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 77 {
			t.Fatalf("child did not die at %s: %v\n%s", fixture.Boundary, err, output)
		}
	} else if err != nil {
		t.Fatalf("private Sway child %s: %v\n%s", fixture.Action, err, output)
	}
}

// Deliberately expose only RequestContext and identity: promoting PinCompositor
// from the transport would bypass this test's effect/acknowledgement boundary.
type lifecycleHeadlessRequester struct {
	client *swayipc.Client
	point  string
}

func (client lifecycleHeadlessRequester) LifecycleCompositorID(ctx context.Context) (string, error) {
	return client.client.LifecycleCompositorID(ctx)
}

func (client lifecycleHeadlessRequester) RequestContext(ctx context.Context, kind swayipc.MessageType, payload []byte) (swayipc.Message, error) {
	verb := ""
	if kind == swayipc.RunCommand {
		command := string(payload)
		if !strings.HasPrefix(command, "[con_id=") || strings.ContainsAny(command, ";\n") {
			return swayipc.Message{}, fmt.Errorf("unexpected lifecycle command %q", command)
		}
		switch {
		case strings.Contains(command, "] mark --add persist:"):
			verb = "mark"
		case strings.Contains(command, "] unmark persist:"):
			verb = "unmark"
		default:
			return swayipc.Message{}, fmt.Errorf("lifecycle attempted a non-mark effect: %q", command)
		}
		if client.point == "before-"+verb {
			os.Exit(77)
		}
	}
	message, err := client.client.RequestContext(ctx, kind, payload)
	if err == nil && verb != "" {
		if err := swayipc.CheckRunCommandResponse(message); err != nil {
			return message, err
		}
		if client.point == "after-"+verb {
			os.Exit(77)
		}
		if client.point == "lost-ack" {
			return swayipc.Message{}, &swayipc.CommandOutcomeUnknownError{Cause: errors.New("fixture dropped real Sway acknowledgement")}
		}
	}
	return message, err
}

func TestLifecycleHeadlessProcessHelper(t *testing.T) {
	path := os.Getenv("SWAY_SESSION_LIFECYCLE_HEADLESS_CASE")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture lifecycleHeadlessCase
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(fixture.Root) || path != filepath.Join(fixture.Root, "lifecycle-case.json") || fixture.State != filepath.Join(fixture.Root, "state", "sway-session") || filepath.Dir(fixture.Socket) != filepath.Join(fixture.Root, "run") {
		t.Fatal("headless subprocess requires explicit private paths")
	}
	transport := swayipc.NewClient(fixture.Socket)
	defer transport.Close()
	pinned, err := transport.PinCompositor(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	client := lifecycleHeadlessRequester{client: pinned, point: fixture.Boundary}
	switch fixture.Action {
	case "register":
		err = sessionstate.RegisterApplicationContext(t.Context(), fixture.State, client, fixture.After, fixture.Container)
	case "rebind":
		_, _, err = sessionstate.RebindApplicationContext(t.Context(), fixture.State, client, fixture.Before, fixture.After, fixture.Container)
	case "resume":
		for pass := range 8 {
			outcome, stepErr := sessionstate.ReconcileLifecycleOperationContext(t.Context(), fixture.State, fixture.OperationID, client, time.Now().Add(time.Duration(pass)*time.Hour))
			if stepErr != nil {
				t.Fatal(stepErr)
			}
			if outcome.Status == "completed" {
				return
			}
		}
		t.Fatal("private Sway operation failed to converge")
	default:
		t.Fatalf("unknown headless fixture action %q", fixture.Action)
	}
	if err != nil {
		t.Fatal(err)
	}
	if fixture.Boundary != "lost-ack" {
		t.Fatalf("did not reach requested process death boundary %q", fixture.Boundary)
	}
}
