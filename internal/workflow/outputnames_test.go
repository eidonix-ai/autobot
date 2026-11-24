package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

// A block's output tag comes from its name: lower case, anything but letters and digits one _,
// trimmed, at most 40 characters, and starting with a letter.
func TestSlug(t *testing.T) {
	for name, want := range map[string]string{
		"Value Proposition":       "value_proposition",
		"  Gather -- facts! ":     "gather_facts",
		"Agent":                   "agent",
		"2nd pass":                "step_2nd_pass",
		"???":                     "",
		"Ünïcode café":            "n_code_caf",
		strings.Repeat("a", 60):   strings.Repeat("a", 40),
		strings.Repeat("ab ", 20): "ab_ab_ab_ab_ab_ab_ab_ab_ab_ab_ab_ab_ab_a",
	} {
		if got := Slug(name); got != want {
			t.Errorf("Slug(%q) = %q, want %q", name, got, want)
		}
	}
}

func agentNode(id, name, outputTo string) Node {
	n := Node{ID: id, Type: Agent, Name: name}
	if outputTo != "" {
		n.Settings = json.RawMessage(`{"output_to":"` + outputTo + `"}`)
	}
	return n
}

// A name that is taken (by another block's output, a built-in tag or an old built-in name) gets _2,
// _3 …; a block with no tag saved behaves as if it had its name's.
func TestOutputNamesAndCollisions(t *testing.T) {
	g := Graph{Nodes: []Node{
		{ID: "e", Type: "pr_ready"},
		agentNode("a", "Value Proposition", "value_proposition"), // saved
		agentNode("b", "Value Proposition", ""),                  // saved before tags: its name's, taken
		agentNode("c", "Value Proposition", ""),
		agentNode("d", "Input", ""),    // a built-in tag
		agentNode("f", "pr title", ""), // not a built-in name: pr_title is an old one
		agentNode("h", "Pr_Title", ""), // pr_title, an old built-in name
		{ID: "i", Type: Decision, Name: "Decide"},
		{ID: "j", Type: Approval}, // Human Approval
	}}
	want := map[string]string{"a": "value_proposition", "b": "value_proposition_2", "c": "value_proposition_3",
		"d": "input_2", "f": "pr_title_2", "h": "pr_title_3", "i": "decide", "j": "human_approval"}
	got := g.OutputNames(PR)
	for id, tag := range want {
		if got[id] != tag {
			t.Errorf("%s: %q, want %q", id, got[id], tag)
		}
	}
	if _, ok := got["e"]; ok {
		t.Error("an event block has an output tag")
	}
	// A new block takes the free name.
	if tag := g.FreeOutputTag(PR, "value_proposition", "zz"); tag != "value_proposition_4" {
		t.Errorf("free tag %q", tag)
	}
	if tag := g.FreeOutputTag(PR, "case", "zz"); tag != "case_2" {
		t.Errorf("built-in {case}: %q", tag)
	}
	// The suffix fits in the limit.
	long := strings.Repeat("x", 40)
	g2 := Graph{Nodes: []Node{agentNode("a", "", long)}}
	if tag := g2.FreeOutputTag(PR, long, "zz"); len(tag) > 40 || !strings.HasSuffix(tag, "_2") {
		t.Errorf("long tag %q", tag)
	}
}

// Every kind of step that produces something has an output tag, an old block without one gets its
// name's slug, and a stored one wins over the name.
func TestEveryStepHasAnOutputTag(t *testing.T) {
	g := Graph{Nodes: []Node{{ID: "e", Type: "pr_ready"}}}
	for _, typ := range []string{Agent, Decision, CaseEntry, Condition, TestRun, Approval} {
		if !HasOutput(typ) {
			t.Errorf("%s has no output", typ)
		}
		g.Nodes = append(g.Nodes, Node{ID: typ, Type: typ})
	}
	// The block that posts is an endpoint: no tag.
	for _, typ := range []string{Post} {
		if HasOutput(typ) {
			t.Errorf("%s has an output", typ)
		}
		g.Nodes = append(g.Nodes, Node{ID: typ, Type: typ})
	}
	got := g.OutputNames(PR)
	for typ, want := range map[string]string{Agent: "agent", Decision: "decision", CaseEntry: "case_entry",
		Condition: "condition", TestRun: "test_run", Approval: "human_approval", Post: ""} {
		if got[typ] != want {
			t.Errorf("%s: %q, want %q", typ, got[typ], want)
		}
	}
	// A renamed block keeps the tag saved on it.
	n, err := WithOutputTo(Node{ID: "x", Type: CaseEntry, Name: "Old name"}, PR, "old_name")
	if err != nil {
		t.Fatal(err)
	}
	n.Name = "Completely different"
	if got := (Graph{Nodes: []Node{n}}).OutputNames(PR)["x"]; got != "old_name" {
		t.Errorf("after the rename: %q", got)
	}
	if s, err := CaseEntrySettingsOf(n); err != nil || s.Entry != DefaultCaseEntry().Entry || s.OutputTo != "old_name" {
		t.Errorf("a block with no settings gets the defaults with its tag: %+v %v", s, err)
	}
}

// A tag another block has is refused, as are a built-in tag and a bad name.
func TestCheckOutputTag(t *testing.T) {
	g := Graph{Nodes: []Node{agentNode("a", "Discovery", "discovery"), agentNode("b", "Other", "other")}}
	for tag, want := range map[string]string{"discovery": "already the output of Discovery", "pr_title": "built-in", "case": "built-in",
		"input": "built-in", "Bad Name": "lower-case word"} {
		err := g.CheckOutputTag(PR, g.Nodes[1], tag)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", tag, err, want)
		}
	}
	if err := g.CheckOutputTag(PR, g.Nodes[1], "other"); err != nil {
		t.Errorf("a block's own tag: %v", err)
	}
	if err := g.CheckOutputTag(PR, g.Nodes[1], "fresh"); err != nil {
		t.Errorf("a free tag: %v", err)
	}
}

// The canvas card shows the effective tag, with whatever else it shows.
func TestSummaryShowsTheOutputTag(t *testing.T) {
	models := func(m ModelRef) string { return "m" }
	for _, c := range []struct {
		n    Node
		want string
	}{
		{Node{Type: Agent, Settings: json.RawMessage(`{"model":{"provider":1,"model":"x"}}`)}, "m · → {gather}"},
		{Node{Type: Approval}, "Waits up to 24 hours · → {gather}"},
		{Node{Type: CaseEntry}, "Appends a finalized entry · → {gather}"},
		{Node{Type: Condition}, "Turn on a rule · → {gather}"},
		{Node{Type: Post}, "GitHub (trial)"}, // no tag, so no arrow
	} {
		tag := "gather"
		if !HasOutput(c.n.Type) {
			tag = ""
		}
		if got := Summary(c.n, PR, tag, "", models); got != c.want {
			t.Errorf("%s: %q, want %q", c.n.Type, got, c.want)
		}
	}
	if got := Summary(Node{Type: "pr_ready"}, PR, "", "", models); strings.Contains(got, "→") {
		t.Errorf("an event block: %q", got)
	}
}
