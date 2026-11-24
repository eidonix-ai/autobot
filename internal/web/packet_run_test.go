package web

import (
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// A Gather Information block run on a pull request opens its session with the head (link, title,
// description), then the orientation packet, then the instruction; the writing session's task has the
// angle paragraph. A block on another preset gets no packet, and so computes none.
func TestAgentRunGetsThePacket(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = closingModel(false)
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	c.st.SaveItem(c.ws.ID, store.Item{Number: 1, Kind: "pr", Title: "PR 1", State: "open", Author: "alice", Labels: []string{"bug"}, Created: at, Updated: at}, nil)
	c.st.SaveItem(c.ws.ID, store.Item{Number: 2, Kind: "issue", Title: "Alice's other issue", State: "open", Author: "alice", Created: at, Updated: at}, nil)
	c.st.IndexSet(c.ws.ID, "backfill_phase", "done")

	gather := c.agentNode("G", "Gatherer", func(s *workflow.AgentSettings) { s.Persona, s.Instruction, s.Goal = "gather_information", "", "" })
	other := c.agentNode("Q", "Quality", func(s *workflow.AgentSettings) { s.Persona, s.Instruction, s.Goal = "code_quality", "", "" })
	c.saveGraph(append(append([]workflow.Node{}, events...), gather, other), [2]string{"ev1", "G"}, [2]string{"ev2", "Q"})
	c.run("ev1")
	c.run("ev2")

	var first, writer, quality string
	for _, body := range c.llm.requests() {
		msgs := messagesOf(t, body)
		switch task := msgs[1].Content; {
		case strings.Contains(task, "Your research is done"):
			writer = task
		case strings.Contains(task, "You are preparing the case file"):
			if first == "" {
				first = task
			}
		case strings.Contains(task, "Review the change for correctness"):
			quality = task
		}
	}
	head := strings.Index(first, "Pull request: https://github.com/o/r/pull/1\nTitle: PR 1\n\n")
	packet := strings.Index(first, "## Orientation packet (computed by Autobot: a map of where to look, not conclusions)\n\n### The pull request\n- State: open, ready for review")
	instruction := strings.Index(first, "You are preparing the case file for this pull request")
	if head != 0 || packet < head || instruction < packet {
		t.Fatalf("the order is head %d, packet %d, instruction %d in:\n%.1500s", head, packet, instruction, first)
	}
	mustHave(t, "the first message", first, "#2 (open, issue) Alice's other issue — found by: same author", "### The author\n- alice, GitHub's association",
		"Done when: Every angle has been looked into", "read_charter")
	if strings.Contains(first, "{pr:packet}") {
		t.Errorf("the tag was left unfilled")
	}
	mustHave(t, "the writing session's task", writer, "You are preparing the case file", "organize the body with one section per angle in the order above", "follow any organization or grouping specified in the original task above")
	if quality == "" || strings.Contains(quality, "Orientation packet") {
		t.Errorf("another preset's message: %.300s", quality)
	}
}
