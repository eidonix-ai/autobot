package store

import (
	"database/sql"
	"errors"
	"time"
)

// WorkflowRun is one run of a workflow on one item.
type WorkflowRun struct {
	ID          int64
	WorkspaceID int64
	Kind        string // pr or issue
	EventNode   string // the event node it started from
	Item        int    // the pull request or issue
	DryRun      bool   // writes nothing: no case file, no output tags, nothing posted
	Graph       string // the workflow as it was when the run started
	Status      string // running, done or failed
	Note        string // how the event's conditions stood when it started
	StartedBy   string
	StartedAt   time.Time
	FinishedAt  time.Time
}

// WorkflowStep is one node's part in a run.
type WorkflowStep struct {
	NodeID     string
	Status     string // pending, running, done, skipped or failed
	Port       string // the output it took (a decision's choice); "" until done
	Output     string
	Error      string
	AgentRun   int64
	StartedAt  time.Time
	FinishedAt time.Time
}

// StartWorkflowRun records a run with a pending step for each node and returns its id.
func (s *Store) StartWorkflowRun(r WorkflowRun, nodes []string, by *User) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var uid any
	if by != nil {
		uid = by.ID
	}
	res, err := tx.Exec(`INSERT INTO workflow_runs (workspace_id, kind, event_node, item, dry_run, graph, status, note, started_by, started_at)
		VALUES (?, ?, ?, ?, ?, ?, 'running', ?, ?, ?)`, r.WorkspaceID, r.Kind, r.EventNode, r.Item, r.DryRun, r.Graph, r.Note, uid, now())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	for _, n := range nodes {
		if _, err := tx.Exec(`INSERT INTO workflow_steps (run_id, node_id, status) VALUES (?, ?, 'pending')`, id, n); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// SetWorkflowStep stores a step's state.
func (s *Store) SetWorkflowStep(run int64, st WorkflowStep) error {
	unix := func(t time.Time) int64 {
		if t.IsZero() {
			return 0
		}
		return t.Unix()
	}
	_, err := s.db.Exec(`UPDATE workflow_steps SET status = ?, port = ?, output = ?, error = ?, agent_run = ?, started_at = ?, finished_at = ?
		WHERE run_id = ? AND node_id = ?`, st.Status, st.Port, st.Output, st.Error, st.AgentRun, unix(st.StartedAt), unix(st.FinishedAt), run, st.NodeID)
	return err
}

// FinishWorkflowRun ends a run.
func (s *Store) FinishWorkflowRun(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE workflow_runs SET status = ?, finished_at = ? WHERE id = ?`, status, now(), id)
	return err
}

// WorkflowRun returns one run, or ErrNotFound.
func (s *Store) WorkflowRun(id int64) (*WorkflowRun, error) {
	var r WorkflowRun
	var started, finished int64
	err := s.db.QueryRow(`SELECT w.id, w.workspace_id, w.kind, w.event_node, w.item, w.dry_run, w.graph, w.status, w.note, `+authorName+`,
		w.started_at, w.finished_at FROM workflow_runs w LEFT JOIN users u ON u.id = w.started_by WHERE w.id = ?`, id).
		Scan(&r.ID, &r.WorkspaceID, &r.Kind, &r.EventNode, &r.Item, &r.DryRun, &r.Graph, &r.Status, &r.Note, &r.StartedBy, &started, &finished)
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

// WorkflowSteps returns a run's steps by node id.
func (s *Store) WorkflowSteps(run int64) (map[string]WorkflowStep, error) {
	rows, err := s.db.Query(`SELECT node_id, status, port, output, error, agent_run, started_at, finished_at FROM workflow_steps WHERE run_id = ?`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]WorkflowStep{}
	for rows.Next() {
		var st WorkflowStep
		var started, finished int64
		if err := rows.Scan(&st.NodeID, &st.Status, &st.Port, &st.Output, &st.Error, &st.AgentRun, &started, &finished); err != nil {
			return nil, err
		}
		if started != 0 {
			st.StartedAt = time.Unix(started, 0)
		}
		if finished != 0 {
			st.FinishedAt = time.Unix(finished, 0)
		}
		out[st.NodeID] = st
	}
	return out, rows.Err()
}

// StopOrphanWorkflows ends the runs an earlier Autobot process left running: their steps were
// running in that process. Call it once at start.
func (s *Store) StopOrphanWorkflows() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE workflow_steps SET status = 'failed', error = 'Autobot restarted while it ran', finished_at = ?
		WHERE status = 'running' AND run_id IN (SELECT id FROM workflow_runs WHERE status = 'running')`, now()); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE workflow_runs SET status = 'failed', finished_at = ? WHERE status = 'running'`, now()); err != nil {
		return err
	}
	return tx.Commit()
}

// SetCaseValue stores an output tag's value for a case, replacing any earlier one.
func (s *Store) SetCaseValue(uuid, name, value, node string) error {
	_, err := s.db.Exec(`INSERT INTO case_values (case_uuid, name, value, node_id, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (case_uuid, name) DO UPDATE SET value = excluded.value, node_id = excluded.node_id, updated_at = excluded.updated_at`,
		uuid, name, value, node, now())
	return err
}

// CaseValues returns a case's output tags.
func (s *Store) CaseValues(uuid string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT name, value FROM case_values WHERE case_uuid = ?`, uuid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}
