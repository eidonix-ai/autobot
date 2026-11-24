package workflow

import (
	"strings"
	"testing"
)

// [[tag]] makes a link whose label comes from the tag, [[tag|Label]] one with the label given; an empty
// label falls back to the derived one; [[[[ is the text [[.
func TestLinksInTemplates(t *testing.T) {
	text := "See [[discovery.report]], [[pr:diff]], [[discovery.report|Intake report]] and [[case|Case File]], [[discovery_notes| ]] and [[[[not a link]]"
	var got []string
	for _, l := range TemplateLinks(text) {
		got = append(got, l.Name+"="+l.Label)
	}
	want := "discovery.report=Discovery report;pr:diff=Pr diff;discovery.report=Intake report;case=Case File;discovery_notes=Discovery notes"
	if strings.Join(got, ";") != want {
		t.Errorf("links\n got %s\nwant %s", strings.Join(got, ";"), want)
	}
	if got := LinkLabel("case"); got != "Case file" {
		t.Errorf("the case file's label %q", got)
	}
	// Links are not tags, and braces are not links.
	if tags := TemplateTags(text + " {gather}"); len(tags) != 1 || tags[0] != "gather" {
		t.Errorf("tags %v", tags)
	}
	// Render puts in the link made for the text as written, and keeps what has none; [[[[ is [[.
	r := Render("[[a.b|L]] [[a.b]] [[x]] [[[[y]] {{z}}", map[string]string{"[[a.b|L]]": "[L](https://s/p/1.html)", "[[a.b]]": "[A b](https://s/p/2.html)"})
	if r != "[L](https://s/p/1.html) [A b](https://s/p/2.html) [[x]] [[y]] {z}" {
		t.Errorf("rendered %q", r)
	}
}

// A [[name]] is checked like a {name}: a typo is refused, even with a label, and a tag of a step that
// does not run before the block is refused with the step's name.
func TestLinksAreChecked(t *testing.T) {
	g := Graph{Nodes: []Node{{ID: "e", Type: "pr_ready"}, agentNode("a", "Discovery", "discovery"), agentNode("b", "Other", "other"), agentNode("c", "Later", "later")},
		Links: []Link{{"e", "out", "a", "in"}, {"a", "out", "c", "in"}, {"e", "out", "b", "in"}}}
	for tpl, want := range map[string]string{
		"[[discovery.reprot]]":               "[[discovery.reprot]]: an Agent's parts are summary, report and scratchpad",
		"[[nosuch|Intake report]]":           "[[nosuch]]: no block is called Nosuch",
		"[[discovery.report|bad]] [[other]]": "[[other]] comes from Other, which does not run before this block",
		"[[pr:nope]]":                        "[[pr:nope]]: no such tag",
	} {
		err := g.CheckNodeTemplates(PR, "c", tpl)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", tpl, err, want)
		}
	}
	if err := g.CheckNodeTemplates(PR, "c", "[[discovery]] [[discovery.report|Intake report]] [[pr:diff]] [[case|Case File]] [[input.report]] [[[[x]]"); err != nil {
		t.Errorf("good links: %v", err)
	}
}
