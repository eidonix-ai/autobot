package web

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// The Runs page lists the runs in progress first, then the most recent 100, each with its item (linked
// to GitHub), block, status, dry-run badge, who started it and a link; canvas tests are listed apart;
// each real case's page lists the runs on that item; the page updates itself only while a run is going.
func TestRunsPage(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	e.user("mallory", false)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	approvalWorkflow(t, e, ws, derek)
	w, _ := e.st.Workflow(ws.ID, workflow.PR)
	e.st.OpenCase(ws, 7, "pr", "Retry uploads", "Opened")
	start := func(item int, dry bool) int64 {
		id, err := e.st.StartWorkflowRun(store.WorkflowRun{WorkspaceID: ws.ID, Kind: workflow.PR, EventNode: "ev", Item: item, DryRun: dry, Graph: w.Graph}, []string{"ev"}, derek)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := start(7, false)
	e.st.FinishWorkflowRun(first, "done")
	second := start(7, true)
	e.st.FinishWorkflowRun(second, "failed")
	stopped := start(8, false)
	e.st.StopWorkflowRun(stopped, "alice")
	going := start(9, false)
	// A canvas test, and an agent run that belongs to a workflow run.
	test, _, _, _ := e.st.StartAgentRun(store.AgentRun{WorkspaceID: ws.ID, Kind: workflow.PR, NodeID: "ag", NodeName: "Reviewer", Item: 7, Test: true, Model: "m1"}, derek)
	step, _, _, _ := e.st.StartAgentRun(store.AgentRun{WorkspaceID: ws.ID, Kind: workflow.PR, NodeID: "ag", NodeName: "Hidden step", Item: 7, Model: "m1"}, derek)

	b := e.signIn("derek")
	code, body := b.get("/w/o/r/workflow-runs")
	if code != 200 {
		t.Fatalf("%d", code)
	}
	at := func(s string) int { return strings.Index(body, s) }
	runLink := func(id int64) string { return `href="/w/o/r/workflow-runs/` + itoa(id) + `"` }
	// In progress first, then the newest of the rest.
	if !(at(runLink(going)) > 0 && at(runLink(going)) < at(runLink(stopped)) && at(runLink(stopped)) < at(runLink(second)) && at(runLink(second)) < at(runLink(first))) {
		t.Errorf("order: going %d, stopped %d, second %d, first %d\n%s", at(runLink(going)), at(runLink(stopped)), at(runLink(second)), at(runLink(first)), body)
	}
	for _, want := range []string{`href="https://github.com/o/r/pull/7">#7</a> <span class="muted">Retry uploads</span>`, `href="https://github.com/o/r/pull/9"`,
		"PR First Ready", "running", "done", "failed", "stopped", ">dry run<", "derek"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q:\n%s", want, body)
		}
	}
	if strings.Count(body, ">dry run<") != 1 {
		t.Errorf("the dry-run badge shows %d times, want one", strings.Count(body, ">dry run<"))
	}
	if !strings.Contains(body, `/static/live.js`) || !strings.Contains(body, `id="runs-list" data-live`) {
		t.Error("a page with a run going does not update itself")
	}
	// Canvas tests: apart, linked, and only those.
	if !strings.Contains(body, `href="/w/o/r/agent-runs/`+itoa(test)+`"`) || strings.Contains(body, "Hidden step") || strings.Contains(body, "agent-runs/"+itoa(step)+`"`) {
		t.Errorf("the canvas tests:\n%s", body)
	}
	if !(at("Canvas tests") > at(runLink(first))) {
		t.Error("the canvas tests come before the runs")
	}

	// The item's case page lists the runs on that item, not the others'.
	_, page := b.get("/w/o/r/cases/7")
	if !strings.Contains(page, "Workflow runs") || !strings.Contains(page, runLink(first)) || !strings.Contains(page, runLink(second)) || strings.Contains(page, runLink(going)) || strings.Contains(page, runLink(stopped)) {
		t.Errorf("the case page's runs:\n%s", page)
	}

	// It is linked from the workspace page and the canvas.
	for _, from := range []string{"/w/o/r", "/w/o/r/workflows/pr"} {
		if _, p := b.get(from); !strings.Contains(p, `href="/w/o/r/workflow-runs"`) {
			t.Errorf("%s does not link to the runs", from)
		}
	}

	// With nothing going the page stops updating itself.
	e.st.FinishWorkflowRun(going, "done")
	e.st.SaveAgentRun(test, "met", "{}", 0)
	_, body = b.get("/w/o/r/workflow-runs")
	if strings.Contains(body, `/static/live.js`) {
		t.Error("a page with nothing going still updates itself")
	}
	// Only members see it.
	m := e.signIn("mallory")
	if code, _ := m.get("/w/o/r/workflow-runs"); code != http.StatusNotFound {
		t.Errorf("a non-member: %d", code)
	}
}

// The Runs page lists every run in progress and the 100 most recent that have ended.
func TestRunsPageLimit(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	for i := 0; i < recentRuns+5; i++ {
		id, _ := e.st.StartWorkflowRun(store.WorkflowRun{WorkspaceID: ws.ID, Kind: workflow.PR, EventNode: "ev", Item: 1000 + i, Graph: "{}"}, nil, derek)
		e.st.FinishWorkflowRun(id, "done")
	}
	going, _ := e.st.StartWorkflowRun(store.WorkflowRun{WorkspaceID: ws.ID, Kind: workflow.PR, EventNode: "ev", Item: 5, Graph: "{}"}, nil, derek)
	_, body := e.signIn("derek").get("/w/o/r/workflow-runs")
	if n := strings.Count(body, "/w/o/r/workflow-runs/"); n != recentRuns+1 {
		t.Errorf("%d runs listed, want %d", n, recentRuns+1)
	}
	if !strings.Contains(body, "workflow-runs/"+itoa(going)+`"`) || strings.Contains(body, "#1000<") {
		t.Error("the oldest runs are listed, or the one in progress is not")
	}
}

// The Runs page shows who's who: the item's author (from the local copy, or the poller's record of it),
// and who started the run: a person, or Live for the poller. A queued run says its place in line, with
// a grey dot, and stays in the in-progress part. The case page's list has the same columns.
func TestRunsPageWhoIsWho(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	approvalWorkflow(t, e, ws, derek)
	w, _ := e.st.Workflow(ws.ID, workflow.PR)
	if err := e.st.SaveItem(ws.ID, store.Item{Number: 7, Kind: "pr", Title: "Retry uploads", State: "open", Author: "alice-in-index", Created: time.Now(), Updated: time.Now()}, nil); err != nil {
		t.Fatal(err)
	}
	e.st.SavePollItem(ws.ID, store.PollItem{Number: 8, Kind: "pr", Open: true, Author: "bob-from-poller", Updated: time.Now()})
	e.st.OpenCase(ws, 7, "pr", "Retry uploads", "Opened")
	start := func(item int, status string, dry bool, by *store.User) int64 {
		id, err := e.st.StartWorkflowRun(store.WorkflowRun{WorkspaceID: ws.ID, Kind: workflow.PR, EventNode: "ev", Item: item, DryRun: dry, Graph: w.Graph, Status: status}, []string{"ev"}, by)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	byHand := start(7, "running", true, derek)
	live := start(8, "running", false, nil) // the poller's
	q1 := start(9, "queued", false, nil)
	q2 := start(9, "queued", false, derek)
	done := start(7, "running", false, nil)
	e.st.FinishWorkflowRun(done, "done")

	b := e.signIn("derek")
	_, body := b.get("/w/o/r/workflow-runs")
	var got []string
	for _, h := range regexp.MustCompile(`<th>([^<]*)</th>`).FindAllStringSubmatch(body, 8) {
		got = append(got, h[1])
	}
	if want := "Item Author Block Status Started Started by Took "; strings.Join(got, " ") != want {
		t.Errorf("columns %q, want %q", strings.Join(got, " "), want)
	}
	row := func(id int64) string {
		t.Helper()
		for _, r := range strings.Split(body, "</tr>") {
			if strings.Contains(r, `workflow-runs/`+itoa(id)+`"`) {
				return r
			}
		}
		t.Fatalf("no row for run %d:\n%s", id, body)
		return ""
	}
	for id, wants := range map[int64][]string{
		byHand: {`<td class="nowrap">alice-in-index</td>`, `<td class="nowrap">PR First Ready <span class="muted">dry run</span></td>`, `<td class="nowrap">derek</td>`},
		live:   {`<td class="nowrap">bob-from-poller</td>`, `<span class="muted">Live</span>`},
		q1:     {`<span class="dot off"></span> queued (1st in line)`, `<span class="muted">Live</span>`},
		q2:     {`<span class="dot off"></span> queued (2nd in line)`, `<td class="nowrap">derek</td>`},
		done:   {`<span class="muted">Live</span>`, `<span class="dot on"></span> done`},
	} {
		for _, want := range wants {
			if r := row(id); !strings.Contains(r, want) {
				t.Errorf("run %d lacks %q:\n%s", id, want, r)
			}
		}
	}
	if strings.Contains(row(byHand), "Live") {
		t.Error("a run a person started says Live")
	}
	// The queued runs are in progress: after those running, before the ended run.
	at := func(id int64) int { return strings.Index(body, `workflow-runs/`+itoa(id)+`"`) }
	if !(at(byHand) > 0 && at(live) < at(byHand) && at(byHand) < at(q1) && at(q1) < at(q2) && at(q2) < at(done)) {
		t.Errorf("order: live %d, by hand %d, q1 %d, q2 %d, done %d", at(live), at(byHand), at(q1), at(q2), at(done))
	}
	if !strings.Contains(body, `/static/live.js`) {
		t.Error("a page with queued runs does not update itself")
	}
	// The same columns on the item's case page.
	_, page := b.get("/w/o/r/cases/7")
	for _, want := range []string{"<th>Author</th>", "<th>Started by</th>", "alice-in-index", `<span class="muted">Live</span>`, ">derek</td>"} {
		if !strings.Contains(page, want) {
			t.Errorf("the case page lacks %q:\n%s", want, page)
		}
	}
}
