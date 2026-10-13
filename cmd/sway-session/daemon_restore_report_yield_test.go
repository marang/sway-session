package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

func TestRestoreReportInjectedIOYieldResumesAllContexts(t *testing.T) {
	// Test cursor semantics with controlled Go time. Real filesystem latency
	// must not replace the chosen injected yield with an unrelated timer yield.
	// The original batching integration separately exercises real deadlines.
	synctest.Test(t, testRestoreReportInjectedIOYieldResumesAllContexts)
}

func testRestoreReportInjectedIOYieldResumesAllContexts(t *testing.T) {
	ids := make([]sessionstate.ContextID, restoreReportBatch+1)
	for i := range ids {
		id, err := sessionstate.NewContextID()
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	registry := sessionRegistryIDs(ids...)
	runtime, now := restoreReportRuntime(t, registry, sessionstate.LayoutSnapshot{Version: sessionstate.LayoutSchemaVersion, Workspaces: []sessionstate.WorkspaceLayout{}})
	store := sessionstate.RestoreReportStoreFor(runtime.root)
	load := func() sessionstate.RestoreReport {
		t.Helper()
		report, err := store.LoadContext(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Outcomes) != len(ids) {
			t.Fatalf("journal lost contexts: got %d, want %d", len(report.Outcomes), len(ids))
		}
		return report
	}
	// Build a fresh observation on every pass, including new container IDs.
	observe := func(pass int, updater restorePendingOutcomeUpdater) error {
		leaves := make([]*Node, len(ids))
		for i, id := range ids {
			leaves[i] = managedDaemonLeaf(t, int64(40+pass*len(ids)+i), id)
		}
		return runtime.observeRestoreReportWithUpdater(daemonTree("98", leaves...), registry, now.Add(time.Duration(pass)*time.Millisecond), updater)
	}
	before := load()
	initialCursor := runtime.restoreReportCursor
	var resumeID sessionstate.ContextID
	var resumeToken string
	calls := 0
	err := observe(0, func(_ context.Context, _ string, id sessionstate.ContextID, token string, _ sessionstate.RestoreOutcomeUpdate) (bool, error) {
		calls++
		resumeID, resumeToken = id, token
		return false, context.DeadlineExceeded
	})
	if calls != 1 {
		t.Fatalf("observation stopped before the initial injected yield; real 250ms I/O may have expired: calls=%d err=%v", calls, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("initial injected yield: calls=%d err=%v", calls, err)
	}
	if runtime.restoreReportCursor != initialCursor || !reflect.DeepEqual(load(), before) {
		t.Fatal("yield before the first write advanced the cursor or changed durable outcomes")
	}

	// The injected errors do not advance controlled time. Reads and writes keep
	// their production context budget; the unchanged batching integration runs
	// outside synctest and retains actual 250ms deadline coverage.
	committed := make(map[sessionstate.ContextID]string)
	previousCompleted := 0
	totalCalls := calls
	yieldedCalls := calls
	for pass := 1; pass <= len(ids); pass++ {
		beforeRecords := make(map[sessionstate.ContextID]sessionstate.RestoreOutcome)
		for _, record := range before.Outcomes {
			beforeRecords[record.ContextID] = record
		}
		passCalls := 0
		var savedID, yieldedID sessionstate.ContextID
		var savedToken, yieldedToken string
		var actualWriteErr error
		err := observe(pass, func(ctx context.Context, source string, id sessionstate.ContextID, token string, update sessionstate.RestoreOutcomeUpdate) (bool, error) {
			passCalls++
			if passCalls > 1 {
				yieldedCalls++
				yieldedID, yieldedToken = id, token
				return false, context.DeadlineExceeded
			}
			if id != resumeID || token != resumeToken {
				t.Fatalf("pass %d skipped yielded attempt: got %s/%s, want %s/%s", pass, id, token, resumeID, resumeToken)
			}
			matched, err := store.UpdatePendingContext(ctx, source, id, token, update)
			if err != nil {
				actualWriteErr = err
				return false, err
			}
			if !matched {
				t.Fatalf("pass %d did not match pending attempt %s/%s", pass, id, token)
			}
			if _, exists := committed[id]; exists {
				t.Fatalf("pass %d rewrote completed context %s", pass, id)
			}
			savedID, savedToken = id, token
			committed[id] = token
			return true, nil
		})
		totalCalls += passCalls
		if actualWriteErr != nil {
			t.Fatalf("pass %d real public write failed before the injected yield: %v", pass, actualWriteErr)
		}
		if savedID == "" {
			t.Fatalf("pass %d stopped before the first public write; real 250ms I/O may have expired: calls=%d err=%v", pass, passCalls, err)
		}
		if yieldedID != "" {
			if passCalls != 2 || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("pass %d injected yield: calls=%d err=%v", pass, passCalls, err)
			}
		} else if passCalls != 1 || err != nil {
			t.Fatalf("pass %d final observation: calls=%d err=%v", pass, passCalls, err)
		}
		report := load()
		completed := 0
		for _, record := range report.Outcomes {
			if record.ContextID == yieldedID && !reflect.DeepEqual(record, beforeRecords[yieldedID]) {
				t.Fatalf("pass %d changed the yielded attempt: %+v", pass, record)
			}
			if record.Status == "completed" {
				completed++
				if committed[record.ContextID] != record.AttemptID || !record.WindowMapped || record.Reason != "restore_complete" {
					t.Fatalf("pass %d completed without public-store proof: %+v", pass, record)
				}
			} else if record.Status != "pending" {
				t.Fatalf("pass %d unexpected terminal outcome: %+v", pass, record)
			}
		}
		if completed != len(committed) || completed != previousCompleted+1 {
			t.Fatalf("pass %d durable progress: before=%d completed=%d successful writes=%d", pass, previousCompleted, completed, len(committed))
		}
		if runtime.restoreReportCursor != restoreOutcomeCursor(sessionstate.RestoreOutcome{Source: "automatic", ContextID: savedID, AttemptID: savedToken}) {
			t.Fatalf("pass %d cursor advanced beyond the committed attempt: %s", pass, runtime.restoreReportCursor)
		}
		if completed == len(ids) {
			if yieldedID != "" || totalCalls != len(committed)+yieldedCalls {
				t.Fatalf("final updater accounting: calls=%d commits=%d injected yields=%d", totalCalls, len(committed), yieldedCalls)
			}
			return
		}
		if yieldedID == "" || beforeRecords[yieldedID].Status != "pending" {
			t.Fatalf("pass %d stopped before the planned injected yield; real 250ms I/O may have expired: calls=%d completed=%d err=%v", pass, passCalls, completed, err)
		}
		previousCompleted = completed
		resumeID, resumeToken = yieldedID, yieldedToken
		before = report
	}
	t.Fatalf("fresh observations left contexts unfinished: completed=%d of %d", previousCompleted, len(ids))
}
