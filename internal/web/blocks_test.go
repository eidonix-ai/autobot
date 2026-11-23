package web

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// runOnce runs a workspace's PR workflow from node ev on PR #7 and waits for it to end.
func runOnce(t *testing.T, e *env, ws *store.Workspace, by *store.User, dry bool) map[string]store.WorkflowStep {
	t.Helper()
	id, err := e.ui.startWorkflow(t.Context(), ws, workflow.PR, "ev", 7, dry, by)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if r, _ := e.st.WorkflowRun(id); r.Status != "running" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	steps, _ := e.st.WorkflowSteps(id)
	return steps
}

// A Condition routes by the item's facts; a GitHub Comment in a trial writes to the case file and
// posts nothing; posting for real keeps one comment, editing it on the next run.
func TestCommentAndCondition(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "flare-autobot", "tok-good")
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	comment := workflow.DefaultGitHubComment()
	comment.Message = "Reviewed {pr_title}."
	save := func(trial bool) {
		comment.Trial = trial
		g := workflow.Graph{Nodes: []workflow.Node{
			{ID: "ev", Type: "pr_ready"},
			{ID: "go", Type: workflow.Condition, Settings: raw(workflow.ConditionSettings{Match: "all", Files: "*.go"})},
			{ID: "cm", Type: workflow.GitHubComment, Name: "Review comment", Settings: raw(comment)},
			{ID: "nc", Type: workflow.CaseEntry, Settings: raw(workflow.CaseEntrySettings{Entry: "Not Go.", Finalize: true})},
		}, Links: []workflow.Link{{From: "ev", Out: "out", To: "go", In: "in"}, {From: "go", Out: "yes", To: "cm", In: "in"}, {From: "go", Out: "no", To: "nc", In: "in"}}}
		w, _ := e.st.Workflow(ws.ID, workflow.PR)
		if _, err := e.st.SaveWorkflow(ws.ID, workflow.PR, w.Version, string(raw(g)), derek); err != nil {
			t.Fatal(err)
		}
	}
	fakeComments = nil
	save(true)
	steps := runOnce(t, e, ws, derek, false)
	if steps["go"].Port != "yes" || steps["nc"].Status != "skipped" || steps["cm"].Status != "done" || len(fakeComments) != 0 {
		t.Fatalf("trial: %+v, posted %v", steps, fakeComments)
	}
	c, _ := e.st.CaseByNumber(ws.ID, 7)
	if !strings.Contains(c.Contents, "Would post on GitHub:") || !strings.Contains(c.Contents, "Reviewed Retry uploads.") {
		t.Fatalf("case file:\n%s", c.Contents)
	}
	save(false)
	runOnce(t, e, ws, derek, false)
	runOnce(t, e, ws, derek, false)
	if len(fakeComments) != 2 || !strings.HasPrefix(fakeComments[0], "post #7 by tok-good") || !strings.HasPrefix(fakeComments[1], "edit 101 by tok-good") {
		t.Fatalf("posted %q", fakeComments)
	}
	// A dry run posts nothing either.
	runOnce(t, e, ws, derek, true)
	if len(fakeComments) != 2 {
		t.Fatalf("a dry run posted: %q", fakeComments)
	}
}

// A Test Run queues the script on the workspace's machine with the PR's head and follows its result.
func TestTestRunStep(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	m, _, _ := e.st.AddMachine(ws.ID, "cn7", derek)
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	g := workflow.Graph{Nodes: []workflow.Node{
		{ID: "ev", Type: "pr_ready"},
		{ID: "tr", Type: workflow.TestRun, Settings: raw(workflow.TestRunSettings{Machine: m.ID, Script: "go test ./... # {pr_number}", Minutes: 5})},
		{ID: "ok", Type: workflow.CaseEntry, Settings: raw(workflow.CaseEntrySettings{Entry: "Tests passed.", Finalize: true})},
		{ID: "ko", Type: workflow.CaseEntry, Settings: raw(workflow.CaseEntrySettings{Entry: "Tests failed.", Finalize: true})},
	}, Links: []workflow.Link{{From: "ev", Out: "out", To: "tr", In: "in"}, {From: "tr", Out: "passed", To: "ok", In: "in"}, {From: "tr", Out: "failed", To: "ko", In: "in"}}}
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(raw(g)), derek); err != nil {
		t.Fatal(err)
	}
	go func() { // the machine's connector
		for i := 0; i < 400; i++ {
			if r, err := e.st.Run(1); err == nil {
				if r.Script != "go test ./... # 7" || r.Commit != "0123456789abcdef0123456789abcdef01234567" {
					t.Errorf("queued %q at %q", r.Script, r.Commit)
				}
				e.st.StartRun(1)
				e.st.FinishRun(1, "passed", "Passed.")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	steps := runOnce(t, e, ws, derek, false)
	if steps["tr"].Port != "passed" || steps["ok"].Status != "done" || steps["ko"].Status != "skipped" {
		t.Fatalf("%+v", steps)
	}
}
