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

// PR Backlog and Issue Backlog are canvas event blocks: while their workflow is Live and the
// workspace is idle (0 queued, 0 waiting on approval slots, active PR/Issue runs < IdleSlots),
// they pick open items with no case file and no prior non-dry run, honoring order and filters.
func TestBacklogCatchUpPRAndIssue(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, err := e.st.CreateWorkspace("o/r", derek, "flare-autobot", "tok-good")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetOptOutTag(ws, "#autobot:ignore", derek); err != nil {
		t.Fatal(err)
	}

	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

	// PR workflow with a PR Backlog event block (newest first, 1 idle slot, skips drafts & bots).
	prEv := workflow.DefaultEvent(workflow.PRBacklog)
	prEv.WaitCI = "no"
	prEv.BacklogOrder = "newest"
	prEv.IdleSlots = 1
	prG := workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "evpr", Type: workflow.PRBacklog, Settings: raw(prEv)},
			{ID: "cepr", Type: workflow.CaseEntry, Name: "Note", Settings: raw(workflow.CaseEntrySettings{Entry: "Reviewed #{pr:number}", Finalize: true})},
		},
		Links: []workflow.Link{{From: "evpr", Out: "out", To: "cepr", In: "in"}},
	}
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(raw(prG)), derek); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetWorkflowLive(ws.ID, workflow.PR, true, derek); err != nil {
		t.Fatal(err)
	}

	// Issue workflow with an Issue Backlog event block (oldest first, 1 idle slot).
	isEv := workflow.DefaultEvent(workflow.IssueBacklog)
	isEv.BacklogOrder = "oldest"
	isEv.IdleSlots = 1
	isG := workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "evis", Type: workflow.IssueBacklog, Settings: raw(isEv)},
			{ID: "ceis", Type: workflow.CaseEntry, Name: "Note", Settings: raw(workflow.CaseEntrySettings{Entry: "Triaged #{issue:number}", Finalize: true})},
		},
		Links: []workflow.Link{{From: "evis", Out: "out", To: "ceis", In: "in"}},
	}
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.Issue, 0, string(raw(isG)), derek); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetWorkflowLive(ws.ID, workflow.Issue, true, derek); err != nil {
		t.Fatal(err)
	}

	// Mark the local item index ready and populate open PRs + issues.
	if err := e.st.IndexSet(ws.ID, "backfill_phase", "done"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, it := range []store.Item{
		{Number: 10, Kind: workflow.PR, Title: "Already has case", State: "open", Author: "alice", Base: "main", Updated: now.Add(-5 * time.Hour)},
		{Number: 11, Kind: workflow.PR, Title: "Opted out PR", Body: "please #autobot:ignore", State: "open", Author: "alice", Base: "main", Updated: now.Add(-4 * time.Hour)},
		{Number: 12, Kind: workflow.PR, Title: "Draft PR", State: "open", Draft: true, Author: "alice", Base: "main", Updated: now.Add(-3 * time.Hour)},
		{Number: 13, Kind: workflow.PR, Title: "Bot PR", State: "open", Author: "dependabot[bot]", Base: "main", Updated: now.Add(-2 * time.Hour)},
		{Number: 7, Kind: workflow.PR, Title: "Retry uploads", State: "open", Author: "alice", Base: "main", Updated: now.Add(-1 * time.Hour)},
		{Number: 8, Kind: workflow.Issue, Title: "Uploads hang", State: "open", Author: "bob", Updated: now.Add(-3 * time.Hour)},
	} {
		if err := e.st.SaveItem(ws.ID, it, nil); err != nil {
			t.Fatal(err)
		}
	}
	// #10 already has a real case file -> ineligible.
	if _, _, err := e.st.OpenCase(ws, 10, workflow.PR, "Already has case", "opened"); err != nil {
		t.Fatal(err)
	}
	// #7 has only a prior dry run -> still eligible!
	if _, err := e.st.StartWorkflowRun(store.WorkflowRun{WorkspaceID: ws.ID, Kind: workflow.PR, EventNode: "evpr", Item: 7, DryRun: true, Graph: "{}", Status: "done"}, nil, derek); err != nil {
		t.Fatal(err)
	}

	// Verify the PR Backlog settings modal renders and saves via HTTP.
	b := e.signIn("derek")
	if _, form := b.get("/w/o/r/workflows/pr/nodes/evpr"); !strings.Contains(form, `name="backlog_order"`) || !strings.Contains(form, `name="idle_slots"`) {
		t.Fatalf("PR Backlog settings form missing fields:\n%s", form)
	}
	w, _ := e.st.Workflow(ws.ID, workflow.PR)
	if code, _, out := b.post("/w/o/r/workflows/pr/nodes/evpr", url.Values{
		"csrf": {b.csrf()}, "version": {itoa(int64(w.Version))}, "name": {"PR Backlog"},
		"backlog_order": {"newest"}, "idle_slots": {"1"}, "authors": {"skip_bots"}, "over_limit": {"skip"}, "wait_ci": {"no"},
	}); code != http.StatusOK {
		t.Fatalf("save PR Backlog block: %d %s", code, out)
	}

	// Verify low priority: while a PR/Issue run holds a slot (idle_slots=1), backlogRound starts NOTHING.
	e.ui.slots.mu.Lock()
	e.ui.slots.take(999, slotPool{ws: ws.ID})
	e.ui.slots.mu.Unlock()
	e.ui.backlogRound(t.Context())
	if runs, _ := e.st.WorkflowRuns(ws.ID, 20); len(runs) != 1 {
		t.Fatalf("backlog started while workspace was busy: %+v", runs)
	}
	e.ui.slots.mu.Lock()
	delete(e.ui.slots.held, 999)
	e.ui.slots.mu.Unlock()

	// Verify low priority: while a queued PR/Issue run exists, backlogRound starts NOTHING.
	qid, _ := e.st.StartWorkflowRun(store.WorkflowRun{WorkspaceID: ws.ID, Kind: workflow.PR, EventNode: "evpr", Item: 99, Status: "queued", Graph: "{}"}, nil, derek)
	e.ui.backlogRound(t.Context())
	if runs, _ := e.st.WorkflowRuns(ws.ID, 20); len(runs) != 2 {
		t.Fatalf("backlog started while a run was queued: %+v", runs)
	}
	e.st.StopWorkflowRun(qid, "derek")

	// Round 1 (idle): LiveWorkflows orders by kind ("issue" then "pr"), so Issue #8 starts first.
	e.ui.backlogRound(t.Context())
	waitFor(t, "Issue #8 backlog run to finish", func() bool {
		runs, _ := e.st.WorkflowRuns(ws.ID, 20)
		for _, r := range runs {
			if r.Kind == workflow.Issue && r.Item == 8 && !r.DryRun {
				return r.Status == "done"
			}
		}
		return false
	})

	// Round 2 (idle): no more eligible issues -> moves to PR Backlog and starts #7
	// (skipping #10 has case, #11 opt-out, #12 draft, #13 bot).
	e.ui.backlogRound(t.Context())
	var prRunID int64
	waitFor(t, "PR #7 backlog run to finish", func() bool {
		runs, _ := e.st.WorkflowRuns(ws.ID, 20)
		for _, r := range runs {
			if r.Kind == workflow.PR && r.Item == 7 && !r.DryRun {
				prRunID = r.ID
				return r.Status == "done"
			}
		}
		return false
	})
	if prRunID == 0 {
		t.Fatal("expected a backlog run for PR #7")
	}
	if _, body := b.get("/w/o/r/workflow-runs"); !strings.Contains(body, "by Backlog") {
		t.Errorf("Runs page should attribute catch-up run to Backlog")
	}

	// Verify #11 (opt-out), #12 (draft), #13 (bot) were NEVER started.
	runs, _ := e.st.WorkflowRuns(ws.ID, 50)
	for _, r := range runs {
		if r.Item == 11 || r.Item == 12 || r.Item == 13 {
			t.Errorf("ineligible PR #%d was started by backlog: %+v", r.Item, r)
		}
	}
}
