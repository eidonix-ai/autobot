package store

import "strconv"

// How many workflow runs of a workspace may go at once (Derek, 2026-11-24: "max jobs"). A run over
// the limit is queued: recorded with status "queued", and started, oldest first, as slots free up.
const (
	DefaultMaxRuns = 3
	MinMaxRuns     = 1
	MaxMaxRuns     = 20
)

func maxRunsKey(workspaceID int64) string { return "max_runs/" + strconv.FormatInt(workspaceID, 10) }

// MaxRuns is how many workflow runs of a workspace may go at once.
func (s *Store) MaxRuns(workspaceID int64) int {
	v, err := s.Setting(maxRunsKey(workspaceID))
	if err != nil {
		return DefaultMaxRuns
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < MinMaxRuns || n > MaxMaxRuns {
		return DefaultMaxRuns
	}
	return n
}

// SetMaxRuns sets how many workflow runs of a workspace may go at once: 1 to 20.
func (s *Store) SetMaxRuns(ws *Workspace, n int, actor *User) error {
	if n < MinMaxRuns || n > MaxMaxRuns {
		return invalid("Runs at once is a whole number from %d to %d.", MinMaxRuns, MaxMaxRuns)
	}
	if n == s.MaxRuns(ws.ID) {
		return nil
	}
	if err := s.SetSetting(maxRunsKey(ws.ID), strconv.Itoa(n)); err != nil {
		return err
	}
	return addEvent(s.db, ws.ID, actor, "set the runs at once to "+strconv.Itoa(n))
}

// QueuedRuns are a workspace's queued runs in the order they will start: the oldest first.
func (s *Store) QueuedRuns(workspaceID int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM workflow_runs WHERE workspace_id = ? AND status = 'queued' ORDER BY id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// WorkspacesWithQueue are the workspaces that have a queued run.
func (s *Store) WorkspacesWithQueue() ([]int64, error) {
	rows, err := s.db.Query(`SELECT DISTINCT workspace_id FROM workflow_runs WHERE status = 'queued' ORDER BY workspace_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ClaimQueuedRun starts a queued run: it becomes running, as of now. It reports false for a run
// that is no longer queued (a person stopped it first).
func (s *Store) ClaimQueuedRun(id int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE workflow_runs SET status = 'running', started_at = ? WHERE id = ? AND status = 'queued'`, now(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// FailStart records why a run that was claimed could not be started: its event step fails with the
// reason, and the run fails.
func (s *Store) FailStart(id int64, node, reason string) error {
	if _, err := s.db.Exec(`UPDATE workflow_steps SET status = 'failed', error = ?, finished_at = ? WHERE run_id = ? AND node_id = ?`, reason, now(), id, node); err != nil {
		return err
	}
	return s.FinishWorkflowRun(id, "failed")
}
