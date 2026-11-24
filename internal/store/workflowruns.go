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
	Status      string // running, done, failed or stopped
	Note        string // how the event's conditions stood when it started
	StartedBy   string
	StartedAt   time.Time
	FinishedAt  time.Time
	StoppedBy   string // the name of the person who stopped it
	StoppedAt   time.Time
}

// WorkflowStep is one node's part in a run.
type WorkflowStep struct {
	NodeID     string
	Status     string // pending, running, waiting (for a person), done, skipped, failed or stopped
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

// FinishWorkflowRun ends a run that is still running: a run a person stopped stays stopped.
func (s *Store) FinishWorkflowRun(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE workflow_runs SET status = ?, finished_at = ? WHERE id = ? AND status = 'running'`, status, now(), id)
	return err
}

// StopWorkflowRun ends a running run as stopped by a person: the run, its steps that were running
// or waiting, their agent runs and the approvals still open all become stopped. It returns the
// approvals it closed, for the caller to say so where they were asked, and does nothing, and
// returns none, for a run that is not running (a second stop, or a run that just ended).
func (s *Store) StopWorkflowRun(id int64, by string) (bool, []Approval, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback()
	t := now()
	res, err := tx.Exec(`UPDATE workflow_runs SET status = 'stopped', stopped_by = ?, stopped_at = ?, finished_at = ? WHERE id = ? AND status = 'running'`, by, t, t, id)
	if err != nil {
		return false, nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, nil, nil
	}
	if _, err := tx.Exec(`UPDATE workflow_steps SET status = 'stopped', finished_at = ? WHERE run_id = ? AND status IN ('running', 'waiting')`, t, id); err != nil {
		return false, nil, err
	}
	// The agent runs of its steps that are not over, or that ended as stopped without learning who stopped them.
	mine := `id IN (SELECT agent_run FROM workflow_steps WHERE run_id = ? AND agent_run != 0) AND status IN ('running', 'stopped') AND stopped_at = 0`
	if _, err := tx.Exec(`UPDATE sub_agents SET status = 'stopped', finished_at = ? WHERE status = 'running' AND run_id IN (SELECT id FROM agent_runs WHERE `+mine+`)`, t, id); err != nil {
		return false, nil, err
	}
	if _, err := tx.Exec(`UPDATE agent_runs SET status = 'stopped', stopped_by = ?, stopped_at = ?, finished_at = CASE WHEN finished_at = 0 THEN ? ELSE finished_at END WHERE `+mine, by, t, t, id); err != nil {
		return false, nil, err
	}
	rows, err := tx.Query(approvalSelect+` WHERE run_id = ? AND status = 'pending'`, id)
	if err != nil {
		return false, nil, err
	}
	var closed []Approval
	for rows.Next() {
		a, err := scanApprovalRow(rows)
		if err != nil {
			rows.Close()
			return false, nil, err
		}
		a.Status, a.DecidedBy, a.DecidedOn, a.DecidedAt = "stopped", by, "Autobot", time.Unix(t, 0)
		closed = append(closed, *a)
	}
	rows.Close()
	if _, err := tx.Exec(`UPDATE approvals SET status = 'stopped', decided_by = ?, decided_on = 'Autobot', decided_at = ? WHERE run_id = ? AND status = 'pending'`, by, t, id); err != nil {
		return false, nil, err
	}
	return true, closed, tx.Commit()
}

// SetStepAgentRun records the agent run a running step has started, so that its page can be found
// from the run and the run stopped from it.
func (s *Store) SetStepAgentRun(run int64, node string, agentRun int64) error {
	_, err := s.db.Exec(`UPDATE workflow_steps SET agent_run = ? WHERE run_id = ? AND node_id = ? AND status = 'running'`, agentRun, run, node)
	return err
}

// WorkflowOfAgentRun is the workflow run an agent run is a step of, if it is one.
func (s *Store) WorkflowOfAgentRun(agentRun int64) (int64, bool) {
	var id int64
	err := s.db.QueryRow(`SELECT run_id FROM workflow_steps WHERE agent_run = ? ORDER BY run_id DESC LIMIT 1`, agentRun).Scan(&id)
	return id, err == nil
}

// WorkflowRun returns one run, or ErrNotFound.
func (s *Store) WorkflowRun(id int64) (*WorkflowRun, error) {
	var r WorkflowRun
	var started, finished, stopped int64
	err := s.db.QueryRow(`SELECT w.id, w.workspace_id, w.kind, w.event_node, w.item, w.dry_run, w.graph, w.status, w.note, `+authorName+`,
		w.started_at, w.finished_at, w.stopped_by, w.stopped_at FROM workflow_runs w LEFT JOIN users u ON u.id = w.started_by WHERE w.id = ?`, id).
		Scan(&r.ID, &r.WorkspaceID, &r.Kind, &r.EventNode, &r.Item, &r.DryRun, &r.Graph, &r.Status, &r.Note, &r.StartedBy, &started, &finished, &r.StoppedBy, &stopped)
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
	if stopped != 0 {
		r.StoppedAt = time.Unix(stopped, 0)
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
	// A run waiting for a person is kept, to be resumed; only runs with nothing left to wait for end.
	if _, err := tx.Exec(`UPDATE workflow_runs SET status = 'failed', finished_at = ? WHERE status = 'running'
		AND id NOT IN (SELECT run_id FROM workflow_steps WHERE status = 'waiting')`, now()); err != nil {
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

// renameCaseKeys moves the values kept under the tag old, and its parts (old.report …), of this
// workspace's cases of a kind to the tag new. A value already under a new name is replaced, or with keep
// stays (and the old one is dropped).
func renameCaseKeys(tx *sql.Tx, workspaceID int64, kind, old, new string, keep bool) error {
	if old == new || old == "" || new == "" {
		return nil
	}
	cases := `case_uuid IN (SELECT uuid FROM cases WHERE workspace_id = ? AND kind = ?)`
	// Names contain _, which LIKE reads as a wildcard: compare the start of the name instead.
	for _, part := range []struct{ args string }{{"exact"}, {"prefix"}} {
		var rows *sql.Rows
		var err error
		if part.args == "exact" {
			rows, err = tx.Query(`SELECT case_uuid, name FROM case_values WHERE `+cases+` AND name = ?`, workspaceID, kind, old)
		} else {
			rows, err = tx.Query(`SELECT case_uuid, name FROM case_values WHERE `+cases+` AND substr(name, 1, ?) = ?`, workspaceID, kind, len(old)+1, old+".")
		}
		if err != nil {
			return err
		}
		type key struct{ uuid, name string }
		var keys []key
		for rows.Next() {
			var k key
			if err := rows.Scan(&k.uuid, &k.name); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, k)
		}
		rows.Close()
		for _, k := range keys {
			to := new + k.name[len(old):]
			if keep {
				var have int
				if err := tx.QueryRow(`SELECT COUNT(*) FROM case_values WHERE case_uuid = ? AND name = ?`, k.uuid, to).Scan(&have); err != nil {
					return err
				}
				if have > 0 {
					if _, err := tx.Exec(`DELETE FROM case_values WHERE case_uuid = ? AND name = ?`, k.uuid, k.name); err != nil {
						return err
					}
					continue
				}
			}
			if _, err := tx.Exec(`DELETE FROM case_values WHERE case_uuid = ? AND name = ?`, k.uuid, to); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE case_values SET name = ? WHERE case_uuid = ? AND name = ?`, to, k.uuid, k.name); err != nil {
				return err
			}
		}
	}
	return nil
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
