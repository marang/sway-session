package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

func TestRestorePreviewMatchesPolicyWithoutMutation(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 2)
	root, _ := deps.stateRoot()
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	deps.now = func() time.Time { return now }
	if _, err := sessionstate.SetContextStateAt(&registry, string(registry.Contexts[1].ID), sessionstate.ContextArchived, now); err != nil {
		t.Fatal(err)
	}
	for i, policy := range []sessionstate.ApplicationRestorePolicy{sessionstate.ApplicationRestoreFollow, sessionstate.ApplicationRestorePinned} {
		registry.Contexts = append(registry.Contexts, sessionstate.Context{
			ID: sessionstate.ContextID([]string{"22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"}[i]), Label: string(policy), State: sessionstate.ContextActive,
			Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example." + string(policy), FlatpakInstallation: sessionstate.FlatpakUser},
			App:      &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example." + string(policy), SandboxAppID: "org.example." + string(policy)}, DesiredOpen: i == 1, RestorePolicy: policy},
		})
	}
	saveTestRegistry(t, root, registry)
	client := &terminalManageObservationRequester{tree: treeWithContexts(registry.Contexts[0].ID)}
	deps.newSwayClient = func(string) swayRequester { return client }
	deps.resolveProgram = func(string) (string, error) { t.Fatal("preview resolved launcher"); return "", nil }
	deps.findPendingProcess = func(string, sessionstate.ProcessSpec) ([]int, error) {
		t.Fatal("preview inspected launch process")
		return nil, nil
	}
	result, failure := executeRestore(t.Context(), []string{"--preview", "--socket", "/fake/sway.sock"}, deps)
	if failure != nil {
		t.Fatalf("preview failed: %+v", failure)
	}
	if !result.Preview || result.RestorePreview == nil || len(result.RestorePreview.Contexts) != 4 {
		t.Fatalf("missing preview: %+v", result)
	}
	byID := map[sessionstate.ContextID]restorePreviewItem{}
	for _, item := range result.RestorePreview.Contexts {
		byID[item.ContextID] = item
	}
	for _, value := range registry.Contexts {
		item := byID[value.ID]
		if item.Policy != sessionstate.EvaluateRestorePolicy(value) || (item.Status == "eligible") != item.Policy.Eligible {
			t.Fatalf("policy mismatch: %+v", item)
		}
	}
	if byID[registry.Contexts[0].ID].Window != "open" || byID[registry.Contexts[1].ID].Lifecycle == nil {
		t.Fatal("window or lifecycle evidence missing")
	}
	actual, err := restoreTargets(registry, "", false)
	if err != nil || len(actual) != 1 || actual[0].ID != registry.Contexts[0].ID {
		t.Fatalf("terminal selection differs: %+v %v", actual, err)
	}
	coordinator, err := sessionstate.NewApplicationRestoreCoordinator(strings.Repeat("a", 64), sessionstate.ApplicationSessionState{}, now.Add(-time.Minute), sessionstate.ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: time.Second, MaxConcurrent: 2})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := coordinator.Plan(registry, map[sessionstate.ContextID]sessionstate.ApplicationGroup{}, now)
	if err != nil || len(plan.Launch) != 1 || plan.Launch[0].ID != registry.Contexts[3].ID {
		t.Fatalf("app selection differs: %+v %v", plan, err)
	}
	loaded, err := sessionstate.ReadRegistrySnapshot(root)
	if err != nil || !reflect.DeepEqual(loaded, registry) {
		t.Fatalf("preview mutated registry: %v", err)
	}
	terminalManageRequireObservationOnly(t, client)
	var output bytes.Buffer
	if err := writeResult(&output, false, result); err != nil || !strings.Contains(output.String(), "successful launch") || !strings.Contains(output.String(), "archived") {
		t.Fatalf("human explanation missing: %s %v", output.String(), err)
	}
}

func TestRestorePreviewUnavailableCompositorKeepsPolicyAndRedactsErrors(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 2)
	registry.Contexts[1].State = sessionstate.ContextArchived
	root, _ := deps.stateRoot()
	saveTestRegistry(t, root, registry)
	deps.newSwayClient = nil
	result, failure := executeRestore(t.Context(), []string{"--preview"}, deps)
	if failure != nil {
		t.Fatalf("saved policy unavailable: %+v", failure)
	}
	for _, item := range result.RestorePreview.Contexts {
		want := "uncertain"
		if !item.Policy.Eligible {
			want = "skipped"
		}
		if item.Status != want || item.Window != "unknown" || item.Reason != "compositor_unavailable" {
			t.Fatalf("misleading evidence: %+v", item)
		}
	}
	deps.newSwayClient = func(string) swayRequester {
		return &terminalManageObservationRequester{err: errors.New("PRIVATE-SOCKET-DETAILS")}
	}
	result, failure = executeRestore(t.Context(), []string{"--preview", "--socket", "/fake/sway.sock"}, deps)
	if failure != nil {
		t.Fatal("observation failure must retain policy")
	}
	var output bytes.Buffer
	if err := writeResult(&output, true, result); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "PRIVATE-SOCKET-DETAILS") {
		t.Fatal("private error leaked")
	}
	var decoded commandResult
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil || decoded.RestorePreview == nil || !decoded.Preview {
		t.Fatalf("structured preview unavailable: %v", err)
	}
}

func TestRestorePreviewDuplicateTerminalIsUncertain(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 1)
	client := &terminalManageObservationRequester{tree: treeWithContexts(registry.Contexts[0].ID, registry.Contexts[0].ID)}
	deps.newSwayClient = func(string) swayRequester { return client }
	result, failure := executeRestore(t.Context(), []string{"--preview", "--socket", "/fake/sway.sock"}, deps)
	if failure != nil {
		t.Fatalf("preview failed: %+v", failure)
	}
	item := result.RestorePreview.Contexts[0]
	if item.Status != "uncertain" || !item.Policy.Eligible || item.Reason != "ambiguous_managed_window" || item.Window != "unknown" {
		t.Fatalf("duplicate silently accepted: %+v", item)
	}
	terminalManageRequireObservationOnly(t, client)
}

func TestRestorePreviewMissingStateDoesNotCreateIt(t *testing.T) {
	deps := testDependencies(t)
	deps.newSwayClient = nil
	root, _ := deps.stateRoot()
	if _, failure := executeRestore(t.Context(), []string{"--preview"}, deps); failure != nil {
		t.Fatalf("empty preview failed: %+v", failure)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview created state: %v", err)
	}
}

func TestRestorePreviewRejectsExplicitRestoreModifiers(t *testing.T) {
	for _, args := range [][]string{{"--preview", string(testContextID)}, {"--preview", "--require-active"}} {
		if _, failure := executeRestore(t.Context(), args, testDependencies(t)); failure == nil || !failure.usage {
			t.Fatalf("accepted conflicting arguments: %v", args)
		}
	}
}

func TestRestorePreviewMalformedTreeRemainsUnknown(t *testing.T) {
	for _, payload := range []string{`{}`, `null`, `{"error":"PRIVATE"}`, `{"id":1,"type":"con"}`} {
		t.Run(payload, func(t *testing.T) {
			deps := testDependencies(t)
			storeRestoreTestContexts(t, deps, 1)
			client := &terminalManageObservationRequester{payload: []byte(payload)}
			deps.newSwayClient = func(string) swayRequester { return client }
			result, failure := executeRestore(t.Context(), []string{"--preview", "--socket", "/fake/sway.sock"}, deps)
			if failure != nil {
				t.Fatalf("policy must remain available: %+v", failure)
			}
			item := result.RestorePreview.Contexts[0]
			if item.Status != "uncertain" || item.Window != "unknown" || item.Reason != "window_observation_unavailable" {
				t.Fatalf("invalid evidence became absence: %+v", item)
			}
			terminalManageRequireObservationOnly(t, client)
		})
	}
}
