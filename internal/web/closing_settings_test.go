package web

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// The agent form has a Closing step: a preset's own, read-only (shown, not sent); Custom's to choose,
// with the report's turn limit beside the turn limit only when the closing step is the report.
func TestClosingStepInTheAgentForm(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	p, _ := e.st.CreateProvider(store.ProviderEdit{Name: "LLM", Kind: store.KindOpenAI, Endpoint: "https://llm.example/v1", Key: "k"}, derek)
	e.st.SetProviderModels(p.ID, []string{"m"})
	e.st.LendProvider(p, []int64{ws.ID}, derek)
	g := workflow.Graph{Nodes: []workflow.Node{{ID: "ag", Type: workflow.Agent}}, Links: []workflow.Link{}}
	b, _ := json.Marshal(g)
	e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(b), derek)
	br := e.signIn("derek")
	save := func(persona, closing, turns string) (int, workflow.AgentSettings) {
		t.Helper()
		w, _ := e.st.Workflow(ws.ID, workflow.PR)
		code, _, _ := br.post("/w/o/r/workflows/pr/nodes/ag", url.Values{"csrf": {br.csrf()}, "version": {itoa(int64(w.Version))},
			"name": {"Facts"}, "model": {itoa(p.ID) + ":m"}, "persona": {persona}, "instruction": {"my own"}, "goal": {"my goal"},
			"closing": {closing}, "report_turns": {turns}, "max_turns": {"40"}, "max_minutes": {"20"}, "rerun": {"resume"}})
		w, _ = e.st.Workflow(ws.ID, workflow.PR)
		var got workflow.Graph
		json.Unmarshal([]byte(w.Graph), &got)
		a, _ := e.ui.agentSettings(got.Nodes[0], workflow.PR)
		return code, a
	}
	form := func() string {
		_, page := br.get("/w/o/r/workflows/pr/nodes/ag")
		return page
	}

	// A new block is on Gather Information: the report, read-only, with its 15 turns shown.
	page := form()
	if !strings.Contains(page, `<select name="closing" data-closing disabled>`) || !strings.Contains(page, `<option value="report" selected>Write the report from the scratchpad</option>`) ||
		!strings.Contains(page, "None: finish delivers the output") || !strings.Contains(page, `<label data-when-report>Turns to write the report <input type="number" name="report_turns" min="1" max="200" value="15">`) {
		t.Fatalf("the preset's form:\n%s", page)
	}
	// A preset keeps no closing step of its own, whatever the form sends: it follows the preset.
	if code, a := save("code_quality", "report", ""); code != 200 || a.Closing != "" || a.EffectiveClosing(workflow.PR) != "" || a.ReportTurns != 0 {
		t.Fatalf("a preset without a report: %d %+v", code, a)
	}
	if page := form(); strings.Contains(page, "disabled>") && !strings.Contains(page, `<option value="" selected>None: finish delivers the output</option>`) || !strings.Contains(page, "data-when-report hidden") {
		t.Fatalf("the form of a preset without a report:\n%s", page)
	}
	if code, a := save("gather_information", "", ""); code != 200 || a.Closing != "" || a.EffectiveClosing(workflow.PR) != workflow.ClosingReport {
		t.Fatalf("Gather Information: %d %+v", code, a)
	}
	// Custom chooses its own, and its report turns.
	code, a := save("custom", "report", "9")
	if code != 200 || a.Closing != workflow.ClosingReport || a.ReportTurns != 9 || a.EffectiveReportTurns() != 9 {
		t.Fatalf("custom: %d %+v", code, a)
	}
	if page := form(); strings.Contains(page, `data-closing disabled`) || !strings.Contains(page, `<option value="report" selected>`) || !strings.Contains(page, `name="report_turns" min="1" max="200" value="9"`) {
		t.Fatalf("the custom form:\n%s", page)
	}
	if code, _ := save("custom", "poem", ""); code != 400 {
		t.Fatalf("a made-up closing step: %d", code)
	}
	if code, _ := save("custom", "report", "500"); code != 400 {
		t.Fatalf("too many report turns: %d", code)
	}
}
