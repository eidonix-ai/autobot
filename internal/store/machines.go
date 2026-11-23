package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Machine is a donated test machine in a workspace. Its agent signs in with a token.
type Machine struct {
	ID          int64
	WorkspaceID int64
	Name        string
	Revoked     bool
	Enabled     bool // false while taken offline: it gets no new runs
	CreatedAt   time.Time
}

var machineNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func hashToken64(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

const machineCols = `id, workspace_id, name, revoked, enabled, created_at`

func scanMachine(row scanner) (*Machine, error) {
	var m Machine
	var at int64
	if err := row.Scan(&m.ID, &m.WorkspaceID, &m.Name, &m.Revoked, &m.Enabled, &at); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	m.CreatedAt = time.Unix(at, 0)
	return &m, nil
}

// AddMachine adds a machine to a workspace and returns the token its agent signs in with. The
// token is shown only this once; the database keeps its hash.
func (s *Store) AddMachine(workspaceID int64, name string, actor *User) (*Machine, string, error) {
	name = strings.TrimSpace(name)
	if !machineNameRE.MatchString(name) {
		return nil, "", invalid("Name the machine with 1 to 64 letters, digits, dots, dashes or underscores, such as cn7.")
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := "abm_" + hex.EncodeToString(b)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	var taken bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM machines WHERE workspace_id = ? AND name = ? AND revoked = 0)`,
		workspaceID, name).Scan(&taken); err != nil {
		return nil, "", err
	}
	if taken {
		return nil, "", invalid("This workspace already has a machine named %s.", name)
	}
	res, err := tx.Exec(`INSERT INTO machines (workspace_id, name, token_hash, created_by, created_at) VALUES (?, ?, ?, ?, ?)`,
		workspaceID, name, hashToken64(token), actor.ID, now())
	if err != nil {
		return nil, "", err
	}
	id, _ := res.LastInsertId()
	if err := addEvent(tx, workspaceID, actor, "added the machine "+name); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	m, err := s.Machine(id)
	return m, token, err
}

// MachineByToken returns the enabled machine a token belongs to, or ErrNotFound.
func (s *Store) MachineByToken(token string) (*Machine, error) {
	if !strings.HasPrefix(token, "abm_") {
		return nil, ErrNotFound
	}
	return scanMachine(s.db.QueryRow(`SELECT `+machineCols+` FROM machines WHERE token_hash = ? AND revoked = 0`, hashToken64(token)))
}

// Machine looks a machine up by id.
func (s *Store) Machine(id int64) (*Machine, error) {
	return scanMachine(s.db.QueryRow(`SELECT `+machineCols+` FROM machines WHERE id = ?`, id))
}

// Machines lists a workspace's machines that are not revoked, by name.
func (s *Store) Machines(workspaceID int64) ([]Machine, error) {
	rows, err := s.db.Query(`SELECT `+machineCols+` FROM machines WHERE workspace_id = ? AND revoked = 0 ORDER BY name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Machine
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// SetMachineEnabled takes a machine offline (no new runs) or brings it back, and records it in the
// workspace's history. A run in progress is left to finish.
func (s *Store) SetMachineEnabled(m *Machine, enabled bool, actor *User) error {
	if m.Enabled == enabled {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE machines SET enabled = ? WHERE id = ?`, enabled, m.ID); err != nil {
		return err
	}
	what := "took " + m.Name + " offline"
	if enabled {
		what = "brought " + m.Name + " back online"
	}
	if err := addEvent(tx, m.WorkspaceID, actor, what); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeMachine stops a machine's token from working. Its runs stay in the history.
func (s *Store) RevokeMachine(m *Machine, actor *User) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE machines SET revoked = 1 WHERE id = ?`, m.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE test_runs SET state = 'error', summary = 'The machine was removed.', ended_at = ?
		WHERE machine_id = ? AND state IN ('queued', 'running')`, now(), m.ID); err != nil {
		return err
	}
	if err := addEvent(tx, m.WorkspaceID, actor, "removed the machine "+m.Name); err != nil {
		return err
	}
	return tx.Commit()
}

// TestRun is one test job on a machine.
type TestRun struct {
	ID          int64
	WorkspaceID int64
	MachineID   int64
	MachineName string
	StartedBy   string
	Script      string
	Ref         string
	Commit      string
	GPUs        int
	Private     bool // the code comes through Autobot, with the workspace's token
	Session     bool // an agent's shell: the container stays open and takes commands, and Script is empty
	Timeout     time.Duration
	State       string // queued, running, passed, failed, error
	Cancel      bool
	Output      string
	Summary     string
	CreatedAt   time.Time
	StartedAt   time.Time // zero until a machine takes it
	EndedAt     time.Time // zero until it ends
}

// Active reports whether the run has not ended.
func (r TestRun) Active() bool { return r.State == "queued" || r.State == "running" }

const runCols = `r.id, r.workspace_id, r.machine_id, m.name, ` + authorName + `, r.script, r.ref, r.commit_sha, r.gpus, r.private, r.session,
	r.timeout_sec, r.state, r.cancel, r.output, r.summary, r.created_at, r.started_at, r.ended_at`

const runFrom = ` FROM test_runs r JOIN machines m ON m.id = r.machine_id LEFT JOIN users u ON u.id = r.started_by`

func scanRun(row scanner) (*TestRun, error) {
	var r TestRun
	var timeout, created int64
	var started, ended sql.NullInt64
	if err := row.Scan(&r.ID, &r.WorkspaceID, &r.MachineID, &r.MachineName, &r.StartedBy, &r.Script, &r.Ref, &r.Commit, &r.GPUs, &r.Private, &r.Session,
		&timeout, &r.State, &r.Cancel, &r.Output, &r.Summary, &created, &started, &ended); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	r.Timeout, r.CreatedAt = time.Duration(timeout)*time.Second, time.Unix(created, 0)
	if started.Valid {
		r.StartedAt = time.Unix(started.Int64, 0)
	}
	if ended.Valid {
		r.EndedAt = time.Unix(ended.Int64, 0)
	}
	return &r, nil
}

// QueueRun records a test run for a machine to pick up. A machine runs one job at a time, so a
// second is refused while one is queued or running.
func (s *Store) QueueRun(m *Machine, script, ref, commit string, private bool, gpus int, timeout time.Duration, actor *User) (*TestRun, error) {
	return s.queue(m, script, ref, commit, private, false, gpus, timeout, actor)
}

// QueueSession records a session for a machine to pick up: a run with no script, whose container
// takes commands until it is cancelled or its time is up.
func (s *Store) QueueSession(m *Machine, ref, commit string, private bool, gpus int, timeout time.Duration, actor *User) (*TestRun, error) {
	return s.queue(m, "", ref, commit, private, true, gpus, timeout, actor)
}

func (s *Store) queue(m *Machine, script, ref, commit string, private, session bool, gpus int, timeout time.Duration, actor *User) (*TestRun, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var busy bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM test_runs WHERE machine_id = ? AND state IN ('queued', 'running'))`, m.ID).Scan(&busy); err != nil {
		return nil, err
	}
	if busy {
		return nil, invalid("%s is already running a test. Wait for it to finish, or stop it.", m.Name)
	}
	res, err := tx.Exec(`INSERT INTO test_runs (workspace_id, machine_id, started_by, script, ref, commit_sha, private, session, gpus, timeout_sec, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?)`, m.WorkspaceID, m.ID, actor.ID, strings.ReplaceAll(script, "\r\n", "\n"), ref, commit, private, session, gpus,
		int64(timeout/time.Second), now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	what := "started test run"
	if session {
		what = "opened a session, run"
	}
	if err := addEvent(tx, m.WorkspaceID, actor, fmt.Sprintf("%s %d on %s", what, id, m.Name)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Run(id)
}

// Run looks a test run up by id.
func (s *Store) Run(id int64) (*TestRun, error) {
	return scanRun(s.db.QueryRow(`SELECT `+runCols+runFrom+` WHERE r.id = ?`, id))
}

// Runs lists a machine's most recent runs, newest first, without their output.
func (s *Store) Runs(machineID int64, limit int) ([]TestRun, error) {
	rows, err := s.db.Query(`SELECT `+strings.Replace(runCols, "r.output", "''", 1)+runFrom+
		` WHERE r.machine_id = ? ORDER BY r.id DESC LIMIT ?`, machineID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TestRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// RunState is a run's state, whether its stop was asked for, and its summary, without its output.
func (s *Store) RunState(id int64) (state string, cancel bool, summary string, err error) {
	err = s.db.QueryRow(`SELECT state, cancel, summary FROM test_runs WHERE id = ?`, id).Scan(&state, &cancel, &summary)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

// ActiveRun returns the machine's queued or running run, or ErrNotFound.
func (s *Store) ActiveRun(machineID int64) (*TestRun, error) {
	return scanRun(s.db.QueryRow(`SELECT `+runCols+runFrom+` WHERE r.machine_id = ? AND r.state IN ('queued', 'running')
		ORDER BY r.id LIMIT 1`, machineID))
}

// StartRun marks a queued run as taken by its machine. It reports false if the run was no longer
// queued (cancelled meanwhile).
func (s *Store) StartRun(id int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE test_runs SET state = 'running', started_at = ? WHERE id = ? AND state = 'queued'`, now(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// AppendOutput adds text to a running run's output while it is under max bytes, and reports
// whether the run is still running and whether its output has reached max.
func (s *Store) AppendOutput(id int64, text string, max int) (running, full bool, err error) {
	var state string
	var size int
	err = s.db.QueryRow(`SELECT state, length(CAST(output AS BLOB)) FROM test_runs WHERE id = ?`, id).Scan(&state, &size)
	if err != nil || state != "running" {
		return false, false, err
	}
	if size >= max {
		return true, true, nil
	}
	text = strings.ToValidUTF8(text, "\uFFFD")
	if _, err := s.db.Exec(`UPDATE test_runs SET output = output || ? WHERE id = ?`, text, id); err != nil {
		return false, false, err
	}
	return true, size+len(text) >= max, nil
}

// RequestCancel asks for a run to stop. A queued run ends at once; a running one stops when its
// machine next checks in.
func (s *Store) RequestCancel(id int64) error {
	if _, err := s.db.Exec(`UPDATE test_runs SET state = 'error', summary = 'Stopped before it started.', ended_at = ?
		WHERE id = ? AND state = 'queued'`, now(), id); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE test_runs SET cancel = 1 WHERE id = ? AND state = 'running'`, id)
	return err
}

// FinishRun records how an active run ended. It does nothing to a run that has already ended.
func (s *Store) FinishRun(id int64, state, summary string) error {
	_, err := s.db.Exec(`UPDATE test_runs SET state = ?, summary = ?, ended_at = ? WHERE id = ? AND state IN ('queued', 'running')`,
		state, summary, now(), id)
	return err
}

// ActiveRuns lists every queued or running run, for the check on machines that went quiet.
func (s *Store) ActiveRuns() ([]TestRun, error) {
	rows, err := s.db.Query(`SELECT ` + strings.Replace(runCols, "r.output", "''", 1) + runFrom + ` WHERE r.state IN ('queued', 'running')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TestRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}
