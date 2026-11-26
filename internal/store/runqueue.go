package store

import (
	"database/sql"
	"errors"
	"strconv"
	"time"
)

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

// QueuedRuns are a workspace's queued runs ready to start now, oldest first.
// Runs cooling down for a backoff retry (retry_after > now) are excluded until due.
func (s *Store) QueuedRuns(workspaceID int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM workflow_runs WHERE workspace_id = ? AND status = 'queued' AND retry_after <= ? ORDER BY id`, workspaceID, now())
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

// QueuedRunsFor are a workspace's queued runs ready to start now for either the Discord queue
// (discord=true) or the PR/Issue/LFA queue (discord=false), oldest first.
func (s *Store) QueuedRunsFor(workspaceID int64, discord bool) ([]int64, error) {
	q := `SELECT id FROM workflow_runs WHERE workspace_id = ? AND kind != 'discord' AND status = 'queued' AND retry_after <= ? ORDER BY id`
	if discord {
		q = `SELECT id FROM workflow_runs WHERE workspace_id = ? AND kind = 'discord' AND status = 'queued' AND retry_after <= ? ORDER BY id`
	}
	rows, err := s.db.Query(q, workspaceID, now())
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

// HasQueuedRunsFor reports whether workspaceID has ANY queued run (including runs cooling down
// on a transient retry backoff where retry_after > now) in the Discord or PR/Issue/LFA pool.
func (s *Store) HasQueuedRunsFor(workspaceID int64, discord bool) (bool, error) {
	q := `SELECT 1 FROM workflow_runs WHERE workspace_id = ? AND kind != 'discord' AND status = 'queued' LIMIT 1`
	if discord {
		q = `SELECT 1 FROM workflow_runs WHERE workspace_id = ? AND kind = 'discord' AND status = 'queued' LIMIT 1`
	}
	var one int
	err := s.db.QueryRow(q, workspaceID).Scan(&one)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, err
}

// WorkspacesWithQueue are the workspaces that have a queued run ready to start now.
func (s *Store) WorkspacesWithQueue() ([]int64, error) {
	rows, err := s.db.Query(`SELECT DISTINCT workspace_id FROM workflow_runs WHERE status = 'queued' AND retry_after <= ? ORDER BY workspace_id`, now())
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
	res, err := s.db.Exec(`UPDATE workflow_runs SET status = 'running', started_at = ?, retry_after = 0 WHERE id = ? AND status = 'queued'`, now(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RequeueForRetry returns a running run to the queue with a backoff after one or more steps hit a
// transient failure (such as a GitHub 5xx). Failed steps and any downstream skipped steps are reset
// to pending so finished work is kept when the run picks back up.
func (s *Store) RequeueForRetry(id int64, failedNotes map[string]string, resetSkipped []string, retryCount int, retryAfter time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for node, msg := range failedNotes {
		if _, err := tx.Exec(`UPDATE workflow_steps SET status = 'pending', port = '', output = '', error = ?, agent_run = 0, started_at = 0, finished_at = 0
			WHERE run_id = ? AND node_id = ?`, msg, id, node); err != nil {
			return err
		}
	}
	for _, node := range resetSkipped {
		if _, err := tx.Exec(`UPDATE workflow_steps SET status = 'pending', port = '', output = '', error = '', agent_run = 0, started_at = 0, finished_at = 0
			WHERE run_id = ? AND node_id = ? AND status = 'skipped'`, id, node); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE workflow_runs SET status = 'queued', retry_count = ?, retry_after = ? WHERE id = ? AND status = 'running'`,
		retryCount, retryAfter.Unix(), id); err != nil {
		return err
	}
	return tx.Commit()
}

// RequeueStartRetry returns a run whose startup (loading item from GitHub) hit a transient error
// back to the queue with a backoff instead of failing permanently.
func (s *Store) RequeueStartRetry(id int64, eventNode string, retryCount int, retryAfter time.Time, reason string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE workflow_steps SET error = ? WHERE run_id = ? AND node_id = ?`, reason, id, eventNode); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE workflow_runs SET status = 'queued', retry_count = ?, retry_after = ? WHERE id = ? AND status = 'running'`,
		retryCount, retryAfter.Unix(), id); err != nil {
		return err
	}
	return tx.Commit()
}

// ResumeFailedRun puts a failed or stopped non-dry run back into the queue, resetting only the
// named unfinished/failed/downstream-skipped steps to pending so completed work is preserved.
func (s *Store) ResumeFailedRun(id int64, resetNodes []string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE workflow_runs
		SET status = 'queued', finished_at = 0, stopped_by = '', stopped_at = 0, retry_count = 0, retry_after = 0
		WHERE id = ? AND status IN ('failed', 'stopped') AND dry_run = 0`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return false, nil
	}
	for _, node := range resetNodes {
		if _, err := tx.Exec(`UPDATE workflow_steps SET status = 'pending', port = '', output = '', error = '', agent_run = 0, started_at = 0, finished_at = 0
			WHERE run_id = ? AND node_id = ?`, id, node); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// FailStart records why a run that was claimed could not be started: its event step fails with the
// reason, and the run fails.
func (s *Store) FailStart(id int64, node, reason string) error {
	if _, err := s.db.Exec(`UPDATE workflow_steps SET status = 'failed', error = ?, finished_at = ? WHERE run_id = ? AND node_id = ?`, reason, now(), id, node); err != nil {
		return err
	}
	return s.FinishWorkflowRun(id, "failed")
}
