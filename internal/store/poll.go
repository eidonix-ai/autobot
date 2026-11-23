package store

import (
	"database/sql"
	"errors"
	"time"
)

// What the poller remembers between rounds, so that each thing that happens on GitHub starts a
// workflow at most once, and so that it survives a restart.
//
//   - A workflow is "live" when its owner has switched the poller on for it.
//   - poll_items is the last state seen of each pull request or issue.
//   - poll_events is what happened to them (opened, ready, a comment, a push...), in order.
//   - Each event node looks at an item's events from its own place (poll_cursors); a node that has
//     not looked yet starts from where it stood when it was baselined (poll_nodes), so nothing that
//     happened before it went live is acted on.
//   - poll_scan holds where each scan of GitHub reached, and poll_log what was decided, for the canvas.
const schemaPoller = `
ALTER TABLE workflows ADD COLUMN live INTEGER NOT NULL DEFAULT 0;
CREATE TABLE poll_items (
	workspace_id     INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	number           INTEGER NOT NULL,
	kind             TEXT    NOT NULL,
	open             INTEGER NOT NULL,
	author           TEXT    NOT NULL DEFAULT '',
	draft            INTEGER NOT NULL DEFAULT 0,
	head_sha         TEXT    NOT NULL DEFAULT '',
	edit_sig         TEXT    NOT NULL DEFAULT '',
	label_sig        TEXT    NOT NULL DEFAULT '',
	review_requested INTEGER NOT NULL DEFAULT 0,
	updated_at       INTEGER NOT NULL,
	ci_done          INTEGER NOT NULL DEFAULT 1,
	last_event       INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (workspace_id, number)
);
CREATE TABLE poll_events (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	number       INTEGER NOT NULL,
	kind         TEXT    NOT NULL,
	who          TEXT    NOT NULL DEFAULT '',
	text         TEXT    NOT NULL DEFAULT '',
	at           INTEGER NOT NULL
);
CREATE INDEX poll_events_item ON poll_events (workspace_id, number, id);
CREATE TABLE poll_nodes (
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	kind         TEXT    NOT NULL,
	node_id      TEXT    NOT NULL,
	since_id     INTEGER NOT NULL,
	PRIMARY KEY (workspace_id, kind, node_id)
);
CREATE TABLE poll_cursors (
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	number       INTEGER NOT NULL,
	node_id      TEXT    NOT NULL,
	last_id      INTEGER NOT NULL,
	PRIMARY KEY (workspace_id, number, node_id)
);
CREATE TABLE poll_scan (
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	key          TEXT    NOT NULL,
	value        TEXT    NOT NULL,
	PRIMARY KEY (workspace_id, key)
);
CREATE TABLE poll_log (
	id           INTEGER PRIMARY KEY,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	kind         TEXT    NOT NULL,
	number       INTEGER NOT NULL,
	node_id      TEXT    NOT NULL,
	text         TEXT    NOT NULL,
	run_id       INTEGER NOT NULL DEFAULT 0,
	at           INTEGER NOT NULL
);
CREATE INDEX poll_log_by_workspace ON poll_log (workspace_id, id);
`

// LiveWorkflow is a workflow the poller is switched on for.
type LiveWorkflow struct {
	WorkspaceID int64
	Kind        string
	Graph       string
}

// LiveWorkflows lists every workflow that is switched live.
func (s *Store) LiveWorkflows() ([]LiveWorkflow, error) {
	rows, err := s.db.Query(`SELECT workspace_id, kind, graph FROM workflows WHERE live = 1 ORDER BY workspace_id, kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LiveWorkflow
	for rows.Next() {
		var w LiveWorkflow
		if err := rows.Scan(&w.WorkspaceID, &w.Kind, &w.Graph); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SetWorkflowLive switches a saved workflow's poller on or off and records it in the workspace's
// history. ErrNotFound if the workflow was never saved.
func (s *Store) SetWorkflowLive(workspaceID int64, kind string, live bool, actor *User) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE workflows SET live = ? WHERE workspace_id = ? AND kind = ?`, live, workspaceID, kind)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	what := "off"
	if live {
		what = "live"
	}
	if err := addEvent(tx, workspaceID, actor, "Switched the "+kind+" workflow "+what); err != nil {
		return err
	}
	return tx.Commit()
}

// PollItem is the last state the poller saw of a pull request or issue.
type PollItem struct {
	Number          int
	Kind            string
	Open            bool
	Author          string
	Draft           bool
	HeadSHA         string
	EditSig         string // a digest of the title and description
	LabelSig        string
	ReviewRequested bool // a review is requested from Autobot's GitHub user
	Updated         time.Time
	CIDone          bool // the checks of HeadSHA have finished (or were not being watched)
	LastEvent       int64
}

const pollItemCols = `number, kind, open, author, draft, head_sha, edit_sig, label_sig, review_requested, updated_at, ci_done, last_event`

func scanPollItem(row scanner) (*PollItem, error) {
	var it PollItem
	var at int64
	err := row.Scan(&it.Number, &it.Kind, &it.Open, &it.Author, &it.Draft, &it.HeadSHA, &it.EditSig, &it.LabelSig,
		&it.ReviewRequested, &at, &it.CIDone, &it.LastEvent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	it.Updated = time.Unix(at, 0)
	return &it, err
}

// PollItem returns what the poller last saw of an item, or ErrNotFound.
func (s *Store) PollItem(workspaceID int64, number int) (*PollItem, error) {
	return scanPollItem(s.db.QueryRow(`SELECT `+pollItemCols+` FROM poll_items WHERE workspace_id = ? AND number = ?`, workspaceID, number))
}

// PollCIPending lists the open pull requests whose checks have not been seen to finish.
func (s *Store) PollCIPending(workspaceID int64, limit int) ([]PollItem, error) {
	rows, err := s.db.Query(`SELECT `+pollItemCols+` FROM poll_items WHERE workspace_id = ? AND kind = 'pr' AND open = 1 AND ci_done = 0
		ORDER BY updated_at DESC LIMIT ?`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PollItem
	for rows.Next() {
		it, err := scanPollItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

// SavePollItem stores an item's state, keeping the number of the last event recorded for it.
func (s *Store) SavePollItem(workspaceID int64, it PollItem) error {
	_, err := s.db.Exec(`INSERT INTO poll_items (workspace_id, `+pollItemCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (workspace_id, number) DO UPDATE SET kind = excluded.kind, open = excluded.open, author = excluded.author,
		draft = excluded.draft, head_sha = excluded.head_sha, edit_sig = excluded.edit_sig, label_sig = excluded.label_sig,
		review_requested = excluded.review_requested, updated_at = excluded.updated_at, ci_done = excluded.ci_done`,
		workspaceID, it.Number, it.Kind, it.Open, it.Author, it.Draft, it.HeadSHA, it.EditSig, it.LabelSig, it.ReviewRequested,
		it.Updated.Unix(), it.CIDone, it.LastEvent)
	return err
}

// PollEvent is one thing that happened to an item.
type PollEvent struct {
	ID   int64
	Kind string
	Who  string
	Text string
	At   time.Time
}

// AddPollEvent records that something happened to an item, which an item must already be stored for.
func (s *Store) AddPollEvent(workspaceID int64, number int, kind, who, text string, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO poll_events (workspace_id, number, kind, who, text, at) VALUES (?, ?, ?, ?, ?, ?)`,
		workspaceID, number, kind, who, text, at.Unix())
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	if _, err := tx.Exec(`UPDATE poll_items SET last_event = ? WHERE workspace_id = ? AND number = ?`, id, workspaceID, number); err != nil {
		return err
	}
	return tx.Commit()
}

// PollEvents lists an item's events after an event number, oldest first.
func (s *Store) PollEvents(workspaceID int64, number int, after int64) ([]PollEvent, error) {
	rows, err := s.db.Query(`SELECT id, kind, who, text, at FROM poll_events WHERE workspace_id = ? AND number = ? AND id > ? ORDER BY id`,
		workspaceID, number, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PollEvent
	for rows.Next() {
		var e PollEvent
		var at int64
		if err := rows.Scan(&e.ID, &e.Kind, &e.Who, &e.Text, &at); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PollCursor is the last event of an item that an event node has dealt with; the baseline's if
// it has not looked at the item yet. ok is false for a node that was never baselined.
func (s *Store) PollCursor(workspaceID int64, kind string, number int, node string) (last int64, ok bool, err error) {
	err = s.db.QueryRow(`SELECT COALESCE(c.last_id, n.since_id) FROM poll_nodes n
		LEFT JOIN poll_cursors c ON c.workspace_id = n.workspace_id AND c.node_id = n.node_id AND c.number = ?
		WHERE n.workspace_id = ? AND n.kind = ? AND n.node_id = ?`, number, workspaceID, kind, node).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return last, err == nil, err
}

// SetPollCursor records that an event node has dealt with an item's events up to one.
func (s *Store) SetPollCursor(workspaceID int64, number int, node string, last int64) error {
	_, err := s.db.Exec(`INSERT INTO poll_cursors (workspace_id, number, node_id, last_id) VALUES (?, ?, ?, ?)
		ON CONFLICT (workspace_id, number, node_id) DO UPDATE SET last_id = excluded.last_id`, workspaceID, number, node, last)
	return err
}

// PollPending lists the items of a kind with events an event node has not dealt with.
func (s *Store) PollPending(workspaceID int64, kind, node string) ([]int, error) {
	rows, err := s.db.Query(`SELECT i.number FROM poll_items i
		JOIN poll_nodes n ON n.workspace_id = i.workspace_id AND n.kind = i.kind AND n.node_id = ?
		LEFT JOIN poll_cursors c ON c.workspace_id = i.workspace_id AND c.number = i.number AND c.node_id = n.node_id
		WHERE i.workspace_id = ? AND i.kind = ? AND i.last_event > COALESCE(c.last_id, n.since_id) ORDER BY i.number`, node, workspaceID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// PollNodes lists the event nodes of a kind that have been baselined.
func (s *Store) PollNodes(workspaceID int64, kind string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT node_id FROM poll_nodes WHERE workspace_id = ? AND kind = ?`, workspaceID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// BaselineNode makes an event node start fresh: everything that has happened so far is behind it.
func (s *Store) BaselineNode(workspaceID int64, kind, node string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var max int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM poll_events`).Scan(&max); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM poll_cursors WHERE workspace_id = ? AND node_id = ?`, workspaceID, node); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO poll_nodes (workspace_id, kind, node_id, since_id) VALUES (?, ?, ?, ?)
		ON CONFLICT (workspace_id, kind, node_id) DO UPDATE SET since_id = excluded.since_id`, workspaceID, kind, node, max); err != nil {
		return err
	}
	return tx.Commit()
}

// PollScan returns a remembered position of a scan of GitHub, or "".
func (s *Store) PollScan(workspaceID int64, key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM poll_scan WHERE workspace_id = ? AND key = ?`, workspaceID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetPollScan remembers a position of a scan of GitHub.
func (s *Store) SetPollScan(workspaceID int64, key, value string) error {
	_, err := s.db.Exec(`INSERT INTO poll_scan (workspace_id, key, value) VALUES (?, ?, ?)
		ON CONFLICT (workspace_id, key) DO UPDATE SET value = excluded.value`, workspaceID, key, value)
	return err
}

// PollLogEntry is one decision of the poller.
type PollLogEntry struct {
	Kind   string
	Number int
	Node   string
	Text   string
	Run    int64 // the run it started; 0 if it started none
	At     time.Time
}

const pollLogKept = 100

// AddPollLog records a decision, and forgets the oldest beyond the last 100 of the workspace.
func (s *Store) AddPollLog(workspaceID int64, e PollLogEntry) error {
	if _, err := s.db.Exec(`INSERT INTO poll_log (workspace_id, kind, number, node_id, text, run_id, at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		workspaceID, e.Kind, e.Number, e.Node, e.Text, e.Run, e.At.Unix()); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM poll_log WHERE workspace_id = ? AND id <= (SELECT id FROM poll_log WHERE workspace_id = ?
		ORDER BY id DESC LIMIT 1 OFFSET ?)`, workspaceID, workspaceID, pollLogKept)
	return err
}

// PollLog returns the latest decisions for a kind of workflow, newest first.
func (s *Store) PollLog(workspaceID int64, kind string, limit int) ([]PollLogEntry, error) {
	rows, err := s.db.Query(`SELECT kind, number, node_id, text, run_id, at FROM poll_log WHERE workspace_id = ? AND kind = ?
		ORDER BY id DESC LIMIT ?`, workspaceID, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PollLogEntry
	for rows.Next() {
		var e PollLogEntry
		var at int64
		if err := rows.Scan(&e.Kind, &e.Number, &e.Node, &e.Text, &e.Run, &at); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneEvents forgets events older than a time; the poller calls it now and then.
func (s *Store) PruneEvents(before time.Time) error {
	_, err := s.db.Exec(`DELETE FROM poll_events WHERE at < ?`, before.Unix())
	return err
}

// WorkflowRunning reports whether a run from an event node on an item is still going.
func (s *Store) WorkflowRunning(workspaceID int64, kind, node string, item int) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM workflow_runs WHERE workspace_id = ? AND kind = ? AND event_node = ? AND item = ?
		AND status = 'running'`, workspaceID, kind, node, item).Scan(&n)
	return n > 0, err
}
