package store

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestRun queues and starts a test run on a new machine.
func newTestRun(t *testing.T, st *Store, ws *Workspace, by *User) *TestRun {
	t.Helper()
	m, _, err := st.AddMachine(ws.ID, "cn1", by)
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.QueueRun(m, "echo hi", "", "", false, 0, time.Minute, by)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.StartRun(r.ID); !ok || err != nil {
		t.Fatalf("start %v %v", ok, err)
	}
	return r
}

// A test run's output goes to a compressed file, one gzip member per write, and reads back whole;
// the database row holds none of it while it runs and only the last 4 KB once it ends.
func TestTestRunOutputIsAFile(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	r := newTestRun(t, st, ws, admin)
	chunk := strings.Repeat("ok  github.com/o/r/pkg 0.123s\n", 2000)
	for i := 0; i < 3; i++ {
		if running, full, err := st.AppendOutput(r.ID, chunk, 8<<20); !running || full || err != nil {
			t.Fatalf("append %d: %v %v %v", i, running, full, err)
		}
	}
	file := st.testLog(r.ID)
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("github.com/o/r/pkg")) || len(raw) > 3*len(chunk)/10 {
		t.Fatalf("not compressed: %d bytes for %d", len(raw), 3*len(chunk))
	}
	if members := bytes.Count(raw, []byte{0x1f, 0x8b, 0x08}); members != 3 {
		t.Fatalf("%d gzip members, want one per write", members)
	}
	got, _ := st.Run(r.ID)
	if got.Output != "" || got.OutputSize != 3*len(chunk) {
		t.Fatalf("the row holds %d bytes of output (size %d) while running", len(got.Output), got.OutputSize)
	}
	whole, kept, err := st.RunOutput(got)
	if err != nil || !kept || whole != chunk+chunk+chunk {
		t.Fatalf("read back %d bytes, kept %v, %v", len(whole), kept, err)
	}
	if err := st.FinishRun(r.ID, "passed", "Passed."); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Run(r.ID)
	if len(got.Output) != TailKept || !strings.HasSuffix(chunk, got.Output[len(got.Output)-10:]) {
		t.Fatalf("the row keeps %d bytes", len(got.Output))
	}
	// The cap counts what was written, not what the file weighs.
	if _, full, _ := st.AppendOutput(r.ID, "x", 10); full {
		t.Fatal("a finished run took output")
	}
}

// A file whose last gzip member was cut off by a crash still reads, up to the cut.
func TestCutOffFileReads(t *testing.T) {
	st := open(t)
	path := filepath.Join(st.RunsDir(), "x.gz")
	st.appendGz(path, []byte("first\n"))
	st.appendGz(path, []byte("second line that is long enough to cut\n"))
	raw, _ := os.ReadFile(path)
	os.WriteFile(path, raw[:len(raw)-6], 0o640)
	got, err := readGz(path)
	if err != nil || !strings.HasPrefix(string(got), "first\n") {
		t.Fatalf("%q %v", got, err)
	}
	// And a conversation drops a last line that was cut in half.
	st.AppendConv(5, ConvEvent{T: "header", Model: "m"})
	z, _ := member([]byte(`{"t":"msg","m":{"role":"us`))
	f, _ := os.OpenFile(st.convFile(5), os.O_APPEND|os.O_WRONLY, 0)
	f.Write(z)
	f.Close()
	if evs, err := st.ReadConv(5); err != nil || len(evs) != 1 || evs[0].Model != "m" {
		t.Fatalf("%+v %v", evs, err)
	}
}

// startConv starts a run of a block on an item, in a new thread, with a new conversation.
func startConv(t *testing.T, st *Store, ws *Workspace, node string, item int, test bool) (run, conv, thread int64) {
	t.Helper()
	run, conv, thread, err := st.StartAgentRun(AgentRun{WorkspaceID: ws.ID, Kind: "pr", NodeID: node, NodeName: node, Item: item, Test: test, Model: "m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return
}

// What the retention job keeps and deletes.
func TestRetention(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	at := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	ago := func(days int) time.Time { return at.Add(-time.Duration(days) * 24 * time.Hour) }
	exists := func(path string) bool { _, err := os.Stat(path); return err == nil }

	// Items: 1 is open, 2 closed 31 days ago, 3 closed 29 days ago. 4 is not known to the local copy.
	for _, it := range []Item{
		{Number: 1, Kind: "pr", Title: "open", State: "open", Created: ago(90), Updated: ago(1)},
		{Number: 2, Kind: "pr", Title: "closed long ago", State: "closed", Created: ago(90), Updated: ago(31), Closed: ago(31)},
		{Number: 3, Kind: "pr", Title: "closed lately", State: "closed", Created: ago(90), Updated: ago(29), Closed: ago(29)},
	} {
		if err := st.SaveItem(ws.ID, it, nil); err != nil {
			t.Fatal(err)
		}
	}
	long := strings.Repeat("a long report. ", 400)
	threads := map[int64]int64{} // conversation -> its thread, which has a note in its scratchpad
	conv := func(item int, lastTurnDaysAgo int) (run, id int64) {
		run, id, th := startConv(t, st, ws, "ag", item, false)
		st.AppendConv(id, ConvEvent{T: "header", Model: "m"}, ConvEvent{T: "msg", M: json.RawMessage(`{"role":"user","content":"hi"}`)})
		sum := `{"plan":[{"step":"x","status":"done"}],"read":["a.go"],"turns":3,"tokens_in":900,"tokens_out":40,"outcome":"met","report":` + string(mustJSON(long)) + `}`
		st.SaveAgentRun(run, "met", sum, 1)
		st.db.Exec(`UPDATE conversations SET last_turn_at = ? WHERE id = ?`, ago(lastTurnDaysAgo).Unix(), id)
		if _, err := st.Pad(th).Add(PadEntry{Title: "a note", Importance: High, Description: "d", Body: "b"}); err != nil {
			t.Fatal(err)
		}
		st.db.Exec(`UPDATE threads SET last_at = ? WHERE id = ?`, ago(lastTurnDaysAgo).Unix(), th)
		threads[id] = th
		return
	}
	_, open := conv(1, 100) // open: kept however long ago its last turn was
	run2, closedLong := conv(2, 40)
	_, closedLately := conv(3, 40)
	_, unknownOld := conv(4, 31)
	_, unknownNew := conv(5, 29)
	_, noItem := conv(0, 45)

	p, err := st.PruneRuns(at, func(int64) bool { return true })
	if err != nil || p.Conversations != 3 {
		t.Fatalf("pruned %+v %v", p, err)
	}
	for id, want := range map[int64]bool{open: true, closedLong: false, closedLately: true, unknownOld: false, unknownNew: true, noItem: false} {
		if got := exists(st.convFile(id)); got != want {
			t.Errorf("conversation %d kept %v, want %v", id, got, want)
		}
		// A thread's scratchpad goes with its conversation, on the same rule.
		if notes, _ := st.Pad(threads[id]).List(); (len(notes) == 1) != want {
			t.Errorf("conversation %d: %d notes kept, want kept %v", id, len(notes), want)
		}
	}
	// The run of a deleted conversation stays, with outcome, turns, tokens, and the report cut to 2 KB.
	r, _ := st.AgentRun(run2)
	var sum map[string]any
	json.Unmarshal([]byte(r.Summary), &sum)
	report, _ := sum["report"].(string)
	if r.Status != "met" || sum["outcome"] != "met" || sum["turns"] != float64(3) || sum["tokens_in"] != float64(900) ||
		len(report) > maxKeptReport+10 || !strings.HasPrefix(report, "a long report.") || sum["plan"] != nil || sum["read"] != nil {
		t.Fatalf("%s", r.Summary)
	}
	// A deleted thread is not found again; an open item's is.
	if _, err := st.LatestThread(ws.ID, "pr", "ag", 2); err != ErrNotFound {
		t.Fatalf("a deleted thread was found: %v", err)
	}
	if th, err := st.LatestThread(ws.ID, "pr", "ag", 1); err != nil || th.ID != threads[open] {
		t.Fatalf("%v %v", th, err)
	}

	// Test-run output and workflow-step output older than 30 days go; the rows stay.
	old, fresh := newTestRun(t, st, ws, admin), (*TestRun)(nil)
	st.AppendOutput(old.ID, "old output\nsecond line\n", 1<<20)
	st.FinishRun(old.ID, "failed", "Failed with exit code 1.")
	st.db.Exec(`UPDATE test_runs SET ended_at = ? WHERE id = ?`, ago(31).Unix(), old.ID)
	m2, _, _ := st.AddMachine(ws.ID, "cn2", admin)
	fresh, _ = st.QueueRun(m2, "true", "", "", false, 0, time.Minute, admin)
	st.StartRun(fresh.ID)
	st.AppendOutput(fresh.ID, "fresh output\n", 1<<20)
	st.FinishRun(fresh.ID, "passed", "Passed.")
	st.db.Exec(`UPDATE test_runs SET ended_at = ? WHERE id = ?`, ago(29).Unix(), fresh.ID)
	wr, _ := st.StartWorkflowRun(WorkflowRun{WorkspaceID: ws.ID, Kind: "pr", EventNode: "ev", Item: 2, Graph: "{}"}, []string{"ev", "a", "b"}, admin)
	st.SetWorkflowStep(wr, WorkflowStep{NodeID: "a", Status: "done", Port: "out", Output: "Summary line\n" + long, StartedAt: ago(40), FinishedAt: ago(40)})
	st.SetWorkflowStep(wr, WorkflowStep{NodeID: "b", Status: "done", Port: "out", Output: "recent\nmultiline", StartedAt: ago(5), FinishedAt: ago(5)})

	p, err = st.PruneRuns(at, func(int64) bool { return true })
	if err != nil || p.TestLogs != 1 || p.StepOutputs != 1 {
		t.Fatalf("pruned %+v %v", p, err)
	}
	if exists(st.testLog(old.ID)) || !exists(st.testLog(fresh.ID)) {
		t.Fatal("the wrong test logs were deleted")
	}
	if got, _ := st.Run(old.ID); got.State != "failed" || got.Summary != "Failed with exit code 1." || got.Output != "" {
		t.Fatalf("%+v", got)
	}
	steps, _ := st.WorkflowSteps(wr)
	if a := steps["a"]; a.Status != "done" || a.Port != "out" || a.Output != "Summary line […]" {
		t.Fatalf("%+v", a)
	}
	if steps["b"].Output != "recent\nmultiline" {
		t.Fatalf("a recent step was shortened: %q", steps["b"].Output)
	}
	// Doing it again changes nothing.
	if p, err := st.PruneRuns(at, func(int64) bool { return true }); err != nil || p != (RunsPruned{}) {
		t.Fatalf("second pass %+v %v", p, err)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// What earlier versions kept in the database moves to files once: a test run's output, and each
// agent run's messages as a conversation of its own, which is shown but never continued.
func TestMoveRunFilesOnce(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	r := newTestRun(t, st, ws, admin)
	old := strings.Repeat("0123456789", 1000)
	st.db.Exec(`UPDATE test_runs SET output = ? WHERE id = ?`, old, r.ID)
	// An agent run as the old code wrote it: the run, and its messages gzipped one to a row.
	res, _ := st.db.Exec(`INSERT INTO agent_runs (workspace_id, kind, node_id, node_name, item, test, model, status, summary, started_at, finished_at)
		VALUES (?, 'pr', 'ag', 'Reviewer', 7, 0, 'm', 'met', '{"outcome":"met"}', 100, 200)`, ws.ID)
	run, _ := res.LastInsertId()
	for i, m := range []string{`{"role":"system","content":"You are Autobot."}`, `{"role":"user","content":"Review it."}`, `{"role":"assistant","content":"Done."}`} {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write([]byte(m))
		zw.Close()
		st.db.Exec(`INSERT INTO agent_messages (run_id, seq, body) VALUES (?, ?, ?)`, run, i, buf.Bytes())
	}

	if err := st.MoveRunFiles(); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Run(r.ID)
	if text, kept, _ := st.RunOutput(got); text != old || !kept || got.OutputSize != len(old) || len(got.Output) != TailKept {
		t.Fatalf("output: %d bytes, kept %v, row %d, size %d", len(text), kept, len(got.Output), got.OutputSize)
	}
	ar, _ := st.AgentRun(run)
	if ar.Conversation == 0 || ar.MsgsFrom != 0 || ar.MsgsTo != 2 {
		t.Fatalf("%+v", ar)
	}
	evs, err := st.ReadConv(ar.Conversation)
	if err != nil || len(evs) != 3 || evs[0].T != "header" || evs[0].System != "You are Autobot." || !strings.Contains(string(evs[2].M), "Done.") {
		t.Fatalf("%+v %v", evs, err)
	}
	var left int
	st.db.QueryRow(`SELECT COUNT(*) FROM agent_messages`).Scan(&left)
	if left != 0 {
		t.Fatalf("%d message rows left", left)
	}
	if _, err := st.LatestThread(ws.ID, "pr", "ag", 7); err != ErrNotFound {
		t.Fatalf("a moved conversation has a thread to continue: %v", err)
	}
	// A second start moves nothing and writes nothing.
	before, _ := os.Stat(st.convFile(ar.Conversation))
	if err := st.MoveRunFiles(); err != nil {
		t.Fatal(err)
	}
	var convs int
	st.db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&convs)
	after, _ := os.Stat(st.convFile(ar.Conversation))
	if convs != 1 || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("%d conversations", convs)
	}
}

// Only one run at a time in a thread; a test has a thread of its own, which is never continued.
func TestOneRunPerThread(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	run, _, th := startConv(t, st, ws, "ag", 7, false)
	if _, _, _, err := st.StartAgentRun(AgentRun{WorkspaceID: ws.ID, Kind: "pr", NodeID: "other", NodeName: "x", Item: 7, Model: "m", Thread: th}, nil); err != ErrThreadBusy {
		t.Fatalf("a second run in a busy thread: %v", err)
	}
	if busy, _ := st.ThreadRunning(th); !busy {
		t.Fatal("not running")
	}
	// Another item's thread is free.
	if _, _, _, err := st.StartAgentRun(AgentRun{WorkspaceID: ws.ID, Kind: "pr", NodeID: "ag", NodeName: "x", Item: 8, Model: "m"}, nil); err != nil {
		t.Fatalf("a run in another thread: %v", err)
	}
	st.SaveAgentRun(run, "met", "{}", 0)
	if busy, _ := st.ThreadRunning(th); busy {
		t.Fatal("still running")
	}
	// The next run joins the same thread, with a conversation of its own.
	run2, conv2, th2, err := st.StartAgentRun(AgentRun{WorkspaceID: ws.ID, Kind: "pr", NodeID: "other", NodeName: "x", Item: 7, Model: "m", Thread: th}, nil)
	if err != nil || th2 != th {
		t.Fatalf("after it ended: %v, thread %d", err, th2)
	}
	if r1, _ := st.AgentRun(run); r1.Conversation == conv2 || r1.Thread != th {
		t.Fatalf("the sessions share a conversation: %+v", r1)
	}
	_ = run2
	_, _, testThread := startConv(t, st, ws, "ag", 9, true)
	if got, err := st.LatestThread(ws.ID, "pr", "ag", 9); err == nil {
		t.Fatalf("a test's thread %d (%d) can be continued", got.ID, testThread)
	}
}

// A thread's scratchpad is its own: another thread, of the same block or another workspace, never sees
// it; numbers count per thread; entries are only added; the newest report is found.
func TestScratchpad(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	a, _ := st.CreateWorkspace("o/a", admin, "", "")
	b, _ := st.CreateWorkspace("o/b", admin, "", "")
	_, _, ta := startConv(t, st, a, "ag", 1, false)
	_, _, ta2 := startConv(t, st, a, "ag", 2, false)
	_, _, tb := startConv(t, st, b, "ag", 1, false)
	pa := st.Pad(ta)
	n1, _ := pa.Add(PadEntry{Title: "one", Importance: High, Description: "first\nline  here", Body: "body one"})
	n2, _ := pa.Add(PadEntry{Author: "helper", Title: "two", Importance: "weird", Body: "body two"})
	nb, _ := st.Pad(tb).Add(PadEntry{Title: "other workspace", Importance: Low, Body: "secret of o/b"})
	if n1 != 1 || n2 != 2 || nb != 1 {
		t.Fatalf("numbers %d %d %d", n1, n2, nb)
	}
	got, err := pa.List()
	if err != nil || len(got) != 2 || got[0].Author != "main" || got[0].Description != "first line here" || got[1].Author != "helper" || got[1].Importance != Medium {
		t.Fatalf("%+v %v", got, err)
	}
	for _, other := range []int64{ta2, tb} {
		if list, _ := st.Pad(other).List(); len(list) > 1 || (len(list) == 1 && list[0].Body != "secret of o/b") {
			t.Fatalf("thread %d sees %+v", other, list)
		}
	}
	if list, _ := st.Pad(ta2).List(); len(list) != 0 {
		t.Fatalf("another item's thread sees %+v", list)
	}
	if _, ok, _ := pa.LastReport(); ok {
		t.Fatal("a report with none saved")
	}
	pa.Add(PadEntry{Title: "Report of day 1", Importance: High, Body: "first report", Report: true})
	pa.Add(PadEntry{Title: "later note", Body: "x"})
	pa.Add(PadEntry{Title: "Report of day 2", Importance: High, Body: "second report", Report: true})
	if rep, ok, _ := pa.LastReport(); !ok || rep.Body != "second report" {
		t.Fatalf("%+v", rep)
	}
}

// SQLite gives a new row the id of a deleted one when that was the highest: a new conversation or
// test run must start empty, never on the deleted one's file (another workspace's, perhaps).
func TestReusedIDsStartEmpty(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	a, _ := st.CreateWorkspace("o/a", admin, "", "")
	b, _ := st.CreateWorkspace("o/b", admin, "", "")
	_, conv, _ := startConv(t, st, a, "ag", 1, false)
	st.AppendConv(conv, ConvEvent{T: "header", Model: "m", System: "secret of o/a"})
	r := newTestRun(t, st, a, admin)
	st.AppendOutput(r.ID, "output of o/a\n", 1<<20)
	if err := st.DeleteWorkspace(a.ID); err != nil {
		t.Fatal(err)
	}
	// The daily pass removes the files of rows that are gone.
	if p, err := st.PruneRuns(time.Now(), func(int64) bool { return true }); err != nil || p.Orphans != 2 {
		t.Fatalf("%+v %v", p, err)
	}
	// Even if they were left, a new row with a reused id starts empty.
	st.writeGz(st.convFile(conv), []byte(`{"t":"header","system":"secret of o/a"}`+"\n"))
	st.writeGz(st.testLog(r.ID), []byte("output of o/a\n"))
	_, conv2, _ := startConv(t, st, b, "ag", 1, false)
	r2 := newTestRun(t, st, b, admin)
	if conv2 != conv || r2.ID != r.ID {
		t.Skipf("ids were not reused (%d %d, %d %d): nothing to check", conv, conv2, r.ID, r2.ID)
	}
	st.AppendConv(conv2, ConvEvent{T: "header", Model: "m", System: "o/b"})
	evs, err := st.ReadConv(conv2)
	if err != nil || len(evs) != 1 || evs[0].System != "o/b" {
		t.Fatalf("the new conversation holds the deleted one's: %+v %v", evs, err)
	}
	st.AppendOutput(r2.ID, "output of o/b\n", 1<<20)
	got, _ := st.Run(r2.ID)
	if text, _, _ := st.RunOutput(got); text != "output of o/b\n" {
		t.Fatalf("the new test run's output holds the deleted one's: %q", text)
	}
}

// Until a workspace's local copy of items is complete, a conversation on an item it does not have is
// kept; and one continued after its item closed is kept from its last turn.
func TestRetentionWaitsForTheLocalCopy(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	at := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	ago := func(days int) time.Time { return at.Add(-time.Duration(days) * 24 * time.Hour) }
	st.SaveItem(ws.ID, Item{Number: 2, Kind: "pr", Title: "closed", State: "closed", Created: ago(90), Updated: ago(40), Closed: ago(40)}, nil)
	conv := func(item, lastTurnDaysAgo int) int64 {
		run, id, _ := startConv(t, st, ws, "ag", item, false)
		st.SaveAgentRun(run, "met", "{}", 0)
		st.AppendConv(id, ConvEvent{T: "header", Model: "m"})
		st.db.Exec(`UPDATE conversations SET last_turn_at = ? WHERE id = ?`, ago(lastTurnDaysAgo).Unix(), id)
		return id
	}
	unknown, continued := conv(9, 60), conv(2, 3)
	if p, err := st.PruneRuns(at, func(int64) bool { return false }); err != nil || p.Conversations != 0 {
		t.Fatalf("pruned before the local copy was complete: %+v %v", p, err)
	}
	if p, err := st.PruneRuns(at, func(int64) bool { return true }); err != nil || p.Conversations != 1 {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := os.Stat(st.convFile(continued)); err != nil {
		t.Fatal("a conversation continued 3 days ago was deleted because its item closed 40 days ago")
	}
	if _, err := os.Stat(st.convFile(unknown)); err == nil {
		t.Fatal("an old conversation on an unknown item was kept once the copy was complete")
	}
}

// A message row that cannot be read is left out of the move; it does not stop Autobot from starting.
func TestMoveSkipsABadMessage(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	res, _ := st.db.Exec(`INSERT INTO agent_runs (workspace_id, kind, node_id, node_name, item, test, model, status, summary, started_at, finished_at)
		VALUES (?, 'pr', 'ag', 'Reviewer', 7, 0, 'm', 'met', '{}', 100, 200)`, ws.ID)
	run, _ := res.LastInsertId()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(`{"role":"user","content":"Review it."}`))
	zw.Close()
	st.db.Exec(`INSERT INTO agent_messages (run_id, seq, body) VALUES (?, 0, ?), (?, 1, ?)`, run, buf.Bytes(), run, []byte("not gzip"))
	if err := st.MoveRunFiles(); err != nil {
		t.Fatal(err)
	}
	ar, _ := st.AgentRun(run)
	if evs, err := st.ReadConv(ar.Conversation); err != nil || len(evs) != 2 {
		t.Fatalf("%+v %v", evs, err)
	}
}
