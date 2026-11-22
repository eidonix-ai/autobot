package store

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"errors"
	"io"
	"time"
)

// AgentRun is one run of an agent block. Summary is the harness's small JSON (plan, outcome, report,
// tokens); the conversation is read with AgentMessages. Status is "running" until the run ends,
// then its outcome.
type AgentRun struct {
	ID          int64
	WorkspaceID int64
	Kind        string
	NodeID      string
	NodeName    string
	Item        int  // the pull request or issue; 0 for a test with none
	Test        bool // started from the canvas, not by a workflow
	Model       string
	Status      string
	Summary     string
	StartedBy   string
	StartedAt   time.Time
	FinishedAt  time.Time // zero while running
}

// StartAgentRun records a run as running and returns its id.
func (s *Store) StartAgentRun(r AgentRun, by *User) (int64, error) {
	var uid any
	if by != nil {
		uid = by.ID
	}
	res, err := s.db.Exec(`INSERT INTO agent_runs (workspace_id, kind, node_id, node_name, item, test, model, status, started_by, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'running', ?, ?)`, r.WorkspaceID, r.Kind, r.NodeID, r.NodeName, r.Item, r.Test, r.Model, uid, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SaveAgentRun stores a run's summary and the messages from number first on (messages are only ever
// added, so each is written once), and, when status is not "running", ends the run with it.
func (s *Store) SaveAgentRun(id int64, status, summary string, first int, messages [][]byte) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, m := range messages {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write(m)
		if err := zw.Close(); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO agent_messages (run_id, seq, body) VALUES (?, ?, ?)`, id, first+i, buf.Bytes()); err != nil {
			return err
		}
	}
	finished := int64(0)
	if status != "running" {
		finished = now()
	}
	if _, err := tx.Exec(`UPDATE agent_runs SET status = ?, summary = ?, finished_at = ? WHERE id = ?`, status, summary, finished, id); err != nil {
		return err
	}
	return tx.Commit()
}

// AgentMessages returns a run's messages, in order.
func (s *Store) AgentMessages(id int64) ([][]byte, error) {
	rows, err := s.db.Query(`SELECT body FROM agent_messages WHERE run_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var z []byte
		if err := rows.Scan(&z); err != nil {
			return nil, err
		}
		zr, err := gzip.NewReader(bytes.NewReader(z))
		if err != nil {
			return nil, err
		}
		m, err := io.ReadAll(zr)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AgentRun returns one run, or ErrNotFound.
func (s *Store) AgentRun(id int64) (*AgentRun, error) {
	var r AgentRun
	var started, finished int64
	err := s.db.QueryRow(`SELECT a.id, a.workspace_id, a.kind, a.node_id, a.node_name, a.item, a.test, a.model, a.status, a.summary,
		`+authorName+`, a.started_at, a.finished_at FROM agent_runs a LEFT JOIN users u ON u.id = a.started_by WHERE a.id = ?`, id).
		Scan(&r.ID, &r.WorkspaceID, &r.Kind, &r.NodeID, &r.NodeName, &r.Item, &r.Test, &r.Model, &r.Status, &r.Summary,
			&r.StartedBy, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.StartedAt = time.Unix(started, 0)
	if finished != 0 {
		r.FinishedAt = time.Unix(finished, 0)
	}
	return &r, nil
}

// StopOrphanRuns marks runs left running by an earlier Autobot process as failed: whatever ran
// them is gone. Call it once at start, before any run begins.
func (s *Store) StopOrphanRuns() error {
	_, err := s.db.Exec(`UPDATE agent_runs SET status = 'failed', finished_at = ? WHERE status = 'running'`, now())
	return err
}
