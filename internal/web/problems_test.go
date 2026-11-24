package web

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/workflow"
)

// problemBlocks are blocks saved with text the rules refuse today, as an older canvas may have kept it:
// each kind of mistake once.
func (c *contEnv) problemBlocks() []workflow.Node {
	post := workflow.DefaultPost()
	post.Message = "Done: [[value.report]]"
	return append(append([]workflow.Node{}, events...),
		node("dc", workflow.Decision, "Decision", workflow.DefaultDecision(workflow.PR)),
		caseEntryNode("us", "Uses", "{decision.output} and {decision.reason}"),
		node("gc", workflow.Post, "GitHub Comment", post),
		caseEntryNode("lk", "Linker", "[Label]([[decision]])"),
		caseEntryNode("ea", "Early", "{late}"),
		caseEntryNode("la", "Late", "fine"),
		caseEntryNode("ok", "Fine", "{decision} {pr:title} [[decision.reason|why]]"))
}

// saveProblemGraph stores them with their links, from the Decision's first choice.
func (c *contEnv) saveProblemGraph() {
	c.t.Helper()
	g := workflow.Graph{Nodes: c.problemBlocks(), Links: []workflow.Link{}}
	choice := workflow.Outputs(g.Nodes[2])[0]
	for _, l := range problemLinks {
		out := "out"
		if l[0] == "dc" {
			out = choice
		}
		g.Links = append(g.Links, workflow.Link{From: l[0], Out: out, To: l[1], In: "in"})
	}
	b, _ := json.Marshal(g)
	w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
	if _, err := c.st.SaveWorkflow(c.ws.ID, workflow.PR, w.Version, string(b), c.admin); err != nil {
		c.t.Fatal(err)
	}
}

var problemLinks = [][2]string{{"ev1", "dc"}, {"dc", "us"}, {"ev1", "gc"}, {"ev1", "lk"}, {"dc", "lk"}, {"ev1", "ea"}, {"ea", "la"}, {"dc", "ok"}}

var wantProblems = map[string]string{
	"gc": "GitHub Comment: [[value.report]]: no block is called Value",
	"us": "Uses: {decision.output}: a Decision's parts are confidence and reason",
	"lk": "Linker: [[decision]] already makes a link: write [[decision|Label]] instead",
	"ea": "Early: {late} comes from Late, which does not run before this block",
}

// Text saved before the rules existed is checked when the canvas loads and on every save of the layout:
// the blocks are flagged, never refused, each with a message that names the block, the tag and the reason.
func TestProblemsNameTheBlockTheTagAndTheReason(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveProblemGraph()
	// On load.
	code, page := c.b.get("/w/o/r/workflows/pr")
	if code != 200 {
		t.Fatalf("the canvas: %d", code)
	}
	for _, want := range []string{"GitHub Comment: [[value.report]]: no block is called Value", "Early: {late} comes from Late, which does not run before this block",
		"Linker: [[decision]] already makes a link: write [[decision|Label]] instead", "parts are confidence and reason"} {
		if !strings.Contains(page, want) {
			t.Errorf("the canvas does not carry %q when it loads", want)
		}
	}
	if strings.Contains(page, "Fine: ") {
		t.Error("a block with good text was flagged on load")
	}
	// On every save of the layout, which still goes through.
	var nodes []map[string]any
	for _, n := range c.storedGraph().Nodes {
		nodes = append(nodes, map[string]any{"id": n.ID, "type": n.Type, "x": n.X + 1, "y": n.Y})
	}
	var links []workflow.Link
	for _, l := range problemLinks {
		out := "out"
		if l[0] == "dc" { // a Decision's outputs are its choices
			out = workflow.Outputs(c.storedNodeByID("dc"))[0]
		}
		links = append(links, workflow.Link{From: l[0], Out: out, To: l[1], In: "in"})
	}
	code, answer := c.layout(nodes, links)
	if code != 200 {
		t.Fatalf("the layout was refused: %d %v", code, answer)
	}
	problems, _ := answer["problems"].(map[string]any)
	if len(problems) != len(wantProblems) {
		t.Errorf("problems: %v", problems)
	}
	for id, want := range wantProblems {
		if got, _ := problems[id].(string); got != want {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
	}
	// A card shows what is wrong without the block's name, which it has above it.
	summaries := answer["summaries"].(map[string]any)
	if got := summaries["gc"]; got != "⚠ [[value.report]]: no block is called Value" {
		t.Errorf("the card of the Post says %q", got)
	}
	if got, _ := summaries["ok"].(string); strings.HasPrefix(got, "⚠") {
		t.Errorf("the card of a good block says %q", got)
	}
}

// In the dialog a refused save says the same without the block's name, and a link inside a link is refused
// with how to write it.
func TestDialogRefusesAndNamesTheProblem(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveProblemGraph()
	save := func(id, name, entry string) (int, string) {
		code, _, body := c.b.post("/w/o/r/workflows/pr/nodes/"+id, url.Values{"csrf": {c.csrf}, "version": {itoa(int64(c.version()))},
			"name": {name}, "entry": {entry}, "finalize": {"on"}})
		return code, strings.ReplaceAll(body, "&#39;", "'")
	}
	for _, bad := range []struct{ id, name, entry, want string }{
		{"us", "Uses", "[[value.report]]", "[[value.report]]: no block is called Value"},
		{"us", "Uses", "{decision.output}", "{decision.output}: a Decision's parts are confidence and reason"},
		{"ea", "Early", "{late}", "{late} comes from Late, which does not run before this block"},
		{"us", "Uses", "see [the reason]([[decision.reason]])", "[[decision.reason]] already makes a link: write [[decision.reason|Label]] instead"},
		{"us", "Uses", "[Label]([[decision|Other]])", "[[decision]] already makes a link: write [[decision|Label]] instead"},
	} {
		if code, body := save(bad.id, bad.name, bad.entry); code != 400 || !strings.Contains(body, bad.want) {
			t.Errorf("%s with %s: %d, want %q\n%s", bad.id, bad.entry, code, bad.want, body)
		}
	}
	if code, body := save("us", "Uses", "[[decision.reason|Label]] and [Label](https://example.test)"); code != 200 {
		t.Errorf("a link of its own: %d\n%s", code, body)
	}
}

func (c *contEnv) storedNodeByID(id string) workflow.Node {
	c.t.Helper()
	for _, n := range c.storedGraph().Nodes {
		if n.ID == id {
			return n
		}
	}
	c.t.Fatalf("no node %s", id)
	return workflow.Node{}
}
