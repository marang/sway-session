package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func restoreTestStore(t *testing.T) RestoreReportStore {
	t.Helper()
	return RestoreReportStoreFor(filepath.Join(t.TempDir(), "state"))
}

func restoreTestLoad(t *testing.T, store RestoreReportStore) RestoreReport {
	t.Helper()
	report, err := store.LoadContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func restoreTestFind(t *testing.T, report RestoreReport, source string, id ContextID) RestoreOutcome {
	t.Helper()
	for _, o := range report.Outcomes {
		if o.Source == source && o.ContextID == id {
			return o
		}
	}
	t.Fatalf("missing %s outcome for %s", source, id)
	return RestoreOutcome{}
}

func restoreTestUpdate(o RestoreOutcome) RestoreOutcomeUpdate {
	return RestoreOutcomeUpdate{Status: "pending", Reason: "launch_accepted", UpdatedAt: o.UpdatedAt.Add(time.Second), LaunchAccepted: true}
}

func TestRestoreReportReadOnlyMissing(t *testing.T) {
	store := restoreTestStore(t)
	report, err := store.LoadContext(t.Context())
	if err != nil || report.AutomaticRunID != "" || len(report.Outcomes) != 0 {
		t.Fatalf("missing report: %+v, %v", report, err)
	}
	if _, err := os.Stat(store.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read created missing root: %v", err)
	}
	if err := os.Mkdir(store.root, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(store.root, legacyContextsFilename)
	if err := os.WriteFile(legacyPath, []byte("legacy state is not a report"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := restoreTestLoad(t, store); len(got.Outcomes) != 0 {
		t.Fatal("legacy JSON fabricated restore history")
	}
	if _, err := os.Stat(filepath.Join(store.root, StateDatabaseFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("legacy history read created a database")
	}
	if err := os.Remove(legacyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(store.root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("missing database read created files: %v, %v", entries, err)
	}
	if err := RegistryStoreFor(store.root).Save(emptyRegistry()); err != nil {
		t.Fatal(err)
	}
	if got := restoreTestLoad(t, store); len(got.Outcomes) != 0 {
		t.Fatal(got)
	}
	database, err := openStateDatabase(t.Context(), store.root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	exists, err := restoreReportTablesExist(t.Context(), database.db)
	if err != nil || exists {
		t.Fatalf("read created journal: exists=%v err=%v", exists, err)
	}
	var version int
	if err := database.db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
}

func TestRestoreReportAutomaticRetentionAndIdempotence(t *testing.T) {
	store := restoreTestStore(t)
	explicit := restoreTestOutcome("explicit", testContextID)
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{explicit}); err != nil {
		t.Fatal(err)
	}
	automatic := restoreTestOutcome("automatic", testContextID)
	if err := store.BeginAutomaticContext(t.Context(), restoreTestRun, []RestoreOutcome{automatic}); err != nil {
		t.Fatal(err)
	}
	update := restoreTestUpdate(automatic)
	if ok, err := store.UpdateContext(t.Context(), "automatic", testContextID, automatic.AttemptID, update); err != nil || !ok {
		t.Fatalf("update=%v err=%v", ok, err)
	}
	before := restoreTestLoad(t, store)
	if err := store.BeginAutomaticContext(t.Context(), restoreTestRun, []RestoreOutcome{automatic}); err != nil {
		t.Fatal(err)
	}
	if got := restoreTestLoad(t, store); !reflect.DeepEqual(got, before) {
		t.Fatal("same run reset progress")
	}
	if err := store.BeginAutomaticContext(t.Context(), restoreTestOtherRun, nil); err != nil {
		t.Fatal(err)
	}
	got := restoreTestLoad(t, store)
	if got.AutomaticRunID != restoreTestOtherRun || len(got.Outcomes) != 1 || !reflect.DeepEqual(got.Outcomes[0], explicit) {
		t.Fatalf("new automatic run changed explicit rows: %+v", got)
	}
}

func TestRestoreReportExplicitLatestAndTokenCAS(t *testing.T) {
	store := restoreTestStore(t)
	old := restoreTestOutcome("explicit", testContextID)
	other := restoreTestOutcome("explicit", ContextID("00000002-e89b-42d3-a456-000000000002"))
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{old, other}); err != nil {
		t.Fatal(err)
	}
	update := restoreTestUpdate(old)
	if ok, err := store.UpdateContext(t.Context(), old.Source, old.ContextID, old.AttemptID, update); err != nil || !ok {
		t.Fatal(ok, err)
	}
	advanced := restoreTestLoad(t, store)
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{old}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restoreTestLoad(t, store), advanced) {
		t.Fatal("replay reset progress")
	}
	newer := old
	newer.AttemptID = restoreTestOtherAttempt
	newer.StartedAt = old.StartedAt.Add(time.Hour)
	newer.UpdatedAt = newer.StartedAt
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{newer}); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{old}); err != nil {
		t.Fatal(err)
	}
	before := restoreTestLoad(t, store)
	if got := restoreTestFind(t, before, other.Source, other.ContextID); !reflect.DeepEqual(got, other) {
		t.Fatal("unrelated explicit changed")
	}
	if got := restoreTestFind(t, before, newer.Source, newer.ContextID); !reflect.DeepEqual(got, newer) {
		t.Fatal("older begin replaced newer attempt")
	}
	if ok, err := store.UpdateContext(t.Context(), old.Source, old.ContextID, old.AttemptID, update); err != nil || ok {
		t.Fatalf("stale update=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(restoreTestLoad(t, store), before) {
		t.Fatal("stale writer changed report")
	}
}

func TestRestoreReportMonotonicProofsAndCompletion(t *testing.T) {
	store := restoreTestStore(t)
	o := restoreTestOutcome("explicit", testContextID)
	o.Requested.Placement, o.Requested.Layout = true, true
	o.Requested.LayoutDigest = strings.Repeat("b", 64)
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{o}); err != nil {
		t.Fatal(err)
	}
	update := restoreTestUpdate(o)
	update.Status, update.Reason = "completed", "restore_complete"
	if ok, err := store.UpdateContext(t.Context(), o.Source, o.ContextID, o.AttemptID, update); err == nil || ok {
		t.Fatal("launch alone completed")
	}
	if got := restoreTestFind(t, restoreTestLoad(t, store), o.Source, o.ContextID); !reflect.DeepEqual(got, o) {
		t.Fatal("invalid completion persisted partial proofs")
	}
	update.Status, update.Reason, update.WindowMapped = "pending", "window_mapped", true
	update.UpdatedAt = o.UpdatedAt.Add(time.Hour)
	if _, err := store.UpdateContext(t.Context(), o.Source, o.ContextID, o.AttemptID, update); err != nil {
		t.Fatal(err)
	}
	update = RestoreOutcomeUpdate{Status: "pending", Reason: "placement_applied", UpdatedAt: o.UpdatedAt.Add(time.Second), PlacementApplied: true}
	if _, err := store.UpdateContext(t.Context(), o.Source, o.ContextID, o.AttemptID, update); err != nil {
		t.Fatal(err)
	}
	got := restoreTestFind(t, restoreTestLoad(t, store), o.Source, o.ContextID)
	if !got.WindowMapped || !got.LaunchAccepted || !got.PlacementApplied || got.UpdatedAt != o.UpdatedAt.Add(time.Hour) {
		t.Fatalf("proofs or time regressed: %+v", got)
	}
	update.Status, update.Reason = "completed", "restore_complete"
	if _, err := store.UpdateContext(t.Context(), o.Source, o.ContextID, o.AttemptID, update); err == nil {
		t.Fatal("unproven layout completed")
	}
	update.LayoutApplied = true
	if _, err := store.UpdateContext(t.Context(), o.Source, o.ContextID, o.AttemptID, update); err != nil {
		t.Fatal(err)
	}
	got = restoreTestFind(t, restoreTestLoad(t, store), o.Source, o.ContextID)
	if got.Status != "completed" || !got.LayoutApplied {
		t.Fatalf("completion missing: %+v", got)
	}
}

func TestRestoreReportFinalizedStatusNeverChanges(t *testing.T) {
	for _, status := range []string{"completed", "failed", "skipped", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			store := restoreTestStore(t)
			o := restoreTestOutcome("explicit", testContextID)
			o.Status, o.WindowMapped, o.OwnerRunID = status, true, restoreTestRun
			if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{o}); err != nil {
				t.Fatal(err)
			}
			for _, incoming := range []string{"pending", "completed", "failed"} {
				update := RestoreOutcomeUpdate{Status: incoming, Reason: "restore_complete", UpdatedAt: o.UpdatedAt.Add(time.Second), OwnerRunID: restoreTestOtherRun}
				if ok, err := store.UpdateContext(t.Context(), o.Source, o.ContextID, o.AttemptID, update); err != nil || !ok {
					t.Fatal(ok, err)
				}
				got := restoreTestFind(t, restoreTestLoad(t, store), o.Source, o.ContextID)
				if got.Status != status || got.Reason != o.Reason || got.OwnerRunID != o.OwnerRunID {
					t.Fatalf("terminal status reset: %+v", got)
				}
			}
		})
	}
}

func TestRestoreReportConcurrentProofUpdates(t *testing.T) {
	store := restoreTestStore(t)
	o := restoreTestOutcome("explicit", testContextID)
	o.Requested.Placement, o.Requested.Layout = true, true
	o.Requested.LayoutDigest = strings.Repeat("b", 64)
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{o}); err != nil {
		t.Fatal(err)
	}
	updates := []RestoreOutcomeUpdate{
		{LaunchAccepted: true, Status: "pending", Reason: "launch_accepted", UpdatedAt: o.UpdatedAt.Add(time.Second)},
		{WindowMapped: true, Status: "pending", Reason: "window_mapped", UpdatedAt: o.UpdatedAt.Add(2 * time.Second)},
		{WindowMapped: true, PlacementApplied: true, Status: "pending", Reason: "placement_applied", UpdatedAt: o.UpdatedAt.Add(3 * time.Second)},
		{WindowMapped: true, PlacementApplied: true, LayoutApplied: true, Status: "pending", Reason: "layout_applied", UpdatedAt: o.UpdatedAt.Add(4 * time.Second)},
	}
	start := make(chan struct{})
	results := make(chan error, len(updates))
	var wg sync.WaitGroup
	for _, update := range updates {
		wg.Go(func() {
			<-start
			ok, err := store.UpdateContext(t.Context(), o.Source, o.ContextID, o.AttemptID, update)
			if err == nil && !ok {
				err = errors.New("matching token ignored")
			}
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	got := restoreTestFind(t, restoreTestLoad(t, store), o.Source, o.ContextID)
	if !got.LaunchAccepted || !got.WindowMapped || !got.PlacementApplied || !got.LayoutApplied || got.UpdatedAt != updates[3].UpdatedAt {
		t.Fatalf("lost concurrent proof: %+v", got)
	}
}

func TestRestoreReportConcurrentReplacementRejectsOldWriter(t *testing.T) {
	store := restoreTestStore(t)
	old := restoreTestOutcome("explicit", testContextID)
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{old}); err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.AttemptID = restoreTestOtherAttempt
	newer.StartedAt = old.StartedAt.Add(time.Hour)
	newer.UpdatedAt = newer.StartedAt
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		results <- store.BeginExplicitContext(t.Context(), []RestoreOutcome{newer})
	})
	wg.Go(func() {
		<-start
		// Either this commits before replacement or its old token is ignored.
		_, err := store.UpdateContext(t.Context(), old.Source, old.ContextID, old.AttemptID, restoreTestUpdate(old))
		results <- err
	})
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	got := restoreTestFind(t, restoreTestLoad(t, store), newer.Source, newer.ContextID)
	if !reflect.DeepEqual(got, newer) {
		t.Fatalf("old writer contaminated replacement: %+v", got)
	}
}

func TestRestoreReportWriterDeadlineRollsBack(t *testing.T) {
	store := restoreTestStore(t)
	o := restoreTestOutcome("explicit", testContextID)
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{o}); err != nil {
		t.Fatal(err)
	}
	database, err := openStateDatabase(t.Context(), store.root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	tx, err := beginStateWrite(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	ok, err := store.UpdateContext(ctx, o.Source, o.ContextID, o.AttemptID, restoreTestUpdate(o))
	if ok || (!errors.Is(err, context.DeadlineExceeded) && !IsStateDatabaseBusy(err)) {
		t.Fatalf("blocked update did not report deadline/busy: matched=%v err=%v", ok, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	got := restoreTestFind(t, restoreTestLoad(t, store), o.Source, o.ContextID)
	if !reflect.DeepEqual(got, o) {
		t.Fatal("blocked update persisted progress")
	}
}

func TestRestoreReportInterruptAndRecoverPending(t *testing.T) {
	store := restoreTestStore(t)
	auto := restoreTestOutcome("automatic", testContextID)
	explicit := restoreTestOutcome("explicit", testContextID)
	explicit.OwnerRunID = restoreTestRun
	unrelated := restoreTestOutcome("explicit", ContextID("00000002-e89b-42d3-a456-000000000002"))
	unrelated.OwnerRunID = restoreTestOtherRun
	finalized := restoreTestOutcome("explicit", ContextID("00000003-e89b-42d3-a456-000000000003"))
	finalized.Status, finalized.Reason = "failed", "launch_failed"
	if err := store.BeginAutomaticContext(t.Context(), restoreTestRun, []RestoreOutcome{auto}); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{explicit, unrelated, finalized}); err != nil {
		t.Fatal(err)
	}
	now := auto.StartedAt.Add(time.Hour)
	if err := store.InterruptPendingContext(t.Context(), restoreTestRun, now, "user_cancelled"); err != nil {
		t.Fatal(err)
	}
	report := restoreTestLoad(t, store)
	for _, o := range []RestoreOutcome{auto, explicit} {
		got := restoreTestFind(t, report, o.Source, o.ContextID)
		if got.Status != "interrupted" || got.Reason != "user_cancelled" || got.UpdatedAt != now {
			t.Fatal(got)
		}
	}
	if got := restoreTestFind(t, report, unrelated.Source, unrelated.ContextID); !reflect.DeepEqual(got, unrelated) {
		t.Fatal("wrong owner interrupted")
	}
	if err := store.RecoverPendingContext(t.Context(), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	report = restoreTestLoad(t, store)
	got := restoreTestFind(t, report, unrelated.Source, unrelated.ContextID)
	if got.Status != "interrupted" || got.Reason != "daemon_restarted" {
		t.Fatal(got)
	}
	if got := restoreTestFind(t, report, finalized.Source, finalized.ContextID); !reflect.DeepEqual(got, finalized) {
		t.Fatal("restart reset failure")
	}
}

func TestRestoreReportUnboundedInventory(t *testing.T) {
	store := restoreTestStore(t)
	records := make([]RestoreOutcome, 513)
	for i := range records {
		records[i] = restoreTestOutcome("automatic", ContextID(fmt.Sprintf("%08x-e89b-42d3-a456-%012x", i+1, i+1)))
	}
	if err := store.BeginAutomaticContext(t.Context(), restoreTestRun, records); err != nil {
		t.Fatal(err)
	}
	report := restoreTestLoad(t, store)
	if len(report.Outcomes) != len(records) {
		t.Fatalf("inventory truncated to %d", len(report.Outcomes))
	}
}

func TestRestoreReportPermissions(t *testing.T) {
	store := restoreTestStore(t)
	if err := store.BeginAutomaticContext(t.Context(), restoreTestRun, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.root, StateDatabaseFilename)
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0o600 {
		t.Fatalf("database permissions: %o", stat.Mode().Perm())
	}
	stat, err = os.Stat(store.root)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0o700 {
		t.Fatalf("directory permissions: %o", stat.Mode().Perm())
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadContext(t.Context()); err == nil {
		t.Fatal("public database read accepted")
	}
	if err := store.BeginAutomaticContext(t.Context(), restoreTestOtherRun, nil); err == nil {
		t.Fatal("public database write accepted")
	}
}

func TestRestoreReportAtomicBeginRollback(t *testing.T) {
	store := restoreTestStore(t)
	auto := restoreTestOutcome("automatic", testContextID)
	explicit := restoreTestOutcome("explicit", testContextID)
	if err := store.BeginAutomaticContext(t.Context(), restoreTestRun, []RestoreOutcome{auto}); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{explicit}); err != nil {
		t.Fatal(err)
	}
	before := restoreTestLoad(t, store)
	database, err := openStateDatabase(t.Context(), store.root, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.db.ExecContext(t.Context(), `CREATE TRIGGER fail_restore_insert BEFORE INSERT ON restore_outcomes WHEN NEW.context_id = '00000002-e89b-42d3-a456-000000000002' BEGIN SELECT RAISE(ABORT, 'injected insert failure'); END`)
	database.Close()
	if err != nil {
		t.Fatal(err)
	}
	failing := restoreTestOutcome("automatic", ContextID("00000002-e89b-42d3-a456-000000000002"))
	if err := store.BeginAutomaticContext(t.Context(), restoreTestOtherRun, []RestoreOutcome{auto, failing}); err == nil {
		t.Fatal("injected SQL error accepted")
	}
	if !reflect.DeepEqual(restoreTestLoad(t, store), before) {
		t.Fatal("failed begin replaced run or rows")
	}
}

func TestRestoreReportOptionalDDLAndUpdateRollback(t *testing.T) {
	store := restoreTestStore(t)
	if err := RegistryStoreFor(store.root).Save(emptyRegistry()); err != nil {
		t.Fatal(err)
	}
	originalCommit := executeStateCommit
	t.Cleanup(func() { executeStateCommit = originalCommit })
	executeStateCommit = func(*stateWriteTransaction) error { return errors.New("injected commit failure") }
	o := restoreTestOutcome("explicit", testContextID)
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{o}); err == nil {
		t.Fatal("commit error lost")
	}
	executeStateCommit = originalCommit
	database, err := openStateDatabase(t.Context(), store.root, false)
	if err != nil {
		t.Fatal(err)
	}
	exists, err := restoreReportTablesExist(t.Context(), database.db)
	database.Close()
	if err != nil || exists {
		t.Fatalf("failed commit retained optional DDL: %v %v", exists, err)
	}
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{o}); err != nil {
		t.Fatal(err)
	}
	before := restoreTestLoad(t, store)
	executeStateCommit = func(*stateWriteTransaction) error { return errors.New("injected commit failure") }
	if ok, err := store.UpdateContext(t.Context(), o.Source, o.ContextID, o.AttemptID, restoreTestUpdate(o)); err == nil || ok {
		t.Fatal("failed commit claimed success")
	}
	executeStateCommit = originalCommit
	if !reflect.DeepEqual(restoreTestLoad(t, store), before) {
		t.Fatal("failed update persisted")
	}
}

func TestRestoreReportMalformedPayloadFailsClosed(t *testing.T) {
	store := restoreTestStore(t)
	o := restoreTestOutcome("explicit", testContextID)
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{o}); err != nil {
		t.Fatal(err)
	}
	database, err := openStateDatabase(t.Context(), store.root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	payload, err := marshalDatabasePayload("test", o)
	if err != nil {
		t.Fatal(err)
	}
	private := strings.TrimSuffix(string(payload), "}") + `,"private_output":"secret"}`
	if _, err := database.db.ExecContext(t.Context(), `UPDATE restore_outcomes SET payload = ?`, []byte(private)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadContext(t.Context()); err == nil {
		t.Fatal("unknown private field loaded")
	}
	if _, err := store.UpdateContext(t.Context(), o.Source, o.ContextID, o.AttemptID, restoreTestUpdate(o)); err == nil {
		t.Fatal("malformed payload overwritten")
	}
}

func TestRestoreReportInvalidInputsDoNotCreateState(t *testing.T) {
	store := restoreTestStore(t)
	o := restoreTestOutcome("explicit", testContextID)
	o.Reason = "SECRET=https://private.example/"
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{o}); err == nil {
		t.Fatal("private reason accepted")
	}
	if _, err := os.Stat(store.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid begin created state")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.LoadContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := store.RecoverPendingContext(t.Context(), o.StartedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("recovery created missing state")
	}
}

func TestRestoreReportRestartSummarySurvivesNewAutomaticRun(t *testing.T) {
	store := restoreTestStore(t)
	auto := restoreTestOutcome("automatic", testContextID)
	owned := restoreTestOutcome("explicit", testContextID)
	owned.OwnerRunID = restoreTestRun
	fresh := restoreTestOutcome("explicit", ContextID("00000002-e89b-42d3-a456-000000000002"))
	fresh.Reason = "awaiting_daemon"
	if err := store.BeginAutomaticContext(t.Context(), restoreTestRun, []RestoreOutcome{auto}); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{owned, fresh}); err != nil {
		t.Fatal(err)
	}
	now := auto.UpdatedAt.Add(time.Hour)
	if err := store.RecoverPendingContext(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	report := restoreTestLoad(t, store)
	if report.InterruptedRunID != restoreTestRun || report.InterruptedAt == nil || *report.InterruptedAt != now {
		t.Fatalf("missing restart summary: %+v", report)
	}
	for _, o := range []RestoreOutcome{auto, owned} {
		got := restoreTestFind(t, report, o.Source, o.ContextID)
		if got.Status != "interrupted" || got.Reason != "daemon_restarted" {
			t.Fatalf("stale owned attempt survived: %+v", got)
		}
	}
	if got := restoreTestFind(t, report, fresh.Source, fresh.ContextID); !reflect.DeepEqual(got, fresh) {
		t.Fatal("fresh ownerless request interrupted")
	}
	replacement := auto
	replacement.AttemptID = restoreTestOtherAttempt
	replacement.StartedAt = now.Add(time.Second)
	replacement.UpdatedAt = replacement.StartedAt
	if err := store.BeginAutomaticContext(t.Context(), restoreTestOtherRun, []RestoreOutcome{replacement}); err != nil {
		t.Fatal(err)
	}
	report = restoreTestLoad(t, store)
	if report.AutomaticRunID != restoreTestOtherRun || report.InterruptedRunID != restoreTestRun || report.InterruptedAt == nil || *report.InterruptedAt != now {
		t.Fatalf("new run erased prior interruption identity: %+v", report)
	}
	if got := restoreTestFind(t, report, "automatic", testContextID); !reflect.DeepEqual(got, replacement) {
		t.Fatal("new run not installed")
	}
	if ok, err := store.UpdateContext(t.Context(), auto.Source, auto.ContextID, auto.AttemptID, restoreTestUpdate(auto)); err != nil || ok {
		t.Fatalf("old automatic attempt was not ignored: matched=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(restoreTestLoad(t, store), report) {
		t.Fatal("old automatic writer changed the replacement run")
	}
	if err := store.InterruptPendingContext(t.Context(), restoreTestRun, now.Add(time.Minute), "user_cancelled"); err != nil {
		t.Fatal(err)
	}
	afterOldOwner := restoreTestLoad(t, store)
	if !reflect.DeepEqual(afterOldOwner, report) {
		t.Fatal("old run interrupted new run or changed summary")
	}
	if err := store.InterruptPendingContext(t.Context(), restoreTestOtherRun, now.Add(time.Minute), "user_cancelled"); err != nil {
		t.Fatal(err)
	}
	report = restoreTestLoad(t, store)
	if report.InterruptedRunID != restoreTestOtherRun || report.InterruptedAt == nil || *report.InterruptedAt != now.Add(time.Minute) {
		t.Fatal("latest interruption summary not replaced")
	}
}

func TestRestoreReportConcurrentStartupPreservesOwnerlessRequest(t *testing.T) {
	store := restoreTestStore(t)
	auto := restoreTestOutcome("automatic", testContextID)
	if err := store.BeginAutomaticContext(t.Context(), restoreTestRun, []RestoreOutcome{auto}); err != nil {
		t.Fatal(err)
	}
	fresh := restoreTestOutcome("explicit", testContextID)
	fresh.StartedAt = auto.StartedAt.Add(time.Hour)
	fresh.UpdatedAt = fresh.StartedAt
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		results <- store.BeginExplicitContext(t.Context(), []RestoreOutcome{fresh})
	})
	wg.Go(func() {
		<-start
		results <- store.RecoverPendingContext(t.Context(), fresh.StartedAt)
	})
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	got := restoreTestFind(t, restoreTestLoad(t, store), fresh.Source, fresh.ContextID)
	if !reflect.DeepEqual(got, fresh) {
		t.Fatal("startup interrupted a concurrent ownerless request")
	}
}

func TestRestoreReportInvalidProofUpdateRollsBack(t *testing.T) {
	store := restoreTestStore(t)
	o := restoreTestOutcome("explicit", testContextID)
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{o}); err != nil {
		t.Fatal(err)
	}
	before := restoreTestLoad(t, store)
	update := restoreTestUpdate(o)
	update.PlacementApplied, update.WindowMapped = true, true
	if ok, err := store.UpdateContext(t.Context(), o.Source, o.ContextID, o.AttemptID, update); ok || err == nil {
		t.Fatal("unrequested placement proof persisted")
	}
	if !reflect.DeepEqual(restoreTestLoad(t, store), before) {
		t.Fatal("invalid proof update changed stored record")
	}
}

func TestRestoreReportUpdatePendingFinalizationPreservesRecord(t *testing.T) {
	for _, source := range []string{"automatic", "explicit"} {
		for _, status := range []string{"completed", "failed", "skipped", "interrupted"} {
			t.Run(source+"/"+status, func(t *testing.T) {
				store := restoreTestStore(t)
				o := restoreTestOutcome(source, testContextID)
				o.OwnerRunID = restoreTestRun
				if source == "automatic" {
					if err := store.BeginAutomaticContext(t.Context(), restoreTestRun, []RestoreOutcome{o}); err != nil {
						t.Fatal(err)
					}
				} else if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{o}); err != nil {
					t.Fatal(err)
				}
				finalize := RestoreOutcomeUpdate{Status: status, Reason: "restore_complete", UpdatedAt: o.UpdatedAt.Add(time.Second), OwnerRunID: restoreTestRun}
				if status == "completed" {
					finalize.WindowMapped = true
				}
				if ok, err := store.UpdatePendingContext(t.Context(), o.Source, o.ContextID, o.AttemptID, finalize); err != nil || !ok {
					t.Fatalf("finalize pending: matched=%v err=%v", ok, err)
				}
				before := restoreTestLoad(t, store)
				rearm := RestoreOutcomeUpdate{Status: "pending", Reason: "awaiting_daemon", UpdatedAt: o.UpdatedAt.Add(time.Hour), OwnerRunID: restoreTestOtherRun, LaunchAccepted: true, WindowMapped: true}
				if ok, err := store.UpdatePendingContext(t.Context(), o.Source, o.ContextID, o.AttemptID, rearm); err != nil || ok {
					t.Fatalf("finalized attempt matched: matched=%v err=%v", ok, err)
				}
				if got := restoreTestLoad(t, store); !reflect.DeepEqual(got, before) {
					t.Fatalf("finalized record proofs/time/owner or summary changed: before=%+v after=%+v", before, got)
				}
			})
		}
	}
}

func TestRestoreReportUpdatePendingReplacementCAS(t *testing.T) {
	store := restoreTestStore(t)
	old := restoreTestOutcome("explicit", testContextID)
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{old}); err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.AttemptID = restoreTestOtherAttempt
	newer.StartedAt = old.StartedAt.Add(time.Hour)
	newer.UpdatedAt = newer.StartedAt
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{newer}); err != nil {
		t.Fatal(err)
	}
	before := restoreTestLoad(t, store)
	if ok, err := store.UpdatePendingContext(t.Context(), old.Source, old.ContextID, old.AttemptID, restoreTestUpdate(old)); err != nil || ok {
		t.Fatalf("old pending token matched replacement: matched=%v err=%v", ok, err)
	}
	if got := restoreTestLoad(t, store); !reflect.DeepEqual(got, before) {
		t.Fatal("stale pending writer changed replacement")
	}
	if ok, err := store.UpdatePendingContext(t.Context(), newer.Source, newer.ContextID, newer.AttemptID, restoreTestUpdate(newer)); err != nil || !ok {
		t.Fatalf("latest pending token not matched: matched=%v err=%v", ok, err)
	}
	got := restoreTestFind(t, restoreTestLoad(t, store), newer.Source, newer.ContextID)
	if !got.LaunchAccepted || got.UpdatedAt != newer.UpdatedAt.Add(time.Second) {
		t.Fatalf("matching pending progress not persisted: %+v", got)
	}
}

func TestRestoreReportUpdateContextStillAcceptsProofAfterFailure(t *testing.T) {
	store := restoreTestStore(t)
	o := restoreTestOutcome("explicit", testContextID)
	o.Status, o.Reason = "failed", "mapping_timeout"
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{o}); err != nil {
		t.Fatal(err)
	}
	proof := RestoreOutcomeUpdate{Status: "completed", Reason: "restore_complete", UpdatedAt: o.UpdatedAt.Add(time.Hour), WindowMapped: true}
	if ok, err := store.UpdateContext(t.Context(), o.Source, o.ContextID, o.AttemptID, proof); err != nil || !ok {
		t.Fatalf("ordinary update no longer accepts later proof: matched=%v err=%v", ok, err)
	}
	got := restoreTestFind(t, restoreTestLoad(t, store), o.Source, o.ContextID)
	if !got.WindowMapped || got.Status != "failed" || got.Reason != "mapping_timeout" || got.UpdatedAt != proof.UpdatedAt {
		t.Fatalf("ordinary update semantics changed: %+v", got)
	}
}

func TestRestoreReportAppendAutomaticProgressiveAndIdempotent(t *testing.T) {
	store := restoreTestStore(t)
	explicit := restoreTestOutcome("explicit", testContextID)
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{explicit}); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginAutomaticContext(t.Context(), restoreTestRun, nil); err != nil {
		t.Fatal(err)
	}
	records := make([]RestoreOutcome, 257)
	for i := range records {
		records[i] = restoreTestOutcome("automatic", ContextID(fmt.Sprintf("%08x-e89b-42d3-a456-%012x", i+1, i+1)))
	}
	for start := 0; start < len(records); start += 64 {
		if ok, err := store.AppendAutomaticContext(t.Context(), restoreTestRun, records[start:min(start+64, len(records))]); err != nil || !ok {
			t.Fatalf("append chunk: matched=%v err=%v", ok, err)
		}
	}
	report := restoreTestLoad(t, store)
	if len(report.Outcomes) != len(records)+1 {
		t.Fatalf("progressive inventory truncated: %d", len(report.Outcomes))
	}
	if got := restoreTestFind(t, report, explicit.Source, explicit.ContextID); !reflect.DeepEqual(got, explicit) {
		t.Fatal("append changed explicit row")
	}
	first := records[0]
	if ok, err := store.UpdatePendingContext(t.Context(), first.Source, first.ContextID, first.AttemptID, restoreTestUpdate(first)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	before := restoreTestLoad(t, store)
	replay := first
	replay.AttemptID = restoreTestOtherAttempt
	replay.StartedAt = first.StartedAt.Add(time.Hour)
	replay.UpdatedAt = replay.StartedAt
	if ok, err := store.AppendAutomaticContext(t.Context(), restoreTestRun, []RestoreOutcome{replay}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if !reflect.DeepEqual(restoreTestLoad(t, store), before) {
		t.Fatal("append replay reset token or progress")
	}
	if err := store.BeginAutomaticContext(t.Context(), restoreTestRun, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restoreTestLoad(t, store), before) {
		t.Fatal("same empty begin erased appended progress")
	}
	if err := store.BeginAutomaticContext(t.Context(), restoreTestOtherRun, nil); err != nil {
		t.Fatal(err)
	}
	before = restoreTestLoad(t, store)
	if ok, err := store.AppendAutomaticContext(t.Context(), restoreTestRun, records[:64]); err != nil || ok {
		t.Fatalf("stale run appended: matched=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(restoreTestLoad(t, store), before) {
		t.Fatal("stale run changed journal")
	}
}

func TestRestoreReportMissingJournalMutationsDoNotCreateDDL(t *testing.T) {
	for _, existingDatabase := range []bool{false, true} {
		t.Run(fmt.Sprint(existingDatabase), func(t *testing.T) {
			store := restoreTestStore(t)
			if existingDatabase {
				if err := RegistryStoreFor(store.root).Save(emptyRegistry()); err != nil {
					t.Fatal(err)
				}
			}
			o := restoreTestOutcome("automatic", testContextID)
			if ok, err := store.UpdatePendingContext(t.Context(), o.Source, o.ContextID, o.AttemptID, restoreTestUpdate(o)); err != nil || ok {
				t.Fatal(ok, err)
			}
			if ok, err := store.AppendAutomaticContext(t.Context(), restoreTestRun, []RestoreOutcome{o}); err != nil || ok {
				t.Fatal(ok, err)
			}
			if err := store.RecoverPendingContext(t.Context(), o.StartedAt); err != nil {
				t.Fatal(err)
			}
			if got := restoreTestLoad(t, store); len(got.Outcomes) != 0 || got.AutomaticRunID != "" {
				t.Fatal("missing journal fabricated report")
			}
			if !existingDatabase {
				if _, err := os.Stat(store.root); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("mutation created missing state")
				}
				return
			}
			database, err := openStateDatabase(t.Context(), store.root, false)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			exists, err := restoreReportTablesExist(t.Context(), database.db)
			if err != nil || exists {
				t.Fatalf("missing journal mutation created DDL: exists=%v err=%v", exists, err)
			}
		})
	}
}

func TestRestoreReportAppendAtomicRollback(t *testing.T) {
	store := restoreTestStore(t)
	if err := store.BeginAutomaticContext(t.Context(), restoreTestRun, nil); err != nil {
		t.Fatal(err)
	}
	before := restoreTestLoad(t, store)
	database, err := openStateDatabase(t.Context(), store.root, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.db.ExecContext(t.Context(), `CREATE TRIGGER fail_restore_append BEFORE INSERT ON restore_outcomes WHEN NEW.context_id = '00000002-e89b-42d3-a456-000000000002' BEGIN SELECT RAISE(ABORT, 'injected append failure'); END`)
	database.Close()
	if err != nil {
		t.Fatal(err)
	}
	records := []RestoreOutcome{restoreTestOutcome("automatic", testContextID), restoreTestOutcome("automatic", ContextID("00000002-e89b-42d3-a456-000000000002"))}
	if ok, err := store.AppendAutomaticContext(t.Context(), restoreTestRun, records); err == nil || ok {
		t.Fatal("partial append claimed success")
	}
	if !reflect.DeepEqual(restoreTestLoad(t, store), before) {
		t.Fatal("failed append retained partial chunk")
	}
}
