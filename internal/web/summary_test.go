package web

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// finishOf is the description of the finish tool of a request and its system prompt.
func finishOf(t *testing.T, body string) (description, system string) {
	t.Helper()
	var req struct {
		Messages []agent.Message
		Tools    []struct{ Function struct{ Name, Description string } }
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	for _, tl := range req.Tools {
		if tl.Function.Name == "finish" {
			description = tl.Function.Description
		}
	}
	return description, req.Messages[0].Content
}

// With the closing step the research is asked for a short summary, in the finish tool and in the
// preamble; the writing session, and a block with no closing step, are asked for the full report.
func TestResearchFinishAsksForASummaryOnlyWithAClosingStep(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = closingModel(false)
	c.saveGraph(append(append([]workflow.Node{}, events...), c.closingBlock("A")), [2]string{"ev1", "A"})
	c.run("ev1")
	var sawResearch, sawWriter bool
	for _, body := range c.llm.requests() {
		d, sys := finishOf(t, body)
		if strings.Contains(messagesOf(t, body)[1].Content, "Your research is done") {
			sawWriter = true
			if !strings.Contains(d, "The report is your final answer") || !strings.Contains(sys, reportSentence) || strings.Contains(sys, "short summary") {
				t.Fatalf("the writing session: %s\n%s", d, sys)
			}
			continue
		}
		sawResearch = true
		if !strings.Contains(d, "only a short summary") || !strings.Contains(sys, summarySentence) || strings.Contains(sys, reportSentence) {
			t.Fatalf("the research session:\n%s\n%s", d, sys)
		}
	}
	if !sawResearch || !sawWriter {
		t.Fatal("a session did not run")
	}
	c2 := newContEnv(t, nil)
	c2.saveGraph(append(append([]workflow.Node{}, events...), c2.agentNode("A", "Plain", nil)), [2]string{"ev1", "A"})
	c2.run("ev1")
	if d, sys := finishOf(t, c2.llm.requests()[0]); !strings.Contains(d, "The report is your final answer") || !strings.Contains(sys, reportSentence) {
		t.Fatalf("a block with no closing step:\n%s\n%s", d, sys)
	}
	if !strings.Contains(preambleText("o/r", true), summarySentence) || strings.Contains(preambleText("o/r", false), "short summary") {
		t.Fatal("preambleText")
	}
}
