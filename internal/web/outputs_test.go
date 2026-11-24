package web

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// outputsModel plays a main agent that saves three notes (low, medium, high), hands one task to a
// sub-agent, which saves a note of its own, and then finishes with researchEnd; with a closing step
// the writing session finishes with a summary and a report.
func outputsModel(researchEnd string) func([]agent.Message, []string) agent.Message {
	return func(msgs []agent.Message, tools []string) agent.Message {
		assistants := 0
		for _, m := range msgs {
			if m.Role == "assistant" {
				assistants++
			}
		}
		note := func(title, imp string) agent.Message {
			return toolCall(title, "scratchpad_write", fmt.Sprintf(`{"title":%q,"importance":%q,"description":"d","body":"body of %s"}`, title, imp, title))
		}
		switch {
		case strings.HasPrefix(msgs[0].Content, "You are Autobot, a sub-agent"):
			if assistants == 0 {
				return note("SUBNOTE", "high")
			}
			return toolCall("f", "finish", `{"status":"met","report":"SUB REPORT"}`)
		case strings.Contains(msgs[1].Content, "Your research is done"):
			return toolCall("w", "finish", `{"status":"met","summary":"THE SUMMARY","report":"THE REPORT"}`)
		case strings.HasPrefix(msgs[len(msgs)-1].Content, "Before your report is written"):
			return agent.Message{Role: "assistant", Content: "nothing more"}
		}
		switch assistants {
		case 0:
			return note("Minor", "low")
		case 1:
			return note("Detail", "medium")
		case 2:
			return note("Core", "high")
		case 3:
			return toolCall("s", "spawn_agent", `{"title":"callers","task":"find the callers"}`)
		}
		return toolCall("r", "finish", researchEnd)
	}
}

func caseEntryNode(id, name, text string) workflow.Node {
	b, _ := json.Marshal(workflow.CaseEntrySettings{Entry: text, Finalize: true})
	return workflow.Node{ID: id, Type: workflow.CaseEntry, Name: name, Settings: b}
}

// caseValues are the output tags kept for pull request 1's case.
func (c *contEnv) caseValues() map[string]string {
	c.t.Helper()
	cs, err := c.st.CaseByNumber(c.ws.ID, 1)
	if err != nil {
		c.t.Fatalf("no case file: %v", err)
	}
	v, err := c.st.CaseValues(cs.UUID)
	if err != nil {
		c.t.Fatal(err)
	}
	return v
}

// notesText is the thread's notes as {gather.scratchpad} gives them in the tests of this file: the
// high ones first (the written one, then the research's summary Autobot saved), then medium, then low.
// A sub-agent's note is not among them, and neither is the final report.
const notesText = "#3 [high] Core\n\nbody of Core\n\n#4 [high] Research summary\n\nRESEARCH SHORT\n\n#2 [medium] Detail\n\nbody of Detail\n\n#1 [low] Minor\n\nbody of Minor"

// A block with a closing step and "Output to" gather keeps four values for the case: {gather} the
// summary, the report and the notes under headings; {gather.summary}, {gather.report} and
// {gather.scratchpad} the parts. The notes are the main agent's, high first, with no sub-agent's.
func TestOutputTagHoldsTheWholeAndItsParts(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = outputsModel(`{"status":"met","report":"RESEARCH SHORT"}`)
	a := c.agentNode("A", "Researcher", func(s *workflow.AgentSettings) {
		s.Closing, s.ReportTurns, s.MaxTurns, s.SubAgents, s.OutputTo = workflow.ClosingReport, 4, 10, 1, "gather"
	})
	c.saveGraph(append(append([]workflow.Node{}, events...), a), [2]string{"ev1", "A"})
	steps := c.run("ev1")
	run := c.agentRun(steps, "A")
	if steps["A"].Status != "done" || run.Status != "met" {
		t.Fatalf("A: %+v", steps["A"])
	}
	v := c.caseValues()
	whole := "## Summary\n\nTHE SUMMARY\n\n## Report\n\nTHE REPORT\n\n## Notes\n\n" + notesText
	for tag, want := range map[string]string{"gather": whole, "gather.summary": "THE SUMMARY", "gather.report": "THE REPORT", "gather.scratchpad": notesText} {
		if v[tag] != want {
			t.Errorf("{%s} = %q\nwant %q", tag, v[tag], want)
		}
	}
	if strings.Contains(v["gather"], "SUBNOTE") || strings.Contains(v["gather.scratchpad"], "SUB REPORT") {
		t.Error("a sub-agent's note is in the notes")
	}
	if strings.Count(v["gather.scratchpad"], "[high]") != 2 || strings.Contains(v["gather.scratchpad"], "Report of") {
		t.Errorf("the notes: %q", v["gather.scratchpad"])
	}
	// The step's own output stays the report.
	if steps["A"].Output != "THE REPORT" {
		t.Errorf("the step's output %q", steps["A"].Output)
	}
}

// With no closing step the block has one report, which is its summary too: {gather} is the report
// with the notes after it, if there are any; {gather.summary} and {gather.report} are the report.
// Every value is replaced by the next write, none left from the last.
func TestOutputTagWithoutAClosingStep(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = outputsModel(`{"status":"met","report":"NONE REPORT"}`)
	a := c.agentNode("A", "Researcher", func(s *workflow.AgentSettings) { s.MaxTurns, s.SubAgents, s.OutputTo = 10, 1, "gather" })
	c.saveGraph(append(append([]workflow.Node{}, events...), a), [2]string{"ev1", "A"})
	c.run("ev1")
	notes := "#3 [high] Core\n\nbody of Core\n\n#2 [medium] Detail\n\nbody of Detail\n\n#1 [low] Minor\n\nbody of Minor"
	v := c.caseValues()
	for tag, want := range map[string]string{"gather": "NONE REPORT\n\n## Notes\n\n" + notes, "gather.summary": "NONE REPORT", "gather.report": "NONE REPORT", "gather.scratchpad": notes} {
		if v[tag] != want {
			t.Errorf("{%s} = %q\nwant %q", tag, v[tag], want)
		}
	}
	// A run of a block that took no notes: the report alone, and no notes.
	c.llm.reply = nil
	b := c.agentNode("B", "Researcher", func(s *workflow.AgentSettings) { s.OutputTo = "gather" }) // another block: a thread of its own, with no notes
	c.saveGraph(append(append([]workflow.Node{}, events...), b), [2]string{"ev1", "B"})
	run := c.agentRun(c.run("ev1"), "B")
	v = c.caseValues()
	if !strings.HasPrefix(v["gather"], "Report ") || strings.Contains(v["gather"], "\n") || v["gather.report"] != v["gather"] || v["gather.summary"] != v["gather"] || v["gather.scratchpad"] != "" {
		t.Errorf("the next write (run %d) left %v", run.ID, v)
	}
	if strings.Contains(v["gather"], "NONE REPORT") || strings.Contains(v["gather"], "Notes") {
		t.Errorf("a value of the earlier write is left: %v", v)
	}
	// And from a closing step to none, and back: the summary and the report each follow.
	c.llm.reply = outputsModel(`{"status":"met","report":"RESEARCH SHORT"}`)
	a = c.agentNode("A", "Researcher", func(s *workflow.AgentSettings) {
		s.Closing, s.ReportTurns, s.MaxTurns, s.SubAgents, s.OutputTo = workflow.ClosingReport, 4, 10, 1, "gather"
	})
	c.saveGraph(append(append([]workflow.Node{}, events...), a), [2]string{"ev1", "A"})
	c.run("ev1")
	if v = c.caseValues(); v["gather.summary"] != "THE SUMMARY" || v["gather.report"] != "THE REPORT" || !strings.HasPrefix(v["gather"], "## Summary\n\nTHE SUMMARY") {
		t.Errorf("after the next write: %v", v)
	}
}

// What follows an agent: {input} is its summary, report and notes, and each part; {previous_output}
// is its report, as it always was. After a step that is not an agent {input} is its output, as the
// summary and the report, and has no notes. Where several steps link in, each is under its name.
func TestInputAfterAnAgentAnAnotherStepAndAMerge(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = outputsModel(`{"status":"met","report":"RESEARCH SHORT"}`)
	a := c.agentNode("A", "Researcher", func(s *workflow.AgentSettings) {
		s.Closing, s.ReportTurns, s.MaxTurns, s.SubAgents = workflow.ClosingReport, 4, 10, 1
	})
	nodes := append(append([]workflow.Node{}, events...), a,
		caseEntryNode("whole", "Whole", "{input}"),
		caseEntryNode("parts", "Parts", "S=[{input.summary}] R=[{input.report}] N=[{input.scratchpad}] P=[{previous_output}]"),
		caseEntryNode("side", "Side", "SIDE TEXT"),
		caseEntryNode("after", "After", "{input} | {input.summary} | {input.report} | [{input.scratchpad}] | {previous_output}"),
		caseEntryNode("merge", "Merge", "{input.report}"),
		caseEntryNode("mergeall", "Merge all", "{input}"),
		caseEntryNode("mergenotes", "Merge notes", "{input.scratchpad}|{input.summary}"))
	c.saveGraph(nodes, [2]string{"ev1", "A"}, [2]string{"A", "whole"}, [2]string{"A", "parts"}, [2]string{"ev1", "side"}, [2]string{"side", "after"},
		[2]string{"A", "merge"}, [2]string{"side", "merge"}, [2]string{"A", "mergeall"}, [2]string{"side", "mergeall"}, [2]string{"A", "mergenotes"}, [2]string{"side", "mergenotes"})
	steps := c.run("ev1")
	for id, s := range steps {
		if s.Status == "failed" {
			t.Fatalf("%s failed: %+v", id, s)
		}
	}
	bundle := "## Summary\n\nTHE SUMMARY\n\n## Report\n\nTHE REPORT\n\n## Notes\n\n" + notesText
	want := map[string]string{
		// After the agent.
		"whole": bundle,
		"parts": "S=[THE SUMMARY] R=[THE REPORT] N=[" + notesText + "] P=[THE REPORT]",
		// After a step that is not an agent: its output, twice, and no notes.
		"after": "SIDE TEXT | SIDE TEXT | SIDE TEXT | [] | SIDE TEXT",
		// Several in: each under its name.
		"merge":      "## Researcher\n\nTHE REPORT\n\n## Side\n\nSIDE TEXT",
		"mergeall":   "## Researcher\n\n" + bundle + "\n\n## Side\n\nSIDE TEXT",
		"mergenotes": "## Researcher\n\n" + notesText + "\n\n## Side\n\n|## Researcher\n\nTHE SUMMARY\n\n## Side\n\nSIDE TEXT",
	}
	for id, w := range want {
		if steps[id].Status != "done" || steps[id].Output != w {
			t.Errorf("%s: %s\n got %q\nwant %q", id, steps[id].Status, steps[id].Output, w)
		}
	}
	// The next step after the agent, with only {previous_output}: the report, with no notes in it.
	if strings.Contains(steps["parts"].Output, "SUBNOTE") {
		t.Error("a sub-agent's note got in")
	}
}

// An agent's parts for the next step are rebuilt from its run, so {input} survives a restart of
// Autobot between the two steps: the step after a finished agent still has them.
func TestInputPartsComeFromTheAgentRun(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = outputsModel(`{"status":"met","report":"RESEARCH SHORT"}`)
	a := c.agentNode("A", "Researcher", func(s *workflow.AgentSettings) {
		s.Closing, s.ReportTurns, s.MaxTurns, s.SubAgents = workflow.ClosingReport, 4, 10, 1
	})
	c.saveGraph(append(append([]workflow.Node{}, events...), a), [2]string{"ev1", "A"})
	steps := c.run("ev1")
	rs := &runState{run: &store.WorkflowRun{Kind: workflow.PR}}
	got := c.ui.stepParts(rs, source{a, steps["A"]})
	if got[".summary"] != "THE SUMMARY" || got[".report"] != "THE REPORT" || got[".scratchpad"] != notesText || !strings.HasPrefix(got[""], "## Summary\n\nTHE SUMMARY") {
		t.Fatalf("%q", got)
	}
}

// A continuation opens with the last summary and says where the full report is; the full report is
// not in the opening.
func TestContinuationOpensWithTheSummaryNotTheReport(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = closingModel(false)
	b := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) { s.ContinueOf = "A" })
	c.saveGraph(append(append([]workflow.Node{}, events...), c.closingBlock("A"), b), [2]string{"ev1", "A"}, [2]string{"ev2", "B"})
	run := c.agentRun(c.run("ev1"), "A")
	pad := c.pad(run.Thread) // as the continuation finds it
	c.run("ev2")
	reqs := c.llm.requests()
	opening := messagesOf(t, reqs[len(reqs)-1])[1].Content
	n := pad[len(pad)-1].N // the final report's entry
	if !pad[len(pad)-1].Report {
		t.Fatalf("the report is not the last entry: %+v", pad)
	}
	for _, want := range []string{"Your last summary on this item:\nWRITTEN SUMMARY: the key findings.", fmt.Sprintf("The full report is entry #%d of your notes: read it with scratchpad_read.", n),
		fmt.Sprintf("#%d [high] Report of ", n)} {
		if !strings.Contains(opening, want) {
			t.Errorf("the opening lacks %q:\n%s", want, opening)
		}
	}
	if strings.Contains(opening, "WRITTEN REPORT") || strings.Contains(opening, "Your last report") {
		t.Errorf("the opening has the full report:\n%s", opening)
	}
	// The report entry holds the full report, with the summary cut to one line as its description.
	rep := pad[len(pad)-1]
	if rep.Body != "WRITTEN REPORT: the findings, high ones first." || rep.Summary != "WRITTEN SUMMARY: the key findings." || rep.Description != rep.Summary {
		t.Errorf("the report entry: %+v", rep)
	}
}

// A report kept before summaries existed (no summary of its own) opens the next run with one made
// from it, marked as made.
func TestContinuationFromAReportWithoutASummary(t *testing.T) {
	c := newContEnv(t, nil)
	b := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) { s.ContinueOf = "A" })
	c.saveGraph(append(append([]workflow.Node{}, events...), c.agentNode("A", "Researcher", nil), b), [2]string{"ev1", "A"}, [2]string{"ev2", "B"})
	run := c.agentRun(c.run("ev1"), "A")
	// The entry as an older version kept it: no summary, a long report.
	old := "## Key findings\n\n- the old finding\n\n## Detail\n\n" + strings.Repeat("old detail ", 100)
	if _, err := c.st.Pad(run.Thread).Add(store.PadEntry{Author: "main", Title: "Report of long ago", Importance: store.High, Body: old, Report: true}); err != nil {
		t.Fatal(err)
	}
	c.run("ev2")
	reqs := c.llm.requests()
	opening := messagesOf(t, reqs[len(reqs)-1])[1].Content
	got := section(opening, "Your last summary on this item:\n", "\n\nThe full report is entry")
	if !strings.Contains(got, "the old finding") || strings.Contains(got, "old detail") || !strings.Contains(got, "No summary was written") {
		t.Fatalf("it opened with %q", got)
	}
}

// Every way a run can end gives it a summary, in its row and kept with its report: a writer that
// wrote none (the research's own report stands), a crash, and the notes report.
func TestEveryFallbackRunHasASummary(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = closingModel(true) // the writing session says nothing
	c.saveGraph(append(append([]workflow.Node{}, events...), c.closingBlock("A")), [2]string{"ev1", "A"})
	run := c.agentRun(c.run("ev1"), "A")
	var tr agent.Transcript
	json.Unmarshal([]byte(run.Summary), &tr)
	if tr.Report != "RESEARCH REPORT" || tr.Summary != "RESEARCH REPORT" {
		t.Fatalf("the writer failed: %s", run.Summary)
	}
	if pad := c.pad(run.Thread); !slices.ContainsFunc(pad, func(e store.PadEntry) bool { return e.Report && e.Summary == "RESEARCH REPORT" }) {
		t.Fatalf("the kept report has no summary: %+v", pad)
	}
	// With nothing but notes.
	c3 := newContEnv(t, nil)
	model := closingModel(true)
	c3.llm.reply = func(msgs []agent.Message, tools []string) agent.Message {
		if !strings.Contains(msgs[1].Content, "Your research is done") && slices.ContainsFunc(msgs, func(m agent.Message) bool { return m.Role == "assistant" }) &&
			!strings.HasPrefix(msgs[len(msgs)-1].Content, "Before your report is written") {
			return agent.Message{Role: "assistant"}
		}
		return model(msgs, tools)
	}
	c3.saveGraph(append(append([]workflow.Node{}, events...), c3.closingBlock("A")), [2]string{"ev1", "A"})
	run = c3.agentRun(c3.run("ev1"), "A")
	json.Unmarshal([]byte(run.Summary), &tr)
	want := "## Found\n\nbody of the main finding\n\n## Late\n\nbody of the late note"
	if tr.Report != want || tr.Summary != want {
		t.Fatalf("the notes report: %s", run.Summary)
	}
}

// The Condition block's "tag contains" rule on an output tag reads the whole of it: the summary,
// the report and the notes.
func TestConditionTagRuleReadsTheWholeOutput(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = outputsModel(`{"status":"met","report":"RESEARCH SHORT"}`)
	a := c.agentNode("A", "Researcher", func(s *workflow.AgentSettings) {
		s.Closing, s.ReportTurns, s.MaxTurns, s.SubAgents, s.OutputTo = workflow.ClosingReport, 4, 10, 1, "gather"
	})
	cond := func(id, has string) workflow.Node {
		b, _ := json.Marshal(workflow.ConditionSettings{Match: "all", Tag: "gather", TagHas: has})
		return workflow.Node{ID: id, Type: workflow.Condition, Settings: b}
	}
	nodes := append(append([]workflow.Node{}, events...), a, cond("heading", "## Summary"), cond("notes", "body of Core"), cond("absent", "SUBNOTE"))
	c.saveGraph(nodes, [2]string{"ev1", "A"}, [2]string{"A", "heading"}, [2]string{"A", "notes"}, [2]string{"A", "absent"})
	steps := c.run("ev1")
	if steps["heading"].Port != "yes" || steps["notes"].Port != "yes" || steps["absent"].Port != "no" {
		t.Fatalf("heading %+v\nnotes %+v\nabsent %+v", steps["heading"], steps["notes"], steps["absent"])
	}
}
