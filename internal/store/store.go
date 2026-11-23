// Package store keeps all of Autobot's settings and data in one SQLite file.
package store

import (
	"crypto/cipher"
	"database/sql"
	"errors"
	"fmt"
	"sync"
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
	db    *sql.DB
	path  string      // the database file; the runs folder is beside it
	files sync.Mutex  // one write to a run file at a time
	aead  cipher.AEAD // seals GitHub tokens and provider API keys; set once at start by UseKey
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
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// migrations[i] takes the schema from version i to i+1. Append only; never edit a released entry.
var migrations = []string{schemaV1, schemaV2, schemaV3, schemaV4, schemaV5, schemaV6, schemaV7, schemaV8, schemaV9, schemaV10, schemaV11, schemaV12, schemaV13, schemaV14, schemaV15, schemaV16, schemaV17, schemaV18, schemaPoller, schemaItems, schemaRunFiles, schemaThreads, schemaSubAgents}

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

// A run of a private repository gets its code through Autobot, which holds the token; any other
// run's connector downloads straight from GitHub.
const schemaV4 = `
ALTER TABLE test_runs ADD COLUMN private INTEGER NOT NULL DEFAULT 0;
`

// The LLM inference providers Autobot may call, and the models it may use from each. The API key is
// sealed with the key file like a GitHub token; auth names how Autobot signs in (only api_key so far).
const schemaV5 = `
CREATE TABLE providers (
	id         INTEGER PRIMARY KEY,
	name       TEXT    NOT NULL UNIQUE COLLATE NOCASE,
	kind       TEXT    NOT NULL,                -- openai (any OpenAI-compatible API) or anthropic
	endpoint   TEXT    NOT NULL,
	auth       TEXT    NOT NULL DEFAULT 'api_key',
	api_key    BLOB    NOT NULL DEFAULT x'',    -- sealed; empty when there is no key
	created_by INTEGER REFERENCES users(id),
	updated_at INTEGER NOT NULL
);
CREATE TABLE provider_models (
	provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
	model_id    TEXT    NOT NULL,
	PRIMARY KEY (provider_id, model_id)
);
`

// A provider belongs to the user who pays for it, who lends it to workspaces. Names need only be
// unique per owner. API keys are the only sign-in, so the auth column goes. SQLite cannot change a
// table's constraints, so providers and provider_models are rebuilt (models first, so dropping the
// old providers table cascades nothing).
const schemaV6 = `
CREATE TABLE providers_new (
	id         INTEGER PRIMARY KEY,
	owner_id   INTEGER NOT NULL REFERENCES users(id),
	name       TEXT    NOT NULL COLLATE NOCASE,
	kind       TEXT    NOT NULL,
	endpoint   TEXT    NOT NULL,
	api_key    BLOB    NOT NULL DEFAULT x'',
	updated_at INTEGER NOT NULL,
	UNIQUE (owner_id, name)
);
INSERT INTO providers_new (id, owner_id, name, kind, endpoint, api_key, updated_at)
	SELECT id, COALESCE(created_by, (SELECT min(id) FROM users WHERE is_admin = 1)), name, kind, endpoint, api_key, updated_at
	FROM providers;
CREATE TABLE provider_models_new (
	provider_id INTEGER NOT NULL REFERENCES providers_new(id) ON DELETE CASCADE,
	model_id    TEXT    NOT NULL,
	PRIMARY KEY (provider_id, model_id)
);
INSERT INTO provider_models_new SELECT provider_id, model_id FROM provider_models;
DROP TABLE provider_models;
DROP TABLE providers;
ALTER TABLE providers_new RENAME TO providers;
ALTER TABLE provider_models_new RENAME TO provider_models;
-- The workspaces each provider is lent to.
CREATE TABLE provider_loans (
	provider_id  INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	PRIMARY KEY (provider_id, workspace_id)
);
`

// Case files: one per issue or pull request, a Markdown log of findings. Writers add to it in
// contributions: a draft its author may revise until it is finalized, when it is appended to the
// file and becomes immutable, with the lines it occupies recorded. People may edit the file
// directly; each such edit is recorded with a note of the lines it changed. The uuid is what the
// case is shared by: random, unguessable, never listed.
const schemaV7 = `
CREATE TABLE cases (
	uuid         TEXT    PRIMARY KEY,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	number       INTEGER NOT NULL,
	kind         TEXT    NOT NULL,            -- pr or issue
	title        TEXT    NOT NULL,
	contents     TEXT    NOT NULL,
	version      INTEGER NOT NULL DEFAULT 1,  -- goes up with every change, for edit conflicts
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL,
	UNIQUE (workspace_id, number)
);
CREATE TABLE case_contributions (
	id           INTEGER PRIMARY KEY,
	case_uuid    TEXT    NOT NULL REFERENCES cases(uuid) ON DELETE CASCADE,
	author       TEXT    NOT NULL,            -- a person's name, an agent's, or Autobot
	kind         TEXT    NOT NULL,            -- system, agent, human (later: compact)
	draft        TEXT    NOT NULL DEFAULT '', -- an open contribution's text, not yet in the file
	note         TEXT    NOT NULL DEFAULT '', -- a human edit: which lines it changed
	start_line   INTEGER,                     -- the lines a finalized contribution was appended as
	end_line     INTEGER,
	started_at   INTEGER NOT NULL,
	finalized_at INTEGER
);
CREATE INDEX case_contributions_by_case ON case_contributions(case_uuid, id);
`

// A machine can be taken offline for a while without removing it: it gets no new runs.
const schemaV8 = `
ALTER TABLE machines ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1;
`

// A workspace's workflows, one per kind (pr, issue): the canvas as JSON (nodes with their settings,
// and links), checked by package workflow before it is stored.
const schemaV9 = `
CREATE TABLE workflows (
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	kind         TEXT    NOT NULL,
	graph        TEXT    NOT NULL,
	version      INTEGER NOT NULL,
	updated_by   INTEGER REFERENCES users(id),
	updated_at   INTEGER NOT NULL,
	PRIMARY KEY (workspace_id, kind)
);
`

// What a provider's endpoint was found to serve besides its kind's own API: the decision routes
// /decisions and /systemone, and when it was last asked (0: never).
const schemaV10 = `
ALTER TABLE providers ADD COLUMN decisions_route INTEGER NOT NULL DEFAULT 0;
ALTER TABLE providers ADD COLUMN systemone_route INTEGER NOT NULL DEFAULT 0;
ALTER TABLE providers ADD COLUMN routes_checked  INTEGER NOT NULL DEFAULT 0;
`

// Agent runs: one run of an agent block, a test from the canvas or (later) a workflow's. The run
// keeps a small summary (plan, outcome, report, tokens); its conversation is one row per message,
// gzipped and written once, so a long run is neither rewritten every turn nor stored uncompressed.
const schemaV11 = `
CREATE TABLE agent_runs (
	id           INTEGER PRIMARY KEY,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	kind         TEXT    NOT NULL,
	node_id      TEXT    NOT NULL,
	node_name    TEXT    NOT NULL,
	item         INTEGER NOT NULL DEFAULT 0,
	test         INTEGER NOT NULL DEFAULT 0,
	model        TEXT    NOT NULL,
	status       TEXT    NOT NULL,
	summary      TEXT    NOT NULL DEFAULT '{}',
	started_by   INTEGER REFERENCES users(id),
	started_at   INTEGER NOT NULL,
	finished_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX agent_runs_node ON agent_runs (workspace_id, kind, node_id, item);
CREATE TABLE agent_messages (
	run_id INTEGER NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
	seq    INTEGER NOT NULL,
	body   BLOB    NOT NULL,
	PRIMARY KEY (run_id, seq)
);
`

// A workspace's Discord channels, each a webhook. The webhook's address is its secret (anyone with
// it can post to the channel), so it is sealed like a key; its id is kept in the clear to show.
const schemaV12 = `
CREATE TABLE discord_channels (
	id           INTEGER PRIMARY KEY,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	name         TEXT    NOT NULL,
	webhook_id   TEXT    NOT NULL,
	webhook      BLOB    NOT NULL,
	added_by     INTEGER REFERENCES users(id),
	added_at     INTEGER NOT NULL,
	UNIQUE (workspace_id, name)
);
`

// The Discord webhook channels of schemaV12 are gone: Discord is reached through a bot instead
// (Derek, 2026-11-23), which can post into a case's forum post, show buttons and read mentions.
const schemaV13 = `DROP TABLE discord_channels;`

// Workflow runs: one run of a workflow on one pull request or issue, from one event node. The run
// keeps the workflow as it was when it started (later edits do not change a run under way), and
// each step's state, port and output as it goes, so a run can be read back, and later resumed.
// A case's values are its steps' output tags: one value per name, replaced by every write.
const schemaV14 = `
CREATE TABLE workflow_runs (
	id           INTEGER PRIMARY KEY,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	kind         TEXT    NOT NULL,
	event_node   TEXT    NOT NULL,
	item         INTEGER NOT NULL,
	dry_run      INTEGER NOT NULL DEFAULT 0,
	graph        TEXT    NOT NULL,
	status       TEXT    NOT NULL,
	note         TEXT    NOT NULL DEFAULT '',
	started_by   INTEGER REFERENCES users(id),
	started_at   INTEGER NOT NULL,
	finished_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX workflow_runs_item ON workflow_runs (workspace_id, kind, item);
CREATE TABLE workflow_steps (
	run_id      INTEGER NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
	node_id     TEXT    NOT NULL,
	status      TEXT    NOT NULL,
	port        TEXT    NOT NULL DEFAULT '',
	output      TEXT    NOT NULL DEFAULT '',
	error       TEXT    NOT NULL DEFAULT '',
	agent_run   INTEGER NOT NULL DEFAULT 0,
	started_at  INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (run_id, node_id)
);
CREATE TABLE case_values (
	case_uuid  TEXT    NOT NULL REFERENCES cases(uuid) ON DELETE CASCADE,
	name       TEXT    NOT NULL,
	value      TEXT    NOT NULL,
	node_id    TEXT    NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (case_uuid, name)
);
`

// A session is a test run that holds its machine open for an agent: the connector starts the
// container with the code and keeps it, and Autobot sends it commands until the run is closed.
const schemaV15 = `
ALTER TABLE test_runs ADD COLUMN session INTEGER NOT NULL DEFAULT 0;
`

// Settings of the whole instance, by name, such as the hosts providers may use on the local network.
const schemaV16 = `
CREATE TABLE settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// The comment a GitHub Comment node posted on a case's item, so a later run edits it instead of
// adding another; and the forum post of a case on Discord, with its first message (the summary).
const schemaV17 = `
CREATE TABLE github_comments (
	case_uuid  TEXT    NOT NULL REFERENCES cases(uuid) ON DELETE CASCADE,
	node_id    TEXT    NOT NULL,
	comment_id INTEGER NOT NULL,
	url        TEXT    NOT NULL,
	PRIMARY KEY (case_uuid, node_id)
);
CREATE TABLE discord_posts (
	case_uuid  TEXT PRIMARY KEY REFERENCES cases(uuid) ON DELETE CASCADE,
	thread_id  TEXT NOT NULL,
	summary_id TEXT NOT NULL
);
`

// Approvals: a Human Approval step's request, waiting for a person (on Discord, or on the run's
// page) or for its time limit. Its status changes once, from pending, and the first answer wins.
const schemaV18 = `
CREATE TABLE approvals (
	id         INTEGER PRIMARY KEY,
	run_id     INTEGER NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
	node_id    TEXT    NOT NULL,
	request    TEXT    NOT NULL,
	status     TEXT    NOT NULL DEFAULT 'pending',
	channel    TEXT    NOT NULL DEFAULT '',
	message    TEXT    NOT NULL DEFAULT '',
	decided_by TEXT    NOT NULL DEFAULT '',
	decided_on TEXT    NOT NULL DEFAULT '',
	decided_at INTEGER NOT NULL DEFAULT 0,
	expires_at INTEGER NOT NULL,
	UNIQUE (run_id, node_id)
);
`
