package web

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/workflow"
)

// Live-started workflow runs have actor == nil: QueueSession and QueueRun must handle nil actor
// cleanly instead of panicking on actor.ID.
func TestQueueSessionAndRunAllowNilActor(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, err := e.st.CreateWorkspace("o/r", derek, "flare-autobot", "tok-good")
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := e.st.AddMachine(ws.ID, "cn7", derek)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := e.st.QueueSession(m, "main", "0123456789abcdef0123456789abcdef01234567", false, 1, time.Minute, nil)
	if err != nil || sess.ID == 0 || !sess.Session {
		t.Fatalf("QueueSession with nil actor: %+v err=%v", sess, err)
	}
	if err := e.st.FinishRun(sess.ID, "passed", "done"); err != nil {
		t.Fatal(err)
	}
	run, err := e.st.QueueRun(m, "echo ok", "main", "0123456789abcdef0123456789abcdef01234567", false, 1, time.Minute, nil)
	if err != nil || run.ID == 0 || run.Session {
		t.Fatalf("QueueRun with nil actor: %+v err=%v", run, err)
	}
}

// Machine-bound test agents do not wait in or hold PR/Issue workflow slots; instead they serialize
// cleanly per available machine (1 job per machine) without failing when the machine is busy.
func TestMachineAgentBypassesPRQueueAndQueuesPerMachine(t *testing.T) {
	oldMachinePoll := machinePoll
	oldAgentPoll := agentPoll
	machinePoll = 40 * time.Millisecond
	agentPoll = 40 * time.Millisecond
	t.Cleanup(func() {
		machinePoll = oldMachinePoll
		agentPoll = oldAgentPoll
	})

	e := newGateEnv(t, 1) // PR/Issue MaxRuns = 1
	cn7, tok7, err := e.st.AddMachine(e.ws.ID, "cn7", e.admin)
	if err != nil {
		t.Fatal(err)
	}
	e.fakeConnector(t, tok7)
	waitFor(t, "cn7 online", func() bool { _, on := e.ui.live.get(cn7.ID); return on })

	// Workflow with two entry points:
	// - ev1 -> ag (regular PR agent, occupies the 1 PR/Issue slot)
	// - ev2 -> ap (Approval) -> te (machine-bound Test Engineer on cn7)
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	reg := workflow.DefaultAgent(workflow.PR)
	reg.Persona, reg.Model, reg.Instruction, reg.Goal, reg.NoTools = "custom", workflow.ModelRef{Provider: e.prov.ID, Model: "m"}, "review", "done", true
	te := workflow.DefaultAgent(workflow.PR)
	te.Persona, te.Model, te.Instruction, te.Goal, te.Machines = "custom", workflow.ModelRef{Provider: e.prov.ID, Model: "m"}, "test on hw", "done", []int64{cn7.ID}
	ap := workflow.DefaultApproval(workflow.PR)

	g := workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "ev1", Type: "pr_ready"},
			{ID: "ag", Type: workflow.Agent, Name: "Review", Settings: raw(reg)},
			{ID: "ev2", Type: "pr_activity"},
			{ID: "ap", Type: workflow.Approval, Name: "Approve HW", Settings: raw(ap)},
			{ID: "te", Type: workflow.Agent, Name: "Test Engineer", Settings: raw(te)},
		},
		Links: []workflow.Link{
			{From: "ev1", Out: "out", To: "ag", In: "in"},
			{From: "ev2", Out: "out", To: "ap", In: "in"},
			{From: "ap", Out: "approved", To: "te", In: "in"},
		},
	}
	w, _ := e.st.Workflow(e.ws.ID, workflow.PR)
	if _, err := e.st.SaveWorkflow(e.ws.ID, workflow.PR, w.Version, string(raw(g)), e.admin); err != nil {
		t.Fatal(err)
	}

	// Start two approval runs on ev2 (they immediately wait on Approval and release their slots).
	hw1 := e.startItem("ev2", false)
	waitFor(t, "hw1 waiting on approval", func() bool { s, _ := e.st.WorkflowSteps(hw1); return s["ap"].Status == "waiting" })
	hw2 := e.startItem("ev2", false)
	waitFor(t, "hw2 waiting on approval", func() bool { s, _ := e.st.WorkflowSteps(hw2); return s["ap"].Status == "waiting" })

	// Now fill the single PR/Issue slot with a regular PR review blocked at the LLM gate.
	pr1 := e.startItem("ev1", false)
	e.gate.waitEntered(t, 1)

	// Approve BOTH hw1 and hw2 while pr1 holds the only PR/Issue slot!
	for _, rid := range []int64{hw1, hw2} {
		apRow, err := e.st.ApprovalFor(rid, "ap")
		if err != nil {
			t.Fatalf("run %d approval: %v", rid, err)
		}
		if ok, err := e.st.Decide(apRow.ID, "approved", "derek", "web"); err != nil || !ok {
			t.Fatalf("decide run %d: %v %v", rid, ok, err)
		}
	}

	// One of hw1/hw2 must bypass the full PR/Issue queue and enter the LLM on cn7 immediately!
	e.gate.waitEntered(t, 1)
	// The second waits cleanly for cn7 (still running, NOT failed!).
	e.gate.quiet(t, "only one test agent should run on cn7 at a time")
	if got := e.statuses(pr1, hw1, hw2); got != "running running running" {
		t.Fatalf("while cn7 runs first test and pr1 holds PR slot: %s", got)
	}

	// Release all gated LLM calls: hw1 finishes on cn7 -> hw2 claims cn7 and finishes -> pr1 finishes.
	e.gate.openAll()
	for _, id := range []int64{hw1, hw2, pr1} {
		e.waitStatus(id, "done")
	}
}
