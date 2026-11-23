package web

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

func toolCall(id, name, args string) agent.Message {
	tc := agent.ToolCall{ID: id, Type: "function"}
	tc.Function.Name, tc.Function.Arguments = name, args
	return agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{tc}}
}

// closingModel plays a model through research, the save turn and the writing of the report: it
// knows which session it is in from the request. With writerFails, the writing session only ever
// answers in empty words.
func closingModel(writerFails bool) func([]agent.Message, []string) agent.Message {
	return func(msgs []agent.Message, tools []string) agent.Message {
		first, last := msgs[1], msgs[len(msgs)-1]
		switch {
		case strings.Contains(first.Content, "Your research is done"):
			switch {
			case writerFails:
				return agent.Message{Role: "assistant"}
			case last.Role == "user":
				return toolCall("w1", "scratchpad_index", `{}`)
			}
			return toolCall("w2", "finish", `{"status":"met","report":"WRITTEN REPORT: the findings, high ones first."}`)
		case strings.HasPrefix(last.Content, "Before your report is written"):
			return toolCall("s1", "scratchpad_write", `{"title":"Late","importance":"medium","description":"found late","body":"body of the late note"}`)
		}
		if slices.ContainsFunc(msgs, func(m agent.Message) bool { return m.Role == "assistant" }) {
			return toolCall("r2", "finish", `{"status":"met","report":"RESEARCH REPORT"}`)
		}
		return toolCall("r1", "scratchpad_write", `{"title":"Found","importance":"high","description":"the main finding","body":"body of the main finding"}`)
	}
}

func (c *contEnv) closingBlock(id string) workflow.Node {
	return c.agentNode(id, "Researcher", func(s *workflow.AgentSettings) {
		s.Rerun, s.Closing, s.ReportTurns, s.MaxTurns = "resume", workflow.ClosingReport, 4, 8
	})
}

// With the closing step, the research ends with its summary in the scratchpad, gets one turn to save
// notes with only scratchpad_write, and a new session with read-only tools writes the report from the
// notes; the run's report is that one, its page shows both sessions and the scratchpad, and the next
// run of the thread opens with the written report.
func TestClosingReportEndToEnd(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = closingModel(false)
	b := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) { s.ContinueOf = "A" })
	c.saveGraph(append(append([]workflow.Node{}, events...), c.closingBlock("A"), b), [2]string{"ev1", "A"}, [2]string{"ev2", "B"})
	steps := c.run("ev1")
	run := c.agentRun(steps, "A")
	if steps["A"].Status != "done" || steps["A"].Output != "WRITTEN REPORT: the findings, high ones first." || run.Status != "met" {
		t.Fatalf("A: %+v, run %s", steps["A"], run.Status)
	}
	var tr agent.Transcript
	json.Unmarshal([]byte(run.Summary), &tr)
	if tr.Outcome != "met" || tr.Writing == nil || tr.Writing.Turns != 2 || tr.Writing.Outcome != "met" || tr.Turns != 3 {
		t.Fatalf("summary %s", run.Summary)
	}
	// The notes: the finding, the research's own report, the late note, and the final report.
	pad := c.pad(run.Thread)
	var titles []string
	for _, e := range pad {
		titles = append(titles, e.Title)
	}
	if len(pad) != 4 || strings.Join(titles[:3], "|") != "Found|Research summary|Late" || pad[1].Importance != "high" || pad[1].Body != "RESEARCH REPORT" ||
		!pad[3].Report || pad[3].Title != reportTitle(run.ID) || pad[3].Body != "WRITTEN REPORT: the findings, high ones first." {
		t.Fatalf("the scratchpad: %+v", pad)
	}
	// What each session was offered.
	var research, save, writer []string
	for _, body := range c.llm.requests() {
		tools := requestTools(t, body)
		switch msgs := messagesOf(t, body); {
		case strings.Contains(msgs[1].Content, "Your research is done"):
			writer = tools
		case strings.HasPrefix(msgs[len(msgs)-1].Content, "Before your report is written"):
			save = tools
		default:
			research = tools
		}
	}
	if !slices.Contains(research, "compact") || !slices.Contains(research, "scratchpad_write") || !slices.Contains(research, "read_file") {
		t.Fatalf("research tools %v", research)
	}
	if !slices.Equal(save, research) {
		t.Fatalf("the save turn changed the tools: %v", save)
	}
	allowed := append([]string{"finish", "update_plan", "prune_result", "scratchpad_index", "scratchpad_read", "scratchpad_search"}, readOnlyTools...)
	for _, name := range writer {
		if !slices.Contains(allowed, name) {
			t.Errorf("the writing session was offered %s", name)
		}
	}
	for _, name := range []string{"compact", "scratchpad_write", "bash", "fetch_url", "web_search", "write_file"} {
		if slices.Contains(writer, name) {
			t.Errorf("the writing session has %s", name)
		}
	}
	if !slices.Contains(writer, "read_file") || !slices.Contains(writer, "scratchpad_read") {
		t.Fatalf("writer tools %v", writer)
	}
	// Two conversations, the writer's own: its first message carries the instruction, Done when and the index.
	if run.WriterConversation == 0 || run.WriterConversation == run.Conversation {
		t.Fatalf("conversations %d and %d", run.Conversation, run.WriterConversation)
	}
	wevs, err := c.st.ReadConv(run.WriterConversation)
	if err != nil || !strings.Contains(string(wevs[1].M), "Done when: goal of A") || !strings.Contains(string(wevs[1].M), "#2 [high] Research summary") {
		t.Fatalf("the writer's file: %v %s", err, wevs[1].M)
	}
	revs, _ := c.st.ReadConv(run.Conversation)
	for _, e := range revs {
		if strings.Contains(string(e.M), "Your research is done") {
			t.Fatal("the writing is in the research conversation")
		}
	}
	// The run's page: both sessions, the scratchpad, the final report.
	code, page := c.b.get(fmt.Sprintf("/w/o/r/agent-runs/%d", run.ID))
	for _, want := range []string{"Goal met", "WRITTEN REPORT: the findings", "Scratchpad", "body of the late note", "#2 · high · main", "Research summary", "Writing the report", "Writing the report: 2 turns",
		`id="run-pad"`, `id="run-writing"`, `id="run-transcript"`, "scratchpad_index", "Your research is done"} {
		if code != 200 || !strings.Contains(page, want) {
			t.Errorf("the run page (%d) lacks %q", code, want)
		}
	}
	// The next run of the thread opens with the written report, not the research's.
	c.run("ev2")
	reqs := c.llm.requests()
	opening := messagesOf(t, reqs[len(reqs)-1])[1].Content
	if got := section(opening, "Your last report on this item:\n", "\n\nYour notes so far"); got != "WRITTEN REPORT: the findings, high ones first." {
		t.Fatalf("it opened with %q", got)
	}
}

// A writing session that ends with no report leaves the run with the research's own finish report;
// with none of that either, the scratchpad's high and medium notes in order. The run's outcome stays
// the research's.
func TestClosingReportFallsBackToTheNotes(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = closingModel(true)
	c.saveGraph(append(append([]workflow.Node{}, events...), c.closingBlock("A")), [2]string{"ev1", "A"})
	steps := c.run("ev1")
	run := c.agentRun(steps, "A")
	if steps["A"].Output != "RESEARCH REPORT" || run.Status != "met" {
		t.Fatalf("output %q, run %s", steps["A"].Output, run.Status)
	}
	var tr agent.Transcript
	json.Unmarshal([]byte(run.Summary), &tr)
	if tr.Writing == nil || tr.Writing.Outcome != agent.Unfinished {
		t.Fatalf("writing %+v", tr.Writing)
	}
	// No research report either: the notes.
	c3 := newContEnv(t, nil)
	model := closingModel(true)
	c3.llm.reply = func(msgs []agent.Message, tools []string) agent.Message {
		if len(msgs) > 0 && !strings.Contains(msgs[1].Content, "Your research is done") && slices.ContainsFunc(msgs, func(m agent.Message) bool { return m.Role == "assistant" }) &&
			!strings.HasPrefix(msgs[len(msgs)-1].Content, "Before your report is written") {
			return agent.Message{Role: "assistant"} // the research has nothing more to say, and no report
		}
		return model(msgs, tools)
	}
	c3.saveGraph(append(append([]workflow.Node{}, events...), c3.closingBlock("A")), [2]string{"ev1", "A"})
	steps3 := c3.run("ev1")
	if want := "## Found\n\nbody of the main finding\n\n## Late\n\nbody of the late note"; steps3["A"].Output != want {
		t.Fatalf("the notes report %q (%+v)", steps3["A"].Output, steps3["A"])
	}
	// With the closing step none, a run is as before: one session, the finish report, no writing panel.
	c2 := newContEnv(t, nil)
	c2.saveGraph(append(append([]workflow.Node{}, events...), c2.agentNode("A", "Plain", nil)), [2]string{"ev1", "A"})
	plain := c2.agentRun(c2.run("ev1"), "A")
	_, page := c2.b.get(fmt.Sprintf("/w/o/r/agent-runs/%d", plain.ID))
	if plain.WriterConversation != 0 || len(c2.llm.requests()) != 1 || strings.Contains(page, "Writing the report") || !strings.Contains(page, "Report 1.") {
		t.Fatalf("writer %d, %d requests", plain.WriterConversation, len(c2.llm.requests()))
	}
	// Its final report is in the scratchpad all the same.
	if pad := c2.pad(plain.Thread); len(pad) != 1 || !pad[0].Report || pad[0].Body != "Report 1." {
		t.Fatalf("%+v", pad)
	}
}
