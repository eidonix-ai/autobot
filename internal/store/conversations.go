package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// An agent conversation is one session of a run: its research, or the writing of its report. Each
// starts fresh (a continuation or re-run opens with the thread's last report and scratchpad: see
// threads.go), so a conversation is never continued; the ones from before threads are only shown.
// The row here says whose it is and when it last moved; the conversation itself is one compressed
// file, runs/conv-<id>.jsonl.gz, of JSON lines (ConvEvent):
//
//	header    the model, provider, session key, system prompt and tool definitions: sent unchanged
//	          on every turn
//	msg       one message of the conversation (never rewritten once sent)
//	trim      the tool results of the messages before upto were shortened, for good (old files only:
//	          nothing is trimmed automatically any more)
//	prune     the agent pruned the tool results with these numbers
//	compact   the conversation was compacted: from then on its messages are exactly these
//	turn      what one model call cost (input, output and cached input tokens), after its reply
//	run_end   a run ended (old files; the thread keeps what the run saw of the item now)
//
// A run is a row in agent_runs that records its conversations; its own summary stays small.
const schemaRunFiles = `
ALTER TABLE test_runs ADD COLUMN output_size INTEGER NOT NULL DEFAULT 0;
CREATE TABLE conversations (
	id           INTEGER PRIMARY KEY,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	kind         TEXT    NOT NULL,
	node_id      TEXT    NOT NULL,
	item         INTEGER NOT NULL DEFAULT 0,
	model        TEXT    NOT NULL DEFAULT '',
	standalone   INTEGER NOT NULL DEFAULT 0,
	deleted      INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL,
	last_turn_at INTEGER NOT NULL
);
CREATE INDEX conversations_by_node ON conversations (workspace_id, kind, node_id, item);
ALTER TABLE agent_runs ADD COLUMN conversation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_runs ADD COLUMN msgs_from    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_runs ADD COLUMN msgs_to      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_runs ADD COLUMN turns_from   INTEGER NOT NULL DEFAULT 0;
-- A conversation takes one run at a time.
CREATE UNIQUE INDEX agent_runs_one_running ON agent_runs (conversation) WHERE status = 'running' AND conversation != 0;
`

// ConvEvent is one line of a conversation's file. T says which kind it is.
type ConvEvent struct {
	T string `json:"t"`

	// header
	Model    string          `json:"model,omitempty"`
	Provider int64           `json:"provider,omitempty"`
	Session  string          `json:"session,omitempty"`
	System   string          `json:"system,omitempty"`
	Tools    json.RawMessage `json:"tools,omitempty"`

	M json.RawMessage `json:"m,omitempty"` // msg: the message

	Upto int `json:"upto,omitempty"` // trim: messages before this one

	Nums []int             `json:"nums,omitempty"` // prune: the result numbers pruned
	Msgs []json.RawMessage `json:"msgs,omitempty"` // compact: the messages the conversation now has

	// turn, and run_end
	Run    int64 `json:"run,omitempty"`
	In     int   `json:"in,omitempty"`
	Out    int   `json:"out,omitempty"`
	Cached int   `json:"cached,omitempty"`

	// run_end
	Head string   `json:"head,omitempty"` // the item's head commit the run saw
	Seen int64    `json:"seen,omitempty"` // when it last read the item, in unix seconds
	At   int64    `json:"at,omitempty"`
	Read []string `json:"read,omitempty"`
}

// Conversation is a conversation's row.
type Conversation struct {
	ID          int64
	WorkspaceID int64
	Kind        string
	NodeID      string // the Agent block that started it
	Item        int
	Model       string
	Standalone  bool // never continued: a test from the canvas, or one from before conversations continued
	Deleted     bool // its file was removed by retention
	CreatedAt   time.Time
	LastTurnAt  time.Time
}

// AppendConv adds events to a conversation's file, in one write, and notes that it moved.
func (s *Store) AppendConv(id int64, events ...ConvEvent) error {
	if len(events) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, e := range events {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := s.appendGz(s.convFile(id), buf.Bytes()); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE conversations SET last_turn_at = ? WHERE id = ?`, now(), id)
	return err
}

// ReadConv reads a conversation's file, in order. A last line cut off by a crash is left out.
func (s *Store) ReadConv(id int64) ([]ConvEvent, error) {
	data, err := readGz(s.convFile(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var out []ConvEvent
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e ConvEvent
		if err := json.Unmarshal(line, &e); err != nil {
			break // a cut-off last line: nothing after it can be trusted
		}
		out = append(out, e)
	}
	return out, nil
}

// MoveRunFiles moves what earlier versions kept in the database into files, once: each test run's
// output, and each agent run's messages (which become a conversation of their own, shown but
// never continued). Run it at start; it does nothing once everything has moved. The database
// rows are emptied only after their file is written.
func (s *Store) MoveRunFiles() error {
	rows, err := s.db.Query(`SELECT id, output FROM test_runs WHERE output_size = 0 AND output != ''`)
	if err != nil {
		return err
	}
	type out struct {
		id   int64
		text string
	}
	var outs []out
	for rows.Next() {
		var o out
		if err := rows.Scan(&o.id, &o.text); err != nil {
			rows.Close()
			return err
		}
		outs = append(outs, o)
	}
	rows.Close()
	for _, o := range outs {
		if err := s.writeGz(s.testLog(o.id), []byte(o.text)); err != nil {
			return err
		}
		if _, err := s.db.Exec(`UPDATE test_runs SET output = ?, output_size = ? WHERE id = ?`, tail(o.text, TailKept), len(o.text), o.id); err != nil {
			return err
		}
	}

	rows, err = s.db.Query(`SELECT DISTINCT run_id FROM agent_messages ORDER BY run_id`)
	if err != nil {
		return err
	}
	var runs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		runs = append(runs, id)
	}
	rows.Close()
	for _, id := range runs {
		if err := s.moveAgentRun(id); err != nil {
			return fmt.Errorf("moving agent run %d to a file: %w", id, err)
		}
	}
	return nil
}

func (s *Store) moveAgentRun(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ws, item, started, finished int64
	var kind, node, model string
	if err := tx.QueryRow(`SELECT workspace_id, kind, node_id, item, model, started_at, finished_at FROM agent_runs WHERE id = ?`, id).
		Scan(&ws, &kind, &node, &item, &model, &started, &finished); err != nil {
		if errors.Is(err, sql.ErrNoRows) { // messages of a run that is gone
			_, err = tx.Exec(`DELETE FROM agent_messages WHERE run_id = ?`, id)
			if err == nil {
				err = tx.Commit()
			}
			return err
		}
		return err
	}
	zs, err := tx.Query(`SELECT body FROM agent_messages WHERE run_id = ? ORDER BY seq`, id)
	if err != nil {
		return err
	}
	var msgs [][]byte
	for zs.Next() {
		var z []byte
		if err := zs.Scan(&z); err != nil {
			zs.Close()
			return err
		}
		m, err := gunzip(z)
		if err != nil { // one bad row is left out; it must not stop Autobot from starting
			log.Printf("agent run %d: a message could not be read and was left out: %v", id, err)
			continue
		}
		msgs = append(msgs, m)
	}
	zs.Close()
	last := finished
	if last == 0 {
		last = started
	}
	res, err := tx.Exec(`INSERT INTO conversations (workspace_id, kind, node_id, item, model, standalone, created_at, last_turn_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?)`, ws, kind, node, item, model, started, last)
	if err != nil {
		return err
	}
	conv, _ := res.LastInsertId()
	head := ConvEvent{T: "header", Model: model}
	var lines []ConvEvent
	for i, m := range msgs {
		var probe struct{ Role, Content string }
		json.Unmarshal(m, &probe)
		if i == 0 && probe.Role == "system" {
			head.System = probe.Content
			continue
		}
		lines = append(lines, ConvEvent{T: "msg", M: m})
	}
	var buf bytes.Buffer
	for _, e := range append([]ConvEvent{head}, lines...) {
		b, _ := json.Marshal(e)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := s.writeGz(s.convFile(conv), buf.Bytes()); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE agent_runs SET conversation = ?, msgs_from = 0, msgs_to = ? WHERE id = ?`, conv, len(lines), id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM agent_messages WHERE run_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// summaryKept are the parts of an agent run's summary that stay once its conversation is deleted.
var summaryKept = []string{"turns", "tokens_in", "tokens_out", "tokens_cached", "outcome", "summary", "report", "error"}

// maxKeptReport is how much of a report, and of its summary, stays in a run's row after its
// conversation is deleted.
const maxKeptReport = 2 << 10

// slimSummary cuts an agent run's summary to what is kept: outcome, turns, tokens, and the report
// cut to 2 KB.
func slimSummary(summary string) string {
	var all map[string]json.RawMessage
	if json.Unmarshal([]byte(summary), &all) != nil {
		return summary
	}
	kept := map[string]json.RawMessage{}
	for _, k := range summaryKept {
		if v, ok := all[k]; ok {
			kept[k] = v
		}
	}
	for _, k := range []string{"report", "summary"} {
		var text string
		if json.Unmarshal(kept[k], &text) == nil && len(text) > maxKeptReport {
			b, _ := json.Marshal(strings.ToValidUTF8(text[:maxKeptReport], "") + " […]")
			kept[k] = b
		}
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return summary
	}
	return string(b)
}

// RunsPruned counts what PruneRuns removed.
type RunsPruned struct {
	TestLogs, Conversations, StepOutputs, Orphans int
}

// Retention of run details, by age.
const (
	KeepTestLogs     = 30 * 24 * time.Hour // test-run output, and workflow-step output
	KeepConversation = 30 * 24 * time.Hour // after its pull request or issue closed, or its last turn
)

// PruneRuns deletes what has outlived its keeping, as of at; run it daily.
//   - Test-run output and workflow-step output older than 30 days: the rows keep status, port,
//     times and a one-line summary.
//   - A conversation is kept while its pull request or issue is open, and deleted 30 days after it
//     closed (by the local copy of items); when the item is not known there, 30 days after the
//     conversation's last turn. Its runs stay with outcome, turns, tokens, model, times and the
//     report cut to 2 KB.
//
// complete says whether a workspace's local copy of items is complete (its backfill done); until it
// is, a conversation whose item it does not have is kept, since the item may still be open.
func (s *Store) PruneRuns(at time.Time, complete func(ws int64) bool) (RunsPruned, error) {
	var p RunsPruned
	cutoff := at.Add(-KeepTestLogs).Unix()

	rows, err := s.db.Query(`SELECT id FROM test_runs WHERE ended_at IS NOT NULL AND ended_at < ? AND (output != '' OR output_size > 0)`, cutoff)
	if err != nil {
		return p, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return p, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := removeFile(s.testLog(id)); err != nil {
			return p, err
		}
		if _, err := s.db.Exec(`UPDATE test_runs SET output = '', output_size = -1 WHERE id = ?`, id); err != nil {
			return p, err
		}
		p.TestLogs++
	}

	rows, err = s.db.Query(`SELECT run_id, node_id, output FROM workflow_steps WHERE finished_at > 0 AND finished_at < ? AND `+stepOutputOld, cutoff)
	if err != nil {
		return p, err
	}
	type step struct {
		run  int64
		node string
		out  string
	}
	var steps []step
	for rows.Next() {
		var st step
		if err := rows.Scan(&st.run, &st.node, &st.out); err != nil {
			rows.Close()
			return p, err
		}
		steps = append(steps, st)
	}
	rows.Close()
	for _, st := range steps {
		if short := oneLine(st.out); short != st.out {
			if _, err := s.db.Exec(`UPDATE workflow_steps SET output = ? WHERE run_id = ? AND node_id = ?`, short, st.run, st.node); err != nil {
				return p, err
			}
			p.StepOutputs++
		}
	}

	// A conversation's end is when its item closed, if the local copy of items knows it; else its last
	// turn. A thread, and so its scratchpad, goes with it.
	for _, table := range []string{"conversations", "threads"} {
		gone, err := s.expired(table, cutoff, complete)
		if err != nil {
			return p, err
		}
		for _, id := range gone {
			if table == "threads" {
				err = s.dropThread(id)
			} else {
				err = s.dropConversation(id)
				p.Conversations++
			}
			if err != nil {
				return p, err
			}
		}
	}
	n, err := s.removeOrphans()
	p.Orphans = n
	return p, err
}

// removeOrphans deletes the files of runs and conversations whose rows are gone (their workspace or
// machine was deleted).
func (s *Store) removeOrphans() (int, error) {
	names, err := os.ReadDir(s.RunsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range names {
		var id int64
		var table string
		switch name := e.Name(); {
		case fmtScan(name, "conv-%d.jsonl.gz", &id):
			table = "conversations"
		case fmtScan(name, "test-%d.log.gz", &id):
			table = "test_runs"
		default:
			continue
		}
		var found int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE id = ?`, id).Scan(&found); err != nil {
			return n, err
		}
		if found == 0 {
			if err := removeFile(filepath.Join(s.RunsDir(), e.Name())); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// fmtScan reports whether name is exactly format with one number, which it sets.
func fmtScan(name, format string, id *int64) bool {
	_, err := fmt.Sscanf(name, format, id)
	return err == nil && fmt.Sprintf(format, *id) == name
}

// dropConversation removes a conversation's file and cuts its runs' summaries to what is kept.
func (s *Store) dropConversation(id int64) error {
	if err := removeFile(s.convFile(id)); err != nil {
		return err
	}
	rows, err := s.db.Query(`SELECT id, summary FROM agent_runs WHERE conversation = ? OR writer_conversation = ?`, id, id)
	if err != nil {
		return err
	}
	type run struct {
		id  int64
		sum string
	}
	var runs []run
	for rows.Next() {
		var r run
		if err := rows.Scan(&r.id, &r.sum); err != nil {
			rows.Close()
			return err
		}
		runs = append(runs, r)
	}
	rows.Close()
	for _, r := range runs {
		if _, err := s.db.Exec(`UPDATE agent_runs SET summary = ? WHERE id = ?`, slimSummary(r.sum), r.id); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(`UPDATE conversations SET deleted = 1 WHERE id = ?`, id)
	return err
}

// expired lists the conversations (or threads) whose keeping has run out: their item closed, or
// they last moved, before cutoff. One with a run in progress never has.
func (s *Store) expired(table string, cutoff int64, complete func(ws int64) bool) ([]int64, error) {
	last, running := "c.last_turn_at", "(r.conversation = c.id OR r.writer_conversation = c.id)"
	if table == "threads" {
		last, running = "c.last_at", "r.thread = c.id"
	}
	rows, err := s.db.Query(`SELECT c.id, c.workspace_id, c.item, ` + last + `, COALESCE(i.state, ''), COALESCE(MAX(i.closed_at, i.merged_at), 0)
		FROM ` + table + ` c LEFT JOIN items i ON i.workspace_id = c.workspace_id AND i.number = c.item AND c.item != 0
		WHERE c.deleted = 0 AND NOT EXISTS (SELECT 1 FROM agent_runs r WHERE r.status = 'running' AND ` + running + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var gone []int64
	for rows.Next() {
		var id, ws, item, last, closed int64
		var state string
		if err := rows.Scan(&id, &ws, &item, &last, &state, &closed); err != nil {
			return nil, err
		}
		end := last // a conversation continued after its item closed is kept from its last turn
		switch {
		case state == "open":
			continue
		case state == "" && item != 0 && !complete(ws):
			continue
		case state != "":
			end = max(last, closed)
		}
		if end < cutoff {
			gone = append(gone, id)
		}
	}
	return gone, rows.Err()
}
