// Package store keeps all of Autobot's settings and data in one SQLite file.
package store

import (
	"crypto/cipher"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a lookup matches nothing.
var ErrNotFound = errors.New("not found")

// InputError is a problem with what a person entered. Its message is written to be shown to them.
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &InputError{fmt.Sprintf(format, a...)} }

// Store is the database. It is safe for concurrent use.
type Store struct {
	db   *sql.DB
	aead cipher.AEAD // seals GitHub tokens; set once at start by UseKey
}

// querier is satisfied by both *sql.DB and *sql.Tx, so helpers can run inside or outside a transaction.
type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Open opens (creating if needed) the database at path and brings its schema up to date.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: SQLite has a single writer anyway, and this rules out SQLITE_BUSY between our
	// own requests. Code holding a transaction must therefore use the tx, never s.db.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// migrations[i] takes the schema from version i to i+1. Append only; never edit a released entry.
var migrations = []string{schemaV1, schemaV2, schemaV3}

const schemaV1 = `
CREATE TABLE users (
	id            INTEGER PRIMARY KEY,
	username      TEXT    NOT NULL UNIQUE COLLATE NOCASE,
	display_name  TEXT    NOT NULL DEFAULT '',
	password_hash TEXT    NOT NULL,
	is_admin      INTEGER NOT NULL DEFAULT 0,
	disabled      INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL
);
CREATE TABLE sessions (
	token_hash TEXT    PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	csrf       TEXT    NOT NULL,
	expires_at INTEGER NOT NULL
);
-- A workspace is named after its GitHub repository: owner/repo.
CREATE TABLE workspaces (
	id         INTEGER PRIMARY KEY,
	name       TEXT    NOT NULL UNIQUE COLLATE NOCASE,
	created_at INTEGER NOT NULL
);
CREATE TABLE workspace_members (
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	PRIMARY KEY (workspace_id, user_id)
);
-- Every charter save is a new revision; the highest rev is the current charter.
CREATE TABLE charter_revisions (
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	rev          INTEGER NOT NULL,
	body         TEXT    NOT NULL,
	author_id    INTEGER REFERENCES users(id),
	created_at   INTEGER NOT NULL,
	PRIMARY KEY (workspace_id, rev)
);
-- A workspace's history: one line per thing that happened to it.
CREATE TABLE events (
	id           INTEGER PRIMARY KEY,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	actor_id     INTEGER REFERENCES users(id),
	summary      TEXT    NOT NULL,
	created_at   INTEGER NOT NULL
);
CREATE INDEX events_by_workspace ON events(workspace_id, id);
`

// The GitHub user Autobot acts as in a workspace, and that user's token, sealed with the key file
// so that the database alone does not reveal it.
const schemaV2 = `
CREATE TABLE workspace_github (
	workspace_id INTEGER PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
	login        TEXT    NOT NULL,
	token        BLOB    NOT NULL,
	saved_by     INTEGER REFERENCES users(id),
	saved_at     INTEGER NOT NULL
);
`

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this binary knows (%d)", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("schema migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// now is the clock, in Unix seconds. Tests may replace it.
var now = func() int64 { return time.Now().Unix() }

func addEvent(q querier, workspaceID int64, actor *User, summary string) error {
	var actorID any
	if actor != nil {
		actorID = actor.ID
	}
	_, err := q.Exec(`INSERT INTO events (workspace_id, actor_id, summary, created_at) VALUES (?, ?, ?, ?)`,
		workspaceID, actorID, summary, now())
	return err
}

// Donated test machines and the test runs on them. A machine's agent signs in with a token that
// is shown once when the machine is added; only its hash is kept.
const schemaV3 = `
CREATE TABLE machines (
	id           INTEGER PRIMARY KEY,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	name         TEXT    NOT NULL,
	token_hash   TEXT    NOT NULL UNIQUE,
	revoked      INTEGER NOT NULL DEFAULT 0,
	created_by   INTEGER REFERENCES users(id),
	created_at   INTEGER NOT NULL
);
CREATE TABLE test_runs (
	id           INTEGER PRIMARY KEY,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	machine_id   INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
	started_by   INTEGER REFERENCES users(id),
	script       TEXT    NOT NULL,            -- the bash script the job runs
	ref          TEXT    NOT NULL DEFAULT '', -- the branch, tag or commit asked for
	commit_sha   TEXT    NOT NULL DEFAULT '', -- the commit checked out, or '' for none
	gpus         INTEGER NOT NULL,
	timeout_sec  INTEGER NOT NULL,
	state        TEXT    NOT NULL,            -- queued, running, passed, failed, error
	cancel       INTEGER NOT NULL DEFAULT 0,  -- someone asked to stop it
	output       TEXT    NOT NULL DEFAULT '',
	summary      TEXT    NOT NULL DEFAULT '', -- one line on how it ended
	created_at   INTEGER NOT NULL,
	started_at   INTEGER,
	ended_at     INTEGER
);
CREATE INDEX test_runs_by_machine ON test_runs(machine_id, id);
`
