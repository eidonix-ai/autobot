package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// The Tags list shows an Agent's output tag with its parts indented under it, a Decision's alone,
// and {input} with its parts; the old {previous_output} is not listed. The Test box asks, as long
// text, for each part a template uses and for nothing else; the form refuses a misspelt part.
func TestTagsListAndTestBoxShowTheOutputParts(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	a := workflow.DefaultAgent(workflow.PR)
	a.OutputTo = "gather"
	d := workflow.DefaultDecision(workflow.PR)
	d.OutputTo = "verdict"
	ce := workflow.CaseEntrySettings{Entry: "{gather} {gather.report} {verdict} {input.summary} {previous_output}", Finalize: true}
	g := workflow.Graph{Nodes: []workflow.Node{
		{ID: "ag", Type: workflow.Agent, Name: "Gather", Settings: raw(a)},
		{ID: "de", Type: workflow.Decision, Name: "Verdict", Settings: raw(d)},
		{ID: "ce", Type: workflow.CaseEntry, Settings: raw(ce)},
	}, Links: []workflow.Link{{From: "de", Out: "yes", To: "ag", In: "in"}, {From: "ag", Out: "out", To: "ce", In: "in"}}}
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(raw(g)), derek); err != nil {
		t.Fatal(err)
	}
	b := e.signIn("derek")
	tok := b.csrf()
	base := "/w/o/r/workflows/pr/nodes/"

	code, body := b.get(base + "ce")
	if code != 200 {
		t.Fatalf("%d", code)
	}
	for _, want := range []string{
		`<dt><code>{gather}</code></dt><dd>its summary, report and notes</dd>`,
		`<dt class="field"><code>{gather.summary}</code></dt><dd>the summary</dd>`,
		`<dt class="field"><code>{gather.report}</code></dt><dd>the full report</dd>`,
		`<dt class="field"><code>{gather.scratchpad}</code></dt><dd>its notes in full</dd>`,
		`<dt><code>{verdict}</code></dt><dd>the choice</dd>`,
		`<dt class="field"><code>{verdict.confidence}</code></dt><dd>the chosen answer&#39;s probability, as a percentage</dd>`,
		`<dt class="field"><code>{verdict.reason}</code></dt><dd>the description given to the chosen choice</dd>`,
		`<dt><code>{case}</code></dt><dd><span class="auto" title="Filled in by Autobot; it cannot be set">auto</span> the item&#39;s case file so far</dd>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the Tags list lacks %s", want)
		}
	}
	// {input} and its parts are listed, with what they hold; the old {previous_output} is not (templates that use it still work).
	for _, want := range []string{`<dt><code>{input}</code></dt><dd>everything linked into this block, each under its block&#39;s name, once all of them have finished (failed or skipped ones are named too)</dd>`,
		`<dt class="field"><code>{input.summary}</code></dt>`, `<dt class="field"><code>{input.report}</code></dt>`, `<dt class="field"><code>{input.scratchpad}</code></dt>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the Tags list lacks %s", want)
		}
	}
	for _, not := range []string{"{verdict.report}", "{previous_output}</code></dt>", `{gather.summary}</code></dt><dd><span class="auto"`} {
		if strings.Contains(body, not) {
			t.Errorf("the Tags list has %s", not)
		}
	}
	// The Agent block's own output is not offered to it.
	if _, body := b.get(base + "ag"); strings.Contains(body, "<dt><code>{gather}</code>") || !strings.Contains(body, "<dt><code>{verdict}</code>") {
		t.Error("an agent is offered its own output")
	}

	// The Test box asks for each part the templates use, in a text box, and the old name as {input.report}.
	_, form := b.get(base + "ce/test")
	for _, want := range []string{`<textarea name="tag_gather"`, `<textarea name="tag_gather.report"`, `<textarea name="tag_verdict"`, `<textarea name="tag_input.summary"`, `<textarea name="tag_input.report"`} {
		if !strings.Contains(form, want) {
			t.Errorf("the Test box lacks %s:\n%s", want, form)
		}
	}
	for _, not := range []string{"tag_gather.summary", "tag_gather.scratchpad", "tag_input.scratchpad", "tag_previous_output"} {
		if strings.Contains(form, not) {
			t.Errorf("the Test box asks for %s", not)
		}
	}
	code, _, out := b.post(base+"ce/test", url.Values{"csrf": {tok}, "tag_gather": {"WHOLE"}, "tag_gather.report": {"THE REPORT"}, "tag_verdict": {"yes"}, "tag_input.summary": {"SUM"}, "tag_input.report": {"REP"}})
	if code != 200 || !strings.Contains(out, "WHOLE THE REPORT yes SUM REP") {
		t.Fatalf("the test of a case entry %d:\n%s", code, out)
	}

	// A misspelt part, a colon where a dot belongs and a dot on a decision's output are refused on save.
	for tpl, want := range map[string]string{
		"{gather.notes}":      "parts are summary, report and scratchpad",
		"{gather:report}":     "use a dot, such as {gather.report}",
		"{verdict.report}":    "parts are confidence and reason",
		"{gather.confidence}": "parts are summary, report and scratchpad",
		"{pr.title}":          "use a colon",
	} {
		code, _, out := b.post(base+"ce", url.Values{"csrf": {tok}, "name": {"Case entry"}, "entry": {tpl}, "finalize": {"on"}, "version": {"0"}})
		if code != http.StatusBadRequest || !strings.Contains(out, want) {
			t.Errorf("%s: %d, want it to say %q:\n%s", tpl, code, want, out)
		}
	}
}

// The run's page has a Summary panel above the Report panel, inside the part that updates live; a
// block with no closing step, whose summary is its report, has none.
func TestRunPageShowsTheSummaryPanel(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = closingModel(false)
	c.saveGraph(append(append([]workflow.Node{}, events...), c.closingBlock("A")), [2]string{"ev1", "A"})
	run := c.agentRun(c.run("ev1"), "A")
	_, page := c.b.get(fmt.Sprintf("/w/o/r/agent-runs/%d", run.ID))
	head, summary, report := strings.Index(page, `id="run-head" data-live`), strings.Index(page, `<h2>Summary</h2><div class="md"><p>WRITTEN SUMMARY: the key findings.</p>`), strings.Index(page, `<h2>Report</h2>`)
	if head < 0 || summary < head || report < summary || !strings.Contains(page, "WRITTEN REPORT: the findings") {
		t.Fatalf("the Summary panel is at %d, the live part at %d and the Report at %d:\n%s", summary, head, report, page)
	}
	if !strings.Contains(page[report:], `<p>WRITTEN REPORT: the findings, high ones first.</p>`) || strings.Contains(page[summary:report], "WRITTEN REPORT") {
		t.Error("the panels hold each other's text")
	}
	// The summary of a run still going is shown as it is saved.
	r2, _, _, _ := c.st.StartAgentRun(store.AgentRun{WorkspaceID: c.ws.ID, Kind: "pr", NodeID: "x", NodeName: "Live", Item: 1, Model: "m"}, c.admin)
	c.st.SaveAgentRun(r2, "running", `{"turns":2,"summary":"SUMMARY SO FAR","report":"REPORT SO FAR"}`, 0)
	if _, page := c.b.get(fmt.Sprintf("/w/o/r/agent-runs/%d", r2)); !strings.Contains(page, "<h2>Summary</h2>") || !strings.Contains(page, "SUMMARY SO FAR") {
		t.Error("a running run has no summary panel")
	}
	// With no closing step the report is the summary: one panel.
	c2 := newContEnv(t, nil)
	c2.saveGraph(append(append([]workflow.Node{}, events...), c2.agentNode("A", "Plain", nil)), [2]string{"ev1", "A"})
	plain := c2.agentRun(c2.run("ev1"), "A")
	_, page = c2.b.get(fmt.Sprintf("/w/o/r/agent-runs/%d", plain.ID))
	if strings.Contains(page, "<h2>Summary</h2>") || !strings.Contains(page, "<h2>Report</h2>") {
		t.Error("a run with one report has a Summary panel")
	}
}
