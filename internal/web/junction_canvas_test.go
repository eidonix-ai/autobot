package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/workflow"
)

// A Node placed on the canvas is named Node, then Node 2; its card says what it is; its dialog has a
// name and nothing else, and a rename saves; it has no test.
func TestNodeOnTheCanvas(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveGraph(events)
	nodes := []map[string]any{{"id": "ev1", "type": "pr_ready"}, {"id": "ev2", "type": "pr_activity"},
		{"id": "j1", "type": "junction", "x": 1, "y": 1}, {"id": "j2", "type": "junction", "x": 2, "y": 2}}
	links := []workflow.Link{{From: "ev1", Out: "out", To: "j1", In: "in"}, {From: "ev2", Out: "out", To: "j1", In: "in"}, {From: "j1", Out: "out", To: "j2", In: "in"}}
	code, answer := c.layout(nodes, links)
	if code != 200 {
		t.Fatalf("layout %d %v", code, answer)
	}
	names, sums := answer["names"].(map[string]any), answer["summaries"].(map[string]any)
	if names["j1"] != "Node" || names["j2"] != "Node 2" || sums["j1"] != "connects links" {
		t.Errorf("names %v, cards %v", names, sums)
	}
	if len(c.storedGraph().Links) != 3 {
		t.Errorf("links stored: %+v", c.storedGraph().Links)
	}
	if code, page := c.b.get("/w/o/r/workflows/pr"); code != 200 || !strings.Contains(page, `&#34;type&#34;:&#34;junction&#34;,&#34;kind&#34;:&#34;&#34;,&#34;label&#34;:&#34;Node&#34;`) {
		t.Errorf("the canvas does not offer Node: %d", code)
	}
	code, form := c.b.get("/w/o/r/workflows/pr/nodes/j1")
	if code != 200 || !strings.Contains(form, `name="name"`) || strings.Contains(form, "<textarea") || strings.Contains(form, "output_to") {
		t.Errorf("the dialog: %d\n%s", code, form)
	}
	post := func(name string) (int, string) {
		code, _, body := c.b.post("/w/o/r/workflows/pr/nodes/j1", url.Values{"csrf": {c.csrf}, "version": {itoa(int64(c.version()))}, "name": {name}})
		return code, body
	}
	if code, body := post("Where the reviews meet"); code != 200 {
		t.Fatalf("rename: %d %s", code, body)
	}
	if code, body := post("Node 2"); code != 400 || !strings.Contains(body, "another block is already named Node 2") {
		t.Errorf("a name that is another block's: %d %s", code, body)
	}
	for _, n := range c.storedGraph().Nodes {
		if n.ID == "j1" && n.Name != "Where the reviews meet" {
			t.Errorf("stored %+v", n)
		}
	}
	if code, body := c.b.get("/w/o/r/workflows/pr/nodes/j1/test"); !strings.Contains(body, "nothing to test") {
		t.Errorf("the test of a Node: %d %s", code, body)
	}
}
