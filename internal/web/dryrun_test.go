package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// A dry run writes to a case file of its own and never reads or writes the item's real one: its
// entries and trial post go there, {case} is that file, no tag is kept, and the real case's tags are
// not read. [M]
func TestDryRunHasItsOwnCaseFile(t *testing.T) {
	c := newContEnv(t, nil)
	c.st.SetSetting(publicAddressKey, "https://autobot.example.com")
	post := workflow.DefaultPost()
	post.Message = "Would say: {review.summary}"
	c.saveGraph(append(append([]workflow.Node{}, events...),
		c.agentNode("ag", "Review", nil),
		caseEntryNode("pr", "Prior", "Prior review: [{review.report}]"), // starts with the run, before the agent has kept its tag
		caseEntryNode("ce", "Notes", "Case so far: {case} at {case:link}"),
		workflow.Node{ID: "po", Type: workflow.Post, Name: "Say", Settings: rawJSON(post)}),
		[2]string{"ev1", "ag"}, [2]string{"ev1", "pr"}, [2]string{"ag", "ce"}, [2]string{"ce", "po"})

	// Two real runs: the item has a case file with a kept tag and entries, and the second sees the first's tag.
	c.run("ev1")
	steps := c.run("ev1")
	if !strings.Contains(steps["pr"].Output, "Report ") {
		t.Fatalf("a real run does not see the case's kept tag (the control): %q", steps["pr"].Output)
	}
	real, err := c.st.CaseByNumber(c.ws.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Mark the real case so that anything leaking from it into a dry run shows.
	c.st.SetCaseValue(real.UUID, "review.report", "REAL TAG VALUE", "ag")
	real, _ = c.st.CaseByNumber(c.ws.ID, 1)
	realValues := c.caseValues()
	pagesBefore, _ := c.st.PagesOf(real.UUID)

	id, err := c.ui.startWorkflow(c.t.Context(), c.ws, workflow.PR, "ev1", 1, true, c.admin)
	if err != nil {
		t.Fatal(err)
	}
	steps = c.finish(id)
	if steps["po"].Status != "done" || steps["ce"].Status != "done" || steps["pr"].Status != "done" {
		t.Fatalf("the dry run: %+v", steps)
	}
	if strings.Contains(steps["pr"].Output, "REAL TAG VALUE") {
		t.Errorf("the dry run read the real case's tags: %q", steps["pr"].Output)
	}

	// The real case file is as it was: contents, version, time, kept tags, pages.
	after, err := c.st.CaseByNumber(c.ws.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if after.UUID != real.UUID || after.Contents != real.Contents || after.Version != real.Version || !after.UpdatedAt.Equal(real.UpdatedAt) {
		t.Fatalf("the real case file changed:\n%s\n---\n%s", real.Contents, after.Contents)
	}
	if got := c.caseValues(); len(got) != len(realValues) || got["review.report"] != "REAL TAG VALUE" {
		t.Fatalf("the real case's values changed: %v", got)
	}
	if pages, _ := c.st.PagesOf(real.UUID); len(pages) != len(pagesBefore) {
		t.Fatalf("a dry run published into the real case: %+v", pages)
	}

	// The dry run's own case file has what a real run would have written.
	dry, err := c.st.DryCase(id)
	if err != nil {
		t.Fatalf("no case file for the dry run: %v", err)
	}
	if dry.UUID == real.UUID || dry.DryRunOf != id || dry.Number != 1 || dry.Kind != "pr" || !strings.HasSuffix(dry.Title, " (dry run)") {
		t.Fatalf("the dry run's case: %+v", dry)
	}
	if !strings.HasPrefix(dry.Contents, "**Dry run of workflow run "+itoa(id)+": not a real case file.**") {
		t.Fatalf("no banner:\n%s", dry.Contents)
	}
	for _, want := range []string{"## Review", "Prior review: [", "Case so far: ", "Would post on GitHub:", "Would say: "} {
		if !strings.Contains(dry.Contents, want) {
			t.Errorf("the dry run's case lacks %q:\n%s", want, dry.Contents)
		}
	}
	if strings.Contains(dry.Contents, "REAL TAG VALUE") {
		t.Errorf("the real case's values are in the dry run's case:\n%s", dry.Contents)
	}
	// {case} and {case:link} in the run are the dry run's case.
	if !strings.Contains(steps["ce"].Output, "/api/v1/cases/"+dry.UUID+".html") || strings.Contains(steps["ce"].Output, real.UUID) {
		t.Errorf("{case:link} in a dry run: %q", steps["ce"].Output)
	}
	if !strings.HasPrefix(steps["ce"].Output, "Case so far: **Dry run of workflow run "+itoa(id)+": not a real case file.**") {
		t.Errorf("{case} in a dry run is the real case: %q", steps["ce"].Output)
	}
	// Kept tags stay in memory.
	if vals, _ := c.st.CaseValues(dry.UUID); len(vals) != 0 {
		t.Errorf("a dry run kept tags: %v", vals)
	}

	// Nothing a lookup by the item's number does can find the dry run's case.
	if got, _ := c.st.CaseByNumber(c.ws.ID, 1); got.UUID != real.UUID {
		t.Fatal("the lookup by number found the dry run's case")
	}
}

// A dry run's approval outcome goes to its own case file too.
func TestDryRunApprovalOutcome(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	approvalWorkflow(t, e, ws, derek)
	id, err := e.ui.startWorkflow(t.Context(), ws, workflow.PR, "ev", 7, true, derek)
	if err != nil {
		t.Fatal(err)
	}
	var ap *store.Approval
	waitFor(t, "the request", func() bool { ap, _ = e.st.ApprovalFor(id, "ap"); return ap != nil })
	b := e.signIn("derek")
	if code, _, _ := b.post("/w/o/r/workflow-runs/"+itoa(id)+"/approvals/"+itoa(ap.ID), url.Values{"csrf": {b.csrf()}, "answer": {"approved"}}); code != http.StatusSeeOther {
		t.Fatalf("answer: %d", code)
	}
	waitFor(t, "the run to end", func() bool { r, _ := e.st.WorkflowRun(id); return r.Status != "running" })
	dry, err := e.st.DryCase(id)
	if err != nil || !strings.Contains(dry.Contents, "Approved by derek") || !strings.Contains(dry.Contents, "Running it.") {
		t.Fatalf("the dry run's case: %v\n%v", err, dry)
	}
	if c, err := e.st.CaseByNumber(ws.ID, 7); err == nil {
		t.Fatalf("a dry run opened the item's case:\n%s", c.Contents)
	}
}

// The dry run's case file is a page of its own, with a banner on top; the run's page links to it,
// the Cases page lists it after the real ones with a badge, and the item's case page does not show it.
func TestDryRunCasePages(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveGraph(append(append([]workflow.Node{}, events...), caseEntryNode("ce", "Notes", "A note.")), [2]string{"ev1", "ce"})
	c.addPR(2, false)
	c.run("ev1") // a real case for #1
	id, err := c.ui.startWorkflow(c.t.Context(), c.ws, workflow.PR, "ev1", 2, true, c.admin)
	if err != nil {
		t.Fatal(err)
	}
	c.finish(id)
	dry, _ := c.st.DryCase(id)
	page := "/w/o/r/dry-cases/" + itoa(id)

	_, body := c.b.get("/w/o/r/workflow-runs/" + itoa(id))
	if !strings.Contains(body, `href="`+page+`"`) || !strings.Contains(body, "Case file (dry run)") {
		t.Errorf("the run page does not link to the dry run's case:\n%s", body)
	}
	code, body := c.b.get(page)
	if code != 200 || !strings.Contains(body, "Dry run of workflow run "+itoa(id)+": not a real case file.") || !strings.Contains(body, "(dry run)") || strings.Contains(body, "/edit") {
		t.Errorf("the dry run's case page: %d\n%s", code, body)
	}
	if code, _ := c.b.get("/w/o/r/cases/2"); code != http.StatusNotFound {
		t.Errorf("the item's case page for a dry run's item: %d", code)
	}
	// The public view and the API carry the banner too.
	if code, body := c.e().apiGet(t, nil, "/api/v1/cases/"+dry.UUID+".html", ""); code != 200 || !strings.Contains(body, "Dry run of workflow run "+itoa(id)+": not a real case file.") {
		t.Errorf("the public page: %d\n%s", code, body)
	}
	if code, body := c.e().apiGet(t, nil, "/api/v1/cases/"+dry.UUID, ""); code != 200 || !strings.HasPrefix(body, "**Dry run of workflow run") {
		t.Errorf("the API: %d\n%s", code, body)
	}
	// Another workspace cannot read it through its own address.
	c.st.CreateWorkspace("o/r2", c.admin, "", "")
	if code, _ := c.b.get("/w/o/r2/dry-cases/" + itoa(id)); code != http.StatusNotFound {
		t.Errorf("another workspace's dry-case page: %d", code)
	}
	_, body = c.b.get("/w/o/r/cases")
	real, dryRow := strings.Index(body, `/w/o/r/cases/1"`), strings.Index(body, page+`"`)
	if real < 0 || dryRow < 0 || dryRow < real || !strings.Contains(body, ">dry run<") {
		t.Errorf("the Cases page: real at %d, dry at %d\n%s", real, dryRow, body)
	}
	if strings.Contains(body[:dryRow], "#2") {
		t.Errorf("the dry run's case is listed among the real ones:\n%s", body)
	}
}

// A dry run's case file and its pages go 30 days after the run ends, and never while it runs.
func TestDryCasesExpire(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	mk := func(item int) (int64, *store.Case) {
		id, err := e.st.StartWorkflowRun(store.WorkflowRun{WorkspaceID: ws.ID, Kind: workflow.PR, EventNode: "ev", Item: item, DryRun: true, Graph: "{}"}, nil, derek)
		if err != nil {
			t.Fatal(err)
		}
		c, _, err := e.st.OpenDryCase(ws, id, item, "pr", "A change", "Opened")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.Publish(c, "report", "text"); err != nil {
			t.Fatal(err)
		}
		return id, c
	}
	oldID, oldCase := mk(1)
	e.st.FinishWorkflowRun(oldID, "done")
	_, runningCase := mk(2)
	realCase, _, _ := e.st.OpenCase(ws, 3, "pr", "Real", "Opened")
	e.st.Publish(realCase, "report", "kept")

	if n, err := e.st.PruneDryCases(time.Now().Add(29 * 24 * time.Hour)); err != nil || n != 0 {
		t.Fatalf("pruned %d, %v at 29 days", n, err)
	}
	if n, err := e.st.PruneDryCases(time.Now().Add(31 * 24 * time.Hour)); err != nil || n != 1 {
		t.Fatalf("pruned %d, %v at 31 days", n, err)
	}
	if _, err := e.st.Case(oldCase.UUID); err == nil {
		t.Error("the old dry run's case is kept")
	}
	if pages, _ := e.st.PagesOf(oldCase.UUID); len(pages) != 0 {
		t.Errorf("its pages are kept: %+v", pages)
	}
	if _, err := e.st.Case(runningCase.UUID); err != nil {
		t.Errorf("the running dry run's case was deleted: %v", err)
	}
	if _, err := e.st.Case(realCase.UUID); err != nil {
		t.Errorf("a real case was deleted: %v", err)
	}
	if pages, _ := e.st.PagesOf(realCase.UUID); len(pages) != 1 {
		t.Errorf("a real case's pages: %+v", pages)
	}
}
