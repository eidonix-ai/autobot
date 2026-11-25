package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// WorkflowRun is one run of a workflow on one item (or Discord mention).
type WorkflowRun struct {
	ID          int64
	WorkspaceID int64
	Kind        string // pr, issue, or discord
	EventNode   string // the event node it started from
	Item        int    // the pull request or issue (0 for discord)
	TriggerData string // JSON payload for non-GitHub triggers (e.g. Discord mention)
	DryRun      bool   // writes nothing: no case file, no output tags, nothing posted
	Graph       string // the workflow as it was when the run started
	Status      string // queued (over the workspace's limit, not started), running, done, failed or stopped
	Note        string // how the event's conditions stood when it started
	StartedBy   string // the name of the person who started it; "" when live automation did
	StartedByID int64  // that person's id; 0 when live automation started it
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

// StartWorkflowRun records a run with a pending step for each node and returns its id. The run
// starts as running unless r.Status says queued.
func (s *Store) StartWorkflowRun(r WorkflowRun, nodes []string, by *User) (int64, error) {
	if r.Status == "" {
		r.Status = "running"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var uid any
	if by != nil {
		uid = by.ID
	}
	res, err := tx.Exec(`INSERT INTO workflow_runs (workspace_id, kind, event_node, item, trigger_data, dry_run, graph, status, note, started_by, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.WorkspaceID, r.Kind, r.EventNode, r.Item, r.TriggerData, r.DryRun, r.Graph, r.Status, r.Note, uid, now())
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

// StopWorkflowRun ends a running or queued run as stopped by a person: the run, its steps that were running
// or waiting, their agent runs and the approvals still open all become stopped. It returns the
// approvals it closed, for the caller to say so where they were asked, and does nothing, and
// returns none, for a run that is neither (a second stop, or a run that just ended).
func (s *Store) StopWorkflowRun(id int64, by string) (bool, []Approval, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback()
	t := now()
	res, err := tx.Exec(`UPDATE workflow_runs SET status = 'stopped', stopped_by = ?, stopped_at = ?, finished_at = ? WHERE id = ? AND status IN ('running', 'queued')`, by, t, t, id)
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
	return scanWorkflowRun(s.db.QueryRow(workflowRunSelect+` WHERE w.id = ?`, id))
}

const workflowRunSelect = `SELECT w.id, w.workspace_id, w.kind, w.event_node, w.item, w.trigger_data, w.dry_run, w.graph, w.status, w.note, ` + authorName + `, COALESCE(w.started_by, 0),
	w.started_at, w.finished_at, w.stopped_by, w.stopped_at FROM workflow_runs w LEFT JOIN users u ON u.id = w.started_by`

func scanWorkflowRun(row scanner) (*WorkflowRun, error) {
	var r WorkflowRun
	var started, finished, stopped int64
	err := row.Scan(&r.ID, &r.WorkspaceID, &r.Kind, &r.EventNode, &r.Item, &r.TriggerData, &r.DryRun, &r.Graph, &r.Status, &r.Note, &r.StartedBy, &r.StartedByID, &started, &finished, &r.StoppedBy, &stopped)
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

func (s *Store) queryWorkflowRuns(where string, args ...any) ([]WorkflowRun, error) {
	rows, err := s.db.Query(workflowRunSelect+` WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkflowRun
	for rows.Next() {
		r, err := scanWorkflowRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// WorkflowRuns lists a workspace's workflow runs for the Runs page: every run still going, the
// running ones newest first and then the queued ones in the order they will start, then the most
// recent recent runs that have ended.
func (s *Store) WorkflowRuns(workspaceID int64, recent int) ([]WorkflowRun, error) {
	going, err := s.queryWorkflowRuns(`w.workspace_id = ? AND w.status IN ('running', 'queued')
		ORDER BY w.status = 'queued', CASE w.status WHEN 'queued' THEN w.id ELSE -w.id END`, workspaceID)
	if err != nil {
		return nil, err
	}
	ended, err := s.queryWorkflowRuns(`w.workspace_id = ? AND w.status NOT IN ('running', 'queued') ORDER BY w.id DESC LIMIT ?`, workspaceID, recent)
	return append(going, ended...), err
}

// WorkflowRunsOfItem lists the runs of a workspace on one pull request or issue, newest first.
func (s *Store) WorkflowRunsOfItem(workspaceID int64, item, limit int) ([]WorkflowRun, error) {
	return s.queryWorkflowRuns(`w.workspace_id = ? AND w.item = ? ORDER BY w.id DESC LIMIT ?`, workspaceID, item, limit)
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

// PostAlreadySent reports whether a Post block has already posted on this item in an earlier
// non-dry run. Only steps that actually posted to GitHub or Discord count; trial runs, dry runs,
// and skipped posts do not.
func (s *Store) PostAlreadySent(workspaceID int64, kind string, item int, nodeID string, excludeRunID int64) (bool, error) {
	if item == 0 {
		return false, nil
	}
	rows, err := s.db.Query(`SELECT ws.error FROM workflow_steps ws
		JOIN workflow_runs wr ON wr.id = ws.run_id
		WHERE wr.workspace_id = ? AND wr.kind = ? AND wr.item = ? AND wr.dry_run = 0
		  AND wr.id != ? AND ws.node_id = ? AND ws.status = 'done'`,
		workspaceID, kind, item, excludeRunID, nodeID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			return false, err
		}
		if strings.Contains(note, "GitHub: posted ") ||
			strings.Contains(note, "Discord: posted on Discord") ||
			strings.Contains(note, "Discord: summary updated on Discord") {
			return true, nil
		}
	}
	return false, rows.Err()
}

// StopOrphanWorkflows deals with the runs an earlier Autobot process left running: their steps were
// running in that process. Call it once at start, with restartable saying whether a node of a run's
// graph may simply run again (Derek, 2026-11-24: a deploy must not kill the runs in progress). Such a
// step of a run that is not a dry run goes back to pending, and its run goes back to the queue (to
// the head of it: the queue starts the oldest first), keeping every step that had finished; a run that
// also waits for a person stays running and is resumed with the others that wait. Any other step that
// was running fails, and a run with nothing to start again or wait for fails. A dry run's values were
// kept only in memory, so its steps are never started again.
func (s *Store) StopOrphanWorkflows(restartable func(graph, node string) bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	type orphan struct {
		id      int64
		graph   string
		dry     bool
		running []string
	}
	rows, err := tx.Query(`SELECT w.id, w.graph, w.dry_run, s.node_id FROM workflow_runs w JOIN workflow_steps s ON s.run_id = w.id
		WHERE w.status = 'running' AND s.status = 'running' ORDER BY w.id`)
	if err != nil {
		return err
	}
	var orphans []*orphan
	for rows.Next() {
		var o orphan
		var node string
		if err := rows.Scan(&o.id, &o.graph, &o.dry, &node); err != nil {
			rows.Close()
			return err
		}
		if n := len(orphans); n > 0 && orphans[n-1].id == o.id {
			orphans[n-1].running = append(orphans[n-1].running, node)
			continue
		}
		o.running = []string{node}
		orphans = append(orphans, &o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range orphans {
		again := false
		for _, node := range o.running {
			if !o.dry && restartable != nil && restartable(o.graph, node) {
				again = true
				if _, err := tx.Exec(`UPDATE workflow_steps SET status = 'pending', port = '', output = '', error = '', agent_run = 0, started_at = 0, finished_at = 0
					WHERE run_id = ? AND node_id = ?`, o.id, node); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(`UPDATE workflow_steps SET status = 'failed', error = 'Autobot restarted while it ran', finished_at = ?
				WHERE run_id = ? AND node_id = ?`, now(), o.id, node); err != nil {
				return err
			}
		}
		if again {
			if _, err := tx.Exec(`UPDATE workflow_runs SET status = 'queued' WHERE id = ?
				AND id NOT IN (SELECT run_id FROM workflow_steps WHERE status = 'waiting')`, o.id); err != nil {
				return err
			}
		}
	}
	// A run waiting for a person is kept, to be resumed; only runs with nothing left to start or wait for end.
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
