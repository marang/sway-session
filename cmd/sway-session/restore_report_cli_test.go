package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/diagnostic"
	sessionstate "github.com/marang/sway-session/internal/session"
)

func cliRestoreTestHistory(t *testing.T, root string) sessionstate.RestoreReport {
	t.Helper()
	report, err := sessionstate.RestoreReportStoreFor(root).LoadContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func cliRestoreTestRecord(t *testing.T, item sessionstate.Context, status string, at time.Time) sessionstate.RestoreOutcome {
	t.Helper()
	token, err := sessionstate.NewContextID()
	if err != nil {
		t.Fatal(err)
	}
	reason := "restore_requested"
	if status == "failed" {
		reason = "launch_failed"
	}
	if status == "interrupted" {
		reason = "interrupted"
	}
	if status == "skipped" {
		reason = "policy_archived"
	}
	mapped := status == "completed"
	if mapped {
		reason = "restore_complete"
	}
	return sessionstate.RestoreOutcome{ContextID: item.ID, AttemptID: string(token), Source: "explicit", StartedAt: at.UTC(), UpdatedAt: at.UTC(), Status: status, Reason: reason, Requested: sessionstate.RestoreWork{Window: true}, WindowMapped: mapped, IdentityDigest: sessionstate.RestoreContextDigest(item)}
}

func cliRestoreTestSeed(t *testing.T, root string, record sessionstate.RestoreOutcome) {
	t.Helper()
	if err := sessionstate.RestoreReportStoreFor(root).BeginExplicitContext(t.Context(), []sessionstate.RestoreOutcome{record}); err != nil {
		t.Fatal(err)
	}
}

func TestCLIRestoreIntentTokenAndPartialProofs(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 3)
	root, _ := deps.stateRoot()
	reporter, err := beginCLIRestoreReport(t.Context(), root, "", false, deps)
	if err != nil {
		t.Fatal(err)
	}
	before := cliRestoreTestHistory(t, root)
	if len(before.Outcomes) != 3 {
		t.Fatal("intent was not recorded before effects")
	}
	token := before.Outcomes[0].AttemptID
	for _, record := range before.Outcomes {
		if record.AttemptID != token || record.Status != "pending" || record.LaunchAccepted || record.WindowMapped || record.IdentityDigest != sessionstate.RestoreContextDigest(registry.Contexts[0]) && record.ContextID == registry.Contexts[0].ID {
			t.Fatalf("invalid initial intent: %+v", record)
		}
	}
	first, second := registry.Contexts[0], registry.Contexts[1]
	if err := reporter.launchAccepted(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	failure := &commandFailure{diagnostics: []diagnostic.Diagnostic{{Code: "launch", Message: "private launcher text", Details: map[string]any{"context_id": string(second.ID)}}}}
	if err := reporter.finish(commandResult{Contexts: []sessionstate.Context{first}}, failure, nil); err != nil {
		t.Fatal(err)
	}
	records := latestCLIRestoreOutcomes(cliRestoreTestHistory(t, root))
	if got := records[first.ID]; got.Status != "completed" || !got.WindowMapped || !got.LaunchAccepted || got.PlacementApplied || got.LayoutApplied {
		t.Fatalf("mapped window proof = %+v", got)
	}
	if got := records[second.ID]; got.Status != "failed" || got.Reason != "launch_failed" || got.WindowMapped {
		t.Fatalf("scoped failure = %+v", got)
	}
	if got := records[registry.Contexts[2].ID]; got.Status != "pending" {
		t.Fatalf("unproven sibling inferred failed: %+v", got)
	}
}

func TestCLIRestoreMappingLeavesPlacementAndLayoutForDaemon(t *testing.T) {
	for _, layout := range []bool{false, true} {
		t.Run(map[bool]string{false: "placement", true: "layout"}[layout], func(t *testing.T) {
			deps := testDependencies(t)
			registry := storeRestoreTestContexts(t, deps, 1)
			root, _ := deps.stateRoot()
			item := registry.Contexts[0]
			snapshot := placementOnlySnapshot("98", item.ID)
			if layout {
				snapshot = exactDaemonSnapshot("98", item.ID)
			}
			if err := sessionstate.LayoutStoreFor(root).Save(snapshot); err != nil {
				t.Fatal(err)
			}
			reporter, err := beginCLIRestoreReport(t.Context(), root, string(item.ID), false, deps)
			if err != nil {
				t.Fatal(err)
			}
			if err := reporter.finish(commandResult{Contexts: []sessionstate.Context{item}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			got := latestCLIRestoreOutcomes(cliRestoreTestHistory(t, root))[item.ID]
			if got.Status != "pending" || got.Reason != "awaiting_daemon" || !got.WindowMapped || !got.Requested.Placement || got.Requested.Layout != layout || got.PlacementApplied || got.LayoutApplied {
				t.Fatalf("mapping implied placement/layout: %+v", got)
			}
		})
	}
}

func TestCLIRestoreApplicationQueueIsNotMappingProof(t *testing.T) {
	deps := testDependencies(t)
	root, _ := deps.stateRoot()
	item := sessionstate.Context{ID: testContextID, Label: "App", State: sessionstate.ContextActive, Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherFlatpak, FlatpakID: "org.example.App", FlatpakInstallation: sessionstate.FlatpakUser}, App: &sessionstate.Application{Identity: sessionstate.ApplicationIdentity{Protocol: sessionstate.WindowWayland, WaylandAppID: "org.example.App", SandboxAppID: "org.example.App"}, DesiredOpen: false, RestorePolicy: sessionstate.ApplicationRestoreFollow}}
	saveTestRegistry(t, root, sessionstate.Registry{Version: sessionstate.ContextsSchemaVersion, Contexts: []sessionstate.Context{item}})
	reporter, err := beginCLIRestoreReport(t.Context(), root, string(item.ID), false, deps)
	if err != nil {
		t.Fatal(err)
	}
	if err := reporter.finish(commandResult{Contexts: []sessionstate.Context{item}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	got := latestCLIRestoreOutcomes(cliRestoreTestHistory(t, root))[item.ID]
	if got.Status != "pending" || got.WindowMapped || got.Reason != "awaiting_daemon" {
		t.Fatalf("queued app proved mapping: %+v", got)
	}
}

func TestCLIRestoreIdentityDriftRejectsMappingProof(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 1)
	root, _ := deps.stateRoot()
	item := registry.Contexts[0]
	reporter, err := beginCLIRestoreReport(t.Context(), root, string(item.ID), false, deps)
	if err != nil {
		t.Fatal(err)
	}
	item.Launcher.Session = "different-session"
	if err := reporter.finish(commandResult{Contexts: []sessionstate.Context{item}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	got := latestCLIRestoreOutcomes(cliRestoreTestHistory(t, root))[item.ID]
	if got.WindowMapped || got.Status != "failed" || got.Reason != "identity_changed" {
		t.Fatalf("different identity accepted: %+v", got)
	}
}

func TestCLIRestoreCancellationUsesFreshBoundedContext(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 2)
	root, _ := deps.stateRoot()
	reporter, err := beginCLIRestoreReport(t.Context(), root, "", false, deps)
	if err != nil {
		t.Fatal(err)
	}
	if err := reporter.finish(commandResult{Contexts: []sessionstate.Context{registry.Contexts[0]}}, nil, context.Canceled); err != nil {
		t.Fatal(err)
	}
	records := latestCLIRestoreOutcomes(cliRestoreTestHistory(t, root))
	if records[registry.Contexts[0].ID].Status != "completed" || records[registry.Contexts[1].ID].Status != "interrupted" {
		t.Fatal("cancellation discarded success or failed to journal interruption")
	}
}

func TestCLIRestorePendingUpdatesRespectAttemptCASAndFinalization(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 1)
	root, _ := deps.stateRoot()
	item := registry.Contexts[0]
	fixed := time.Now().UTC()
	deps.now = func() time.Time { return fixed }
	old, err := beginCLIRestoreReport(t.Context(), root, string(item.ID), false, deps)
	if err != nil {
		t.Fatal(err)
	}
	current, err := beginCLIRestoreReport(t.Context(), root, string(item.ID), false, deps)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.finish(commandResult{Contexts: []sessionstate.Context{item}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	record := latestCLIRestoreOutcomes(cliRestoreTestHistory(t, root))[item.ID]
	if record.AttemptID != current.records[item.ID].AttemptID || record.Status != "pending" || record.WindowMapped {
		t.Fatal("stale attempt overwrote current intent")
	}
	if err := current.finish(commandResult{Contexts: []sessionstate.Context{item}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := current.launchAccepted(t.Context(), item.ID); err != nil {
		t.Fatal(err)
	}
	if err := current.finish(commandResult{}, nil, context.Canceled); err != nil {
		t.Fatal(err)
	}
	record = latestCLIRestoreOutcomes(cliRestoreTestHistory(t, root))[item.ID]
	if record.Status != "completed" || record.LaunchAccepted {
		t.Fatalf("finalized record changed: %+v", record)
	}
}

func TestCLIRestoreHistoryFailurePreservesOriginalDiagnosticAndJournal(t *testing.T) {
	original := &commandFailure{usage: true, diagnostics: []diagnostic.Diagnostic{{Code: "launch", Message: "original error"}}}
	appended := appendRestoreHistoryFailure(original, errors.New("private storage details"))
	if len(original.diagnostics) != 1 || len(appended.diagnostics) != 2 || !reflect.DeepEqual(appended.diagnostics[0], original.diagnostics[0]) || !appended.usage || appended.diagnostics[1].Code != "restore_history" || strings.Contains(appended.diagnostics[1].Message, "private") {
		t.Fatal("history error masked/mutated/leaked original failure")
	}
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 1)
	root, _ := deps.stateRoot()
	item := registry.Contexts[0]
	reporter, err := beginCLIRestoreReport(t.Context(), root, string(item.ID), false, deps)
	if err != nil {
		t.Fatal(err)
	}
	if err := reporter.finish(commandResult{Contexts: []sessionstate.Context{item}}, &commandFailure{diagnostics: []diagnostic.Diagnostic{{Code: "restore_history"}}}, nil); err != nil {
		t.Fatal(err)
	}
	got := latestCLIRestoreOutcomes(cliRestoreTestHistory(t, root))[item.ID]
	if got.Status != "pending" || got.Reason != "history_unavailable" || !got.WindowMapped {
		t.Fatalf("history failure became success: %+v", got)
	}
}

func TestCLIRestorePartialWavesKeepEarlierSuccessInHistory(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 3)
	root, _ := deps.stateRoot()
	harness := &restoreBatchHarness{}
	clients := 0
	deps.newSwayClient = func(string) swayRequester {
		clients++
		if clients == 1 {
			return harness
		}
		return &fakeSwayClient{err: errors.New("later wave failed")}
	}
	deps.processStarter = harness
	result, failure := executeRestore(t.Context(), []string{"--socket", "/fake/sway.sock"}, deps)
	if failure == nil || len(result.Contexts) != maxConcurrentTerminalRestores {
		t.Fatalf("partial restore result lost: %+v %+v", result, failure)
	}
	records := latestCLIRestoreOutcomes(cliRestoreTestHistory(t, root))
	token := records[registry.Contexts[0].ID].AttemptID
	for i, item := range registry.Contexts {
		record := records[item.ID]
		if record.AttemptID != token {
			t.Fatal("waves did not share attempt token")
		}
		if i < maxConcurrentTerminalRestores && (record.Status != "completed" || !record.WindowMapped) {
			t.Fatalf("earlier success lost: %+v", record)
		}
		if i >= maxConcurrentTerminalRestores && record.Status != "failed" {
			t.Fatalf("later failure missing: %+v", record)
		}
	}
}

func TestRestoreReportReadOnlyMissingHistoryAndDecoration(t *testing.T) {
	deps := testDependencies(t)
	root, _ := deps.stateRoot()
	result, failure := executeRestoreReport(t.Context(), nil, deps)
	if failure != nil || result.RestoreReport == nil || len(result.RestoreReport.Outcomes) != 0 {
		t.Fatalf("no history is not normal: %+v %+v", result, failure)
	}
	var output bytes.Buffer
	if err := writeResult(&output, false, result); err != nil || !strings.Contains(output.String(), "No restore history") || !strings.Contains(output.String(), "0 pending") {
		t.Fatal("empty human report missing")
	}
	if failure := decorateTerminalRestoreHistory(t.Context(), deps, []terminalInventoryResult{{ContextID: testContextID}}); failure != nil {
		t.Fatal(failure)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("history read created root: %v", err)
	}
}

func TestRestoreReportTotalsAndLatestDecoration(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 5)
	root, _ := deps.stateRoot()
	at := time.Now().UTC().Add(-time.Minute)
	for index, status := range []string{"pending", "completed", "skipped", "failed", "interrupted"} {
		cliRestoreTestSeed(t, root, cliRestoreTestRecord(t, registry.Contexts[index], status, at))
	}
	result, failure := executeRestoreReport(t.Context(), nil, deps)
	if failure != nil {
		t.Fatal(failure)
	}
	if result.RestoreReport.Totals != (restoreReportTotals{Pending: 1, Completed: 1, Skipped: 1, Failed: 1, Interrupted: 1}) {
		t.Fatalf("totals = %+v", result.RestoreReport.Totals)
	}
	var output bytes.Buffer
	if err := writeResult(&output, true, result); err != nil {
		t.Fatal(err)
	}
	var wire struct {
		RestoreReport struct {
			Totals   restoreReportTotals        `json:"totals"`
			Outcomes []restoreReportCommandItem `json:"outcomes"`
		} `json:"restore_report"`
	}
	if err := json.Unmarshal(output.Bytes(), &wire); err != nil || len(wire.RestoreReport.Outcomes) != 5 || strings.Count(output.String(), `"outcomes":`) != 1 || wire.RestoreReport.Totals != result.RestoreReport.Totals {
		t.Fatalf("JSON report lost data: %v %s", err, output.String())
	}
	previous := cliRestoreTestRecord(t, registry.Contexts[0], "failed", at.Add(-time.Hour))
	previous.Source = "automatic"
	previous.UpdatedAt = time.Now().UTC()
	runID, _ := sessionstate.NewContextID()
	if err := sessionstate.RestoreReportStoreFor(root).BeginAutomaticContext(t.Context(), string(runID), []sessionstate.RestoreOutcome{previous}); err != nil {
		t.Fatal(err)
	}
	items := terminalInventory(registry.Contexts)
	if failure := decorateTerminalRestoreHistory(t.Context(), deps, items); failure != nil {
		t.Fatal(failure)
	}
	if items[0].RestoreOutcome == nil || items[0].RestoreOutcome.Source != "explicit" || items[0].RestoreOutcome.Status != "pending" {
		t.Fatal("old automatic update displaced latest attempt")
	}
}

func TestRestoreRetryRejectsArchivedCompletedAndNoHistoryWithoutEffects(t *testing.T) {
	for _, scenario := range []string{"archived", "completed", "no history", "label"} {
		t.Run(scenario, func(t *testing.T) {
			deps := testDependencies(t)
			registry := storeRestoreTestContexts(t, deps, 1)
			root, _ := deps.stateRoot()
			item := registry.Contexts[0]
			if scenario != "no history" {
				status := "failed"
				if scenario == "completed" {
					status = "completed"
				}
				cliRestoreTestSeed(t, root, cliRestoreTestRecord(t, item, status, time.Now().UTC().Add(-time.Minute)))
			}
			if scenario == "archived" {
				if _, err := sessionstate.SetContextStateAt(&registry, string(item.ID), sessionstate.ContextArchived, time.Now()); err != nil {
					t.Fatal(err)
				}
				saveTestRegistry(t, root, registry)
			}
			deps.newSwayClient = func(string) swayRequester { t.Fatal("ineligible retry touched compositor"); return nil }
			id := item.ID
			if scenario == "label" {
				id = sessionstate.ContextID(item.Label)
			}
			if err := executeRestoreRetry(t.Context(), id, "/fake/sway.sock", deps); err == nil {
				t.Fatal("invalid retry accepted")
			}
		})
	}
}

func TestRestoreRetryMappedWindowUsesExistingPathAndLeavesLayoutPending(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 1)
	root, _ := deps.stateRoot()
	item := registry.Contexts[0]
	cliRestoreTestSeed(t, root, cliRestoreTestRecord(t, item, "failed", time.Now().UTC().Add(-time.Minute)))
	if err := sessionstate.LayoutStoreFor(root).Save(exactDaemonSnapshot("98", item.ID)); err != nil {
		t.Fatal(err)
	}
	client := &fakeSwayClient{trees: []*Node{treeWithContexts(item.ID)}}
	deps.newSwayClient = func(string) swayRequester { return client }
	deps.resolveProgram = func(string) (string, error) { t.Fatal("mapped retry resolved adapter"); return "", nil }
	deps.processStarter = &recordingStarter{err: errors.New("must not launch")}
	if err := executeRestoreRetry(t.Context(), item.ID, "/fake/sway.sock", deps); err != nil {
		t.Fatal(err)
	}
	got := latestCLIRestoreOutcomes(cliRestoreTestHistory(t, root))[item.ID]
	if got.Status != "pending" || !got.WindowMapped || got.LayoutApplied || got.LaunchAccepted || got.Reason != "awaiting_daemon" {
		t.Fatalf("retry mapping treated as layout completion: %+v", got)
	}
}

func TestCLIRestoreSelectedArchivedPreservesNormalRestoreSemantics(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 1)
	root, _ := deps.stateRoot()
	item := registry.Contexts[0]
	if _, err := sessionstate.SetContextStateAt(&registry, string(item.ID), sessionstate.ContextArchived, time.Now()); err != nil {
		t.Fatal(err)
	}
	saveTestRegistry(t, root, registry)
	deps.newSwayClient = func(string) swayRequester { return &fakeSwayClient{trees: []*Node{treeWithContexts(item.ID)}} }
	result, failure := executeRestore(t.Context(), []string{"--socket", "/fake/sway.sock", string(item.ID)}, deps)
	if failure != nil || len(result.Contexts) != 1 {
		t.Fatalf("normal explicit archived restore changed: %+v", failure)
	}
	loaded, err := sessionstate.ReadRegistrySnapshot(root)
	if err != nil || loaded.Contexts[0].State != sessionstate.ContextArchived {
		t.Fatal("restore unexpectedly activated archived context")
	}
}

func TestCLIRestoreConcurrentFixedClockIntentOrdersTokens(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 1)
	root, _ := deps.stateRoot()
	item := registry.Contexts[0]
	at := time.Now().UTC()
	deps.now = func() time.Time { return at }
	type result struct {
		reporter *cliRestoreReporter
		err      error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			reporter, err := beginCLIRestoreReport(t.Context(), root, string(item.ID), false, deps)
			results <- result{reporter, err}
		}()
	}
	workers.Wait()
	close(results)
	var records []sessionstate.RestoreOutcome
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		records = append(records, result.reporter.records[item.ID])
	}
	if records[0].StartedAt.Equal(records[1].StartedAt) || records[0].AttemptID == records[1].AttemptID {
		t.Fatal("concurrent requests shared timestamp or attempt token")
	}
	latest := records[0]
	if records[1].StartedAt.After(latest.StartedAt) {
		latest = records[1]
	}
	got := latestCLIRestoreOutcomes(cliRestoreTestHistory(t, root))[item.ID]
	if got.AttemptID != latest.AttemptID || got.StartedAt != latest.StartedAt {
		t.Fatalf("latest intent ignored: %+v", got)
	}
}

func TestCLIRestoreConcurrentInvocationsDoNotDuplicateLaunch(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 1)
	root, _ := deps.stateRoot()
	item := registry.Contexts[0]
	at := time.Now().UTC()
	deps.now = func() time.Time { return at }
	harness := &restoreBatchHarness{}
	deps.newSwayClient = func(string) swayRequester { return harness }
	deps.processStarter = harness
	type outcome struct {
		result  commandResult
		failure *commandFailure
	}
	results := make(chan outcome, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			result, failure := executeRestore(t.Context(), []string{"--socket", "/fake/sway.sock", string(item.ID)}, deps)
			results <- outcome{result, failure}
		}()
	}
	workers.Wait()
	close(results)
	for got := range results {
		if got.failure != nil || len(got.result.Contexts) != 1 {
			t.Fatalf("concurrent restore failed: %+v", got.failure)
		}
	}
	if harness.startCount() != 1 {
		t.Fatalf("duplicate adapter launches: %d", harness.startCount())
	}
	got := latestCLIRestoreOutcomes(cliRestoreTestHistory(t, root))[item.ID]
	if got.Status != "completed" || !got.WindowMapped || !got.StartedAt.After(at) {
		t.Fatalf("latest concurrent attempt lost: %+v", got)
	}
}

func TestRestoreReportHumanRunMetadataAndFailedProof(t *testing.T) {
	at := time.Now().UTC()
	report := restoreReportCommandResult{RestoreReport: sessionstate.RestoreReport{AutomaticRunID: "11111111-1111-4111-8111-111111111111", InterruptedRunID: "22222222-2222-4222-8222-222222222222", InterruptedAt: &at}}
	var output bytes.Buffer
	if err := writeRestoreReport(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Automatic restore run: " + report.AutomaticRunID, "Previous automatic run interrupted: " + report.InterruptedRunID, at.Format(time.RFC3339)} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing run evidence %q: %s", want, output.String())
		}
	}
	record := sessionstate.RestoreOutcome{ContextID: testContextID, AttemptID: "33333333-3333-4333-8333-333333333333", Source: "explicit", Status: "failed", Reason: "layout_failed", UpdatedAt: at, WindowMapped: true, Requested: sessionstate.RestoreWork{Window: true, Layout: true}}
	report.Outcomes = []restoreReportCommandItem{{RestoreOutcome: record}}
	output.Reset()
	if err := writeRestoreReport(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"failed · window mapped", "Attempt " + record.AttemptID, "requested: window=true placement=false layout=true", "observed: launch=false window=true placement=false layout=false"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing partial proof %q: %s", want, output.String())
		}
	}
}

func TestRestoreRetryRechecksLatestAttemptBeforeEffects(t *testing.T) {
	deps := testDependencies(t)
	registry := storeRestoreTestContexts(t, deps, 1)
	root, _ := deps.stateRoot()
	item := registry.Contexts[0]
	at := time.Now().UTC().Add(-time.Minute)
	cliRestoreTestSeed(t, root, cliRestoreTestRecord(t, item, "failed", at))
	calls := 0
	deps.stateRoot = func() (string, error) {
		calls++
		if calls == 2 {
			cliRestoreTestSeed(t, root, cliRestoreTestRecord(t, item, "completed", at.Add(time.Second)))
		}
		return root, nil
	}
	deps.newSwayClient = func(string) swayRequester { t.Fatal("stale retry reached compositor"); return nil }
	if err := executeRestoreRetry(t.Context(), item.ID, "/fake/sway.sock", deps); err == nil {
		t.Fatal("retry replaced a newer completed attempt")
	}
}

type cliRestoreAcceptedUnknownStarter struct {
	calls   int
	onStart func()
}

func (starter *cliRestoreAcceptedUnknownStarter) Start(sessionstate.ProcessSpec) error {
	starter.calls++
	if starter.onStart != nil {
		starter.onStart()
	}
	return &sessionstate.ProcessLaunchOutcomeUnknownError{Err: errors.New("injected bookkeeping failure")}
}

func TestCLIRestoreAcceptedUnknownLaunchRetainsOperationalErrorAndEvidence(t *testing.T) {
	for _, scenario := range []string{"mapped", "unmapped", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			deps := testDependencies(t)
			registry := storeRestoreTestContexts(t, deps, 1)
			root, _ := deps.stateRoot()
			item := registry.Contexts[0]
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := &fakeSwayClient{trees: []*Node{treeWithContexts()}}
			if scenario == "mapped" {
				client.trees = append(client.trees, treeWithContexts(item.ID))
			}
			deps.newSwayClient = func(string) swayRequester { return client }
			deps.settleTimeout = 0
			starter := &cliRestoreAcceptedUnknownStarter{}
			if scenario == "cancelled" {
				starter.onStart = cancel
			}
			deps.processStarter = starter
			result, failure := executeRestore(ctx, []string{"--socket", "/fake/sway.sock", string(item.ID)}, deps)
			if failure == nil || starter.calls != 1 {
				t.Fatal("accepted-unknown operational failure discarded")
			}
			sawUnknown := false
			sawCancellation := false
			for _, entry := range failure.diagnostics {
				if entry.Code == "launch_outcome_unknown" && strings.Contains(entry.Message, "injected bookkeeping failure") {
					sawUnknown = true
				}
				if strings.Contains(entry.Message, context.Canceled.Error()) {
					sawCancellation = true
				}
			}
			if !sawUnknown {
				t.Fatalf("original launch uncertainty missing: %+v", failure.diagnostics)
			}
			got := latestCLIRestoreOutcomes(cliRestoreTestHistory(t, root))[item.ID]
			if !got.LaunchAccepted || got.Reason == "launch_failed" {
				t.Fatalf("accepted launch recorded as rejected: %+v", got)
			}
			switch scenario {
			case "mapped":
				if len(result.Contexts) != 1 || !got.WindowMapped || got.Status != "completed" {
					t.Fatalf("mapped proof lost: result=%+v history=%+v", result, got)
				}
			case "unmapped":
				if len(result.Contexts) != 0 || got.WindowMapped || got.Status != "failed" || got.Reason != "observation_unavailable" {
					t.Fatalf("unmapped uncertainty became launch failure: %+v", got)
				}
			case "cancelled":
				if !sawCancellation || got.Status != "interrupted" || got.Reason != "interrupted" {
					t.Fatalf("cancellation overwritten: history=%+v failure=%+v", got, failure.diagnostics)
				}
			}
		})
	}
}
