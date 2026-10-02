package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestTerminalPurgeProcessDeathRecovery(t *testing.T) {
	for _, scenario := range []struct{ action, point string }{
		{"begin", "before-commit"}, {"begin", "after-commit"}, {"begin", "lose-commit-ack"},
		{"stop", "before-stop"}, {"stop", "after-stop"},
		{"delete", "before-delete"}, {"delete", "after-delete"},
		{"retire", "before-commit"}, {"retire", "after-commit"}, {"retire", "lose-commit-ack"},
	} {
		t.Run(scenario.action+"/"+scenario.point, func(t *testing.T) {
			fixture := newPurgeFixture(t)
			if scenario.action != "begin" {
				fixture.begin(t)
			}
			if scenario.action == "delete" {
				if err := os.Remove(filepath.Join(fixture.sessionPath(), "running")); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.action == "retire" {
				if err := os.RemoveAll(fixture.sessionPath()); err != nil {
					t.Fatal(err)
				}
			}
			if err := lifecycleFixturePublish(filepath.Join(fixture.Base, "purge-case.json"), fixture); err != nil {
				t.Fatal(err)
			}
			runPurgeCrashChild(t, fixture, scenario.action, scenario.point)
			operation, exists, err := FindPendingTerminalPurgeContext(t.Context(), fixture.Root, fixture.Before.ID)
			if err != nil {
				t.Fatal(err)
			}
			abortedBegin := scenario.action == "begin" && scenario.point == "before-commit"
			retired := scenario.action == "retire" && scenario.point != "before-commit"
			if exists == (abortedBegin || retired) {
				t.Fatalf("wrong intent presence after process death: %v", exists)
			}
			if abortedBegin {
				registry := lifecycleStoreTestRegistry(t, fixture.Root)
				if len(registry.Contexts) != 1 {
					t.Fatal("uncommitted begin removed context")
				}
				activity, err := ReadTerminalActivitySnapshot(fixture.Root)
				if err != nil || len(activity.Terminals) != 1 {
					t.Fatalf("uncommitted begin removed activity: %+v %v", activity, err)
				}
				operation = fixture.begin(t)
			} else if exists {
				assertPurgeReserved(t, fixture, operation)
			}
			// Recovery happens in independent processes with fresh managers and FDs.
			// Repeated invocations must neither recreate context nor repeat old effects.
			for range 3 {
				runPurgeCrashChild(t, fixture, "drain", "")
			}
			registry := lifecycleStoreTestRegistry(t, fixture.Root)
			if len(registry.Contexts) != 0 {
				t.Fatal("recovery resurrected context")
			}
			if _, exists, err := FindPendingTerminalPurgeContext(t.Context(), fixture.Root, fixture.Before.ID); err != nil || exists {
				t.Fatalf("recovery retained intent: %v %v", exists, err)
			}
			if _, err := os.Stat(fixture.sessionPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("recovery did not delete original directory: %v", err)
			}
		})
	}
}

func runPurgeCrashChild(t *testing.T, fixture purgeFixture, action, point string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestTerminalPurgeCrashHelper$", "-test.count=1")
	command.Dir = fixture.Base
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "GORACE=atexit_sleep_ms=0", "SWAY_SESSION_PURGE_TEST_BASE=" + fixture.Base, "SWAY_SESSION_PURGE_TEST_ACTION=" + action, "SWAY_SESSION_PURGE_TEST_POINT=" + point}
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR", "XDG_CACHE_HOME", "XDG_DATA_HOME"} {
		command.Env = append(command.Env, key+"="+fixture.Base)
	}
	output, err := command.CombinedOutput()
	if point != "" && point != "lose-commit-ack" {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 77 {
			t.Fatalf("child did not die at %s/%s: %v\n%s", action, point, err, output)
		}
	} else if err != nil {
		t.Fatalf("child %s/%s failed: %v\n%s", action, point, err, output)
	}
}

func TestTerminalPurgeCrashHelper(t *testing.T) {
	base := os.Getenv("SWAY_SESSION_PURGE_TEST_BASE")
	if base == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(base, "purge-case.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture purgeFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(base) || fixture.Base != base || fixture.Root != filepath.Join(base, "state") || fixture.Herdr != filepath.Join(base, "herdr") {
		t.Fatal("requires explicit disposable fixture roots")
	}
	point := os.Getenv("SWAY_SESSION_PURGE_TEST_POINT")
	boundary := func(name string) {
		if name == point {
			os.Exit(77)
		}
	} // No cleanup or defers.
	committed := false
	if point == "before-commit" || point == "after-commit" || point == "lose-commit-ack" {
		original := executeStateCommit
		executeStateCommit = func(tx *stateWriteTransaction) error {
			boundary("before-commit")
			if err := original(tx); err != nil {
				return err
			}
			committed = true
			boundary("after-commit")
			if point == "lose-commit-ack" {
				return errors.New("fixture lost database commit acknowledgement")
			}
			return nil
		}
	}
	manager := fixture.manager(&purgeFixtureRunner{fixture: fixture, boundary: boundary})
	action := os.Getenv("SWAY_SESSION_PURGE_TEST_ACTION")
	if action == "begin" {
		_, err = StartTerminalPurgeContext(t.Context(), fixture.Root, fixture.Before, fixture.Herdr, lifecycleCrashNow)
	} else {
		for range 4 {
			operation, exists, loadErr := FindPendingTerminalPurgeContext(t.Context(), fixture.Root, fixture.Before.ID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if !exists {
				break
			}
			var outcome LifecycleOutcome
			outcome, err = ReconcileLifecycleOperationWithPurgeContext(t.Context(), fixture.Root, operation.ID, nil, manager.DeletePurgeTarget, lifecycleCrashNow.Add(time.Hour))
			if err != nil || outcome.Status == "completed" {
				break
			}
			if outcome.Status != "pending" {
				t.Fatalf("unexpected outcome: %+v", outcome)
			}
			if action != "drain" {
				break
			}
		}
	}
	if point == "lose-commit-ack" {
		if !committed {
			t.Fatal("did not commit before losing acknowledgement")
		}
		if action == "begin" && err != nil {
			t.Fatalf("Start failed to resolve exact committed intent: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if point != "" {
		t.Fatal(fmt.Sprintf("did not reach crash boundary %s", point))
	}
}
