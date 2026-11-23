package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// A run's sub-agents: each is a session of its own (a conversation, a transcript) that the main agent
// started for one task, with a private scratchpad of its own. Its name is s1, s2… in the order the run
// started them. Only the main agent writes the thread's scratchpad; a sub-agent's notes live here, with
// the run, and go when the run's thread does.
const schemaSubAgents = `
CREATE TABLE sub_agents (
	id           INTEGER PRIMARY KEY,
	run_id       INTEGER NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
	name         TEXT    NOT NULL,
	title        TEXT    NOT NULL,
	task         TEXT    NOT NULL,
	conversation INTEGER NOT NULL DEFAULT 0,
	status       TEXT    NOT NULL DEFAULT 'running', -- running, then its outcome
	summary      TEXT    NOT NULL DEFAULT '',        -- its transcript, as JSON
	started_at   INTEGER NOT NULL,
	finished_at  INTEGER NOT NULL DEFAULT 0,
	UNIQUE (run_id, name)
);
CREATE TABLE sub_notes (
	sub_id      INTEGER NOT NULL REFERENCES sub_agents(id) ON DELETE CASCADE,
	n           INTEGER NOT NULL,
	title       TEXT    NOT NULL,
	importance  TEXT    NOT NULL,
	description TEXT    NOT NULL,
	body        TEXT    NOT NULL,
	created_at  INTEGER NOT NULL,
	PRIMARY KEY (sub_id, n)
);
`

// SubAgent is a sub-agent's row.
type SubAgent struct {
	ID           int64
	RunID        int64
	Name         string // s1, s2…
	Title        string
	Task         string
	Conversation int64
	Status       string // "running", then how it ended
	Summary      string
	StartedAt    time.Time
	FinishedAt   time.Time // zero while running
}

// StartSub records a sub-agent of a run, with a conversation of its own, and returns it.
func (s *Store) StartSub(run int64, name, title, task string) (*SubAgent, error) {
	r, err := s.AgentRun(run)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	conv, err := s.newConversation(tx, *r)
	if err != nil {
		return nil, err
	}
	res, err := tx.Exec(`INSERT INTO sub_agents (run_id, name, title, task, conversation, started_at) VALUES (?, ?, ?, ?, ?, ?)`, run, name, title, task, conv, now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &SubAgent{ID: id, RunID: run, Name: name, Title: title, Task: task, Conversation: conv, Status: "running", StartedAt: time.Unix(now(), 0)}, nil
}

// SaveSub stores a sub-agent's transcript and, when status is not "running", ends it with it.
func (s *Store) SaveSub(id int64, status, summary string) error {
	finished := int64(0)
	if status != "running" {
		finished = now()
	}
	_, err := s.db.Exec(`UPDATE sub_agents SET status = ?, summary = ?, finished_at = ? WHERE id = ?`, status, summary, finished, id)
	return err
}

// Subs lists a run's sub-agents in the order they started.
func (s *Store) Subs(run int64) ([]SubAgent, error) {
	rows, err := s.db.Query(`SELECT id, run_id, name, title, task, conversation, status, summary, started_at, finished_at FROM sub_agents WHERE run_id = ? ORDER BY id`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SubAgent
	for rows.Next() {
		var a SubAgent
		var started, finished int64
		if err := rows.Scan(&a.ID, &a.RunID, &a.Name, &a.Title, &a.Task, &a.Conversation, &a.Status, &a.Summary, &started, &finished); err != nil {
			return nil, err
		}
		a.StartedAt = time.Unix(started, 0)
		if finished != 0 {
			a.FinishedAt = time.Unix(finished, 0)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ErrNoSub means the run has no sub-agent of that name.
var ErrNoSub = errors.New("no such sub-agent in this run")

// SubNotes are the notes of one sub-agent of a run, by its name: never another run's.
func (s *Store) SubNotes(run int64, name string) ([]PadEntry, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM sub_agents WHERE run_id = ? AND name = ?`, run, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSub
	}
	if err != nil {
		return nil, err
	}
	return s.SubPad(id, name).List()
}

// SubPad is one sub-agent's private scratchpad. It adds and lists entries of that sub-agent only.
type SubPad struct {
	s    *Store
	id   int64
	name string
}

// SubPad returns the scratchpad of a sub-agent, whose entries are by its name.
func (s *Store) SubPad(id int64, name string) *SubPad { return &SubPad{s, id, name} }

// Add appends an entry and returns its number.
func (p *SubPad) Add(e PadEntry) (int, error) {
	if e.Importance != Low && e.Importance != High {
		e.Importance = Medium
	}
	tx, err := p.s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(n), 0) + 1 FROM sub_notes WHERE sub_id = ?`, p.id).Scan(&n); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO sub_notes (sub_id, n, title, importance, description, body, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.id, n, e.Title, e.Importance, strings.Join(strings.Fields(e.Description), " "), e.Body, now()); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// List returns every entry, in order.
func (p *SubPad) List() ([]PadEntry, error) {
	rows, err := p.s.db.Query(`SELECT n, title, importance, description, body, created_at FROM sub_notes WHERE sub_id = ? ORDER BY n`, p.id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PadEntry
	for rows.Next() {
		e := PadEntry{Author: p.name}
		var at int64
		if err := rows.Scan(&e.N, &e.Title, &e.Importance, &e.Description, &e.Body, &at); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// dropSubs deletes the private notes of the sub-agents of a thread's runs, and cuts their summaries to
// what is kept.
func (s *Store) dropSubs(tx *sql.Tx, thread int64) error {
	if _, err := tx.Exec(`DELETE FROM sub_notes WHERE sub_id IN (SELECT a.id FROM sub_agents a JOIN agent_runs r ON r.id = a.run_id WHERE r.thread = ?)`, thread); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT a.id, a.summary FROM sub_agents a JOIN agent_runs r ON r.id = a.run_id WHERE r.thread = ?`, thread)
	if err != nil {
		return err
	}
	type sub struct {
		id  int64
		sum string
	}
	var subs []sub
	for rows.Next() {
		var x sub
		if err := rows.Scan(&x.id, &x.sum); err != nil {
			rows.Close()
			return err
		}
		subs = append(subs, x)
	}
	rows.Close()
	for _, x := range subs {
		if _, err := tx.Exec(`UPDATE sub_agents SET summary = ? WHERE id = ?`, slimSummary(x.sum), x.id); err != nil {
			return err
		}
	}
	return nil
}

