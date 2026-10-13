package session

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestStoredRegistrySnapshotCancellationAfterRowsClose(t *testing.T) {
	for _, deadlineExpired := range []bool{false, true} {
		name := "cancelled"
		if deadlineExpired {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if err := RegistryStoreFor(root).Save(validRegistry()); err != nil {
				t.Fatal(err)
			}
			database, err := openStateDatabase(t.Context(), root, false)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			ctx, cancel := context.WithCancel(t.Context())
			want := context.Canceled
			if deadlineExpired {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), time.Second)
				want = context.DeadlineExceeded
			}
			defer cancel()
			// Hold child cancellation until Rows.Close has returned. This
			// deterministically recreates the parent-Done/transaction-live
			// window in which the old reentrant Commit probe could deadlock.
			delayed := &snapshotCancelContext{Context: ctx, resume: make(chan struct{})}
			defer delayed.release()
			rolledBack := make(chan error, 1)
			connector := &snapshotCancelConnector{dsn: database.dsn, rolledBack: rolledBack, afterClose: func() error {
				if deadlineExpired {
					select {
					case <-ctx.Done():
					case <-time.After(5 * time.Second):
						return errors.New("snapshot deadline did not expire")
					}
				} else {
					cancel()
				}
				return nil
			}}
			db := sql.OpenDB(connector)
			defer db.Close()
			tx, err := db.BeginTx(delayed, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			// Release every delayed callback before transaction/database cleanup,
			// including when row loading or the bounded wait fails.
			defer delayed.release()
			rows, _, _, err := loadStoredRegistryRowsTransaction(delayed, tx, func(ctx context.Context, tx *sql.Tx) error {
				// Rows.Close has now released its query and driver locks. Wait
				// for the real automatic rollback, then exercise the production
				// final commit's translation of ErrTxDone to the request cause.
				delayed.release()
				select {
				case err := <-rolledBack:
					if err != nil {
						return fmt.Errorf("automatic snapshot rollback: %w", err)
					}
				case <-time.After(5 * time.Second):
					return errors.New("automatic snapshot rollback did not finish")
				}
				return commitStateRead(ctx, tx)
			})
			if !errors.Is(err, want) || !strings.HasPrefix(err.Error(), "finish stored registry snapshot:") {
				t.Fatalf("snapshot error = %v, want %v at Commit after automatic rollback", err, want)
			}
			if rows != nil {
				t.Fatal("cancelled snapshot returned rows")
			}
		})
	}
}

// Value hides the parent's cancellation implementation so context.WithCancel
// uses AfterFunc. The callback gate delays propagation without replacing the
// real cancellation or database/sql rollback.
type snapshotCancelContext struct {
	context.Context
	resume chan struct{}
	once   sync.Once
}

func (*snapshotCancelContext) Value(any) any { return nil }

func (ctx *snapshotCancelContext) AfterFunc(callback func()) func() bool {
	return context.AfterFunc(ctx.Context, func() {
		<-ctx.resume
		callback()
	})
}

func (ctx *snapshotCancelContext) release() {
	ctx.once.Do(func() { close(ctx.resume) })
}

// The final result's Close triggers cancellation and the driver's Rollback
// reports completion; all SQL and transaction operations still use SQLite.
type snapshotCancelConnector struct {
	dsn        string
	afterClose func() error
	rolledBack chan<- error
}

func (connector *snapshotCancelConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := connector.Driver().Open(connector.dsn)
	if err != nil {
		return nil, err
	}
	return &snapshotCancelConn{Conn: conn, afterClose: connector.afterClose, rolledBack: connector.rolledBack}, nil
}

func (*snapshotCancelConnector) Driver() driver.Driver { return &sqlite.Driver{} }

type snapshotCancelConn struct {
	driver.Conn
	afterClose func() error
	rolledBack chan<- error
}

func (conn *snapshotCancelConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	tx, err := conn.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &snapshotCancelTx{Tx: tx, rolledBack: conn.rolledBack}, nil
}

type snapshotCancelTx struct {
	driver.Tx
	rolledBack chan<- error
}

func (tx *snapshotCancelTx) Rollback() error {
	err := tx.Tx.Rollback()
	tx.rolledBack <- err
	return err
}

func (conn *snapshotCancelConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := conn.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil || query != "SELECT id, ordinal, encoding_version, payload FROM contexts" {
		return rows, err
	}
	return &snapshotCancelRows{Rows: rows, afterClose: conn.afterClose}, nil
}

type snapshotCancelRows struct {
	driver.Rows
	afterClose func() error
}

func (rows *snapshotCancelRows) Close() error {
	if err := rows.Rows.Close(); err != nil {
		return err
	}
	return rows.afterClose()
}
