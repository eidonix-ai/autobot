package web

import (
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/workflow"
)

// The summary format of a block reaches its writing session: a Custom block's own, and a preset's, which
// the block follows; a block with none gets the default summary wording.
func TestSummaryFormatReachesTheWriter(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = closingModel(false)
	const own = "**Verdict:** <one word>\n**Autobot comments:** <\"None.\" if nothing>"
	withFormat := c.agentNode("F", "Formatted", func(s *workflow.AgentSettings) { s.Closing, s.SummaryFormat = workflow.ClosingReport, own })
	without := c.agentNode("N", "Plain", func(s *workflow.AgentSettings) { s.Closing = workflow.ClosingReport })
	c.saveGraph(append(append([]workflow.Node{}, events...), withFormat, without), [2]string{"ev1", "F"}, [2]string{"ev2", "N"})
	c.run("ev1")
	c.run("ev2")
	var formatted, plain string
	for _, body := range c.llm.requests() {
		msgs := messagesOf(t, body)
		if task := msgs[1].Content; strings.Contains(task, "Your research is done") {
			if strings.Contains(task, "This is F.") {
				formatted = task
			}
			if strings.Contains(task, "This is N.") {
				plain = task
			}
		}
	}
	if !strings.Contains(formatted, "The summary follows this format exactly, filling every field; keep the labels; write 'None.' where a field allows it and there is nothing:\n"+own+"\n") ||
		strings.Contains(formatted, "at most about 300 words") {
		t.Errorf("the writer of the block with a format:\n%s", formatted)
	}
	if !strings.Contains(plain, "The summary is at most about 300 words") || strings.Contains(plain, "follows this format") {
		t.Errorf("the writer of the block without:\n%s", plain)
	}
}

// A block on Gather Information gives its writer the seeded format, as the preset has it now: an admin's
// edit of the format reaches the next run.
func TestPresetSummaryFormatReachesTheWriter(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = closingModel(false)
	gather := c.agentNode("G", "Gatherer", func(s *workflow.AgentSettings) { s.Persona, s.Instruction, s.Goal = "gather_information", "", "" })
	c.saveGraph(append(append([]workflow.Node{}, events...), gather), [2]string{"ev1", "G"})
	writerTask := func() string {
		t.Helper()
		var task string
		for _, body := range c.llm.requests() {
			if msgs := messagesOf(t, body); strings.Contains(msgs[1].Content, "Your research is done") {
				task = msgs[1].Content
			}
		}
		return task
	}
	c.run("ev1")
	if task := writerTask(); !strings.Contains(task, "**Author:** <login (role in the project); Discord handle if the PR gives one>") ||
		!strings.Contains(task, "**Autobot comments:** <anything else highly relevant, explained simply: what the description doesn't mention") ||
		strings.Contains(task, "**Reporter:**") {
		t.Fatalf("the seeded pull request format:\n%s", task)
	}
	g, _ := c.st.PresetByKey("gather_information")
	g.PRSummary = "**Edited:** <x>"
	if err := c.st.UpdatePreset(g.ID, *g, c.admin); err != nil {
		t.Fatal(err)
	}
	c.run("ev1")
	if task := writerTask(); !strings.Contains(task, "**Edited:** <x>") || strings.Contains(task, "**Author:**") {
		t.Fatalf("after the edit:\n%s", task)
	}
}
