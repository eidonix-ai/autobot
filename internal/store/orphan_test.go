package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// A run cut off by a restart with notes of its own and no report gets the notes as its report, kept in
// its thread as the newest report; a run with no notes of its own, one that already has its report,
// and one with no thread, are left as they were.
func TestOrphanRunsGetANotesReport(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	old := now
	t.Cleanup(func() { now = old })
	clock := int64(1000)
	now = func() int64 { return clock }

	// Notes from before the run started, in a thread the run joins.
	_, _, th := startConv(t, st, ws, "ag", 1, false)
	st.StopOrphanRuns() // the run that made the thread is over
	st.Pad(th).Add(PadEntry{Title: "older note", Importance: High, Body: "from an earlier run"})
	clock = 2000
	started := func(thread int64, item int) int64 {
		run, _, _, err := st.StartAgentRun(AgentRun{WorkspaceID: ws.ID, Kind: "pr", NodeID: "ag", NodeName: "ag", Item: item, Model: "m", Thread: thread}, nil)
		if err != nil {
			t.Fatal(err)
		}
		st.SaveAgentRun(run, "running", `{"turns":4,"tokens_in":900}`, 0)
		return run
	}
	quiet := started(th, 1) // joins the thread, writes nothing
	clock = 2500
	// Another thread: a run that took notes and was cut off.
	_, _, th2 := startConv(t, st, ws, "ag", 2, false)
	st.StopOrphanRuns() // not the run we are testing: ends this setup run
	clock = 3000
	cut := started(th2, 2)
	clock = 3100
	st.Pad(th2).Add(PadEntry{Title: "Finding", Importance: High, Description: "d", Body: "the finding"})
	st.Pad(th2).Add(PadEntry{Title: "Minor", Importance: Low, Body: "skip"})
	st.Pad(th2).Add(PadEntry{Title: "Detail", Importance: Medium, Body: "a detail"})
	// A third: it took notes and had saved its report before the restart.
	_, _, th3 := startConv(t, st, ws, "ag", 3, false)
	st.StopOrphanRuns()
	clock = 3200
	done := started(th3, 3)
	st.Pad(th3).Add(PadEntry{Title: "Finding", Importance: High, Body: "x"})
	st.Pad(th3).Add(PadEntry{Title: ReportTitle(done), Importance: High, Body: "its own report", Report: true})
	clock = 3300

	if err := st.StopOrphanRuns(); err != nil {
		t.Fatal(err)
	}
	r, _ := st.AgentRun(cut)
	var sum struct {
		Report, Error, Outcome string
		Turns                  int
	}
	json.Unmarshal([]byte(r.Summary), &sum)
	want := "## Finding\n\nthe finding\n\n## Detail\n\na detail"
	if r.Status != "failed" || sum.Report != want || sum.Outcome != "failed" || !strings.Contains(sum.Error, "restarted") {
		t.Fatalf("the cut-off run: %s %s", r.Status, r.Summary)
	}
	rep, ok, _ := st.Pad(th2).LastReport()
	if !ok || rep.Body != want || rep.Title != ReportTitle(cut) || rep.Importance != High {
		t.Fatalf("its thread's report: %+v %v", rep, ok)
	}
	// What the run had saved stays: its summary keeps its turns.
	r, _ = st.AgentRun(quiet)
	if r.Status != "failed" || strings.Contains(r.Summary, "report") {
		t.Fatalf("the quiet run: %s %s", r.Status, r.Summary)
	}
	if _, ok, _ := st.Pad(th).LastReport(); ok {
		t.Fatal("a report was made from notes the run did not write")
	}
	if all, _ := st.Pad(th3).List(); len(all) != 2 {
		t.Fatalf("a run with its report got another: %+v", all)
	}
	// Done once: the runs are no longer running.
	if err := st.StopOrphanRuns(); err != nil {
		t.Fatal(err)
	}
	if all, _ := st.Pad(th2).List(); len(all) != 4 {
		t.Fatalf("%d entries", len(all))
	}
}
