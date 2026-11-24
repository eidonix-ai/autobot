package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/workflow"
)

// layout posts the canvas's nodes and links as the page does, and returns the answer.
func (c *contEnv) layout(nodes []map[string]any, links []workflow.Link) (int, map[string]any) {
	c.t.Helper()
	w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
	body, _ := json.Marshal(map[string]any{"version": w.Version, "nodes": nodes, "links": links})
	req, _ := http.NewRequest("POST", c.srv.URL+"/w/o/r/workflows/pr/layout", bytes.NewReader(body))
	req.Header.Set("X-Autobot-CSRF", c.csrf)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.b.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func (c *contEnv) storedNode(id string) workflow.AgentSettings {
	c.t.Helper()
	w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
	var g workflow.Graph
	json.Unmarshal([]byte(w.Graph), &g)
	for _, n := range g.Nodes {
		if n.ID == id {
			a, _ := workflow.AgentSettingsOf(n, workflow.PR)
			return a
		}
	}
	c.t.Fatalf("no node %s stored", id)
	return workflow.AgentSettings{}
}

// postNode saves a block's settings from its form, as the canvas's modal does.
func (c *contEnv) postNode(id string, form url.Values) (int, string) {
	c.t.Helper()
	w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
	form.Set("csrf", c.csrf)
	form.Set("version", itoa(int64(w.Version)))
	for k, v := range map[string]string{"name": "Rechecker", "persona": "custom", "instruction": "Recheck {pr_title}.", "goal": "done",
		"max_turns": "5", "max_minutes": "5", "rerun": "resume"} {
		if form.Get(k) == "" {
			form.Set(k, v)
		}
	}
	code, _, body := c.b.post("/w/o/r/workflows/pr/nodes/"+id, form)
	return code, body
}

// A block set to continue another's thread: its model starts as the other block's and may be
// changed; it names a block of its canvas that starts threads of its own; and the canvas says whose
// it continues, including when that block has been deleted, which never stops the canvas saving.
func TestReferenceBlocks(t *testing.T) {
	c := newContEnv(t, nil)
	model := workflow.ModelRef{Provider: c.prov.ID, Model: "m"}
	a := c.agentNode("aa", "Reviewer", nil)
	other := c.agentNode("oo", "Other", func(s *workflow.AgentSettings) { s.ContinueOf = "aa" })
	b := c.agentNode("bb", "Rechecker", nil)
	c.saveGraph(append(append([]workflow.Node{}, events...), a, other, b, workflow.Node{ID: "ee", Type: workflow.CaseEntry}))

	// Saved as a continuation of A with no model, it takes A's, and keeps the choice. With a model of
	// its own it keeps that, an ordinary setting; one the workspace may not use is refused.
	code, body := c.postNode("bb", url.Values{"continue_of": {"aa"}, "no_conversation": {"fresh"}, "model": {""}})
	if got := c.storedNode("bb"); code != 200 || got.ContinueOf != "aa" || !got.FreshWithoutConversation() || got.Model != model {
		t.Fatalf("save: %d %s\n%+v", code, body, got)
	}
	own := workflow.ModelRef{Provider: c.prov.ID, Model: "m2"}
	if code, body := c.postNode("bb", url.Values{"continue_of": {"aa"}, "no_conversation": {"fresh"}, "model": {modelValue(own)}}); code != 200 || c.storedNode("bb").Model != own {
		t.Fatalf("a model of its own: %d %s\n%+v", code, body, c.storedNode("bb"))
	}
	if code, _ := c.postNode("bb", url.Values{"continue_of": {"aa"}, "model": {"99:nonsense"}}); code != 400 {
		t.Fatalf("a model that is not the workspace's: %d", code)
	}
	if code, page := c.b.get("/w/o/r/workflows/pr/nodes/bb"); code != 200 || strings.Contains(page, "readonly data-source-model") ||
		!strings.Contains(page, `<select name="model" data-model>`) || !strings.Contains(page, `<option value="`+modelValue(own)+`" selected>m2 · LLM</option>`) ||
		!strings.Contains(page, `<option value="aa" data-model="`+modelValue(model)+`" selected>Reviewer</option>`) || strings.Contains(page, `value="oo"`) || strings.Contains(page, `value="bb"`) ||
		!strings.Contains(page, "If this item has no earlier report") {
		t.Fatalf("the form %d:\n%s", code, page)
	}
	// Not itself, not a block that continues another, not anything but an Agent block.
	for _, bad := range []string{"bb", "oo", "ee", "nowhere"} {
		code, body := c.postNode("bb", url.Values{"continue_of": {bad}})
		if code != 400 || !strings.Contains(body, "choose a block of this canvas that starts its own conversation") && !strings.Contains(body, "cannot continue its own") {
			t.Fatalf("continue_of %q: %d\n%s", bad, code, body)
		}
	}
	if code, body := c.postNode("bb", url.Values{"continue_of": {"aa"}, "no_conversation": {"sometimes"}}); code != 400 || !strings.Contains(body, "no earlier report") {
		t.Fatalf("no_conversation: %d\n%s", code, body)
	}
	if c.storedNode("bb").ContinueOf != "aa" {
		t.Fatal("a refused save changed the block")
	}

	// A new reference placed on the canvas: an Agent block that continues A, starting with A's model.
	nodes := []map[string]any{{"id": "aa", "type": "agent", "x": 1, "y": 1}, {"id": "bb", "type": "agent", "x": 2, "y": 2},
		{"id": "rr", "type": "agent", "x": 3, "y": 3, "continue_of": "aa"}, {"id": "evone", "type": "pr_ready"}}
	code, answer := c.layout(nodes, []workflow.Link{})
	if code != 200 || answer["continues"].(map[string]any)["rr"] != "continues Reviewer" || answer["continues"].(map[string]any)["bb"] != "continues Reviewer" ||
		answer["summaries"].(map[string]any)["rr"] != "→ {agent}" {
		t.Fatalf("layout %d: %v", code, answer)
	}
	if r := c.storedNode("rr"); r.ContinueOf != "aa" || r.Model != model || r.Persona == "" {
		t.Fatalf("the reference: %+v", r)
	}
	// A reference to a block that continues another, or that is not there, is refused.
	nodes = append(nodes, map[string]any{"id": "ss", "type": "agent", "continue_of": "rr"})
	if code, answer := c.layout(nodes, []workflow.Link{}); code != 400 {
		t.Fatalf("a reference to a reference: %d %v", code, answer)
	}

	// Deleting the block it continues: the save goes through, the reference says so, its form still opens
	// and saves, and the run fails plainly.
	nodes = []map[string]any{{"id": "bb", "type": "agent", "x": 2, "y": 2}, {"id": "rr", "type": "agent", "x": 3, "y": 3}, {"id": "evone", "type": "pr_ready"}}
	code, answer = c.layout(nodes, []workflow.Link{{From: "evone", Out: "out", To: "rr", In: "in"}})
	if code != 200 || answer["continues"].(map[string]any)["rr"] != "continues: (deleted)" {
		t.Fatalf("after deleting A: %d %v", code, answer)
	}
	if code, page := c.b.get("/w/o/r/workflows/pr/nodes/rr"); code != 200 || !strings.Contains(page, `<option value="aa" data-model="" selected>(deleted)</option>`) {
		t.Fatalf("the form of a reference to a deleted block %d:\n%s", code, page)
	}
	if code, body := c.postNode("rr", url.Values{"continue_of": {"aa"}, "name": {"Renamed"}}); code != 200 {
		t.Fatalf("saving a reference to a deleted block: %d\n%s", code, body)
	}
	steps := c.run("evone")
	if steps["rr"].Status != "failed" || !strings.Contains(steps["rr"].Error, "the block it continues is no longer on the canvas") || len(c.llm.requests()) != 0 {
		t.Fatalf("%+v", steps["rr"])
	}
}
