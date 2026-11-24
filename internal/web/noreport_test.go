package web

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// An Agent step whose agent ended without a report fails, so the steps after it do not run on an
// empty report (Derek, 2026-11-24: a Decision chose "yes" from "(The agent ended without a report…)").
func TestAgentWithoutReportFailsItsStep(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = func(msgs []agent.Message, tools []string) agent.Message { // never finishes, never says a word
		tc := agent.ToolCall{ID: "x", Type: "function"}
		tc.Function.Name, tc.Function.Arguments = "list_files", `{}`
		return agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{tc}}
	}
	entry, _ := json.Marshal(workflow.CaseEntrySettings{Entry: "From the reviewer: {reviewer}", Finalize: true})
	c.saveGraph([]workflow.Node{events[0], c.agentNode("A", "Reviewer", nil), {ID: "E", Type: workflow.CaseEntry, Name: "Entry", Settings: entry}},
		[2]string{"ev1", "A"}, [2]string{"A", "E"})
	steps := c.run("ev1")
	if a := steps["A"]; a.Status != "failed" || !strings.Contains(a.Error, "the agent ended without a report") {
		t.Fatalf("agent step %s: %q", a.Status, a.Error)
	}
	if e := steps["E"]; e.Status != "skipped" {
		t.Fatalf("the step after it %s: %q", e.Status, e.Output)
	}
}
