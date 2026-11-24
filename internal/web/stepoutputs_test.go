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
		node("yes", workflow.CaseEntry, "Yes branch", workflow.CaseEntrySettings{Entry: "YES S=[{discovery.summary}] N=[{discovery.scratchpad}] W=[{discovery}] D={route}", Finalize: true}),
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
		"yes":  "YES S=[THE SUMMARY] N=[" + notes + "] W=[## Summary\n\nTHE SUMMARY\n\n## Report\n\nTHE REPORT\n\n## Notes\n\n" + notes + "] D=yes",
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
		"discovery.report": "THE REPORT", "discovery.summary": "THE SUMMARY", "route": "yes",
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

// {input} still works for a step after another, and {previous_output} with it, though neither is listed.
func TestInputStillWorksButIsNotListed(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveGraph(append(append([]workflow.Node{}, events...),
		caseEntryNode("one", "First", "ONE"), caseEntryNode("two", "Second", "I=[{input}] P=[{previous_output}] T=[{first}]")),
		[2]string{"ev1", "one"}, [2]string{"one", "two"})
	steps := c.run("ev1")
	if steps["two"].Output != "I=[ONE] P=[ONE] T=[ONE]" {
		t.Fatalf("%q", steps["two"].Output)
	}
	_, body := c.b.get("/w/o/r/workflows/pr/nodes/two")
	if strings.Contains(body, "<code>{input}</code>") || strings.Contains(body, "{previous_output}</code>") || !strings.Contains(body, "<code>{first}</code>") {
		t.Errorf("the Tags list:\n%s", body)
	}
}

var outputToValue = regexp.MustCompile(`name="output_to" value="([^"]*)"`)

// formTag is the output tag a block's settings form shows.
func (c *contEnv) formTag(id string) string {
	c.t.Helper()
	code, body := c.b.get("/w/o/r/workflows/pr/nodes/" + id)
	m := outputToValue.FindStringSubmatch(body)
	if code != 200 || m == nil {
		c.t.Fatalf("the form of %s: %d\n%s", id, code, body)
	}
	return m[1]
}

func (c *contEnv) storedGraph() workflow.Graph {
	c.t.Helper()
	w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
	var g workflow.Graph
	json.Unmarshal([]byte(w.Graph), &g)
	return g
}

// A block saved before output tags existed behaves as if its tag were its name's: the Tags list
// offers it, the form shows it, the card shows it and templates may use it. Saving the block stores it.
func TestOldBlockGetsItsNamesSlug(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveGraph(append(append([]workflow.Node{}, events...),
		node("old", workflow.CaseEntry, "Value Proposition", map[string]any{"entry": "x", "finalize": true}),
		caseEntryNode("use", "User", "{value_proposition}")), [2]string{"ev1", "old"}, [2]string{"old", "use"})
	if got := c.formTag("old"); got != "value_proposition" {
		t.Fatalf("the form shows %q", got)
	}
	_, body := c.b.get("/w/o/r/workflows/pr/nodes/use")
	if !strings.Contains(body, "<code>{value_proposition}</code>") {
		t.Errorf("the Tags list lacks it:\n%s", body)
	}
	if stored := workflow.StoredOutputTo(c.storedGraph().Nodes[2]); stored != "" {
		t.Fatalf("reading stored it: %q", stored)
	}
	// A template of another block may use it, saved or not.
	code, _, out := c.b.post("/w/o/r/workflows/pr/nodes/use", url.Values{"csrf": {c.csrf}, "version": {itoa(int64(c.version()))},
		"name": {"User"}, "entry": {"Using {value_proposition}"}, "finalize": {"on"}, "output_to": {"user"}}) // the form says what it showed
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	// Saving the old block stores the tag, as its form showed it.
	code, _, out = c.b.post("/w/o/r/workflows/pr/nodes/old", url.Values{"csrf": {c.csrf}, "version": {itoa(int64(c.version()))},
		"name": {"Value Proposition"}, "entry": {"x"}, "finalize": {"on"}, "output_to": {"value_proposition"}})
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	if got := workflow.StoredOutputTo(c.storedGraph().Nodes[2]); got != "value_proposition" {
		t.Errorf("stored %q", got)
	}
}

func (c *contEnv) version() int {
	w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
	return w.Version
}

// A new block gets its tag from its name when it is placed, and again when its settings are first saved
// with the box empty; the second of a name gets _2; renaming the block keeps its tag; and a tag another
// block has is refused.
func TestNewBlocksNameTheirOutputAndRenamingKeepsIt(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveGraph(events)
	nodes := []map[string]any{{"id": "ev1", "type": "pr_ready"}, {"id": "ev2", "type": "pr_activity"},
		{"id": "one", "type": "case_entry", "x": 1, "y": 1}, {"id": "two", "type": "case_entry", "x": 2, "y": 2}, {"id": "ag", "type": "agent", "x": 3, "y": 3}}
	code, answer := c.layout(nodes, []workflow.Link{})
	if code != 200 {
		t.Fatalf("layout %d %v", code, answer)
	}
	// The cards show the tag.
	sums := answer["summaries"].(map[string]any)
	if sums["one"] != "Appends a finalized entry · → {case_entry}" || sums["two"] != "Appends a finalized entry · → {case_entry_2}" || sums["ag"] != "Choose a model · → {agent}" {
		t.Errorf("the cards say %v", sums)
	}
	for id, want := range map[string]string{"one": "case_entry", "two": "case_entry_2", "ag": "agent"} {
		if got := c.formTag(id); got != want {
			t.Errorf("%s: the form shows %q, want %q", id, got, want)
		}
		var stored string
		for _, n := range c.storedGraph().Nodes {
			if n.ID == id {
				stored = workflow.StoredOutputTo(n)
			}
		}
		if stored != want {
			t.Errorf("%s: stored %q, want %q", id, stored, want)
		}
	}
	// Deleting the first block does not change the second's tag.
	if code, _ := c.layout(append(append([]map[string]any{}, nodes[:2]...), nodes[3:]...), []workflow.Link{}); code != 200 {
		t.Fatal(code)
	}
	if got := c.formTag("two"); got != "case_entry_2" {
		t.Errorf("after deleting the first: %q", got)
	}
	save := func(id string, form url.Values) (int, string) {
		form.Set("csrf", c.csrf)
		form.Set("version", itoa(int64(c.version())))
		form.Set("entry", "text")
		form.Set("finalize", "on")
		code, _, body := c.b.post("/w/o/r/workflows/pr/nodes/"+id, form)
		return code, body
	}
	// Renamed in the settings, with the box as the form showed it: the tag stays.
	if code, body := save("two", url.Values{"name": {"Second entry"}, "output_to": {"case_entry_2"}}); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if got := c.formTag("two"); got != "case_entry_2" {
		t.Errorf("after the rename the form shows %q", got)
	}
	var renamed workflow.Node
	for _, n := range c.storedGraph().Nodes {
		if n.ID == "two" {
			renamed = n
		}
	}
	if renamed.Name != "Second entry" || workflow.StoredOutputTo(renamed) != "case_entry_2" {
		t.Errorf("renamed: %q with %q", renamed.Name, workflow.StoredOutputTo(renamed))
	}
	// An empty box takes the name's slug.
	agentForm := func(tag string) url.Values {
		return url.Values{"name": {"Value Proposition"}, "output_to": {tag}, "persona": {"custom"}, "instruction": {"x"}, "goal": {"y"},
			"max_turns": {"5"}, "max_minutes": {"5"}, "rerun": {"resume"}, "model": {itoa(c.prov.ID) + ":m"}}
	}
	if code, body := save("ag", agentForm("")); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if got := c.formTag("ag"); got != "value_proposition" {
		t.Errorf("an empty box: %q", got)
	}
	// Another block's tag, and a built-in one, are refused with a message, and nothing is saved.
	for tag, want := range map[string]string{"case_entry_2": "already the output of Second entry", "pr_title": "built-in", "input": "built-in", "case": "built-in"} {
		code, body := save("ag", agentForm(tag))
		if code != 400 || !strings.Contains(body, want) {
			t.Errorf("%s: %d, want %q:\n%s", tag, code, want, body)
		}
	}
	if got := c.formTag("ag"); got != "value_proposition" {
		t.Errorf("a refused save changed the tag to %q", got)
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
		{"d", "Delta", "[[gamma]]", "{gamma} comes from Gamma, which does not run before this block"},
		{"c", "Gamma", "see {beta}", "{beta} comes from Beta, which does not run before this block"},
		{"a", "Alpha", "{delta}", "{delta} comes from Delta, which does not run before this block"},
		{"d", "Delta", "{delta}", "{delta} is not a tag here"}, // its own output is not upstream, and is nobody else's to name
		{"d", "Delta", "{nosuch}", "{nosuch} is not a tag here"},
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
	if !strings.Contains(d, "<code>{alpha}</code>") || !strings.Contains(d, "<code>{beta}</code>") || strings.Contains(d, "<code>{gamma}</code>") || strings.Contains(d, "<code>{delta}</code>") {
		t.Errorf("the Tags list of D:\n%s", d)
	}
	cc := tagsOf("c")
	if !strings.Contains(cc, "<code>{alpha}</code>") || strings.Contains(cc, "<code>{beta}</code>") || strings.Contains(cc, "<code>{delta}</code>") {
		t.Errorf("the Tags list of C:\n%s", cc)
	}
	// Removing the link B → D leaves D, which uses {alpha} and {beta}, with nothing upstream: the layout saves, and D is marked.
	save("d", "Delta", "{alpha} and {beta}")
	var nodes []map[string]any
	for _, n := range c.storedGraph().Nodes {
		nodes = append(nodes, map[string]any{"id": n.ID, "type": n.Type, "x": n.X, "y": n.Y})
	}
	code, answer := c.layout(nodes, []workflow.Link{{From: "ev1", Out: "out", To: "a", In: "in"}, {From: "a", Out: "out", To: "b", In: "in"}, {From: "a", Out: "out", To: "c", In: "in"}})
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
