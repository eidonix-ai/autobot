package workflow

import (
	"slices"
	"strings"
	"testing"
)

func link(from, to string) Link { return Link{From: from, Out: "out", To: to, In: "in"} }

// A Node is offered in the palette of both kinds as "Node", is named Node, Node 2 … like any block,
// and its card says what it is.
func TestNodeBlockIsOfferedAndNamed(t *testing.T) {
	for _, kind := range Kinds {
		var labels []string
		for _, ty := range TypesFor(kind) {
			labels = append(labels, ty.Label)
		}
		if !slices.Contains(labels, "Node") {
			t.Errorf("the %s palette has no Node: %v", kind, labels)
		}
	}
	g := Graph{Nodes: []Node{{ID: "e", Type: "pr_ready"}, {ID: "j1", Type: Junction}}}
	if got := g.DefaultName(Junction, ""); got != "Node 2" {
		t.Errorf("the second Node is named %q", got)
	}
	if got := (Graph{}).DefaultName(Junction, ""); got != "Node" {
		t.Errorf("the first Node is named %q", got)
	}
	if got := Summary(Node{ID: "j", Type: Junction}, PR, "", "", nil); got != "connects links" {
		t.Errorf("the card says %q", got)
	}
	if got := Outputs(Node{Type: Junction}); !slices.Equal(got, []string{"out"}) {
		t.Errorf("outputs %v", got)
	}
	if got := Inputs(Node{Type: Junction}); !slices.Equal(got, []string{"in"}) {
		t.Errorf("inputs %v", got)
	}
}

// Any number of links may come into a Node and leave it; it keeps no tag and is not in the Tags list;
// a block after it may use the tags of everything upstream of the Node.
func TestNodeBlockIsAWire(t *testing.T) {
	g := Graph{Nodes: []Node{
		{ID: "e", Type: "pr_ready"},
		agentNode("a", "Alpha", ""), agentNode("b", "Beta", ""), agentNode("c", "Gamma", ""),
		{ID: "j", Type: Junction},
		agentNode("x", "X", ""), agentNode("y", "Y", ""), agentNode("z", "Z", ""),
	}, Links: []Link{link("e", "a"), link("e", "b"), link("e", "c"),
		link("a", "j"), link("b", "j"), link("c", "j"), link("j", "x"), link("j", "y"), link("j", "z")}}
	if err := g.Check(PR); err != nil {
		t.Fatalf("three links in and three out: %v", err)
	}
	if err := g.Check(Issue); err == nil {
		t.Fatal("a PR event on an issue workflow")
	}
	names := g.OutputNames(PR)
	if tag, ok := names["j"]; ok || len(names) != 6 {
		t.Errorf("a Node has a tag %q: %v", tag, names)
	}
	// A block after the Node uses the tags of Alpha, Beta and Gamma, though no link joins it to them.
	if err := g.CheckNodeTemplates(PR, "x", "{alpha} {beta.report} {gamma.summary} {input}"); err != nil {
		t.Errorf("tags upstream of the Node: %v", err)
	}
	tags := g.OutputTags(PR, "x")
	if len(tags) != 3 || tags["alpha"] != Agent || tags["beta"] != Agent || tags["gamma"] != Agent {
		t.Errorf("the tags after a Node: %v", tags)
	}
	// A sibling after the Node is not upstream; the Node's own name is no tag.
	if err := g.CheckNodeTemplates(PR, "x", "{y}"); err == nil || !strings.Contains(err.Error(), "does not run before") {
		t.Errorf("a sibling's tag: %v", err)
	}
	if err := g.CheckNodeTemplates(PR, "x", "{node}"); err == nil {
		t.Error("a Node's name is a tag")
	}
	for _, tag := range OutputTagList(tags) {
		if tag.Name == "node" || tag.Name == "j" {
			t.Errorf("the Tags list has %q", tag.Name)
		}
	}
	// Nothing is stored on it as a tag.
	n, err := WithOutputTo(Node{ID: "j", Type: Junction}, PR, "node")
	if err != nil || len(n.Settings) != 0 {
		t.Errorf("an output tag was stored on a Node: %s %v", n.Settings, err)
	}
	// And a Node can still not loop.
	loop := Graph{Nodes: []Node{{ID: "e", Type: "pr_ready"}, {ID: "j", Type: Junction}, agentNode("a", "A", "")},
		Links: []Link{link("e", "j"), link("j", "a"), link("a", "j")}}
	if err := loop.Check(PR); err == nil || !strings.Contains(err.Error(), "loop") {
		t.Errorf("a loop through a Node: %v", err)
	}
}
