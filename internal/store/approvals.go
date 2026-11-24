package store

import (
	"database/sql"
	"errors"
	"time"
)

// Approval is a Human Approval step's request.
type Approval struct {
	ID        int64
	RunID     int64
	NodeID    string
	Request   string
	Status    string // pending, approved, denied, timed_out or stopped (its run was stopped)
	Channel   string // where it was posted on Discord, if it was
	Message   string
	DecidedBy string
	DecidedOn string // "Discord" or "Autobot"
	DecidedAt time.Time
	ExpiresAt time.Time
}

// OpenApproval records a step's request, or returns the one it already has (a resumed run).
func (s *Store) OpenApproval(run int64, node, request string, expires time.Time) (*Approval, error) {
	_, err := s.db.Exec(`INSERT INTO approvals (run_id, node_id, request, expires_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (run_id, node_id) DO NOTHING`, run, node, request, expires.Unix())
	if err != nil {
		return nil, err
	}
	return s.scanApproval(s.db.QueryRow(approvalSelect+` WHERE run_id = ? AND node_id = ?`, run, node))
}

const approvalSelect = `SELECT id, run_id, node_id, request, status, channel, message, decided_by, decided_on, decided_at, expires_at FROM approvals`

func (s *Store) scanApproval(row *sql.Row) (*Approval, error) {
	a, err := scanApprovalRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

func scanApprovalRow(row interface{ Scan(...any) error }) (*Approval, error) {
	var a Approval
	var decided, expires int64
	err := row.Scan(&a.ID, &a.RunID, &a.NodeID, &a.Request, &a.Status, &a.Channel, &a.Message, &a.DecidedBy, &a.DecidedOn, &decided, &expires)
	if err != nil {
		return nil, err
	}
	if decided != 0 {
		a.DecidedAt = time.Unix(decided, 0)
	}
	a.ExpiresAt = time.Unix(expires, 0)
	return &a, nil
}

// Approval returns one request, or ErrNotFound.
func (s *Store) Approval(id int64) (*Approval, error) {
	return s.scanApproval(s.db.QueryRow(approvalSelect+` WHERE id = ?`, id))
}

// RunApprovals returns a run's requests by node.
func (s *Store) RunApprovals(run int64) (map[string]*Approval, error) {
	rows, err := s.db.Query(`SELECT id FROM approvals WHERE run_id = ?`, run)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	out := map[string]*Approval{}
	for _, id := range ids {
		a, err := s.Approval(id)
		if err != nil {
			return nil, err
		}
		out[a.NodeID] = a
	}
	return out, nil
}

// SetApprovalMessage records where a request was posted on Discord.
func (s *Store) SetApprovalMessage(id int64, channel, message string) error {
	_, err := s.db.Exec(`UPDATE approvals SET channel = ?, message = ? WHERE id = ?`, channel, message, id)
	return err
}

// Decide answers a pending request and reports whether this answer was the one taken: the first
// answer wins, and a request past its time limit takes none.
func (s *Store) Decide(id int64, status, by, on string) (bool, error) {
	if status != "approved" && status != "denied" && status != "timed_out" {
		return false, errors.New("no such answer")
	}
	cond := ` AND expires_at > ?`
	if status == "timed_out" {
		cond = ` AND expires_at <= ?`
	}
	res, err := s.db.Exec(`UPDATE approvals SET status = ?, decided_by = ?, decided_on = ?, decided_at = ? WHERE id = ? AND status = 'pending'`+cond,
		status, by, on, now(), id, now())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// WaitingRuns are the runs left running with a step waiting for a person: they resume at start.
func (s *Store) WaitingRuns() ([]int64, error) {
	rows, err := s.db.Query(`SELECT DISTINCT r.id FROM workflow_runs r JOIN workflow_steps st ON st.run_id = r.id
		WHERE r.status = 'running' AND st.status = 'waiting'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		out = append(out, id)
	}
	return out, rows.Err()
}

// ApprovalFor returns a step's request, or ErrNotFound when it has not asked yet.
func (s *Store) ApprovalFor(run int64, node string) (*Approval, error) {
	return s.scanApproval(s.db.QueryRow(approvalSelect+` WHERE run_id = ? AND node_id = ?`, run, node))
}
