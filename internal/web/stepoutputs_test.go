package web

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// node is a step block of a type, with settings, named name.
func node(id, typ, name string, settings any) workflow.Node {
	b, _ := json.Marshal(settings)
	return workflow.Node{ID: id, Type: typ, Name: name, Settings: b}
}

// Every step's output is kept under its name, with no "Output to" filled in: a step anywhere
// downstream reads an earlier step's parts in the same run, across a merge and down a Decision's
// branch, and the next run on the item finds them in the case.
func TestEveryStepKeepsItsOutputUnderItsName(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = outputsModel(`{"status":"met","report":"RESEARCH SHORT"}`)
	llm, _ := llmServer(t) // its decisions answer "yes"
	dp, err := c.st.CreateProvider(store.ProviderEdit{Name: "Decider", Kind: store.KindOpenAI, Endpoint: llm.URL + "/v1", Key: "k"}, c.admin)
	if err != nil {
		t.Fatal(err)
	}
	c.st.SetProviderModels(dp.ID, []string{"d"})
	c.st.SetProviderRoutes(dp.ID, true, false)
	c.st.LendProvider(dp, []int64{c.ws.ID}, c.admin)

	a := c.agentNode("A", "Discovery", func(s *workflow.AgentSettings) {
		s.Closing, s.ReportTurns, s.MaxTurns, s.SubAgents = workflow.ClosingReport, 4, 10, 1
	})
	d := workflow.DefaultDecision(workflow.PR)
	d.Model = workflow.ModelRef{Provider: dp.ID, Model: "d"}
	comment := workflow.DefaultPost()
	comment.Message = "Comment on {discovery.summary}"
	nodes := append(append([]workflow.Node{}, events...), a,
		node("route", workflow.Decision, "Route", d),
		node("cond", workflow.Condition, "Is PR", workflow.ConditionSettings{Match: "all", Contains: "PR"}),
		node("mid", workflow.CaseEntry, "Mid note", workflow.CaseEntrySettings{Entry: "MID {discovery.report}", Finalize: true}),
		// Two hops from the agent, with a block between that is not an agent.
		node("end", workflow.CaseEntry, "End note", workflow.CaseEntrySettings{Entry: "END {discovery.report} | {mid_note} | {is_pr}", Finalize: true}),
		// Down a Decision's branch: the agent's parts, and the decision's own answer.
		node("yes", workflow.CaseEntry, "Yes branch", workflow.CaseEntrySettings{Entry: "YES S=[{discovery.summary}] N=[{discovery.scratchpad}] W=[{discovery}] D={route} C={route.confidence} R={route.reason}", Finalize: true}),
		node("no", workflow.CaseEntry, "No branch", workflow.CaseEntrySettings{Entry: "NO", Finalize: true}),
		// A merge of two branches reads the tags of both, which neither is the step before.
		node("join", workflow.CaseEntry, "Joined", workflow.CaseEntrySettings{Entry: "JOIN {discovery.report} | {mid_note} | {yes_branch}", Finalize: true}),
		node("cm", workflow.Post, "Review comment", comment))
	g := workflow.Graph{Nodes: nodes, Links: []workflow.Link{
		{From: "ev1", Out: "out", To: "A", In: "in"},
		{From: "A", Out: "out", To: "cond", In: "in"},
		{From: "cond", Out: "yes", To: "mid", In: "in"},
		{From: "mid", Out: "out", To: "end", In: "in"},
		{From: "A", Out: "out", To: "route", In: "in"},
		{From: "route", Out: "yes", To: "yes", In: "in"},
		{From: "route", Out: "no", To: "no", In: "in"},
		{From: "mid", Out: "out", To: "join", In: "in"},
		{From: "yes", Out: "out", To: "join", In: "in"},
		{From: "join", Out: "out", To: "cm", In: "in"},
	}}
	b, _ := json.Marshal(g)
	w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
	if _, err := c.st.SaveWorkflow(c.ws.ID, workflow.PR, w.Version, string(b), c.admin); err != nil {
		t.Fatal(err)
	}
	steps := c.run("ev1")
	for id, s := range steps {
		if s.Status == "failed" {
			t.Fatalf("%s failed: %+v", id, s)
		}
	}
	notes := notesText
	want := map[string]string{
		"mid":  "MID THE REPORT",
		"end":  "END THE REPORT | MID THE REPORT | " + steps["cond"].Output,
		"yes":  "YES S=[THE SUMMARY] N=[" + notes + "] W=[## Summary\n\nTHE SUMMARY\n\n## Report\n\nTHE REPORT\n\n## Notes\n\n" + notes + "] D=yes C=80% R=It should.",
		"join": "JOIN THE REPORT | MID THE REPORT | " + steps["yes"].Output,
	}
	for id, w := range want {
		if steps[id].Status != "done" || steps[id].Output != w {
			t.Errorf("%s: %s\n got %q\nwant %q", id, steps[id].Status, steps[id].Output, w)
		}
	}
	if steps["no"].Status != "skipped" {
		t.Errorf("the other branch: %s", steps["no"].Status)
	}
	if steps["cm"].Status != "done" || !strings.Contains(steps["cm"].Output, "Comment on THE SUMMARY") {
		t.Errorf("the comment: %+v", steps["cm"])
	}
	// Each step's output is kept per case under the step's name, with an Agent's parts.
	v := c.caseValues()
	for tag, w := range map[string]string{
		"discovery.report": "THE REPORT", "discovery.summary": "THE SUMMARY", "route": "yes", "route.confidence": "80%", "route.reason": "It should.",
		"is_pr": steps["cond"].Output, "mid_note": "MID THE REPORT", "end_note": want["end"], "yes_branch": want["yes"],
		"joined": want["join"],
	} {
		if v[tag] != w || w == "" {
			t.Errorf("{%s} = %q, want %q", tag, v[tag], w)
		}
	}
	for _, tag := range []string{"no_branch", "review_comment"} {
		if _, kept := v[tag]; kept {
			t.Errorf("{%s} was kept: a skipped step, and a block that posts, keep no output", tag)
		}
	}
}

// {input} works for a step after another, and is listed; the old {previous_output} still works, unlisted.
func TestInputIsListedAndPreviousOutputStillWorks(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveGraph(append(append([]workflow.Node{}, events...),
		caseEntryNode("one", "First", "ONE"), caseEntryNode("two", "Second", "I=[{input}] P=[{previous_output}] T=[{first}]")),
		[2]string{"ev1", "one"}, [2]string{"one", "two"})
	steps := c.run("ev1")
	if steps["two"].Output != "I=[ONE] P=[ONE] T=[ONE]" {
		t.Fatalf("%q", steps["two"].Output)
	}
	_, body := c.b.get("/w/o/r/workflows/pr/nodes/two")
	if !strings.Contains(body, "<code>{input}</code>") || strings.Contains(body, "{previous_output}</code>") || !strings.Contains(body, "<code>{first}</code>") {
		t.Errorf("the Tags list:\n%s", body)
	}
}

// formTag is the tag a block's settings form shows, read-only under its name; "" when it shows none.
func (c *contEnv) formTag(id string) string {
	c.t.Helper()
	code, body := c.b.get("/w/o/r/workflows/pr/nodes/" + id)
	if code != 200 || strings.Contains(body, `name="output_to"`) {
		c.t.Fatalf("the form of %s: %d (it has an Output to box: %v)\n%s", id, code, strings.Contains(body, `name="output_to"`), body)
	}
	m := blockTagRE.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

var blockTagRE = regexp.MustCompile(`<p class="blocktag">Tag <code>\{([^}]*)\}</code>`)

func (c *contEnv) storedGraph() workflow.Graph {
	c.t.Helper()
	w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
	var g workflow.Graph
	json.Unmarshal([]byte(w.Graph), &g)
	return g
}

func (c *contEnv) version() int {
	w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
	return w.Version
}

// A block's tag is its name in snake case: the dialog shows it under the name, read-only, with no
// Output to box; the Tags list of a later block offers it; a block with no tag (an event, a Post) shows none.
func TestTagIsTheNameInSnakeCase(t *testing.T) {
	c := newContEnv(t, nil)
	post := workflow.DefaultPost()
	post.Message = "x"
	c.saveGraph(append(append([]workflow.Node{}, events...), node("old", workflow.CaseEntry, "Value Proposition", map[string]any{"entry": "x", "finalize": true}),
		caseEntryNode("use", "User", "{value_proposition}"), node("po", workflow.Post, "Say it", post)),
		[2]string{"ev1", "old"}, [2]string{"old", "use"}, [2]string{"use", "po"})
	for id, want := range map[string]string{"old": "value_proposition", "use": "user", "ev1": "", "po": ""} {
		if got := c.formTag(id); got != want {
			t.Errorf("the form of %s shows the tag %q, want %q", id, got, want)
		}
	}
	_, body := c.b.get("/w/o/r/workflows/pr/nodes/use")
	if !strings.Contains(body, "<dt><code>{value_proposition}</code>") {
		t.Errorf("the Tags list lacks it:\n%s", body)
	}
	// Saved, the tag is kept (for older readers) as the name's, whatever the form sends.
	code, _, out := c.b.post("/w/o/r/workflows/pr/nodes/old", url.Values{"csrf": {c.csrf}, "version": {itoa(int64(c.version()))},
		"name": {"Value Proposition"}, "entry": {"x"}, "finalize": {"on"}, "output_to": {"sneaky"}})
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	if got := workflow.StoredOutputTo(c.storedGraph().Nodes[2]); got != "value_proposition" {
		t.Errorf("stored %q", got)
	}
}

// A new block is named after its type, then with 2, 3 … where the workflow has a block of that name; a
// name that is another block's (an event's too, compared by its tag) or a built-in tag is refused.
func TestNewBlocksGetFreeNamesAndDuplicatesAreRefused(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveGraph(events)
	nodes := []map[string]any{{"id": "ev1", "type": "pr_ready"}, {"id": "ev2", "type": "pr_activity"},
		{"id": "one", "type": "case_entry", "x": 1, "y": 1}, {"id": "two", "type": "case_entry", "x": 2, "y": 2}, {"id": "three", "type": "case_entry", "x": 3, "y": 3},
		{"id": "ag", "type": "agent", "x": 4, "y": 4}, {"id": "ag2", "type": "agent", "x": 5, "y": 5}}
	code, answer := c.layout(nodes, []workflow.Link{})
	if code != 200 {
		t.Fatalf("layout %d %v", code, answer)
	}
	names := answer["names"].(map[string]any)
	for id, want := range map[string]string{"one": "Case Entry", "two": "Case Entry 2", "three": "Case Entry 3", "ag": "Agent", "ag2": "Agent 2", "ev1": "PR First Ready"} {
		if names[id] != want {
			t.Errorf("%s is named %v, want %s", id, names[id], want)
		}
	}
	sums := answer["summaries"].(map[string]any)
	if sums["two"] != "Appends a finalized entry · → {case_entry_2}" || sums["ag"] != "Choose a model · → {agent}" {
		t.Errorf("the cards say %v", sums)
	}
	for id, want := range map[string]string{"one": "case_entry", "two": "case_entry_2", "ag2": "agent_2"} {
		if got := c.formTag(id); got != want {
			t.Errorf("%s: the form shows %q, want %q", id, got, want)
		}
	}
	// Deleting the first does not rename the others.
	if code, _ := c.layout(append(append([]map[string]any{}, nodes[:2]...), nodes[3:]...), []workflow.Link{}); code != 200 {
		t.Fatal(code)
	}
	if got := c.formTag("two"); got != "case_entry_2" {
		t.Errorf("after deleting the first: %q", got)
	}
	save := func(id, name string) (int, string) {
		code, _, body := c.b.post("/w/o/r/workflows/pr/nodes/"+id, url.Values{"csrf": {c.csrf}, "version": {itoa(int64(c.version()))},
			"name": {name}, "entry": {"text"}, "finalize": {"on"}})
		return code, body
	}
	for name, want := range map[string]string{"case entry 3": "another block is already named Case Entry 3", "Pr Title": "built-in tag", "Case": "built-in tag",
		"When ready": "", "pr first ready": "another block is already named PR First Ready", "???": "needs a letter or a digit"} {
		code, body := save("two", name)
		if want == "" {
			if code != 200 {
				t.Errorf("%q refused: %d %s", name, code, body)
			}
			continue
		}
		if code != 400 || !strings.Contains(body, want) {
			t.Errorf("%q: %d, want %q:\n%s", name, code, want, body)
		}
	}
	if got := c.formTag("two"); got != "when_ready" {
		t.Errorf("the tag after the one accepted rename: %q", got)
	}
}

// Renaming a block rewrites every reference to its tag in the workflow's other blocks, in the same
// save: {x}, {x.part}, [[x]], [[x.part|Label]] and a Condition's tag field, not {{x}} nor {xs}; the
// values kept for the cases move to the new names; the answer says what was done; and the other kind
// of workflow is not touched.
func TestRenamingABlockRewritesItsReferences(t *testing.T) {
	c := newContEnv(t, nil)
	entry := func(text string) workflow.CaseEntrySettings { return workflow.CaseEntrySettings{Entry: text} }
	cond := workflow.ConditionSettings{Match: "all", Tag: "discovery", TagHas: "ok"}
	post := workflow.DefaultPost()
	post.Message = "Post [[discovery.report|The report]]"
	// An issue workflow with a block of the same name, which must not be touched.
	issueEntry, _ := json.Marshal(entry("Issue {discovery}"))
	ig, _ := json.Marshal(workflow.Graph{Nodes: []workflow.Node{{ID: "ie", Type: "issue_opened"}, {ID: "x", Type: workflow.CaseEntry, Name: "Discovery"}, {ID: "y", Type: workflow.CaseEntry, Name: "Other", Settings: issueEntry}},
		Links: []workflow.Link{{From: "ie", Out: "out", To: "x", In: "in"}, {From: "x", Out: "out", To: "y", In: "in"}}})
	if _, err := c.st.SaveWorkflow(c.ws.ID, workflow.Issue, 0, string(ig), c.admin); err != nil {
		t.Fatal(err)
	}
	pg, _ := json.Marshal(workflow.Graph{Nodes: append(append([]workflow.Node{}, events...), caseEntryNode("a", "Discovery", "found"),
		caseEntryNode("b", "Uses it", "{discovery} {discovery.report} [[discovery]] [[discovery.report|Intake]] {{discovery}} {discoveries} [[[[discovery]]"),
		node("c", workflow.Condition, "Looks ok", cond), node("po", workflow.Post, "Say it", post)),
		Links: []workflow.Link{{From: "ev1", Out: "out", To: "a", In: "in"}, {From: "a", Out: "out", To: "b", In: "in"}, {From: "a", Out: "out", To: "c", In: "in"}, {From: "c", Out: "yes", To: "po", In: "in"}}})
	if _, err := c.st.SaveWorkflow(c.ws.ID, workflow.PR, c.version(), string(pg), c.admin); err != nil {
		t.Fatal(err)
	}
	// Values kept for a pull request's case and for an issue's.
	pr, _, _ := c.st.OpenCase(c.ws, 1, "pr", "t", "opened")
	is, _, _ := c.st.OpenCase(c.ws, 2, "issue", "t", "opened")
	for _, cs := range []string{pr.UUID, is.UUID} {
		for tag, v := range map[string]string{"discovery": "W", "discovery.report": "R", "discovery.summary": "S", "discoveryx": "NOT", "other": "O"} {
			c.st.SetCaseValue(cs, tag, v, "a")
		}
	}
	w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
	code, _, out := c.b.post("/w/o/r/workflows/pr/nodes/a", url.Values{"csrf": {c.csrf}, "version": {itoa(int64(w.Version))}, "name": {"Intake Notes"}, "entry": {"found"}, "finalize": {"on"}})
	if code != 200 || !strings.Contains(out, `"note":"Renamed {discovery} to {intake_notes} in 3 blocks"`) {
		t.Fatalf("%d %s", code, out)
	}
	g := c.storedGraph()
	byID := map[string]workflow.Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	if e, _ := workflow.CaseEntrySettingsOf(byID["b"]); e.Entry != "{intake_notes} {intake_notes.report} [[intake_notes]] [[intake_notes.report|Intake]] {{discovery}} {discoveries} [[[[discovery]]" {
		t.Errorf("the entry: %q", e.Entry)
	}
	if cs, _ := workflow.SettingsOf(byID["c"], workflow.DefaultCondition()); cs.Tag != "intake_notes" || cs.TagHas != "ok" {
		t.Errorf("the Condition: %+v", cs)
	}
	if p, _ := workflow.SettingsOf(byID["po"], workflow.DefaultPost()); p.Message != "Post [[intake_notes.report|The report]]" {
		t.Errorf("the Post: %q", p.Message)
	}
	if byID["a"].Name != "Intake Notes" || workflow.StoredOutputTo(byID["a"]) != "intake_notes" {
		t.Errorf("the renamed block: %q %q", byID["a"].Name, workflow.StoredOutputTo(byID["a"]))
	}
	// The values moved, for pull requests' cases only.
	got, _ := c.st.CaseValues(pr.UUID)
	if got["intake_notes"] != "W" || got["intake_notes.report"] != "R" || got["intake_notes.summary"] != "S" || got["discovery"] != "" || got["discovery.report"] != "" || got["discoveryx"] != "NOT" || got["other"] != "O" {
		t.Errorf("a pull request's values: %v", got)
	}
	if got, _ := c.st.CaseValues(is.UUID); got["discovery"] != "W" || got["intake_notes"] != "" {
		t.Errorf("an issue's values were touched: %v", got)
	}
	// The issue workflow is as it was.
	iw, _ := c.st.Workflow(c.ws.ID, workflow.Issue)
	if iw.Graph != string(ig) && !strings.Contains(iw.Graph, `Issue {discovery}`) {
		t.Errorf("the issue workflow changed: %s", iw.Graph)
	}
	// A name that does not change the tag changes nothing else.
	w, _ = c.st.Workflow(c.ws.ID, workflow.PR)
	_, _, out = c.b.post("/w/o/r/workflows/pr/nodes/a", url.Values{"csrf": {c.csrf}, "version": {itoa(int64(w.Version))}, "name": {"Intake notes!"}, "entry": {"found"}, "finalize": {"on"}})
	if strings.Contains(out, "Renamed") {
		t.Errorf("a rename that kept the tag said %s", out)
	}
}

// A workflow saved before tags were names, with a block whose Output to is not its name's, is brought up
// to date once, as a new version: the block is renamed to its tag, so what uses the tag still works; where
// another block has that name the block keeps its own and the references and the kept values move.
func TestOldOutputToIsMigratedOnce(t *testing.T) {
	c := newContEnv(t, nil)
	custom := func(tag string) workflow.AgentSettings {
		a := workflow.DefaultAgent(workflow.PR)
		a.OutputTo = tag
		return a
	}
	c.saveGraph(append(append([]workflow.Node{}, events...),
		node("a", workflow.Agent, "Gatherer", custom("value_summary")),
		node("b", workflow.Agent, "Checker", custom("verdict")),
		caseEntryNode("v", "Verdict", "unrelated"),
		caseEntryNode("u", "User", "{value_summary.report} {verdict}")),
		[2]string{"ev1", "a"}, [2]string{"a", "b"}, [2]string{"b", "v"}, [2]string{"v", "u"})
	cs, _, _ := c.st.OpenCase(c.ws, 1, "pr", "t", "opened")
	c.st.SetCaseValue(cs.UUID, "value_summary", "W", "a")
	c.st.SetCaseValue(cs.UUID, "verdict", "V", "b")
	before := c.version()
	c.ui.MigrateWorkflows()
	g := c.storedGraph()
	byID := map[string]workflow.Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	if byID["a"].Name != "Value summary" || byID["b"].Name != "Checker" || c.version() != before+1 {
		t.Errorf("names %q and %q, version %d (was %d)", byID["a"].Name, byID["b"].Name, c.version(), before)
	}
	if e, _ := workflow.CaseEntrySettingsOf(byID["u"]); e.Entry != "{value_summary.report} {checker}" {
		t.Errorf("the reference to the one that was renamed: %q", e.Entry)
	}
	if v, _ := c.st.CaseValues(cs.UUID); v["value_summary"] != "W" || v["checker"] != "V" || v["verdict"] != "" {
		t.Errorf("values %v", v)
	}
	// Once.
	c.ui.MigrateWorkflows()
	if c.version() != before+1 {
		t.Errorf("migrated again: version %d", c.version())
	}
	// And a workflow read before any run or edit is migrated then, too.
	c.saveGraph(append(append([]workflow.Node{}, events...), node("a", workflow.Agent, "Gatherer", custom("kept_tag"))), [2]string{"ev1", "a"})
	if got := c.formTag("a"); got != "kept_tag" {
		t.Errorf("an old block opened in the dialog: %q", got)
	}
}

// A block's templates may use the output of the steps upstream of it only: with Event → A → B, A → C
// and B → D, D may use A's and B's tags but not C's, and C may not use B's. The refusal names the step.
// The Tags list shows the same set, a link removed later marks the block that now uses a tag from a
// step that does not run before it (the layout still saves), and [[ ]] follows the same rule.
func TestBlocksSeeOnlyTheTagsOfTheStepsBeforeThem(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveGraph(append(append([]workflow.Node{}, events...), caseEntryNode("a", "Alpha", "A"), caseEntryNode("b", "Beta", "B"),
		caseEntryNode("c", "Gamma", "C"), caseEntryNode("d", "Delta", "D")),
		[2]string{"ev1", "a"}, [2]string{"a", "b"}, [2]string{"a", "c"}, [2]string{"b", "d"})
	save := func(id, name, entry string) (int, string) {
		code, _, body := c.b.post("/w/o/r/workflows/pr/nodes/"+id, url.Values{"csrf": {c.csrf}, "version": {itoa(int64(c.version()))},
			"name": {name}, "entry": {entry}, "finalize": {"on"}})
		return code, body
	}
	for _, ok := range []struct{ id, name, entry string }{{"d", "Delta", "{alpha} and {beta}"}, {"d", "Delta", "[[alpha]] and [[beta]]"}, {"c", "Gamma", "{alpha}"}, {"b", "Beta", "{alpha}"}} {
		if code, body := save(ok.id, ok.name, ok.entry); code != 200 {
			t.Errorf("%s may use %s: %d\n%s", ok.id, ok.entry, code, body)
		}
	}
	for _, bad := range []struct{ id, name, entry, want string }{
		{"d", "Delta", "{gamma}", "{gamma} comes from Gamma, which does not run before this block"},
		{"d", "Delta", "[[gamma]]", "[[gamma]] comes from Gamma, which does not run before this block"},
		{"c", "Gamma", "see {beta}", "{beta} comes from Beta, which does not run before this block"},
		{"a", "Alpha", "{delta}", "{delta} comes from Delta, which does not run before this block"},
		{"d", "Delta", "{delta}", "{delta}: this block's own output"}, // its own output is not upstream, and is nobody else's to name
		{"d", "Delta", "{nosuch}", "{nosuch}: no block is called Nosuch"},
	} {
		code, body := save(bad.id, bad.name, bad.entry)
		if code != 400 || !strings.Contains(strings.ReplaceAll(body, "&#39;", "'"), bad.want) {
			t.Errorf("%s with %s: %d, want %q\n%s", bad.id, bad.entry, code, bad.want, body)
		}
	}
	// The Tags list of D has A's and B's, not C's nor its own; C's has A's only.
	tagsOf := func(id string) string {
		_, body := c.b.get("/w/o/r/workflows/pr/nodes/" + id)
		return body
	}
	d := tagsOf("d")
	if !strings.Contains(d, "<dt><code>{alpha}</code>") || !strings.Contains(d, "<dt><code>{beta}</code>") || strings.Contains(d, "<dt><code>{gamma}</code>") || strings.Contains(d, "<dt><code>{delta}</code>") {
		t.Errorf("the Tags list of D:\n%s", d)
	}
	cc := tagsOf("c")
	if !strings.Contains(cc, "<dt><code>{alpha}</code>") || strings.Contains(cc, "<dt><code>{beta}</code>") || strings.Contains(cc, "<dt><code>{delta}</code>") {
		t.Errorf("the Tags list of C:\n%s", cc)
	}
	// Replacing the link B → D by one from the event leaves D, which uses {alpha} and {beta}, in their chain
	// with neither upstream: the layout saves, and D is marked.
	save("d", "Delta", "{alpha} and {beta}")
	var nodes []map[string]any
	for _, n := range c.storedGraph().Nodes {
		nodes = append(nodes, map[string]any{"id": n.ID, "type": n.Type, "x": n.X, "y": n.Y})
	}
	code, answer := c.layout(nodes, []workflow.Link{{From: "ev1", Out: "out", To: "a", In: "in"}, {From: "a", Out: "out", To: "b", In: "in"}, {From: "a", Out: "out", To: "c", In: "in"}, {From: "ev1", Out: "out", To: "d", In: "in"}})
	if code != 200 {
		t.Fatalf("the layout was not saved: %d %v", code, answer)
	}
	problems, _ := answer["problems"].(map[string]any)
	if len(problems) != 1 || !strings.Contains(problems["d"].(string), "comes from Alpha, which does not run before this block") {
		t.Errorf("problems: %v", answer["problems"])
	}
	if sum := answer["summaries"].(map[string]any)["d"].(string); !strings.HasPrefix(sum, "⚠ ") {
		t.Errorf("the card of D says %q", sum)
	}
	// The page, loaded later, marks it the same way.
	if _, page := c.b.get("/w/o/r/workflows/pr"); !strings.Contains(page, "⚠ {alpha} comes from Alpha") {
		t.Errorf("the canvas does not mark D when loaded")
	}
}

// The block that posts is an endpoint: no tag, no field, nothing on the card or in the Tags list, nothing
// kept in the case.
func TestPostingBlockHasNoOutputTag(t *testing.T) {
	c := newContEnv(t, nil)
	post := workflow.DefaultPost()
	post.Message = "Hello {pr:title}"
	c.saveGraph(append(append([]workflow.Node{}, events...), node("cm", workflow.Post, "Review comment", post), caseEntryNode("ce", "Later", "x")),
		[2]string{"ev1", "cm"}, [2]string{"cm", "ce"})
	g := c.storedGraph()
	if len(g.OutputNames(workflow.PR)) != 1 || g.OutputNames(workflow.PR)["ce"] != "later" {
		t.Errorf("output names: %v", g.OutputNames(workflow.PR))
	}
	code, body := c.b.get("/w/o/r/workflows/pr/nodes/cm")
	if code != 200 || strings.Contains(body, `name="output_to"`) || strings.Contains(body, "Output to") {
		t.Errorf("the form of the Post has an Output to:\n%s", body)
	}
	_, body = c.b.get("/w/o/r/workflows/pr/nodes/ce")
	if strings.Contains(body, "<code>{review_comment}</code>") || strings.Contains(body, "<code>{post}</code>") {
		t.Errorf("the Tags list offers a posting block's output:\n%s", body)
	}
	if card := c.ui.summaries(c.ws, g, workflow.PR)["cm"]; card != "GitHub (trial)" {
		t.Errorf("the card says %q", card)
	}
	// Saving it stores no tag, though one is sent.
	w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
	if code, _, body := c.b.post("/w/o/r/workflows/pr/nodes/cm", url.Values{"csrf": {c.csrf}, "version": {itoa(int64(w.Version))}, "name": {"Review comment"}, "message": {"Hi"},
		"github": {"on"}, "trial": {"on"}, "output_to": {"sneaky"}}); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	for _, n := range c.storedGraph().Nodes {
		if n.ID == "cm" && strings.Contains(string(n.Settings), "output_to") {
			t.Errorf("a tag was stored on the Post: %s", n.Settings)
		}
	}
	steps := c.run("ev1")
	if steps["cm"].Status != "done" || steps["ce"].Status != "done" {
		t.Fatalf("%+v", steps)
	}
	// The trial wrote the case file; the Post kept no tag, and the entry after it kept its own.
	if v := c.caseValues(); len(v) != 1 || v["later"] != "x" {
		t.Errorf("tags kept: %v", v)
	}
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// Another event's steps may be used too: those of a chain the block is not in give what their latest run
// on the item kept. With E1 → Discovery → Check and E1 → Sibling, and E2 → Recheck: Recheck may use
// {discovery} and {check}; Check may use Discovery's but not Sibling's, nor Recheck's, which it does not
// share a chain with... and Recheck's only if it is not in a chain of Check's.
func TestStepsOfOtherEventsMayBeUsed(t *testing.T) {
	c := newContEnv(t, nil)
	graph := func(extra ...[2]string) {
		c.saveGraph(append(append([]workflow.Node{}, events...), caseEntryNode("d", "Discovery", "DISCOVERED"), caseEntryNode("k", "Check", "CHECKED"),
			caseEntryNode("s", "Sibling", "SIB"), caseEntryNode("r", "Recheck", "x"), caseEntryNode("m", "Merge", "x")),
			append([][2]string{{"ev1", "d"}, {"d", "k"}, {"ev1", "s"}, {"ev2", "r"}}, extra...)...)
	}
	graph()
	save := func(id, name, entry string) (int, string) {
		code, _, body := c.b.post("/w/o/r/workflows/pr/nodes/"+id, url.Values{"csrf": {c.csrf}, "version": {itoa(int64(c.version()))},
			"name": {name}, "entry": {entry}, "finalize": {"on"}})
		return code, strings.ReplaceAll(body, "&#39;", "'")
	}
	// Another chain's tags are allowed; the stored value is what renders; and it is empty before it ever ran.
	if code, body := save("r", "Recheck", "D=[{discovery}] K=[{check}] S=[{sibling}] L=[[discovery]]"); code != 200 {
		t.Fatalf("another chain's tags: %d\n%s", code, body)
	}
	steps := c.run("ev2")
	if !strings.HasPrefix(steps["r"].Output, "D=[] K=[] S=[] L=[Discovery](") {
		t.Errorf("before the other chain ever ran: %q", steps["r"].Output)
	}
	c.run("ev1")
	steps = c.run("ev2")
	if got := steps["r"].Output; !strings.HasPrefix(got, "D=[DISCOVERED] K=[CHECKED] S=[SIB] L=[Discovery](") {
		t.Errorf("after it ran: %q", got)
	}
	// Within a chain only what is upstream: Check may use Discovery's, not Sibling's, and a step of another chain only if it shares none.
	for _, c1 := range []struct {
		id, name, entry, want string
		ok                    bool
	}{
		{"k", "Check", "{discovery}", "", true},
		{"k", "Check", "{sibling}", "{sibling} comes from Sibling, which does not run before this block", false},
		{"d", "Discovery", "{check}", "{check} comes from Check, which does not run before this block", false},
		{"k", "Check", "{recheck}", "", true},
		{"s", "Sibling", "{discovery}", "{discovery} comes from Discovery, which does not run before this block", false},
	} {
		code, body := save(c1.id, c1.name, c1.entry)
		if c1.ok != (code == 200) || (!c1.ok && !strings.Contains(body, c1.want)) {
			t.Errorf("%s with %s: %d, want ok=%v %q\n%s", c1.id, c1.entry, code, c1.ok, c1.want, body)
		}
	}
	// A block in two chains (E1 → M and E2 → M): it shares one with Discovery (E1's) and one with Recheck (E2's), so it may
	// use neither unless upstream; Check, in E1's only, may use Recheck's.
	graph([2]string{"ev1", "m"}, [2]string{"ev2", "m"})
	for entry, want := range map[string]string{"{discovery}": "{discovery} comes from Discovery", "{recheck}": "{recheck} comes from Recheck", "{check}": "{check} comes from Check"} {
		if code, body := save("m", "Merge", entry); code != 400 || !strings.Contains(body, want) {
			t.Errorf("a block in two chains with %s: %d\n%s", entry, code, body)
		}
	}
	// The Tags list groups them apart, with their parts as usual.
	_, form := c.b.get("/w/o/r/workflows/pr/nodes/k")
	i := strings.Index(form, "From other events (their last run on this item)")
	if i < 0 || !strings.Contains(form[i:], "<dt><code>{recheck}</code>") || strings.Contains(form[i:], "<dt><code>{discovery}</code>") || !strings.Contains(form[:i], "<dt><code>{discovery}</code>") {
		t.Errorf("the Tags list of Check:\n%s", form)
	}
	// A link made later that puts the block in a chain with a step it used from another event flags it,
	// and the layout still saves.
	graph()
	save("r", "Recheck", "{discovery}")
	var nodes []map[string]any
	for _, n := range c.storedGraph().Nodes {
		nodes = append(nodes, map[string]any{"id": n.ID, "type": n.Type, "x": n.X, "y": n.Y})
	}
	links := append(append([]workflow.Link{}, c.storedGraph().Links...), workflow.Link{From: "ev1", Out: "out", To: "r", In: "in"})
	code, answer := c.layout(nodes, links)
	problems, _ := answer["problems"].(map[string]any)
	if code != 200 || len(problems) != 1 || !strings.Contains(problems["r"].(string), "{discovery} comes from Discovery, which does not run before this block") {
		t.Errorf("a link into a shared chain: %d %v", code, answer["problems"])
	}
}
