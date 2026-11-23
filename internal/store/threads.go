package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A thread is what an agent block knows about one item: (workspace, workflow kind, the block that
// started it, item). Every run of the block on the item, and of the blocks that continue it, is a
// new session (its own conversation, starting fresh) in the same thread. The thread holds the one
// scratchpad all those sessions share, and with it every run's final report: the newest opens the
// next run. It also notes the head commit and time the latest run saw, so the next knows what is new.
// Retention keeps a thread while its item is open plus 30 days, like conversations.
//
// The scratchpad is the agent's notes, outside its context. Entries are only ever added, and are
// never read across threads.
const schemaThreads = `
ALTER TABLE provider_models ADD COLUMN context_window INTEGER NOT NULL DEFAULT 0;
CREATE TABLE threads (
	id           INTEGER PRIMARY KEY,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	kind         TEXT    NOT NULL,
	node_id      TEXT    NOT NULL,
	item         INTEGER NOT NULL DEFAULT 0,
	standalone   INTEGER NOT NULL DEFAULT 0,  -- a test from the canvas: never continued
	deleted      INTEGER NOT NULL DEFAULT 0,  -- retention removed its scratchpad
	head         TEXT    NOT NULL DEFAULT '', -- the item's head commit the latest run saw
	seen         INTEGER NOT NULL DEFAULT 0,  -- when it last read the item, in unix seconds
	created_at   INTEGER NOT NULL,
	last_at      INTEGER NOT NULL
);
CREATE INDEX threads_by_node ON threads (workspace_id, kind, node_id, item);
CREATE TABLE scratchpad (
	thread_id   INTEGER NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
	n           INTEGER NOT NULL,
	author      TEXT    NOT NULL,
	title       TEXT    NOT NULL,
	importance  TEXT    NOT NULL,
	description TEXT    NOT NULL,
	body        TEXT    NOT NULL,
	report      INTEGER NOT NULL DEFAULT 0,   -- a run's final report, saved by Autobot
	created_at  INTEGER NOT NULL,
	PRIMARY KEY (thread_id, n)
);
ALTER TABLE agent_runs ADD COLUMN thread INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_runs ADD COLUMN writer_conversation INTEGER NOT NULL DEFAULT 0;
-- A thread takes one run at a time.
CREATE UNIQUE INDEX agent_runs_one_running_thread ON agent_runs (thread) WHERE status = 'running' AND thread != 0;
`

// ErrThreadBusy means the thread already has a run in progress.
var ErrThreadBusy = errors.New("the item's thread has a run in progress")

// ErrThreadGone means the thread was deleted by retention (or never was): nothing is written to it.
var ErrThreadGone = errors.New("the thread's scratchpad was deleted")

// Thread is a thread's row.
type Thread struct {
	ID          int64
	WorkspaceID int64
	Kind        string
	NodeID      string // the Agent block that started it
	Item        int
	Standalone  bool
	Head        string    // the item's head commit the latest run saw
	Seen        time.Time // when it last read the item; zero if never
	CreatedAt   time.Time
}

// LatestThread is the newest thread the block started on the item that can still be continued, or
// ErrNotFound.
func (s *Store) LatestThread(ws int64, kind, node string, item int) (*Thread, error) {
	var t Thread
	var seen, created int64
	err := s.db.QueryRow(`SELECT id, workspace_id, kind, node_id, item, standalone, head, seen, created_at FROM threads
		WHERE workspace_id = ? AND kind = ? AND node_id = ? AND item = ? AND standalone = 0 AND deleted = 0 ORDER BY id DESC LIMIT 1`, ws, kind, node, item).
		Scan(&t.ID, &t.WorkspaceID, &t.Kind, &t.NodeID, &t.Item, &t.Standalone, &t.Head, &seen, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = time.Unix(created, 0)
	if seen != 0 {
		t.Seen = time.Unix(seen, 0)
	}
	return &t, nil
}

// SetThreadSeen notes what a run that ended saw of the item: its head commit and when.
func (s *Store) SetThreadSeen(id int64, head string, seen time.Time) error {
	_, err := s.db.Exec(`UPDATE threads SET head = ?, seen = ?, last_at = ? WHERE id = ?`, head, seen.Unix(), now(), id)
	return err
}

// ThreadRunning reports whether a run of the thread is in progress.
func (s *Store) ThreadRunning(id int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM agent_runs WHERE thread = ? AND status = 'running'`, id).Scan(&n)
	return n > 0, err
}

// dropThread removes a thread's scratchpad. The row stays, marked deleted, for the runs that name it.
func (s *Store) dropThread(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM scratchpad WHERE thread_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE threads SET deleted = 1 WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Importance levels of a scratchpad entry.
const (
	Low    = "low"
	Medium = "medium"
	High   = "high"
)

// PadEntry is one note of a scratchpad.
type PadEntry struct {
	N           int // 1, 2, … within the thread
	Author      string
	Title       string
	Importance  string // low, medium or high
	Description string // one line
	Body        string
	Report      bool // a run's final report, saved by Autobot, not written by the agent
	At          time.Time
}

// Pad is one thread's scratchpad: it reads and adds entries of that thread, and no other.
type Pad struct {
	s      *Store
	thread int64
}

// Pad returns the scratchpad of a thread.
func (s *Store) Pad(thread int64) *Pad { return &Pad{s, thread} }

// Add appends an entry and returns its number. Author "" is "main"; an importance that is not low,
// medium or high is medium.
func (p *Pad) Add(e PadEntry) (int, error) {
	if e.Author == "" {
		e.Author = "main"
	}
	if e.Importance != Low && e.Importance != High {
		e.Importance = Medium
	}
	tx, err := p.s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var deleted bool
	if err := tx.QueryRow(`SELECT deleted FROM threads WHERE id = ?`, p.thread).Scan(&deleted); err != nil || deleted {
		return 0, ErrThreadGone
	}
	var n int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(n), 0) + 1 FROM scratchpad WHERE thread_id = ?`, p.thread).Scan(&n); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO scratchpad (thread_id, n, author, title, importance, description, body, report, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.thread, n, e.Author, e.Title, e.Importance, strings.Join(strings.Fields(e.Description), " "), e.Body, e.Report, now()); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE threads SET last_at = ? WHERE id = ?`, now(), p.thread); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// List returns every entry of the thread, in order.
func (p *Pad) List() ([]PadEntry, error) {
	rows, err := p.s.db.Query(`SELECT n, author, title, importance, description, body, report, created_at FROM scratchpad WHERE thread_id = ? ORDER BY n`, p.thread)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PadEntry
	for rows.Next() {
		var e PadEntry
		var at int64
		if err := rows.Scan(&e.N, &e.Author, &e.Title, &e.Importance, &e.Description, &e.Body, &e.Report, &at); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// LastReport is the newest final report of the thread, if it has one.
func (p *Pad) LastReport() (PadEntry, bool, error) {
	all, err := p.List()
	if err != nil {
		return PadEntry{}, false, err
	}
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Report {
			return all[i], true, nil
		}
	}
	return PadEntry{}, false, nil
}

// ReportTitle is the title of the scratchpad entry that keeps a run's final report: its date, in
// Eastern time, and the run.
func ReportTitle(run int64) string {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	return fmt.Sprintf("Report of %s, run %d", time.Now().In(loc).Format("2006-01-02"), run)
}

// NotesText is a report made of the high and medium notes in order, each with its title and body:
// what a run that took notes ends with when it wrote no report. The final reports of earlier runs
// and the low notes are not part of it.
func NotesText(entries []PadEntry) string {
	var parts []string
	for _, e := range entries {
		if e.Report || e.Importance == Low {
			continue
		}
		parts = append(parts, "## "+e.Title+"\n\n"+e.Body)
	}
	return strings.Join(parts, "\n\n")
}
