package store

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

// gunzip reads one gzipped value.
func gunzip(z []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(z))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}

// AgentRun is one run of an agent block. Summary is the harness's small JSON (plan, outcome, report,
// tokens); the conversation is in a file (see conversations.go), and the run records which of its
// messages and turns it added. Status is "running" until the run ends, then its outcome.
//
// A run belongs to a thread (see threads.go) and has a conversation of its own, its research
// session; a run with a closing step has a second, the session that wrote its report. Runs from
// before threads have no thread, only the conversation.
type AgentRun struct {
	ID                 int64
	WorkspaceID        int64
	Kind               string
	NodeID             string
	NodeName           string
	Item               int  // the pull request or issue; 0 for a test with none
	Test               bool // started from the canvas, not by a workflow
	Model              string
	Status             string
	Summary            string
	StartedBy          string
	StartedAt          time.Time
	FinishedAt         time.Time // zero while running
	Conversation       int64     // its research session's conversation
	WriterConversation int64     // the session that wrote its report; 0 when it has none
	Thread             int64     // the thread it belongs to; 0 in StartAgentRun starts a new one
	MsgsFrom           int       // the first message it added to its conversation (0 but for old runs)
	MsgsTo             int       // one past the last
	TurnsFrom          int       // the conversation's turns before it
}

// StartAgentRun records a run as running and returns its id, its new conversation's and its
// thread's. A run with no thread starts one (standalone, when it is a test from the canvas). A thread
// takes one run at a time: if another is running in it, the answer is ErrThreadBusy.
func (s *Store) StartAgentRun(r AgentRun, by *User) (run, conv, thread int64, err error) {
	var uid any
	if by != nil {
		uid = by.ID
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, 0, err
	}
	defer tx.Rollback()
	thread = r.Thread
	if thread != 0 { // a run on a thread keeps it from expiring as it starts
		if _, err := tx.Exec(`UPDATE threads SET last_at = ? WHERE id = ?`, now(), thread); err != nil {
			return 0, 0, 0, err
		}
	}
	if thread == 0 {
		res, err := tx.Exec(`INSERT INTO threads (workspace_id, kind, node_id, item, standalone, created_at, last_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.WorkspaceID, r.Kind, r.NodeID, r.Item, r.Test || r.Item == 0, now(), now())
		if err != nil {
			return 0, 0, 0, err
		}
		thread, _ = res.LastInsertId()
	}
	if conv, err = s.newConversation(tx, r); err != nil {
		return 0, 0, 0, err
	}
	res, err := tx.Exec(`INSERT INTO agent_runs (workspace_id, kind, node_id, node_name, item, test, model, status, started_by, started_at,
		conversation, thread, msgs_from, msgs_to, turns_from) VALUES (?, ?, ?, ?, ?, ?, ?, 'running', ?, ?, ?, ?, ?, ?, ?)`,
		r.WorkspaceID, r.Kind, r.NodeID, r.NodeName, r.Item, r.Test, r.Model, uid, now(), conv, thread, r.MsgsFrom, r.MsgsFrom, r.TurnsFrom)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return 0, 0, 0, ErrThreadBusy
		}
		return 0, 0, 0, err
	}
	run, _ = res.LastInsertId()
	return run, conv, thread, tx.Commit()
}

// newConversation adds the row of a conversation that is never continued: every session starts
// fresh, from its thread's scratchpad.
func (s *Store) newConversation(tx *sql.Tx, r AgentRun) (int64, error) {
	res, err := tx.Exec(`INSERT INTO conversations (workspace_id, kind, node_id, item, model, standalone, created_at, last_turn_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?)`, r.WorkspaceID, r.Kind, r.NodeID, r.Item, r.Model, now(), now())
	if err != nil {
		return 0, err
	}
	conv, _ := res.LastInsertId()
	// The id may be a deleted conversation's (SQLite reuses the highest one): never add to its file.
	if err := removeFile(s.convFile(conv)); err != nil {
		return 0, err
	}
	return conv, nil
}

// StartWriterSession adds the conversation in which a run's report is written, and returns its id.
func (s *Store) StartWriterSession(run int64) (int64, error) {
	r, err := s.AgentRun(run)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	conv, err := s.newConversation(tx, *r)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE agent_runs SET writer_conversation = ? WHERE id = ?`, conv, run); err != nil {
		return 0, err
	}
	return conv, tx.Commit()
}

// SaveAgentRun stores a run's summary and how far its messages reach (msgsTo), and, when status is
// not "running", ends the run with it.
func (s *Store) SaveAgentRun(id int64, status, summary string, msgsTo int) error {
	finished := int64(0)
	if status != "running" {
		finished = now()
	}
	_, err := s.db.Exec(`UPDATE agent_runs SET status = ?, summary = ?, finished_at = ?, msgs_to = MAX(msgs_to, ?) WHERE id = ?`,
		status, summary, finished, msgsTo, id)
	return err
}

// AgentRun returns one run, or ErrNotFound.
func (s *Store) AgentRun(id int64) (*AgentRun, error) {
	var r AgentRun
	var started, finished int64
	err := s.db.QueryRow(`SELECT a.id, a.workspace_id, a.kind, a.node_id, a.node_name, a.item, a.test, a.model, a.status, a.summary,
		`+authorName+`, a.started_at, a.finished_at, a.conversation, a.writer_conversation, a.thread, a.msgs_from, a.msgs_to, a.turns_from
		FROM agent_runs a LEFT JOIN users u ON u.id = a.started_by WHERE a.id = ?`, id).
		Scan(&r.ID, &r.WorkspaceID, &r.Kind, &r.NodeID, &r.NodeName, &r.Item, &r.Test, &r.Model, &r.Status, &r.Summary,
			&r.StartedBy, &started, &finished, &r.Conversation, &r.WriterConversation, &r.Thread, &r.MsgsFrom, &r.MsgsTo, &r.TurnsFrom)
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
// them is gone. A run that took notes in its thread since it started, and has no report of its own
// kept there, gets the notes as its report (the high and medium ones in order), which is also kept in
// the thread as its newest report, so that what continues the thread has something to open with. A
// session's commands are queued in memory, so a session left over from the last process is ended too
// (its connector is told to stop at its next check-in). Call it once at start, before any run begins.
func (s *Store) StopOrphanRuns() error {
	if err := s.reportOrphans(); err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE agent_runs SET status = 'failed', finished_at = ? WHERE status = 'running'`, now()); err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE sub_agents SET status = 'failed', finished_at = ? WHERE status = 'running'`, now()); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE test_runs SET state = 'error', summary = 'Autobot restarted: the session ended.', ended_at = ?
		WHERE session = 1 AND state IN ('queued', 'running')`, now())
	return err
}

// reportOrphans gives each running run with notes of its own, and no report, a notes report.
func (s *Store) reportOrphans() error {
	rows, err := s.db.Query(`SELECT id, thread, summary, started_at FROM agent_runs WHERE status = 'running' AND thread != 0`)
	if err != nil {
		return err
	}
	type orphan struct {
		id, thread, started int64
		summary             string
	}
	var orphans []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.id, &o.thread, &o.summary, &o.started); err != nil {
			rows.Close()
			return err
		}
		orphans = append(orphans, o)
	}
	rows.Close()
	for _, o := range orphans {
		entries, err := s.Pad(o.thread).List()
		if err != nil {
			return err
		}
		var mine []PadEntry // what the run wrote since it started
		hasReport := false
		for _, e := range entries {
			if e.At.Unix() >= o.started {
				mine = append(mine, e)
				hasReport = hasReport || e.Report
			}
		}
		report := NotesText(mine)
		if hasReport || report == "" {
			continue
		}
		var sum map[string]any
		if json.Unmarshal([]byte(o.summary), &sum) != nil || sum == nil {
			sum = map[string]any{}
		}
		summary := FallbackSummary(report)
		sum["report"], sum["summary"] = report, summary
		if sum["outcome"] == nil {
			sum["outcome"] = "failed"
		}
		if sum["error"] == nil || sum["error"] == "" {
			sum["error"] = "Autobot restarted while it ran."
		}
		b, _ := json.Marshal(sum)
		if _, err := s.db.Exec(`UPDATE agent_runs SET summary = ? WHERE id = ?`, string(b), o.id); err != nil {
			return err
		}
		if _, err := s.Pad(o.thread).Add(PadEntry{Title: ReportTitle(o.id), Importance: High, Description: firstLineOf(summary, 160), Body: report, Summary: summary, Report: true}); err != nil && !errors.Is(err, ErrThreadGone) {
			return err
		}
	}
	return nil
}

// firstLineOf is the first line of a text, at most n characters.
func firstLineOf(s string, n int) string {
	first, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(first); len(r) > n {
		first = string(r[:n]) + "…"
	}
	return first
}
