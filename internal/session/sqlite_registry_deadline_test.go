package session

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
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
			var tx *sql.Tx
			connector := &snapshotCancelConnector{dsn: database.dsn, afterClose: func() error {
				if deadlineExpired {
					<-ctx.Done()
				} else {
					cancel()
				}
				// Commit does not mutate a cancelled transaction. Wait until the
				// real database/sql rollback has claimed it, while Rows.Close
				// still prevents the driver's rollback from completing.
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					err := tx.Commit()
					if errors.Is(err, sql.ErrTxDone) {
						return nil
					}
					if !errors.Is(err, want) {
						return fmt.Errorf("cancelled Commit probe: %w", err)
					}
					runtime.Gosched()
				}
				return errors.New("cancellation rollback did not claim the read transaction")
			}}
			db := sql.OpenDB(connector)
			defer db.Close()
			tx, err = db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			rows, _, _, err := loadStoredRegistryRowsTransaction(ctx, tx)
			if !errors.Is(err, want) || !strings.HasPrefix(err.Error(), "finish stored registry snapshot:") {
				t.Fatalf("snapshot error = %v, want %v at Commit after automatic rollback", err, want)
			}
			if rows != nil {
				t.Fatal("cancelled snapshot returned rows")
			}
		})
	}
}

// Only the final result's Close is instrumented; all queries and transaction
// lifecycle operations run through SQLite and database/sql unchanged.
type snapshotCancelConnector struct {
	dsn        string
	afterClose func() error
}

func (connector *snapshotCancelConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := connector.Driver().Open(connector.dsn)
	if err != nil {
		return nil, err
	}
	return &snapshotCancelConn{Conn: conn, afterClose: connector.afterClose}, nil
}

func (*snapshotCancelConnector) Driver() driver.Driver { return &sqlite.Driver{} }

type snapshotCancelConn struct {
	driver.Conn
	afterClose func() error
}

func (conn *snapshotCancelConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	return conn.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
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
