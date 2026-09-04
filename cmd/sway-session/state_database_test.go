package main

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
)

func openRawStateDatabaseForTest(t *testing.T, root string) *sql.DB {
	t.Helper()
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     filepath.Join(root, sessionstate.StateDatabaseFilename),
		RawQuery: "mode=rw&_busy_timeout=1&_foreign_keys=on&_txlock=immediate",
	}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.PingContext(t.Context()); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func execStateDatabaseForTest(t *testing.T, root string, statement string, arguments ...any) {
	t.Helper()
	database := openRawStateDatabaseForTest(t, root)
	if _, err := database.ExecContext(t.Context(), statement, arguments...); err != nil {
		t.Fatal(err)
	}
}
