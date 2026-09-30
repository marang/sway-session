package session

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestReadCommitCancellationRetainsItsCauseAfterRollback(t *testing.T) {
	database, err := openStateDatabase(t.Context(), filepath.Join(t.TempDir(), "state"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		tx, err := database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if cancelled {
			cancel()
		}
		// Force the ordering that database/sql's cancellation goroutine can
		// produce. Both competing rollback outcomes are legitimate here.
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Fatal(err)
		}
		want := sql.ErrTxDone
		if cancelled {
			want = context.Canceled
		}
		if err := commitStateRead(ctx, tx); !errors.Is(err, want) {
			t.Fatalf("cancelled=%t: got %v, want %v", cancelled, err, want)
		}
		cancel()
	}
}
