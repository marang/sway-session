package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"

	"github.com/marang/sway-session/internal/statefile"
)

var (
	ErrWorkEnvironmentNotFound     = errors.New("work environment not found")
	ErrWorkEnvironmentConflict     = errors.New("work environment changed concurrently")
	ErrWorkEnvironmentNameConflict = errors.New("work environment name already exists")
)

const workEnvironmentsSQL = `CREATE TABLE work_environments (
	id TEXT PRIMARY KEY CHECK (length(id) = 36),
	name TEXT NOT NULL UNIQUE COLLATE BINARY CHECK (length(CAST(name AS BLOB)) BETWEEN 1 AND 256),
	revision INTEGER NOT NULL CHECK (revision > 0),
	primary_context_id TEXT NOT NULL DEFAULT '' CHECK (length(primary_context_id) IN (0, 36))
) STRICT`

// Context references deliberately have no foreign key to contexts. Purging a
// context must retain its unresolved memberships and primary designation.
const workEnvironmentMembersSQL = `CREATE TABLE work_environment_members (
	environment_id TEXT NOT NULL REFERENCES work_environments(id) ON DELETE CASCADE CHECK (length(environment_id) = 36),
	context_id TEXT NOT NULL CHECK (length(context_id) = 36),
	ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
	PRIMARY KEY (environment_id, context_id),
	UNIQUE (environment_id, ordinal)
) STRICT`

func workEnvironmentSchemaV1() map[string]stateBackupSchemaObject {
	return map[string]stateBackupSchemaObject{
		"work_environments":                           {kind: "table", table: "work_environments", extension: "environments", sql: workEnvironmentsSQL},
		"sqlite_autoindex_work_environments_1":        {kind: "index", table: "work_environments", extension: "environments"},
		"sqlite_autoindex_work_environments_2":        {kind: "index", table: "work_environments", extension: "environments"},
		"work_environment_members":                    {kind: "table", table: "work_environment_members", extension: "environments", sql: workEnvironmentMembersSQL},
		"sqlite_autoindex_work_environment_members_1": {kind: "index", table: "work_environment_members", extension: "environments"},
		"sqlite_autoindex_work_environment_members_2": {kind: "index", table: "work_environment_members", extension: "environments"},
	}
}

// Reads recognize the complete optional v1 extension without creating it.
func workEnvironmentTablesExist(ctx context.Context, queryer stateQueryer) (bool, error) {
	expected := workEnvironmentSchemaV1()
	rows, err := queryer.QueryContext(ctx, `SELECT type, name, tbl_name, rootpage, sql FROM main.sqlite_schema
		WHERE tbl_name IN ('work_environments', 'work_environment_members')
		OR name IN ('work_environments', 'work_environment_members', 'sqlite_autoindex_work_environments_1',
		'sqlite_autoindex_work_environments_2', 'sqlite_autoindex_work_environment_members_1', 'sqlite_autoindex_work_environment_members_2')`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		found = true
		var kind, name, table string
		var rootPage sql.NullInt64
		var definition sql.NullString
		if err := rows.Scan(&kind, &name, &table, &rootPage, &definition); err != nil {
			return false, err
		}
		want, known := expected[name]
		if !known || kind != want.kind || table != want.table || definition.Valid != (want.sql != "") || normalizeStateBackupSchemaSQL(definition.String) != normalizeStateBackupSchemaSQL(want.sql) || !rootPage.Valid || rootPage.Int64 <= 1 || rootPage.Int64 > stateDatabaseMaxPageCount {
			return false, fmt.Errorf("unsupported work environment schema object %q", name)
		}
		delete(expected, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return false, err
	}
	if found && len(expected) != 0 {
		return false, errors.New("work environment schema is incomplete")
	}
	return found, nil
}

func createWorkEnvironmentTables(ctx context.Context, tx stateTransaction) error {
	exists, err := workEnvironmentTablesExist(ctx, tx)
	if err != nil || exists {
		return err
	}
	for _, statement := range []string{workEnvironmentsSQL, workEnvironmentMembersSQL} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

type WorkEnvironmentStore struct{ root string }

func WorkEnvironmentStoreFor(root string) WorkEnvironmentStore {
	return WorkEnvironmentStore{root: root}
}

func (store WorkEnvironmentStore) ListContext(ctx context.Context) ([]WorkEnvironment, error) {
	return store.read(ctx, "")
}

func (store WorkEnvironmentStore) LoadContext(ctx context.Context, id EnvironmentID) (WorkEnvironment, error) {
	if err := id.Validate(); err != nil {
		return WorkEnvironment{}, err
	}
	values, err := store.read(ctx, id)
	if err != nil {
		return WorkEnvironment{}, err
	}
	if len(values) == 0 {
		return WorkEnvironment{}, ErrWorkEnvironmentNotFound
	}
	return values[0], nil
}

func (store WorkEnvironmentStore) read(ctx context.Context, id EnvironmentID) ([]WorkEnvironment, error) {
	database, err := openStateDatabase(ctx, store.root, false)
	if errors.Is(err, os.ErrNotExist) {
		return []WorkEnvironment{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer database.Close()
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	values, err := loadWorkEnvironments(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return values, commitStateRead(ctx, tx)
}

func loadWorkEnvironments(ctx context.Context, queryer stateQueryer, id EnvironmentID) ([]WorkEnvironment, error) {
	values := []WorkEnvironment{}
	exists, err := workEnvironmentTablesExist(ctx, queryer)
	if err != nil || !exists {
		return values, err
	}
	query := `SELECT e.id, e.name, e.revision, e.primary_context_id, m.context_id, m.ordinal
		FROM work_environments e LEFT JOIN work_environment_members m ON m.environment_id = e.id`
	var args []any
	if id != "" {
		query += " WHERE e.id = ?"
		args = append(args, id)
	}
	query += " ORDER BY e.name COLLATE BINARY, e.id, m.ordinal"
	rows, err := queryer.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lastOrdinal int64
	for rows.Next() {
		var value WorkEnvironment
		var member sql.NullString
		var ordinal sql.NullInt64
		if err := rows.Scan(&value.ID, &value.Name, &value.Revision, &value.Primary, &member, &ordinal); err != nil {
			return nil, err
		}
		if len(values) == 0 || values[len(values)-1].ID != value.ID {
			value.Members = []ContextID{}
			values = append(values, value)
			lastOrdinal = -1
		}
		if member.Valid != ordinal.Valid {
			return nil, errors.New("incomplete work environment member")
		}
		if member.Valid {
			if ordinal.Int64 < 0 || ordinal.Int64 <= lastOrdinal {
				return nil, errors.New("invalid work environment member order")
			}
			values[len(values)-1].Members = append(values[len(values)-1].Members, ContextID(member.String))
			lastOrdinal = ordinal.Int64
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for _, value := range values {
		if err := value.Validate(); err != nil {
			return nil, fmt.Errorf("stored work environment: %w", err)
		}
	}
	return values, nil
}

func requireWorkEnvironmentNameAvailable(ctx context.Context, queryer stateQueryer, name string, except EnvironmentID) error {
	var count int
	if err := queryer.QueryRowContext(ctx, "SELECT count(*) FROM work_environments WHERE name = ? COLLATE BINARY AND id != ?", name, except).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrWorkEnvironmentNameConflict
	}
	return nil
}

// CreateContext returns the candidate ID with an unknown-commit error, allowing
// callers to reload that exact identity before considering another creation.
func (store WorkEnvironmentStore) CreateContext(ctx context.Context, name string) (WorkEnvironment, error) {
	if err := ValidateWorkEnvironmentName(name); err != nil {
		return WorkEnvironment{}, err
	}
	id, err := NewEnvironmentID()
	if err != nil {
		return WorkEnvironment{}, err
	}
	value := WorkEnvironment{ID: id, Name: name, Revision: 1, Members: []ContextID{}}
	database, err := openStateDatabase(ctx, store.root, true)
	if err != nil {
		return WorkEnvironment{}, err
	}
	defer database.Close()
	return withWorkEnvironmentMutation(ctx, store.root, func() (WorkEnvironment, error) {
		tx, err := beginStateWrite(ctx, database)
		if err != nil {
			return WorkEnvironment{}, err
		}
		defer tx.Rollback()
		if err := createWorkEnvironmentTables(ctx, tx); err != nil {
			return WorkEnvironment{}, err
		}
		if err := requireWorkEnvironmentNameAvailable(ctx, tx, name, ""); err != nil {
			return WorkEnvironment{}, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO work_environments (id, name, revision) VALUES (?, ?, 1)", id, name); err != nil {
			return WorkEnvironment{}, err
		}
		return value, commitStateWrite(ctx, tx)
	})
}

type workEnvironmentEdit struct {
	kind   string
	name   string
	member ContextID
}

func (store WorkEnvironmentStore) RenameContext(ctx context.Context, id EnvironmentID, revision int64, name string) (WorkEnvironment, error) {
	if err := ValidateWorkEnvironmentName(name); err != nil {
		return WorkEnvironment{}, err
	}
	return store.edit(ctx, id, revision, workEnvironmentEdit{kind: "rename", name: name})
}

func (store WorkEnvironmentStore) AddMemberContext(ctx context.Context, id EnvironmentID, revision int64, member ContextID) (WorkEnvironment, error) {
	if err := member.Validate(); err != nil {
		return WorkEnvironment{}, err
	}
	return store.edit(ctx, id, revision, workEnvironmentEdit{kind: "add", member: member})
}

func (store WorkEnvironmentStore) RemoveMemberContext(ctx context.Context, id EnvironmentID, revision int64, member ContextID) (WorkEnvironment, error) {
	if err := member.Validate(); err != nil {
		return WorkEnvironment{}, err
	}
	return store.edit(ctx, id, revision, workEnvironmentEdit{kind: "remove", member: member})
}

func (store WorkEnvironmentStore) SetPrimaryContext(ctx context.Context, id EnvironmentID, revision int64, primary ContextID) (WorkEnvironment, error) {
	if primary != "" {
		if err := primary.Validate(); err != nil {
			return WorkEnvironment{}, err
		}
	}
	return store.edit(ctx, id, revision, workEnvironmentEdit{kind: "primary", member: primary})
}

func (store WorkEnvironmentStore) DeleteContext(ctx context.Context, id EnvironmentID, revision int64) error {
	_, err := store.edit(ctx, id, revision, workEnvironmentEdit{kind: "delete"})
	return err
}

func (store WorkEnvironmentStore) edit(ctx context.Context, id EnvironmentID, revision int64, edit workEnvironmentEdit) (WorkEnvironment, error) {
	value, err := store.LoadContext(ctx, id)
	if err != nil {
		return WorkEnvironment{}, err
	}
	if revision <= 0 || value.Revision != revision {
		return WorkEnvironment{}, ErrWorkEnvironmentConflict
	}
	if revision == math.MaxInt64 {
		return WorkEnvironment{}, errors.New("work environment revision exhausted")
	}
	index := slices.Index(value.Members, edit.member)
	switch edit.kind {
	case "rename":
		value.Name = edit.name
	case "add":
		if index >= 0 {
			return WorkEnvironment{}, errors.New("context is already an environment member")
		}
		value.Members = append(value.Members, edit.member)
	case "remove":
		if index < 0 {
			return WorkEnvironment{}, errors.New("context is not an environment member")
		}
		value.Members = slices.Delete(value.Members, index, index+1)
		if value.Primary == edit.member {
			value.Primary = ""
		}
	case "primary":
		value.Primary = edit.member
	case "delete":
	default:
		return WorkEnvironment{}, errors.New("unsupported environment edit")
	}
	value.Revision++
	if err := value.Validate(); err != nil {
		return WorkEnvironment{}, err
	}
	database, err := openStateDatabase(ctx, store.root, false)
	if err != nil {
		return WorkEnvironment{}, err
	}
	defer database.Close()
	return withWorkEnvironmentMutation(ctx, store.root, func() (WorkEnvironment, error) {
		tx, err := beginStateWrite(ctx, database)
		if err != nil {
			return WorkEnvironment{}, err
		}
		defer tx.Rollback()
		exists, err := workEnvironmentTablesExist(ctx, tx)
		if err != nil {
			return WorkEnvironment{}, err
		}
		if !exists {
			return WorkEnvironment{}, ErrWorkEnvironmentConflict
		}
		if edit.kind == "rename" {
			if err := requireWorkEnvironmentNameAvailable(ctx, tx, value.Name, id); err != nil {
				return WorkEnvironment{}, err
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE work_environments SET name = ?, revision = ?, primary_context_id = ? WHERE id = ? AND revision = ?`, value.Name, value.Revision, value.Primary, id, revision)
		if err != nil {
			return WorkEnvironment{}, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return WorkEnvironment{}, err
		}
		if changed != 1 {
			return WorkEnvironment{}, ErrWorkEnvironmentConflict
		}
		switch edit.kind {
		case "add":
			if err := requireWorkEnvironmentContext(ctx, tx, edit.member); err != nil {
				return WorkEnvironment{}, err
			}
			var ordinal int64
			if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(ordinal), -1) FROM work_environment_members WHERE environment_id = ?", id).Scan(&ordinal); err != nil {
				return WorkEnvironment{}, err
			}
			if ordinal == math.MaxInt64 {
				return WorkEnvironment{}, errors.New("work environment member order exhausted")
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO work_environment_members (environment_id, context_id, ordinal) VALUES (?, ?, ?)", id, edit.member, ordinal+1)
		case "remove":
			_, err = tx.ExecContext(ctx, "DELETE FROM work_environment_members WHERE environment_id = ? AND context_id = ?", id, edit.member)
		case "delete":
			_, err = tx.ExecContext(ctx, "DELETE FROM work_environments WHERE id = ?", id)
		}
		if err != nil {
			return WorkEnvironment{}, err
		}
		return value, commitStateWrite(ctx, tx)
	})
}

// The same registry lock spans Reach's final definition check and external
// focus effect. Metadata edits linearize before or after that effect; they
// cannot invalidate a checked definition between the check and its command.
// Snapshot preparation stays outside this lock and the short SQL transaction.
func withWorkEnvironmentMutation(ctx context.Context, root string, action func() (WorkEnvironment, error)) (WorkEnvironment, error) {
	var value WorkEnvironment
	err := WithRegistryLockContext(ctx, root, func(*statefile.LockedPrivateDirectory) error {
		var actionErr error
		value, actionErr = action()
		return actionErr
	})
	return value, err
}

func requireWorkEnvironmentContext(ctx context.Context, queryer stateQueryer, id ContextID) error {
	var version int
	var payload []byte
	if err := queryer.QueryRowContext(ctx, "SELECT encoding_version, payload FROM contexts WHERE id = ?", id).Scan(&version, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrContextNotFound, id)
		}
		return err
	}
	if version != ContextsSchemaVersion {
		return &UnsupportedVersionError{Document: "context row", Got: version, Want: ContextsSchemaVersion}
	}
	var value Context
	if err := decodeDatabasePayload("environment member context", payload, &value); err != nil {
		return err
	}
	if value.ID != id {
		return errors.New("environment member context key disagrees with payload")
	}
	return value.Validate()
}
