package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A sub-agent's notes are its own and its run's: another sub-agent's, another run's of the same thread
// or another workspace's are never read by name; entries count from 1 for each; they go with the thread.
func TestSubAgentNotesAreScopedToTheirRun(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	a, _ := st.CreateWorkspace("o/a", admin, "", "")
	b, _ := st.CreateWorkspace("o/b", admin, "", "")
	run1, _, th := startConv(t, st, a, "ag", 1, false)
	st.SaveAgentRun(run1, "met", "{}", 0)
	run2, _, _, err := st.StartAgentRun(AgentRun{WorkspaceID: a.ID, Kind: "pr", NodeID: "ag", NodeName: "ag", Item: 1, Model: "m", Thread: th}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runB, _, _ := startConv(t, st, b, "ag", 1, false)
	mk := func(run int64, name, secret string) *SubAgent {
		sub, err := st.StartSub(run, name, "title "+name, "task")
		if err != nil {
			t.Fatal(err)
		}
		st.SubPad(sub.ID, name).Add(PadEntry{Title: "note", Importance: High, Description: "d", Body: secret})
		st.SubPad(sub.ID, name).Add(PadEntry{Title: "second", Importance: "weird", Body: "x"})
		return sub
	}
	s1 := mk(run1, "s1", "SECRET OF RUN 1")
	mk(run2, "s1", "SECRET OF RUN 2")
	mk(runB, "s1", "SECRET OF WORKSPACE B")
	mk(run2, "s2", "SECRET OF S2")

	read := func(run int64, name string) string {
		notes, err := st.SubNotes(run, name)
		if err != nil {
			return "ERR " + err.Error()
		}
		return notes[0].Body + "|" + notes[0].Author + "|" + notes[1].Importance
	}
	if got := read(run1, "s1"); got != "SECRET OF RUN 1|s1|medium" {
		t.Fatalf("%s", got)
	}
	if got := read(run2, "s1"); got != "SECRET OF RUN 2|s1|medium" {
		t.Fatalf("%s", got)
	}
	if got := read(run1, "s2"); got != "ERR "+ErrNoSub.Error() { // run 2's s2 is not run 1's
		t.Fatalf("%s", got)
	}
	if got := read(runB, "s1"); got != "SECRET OF WORKSPACE B|s1|medium" {
		t.Fatalf("%s", got)
	}
	if _, err := st.SubNotes(run1, "s9"); !errors.Is(err, ErrNoSub) {
		t.Fatalf("%v", err)
	}
	if subs, _ := st.Subs(run2); len(subs) != 2 || subs[0].Name != "s1" || subs[1].Name != "s2" || subs[0].Conversation == 0 || subs[0].Conversation == subs[1].Conversation {
		t.Fatalf("%+v", subs)
	}
	// The notes go with the thread; the sub-agents' summaries are cut to what is kept.
	st.SaveSub(s1.ID, "met", `{"outcome":"met","report":"`+strings.Repeat("x", 5000)+`","plan":[{"step":"s"}]}`)
	st.SaveAgentRun(run2, "met", "{}", 0)
	old := time.Now().Add(-90 * 24 * time.Hour).Unix()
	st.db.Exec(`UPDATE threads SET last_at = ? WHERE id = ?`, old, th)
	st.db.Exec(`UPDATE conversations SET last_turn_at = ?`, old)
	if p, err := st.PruneRuns(time.Now(), func(int64) bool { return true }); err != nil || p.Conversations == 0 {
		t.Fatalf("%+v %v", p, err)
	}
	notes, _ := st.SubNotes(run1, "s1")
	if len(notes) != 0 {
		t.Fatalf("the notes outlived their thread: %+v", notes)
	}
	if other, _ := st.SubNotes(runB, "s1"); len(other) != 2 {
		t.Fatal("another workspace's notes were deleted")
	}
	subs, _ := st.Subs(run1)
	if len(subs) != 1 || len(subs[0].Summary) > 3000 || strings.Contains(subs[0].Summary, "plan") {
		t.Fatalf("the summary was not cut: %d bytes", len(subs[0].Summary))
	}
}

// A sub-agent left running by a restart ends as failed.
func TestOrphanSubAgentsFail(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	run, _, _ := startConv(t, st, ws, "ag", 1, false)
	sub, _ := st.StartSub(run, "s1", "t", "task")
	if err := st.StopOrphanRuns(); err != nil {
		t.Fatal(err)
	}
	subs, _ := st.Subs(run)
	if subs[0].ID != sub.ID || subs[0].Status != "failed" || subs[0].FinishedAt.IsZero() {
		t.Fatalf("%+v", subs[0])
	}
}
