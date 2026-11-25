package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// Backlog catch-up runs open PRs and issues that have no case file only when the workspace is idle
// (behind queued runs and slot waiters), respects event filters and opt-out tags, never retries
// items that already have a real case or prior non-dry run, and shows "Backlog" on the Runs page.
func TestBacklogCatchUpPRAndIssue(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	e.st.SetOptOutTag(ws, "Autobot: ignore", derek)
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

	// Save a PR workflow (PR First Ready -> Case Entry) and an Issue workflow (Issue Opened -> Case Entry).
	prEv := workflow.DefaultEvent("pr_ready")
	prEv.WaitCI = "no"
	prGraph := workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "evpr", Type: "pr_ready", Name: "PR First Ready", Settings: raw(prEv)},
			{ID: "cepr", Type: workflow.CaseEntry, Name: "Note", Settings: raw(workflow.CaseEntrySettings{Entry: "Reviewed #{pr:number}", Finalize: true})},
		},
		Links: []workflow.Link{{From: "evpr", Out: "out", To: "cepr", In: "in"}},
	}
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(raw(prGraph)), derek); err != nil {
		t.Fatal(err)
	}
	isEv := workflow.DefaultEvent("issue_opened")
	isGraph := workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "evis", Type: "issue_opened", Name: "Issue Opened", Settings: raw(isEv)},
			{ID: "ceis", Type: workflow.CaseEntry, Name: "Note", Settings: raw(workflow.CaseEntrySettings{Entry: "Triaged #{issue:number}", Finalize: true})},
		},
		Links: []workflow.Link{{From: "evis", Out: "out", To: "ceis", In: "in"}},
	}
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.Issue, 0, string(raw(isGraph)), derek); err != nil {
		t.Fatal(err)
	}

	// Mark the local item index ready and populate open PRs + issues.
	now := time.Now()
	if err := e.st.IndexSet(ws.ID, "backfill_phase", "done"); err != nil {
		t.Fatal(err)
	}
	saveItem := func(num int, kind, author, title, body string, draft bool) {
		t.Helper()
		if err := e.st.SaveItem(ws.ID, store.Item{
			Number: num, Kind: kind, Title: title, Body: body, State: "open", Draft: draft,
			Author: author, Base: "main", Created: now, Updated: now,
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	saveItem(10, workflow.PR, "alice", "Already has case", "ok", false)
	saveItem(11, workflow.PR, "alice", "Draft PR", "wip", true)
	saveItem(12, workflow.PR, "alice", "Opted out PR", "Autobot: ignore please", false)
	saveItem(13, workflow.PR, "alice", "Eligible older PR", "needs review", false)
	saveItem(14, workflow.PR, "alice", "Eligible newer PR", "needs review", false)
	saveItem(20, workflow.Issue, "bob", "Eligible issue", "bug report", false)

	// PR #10 already has a real case file -> never picked.
	if _, _, err := e.st.OpenCase(ws, 10, workflow.PR, "Already has case", "Opened."); err != nil {
		t.Fatal(err)
	}
	// PR #14 has a dry run -> still eligible!
	if _, err := e.st.StartWorkflowRun(store.WorkflowRun{WorkspaceID: ws.ID, Kind: workflow.PR, EventNode: "evpr", Item: 14, DryRun: true, Graph: "{}", Status: "done"}, nil, derek); err != nil {
		t.Fatal(err)
	}

	// Fake GitHub answers for PR #13, #14 and Issue #20.
	e.ui.gh = backlogGH{
		fakeGitHub: fakeGitHub{},
		prs: map[int]*github.PullRequest{
			13: {Number: 13, Title: "Eligible older PR", Body: "needs review", State: "open", Author: "alice", Base: "main", URL: "https://github.com/o/r/pull/13"},
			14: {Number: 14, Title: "Eligible newer PR", Body: "needs review", State: "open", Author: "alice", Base: "main", URL: "https://github.com/o/r/pull/14"},
		},
		issues: map[int]*github.Item{
			20: {Number: 20, Kind: "issue", Title: "Eligible issue", Body: "bug report", State: "open", Author: "bob", URL: "https://github.com/o/r/issues/20"},
		},
	}

	// Configure PR backlog via HTTP form (newest first, 1 idle slot).
	b := e.signIn("derek")
	if code, _, body := b.post("/w/o/r/workflows/pr/backlog", url.Values{
		"csrf": {b.csrf()}, "enabled": {"on"}, "node_id": {"evpr"}, "order": {"newest"}, "max_slots": {"1"},
	}); code != http.StatusSeeOther {
		t.Fatalf("save PR backlog: %d\n%s", code, body)
	}
	// Configure Issue backlog via HTTP form.
	if code, _, body := b.post("/w/o/r/workflows/issue/backlog", url.Values{
		"csrf": {b.csrf()}, "enabled": {"on"}, "node_id": {"evis"}, "order": {"oldest"}, "max_slots": {"1"},
	}); code != http.StatusSeeOther {
		t.Fatalf("save Issue backlog: %d\n%s", code, body)
	}

	// Verify low priority: while a run holds a slot (max_slots=1), backlogRound starts NOTHING.
	e.ui.slots.mu.Lock()
	e.ui.slots.take(999, slotPool{ws: ws.ID})
	e.ui.slots.mu.Unlock()
	e.ui.backlogRound(t.Context())
	if runs, _ := e.st.WorkflowRuns(ws.ID, 20); len(runs) != 1 { // only the earlier dry run
		t.Fatalf("backlog started while workspace was busy: %+v", runs)
	}
	e.ui.slots.mu.Lock()
	delete(e.ui.slots.held, 999)
	e.ui.slots.mu.Unlock()

	// Verify low priority: while a queued run exists, backlogRound starts NOTHING.
	qid, _ := e.st.StartWorkflowRun(store.WorkflowRun{WorkspaceID: ws.ID, Kind: workflow.PR, EventNode: "evpr", Item: 99, Status: "queued", Graph: "{}"}, nil, derek)
	e.ui.backlogRound(t.Context())
	if runs, _ := e.st.WorkflowRuns(ws.ID, 20); len(runs) != 2 {
		t.Fatalf("backlog started while a run was queued: %+v", runs)
	}
	e.st.StopWorkflowRun(qid, "derek")

	waitAllDone := func(wantTotal int) {
		t.Helper()
		for i := 0; i < 100; i++ {
			runs, _ := e.st.WorkflowRuns(ws.ID, 20)
			if len(runs) >= wantTotal {
				allDone := true
				for _, r := range runs {
					if r.Status == "running" || r.Status == "queued" {
						allDone = false
					}
				}
				if allDone {
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		runs, _ := e.st.WorkflowRuns(ws.ID, 20)
		t.Fatalf("timed out waiting for %d runs (have %d: %+v)", wantTotal, len(runs), runs)
	}

	// Idle round 1: picks PR #14 (newest first, skipping draft #11, opted-out #12, and case-having #10).
	e.ui.backlogRound(t.Context())
	waitAllDone(3)
	if _, err := e.st.CaseByNumber(ws.ID, 14); err != nil {
		t.Fatalf("PR #14 was not reviewed by backlog catch-up: %v", err)
	}

	// Idle round 2: picks PR #13.
	e.ui.backlogRound(t.Context())
	waitAllDone(4)
	if _, err := e.st.CaseByNumber(ws.ID, 13); err != nil {
		t.Fatalf("PR #13 was not reviewed by backlog catch-up: %v", err)
	}

	// Idle round 3: no more eligible PRs -> picks Issue #20!
	e.ui.backlogRound(t.Context())
	waitAllDone(5)
	if _, err := e.st.CaseByNumber(ws.ID, 20); err != nil {
		t.Fatalf("Issue #20 was not reviewed by backlog catch-up: %v", err)
	}

	// Idle round 4: nothing left -> starts nothing (draft #11 and opted-out #12 remain untouched).
	e.ui.backlogRound(t.Context())
	if runs, _ := e.st.WorkflowRuns(ws.ID, 20); len(runs) != 5 {
		t.Fatalf("backlog started an ineligible or already-run item: %d runs", len(runs))
	}

	// Runs page shows "Backlog" as who started catch-up runs.
	if code, body := b.get("/w/o/r/workflow-runs?q=backlog"); code != 200 || !strings.Contains(body, "by Backlog") || !strings.Contains(body, "#14") {
		t.Fatalf("runs page missing Backlog attribution: %d", code)
	}
}

type backlogGH struct {
	fakeGitHub
	prs    map[int]*github.PullRequest
	issues map[int]*github.Item
}

func (g backlogGH) PullRequest(_ context.Context, _, _ string, n int) (*github.PullRequest, error) {
	if pr, ok := g.prs[n]; ok {
		return pr, nil
	}
	return g.fakeGitHub.PullRequest(context.Background(), "", "", n)
}

func (g backlogGH) Item(_ context.Context, _, _ string, n int) (*github.Item, error) {
	if it, ok := g.issues[n]; ok {
		return it, nil
	}
	return g.fakeGitHub.Item(context.Background(), "", "", n)
}
