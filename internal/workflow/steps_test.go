package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTemplates(t *testing.T) {
	tpl := `Review {pr_link}. Answer as {"verdict": "..."}; write {{pr_link}} for the text. {value_summary}`
	if got := TemplateTags(tpl); strings.Join(got, ",") != "pr_link,value_summary" {
		t.Fatalf("tags %v", got)
	}
	got := Render(tpl, map[string]string{"pr_link": "https://x/1", "value_summary": "fine"})
	if got != `Review https://x/1. Answer as {"verdict": "..."}; write {pr_link} for the text. fine` {
		t.Fatalf("render %q", got)
	}
	// A tag must be built in, or set by another step.
	if err := CheckTemplates(PR, nil, tpl); err == nil || !strings.Contains(err.Error(), "{value_summary}") {
		t.Fatalf("unknown tag: %v", err)
	}
	if err := CheckTemplates(PR, map[string]bool{"value_summary": true}, tpl); err != nil {
		t.Fatal(err)
	}
	// Issue workflows have no pull request.
	if CheckTemplates(Issue, nil, "{pr_diff}") == nil {
		t.Fatal("an issue template used a pull request tag")
	}
}

func TestStepSettings(t *testing.T) {
	a := DefaultAgent(PR)
	if a.Check(PR) == nil {
		t.Fatal("an agent with no model passed")
	}
	a.Model = ModelRef{Provider: 1, Model: "m"}
	if err := a.Check(PR); err != nil {
		t.Fatal(err)
	}
	if err := CheckTemplates(PR, nil, a.SystemPrompt, a.Instruction); err != nil {
		t.Fatalf("the default agent's own templates: %v", err)
	}
	a.OutputTo = "pr_diff"
	if a.Check(PR) == nil {
		t.Fatal("an output tag named after a built-in tag passed")
	}
	d := DecisionFromForm(func(k string) string {
		return map[string]string{"question": "Ship it?", "output": "probabilities",
			"choices": "approve: safe to merge\n\nrequest-changes: needs work\nhold"}[k]
	})
	d.Model = ModelRef{Provider: 1, Model: "m"}
	if err := d.Check(PR); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d)
	n := Node{ID: "d", Type: Decision, Settings: b}
	if got := strings.Join(Outputs(n), ","); got != "approve,request-changes,hold" {
		t.Fatalf("outputs %s", got)
	}
	d.Choices = append(d.Choices, Choice{Name: "hold"})
	if d.Check(PR) == nil {
		t.Fatal("two choices with one name passed")
	}
	// Links come from the choices; one from a choice the decision does not have is refused.
	g := Graph{Nodes: []Node{{ID: "e", Type: "pr_ready"}, n, {ID: "a", Type: Agent}},
		Links: []Link{{"e", "out", "d", "in"}, {"d", "approve", "a", "in"}}}
	if err := g.Check(PR); err != nil {
		t.Fatal(err)
	}
	g.Links = append(g.Links, Link{"d", "yes", "a", "in"})
	if g.Check(PR) == nil {
		t.Fatal("a link from a choice the decision lacks passed")
	}
	// Other steps' output tags are known; a node's own is not offered back to it.
	a.OutputTo = "value_summary"
	ab, _ := json.Marshal(a)
	g.Nodes[2].Settings = ab
	if !g.OutputTags(PR, "d")["value_summary"] || g.OutputTags(PR, "a")["value_summary"] {
		t.Fatal("output tags")
	}
}
