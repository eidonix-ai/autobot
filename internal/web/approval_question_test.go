package web

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// A pending card has the details, then the question in bold, then the answer-by line, with the
// two buttons; an answered one keeps the head it always had and the question in its body; a
// request made before the question existed shows as it always did.
func TestApprovalCard(t *testing.T) {
	n := workflow.Node{ID: "ap", Type: workflow.Approval, Name: "Bypass Low Value Review"}
	ap := &store.Approval{ID: 5, Status: "pending", Request: "Found: a typo fix.", Question: "Skip the review of #7?", ExpiresAt: time.Date(2026, 11, 25, 15, 0, 0, 0, time.UTC)}
	m := approvalMessage(n, ap)
	want := "**Approval needed** · Bypass Low Value Review\n\nFound: a typo fix.\n\n**Skip the review of #7?**\n_Answer by Nov 25, 10:00 AM ET._"
	if m.Text != want || len(m.Buttons) != 2 || m.Buttons[0].Label != "Approve" || m.Buttons[1].Label != "Deny" {
		t.Fatalf("%q %+v", m.Text, m.Buttons)
	}
	ap.Request = ""
	if m = approvalMessage(n, ap); strings.Contains(m.Text, "\n\n\n") || !strings.HasPrefix(m.Text, "**Approval needed** · Bypass Low Value Review\n\n**Skip the review of #7?**\n") {
		t.Fatalf("without details: %q", m.Text)
	}

	ap = &store.Approval{ID: 5, Status: "approved", DecidedBy: "Festr", DecidedOn: "Discord", DecidedAt: time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC),
		Request: "Found: a typo fix.", Question: "Skip the review of #7?"}
	m = approvalMessage(n, ap)
	if m.Text != "**Approved** by Festr on Discord · Nov 24, 10:00 AM ET\n\nFound: a typo fix.\n\n**Skip the review of #7?**" || len(m.Buttons) != 0 {
		t.Fatalf("answered: %q", m.Text)
	}
	if m = approvalMessage(n, &store.Approval{Status: "timed_out", Question: "Q?"}); m.Text != "**No answer in time** · Bypass Low Value Review\n\n**Q?**" {
		t.Fatalf("timed out: %q", m.Text)
	}

	old := &store.Approval{ID: 5, Status: "pending", Request: "Run it on cn7?", ExpiresAt: time.Date(2026, 11, 25, 15, 0, 0, 0, time.UTC)}
	if m = approvalMessage(n, old); m.Text != "**Approval needed** · Bypass Low Value Review\n\nRun it on cn7?\n\n_Answer by Nov 25, 10:00 AM ET._" || len(m.Buttons) != 2 {
		t.Fatalf("an old request: %q", m.Text)
	}
	old.Status, old.DecidedBy, old.DecidedOn, old.DecidedAt = "denied", "alice", "Autobot", time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	if m = approvalMessage(n, old); m.Text != "**Denied** by alice on Autobot · Nov 24, 10:00 AM ET\n\nRun it on cn7?" {
		t.Fatalf("an old answer: %q", m.Text)
	}
}

// The run's page shows the details, the question in bold and the two buttons; the case entry says
// the question and the answer; a block saved before the question existed still runs.
func TestApprovalQuestionOnPageAndEntry(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	approvalWorkflow(t, e, ws, derek)
	id, err := e.ui.startWorkflow(t.Context(), ws, workflow.PR, "ev", 7, false, derek)
	if err != nil {
		t.Fatal(err)
	}
	var ap *store.Approval
	waitFor(t, "the request", func() bool { ap, _ = e.st.ApprovalFor(id, "ap"); return ap != nil })
	if ap.Question != "Run Retry uploads on cn7?" || ap.Request != "Details of Retry uploads." {
		t.Fatalf("%+v", ap)
	}
	b := e.signIn("derek")
	_, page := b.get("/w/o/r/workflow-runs/" + itoa(id))
	i, j, k := strings.Index(page, "Details of Retry uploads."), strings.Index(page, "<strong>Run Retry uploads on cn7?</strong>"), strings.Index(page, `value="approved"`)
	if i < 0 || j < i || k < j {
		t.Fatalf("details %d, question %d, buttons %d", i, j, k)
	}
	b.post("/w/o/r/workflow-runs/"+itoa(id)+"/approvals/"+itoa(ap.ID), url.Values{"csrf": {b.csrf()}, "answer": {"denied"}})
	waitFor(t, "the run to end", func() bool { r, _ := e.st.WorkflowRun(id); return r.Status != "running" })
	c, _ := e.st.CaseByNumber(ws.ID, 7)
	if !strings.Contains(c.Contents, "**Hardware:** Run Retry uploads on cn7? → Denied by derek on Autobot, ") || !strings.Contains(c.Contents, "\n> Details of Retry uploads.") {
		t.Fatalf("the case entry:\n%s", c.Contents)
	}
}

// A block saved with details only (before the question) runs and asks as it did.
func TestApprovalWithoutQuestionRuns(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	g := workflow.Graph{Nodes: []workflow.Node{{ID: "ev", Type: "pr_ready"},
		{ID: "ap", Type: workflow.Approval, Name: "Hardware", Settings: raw(workflow.ApprovalSettings{Request: "Run it?", Hours: 1})}},
		Links: []workflow.Link{{From: "ev", Out: "out", To: "ap", In: "in"}}}
	e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(raw(g)), derek)
	id, err := e.ui.startWorkflow(t.Context(), ws, workflow.PR, "ev", 7, true, derek)
	if err != nil {
		t.Fatal(err)
	}
	var ap *store.Approval
	waitFor(t, "the request", func() bool { ap, _ = e.st.ApprovalFor(id, "ap"); return ap != nil })
	if ap.Question != "" || ap.Request != "Run it?" {
		t.Fatalf("%+v", ap)
	}
	e.st.Decide(ap.ID, "approved", "derek", "Autobot")
	waitFor(t, "the run to end", func() bool { r, _ := e.st.WorkflowRun(id); return r.Status != "running" })
	if dry, err := e.st.DryCase(id); err != nil || !strings.Contains(dry.Contents, "**Hardware:** Approved by derek on Autobot, ") || strings.Contains(dry.Contents, "→") {
		t.Fatalf("%v\n%v", err, dry)
	}
}

// The block's dialog saves and shows the question and the details; the question is required and
// a tag in it is checked.
func TestApprovalDialog(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	approvalWorkflow(t, e, ws, derek)
	b := e.signIn("derek")
	post := func(question, details string) (int, string) {
		w, _ := e.st.Workflow(ws.ID, workflow.PR)
		code, _, body := b.post("/w/o/r/workflows/pr/nodes/ap", url.Values{"csrf": {b.csrf()}, "version": {itoa(int64(w.Version))},
			"name": {"Hardware"}, "hours": {"2"}, "question": {question}, "request": {details}})
		return code, body
	}
	if _, body := post("  ", "Some details."); !strings.Contains(body, "write the question asked above the buttons") {
		t.Fatalf("an empty question was saved: %s", body)
	}
	if _, body := post("Run {nosuchthing}?", ""); !strings.Contains(body, "nosuchthing") {
		t.Fatalf("an unknown tag in the question was saved: %s", body)
	}
	if code, body := post("Skip the review of {pr}?", ""); code != 200 {
		t.Fatalf("save: %d %s", code, body)
	}
	if code, body := post("Skip the review of {pr}?", "Found: {pr:title}."); code != 200 {
		t.Fatalf("save: %d %s", code, body)
	}
	_, form := b.get("/w/o/r/workflows/pr/nodes/ap")
	for _, want := range []string{`name="question" value="Skip the review of {pr}?"`, `name="request"`, "Found: {pr:title}.", ">Question <", ">Details <"} {
		if !strings.Contains(form, want) {
			t.Errorf("the dialog lacks %q:\n%s", want, form)
		}
	}
	if strings.Index(form, `name="question"`) > strings.Index(form, `name="request"`) {
		t.Error("the details come before the question")
	}
}
