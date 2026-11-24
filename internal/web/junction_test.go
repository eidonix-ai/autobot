package web

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

func junction(id string) workflow.Node { return workflow.Node{ID: id, Type: workflow.Junction} }

// Event, A, a Node, then B, C and D: all three run, at the same time (the provider answers none of them
// until all three have asked), and each sees A's tag and has A's output as {input}. The Node keeps no
// tag in the case, and its step is done at once with what came into it.
func TestNodeFansOutInParallel(t *testing.T) {
	c := newContEnv(t, nil)
	var mu sync.Mutex
	asked := 0
	all := make(chan struct{})
	c.llm.status = func(int) int {
		mu.Lock()
		asked++
		if asked == 3 {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
			return 0
		case <-time.After(5 * time.Second):
			return 500 // the three did not overlap
		}
	}
	b := c.agentNode("B", "B", func(s *workflow.AgentSettings) { s.Instruction = "B sees {a} | {input}" })
	cc := c.agentNode("C", "C", func(s *workflow.AgentSettings) { s.Instruction = "C sees {a} | {input}" })
	d := c.agentNode("D", "D", func(s *workflow.AgentSettings) { s.Instruction = "D sees {a} | {input}" })
	c.saveGraph(append(append([]workflow.Node{}, events...), caseEntryNode("A", "A", "A TEXT"), junction("J"), b, cc, d),
		[2]string{"ev1", "A"}, [2]string{"A", "J"}, [2]string{"J", "B"}, [2]string{"J", "C"}, [2]string{"J", "D"})
	steps := c.run("ev1")
	for _, id := range []string{"A", "J", "B", "C", "D"} {
		if steps[id].Status != "done" {
			t.Fatalf("%s: %+v", id, steps[id])
		}
	}
	if steps["J"].Output != "A TEXT" || steps["J"].Port != "out" {
		t.Errorf("the Node's step: %+v", steps["J"])
	}
	reqs := strings.Join(c.llm.requests(), "\n")
	for _, who := range []string{"B", "C", "D"} {
		if !strings.Contains(reqs, who+" sees A TEXT | A TEXT") {
			t.Errorf("%s did not see {a} and {input} as A's output:\n%s", who, reqs)
		}
	}
	if v := c.caseValues(); v["a"] != "A TEXT" || v["node"] != "" || len(v) < 4 {
		t.Errorf("tags kept: %v", v)
	} else {
		for k := range v {
			if strings.HasPrefix(k, "node") || k == "j" {
				t.Errorf("the Node kept a tag %q", k)
			}
		}
	}
}

// (B, C) into a Node, then E: E waits for both, and is skipped when neither fired; when one fired, E
// runs on that one alone.
func TestNodeJoinWaitsForAllAndNeedsOne(t *testing.T) {
	hold := make(chan struct{})
	c := newContEnv(t, hold)
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	c.saveGraph(append(append([]workflow.Node{}, events...),
		caseEntryNode("B", "B", "B TEXT"), c.agentNode("C", "C", nil), junction("J"), caseEntryNode("E", "E", "{input}"),
		// Neither of these is reached by the event that runs.
		caseEntryNode("P", "P", "P TEXT"), caseEntryNode("Q", "Q", "Q TEXT"), junction("J2"), caseEntryNode("E2", "E2", "{input}"),
		// One of these is.
		caseEntryNode("R", "R", "R TEXT"), junction("J3"), caseEntryNode("E3", "E3", "{input}")),
		[2]string{"ev1", "B"}, [2]string{"ev1", "C"}, [2]string{"B", "J"}, [2]string{"C", "J"}, [2]string{"J", "E"},
		[2]string{"ev2", "P"}, [2]string{"ev2", "Q"}, [2]string{"P", "J2"}, [2]string{"Q", "J2"}, [2]string{"J2", "E2"},
		[2]string{"ev1", "R"}, [2]string{"P", "J3"}, [2]string{"R", "J3"}, [2]string{"J3", "E3"})
	id := c.start("ev1")
	var steps map[string]store.WorkflowStep
	for i := 0; i < 200; i++ { // B is done and C is held by the provider
		steps, _ = c.st.WorkflowSteps(id)
		if steps["B"].Status == "done" && steps["C"].Status == "running" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	steps, _ = c.st.WorkflowSteps(id)
	if steps["B"].Status != "done" || steps["C"].Status != "running" || steps["J"].Status != "pending" || steps["E"].Status != "pending" {
		t.Fatalf("with C still running, B %s, C %s, the Node %s, E %s", steps["B"].Status, steps["C"].Status, steps["J"].Status, steps["E"].Status)
	}
	release()
	steps = c.finish(id)
	if steps["J"].Status != "done" || steps["E"].Status != "done" || steps["E"].Output != "## B\n\nB TEXT\n\n## C\n\nReport 1." {
		t.Errorf("after both: the Node %+v, E %+v", steps["J"], steps["E"])
	}
	if steps["J2"].Status != "skipped" || steps["E2"].Status != "skipped" {
		t.Errorf("neither fired: the Node %s, E2 %s", steps["J2"].Status, steps["E2"].Status)
	}
	if steps["J3"].Status != "done" || steps["E3"].Status != "done" || steps["E3"].Output != "R TEXT\n\n## P (skipped)" {
		t.Errorf("one fired: the Node %+v, E3 %+v", steps["J3"], steps["E3"])
	}
}

// Three links into a Node and three out: what follows it has what it would have had with the links
// joined directly, {input} and each of its parts included, through one Node or a chain of two, and the
// tags of everything upstream.
func TestNodePassesEverythingThrough(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = outputsModel(`{"status":"met","report":"RESEARCH SHORT"}`)
	a := c.agentNode("A", "Gather", func(s *workflow.AgentSettings) {
		s.Closing, s.ReportTurns, s.MaxTurns, s.SubAgents = workflow.ClosingReport, 4, 10, 1
	})
	same := "{input} | {input.summary} | {input.report} | [{input.scratchpad}] | {previous_output} | {gather.report} {side} {more}"
	c.saveGraph(append(append([]workflow.Node{}, events...), a,
		caseEntryNode("side", "Side", "SIDE TEXT"), caseEntryNode("more", "More", "MORE TEXT"),
		junction("J"), junction("K"),
		caseEntryNode("X", "X", same), caseEntryNode("Y", "Y", same), caseEntryNode("Z", "Z", same),
		caseEntryNode("W", "W", same), caseEntryNode("direct", "Direct", same),
		caseEntryNode("whole", "Whole", "{input}")),
		[2]string{"ev1", "A"}, [2]string{"ev1", "side"}, [2]string{"ev1", "more"},
		[2]string{"A", "J"}, [2]string{"side", "J"}, [2]string{"more", "J"},
		[2]string{"J", "X"}, [2]string{"J", "Y"}, [2]string{"J", "Z"}, [2]string{"J", "K"}, [2]string{"K", "W"}, [2]string{"J", "whole"},
		[2]string{"A", "direct"}, [2]string{"side", "direct"}, [2]string{"more", "direct"})
	steps := c.run("ev1")
	for id, s := range steps {
		if s.Status == "failed" {
			t.Fatalf("%s failed: %+v", id, s)
		}
	}
	want := steps["direct"].Output
	if !strings.Contains(want, "## Gather") || !strings.Contains(want, "THE SUMMARY") || !strings.Contains(want, "RESEARCH SHORT") || !strings.Contains(want, "SIDE TEXT MORE TEXT") {
		t.Fatalf("the direct links gave %q", want)
	}
	for _, id := range []string{"X", "Y", "Z", "W"} {
		if steps[id].Status != "done" || steps[id].Output != want {
			t.Errorf("%s: %s\n got %q\nwant %q", id, steps[id].Status, steps[id].Output, want)
		}
	}
	if steps["J"].Output != steps["whole"].Output || steps["K"].Output != steps["whole"].Output || !strings.HasPrefix(steps["whole"].Output, "## Gather\n\n## Summary") {
		t.Errorf("the Node's own output %q, the next step's {input} %q", steps["J"].Output, steps["whole"].Output)
	}
}
