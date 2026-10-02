package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/swayipc"
)

var lifecycleCrashNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

type lifecycleCrashCase struct {
	Base      string             `json:"base"`
	Root      string             `json:"root"`
	Sway      string             `json:"sway"`
	Operation LifecycleOperation `json:"operation"`
	Unrelated Context            `json:"unrelated"`
}

func lifecycleCrashApplication(t *testing.T, kind LifecycleOperationKind) lifecycleCrashCase {
	t.Helper()
	base := lifecycleFixtureDir(t)
	fixture := lifecycleCrashCase{Base: base, Root: filepath.Join(base, "state"), Sway: filepath.Join(base, "sway")}
	after := flatpakApplicationContext("org.example.New", "org.example.New")
	after.ID, after.Label, after.App.DesiredOpen = testContextID, "new application", true
	unrelated := flatpakApplicationContext("org.example.Unrelated", "org.example.Unrelated")
	unrelated.ID, unrelated.Label = thirdContextID, "unrelated application"
	fixture.Unrelated = unrelated
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{unrelated}}
	newWindow := appWindow(43, true, "org.example.New", "", "", "org.example.New")
	newWindow.PID = 4300
	newWindow.Marks = []string{"user-new-window"}
	unrelatedWindow := appWindow(84, false, "org.example.Unrelated", "", "", "org.example.Unrelated")
	unrelatedWindow.PID = 8400
	unrelatedMark, _ := unrelated.ID.Mark()
	unrelatedWindow.Marks = []string{"user-unrelated-window", unrelatedMark}
	windows := []*swayipc.TreeNode{newWindow, unrelatedWindow}
	operation := LifecycleOperation{
		ID: restoreTestRun, Version: LifecycleOperationVersion, Kind: kind, Phase: LifecycleForward,
		Before: []Context{}, After: []Context{after}, Targets: []LifecycleWindowTarget{},
		CompositorID: strings.Repeat("a", 64), UpdatedAt: lifecycleCrashNow, NextAttempt: lifecycleCrashNow,
	}
	if kind == LifecycleRebind {
		before := flatpakApplicationContext("org.example.Old", "org.example.Old")
		before.ID, before.Label, before.App.DesiredOpen = testContextID, "old application", true
		registry.Contexts = append(registry.Contexts, before)
		operation.Before = []Context{before}
		oldWindow := appWindow(42, false, "org.example.Old", "", "", "org.example.Old")
		oldWindow.PID = 4200
		mark, _ := before.ID.Mark()
		oldWindow.Marks = []string{"user-old-window", mark}
		windows = append(windows, oldWindow)
		operation.Targets = append(operation.Targets, LifecycleWindowTarget{
			ContextID: before.ID, ContainerID: oldWindow.ID, Identity: before.App.Identity,
			PID: oldWindow.PID, HadMark: true, WantMark: false,
		})
	}
	operation.Targets = append(operation.Targets, LifecycleWindowTarget{
		ContextID: after.ID, ContainerID: newWindow.ID, Identity: after.App.Identity,
		PID: newWindow.PID, HadMark: false, WantMark: true,
	})
	fixture.Operation = operation
	if err := RegistryStoreFor(fixture.Root).Save(registry); err != nil {
		t.Fatal(err)
	}
	lifecycleFixtureNewSway(t, fixture.Sway, windows...)
	lifecycleCrashSaveCase(t, fixture)
	return fixture
}

func lifecycleCrashSaveCase(t *testing.T, fixture lifecycleCrashCase) {
	t.Helper()
	if err := lifecycleFixturePublish(filepath.Join(fixture.Base, "case.json"), fixture); err != nil {
		t.Fatal(err)
	}
}

func lifecycleCrashBegin(t *testing.T, fixture lifecycleCrashCase) {
	t.Helper()
	if _, err := BeginLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation); err != nil {
		t.Fatalf("begin %s: %v", fixture.Operation.Kind, err)
	}
}

func lifecycleCrashStep(t *testing.T, fixture lifecycleCrashCase, client SwayRequestClient) LifecycleOutcome {
	t.Helper()
	result, err := ReconcileLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation.ID, client, lifecycleCrashNow.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("reconcile %s: %+v, %v", fixture.Operation.Kind, result, err)
	}
	return result
}

func lifecycleCrashAssertApplication(t *testing.T, fixture lifecycleCrashCase, rollback bool) {
	t.Helper()
	var actual Registry
	if err := RegistryStoreFor(fixture.Root).LoadInto(&actual); err != nil {
		t.Fatal(err)
	}
	want := Registry{Version: ContextsSchemaVersion, Contexts: []Context{fixture.Unrelated}}
	if rollback {
		want.Contexts = append(want.Contexts, fixture.Operation.Before...)
	} else {
		want.Contexts = append(want.Contexts, fixture.Operation.After...)
		want.Preferences.DesktopIndicators = fixture.Operation.Kind == LifecycleRegister
	}
	if !reflect.DeepEqual(actual, want) {
		t.Errorf("registry did not converge: got=%+v want=%+v", actual, want)
	}
	if _, err := LoadLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation.ID); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("completed operation was retained or recreated: %v", err)
	}
	world := lifecycleFixtureReadWorld(t, &lifecycleFixtureSway{root: fixture.Sway})
	for _, target := range fixture.Operation.Targets {
		node := lifecycleFixtureNode(world.Tree, target.ContainerID)
		mark, _ := target.ContextID.Mark()
		marked := target.WantMark
		if rollback {
			marked = target.HadMark
		}
		if node == nil || slices.Contains(node.Marks, mark) != marked {
			t.Errorf("target %d mark did not converge to %v: %+v", target.ContainerID, marked, node)
		}
		userMark := "user-new-window"
		if target.ContainerID == 42 {
			userMark = "user-old-window"
		}
		if node == nil || !slices.Contains(node.Marks, userMark) {
			t.Errorf("target %d lost its unrelated user mark", target.ContainerID)
		}
	}
	unrelated := lifecycleFixtureNode(world.Tree, 84)
	unrelatedMark, _ := fixture.Unrelated.ID.Mark()
	if unrelated == nil || !reflect.DeepEqual(unrelated.Marks, []string{"user-unrelated-window", unrelatedMark}) {
		t.Errorf("unrelated window changed: %+v", unrelated)
	}
	windows := world.Tree.Nodes[0].Nodes[0].Nodes
	if len(windows) != len(fixture.Operation.Targets)+1 {
		t.Errorf("recovery created or removed windows: %d", len(windows))
	}
	seen := make(map[string]bool)
	for _, effect := range world.Effects {
		key := fmt.Sprintf("%d/%s/%s", effect.ContainerID, effect.Kind, effect.Mark)
		if seen[key] || !effect.Changed || effect.ContainerID == 84 {
			t.Errorf("recovery replayed an acknowledged effect or touched an unrelated target: %+v", effect)
		}
		seen[key] = true
	}
}

func TestLifecycleCrashApplicationEffectsSurviveProcessDeath(t *testing.T) {
	for _, kind := range []LifecycleOperationKind{LifecycleRegister, LifecycleRebind} {
		for _, rollback := range []bool{false, true} {
			points := []string{"before-mark", "after-mark"}
			if kind == LifecycleRebind {
				points = append(points, "before-unmark", "after-unmark")
			} else if rollback {
				points = []string{"before-unmark", "after-unmark"}
			}
			for _, point := range points {
				t.Run(fmt.Sprintf("%s/rollback=%v/%s", kind, rollback, point), func(t *testing.T) {
					fixture := lifecycleCrashApplication(t, kind)
					lifecycleCrashBegin(t, fixture)
					if rollback {
						client := &lifecycleFixtureSway{root: fixture.Sway}
						for range len(fixture.Operation.Targets) {
							if result := lifecycleCrashStep(t, fixture, client); !result.Effects || result.Status == "completed" {
								t.Fatalf("fixture completed before rollback could be requested: %+v", result)
							}
						}
						if err := CancelLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation.ID, lifecycleCrashNow.Add(time.Hour)); err != nil {
							t.Fatal(err)
						}
					}
					lifecycleCrashRunChild(t, fixture, "drain", point, false)
					if _, err := LoadLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation.ID); err != nil {
						t.Fatalf("death discarded durable intent: %v", err)
					}
					for range 3 {
						lifecycleCrashRunChild(t, fixture, "drain", "", false)
					}
					lifecycleCrashAssertApplication(t, fixture, rollback)
				})
			}
		}
	}
}

func TestLifecycleCrashLostMarkAcknowledgementsAreObserved(t *testing.T) {
	for _, kind := range []LifecycleOperationKind{LifecycleRegister, LifecycleRebind} {
		t.Run(string(kind), func(t *testing.T) {
			fixture := lifecycleCrashApplication(t, kind)
			lifecycleCrashBegin(t, fixture)
			lifecycleCrashRunChild(t, fixture, "drain", "", true)
			lifecycleCrashRunChild(t, fixture, "drain", "", true)
			lifecycleCrashAssertApplication(t, fixture, false)
		})
	}
}

// Prime journal storage through its public operation API so commit interception
// below targets Begin itself, not optional first-use table initialization.
func lifecycleCrashPrimeJournal(t *testing.T, fixture lifecycleCrashCase) {
	t.Helper()
	lifecycleCrashBegin(t, fixture)
	if err := CancelLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation.ID, lifecycleCrashNow); err != nil {
		t.Fatal(err)
	}
	if result := lifecycleCrashStep(t, fixture, &lifecycleFixtureSway{root: fixture.Sway}); result.Status != "completed" || result.Effects {
		t.Fatalf("journal initialization changed external state: %+v", result)
	}
}

func TestLifecycleCrashDurableBeginPhaseAndCompletion(t *testing.T) {
	for _, kind := range []LifecycleOperationKind{LifecycleRegister, LifecycleRebind} {
		for _, action := range []string{"begin", "cancel", "complete"} {
			for _, point := range []string{"before-commit", "after-commit"} {
				t.Run(fmt.Sprintf("%s/%s/%s", kind, action, point), func(t *testing.T) {
					fixture := lifecycleCrashApplication(t, kind)
					if action == "begin" {
						lifecycleCrashPrimeJournal(t, fixture)
					} else {
						lifecycleCrashBegin(t, fixture)
						for range len(fixture.Operation.Targets) {
							lifecycleCrashStep(t, fixture, &lifecycleFixtureSway{root: fixture.Sway})
						}
					}
					worldBefore := lifecycleFixtureReadWorld(t, &lifecycleFixtureSway{root: fixture.Sway})
					lifecycleCrashRunChild(t, fixture, action, point, false)
					worldAfter := lifecycleFixtureReadWorld(t, &lifecycleFixtureSway{root: fixture.Sway})
					if !reflect.DeepEqual(worldBefore, worldAfter) {
						t.Fatal("durable transition performed an external effect")
					}
					operation, err := LoadLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation.ID)
					absent := action == "begin" && point == "before-commit" || action == "complete" && point == "after-commit"
					if absent && !errors.Is(err, os.ErrNotExist) || !absent && err != nil {
						t.Fatalf("wrong durable outcome after %s/%s: %+v, %v", action, point, operation, err)
					}
					rollback := action == "cancel" && point == "after-commit"
					if !absent && (operation.Phase == LifecycleRollback) != rollback {
						t.Fatalf("wrong phase after interrupted cancellation: %+v", operation)
					}
					if action == "begin" && absent {
						lifecycleCrashRunChild(t, fixture, "begin", "", false)
					}
					for range 2 {
						lifecycleCrashRunChild(t, fixture, "drain", "", false)
					}
					lifecycleCrashAssertApplication(t, fixture, rollback)
				})
			}
		}
	}
}

func TestLifecycleCrashUncertainFinalCommitNeverResurrectsIntent(t *testing.T) {
	for _, kind := range []LifecycleOperationKind{LifecycleRegister, LifecycleRebind} {
		t.Run(string(kind), func(t *testing.T) {
			fixture := lifecycleCrashApplication(t, kind)
			lifecycleCrashBegin(t, fixture)
			for range len(fixture.Operation.Targets) {
				lifecycleCrashStep(t, fixture, &lifecycleFixtureSway{root: fixture.Sway})
			}
			lifecycleCrashRunChild(t, fixture, "complete", "lose-commit-ack", false)
			for range 3 {
				lifecycleCrashRunChild(t, fixture, "drain", "", false)
			}
			lifecycleCrashAssertApplication(t, fixture, false)
		})
	}
}

func TestLifecycleCrashReusedWindowIdentityBlocksRecovery(t *testing.T) {
	for _, drift := range []string{"compositor", "pid", "application", "foreign mark", "mark moved to unrelated window"} {
		t.Run(drift, func(t *testing.T) {
			fixture := lifecycleCrashApplication(t, LifecycleRegister)
			lifecycleCrashBegin(t, fixture)
			lifecycleCrashRunChild(t, fixture, "drain", "after-mark", false)
			client := &lifecycleFixtureSway{root: fixture.Sway}
			if err := client.access(t.Context(), func(world *lifecycleFixtureWorld) (bool, error) {
				node := lifecycleFixtureNode(world.Tree, 43)
				switch drift {
				case "compositor":
					world.Compositor = strings.Repeat("b", 64)
				case "pid":
					node.PID++
				case "application":
					appID := "org.example.Reused"
					node.AppID, node.SandboxAppID = &appID, &appID
				case "foreign mark":
					mark, _ := secondContextID.Mark()
					node.Marks = append(node.Marks, mark)
				case "mark moved to unrelated window":
					mark, _ := testContextID.Mark()
					node.Marks = slices.DeleteFunc(node.Marks, func(value string) bool { return value == mark })
					other := lifecycleFixtureNode(world.Tree, 84)
					other.Marks = append(other.Marks, mark)
				}
				return true, nil
			}); err != nil {
				t.Fatal(err)
			}
			before := lifecycleFixtureReadWorld(t, client)
			result, err := ReconcileLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation.ID, client, lifecycleCrashNow.Add(time.Hour))
			if !errors.Is(err, ErrLifecycleOperationConflict) || result.Status != "conflict" || result.OperationID != fixture.Operation.ID {
				t.Fatalf("reused target was not an actionable conflict: %+v, %v", result, err)
			}
			operation, err := LoadLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation.ID)
			if err != nil || !operation.Blocked || operation.Reason == "" {
				t.Fatalf("conflict was not durable: %+v, %v", operation, err)
			}
			for range 2 {
				result = lifecycleCrashStep(t, fixture, client)
				if result.Status != "conflict" {
					t.Errorf("repeated recovery lost the conflict: %+v", result)
				}
			}
			if after := lifecycleFixtureReadWorld(t, client); !reflect.DeepEqual(before, after) {
				t.Error("conflict recovery changed foreign marks or reused windows")
			}
			var registry Registry
			if err := RegistryStoreFor(fixture.Root).LoadInto(&registry); err != nil || len(registry.Contexts) != 1 || !reflect.DeepEqual(registry.Contexts[0], fixture.Unrelated) {
				t.Fatalf("conflicted registration changed the registry: %+v, %v", registry, err)
			}
		})
	}
}

func TestLifecycleCrashConcurrentProcessRetries(t *testing.T) {
	for _, kind := range []LifecycleOperationKind{LifecycleRegister, LifecycleRebind} {
		t.Run(string(kind), func(t *testing.T) {
			fixture := lifecycleCrashApplication(t, kind)
			lifecycleCrashBegin(t, fixture)
			lifecycleCrashRunChild(t, fixture, "drain", "after-mark", false)
			const workers = 4
			var commands [workers]*exec.Cmd
			var outputs [workers]bytes.Buffer
			var gates [workers]*os.File
			for index := range workers {
				reader, writer, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
				command := lifecycleCrashCommand(t, fixture, "drain", "", true)
				command.ExtraFiles = []*os.File{reader}
				command.Env = append(command.Env, "SWAY_SESSION_LIFECYCLE_TEST_GATE=1")
				command.Stdout, command.Stderr = &outputs[index], &outputs[index]
				if err := command.Start(); err != nil {
					t.Fatal(err)
				}
				_ = reader.Close()
				commands[index], gates[index] = command, writer
			}
			for _, gate := range gates {
				if _, err := gate.Write([]byte{1}); err != nil {
					t.Fatal(err)
				}
				_ = gate.Close()
			}
			for index, command := range commands {
				if err := command.Wait(); err != nil {
					t.Errorf("retry process %d: %v\n%s", index, err, outputs[index].String())
				}
			}
			lifecycleCrashAssertApplication(t, fixture, false)
		})
	}
}

func TestLifecycleCrashCancellationWaitsForInFlightEffect(t *testing.T) {
	fixture := lifecycleCrashApplication(t, LifecycleRegister)
	lifecycleCrashBegin(t, fixture)
	entered, release := make(chan struct{}), make(chan struct{})
	client := &lifecycleFixtureSway{root: fixture.Sway, boundary: func(point string) {
		if point == "after-mark" {
			close(entered)
			<-release
		}
	}}
	done := make(chan error, 1)
	go func() {
		_, err := ReconcileLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation.ID, client, lifecycleCrashNow)
		done <- err
	}()
	// Cleanup releases and joins even when an assertion fails while the effect
	// caller holds the process-shared lifecycle and registry locks.
	var released sync.Once
	joined := false
	defer func() {
		released.Do(func() { close(release) })
		if !joined {
			<-done
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("effect did not reach the acknowledgement barrier")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	err := CancelLifecycleOperationContext(ctx, fixture.Root, fixture.Operation.ID, lifecycleCrashNow)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel crossed an in-flight effect: %v", err)
	}
	// Permit the pending call to finish, then test a successful cancellation.
	released.Do(func() { close(release) })
	err = <-done
	joined = true
	if err != nil {
		t.Fatal(err)
	}
	operation, err := LoadLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation.ID)
	if err != nil || operation.Phase != LifecycleForward {
		t.Fatalf("cancelled waiter changed durable phase: %+v, %v", operation, err)
	}
	if err := CancelLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation.ID, lifecycleCrashNow); err != nil {
		t.Fatal(err)
	}
	lifecycleCrashRunChild(t, fixture, "drain", "", false)
	lifecycleCrashAssertApplication(t, fixture, true)
}

func TestLifecycleCrashBusyBeginAndCancelledCallsHaveNoEffects(t *testing.T) {
	fixture := lifecycleCrashApplication(t, LifecycleRegister)
	before := lifecycleFixtureReadWorld(t, &lifecycleFixtureSway{root: fixture.Sway})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := BeginLifecycleOperationContext(ctx, fixture.Root, fixture.Operation); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Begin: %v", err)
	}
	database, err := openStateDatabase(t.Context(), fixture.Root, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	writer, err := beginStateWrite(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Rollback() })
	busy, stop := context.WithTimeout(t.Context(), 50*time.Millisecond)
	_, err = BeginLifecycleOperationContext(busy, fixture.Root, fixture.Operation)
	stop()
	if err == nil {
		t.Fatal("Begin succeeded despite another SQLite writer")
	}
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed Begin published intent: %v", err)
	}
	lifecycleCrashBegin(t, fixture)
	if _, err := ReconcileLifecycleOperationContext(ctx, fixture.Root, fixture.Operation.ID, &lifecycleFixtureSway{root: fixture.Sway}, lifecycleCrashNow); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reconcile: %v", err)
	}
	if after := lifecycleFixtureReadWorld(t, &lifecycleFixtureSway{root: fixture.Sway}); !reflect.DeepEqual(before, after) {
		t.Fatal("failed or cancelled calls changed external state")
	}
	lifecycleCrashRunChild(t, fixture, "drain", "", false)
	lifecycleCrashAssertApplication(t, fixture, false)
}

func TestLifecycleCrashBlockedOperationDoesNotStarveAnother(t *testing.T) {
	fixture := lifecycleCrashApplication(t, LifecycleRegister)
	lifecycleCrashBegin(t, fixture)
	second := fixture.Operation
	second.ID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	registered := flatpakApplicationContext("org.example.Second", "org.example.Second")
	registered.ID, registered.Label = secondContextID, "independent application"
	second.After = []Context{registered}
	second.Targets = []LifecycleWindowTarget{{ContextID: registered.ID, ContainerID: 144, PID: 14400, Identity: registered.App.Identity, WantMark: true}}
	client := &lifecycleFixtureSway{root: fixture.Sway}
	if err := client.access(t.Context(), func(world *lifecycleFixtureWorld) (bool, error) {
		// The first operation's window was replaced while its caller was dead.
		lifecycleFixtureNode(world.Tree, 43).PID++
		node := appWindow(144, false, "org.example.Second", "", "", "org.example.Second")
		node.PID = 14400
		workspace := world.Tree.Nodes[0].Nodes[0]
		workspace.Nodes = append(workspace.Nodes, node)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginLifecycleOperationContext(t.Context(), fixture.Root, second); err != nil {
		t.Fatal(err)
	}
	page, err := ReconcileLifecycleOperationsContext(t.Context(), fixture.Root, client, lifecycleCrashNow, "", 1)
	if !errors.Is(err, ErrLifecycleOperationConflict) || page.Processed != 1 || page.NextID != fixture.Operation.ID || page.Effects {
		t.Fatalf("conflicted page did not retain a bounded continuation: %+v, %v", page, err)
	}
	cursor := page.NextID
	for pass := range 2 {
		page, err = ReconcileLifecycleOperationsContext(t.Context(), fixture.Root, client, lifecycleCrashNow, cursor, 1)
		if err != nil || page.Processed != 1 || len(page.Outcomes) != 1 || page.Outcomes[0].OperationID != second.ID {
			t.Fatalf("unrelated operation could not progress past conflict: %+v, %v", page, err)
		}
		wantStatus := "pending"
		if pass == 1 {
			wantStatus = "completed"
		}
		if page.Outcomes[0].Status != wantStatus || page.Effects != (pass == 0) {
			t.Fatalf("page did more than one effect or failed to commit: %+v", page)
		}
	}
	blocked, err := LoadLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation.ID)
	if err != nil || !blocked.Blocked {
		t.Fatalf("unrelated completion discarded first operation: %+v, %v", blocked, err)
	}
	if _, err := LoadLifecycleOperationContext(t.Context(), fixture.Root, second.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second operation was not retired: %v", err)
	}
	var registry Registry
	if err := RegistryStoreFor(fixture.Root).LoadInto(&registry); err != nil || !reflect.DeepEqual(registry.Contexts, []Context{fixture.Unrelated, registered}) {
		t.Fatalf("page committed an unrelated pending context: %+v, %v", registry, err)
	}
	world := lifecycleFixtureReadWorld(t, client)
	if len(world.Effects) != 1 || world.Effects[0].ContainerID != 144 || !world.Effects[0].Changed {
		t.Fatalf("bounded pages replayed effects or touched the conflicted window: %+v", world.Effects)
	}
}

func lifecycleCrashCommand(t *testing.T, fixture lifecycleCrashCase, action, point string, loseAck bool) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, executable, "-test.run=^TestLifecycleCrashProcessHelper$", "-test.count=1")
	command.Dir = fixture.Base
	command.Env = []string{
		"PATH=/usr/bin:/bin", "LANG=C.UTF-8",
		"GORACE=atexit_sleep_ms=0",
		"SWAY_SESSION_LIFECYCLE_TEST_BASE=" + fixture.Base,
		"SWAY_SESSION_LIFECYCLE_TEST_ACTION=" + action,
		"SWAY_SESSION_LIFECYCLE_TEST_POINT=" + point,
		fmt.Sprintf("SWAY_SESSION_LIFECYCLE_TEST_LOSE_ACK=%v", loseAck),
	}
	for _, name := range []string{"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME"} {
		root := filepath.Join(fixture.Base, strings.ToLower(name))
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		command.Env = append(command.Env, name+"="+root)
	}
	return command
}

func lifecycleCrashRunChild(t *testing.T, fixture lifecycleCrashCase, action, point string, loseAck bool) {
	t.Helper()
	command := lifecycleCrashCommand(t, fixture, action, point, loseAck)
	output, err := command.CombinedOutput()
	if point != "" && point != "lose-commit-ack" {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 77 {
			t.Fatalf("child did not die at %s/%s: %v\n%s", action, point, err, output)
		}
	} else if err != nil {
		t.Fatalf("child %s failed: %v\n%s", action, err, output)
	}
}

func TestLifecycleCrashProcessHelper(t *testing.T) {
	base := os.Getenv("SWAY_SESSION_LIFECYCLE_TEST_BASE")
	if base == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(base, "case.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture lifecycleCrashCase
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Base != base || fixture.Root != filepath.Join(base, "state") || fixture.Sway != filepath.Join(base, "sway") || !filepath.IsAbs(base) {
		t.Fatal("child requires explicit owned fixture paths")
	}
	if os.Getenv("SWAY_SESSION_LIFECYCLE_TEST_GATE") == "1" {
		gate := os.NewFile(3, "lifecycle start barrier")
		if _, err := io.ReadFull(gate, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		_ = gate.Close()
	}
	point := os.Getenv("SWAY_SESSION_LIFECYCLE_TEST_POINT")
	boundary := func(name string) {
		if name == point {
			os.Exit(77) // No defers: SQLite, lifecycle locks and fake effects survive independently.
		}
	}
	client := &lifecycleFixtureSway{root: fixture.Sway, boundary: boundary, loseAck: os.Getenv("SWAY_SESSION_LIFECYCLE_TEST_LOSE_ACK") == "true"}
	committed := false
	if point == "before-commit" || point == "after-commit" || point == "lose-commit-ack" {
		originalCommit := executeStateCommit
		executeStateCommit = func(tx *stateWriteTransaction) error {
			boundary("before-commit")
			if err := originalCommit(tx); err != nil {
				return err
			}
			committed = true
			boundary("after-commit")
			if point == "lose-commit-ack" {
				return errors.New("fixture lost acknowledgement after SQLite commit")
			}
			return nil
		}
	}
	action := os.Getenv("SWAY_SESSION_LIFECYCLE_TEST_ACTION")
	switch action {
	case "begin":
		_, err = BeginLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation)
	case "cancel":
		err = CancelLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation.ID, lifecycleCrashNow.Add(time.Hour))
	case "complete":
		_, err = ReconcileLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation.ID, client, lifecycleCrashNow.Add(24*time.Hour))
	case "drain":
		for pass := range 12 {
			var result LifecycleOutcome
			result, err = ReconcileLifecycleOperationContext(t.Context(), fixture.Root, fixture.Operation.ID, client, lifecycleCrashNow.Add(time.Duration(24+pass)*time.Hour))
			if err != nil || result.Status == "completed" {
				break
			}
			if result.Status != "pending" {
				t.Fatalf("unexpected reconciliation outcome: %+v", result)
			}
			if pass == 11 {
				t.Fatal("recovery did not converge in bounded passes")
			}
		}
	default:
		t.Fatalf("unknown lifecycle child action %q", action)
	}
	if point == "lose-commit-ack" {
		if !committed {
			t.Fatal("fixture never committed before dropping its acknowledgement")
		}
		// Callers may report an uncertain outcome; the parent checks durable
		// state and retries independently, rather than trusting that response.
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if point != "" {
		t.Fatalf("child returned without reaching %s", point)
	}
}
