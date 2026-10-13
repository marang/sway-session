package session

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/statefile"
)

const restoreTestWriteAttempts = 8

// Concurrency tests require eventual logical completion, not an unbounded
// SQLite wait. Replay only known rejected writes with the same token/input.
func retryRestoreTestWrite(parent context.Context, write func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := write(ctx)
		if err == nil {
			return nil
		}
		var unknown *statefile.CommitOutcomeUnknownError
		if errors.As(err, &unknown) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if !IsStateDatabaseBusy(err) || attempt+1 == restoreTestWriteAttempts {
			return err
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

func TestRestoreReportTestReplayStopsAtRejectedBoundaries(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{"ordinary", errors.New("storage failure")},
		{"unknown-commit", &statefile.CommitOutcomeUnknownError{Cause: ErrStateDatabaseBusy}},
		{"cancelled", errors.Join(ErrStateDatabaseBusy, context.Canceled)},
		{"deadline", errors.Join(ErrStateDatabaseBusy, context.DeadlineExceeded)},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			err := retryRestoreTestWrite(t.Context(), func(context.Context) error { calls++; return test.err })
			if err != test.err || calls != 1 {
				t.Fatalf("replayed rejected result: calls=%d err=%v", calls, err)
			}
		})
	}
	t.Run("attempt-bound", func(t *testing.T) {
		calls := 0
		err := retryRestoreTestWrite(t.Context(), func(context.Context) error { calls++; return ErrStateDatabaseBusy })
		if !IsStateDatabaseBusy(err) || calls != restoreTestWriteAttempts {
			t.Fatalf("unbounded busy replay: calls=%d err=%v", calls, err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := 0
		err := retryRestoreTestWrite(ctx, func(context.Context) error { calls++; cancel(); return ErrStateDatabaseBusy })
		if !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("cancelled replay continued: calls=%d err=%v", calls, err)
		}
	})
}

func TestRestoreReportReplacementReconcilesKnownBusy(t *testing.T) {
	store := restoreTestStore(t)
	old := restoreTestOutcome("explicit", testContextID)
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{old}); err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.AttemptID = restoreTestOtherAttempt
	newer.StartedAt = old.StartedAt.Add(time.Hour)
	newer.UpdatedAt = newer.StartedAt
	database, err := openStateDatabase(t.Context(), store.root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	blocker, err := beginStateWrite(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	busy, results := make(chan error, 2), make(chan error, 2)
	var workers sync.WaitGroup
	for _, write := range []func(context.Context) error{
		func(ctx context.Context) error { return store.BeginExplicitContext(ctx, []RestoreOutcome{newer}) },
		func(ctx context.Context) error {
			_, err := store.UpdateContext(ctx, old.Source, old.ContextID, old.AttemptID, restoreTestUpdate(old))
			return err
		},
	} {
		workers.Go(func() {
			reported := false
			results <- retryRestoreTestWrite(ctx, func(ctx context.Context) error {
				err := write(ctx)
				if !reported && IsStateDatabaseBusy(err) {
					reported = true
					busy <- err
				}
				return err
			})
		})
	}
	defer workers.Wait()
	for range 2 {
		select {
		case err := <-busy:
			if !IsStateDatabaseBusy(err) {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("both public operations did not reach known busy:", ctx.Err())
		}
	}
	if got := restoreTestFind(t, restoreTestLoad(t, store), old.Source, old.ContextID); !reflect.DeepEqual(got, old) {
		t.Fatal("rejected busy writes mutated the original record")
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if matched, err := store.UpdateContext(t.Context(), old.Source, old.ContextID, old.AttemptID, restoreTestUpdate(old)); err != nil || matched {
		t.Fatalf("stale writer accepted after replacement: matched=%t err=%v", matched, err)
	}
	if got := restoreTestFind(t, restoreTestLoad(t, store), newer.Source, newer.ContextID); !reflect.DeepEqual(got, newer) {
		t.Fatalf("old writer contaminated replacement: %+v", got)
	}
}

func TestRestoreReportTestReplayNeverRepeatsUnknownCommittedUpdate(t *testing.T) {
	store := restoreTestStore(t)
	record := restoreTestOutcome("explicit", testContextID)
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{record}); err != nil {
		t.Fatal(err)
	}
	original := executeStateCommit
	t.Cleanup(func() { executeStateCommit = original })
	executeStateCommit = func(tx *stateWriteTransaction) error {
		if err := original(tx); err != nil {
			return err
		}
		return errors.New("injected acknowledgement loss")
	}
	calls := 0
	err := retryRestoreTestWrite(t.Context(), func(ctx context.Context) error {
		calls++
		_, err := store.UpdateContext(ctx, record.Source, record.ContextID, record.AttemptID, restoreTestUpdate(record))
		return err
	})
	executeStateCommit = original
	var unknown *statefile.CommitOutcomeUnknownError
	if !errors.As(err, &unknown) || calls != 1 {
		t.Fatalf("uncertain committed update replayed: calls=%d err=%v", calls, err)
	}
	if got := restoreTestFind(t, restoreTestLoad(t, store), record.Source, record.ContextID); !got.LaunchAccepted {
		t.Fatal("acknowledgement-loss fixture did not actually commit")
	}
}
