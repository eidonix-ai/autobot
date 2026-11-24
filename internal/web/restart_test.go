package web

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// A deploy does not kill the runs in progress (Derek, 2026-11-24): a step that only computes (an Agent,
// a Decision, a Condition, a Node) and was cut off starts again, its run going back to the queue with
// what had finished kept; a Post, a Case Entry, a Test Run and an Approval never run twice; a dry run's
// steps are never started again (its values were only in memory).
func TestRestartRequeuesWhatMayRunAgain(t *testing.T) {
	old := queueEvery
	queueEvery = 50 * time.Millisecond
	t.Cleanup(func() { queueEvery = old })
	e := newGateEnv(t, 2)
	e.gate.openAll()
	post, _ := json.Marshal(workflow.PostSettings{Message: "hello", GitHub: true, Trial: true})
	e.saveGraph([]workflow.Node{events[0], e.gateAgent("A", "First"), e.gateAgent("B", "Second"), {ID: "P", Type: workflow.Post, Name: "Post", Settings: post}},
		[2]string{"ev1", "A"}, [2]string{"A", "B"})
	// cut makes a run as a process that died would leave it: the event finished, these steps running.
	cut := func(dry bool, running ...string) int64 {
		t.Helper()
		w, _ := e.st.Workflow(e.ws.ID, workflow.PR)
		id, err := e.st.StartWorkflowRun(store.WorkflowRun{WorkspaceID: e.ws.ID, Kind: workflow.PR, EventNode: "ev1", Item: e.item(), DryRun: dry, Graph: w.Graph},
			[]string{"ev1", "A", "B", "P"}, e.admin)
		if err != nil {
			t.Fatal(err)
		}
		e.st.SetWorkflowStep(id, store.WorkflowStep{NodeID: "ev1", Status: "done", Port: "out", Output: "PR First Ready", StartedAt: time.Unix(1000, 0), FinishedAt: time.Unix(1000, 0)})
		e.st.SetWorkflowStep(id, store.WorkflowStep{NodeID: "ev2", Status: "skipped"})
		for _, n := range running {
			e.st.SetWorkflowStep(id, store.WorkflowStep{NodeID: n, Status: "running", StartedAt: time.Now()})
		}
		return id
	}
	agent, posting, dry := cut(false, "A"), cut(false, "P"), cut(true, "A")
	if err := e.st.StopOrphanWorkflows(workflow.Restartable); err != nil {
		t.Fatal(err)
	}
	if got := e.statuses(agent, posting, dry); got != "queued failed failed" {
		t.Fatalf("after the restart: %s", got)
	}
	if steps, _ := e.st.WorkflowSteps(posting); steps["P"].Status != "failed" {
		t.Fatalf("the Post was not failed: %+v", steps["P"])
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { e.ui.RunQueue(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	e.waitStatus(agent, "done")
	steps, _ := e.st.WorkflowSteps(agent)
	if steps["A"].Status != "done" || steps["B"].Status != "done" || !steps["ev1"].StartedAt.Equal(time.Unix(1000, 0)) {
		t.Fatalf("the run went on from where it was: %+v", steps)
	}
}

// Only the steps that compute may run again.
func TestRestartableBlocks(t *testing.T) {
	g, _ := json.Marshal(workflow.Graph{Nodes: []workflow.Node{{ID: "a", Type: workflow.Agent}, {ID: "d", Type: workflow.Decision}, {ID: "c", Type: workflow.Condition},
		{ID: "j", Type: workflow.Junction}, {ID: "p", Type: workflow.Post}, {ID: "e", Type: workflow.CaseEntry}, {ID: "t", Type: workflow.TestRun}, {ID: "h", Type: workflow.Approval}}})
	for node, want := range map[string]bool{"a": true, "d": true, "c": true, "j": true, "p": false, "e": false, "t": false, "h": false, "missing": false} {
		if got := workflow.Restartable(string(g), node); got != want {
			t.Errorf("%s: %v, want %v", node, got, want)
		}
	}
}
