package web

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/workflow"
)

// refAgent is a reference block: an Agent block that continues the block of the id, with no name of its
// own, as the canvas places it.
func (c *contEnv) refAgent(id, of string) workflow.Node {
	return c.agentNode(id, "", func(s *workflow.AgentSettings) { s.ContinueOf = of })
}

// A reference block is the same agent continuing the same thread, so when it runs it writes its source's
// tag and parts, replacing the earlier ones, and a block after it reads this run's report.
func TestReferenceRunWritesTheSourcesTag(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveGraph(append(append([]workflow.Node{}, events...), c.agentNode("A", "Explore", nil), c.refAgent("R", "A"),
		caseEntryNode("af", "After", "SUMMARY={explore.summary} REPORT={explore.report}")),
		[2]string{"ev1", "A"}, [2]string{"ev2", "R"}, [2]string{"R", "af"})
	c.run("ev1")
	if v := c.caseValues(); v["explore.report"] != "Report 1." || v["explore"] == "" {
		t.Fatalf("after the source ran: %v", v)
	}
	runID := c.start("ev2")
	steps := c.finish(runID)
	if steps["R"].Status != "done" || steps["af"].Output != "SUMMARY=Report 2. REPORT=Report 2." {
		t.Fatalf("the block after the reference read %q (reference %+v)", steps["af"].Output, steps["R"])
	}
	if run := c.agentRun(steps, "R"); run.NodeName != "Explore (continued)" || run.NodeID != "R" {
		t.Errorf("the agent run of the reference is titled %q", run.NodeName)
	}
	v := c.caseValues()
	if v["explore.report"] != "Report 2." || v["explore.summary"] != "Report 2." || !strings.Contains(v["explore"], "Report 2.") {
		t.Errorf("the source's tag after the reference ran: %v", v)
	}
	for tag := range v {
		if !strings.HasPrefix(tag, "explore") && tag != "after" {
			t.Errorf("a tag of the reference's own was kept: %q", tag)
		}
	}
	// The run's page names the step as the canvas does.
	if code, page := c.b.get("/w/o/r/workflow-runs/" + itoa(runID)); code != 200 || !strings.Contains(page, "Explore (continued)") {
		t.Errorf("the run page %d does not name the reference:\n%.400s", code, page)
	}
}

// The canvas, the reference's dialog and the Tags list: the reference shows as "<source> (continued)",
// follows a rename of the source, has no name box, takes no default name, and is not listed as a tag.
func TestReferenceHasNoNameAndSharesTheTagInTheCanvas(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveGraph(append(append([]workflow.Node{}, events...), c.agentNode("aa", "Reviewer", nil), c.refAgent("rr", "aa"),
		caseEntryNode("xx", "Next", "{reviewer}"), caseEntryNode("af", "After", "{reviewer.summary}")),
		[2]string{"ev1", "aa"}, [2]string{"aa", "xx"}, [2]string{"ev2", "rr"}, [2]string{"rr", "af"})
	_, page := c.b.get("/w/o/r/workflows/pr")
	if !strings.Contains(page, `&#34;rr&#34;:&#34;Reviewer (continued)&#34;`) && !strings.Contains(page, `"rr":"Reviewer (continued)"`) {
		t.Errorf("the canvas does not name the reference:\n%.300s", page)
	}
	// Its dialog: the name read-only, the note, no name box, and its tag is the source's.
	_, form := c.b.get("/w/o/r/workflows/pr/nodes/rr")
	if !strings.Contains(form, `data-ref-name>Reviewer (continued)</strong>`) || !strings.Contains(form, "continues Reviewer") || strings.Contains(form, `name="name"`) {
		t.Errorf("the reference's dialog:\n%s", form)
	}
	if got := c.formTag("rr"); got != "reviewer" {
		t.Errorf("the reference's tag in its dialog: %q", got)
	}
	// Posting a name for it changes nothing: it has none.
	if code, body := c.postNode("rr", url.Values{"continue_of": {"aa"}, "name": {"Hijack"}}); code != 200 {
		t.Fatalf("saving the reference: %d %s", code, body)
	}
	for _, n := range c.storedGraph().Nodes {
		if n.ID == "rr" && (n.Name != "" || strings.Contains(string(n.Settings), "output_to")) {
			t.Errorf("the reference was given a name or a tag: %q %s", n.Name, n.Settings)
		}
	}
	// The Tags list of a block after it has the tag once, and none from other events.
	_, after := c.b.get("/w/o/r/workflows/pr/nodes/af")
	if n := strings.Count(after, "<dt><code>{reviewer}</code>"); n != 1 {
		t.Errorf("{reviewer} is listed %d times for the block after the reference:\n%s", n, after)
	}
	if i := strings.Index(after, "From other events"); i >= 0 && strings.Contains(after[i:], "{reviewer}") {
		t.Errorf("the reference or its source is listed under other events:\n%s", after[i:])
	}
	_, next := c.b.get("/w/o/r/workflows/pr/nodes/xx")
	if n := strings.Count(next, "<dt><code>{reviewer}</code>"); n != 1 {
		t.Errorf("{reviewer} is listed %d times for the block after the source:\n%s", n, next)
	}
	if i := strings.Index(next, "From other events"); i >= 0 && strings.Contains(next[i:], "{reviewer}") {
		t.Errorf("the source's tag is listed again under other events:\n%s", next[i:])
	}
	// The source's own Tags list does not offer its tag back.
	_, src := c.b.get("/w/o/r/workflows/pr/nodes/aa")
	if strings.Contains(src, "<dt><code>{reviewer}</code>") {
		t.Errorf("the source is offered its own tag:\n%s", src)
	}
	// Both blocks may use it; a block after the reference saves its use.
	for id, entry := range map[string]string{"af": "{reviewer.report}", "xx": "{reviewer.report}"} {
		name := map[string]string{"af": "After", "xx": "Next"}[id]
		w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
		if code, _, body := c.b.post("/w/o/r/workflows/pr/nodes/"+id, url.Values{"csrf": {c.csrf}, "version": {itoa(int64(w.Version))}, "name": {name}, "entry": {entry}, "finalize": {"on"}}); code != 200 {
			t.Errorf("%s may use %s: %d %s", id, entry, code, body)
		}
	}

	// Renaming the source renames how the reference displays.
	if code, body := c.postNode("aa", url.Values{"name": {"Scout"}, "model": {modelValue(workflow.ModelRef{Provider: c.prov.ID, Model: "m"})}}); code != 200 || !strings.Contains(body, `"rr":"Scout (continued)"`) {
		t.Fatalf("renaming the source: %d %s", code, body)
	}
	if _, form = c.b.get("/w/o/r/workflows/pr/nodes/rr"); !strings.Contains(form, `data-ref-name>Scout (continued)</strong>`) {
		t.Errorf("the reference after the rename:\n%s", form)
	}

	// A reference placed on the canvas and a plain block placed with it: the reference has no name and no tag, the
	// plain block takes the first default.
	nodes := []map[string]any{{"id": "aa", "type": "agent"}, {"id": "rr", "type": "agent"}, {"id": "n1", "type": "agent", "continue_of": "aa"},
		{"id": "n2", "type": "agent"}, {"id": "n3", "type": "agent", "continue_of": "aa"}, {"id": "ev1", "type": "pr_ready"}, {"id": "ev2", "type": "pr_activity"},
		{"id": "xx", "type": "case_entry"}, {"id": "af", "type": "case_entry"}}
	code, answer := c.layout(nodes, []workflow.Link{})
	names, _ := answer["names"].(map[string]any)
	if code != 200 || names["n1"] != "Scout (continued)" || names["n3"] != "Scout (continued)" || names["n2"] != "Agent" || names["rr"] != "Scout (continued)" {
		t.Fatalf("names after placing blocks: %d %v", code, answer)
	}
	for _, n := range c.storedGraph().Nodes {
		if (n.ID == "n1" || n.ID == "n3") && (n.Name != "" || strings.Contains(string(n.Settings), "output_to")) {
			t.Errorf("%s was given a name or a tag: %q %s", n.ID, n.Name, n.Settings)
		}
	}
	if tag := c.storedGraph().OutputNames(workflow.PR); tag["n1"] != "scout" || tag["n2"] != "agent" {
		t.Errorf("tags: %v", tag)
	}

	// Set to continue nothing, it is a block of its own again and needs a name: the first free default.
	plain := url.Values{"csrf": {c.csrf}, "version": {itoa(int64(c.version()))}, "model": {modelValue(workflow.ModelRef{Provider: c.prov.ID, Model: "m"})}, "persona": {"custom"}, // the form of a reference has no name box
		"instruction": {"Look."}, "goal": {"done"}, "max_turns": {"5"}, "max_minutes": {"5"}, "rerun": {"resume"}, "continue_of": {""}}
	if code, _, body := c.b.post("/w/o/r/workflows/pr/nodes/n1", plain); code != 200 {
		t.Fatalf("a reference made plain: %d %s", code, body)
	}
	g := c.storedGraph()
	for _, n := range g.Nodes {
		if n.ID == "n1" && n.Name != "Agent 2" {
			t.Errorf("the block that stopped being a reference is called %q", n.Name)
		}
	}
	for _, n := range g.Nodes { // the source's tag is not renamed in what uses it
		if e, _ := workflow.CaseEntrySettingsOf(n); n.ID == "xx" && e.Entry != "{scout.report}" {
			t.Errorf("a block that used the source's tag now has %q", e.Entry)
		}
	}
	if tag := g.OutputNames(workflow.PR)["n1"]; tag != "agent_2" {
		t.Errorf("its tag is %q", tag)
	}
	// And a block set to continue another gives up its name: what used its tag uses the source's.
	c.saveGraph(append(append([]workflow.Node{}, events...), c.agentNode("aa", "Reviewer", nil), c.agentNode("bb", "Rechecker", nil),
		caseEntryNode("us", "User", "{rechecker.report} [[rechecker]]")), [2]string{"ev1", "aa"}, [2]string{"ev2", "bb"}, [2]string{"bb", "us"})
	cs, _, _ := c.st.OpenCase(c.ws, 1, "pr", "t", "opened")
	c.st.SetCaseValue(cs.UUID, "rechecker.report", "RECHECKED", "bb")
	c.st.SetCaseValue(cs.UUID, "reviewer.summary", "KEPT", "aa")
	c.st.SetCaseValue(cs.UUID, "rechecker.summary", "DROPPED", "bb")
	if code, body := c.postNode("bb", url.Values{"continue_of": {"aa"}}); code != 200 || !strings.Contains(body, "Renamed {rechecker} to {reviewer}") {
		t.Fatalf("a block made a reference: %d %s", code, body)
	}
	g = c.storedGraph()
	for _, n := range g.Nodes {
		if n.ID == "bb" && (n.Name != "" || workflow.StoredOutputTo(n) != "") {
			t.Errorf("the block kept its name or tag: %q %q", n.Name, workflow.StoredOutputTo(n))
		}
		if n.ID == "us" {
			if e, _ := workflow.CaseEntrySettingsOf(n); e.Entry != "{reviewer.report} [[reviewer]]" {
				t.Errorf("what used its tag: %q", e.Entry)
			}
		}
	}
	if v := c.caseValues(); v["reviewer.report"] != "RECHECKED" || v["reviewer.summary"] != "KEPT" || v["rechecker.report"] != "" || v["rechecker.summary"] != "" {
		t.Errorf("values after the block became a reference: %v", v)
	}
}

// A reference block saved before references shared their source's tag, with a name of its own (Discovery
// 2), loses it once, as a new version: what used its tag uses the source's, and the values kept under it
// move to the source's, where the source's own stay.
func TestOldReferenceBlocksAreMigrated(t *testing.T) {
	c := newContEnv(t, nil)
	a := c.agentNode("A", "Discovery", func(s *workflow.AgentSettings) { s.OutputTo = "discovery" })
	r := c.agentNode("R", "Discovery 2", func(s *workflow.AgentSettings) { s.ContinueOf, s.OutputTo = "A", "discovery_2" })
	cond, _ := json.Marshal(workflow.ConditionSettings{Match: "all", Tag: "discovery_2", TagHas: "x"})
	c.saveGraph(append(append([]workflow.Node{}, events...), a, r,
		caseEntryNode("u", "User", "{discovery_2.summary} [[discovery_2.report|The report]] {discovery_2} {discovery.report}"),
		workflow.Node{ID: "c", Type: workflow.Condition, Name: "Has it", Settings: cond}),
		[2]string{"ev1", "A"}, [2]string{"ev2", "R"}, [2]string{"R", "u"}, [2]string{"R", "c"})
	cs, _, _ := c.st.OpenCase(c.ws, 1, "pr", "t", "opened")
	for k, v := range map[string]string{"discovery.report": "KEEP", "discovery_2": "OLD", "discovery_2.report": "DROP", "discovery_2.summary": "MOVED", "discovery_20": "NOT"} {
		c.st.SetCaseValue(cs.UUID, k, v, "x")
	}
	before := c.version()
	c.ui.MigrateWorkflows()
	g := c.storedGraph()
	byID := map[string]workflow.Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	if c.version() != before+1 || byID["R"].Name != "" || workflow.StoredOutputTo(byID["R"]) != "" || workflow.ReferenceOf(byID["R"]) != "A" {
		t.Errorf("version %d (was %d); the reference: %q %q", c.version(), before, byID["R"].Name, workflow.StoredOutputTo(byID["R"]))
	}
	if e, _ := workflow.CaseEntrySettingsOf(byID["u"]); e.Entry != "{discovery.summary} [[discovery.report|The report]] {discovery} {discovery.report}" {
		t.Errorf("the entry: %q", e.Entry)
	}
	if cd, _ := workflow.SettingsOf(byID["c"], workflow.DefaultCondition()); cd.Tag != "discovery" {
		t.Errorf("the condition's tag: %q", cd.Tag)
	}
	if v, _ := c.st.CaseValues(cs.UUID); v["discovery.report"] != "KEEP" || v["discovery"] != "OLD" || v["discovery.summary"] != "MOVED" ||
		v["discovery_2"] != "" || v["discovery_2.report"] != "" || v["discovery_2.summary"] != "" || v["discovery_20"] != "NOT" {
		t.Errorf("values: %v", v)
	}
	// Once; and the block still shows as the source's continuation.
	c.ui.MigrateWorkflows()
	if c.version() != before+1 {
		t.Errorf("migrated again: version %d", c.version())
	}
	if got := g.NameOf(byID["R"]); got != "Discovery (continued)" {
		t.Errorf("the reference is called %q", got)
	}
}
