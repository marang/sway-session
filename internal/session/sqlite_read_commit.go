package session

import (
	"context"
	"database/sql"
	"errors"
)

// database/sql may finish its cancellation rollback before Commit runs.
// Preserve the request's cancellation instead of exposing that scheduling
// race as an unrelated closed-transaction error. Live-context errors survive.
func commitStateRead(ctx context.Context, tx *sql.Tx) error {
	err := tx.Commit()
	if errors.Is(err, sql.ErrTxDone) && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
