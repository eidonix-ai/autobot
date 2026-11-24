package workflow

import (
	"strings"
	"testing"
)

func decisionNode(id, name string) Node {
	return Node{ID: id, Type: Decision, Name: name}
}

// ev → Decision → Uses, ev → Sibling, and Later after Uses; Post and Value are blocks that keep no tag.
func problemGraph() Graph {
	return Graph{Nodes: []Node{
		{ID: "ev", Type: "pr_ready"}, decisionNode("dc", "Decision"), entryNode("us", "Uses", ""),
		entryNode("sb", "Sibling", ""), entryNode("lt", "Later", ""), {ID: "po", Type: Post, Name: "Value"},
	}, Links: []Link{{"ev", "out", "dc", "in"}, {"dc", "out", "us", "in"}, {"ev", "out", "sb", "in"}, {"us", "out", "lt", "in"}}}
}

// Three things can be wrong with a tag, and each says which: no block has that name, the block does not run
// before this one, or the part is not one the block's output has. The message names the tag as it is written.
func TestTemplateProblemsSayWhatIsWrong(t *testing.T) {
	g := problemGraph()
	for _, c := range []struct{ id, text, want string }{
		// (a) no such tag: the base name turned back into words, or "no such tag" for a built-in looking name.
		{"us", "[[value_proposition.report]]", "[[value_proposition.report]]: no block is called Value proposition"},
		{"us", "see {nosuch}", "{nosuch}: no block is called Nosuch"},
		{"us", "[[pr:nope]]", "[[pr:nope]]: no such tag"},
		{"us", "{issue}", "{issue}: no such tag"},
		{"us", "{us}", "{us}: no block is called Us"}, // the name is Uses, so this tag is nobody's
		{"us", "{uses}", "{uses}: this block's own output, which exists only once it has run"},
		{"us", "[[value]]", "[[value]]: Value is a Post block, which keeps nothing under a tag"},
		// (b) a block that does not run before this one.
		{"us", "{sibling}", "{sibling} comes from Sibling, which does not run before this block"},
		{"dc", "[[later.report]]", "[[later.report]] comes from Later, which does not run before this block"},
		// (c) a part the output does not have.
		{"lt", "{decision.output}", "{decision.output}: a Decision's parts are confidence and reason"},
		{"lt", "[[decision.verdict]]", "[[decision.verdict]]: a Decision's parts are confidence and reason"},
	} {
		err := g.CheckNodeTemplates(PR, c.id, c.text)
		if err == nil || err.Error() != c.want {
			t.Errorf("%s with %s: %v, want %q", c.id, c.text, err, c.want)
		}
	}
	g.Nodes[2] = agentNode("us", "Uses", "")
	if err := g.CheckNodeTemplates(PR, "lt", "{uses.report} {uses.notes}"); err == nil || err.Error() != "{uses.notes}: an Agent's parts are summary, report and scratchpad" {
		t.Errorf("an agent's parts: %v", err)
	}
	// The Condition's tag says the same.
	if err := g.CheckOutputRef(PR, "lt", "value_summary"); err == nil || err.Error() != "{value_summary}: no block is called Value summary" {
		t.Errorf("a Condition's tag: %v", err)
	}
	if err := g.CheckOutputRef(PR, "us", "sibling"); err == nil || !strings.Contains(err.Error(), "comes from Sibling") {
		t.Errorf("a Condition's tag of a block that does not run before: %v", err)
	}
}

// A Markdown link wrapped around a [[ ]] link is a mistake: [[x]] is the whole link.
func TestLinkInsideALinkIsRefused(t *testing.T) {
	g := problemGraph()
	for text, want := range map[string]string{
		"[Label]([[decision]])":         "[[decision]] already makes a link: write [[decision|Label]] instead",
		"see [the report]([[value.x]])": "[[value.x]] already makes a link: write [[value.x|Label]] instead",
		"[Label]([[decision|Other]])":   "[[decision]] already makes a link: write [[decision|Label]] instead",
	} {
		if err := g.CheckNodeTemplates(PR, "lt", text); err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", text, err, want)
		}
		if err := CheckTemplates(PR, nil, text); err == nil || err.Error() != want {
			t.Errorf("with no workflow, %s: %v", text, err)
		}
	}
	// Not that: a link of its own, a brace tag in a link, text that is not a link, an escaped [[.
	for _, ok := range []string{"[[decision|Label]]", "[Label]({pr})", "[Label](https://x.test/[[[[y]])", "see ]([[ plain", "[a](b) [[decision]]"} {
		if err := g.CheckNodeTemplates(PR, "lt", ok); err != nil && strings.Contains(err.Error(), "already makes a link") {
			t.Errorf("%s was flagged: %v", ok, err)
		}
	}
}
