package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// autoCase is a workspace with an agent provider (its model writes notes, a summary and a report) and a
// decision provider (which answers "yes", 80 percent sure), a public address for the links, and a
// workflow of the nodes given.
type autoCase struct {
	*contEnv
	decider workflow.ModelRef
}

func newAutoCase(t *testing.T) *autoCase {
	c := newContEnv(t, nil)
	c.llm.reply = outputsModel(`{"status":"met","report":"RESEARCH SHORT"}`)
	c.st.SetSetting(publicAddressKey, "https://autobot.example.com")
	llm, _ := llmServer(t)
	dp, err := c.st.CreateProvider(store.ProviderEdit{Name: "Decider", Kind: store.KindOpenAI, Endpoint: llm.URL + "/v1", Key: "k"}, c.admin)
	if err != nil {
		t.Fatal(err)
	}
	c.st.SetProviderModels(dp.ID, []string{"d"})
	c.st.SetProviderRoutes(dp.ID, true, false)
	c.st.LendProvider(dp, []int64{c.ws.ID}, c.admin)
	return &autoCase{c, workflow.ModelRef{Provider: dp.ID, Model: "d"}}
}

// gather is an Agent block named Gather with a written report.
func (c *autoCase) gather(change func(*workflow.AgentSettings)) workflow.Node {
	return c.agentNode("ag", "Gather", func(s *workflow.AgentSettings) {
		s.Closing, s.ReportTurns, s.MaxTurns, s.SubAgents = workflow.ClosingReport, 4, 10, 1
		if change != nil {
			change(s)
		}
	})
}

// route is a Decision block named Route.
func (c *autoCase) route(change func(*workflow.DecisionSettings)) workflow.Node {
	d := workflow.DefaultDecision(workflow.PR)
	d.Model = c.decider
	if change != nil {
		change(&d)
	}
	return node("route", workflow.Decision, "Route", d)
}

// build saves the workflow: the event, the nodes, and a link from the event to each of them.
func (c *autoCase) build(nodes ...workflow.Node) {
	var links [][2]string
	for _, n := range nodes {
		links = append(links, [2]string{"ev1", n.ID})
	}
	c.saveGraph(append(append([]workflow.Node{}, events...), nodes...), links...)
}

// caseText is pull request 1's case file, "" when it has none.
func (c *autoCase) caseText() string {
	cs, err := c.st.CaseByNumber(c.ws.ID, 1)
	if err != nil {
		return ""
	}
	return cs.Contents
}

// linesWith are the lines of a page that have a text, for a short message.
func linesWith(page, text string) string {
	var out []string
	for _, l := range strings.Split(page, "\n") {
		if strings.Contains(l, text) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

const (
	agentEntryHead = "## Gather\n\nTHE SUMMARY\n\n"
	decisionEntry  = "Route: yes (80%): It should."
)

// A real run adds an Agent's summary with a link to its full report and one to its notes (published pages),
// and a Decision's one line, each as a finalized entry under the step's name; a second run adds a second
// entry of each, and nothing is replaced.
func TestFinishedStepsWriteToTheCaseFile(t *testing.T) {
	c := newAutoCase(t)
	c.build(c.gather(nil), c.route(nil))
	steps := c.run("ev1")
	if steps["ag"].Status != "done" || steps["route"].Status != "done" {
		t.Fatalf("%+v", steps)
	}
	text := c.caseText()
	links := pageLinkRE.FindAllStringSubmatch(text, -1)
	if len(links) != 2 || links[0][1] != "Full report" || links[1][1] != "Notes" {
		t.Fatalf("the links in the case file:\n%s", text)
	}
	if !strings.Contains(text, agentEntryHead+"[Full report](https://autobot.example.com/p/"+links[0][2]+".html) · [Notes](https://autobot.example.com/p/"+links[1][2]+".html)\n") {
		t.Errorf("the agent's entry:\n%s", text)
	}
	if strings.Count(text, decisionEntry+"\n") != 1 || strings.Contains(text, "THE REPORT\n\n[Full") {
		t.Errorf("the decision's line, or the report in the entry:\n%s", text)
	}
	cs, _ := c.st.CaseByNumber(c.ws.ID, 1)
	pages, _ := c.st.PagesOf(cs.UUID)
	content := map[string]string{}
	for _, p := range pages {
		content[p.UUID] = p.Tag + "=" + p.Content
	}
	if content[links[0][2]] != "gather.report=THE REPORT" || content[links[1][2]] != "gather.scratchpad="+notesText {
		t.Errorf("the published pages: %v", content)
	}
	authors := map[string]int{}
	contribs, _ := c.st.Contributions(cs.UUID)
	for _, k := range contribs {
		if k.FinalizedAt.IsZero() || k.StartLine == 0 {
			t.Errorf("an entry that is not final: %+v", k)
		}
		authors[k.Author]++
	}
	if authors["Gather"] != 1 || authors["Route"] != 1 {
		t.Errorf("entries by author: %v", authors)
	}
	// A second run adds to the record.
	c.run("ev1")
	text = c.caseText()
	if strings.Count(text, "## Gather\n") != 2 || strings.Count(text, decisionEntry+"\n") != 2 {
		t.Errorf("after a second run:\n%s", text)
	}
	contribs, _ = c.st.Contributions(cs.UUID)
	if len(contribs) < 4 {
		t.Errorf("%d entries after two runs", len(contribs))
	}
}

// With the box unticked a step writes nothing, though its output is kept as always; each block decides for
// itself.
func TestUntickedStepsWriteNothing(t *testing.T) {
	c := newAutoCase(t)
	c.build(c.gather(func(s *workflow.AgentSettings) { s.NoCaseEntry = true }), c.route(func(d *workflow.DecisionSettings) { d.NoCaseEntry = true }))
	steps := c.run("ev1")
	if steps["ag"].Status != "done" || steps["route"].Status != "done" || c.caseValues()["gather.summary"] != "THE SUMMARY" {
		t.Fatalf("%+v", steps)
	}
	if text := c.caseText(); strings.Contains(text, "Gather") || strings.Contains(text, "Route") {
		t.Errorf("unticked steps wrote:\n%s", text)
	}
	// One ticked, one not.
	c.build(c.gather(nil), c.route(func(d *workflow.DecisionSettings) { d.NoCaseEntry = true }))
	c.run("ev1")
	if text := c.caseText(); !strings.Contains(text, agentEntryHead) || strings.Contains(text, "Route:") {
		t.Errorf("one ticked:\n%s", text)
	}
}

// A block saved before the box existed has no field for it, and counts as ticked; the box is shown ticked
// for it and unticked once the block says otherwise.
func TestOldBlocksWithoutTheFieldWrite(t *testing.T) {
	c := newAutoCase(t)
	a, d := c.gather(nil), c.route(nil)
	for _, n := range []*workflow.Node{&a, &d} {
		var fields map[string]any
		json.Unmarshal(n.Settings, &fields)
		delete(fields, "no_case_entry")
		n.Settings, _ = json.Marshal(fields)
		if strings.Contains(string(n.Settings), "case") {
			t.Fatalf("an old block: %s", n.Settings)
		}
	}
	c.build(a, d)
	_, form := c.b.get("/w/o/r/workflows/pr/nodes/ag")
	if !strings.Contains(form, `name="case_entry" checked>`) || !strings.Contains(form, "Write to the case file") || !strings.Contains(form, "Adds this step's result to the case file when it finishes.") {
		t.Errorf("the agent's box:\n%s", linesWith(form, "case"))
	}
	if _, form = c.b.get("/w/o/r/workflows/pr/nodes/route"); !strings.Contains(form, `name="case_entry" checked>`) {
		t.Errorf("the decision's box:\n%s", linesWith(form, "case"))
	}
	c.run("ev1")
	if text := c.caseText(); !strings.Contains(text, agentEntryHead) || !strings.Contains(text, decisionEntry) {
		t.Errorf("old blocks did not write:\n%s", text)
	}
	// Saving the form with the box unticked turns it off; ticked, on.
	save := func(id string, on bool, fields url.Values) {
		t.Helper()
		w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
		fields.Set("csrf", c.csrf)
		fields.Set("version", itoa(int64(w.Version)))
		if on {
			fields.Set("case_entry", "on")
		}
		if code, _, body := c.b.post("/w/o/r/workflows/pr/nodes/"+id, fields); code != 200 {
			t.Fatalf("save %s: %d %s", id, code, body)
		}
	}
	save("route", false, url.Values{"name": {"Route"}, "model": {modelValue(c.decider)}, "question": {"Go?"}, "choices": {"yes: it should\nno: it should not"}, "output": {"decision"}})
	save("ag", false, url.Values{"name": {"Gather"}, "persona": {"gather_information"}, "rerun": {"resume"}, "max_turns": {"10"}, "max_minutes": {"1"}, "model": {modelValue(workflow.ModelRef{Provider: c.prov.ID, Model: "m"})}})
	var gotA, gotD bool
	for _, n := range c.storedGraph().Nodes {
		switch n.ID {
		case "ag":
			s, _ := workflow.AgentSettingsOf(n, workflow.PR)
			gotA = s.NoCaseEntry
		case "route":
			s, _ := workflow.DecisionSettingsOf(n, workflow.PR)
			gotD = s.NoCaseEntry
		}
	}
	if !gotA || !gotD {
		t.Errorf("unticked boxes were not stored: agent %v, decision %v", gotA, gotD)
	}
	save("ag", true, url.Values{"name": {"Gather"}, "persona": {"gather_information"}, "rerun": {"resume"}, "max_turns": {"10"}, "max_minutes": {"1"}, "model": {modelValue(workflow.ModelRef{Provider: c.prov.ID, Model: "m"})}})
	for _, n := range c.storedGraph().Nodes {
		if s, _ := workflow.AgentSettingsOf(n, workflow.PR); n.ID == "ag" && s.NoCaseEntry {
			t.Error("a ticked box was stored as unticked")
		}
	}
}

// A dry run writes nothing, and opens no case file; a test of one block from the canvas writes nothing
// into the case file either.
func TestDryRunAndCanvasTestWriteNothing(t *testing.T) {
	c := newAutoCase(t)
	c.build(c.gather(nil), c.route(nil))
	id, err := c.ui.startWorkflow(t.Context(), c.ws, workflow.PR, "ev1", 1, true, c.admin)
	if err != nil {
		t.Fatal(err)
	}
	steps := c.finish(id)
	if steps["ag"].Status != "done" || steps["route"].Status != "done" {
		t.Fatalf("%+v", steps)
	}
	if _, err := c.st.CaseByNumber(c.ws.ID, 1); err == nil {
		t.Fatal("a dry run opened a case file")
	}
	// A real run for a case file to exist, then the canvas tests: the file stays as it is.
	c.run("ev1")
	before := c.caseText()
	if !strings.Contains(before, agentEntryHead) {
		t.Fatalf("the real run wrote nothing:\n%s", before)
	}
	base := "/w/o/r/workflows/pr/nodes/"
	link := "https://github.com/o/r/pull/1"
	if code, _, body := c.b.post(base+"route/test", url.Values{"csrf": {c.csrf}, "tag_pr": {link}}); code != 200 || !strings.Contains(body, "<strong>yes</strong>") {
		t.Fatalf("decision test %d:\n%s", code, body)
	}
	code, loc, _ := c.b.post(base+"ag/test", url.Values{"csrf": {c.csrf}, "tag_pr": {link}})
	if code != http.StatusSeeOther {
		t.Fatalf("agent test %d", code)
	}
	runID := int64(atoi(loc[strings.LastIndex(loc, "/")+1:]))
	for i := 0; i < 200; i++ {
		if r, err := c.st.AgentRun(runID); err == nil && r.Status != "running" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if r, _ := c.st.AgentRun(runID); r.Status == "running" {
		t.Fatal("the test agent did not end")
	}
	if after := c.caseText(); after != before {
		t.Errorf("a canvas test changed the case file:\n%s", after)
	}
}

// A step that failed, and one that was skipped, write nothing; a step that finished does.
func TestFailedAndSkippedStepsWriteNothing(t *testing.T) {
	c := newAutoCase(t)
	// The agent fails (it has no model), the first decision has no model, the second is on an event
	// that did not start the run; the third decides fine.
	bad := c.route(func(d *workflow.DecisionSettings) { d.Model = workflow.ModelRef{} })
	bad.ID, bad.Name = "bad", "No model"
	skipped := c.route(nil)
	skipped.ID, skipped.Name = "skipped", "Skipped"
	fine := c.route(nil)
	fine.ID, fine.Name = "fine", "Fine"
	c.saveGraph(append(append([]workflow.Node{}, events...), c.gather(func(s *workflow.AgentSettings) { s.Model = workflow.ModelRef{} }), bad, skipped, fine),
		[2]string{"ev1", "ag"}, [2]string{"ev1", "bad"}, [2]string{"ev2", "skipped"}, [2]string{"ev1", "fine"})
	steps := c.run("ev1")
	if steps["ag"].Status != "failed" || steps["bad"].Status != "failed" || steps["skipped"].Status != "skipped" || steps["fine"].Status != "done" {
		t.Fatalf("%+v", steps)
	}
	text := c.caseText()
	if strings.Contains(text, "Gather") || strings.Contains(text, "No model") || strings.Contains(text, "Skipped:") || !strings.Contains(text, "Fine: yes (80%)") {
		t.Errorf("the case file:\n%s", text)
	}
}

// A block that continues another's conversation writes under its shown name, "Gather (continued)", and the
// summary (its report, with no closing step) of this turn.
func TestReferenceWritesUnderItsShownName(t *testing.T) {
	c := newContEnv(t, nil)
	c.st.SetSetting(publicAddressKey, "https://autobot.example.com")
	a := c.agentNode("A", "Gather", nil)
	b := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) { s.ContinueOf = "A" })
	c.saveGraph(append(append([]workflow.Node{}, events...), a, b), [2]string{"ev1", "A"}, [2]string{"A", "B"})
	steps := c.run("ev1")
	if steps["A"].Status != "done" || steps["B"].Status != "done" {
		t.Fatalf("%+v", steps)
	}
	cs, _ := c.st.CaseByNumber(c.ws.ID, 1)
	if !strings.Contains(cs.Contents, "## Gather\n\nReport 1.\n\n[Full report](") || !strings.Contains(cs.Contents, "## Gather (continued)\n\nReport 2.\n\n[Full report](") {
		t.Errorf("the case file:\n%s", cs.Contents)
	}
	contribs, _ := c.st.Contributions(cs.UUID)
	var authors []string
	for _, k := range contribs {
		if k.Author != "Autobot" { // the one that opened the case file
			authors = append(authors, k.Author)
		}
	}
	if strings.Join(authors, ",") != "Gather,Gather (continued)" {
		t.Errorf("authors %v", authors)
	}
}

// An Agent's entry has its report where the summary is empty, and no Notes link when it kept no notes;
// a Decision's line has no reason after it when the choice has none.
func TestEntryWithoutASummaryOrNotes(t *testing.T) {
	c := newAutoCase(t)
	a, d := c.gather(nil), c.route(nil)
	g := workflow.Graph{Nodes: append(append([]workflow.Node{}, events...), a, d)}
	rs := &runState{ws: c.ws, run: &store.WorkflowRun{WorkspaceID: c.ws.ID, Kind: workflow.PR}, g: g, it: &item{kind: workflow.PR, number: 1, title: "Retry uploads"}, tags: map[string]string{}}
	rs.tags["gather.report"] = "ONLY A REPORT"
	c.ui.writeResult(rs, a, stepOutcome{port: "out", agentRun: 1, tags: map[string]string{".summary": " ", ".report": "ONLY A REPORT", ".scratchpad": ""}})
	c.ui.writeResult(rs, d, stepOutcome{port: "yes", tags: map[string]string{"": "yes", ".confidence": "80%"}})
	text := c.caseText()
	if !strings.Contains(text, "## Gather\n\nONLY A REPORT\n\n[Full report](https://autobot.example.com/p/") || strings.Contains(text, "Notes") || !strings.Contains(text, "\nRoute: yes (80%)\n") {
		t.Errorf("the case file:\n%s", text)
	}
}
