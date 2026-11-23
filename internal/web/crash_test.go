package web

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// A crash in a run keeps what the run had: the transcript as last saved with the written report if it
// had one, else the research's report, else the notes, and the report is still kept in the thread's
// scratchpad, so the next run of the thread opens with it.
func TestCrashKeepsTheLastGoodTranscript(t *testing.T) {
	for name, tc := range map[string]struct {
		crashAt func(*agent.Transcript) bool
		report  string
		status  string
		turns   int
		writing bool
	}{
		"after the report was written": {func(tr *agent.Transcript) bool { return tr.Writing != nil && tr.Writing.Outcome == agent.Met },
			"WRITTEN REPORT: the findings, high ones first.", "met", 3, true},
		"after the research finished": {func(tr *agent.Transcript) bool {
			return tr.Outcome == agent.Met && tr.Writing == nil && tr.Report == "RESEARCH REPORT" && tr.Turns == 2
		}, "RESEARCH REPORT", "met", 2, false},
		"with only notes": {func(tr *agent.Transcript) bool { return tr.Outcome == "" && tr.Turns == 1 },
			"## Found\n\nbody of the main finding", "failed", 1, false},
	} {
		t.Run(name, func(t *testing.T) {
			c := newContEnv(t, nil)
			c.llm.reply = closingModel(false)
			b := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) { s.ContinueOf = "A" })
			c.saveGraph(append(append([]workflow.Node{}, events...), c.closingBlock("A"), b), [2]string{"ev1", "A"}, [2]string{"ev2", "B"})
			crashed := false
			saveHook = func(tr *agent.Transcript) {
				if !crashed && tc.crashAt(tr) {
					crashed = true
					panic("a bug somewhere")
				}
			}
			t.Cleanup(func() { saveHook = nil })
			steps := c.run("ev1")
			run := c.agentRun(steps, "A")
			saveHook = nil
			var tr agent.Transcript
			json.Unmarshal([]byte(run.Summary), &tr)
			if !crashed || run.Status != tc.status || tr.Report != tc.report || !strings.Contains(tr.Error, "agent run") || !strings.Contains(tr.Error, "crashed: a bug somewhere") ||
				tr.Turns != tc.turns || (tr.Writing != nil) != tc.writing {
				t.Fatalf("crashed %v, status %s, summary %s", crashed, run.Status, run.Summary)
			}
			// The report is in the thread, once, as its newest report.
			pad := c.pad(run.Thread)
			reports := 0
			for _, e := range pad {
				if e.Report {
					reports++
					if e.Body != tc.report || e.Title != store.ReportTitle(run.ID) {
						t.Errorf("the report entry: %+v", e)
					}
				}
			}
			if reports != 1 {
				t.Fatalf("%d report entries: %+v", reports, pad)
			}
			// So a continuation has something to open with.
			c.llm.reply = closingModel(false)
			steps = c.run("ev2")
			if steps["B"].Error == "no earlier report" || len(c.llm.requests()) == 0 {
				t.Fatalf("the continuation had nothing to continue from: %+v", steps["B"])
			}
			reqs := c.llm.requests()
			if got := section(messagesOf(t, reqs[len(reqs)-1])[1].Content, "Your last report on this item:\n", "\n\nYour notes so far"); got != tc.report {
				t.Fatalf("it opened with %q", got)
			}
		})
	}
}
