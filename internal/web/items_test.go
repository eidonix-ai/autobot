package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// itemsEnv is a workspace o/r with a pull request workflow of two event blocks leading to a case entry,
// and an index holding a few items, newest activity first: PR 1 (open, 1 hour), 3 (open draft, 2
// hours), 2 (open, 1 day), 4 (merged 5 days ago), 5 (closed 40 days ago: too old to list), and
// issue 10.
func itemsEnv(t *testing.T) (*env, *store.Workspace, *store.User) {
	e := newEnv(t)
	derek := e.user("derek", true)
	e.user("mallory", false)
	ws, err := e.st.CreateWorkspace("o/r", derek, "", "")
	if err != nil {
		t.Fatal(err)
	}
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	g := workflow.Graph{Nodes: []workflow.Node{
		{ID: "ev", Type: "pr_ready"},
		{ID: "ev2", Type: "pr_draft", Name: "Draft check"},
		{ID: "ce", Type: workflow.CaseEntry, Name: "Note", Settings: raw(workflow.CaseEntrySettings{Entry: "Looked at {pr_title}.", Finalize: true})},
	}, Links: []workflow.Link{{From: "ev", Out: "out", To: "ce", In: "in"}, {From: "ev2", Out: "out", To: "ce", In: "in"}}}
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(raw(g)), derek); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	save := func(n int, kind, title, state, author string, updated time.Duration, draft, merged bool, labels ...string) {
		t.Helper()
		it := store.Item{Number: n, Kind: kind, Title: title, State: state, Author: author, Draft: draft, Merged: merged, Labels: labels,
			Created: now.Add(-90 * 24 * time.Hour), Updated: now.Add(-updated)}
		if state == "closed" {
			it.Closed = it.Updated
			if merged {
				it.MergedAt = it.Updated
			}
		}
		if err := e.st.SaveItem(ws.ID, it, nil); err != nil {
			t.Fatal(err)
		}
	}
	save(2, "pr", "Older open", "open", "bob", 24*time.Hour, false, false)
	save(1, "pr", "Retry uploads", "open", "alice", time.Hour, false, false, "bug", "storage")
	save(3, "pr", "Draft idea", "open", "carol", 2*time.Hour, true, false)
	save(4, "pr", "Merged lately", "closed", "alice", 5*24*time.Hour, false, true)
	save(5, "pr", "Closed long ago", "closed", "bob", 40*24*time.Hour, false, false)
	save(10, "issue", "Uploads hang", "open", "dave", 3*time.Hour, false, false)
	return e, ws, derek
}

// pos is where want first shows in body, or -1.
func pos(body, want string) int { return strings.Index(body, want) }

// The page lists the open items most recently updated first, then (with Closed or All) those closed in the
// last 30 days, each with its number linked to GitHub, title, author, labels, draft or ready, and its
// last activity; the other kind is on the other tab.
func TestItemsPageRows(t *testing.T) {
	e, _, _ := itemsEnv(t)
	b := e.signIn("derek")

	code, body := b.get("/w/o/r/items/pr")
	if code != 200 {
		t.Fatalf("%d", code)
	}
	for _, want := range []string{`href="https://github.com/o/r/pull/1">#1</a>`, "Retry uploads", "alice", "bug, storage", ">ready<", ">draft<", "1 hour ago", "2 hours ago", "1 day ago"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q:\n%s", want, body)
		}
	}
	// Eastern time on hover.
	if !strings.Contains(body, ` ET">1 hour ago`) {
		t.Errorf("the activity time is not shown in Eastern time on hover:\n%s", body)
	}
	if !(pos(body, "#1<") > 0 && pos(body, "#1<") < pos(body, "#3<") && pos(body, "#3<") < pos(body, "#2<")) {
		t.Errorf("open items are not by recent activity: %d %d %d", pos(body, "#1<"), pos(body, "#3<"), pos(body, "#2<"))
	}
	for _, not := range []string{"#4<", "#5<", "#10<", "Uploads hang"} {
		if strings.Contains(body, not) {
			t.Errorf("the open pull requests list shows %q", not)
		}
	}

	// All: open first, then those closed within 30 days; the merged one says so; the old one stays out.
	_, body = b.get("/w/o/r/items/pr?state=all")
	if !(pos(body, "#2<") > 0 && pos(body, "#2<") < pos(body, "#4<")) || !strings.Contains(body, ">merged<") || strings.Contains(body, "#5<") {
		t.Errorf("all:\n%s", body)
	}
	_, body = b.get("/w/o/r/items/pr?state=closed")
	if !strings.Contains(body, "#4<") || strings.Contains(body, "#1<") || strings.Contains(body, "#5<") {
		t.Errorf("closed:\n%s", body)
	}
	// An unknown state is Open.
	_, body = b.get("/w/o/r/items/pr?state=bogus")
	if !strings.Contains(body, "#1<") || strings.Contains(body, "#4<") {
		t.Errorf("bogus state:\n%s", body)
	}

	// The issues are on their own tab, which links back.
	_, body = b.get("/w/o/r/items/issue")
	if !strings.Contains(body, `href="https://github.com/o/r/issues/10">#10</a>`) || strings.Contains(body, "Retry uploads") || !strings.Contains(body, `href="/w/o/r/items/pr"`) {
		t.Errorf("issues:\n%s", body)
	}
	if code, _ := b.get("/w/o/r/items/nonsense"); code != http.StatusNotFound {
		t.Errorf("an unknown kind: %d", code)
	}
}

// Search finds the words in titles, descriptions and comments; "Has a case file" keeps only items with a
// real case; both are in the address, so a URL can be shared.
func TestItemsPageFilters(t *testing.T) {
	e, ws, _ := itemsEnv(t)
	it := store.Item{Number: 2, Kind: "pr", Title: "Older open", Body: "Tune the checkpoint cadence", State: "open", Author: "bob", Updated: time.Now().Add(-24 * time.Hour)}
	e.st.SaveItem(ws.ID, it, nil)
	e.st.SaveComments(ws.ID, []store.ItemComment{{Kind: "comment", ID: 1, Number: 3, Author: "x", Body: "needs a migration plan", Created: time.Now(), Updated: time.Now()}})
	e.st.OpenCase(ws, 3, "pr", "Draft idea", "Opened")
	e.st.OpenCase(ws, 1, "pr", "Retry uploads", "Opened")
	b := e.signIn("derek")

	shows := func(path string, want, not []string) {
		t.Helper()
		_, body := b.get(path)
		for _, w := range want {
			if !strings.Contains(body, w+"<") {
				t.Errorf("%s lacks %s", path, w)
			}
		}
		for _, n := range not {
			if strings.Contains(body, n+"<") {
				t.Errorf("%s shows %s", path, n)
			}
		}
	}
	shows("/w/o/r/items/pr?q=checkpoint", []string{"#2"}, []string{"#1", "#3"})       // a description
	shows("/w/o/r/items/pr?q=retry", []string{"#1"}, []string{"#2", "#3"})            // a title, stemmed
	shows("/w/o/r/items/pr?q=migration+plan", []string{"#3"}, []string{"#1", "#2"})   // a comment
	shows("/w/o/r/items/pr?q=migration+checkpoint", nil, []string{"#1", "#2", "#3"})  // every word is needed
	shows("/w/o/r/items/pr?case=on", []string{"#1", "#3"}, []string{"#2"})            // real cases only
	shows("/w/o/r/items/pr?case=on&q=idea", []string{"#3"}, []string{"#1", "#2"})     // together
	shows("/w/o/r/items/pr?q=retry&state=closed", nil, []string{"#1", "#4"})          // search follows the state
	shows("/w/o/r/items/pr?q=merged&state=all", []string{"#4"}, []string{"#1", "#5"}) // closed items are searched too
	_, body := b.get("/w/o/r/items/pr?q=%21%21%21")                                   // no words
	if !strings.Contains(body, "at least one word") {
		t.Errorf("a search with no words:\n%s", body)
	}
	// The form is a GET to the page itself and shows what was asked.
	_, body = b.get("/w/o/r/items/pr?q=retry&state=all&case=on")
	for _, want := range []string{`method="get" action="/w/o/r/items/pr"`, `value="retry"`, `<option value="all" selected>`, `name="case" checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("the filter form lacks %q", want)
		}
	}
}

// Case links to the item's real case file; a dry run's case never counts, as the item's case or for the
// filter. Last run is the latest run on the item, with its status, a dry-run marker and a link; a run in
// progress makes the page update itself.
func TestItemsPageCaseAndLastRun(t *testing.T) {
	e, ws, derek := itemsEnv(t)
	e.st.OpenCase(ws, 1, "pr", "Retry uploads", "Opened")
	// Item 2 has only a dry run's case.
	g, _ := e.st.Workflow(ws.ID, workflow.PR)
	start := func(item int, dry bool) int64 {
		id, err := e.st.StartWorkflowRun(store.WorkflowRun{WorkspaceID: ws.ID, Kind: workflow.PR, EventNode: "ev", Item: item, DryRun: dry, Graph: g.Graph}, []string{"ev"}, derek)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	dry := start(2, true)
	e.st.OpenDryCase(ws, dry, 2, "pr", "Older open", "Opened")
	e.st.FinishWorkflowRun(dry, "done")
	first := start(1, false)
	e.st.FinishWorkflowRun(first, "failed")
	second := start(1, true)  // the latest on item 1, still going
	other := start(10, false) // an issue's number on the pull request kind: not listed here
	_ = other
	b := e.signIn("derek")

	_, body := b.get("/w/o/r/items/pr")
	rowOf := func(n string) string {
		i := strings.Index(body, ">#"+n+"<")
		j := strings.Index(body[i:], "</tr>")
		if i < 0 || j < 0 {
			t.Fatalf("no row for #%s:\n%s", n, body)
		}
		return body[i : i+j]
	}
	if r := rowOf("1"); !strings.Contains(r, `href="/w/o/r/cases/1"`) || !strings.Contains(r, `href="/w/o/r/workflow-runs/`+itoa(second)+`"`) ||
		!strings.Contains(r, "running") || !strings.Contains(r, "dry run") || strings.Contains(r, "failed") {
		t.Errorf("row 1:\n%s", r)
	}
	if r := rowOf("2"); strings.Contains(r, "/cases/") || strings.Contains(r, "dry-cases") ||
		!strings.Contains(r, `href="/w/o/r/workflow-runs/`+itoa(dry)+`"`) || !strings.Contains(r, "done") || !strings.Contains(r, "dry run") {
		t.Errorf("row 2: a dry run's case shows as the item's, or its run is not shown:\n%s", r)
	}
	if r := rowOf("3"); strings.Contains(r, "workflow-runs") || strings.Contains(r, "/cases/") || strings.Contains(r, "dry run") {
		t.Errorf("row 3 has neither:\n%s", r)
	}
	if !strings.Contains(body, `/static/live.js`) || !strings.Contains(body, `id="items-list" data-live data-live-list`) {
		t.Error("a page with a run going does not update itself")
	}
	// A real run's status, not a dry one's marker.
	e.st.FinishWorkflowRun(second, "done")
	_, body = b.get("/w/o/r/items/pr")
	if strings.Contains(body, `/static/live.js`) {
		t.Error("a page with nothing going still updates itself")
	}
	// The filter does not count the dry run's case.
	_, body = b.get("/w/o/r/items/pr?case=on")
	if !strings.Contains(body, "#1<") || strings.Contains(body, "#2<") {
		t.Errorf("Has a case file counts a dry run's case:\n%s", body)
	}
}

// Before the copy of pull requests and issues is complete the page says so, and shows what it has.
func TestItemsPageIndexNotReady(t *testing.T) {
	e, ws, _ := itemsEnv(t)
	b := e.signIn("derek")
	_, body := b.get("/w/o/r/items/pr")
	if !strings.Contains(body, "still being made") || !strings.Contains(body, "#1<") {
		t.Errorf("before the first copy:\n%s", body)
	}
	e.st.IndexSet(ws.ID, "backfill_phase", "done")
	_, body = b.get("/w/o/r/items/pr")
	if strings.Contains(body, "still being made") || !strings.Contains(body, "#1<") {
		t.Errorf("after the first copy:\n%s", body)
	}
}

// The Run form lists the kind's event blocks by name, with Dry run ticked, and posts to start the run;
// without an event block the row says there is no workflow.
func TestItemsRunForm(t *testing.T) {
	e, ws, _ := itemsEnv(t)
	b := e.signIn("derek")
	_, body := b.get("/w/o/r/items/pr")
	for _, want := range []string{`action="/w/o/r/items/pr/run"`, `<option value="ev">PR First Ready</option>`, `<option value="ev2">Draft check</option>`,
		`name="dry_run" checked`, `name="item" value="1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `value="ce"`) || strings.Contains(body, "No workflow") {
		t.Errorf("a step is offered as an event block, or a row says No workflow:\n%s", body)
	}
	// The issue workflow has no event block.
	_, body = b.get("/w/o/r/items/issue")
	if !strings.Contains(body, "No workflow") || strings.Contains(body, "<summary>Run</summary>") {
		t.Errorf("issues without an event block:\n%s", body)
	}

	// Posting what the form holds starts a dry run from the chosen block and goes to its page.
	tok := b.csrf()
	code, loc, out := b.post("/w/o/r/items/pr/run", url.Values{"csrf": {tok}, "item": {"7"}, "block": {"ev2"}, "dry_run": {"on"}})
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/w/o/r/workflow-runs/") {
		t.Fatalf("start: %d %q\n%s", code, loc, out)
	}
	id := int64(atoi(strings.TrimPrefix(loc, "/w/o/r/workflow-runs/")))
	run, err := e.st.WorkflowRun(id)
	if err != nil || !run.DryRun || run.EventNode != "ev2" || run.Item != 7 || run.Kind != workflow.PR {
		t.Fatalf("the run: %+v %v", run, err)
	}
	waitFor(t, "the run to end", func() bool { r, _ := e.st.WorkflowRun(id); return r.Status != "running" })
	if _, err := e.st.CaseByNumber(ws.ID, 7); err == nil {
		t.Error("a dry run opened the item's case file")
	}
	// Without Dry run ticked it is a real run.
	code, loc, _ = b.post("/w/o/r/items/pr/run", url.Values{"csrf": {tok}, "item": {"7"}, "block": {"ev"}})
	id = int64(atoi(strings.TrimPrefix(loc, "/w/o/r/workflow-runs/")))
	if run, _ := e.st.WorkflowRun(id); code != http.StatusSeeOther || run == nil || run.DryRun {
		t.Fatalf("a real run: %d %+v", code, run)
	}
	waitFor(t, "the run to end", func() bool { r, _ := e.st.WorkflowRun(id); return r.Status != "running" })

	// Mistakes come back on the page, not as a run.
	for name, form := range map[string]url.Values{
		"no number":    {"csrf": {tok}, "block": {"ev"}},
		"not an event": {"csrf": {tok}, "item": {"7"}, "block": {"ce"}},
		"no such node": {"csrf": {tok}, "item": {"7"}, "block": {"zzz"}},
	} {
		before, _ := e.st.WorkflowRuns(ws.ID, 100)
		code, _, out := b.post("/w/o/r/items/pr/run", form)
		after, _ := e.st.WorkflowRuns(ws.ID, 100)
		if code != http.StatusBadRequest || !strings.Contains(out, `class="error"`) || len(after) != len(before) {
			t.Errorf("%s: %d, %d runs before, %d after\n%s", name, code, len(before), len(after), out)
		}
	}
}

// [M] Only members of the workspace, and admins, see the page and start runs; a POST needs the form token.
func TestItemsAccess(t *testing.T) {
	e, ws, derek := itemsEnv(t)
	alice := e.user("alice", false)
	if err := e.st.SetMembers(ws.ID, []int64{alice.ID}, derek); err != nil {
		t.Fatal(err)
	}
	e.user("outsider", false)
	other, _ := e.st.CreateWorkspace("o/other", derek, "", "") // not alice's, with a workflow of its own
	g, _ := e.st.Workflow(ws.ID, workflow.PR)
	e.st.SaveWorkflow(other.ID, workflow.PR, 0, g.Graph, derek)

	// Members and admins see it.
	for _, who := range []string{"alice", "derek"} {
		if code, body := e.signIn(who).get("/w/o/r/items/pr"); code != 200 || !strings.Contains(body, "Retry uploads") {
			t.Errorf("%s: %d", who, code)
		}
	}
	// To someone who may not open the workspace, it does not exist; nor does it without signing in.
	for _, path := range []string{"/w/o/r/items/pr", "/w/o/r/items/issue"} {
		if code, body := e.signIn("outsider").get(path); code != http.StatusNotFound || strings.Contains(body, "Retry uploads") {
			t.Errorf("an outsider at %s: %d", path, code)
		}
		if code, loc := redirect(t, e.browser(), path); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/login") {
			t.Errorf("signed out at %s: %d %q", path, code, loc)
		}
	}

	runs := func() int { r, _ := e.st.WorkflowRuns(ws.ID, 100); return len(r) }
	form := func(tok string) url.Values {
		return url.Values{"csrf": {tok}, "item": {"7"}, "block": {"ev"}, "dry_run": {"on"}}
	}
	// An outsider's POST, with a token of their own, starts nothing.
	o := e.signIn("outsider")
	if code, _, _ := o.post("/w/o/r/items/pr/run", form(o.csrf())); code != http.StatusNotFound || runs() != 0 {
		t.Errorf("an outsider started a run: %d, %d runs", code, runs())
	}
	// A member without the form token, or with a forged one, starts nothing.
	m := e.signIn("alice")
	for _, tok := range []string{"", "forged"} {
		if code, _, _ := m.post("/w/o/r/items/pr/run", form(tok)); code != http.StatusForbidden || runs() != 0 {
			t.Errorf("token %q: %d, %d runs", tok, code, runs())
		}
	}
	// With it, a member starts one, and on an admin's behalf nothing else changes.
	code, loc, _ := m.post("/w/o/r/items/pr/run", form(m.csrf()))
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/w/o/r/workflow-runs/") {
		t.Fatalf("a member's run: %d %q", code, loc)
	}
	id := int64(atoi(strings.TrimPrefix(loc, "/w/o/r/workflow-runs/")))
	waitFor(t, "the run to end", func() bool { r, _ := e.st.WorkflowRun(id); return r.Status != "running" })
	// A member of one workspace cannot see or run another's.
	if code, body := m.get("/w/o/other/items/pr"); code != http.StatusNotFound || strings.Contains(body, "Retry uploads") {
		t.Errorf("another workspace's page: %d", code)
	}
	if code, _, _ := m.post("/w/o/other/items/pr/run", form(m.csrf())); code != http.StatusNotFound {
		t.Errorf("another workspace's run: %d", code)
	}
	if r, _ := e.st.WorkflowRuns(other.ID, 100); len(r) != 0 {
		t.Errorf("a run started in the other workspace: %d", len(r))
	}
}

// The page is linked from the workspace page and the canvas, next to Runs.
func TestItemsPageLinks(t *testing.T) {
	e, _, _ := itemsEnv(t)
	b := e.signIn("derek")
	if _, p := b.get("/w/o/r"); !strings.Contains(p, `href="/w/o/r/items/pr"`) {
		t.Error("the workspace page does not link to the pull requests")
	}
	for kind, label := range map[string]string{"pr": "Pull requests", "issue": "Issues"} {
		_, p := b.get("/w/o/r/workflows/" + kind)
		if !strings.Contains(p, `<a href="/w/o/r/items/`+kind+`">`+label+`</a>`) {
			t.Errorf("the %s canvas does not link to its items", kind)
		}
	}
}
