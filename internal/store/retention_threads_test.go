package store

import (
	"errors"
	"os"
	"testing"
	"time"
)

// A thread is kept while a run of it goes on, however old its last note; starting a run on a thread
// counts as its being used; and nothing is written to a thread once it is deleted.
func TestRetentionKeepsAThreadWithARunningRun(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	at := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	old := at.Add(-90 * 24 * time.Hour).Unix()
	complete := func(int64) bool { return true }

	run, conv, th := startConv(t, st, ws, "ag", 0, false) // no item: kept for 30 days from its last use
	st.Pad(th).Add(PadEntry{Title: "a note", Importance: High, Body: "b"})
	st.AppendConv(conv, ConvEvent{T: "header", Model: "m"})
	age := func() {
		st.db.Exec(`UPDATE threads SET last_at = ? WHERE id = ?`, old, th)
		st.db.Exec(`UPDATE conversations SET last_turn_at = ? WHERE id = ?`, old, conv)
	}
	age()
	if p, err := st.PruneRuns(at, complete); err != nil || p.Conversations != 0 {
		t.Fatalf("a run in progress: %+v %v", p, err)
	}
	if notes, _ := st.Pad(th).List(); len(notes) != 1 {
		t.Fatal("the scratchpad of a thread with a running run was deleted")
	}
	if _, err := os.Stat(st.convFile(conv)); err != nil {
		t.Fatal("the conversation of a running run was deleted")
	}
	// Once it has ended, the same thread is old and goes.
	st.SaveAgentRun(run, "met", "{}", 0)
	age()
	if p, err := st.PruneRuns(at, complete); err != nil || p.Conversations != 1 {
		t.Fatalf("after it ended: %+v %v", p, err)
	}
	if notes, _ := st.Pad(th).List(); len(notes) != 0 {
		t.Fatal("the scratchpad was kept")
	}
	// Nothing is written into it now.
	if _, err := st.Pad(th).Add(PadEntry{Title: "late", Body: "x"}); !errors.Is(err, ErrThreadGone) {
		t.Fatalf("a note added to a deleted thread: %v", err)
	}
	if notes, _ := st.Pad(th).List(); len(notes) != 0 {
		t.Fatalf("%+v", notes)
	}
	if _, err := st.Pad(9999).Add(PadEntry{Title: "nowhere", Body: "x"}); !errors.Is(err, ErrThreadGone) {
		t.Fatalf("a note added to a thread that is not there: %v", err)
	}
}

// Starting a run on an old thread makes it new again: it is not deleted the moment the run ends.
func TestStartingARunBumpsTheThread(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	run, _, th := startConv(t, st, ws, "ag", 0, false)
	st.SaveAgentRun(run, "met", "{}", 0)
	st.db.Exec(`UPDATE threads SET last_at = 1000 WHERE id = ?`, th)
	if _, _, got, err := st.StartAgentRun(AgentRun{WorkspaceID: ws.ID, Kind: "pr", NodeID: "ag", NodeName: "ag", Model: "m", Thread: th}, nil); err != nil || got != th {
		t.Fatalf("%v %d", err, got)
	}
	var last int64
	st.db.QueryRow(`SELECT last_at FROM threads WHERE id = ?`, th).Scan(&last)
	if last < time.Now().Unix()-5 {
		t.Fatalf("last_at is %d", last)
	}
}
