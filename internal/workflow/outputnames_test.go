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

// A block's tag is always its name in snake case: a tag saved on it is ignored (MigrateNames deals with
// such blocks once), and a name that is a built-in tag, or one a block has already, gets _2, _3 … only
// for a graph from before names were kept unique.
func TestOutputNamesAreTheNamesInSnakeCase(t *testing.T) {
	g := Graph{Nodes: []Node{
		{ID: "e", Type: "pr_ready", Name: "Starts"},
		agentNode("a", "Value Proposition", "something_else"), // the saved tag is ignored
		agentNode("b", "Value Proposition", ""),               // a graph from before: the same name twice
		agentNode("c", "", ""),                                // no name: its type's label
		agentNode("d", "Input", ""),                           // a built-in tag
		agentNode("h", "Pr_Title", ""),                        // an old built-in name
		{ID: "i", Type: Decision, Name: "Decide"},
		{ID: "j", Type: Approval},
	}}
	want := map[string]string{"a": "value_proposition", "b": "value_proposition_2", "c": "agent", "d": "input_2", "h": "pr_title_2", "i": "decide", "j": "human_approval"}
	got := g.OutputNames(PR)
	for id, tag := range want {
		if got[id] != tag {
			t.Errorf("%s: %q, want %q", id, got[id], tag)
		}
	}
	if _, ok := got["e"]; ok {
		t.Error("an event block has an output tag")
	}
	// The suffix fits in the limit.
	long := strings.Repeat("x", 45)
	g2 := Graph{Nodes: []Node{agentNode("a", long, ""), agentNode("b", long, "")}}
	if tag := g2.OutputNames(PR)["b"]; len(tag) > 40 || !strings.HasSuffix(tag, "_2") {
		t.Errorf("long tag %q", tag)
	}
}

// Every kind of step that produces something has a tag, and the blocks that post, and the events, none.
func TestEveryStepHasAnOutputTag(t *testing.T) {
	g := Graph{Nodes: []Node{{ID: "e", Type: "pr_ready"}}}
	for _, typ := range []string{Agent, Decision, CaseEntry, Condition, TestRun, Approval, Post} {
		g.Nodes = append(g.Nodes, Node{ID: typ, Type: typ})
	}
	got := g.OutputNames(PR)
	for typ, want := range map[string]string{Agent: "agent", Decision: "decision", CaseEntry: "case_entry",
		Condition: "condition", TestRun: "test_run", Approval: "human_approval", Post: ""} {
		if got[typ] != want || HasOutput(typ) != (want != "") {
			t.Errorf("%s: %q (has output %v), want %q", typ, got[typ], HasOutput(typ), want)
		}
	}
}

// A new block is named after its type, then with 2, 3 … where a block already has that name.
func TestDefaultNames(t *testing.T) {
	g := Graph{Nodes: []Node{{ID: "a", Type: Agent}}}
	for want, graph := range map[string]Graph{
		"Agent":   {},
		"Agent 2": g,
		"Agent 3": {Nodes: []Node{{ID: "a", Type: Agent}, {ID: "b", Type: Agent, Name: "Agent 2"}}},
		// Another block's name with the same tag takes it too, whatever its type.
		"Decision 2": {Nodes: []Node{{ID: "a", Type: CaseEntry, Name: "decision"}}},
	} {
		typ := Agent
		if strings.HasPrefix(want, "Decision") {
			typ = Decision
		}
		if got := graph.DefaultName(typ, "new"); got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}

// A name needs a letter or digit, may not be another block's (an event's included, since names are
// compared by their tag), and a block with a tag may not take a built-in tag's name.
func TestCheckName(t *testing.T) {
	g := Graph{Nodes: []Node{{ID: "e", Type: "pr_ready", Name: "When ready"}, agentNode("a", "Discovery", ""), agentNode("b", "Other", "")}}
	for name, want := range map[string]string{
		"discovery":   "another block is already named Discovery",
		"DISCOVERY !": "another block is already named Discovery",
		"When  Ready": "another block is already named When ready",
		"pr title":    "built-in tag",
		"Case":        "built-in tag",
		"Input":       "built-in tag",
		"???":         "needs a letter or a digit",
	} {
		if err := g.CheckName(PR, g.Nodes[2], name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", name, err, want)
		}
	}
	for _, name := range []string{"Other", "other!", "Fresh name", "Discovery 2"} {
		if err := g.CheckName(PR, g.Nodes[2], name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	// An event block has no tag, so a built-in tag's name is no harm to it.
	if err := g.CheckName(PR, g.Nodes[0], "Case"); err != nil {
		t.Errorf("an event named Case: %v", err)
	}
}

// Renaming a tag rewrites the references to it, and only those.
func TestRetagText(t *testing.T) {
	text := "{old} {old.report} [[old]] [[old.summary|Intake]] [[old|L]] {{old}} [[[[old]] {older} {old_x} {x.old} {input} {pr:old} [[older]] {old}"
	want := "{new} {new.report} [[new]] [[new.summary|Intake]] [[new|L]] {{old}} [[[[old]] {older} {old_x} {x.old} {input} {pr:old} [[older]] {new}"
	if got := RetagText(text, "old", "new"); got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

// Each kind of block has its templates, and a Condition its tag field, rewritten; a block with
// nothing to rewrite is left as it is.
func TestRetagEveryBlock(t *testing.T) {
	a := DefaultAgent(PR)
	a.Persona, a.Instruction, a.Goal, a.Additional, a.FollowUp, a.SummaryFormat = "custom", "Do {old.report}", "Done {old}", "Sys [[old]]", "Next {old.summary}", "Fmt [[old.report|R]]"
	d := DefaultDecision(PR)
	d.Context, d.Question = "{old}", "Is {old.confidence} high?"
	for typ, c := range map[string]struct {
		settings any
		look     func(Node) string
	}{
		Agent: {a, func(n Node) string {
			s, _ := AgentSettingsOf(n, PR)
			return s.Instruction + s.Goal + s.Additional + s.FollowUp + s.SummaryFormat
		}},
		Decision:  {d, func(n Node) string { s, _ := DecisionSettingsOf(n, PR); return s.Context + s.Question }},
		CaseEntry: {CaseEntrySettings{Entry: "E {old} [[old]]"}, func(n Node) string { s, _ := CaseEntrySettingsOf(n); return s.Entry }},
		Post:      {PostSettings{Message: "P {old.report}", GitHub: true}, func(n Node) string { s, _ := SettingsOf(n, DefaultPost()); return s.Message }},
		Approval:  {ApprovalSettings{Question: "A {old}", Hours: 1}, func(n Node) string { s, _ := SettingsOf(n, DefaultApproval(PR)); return s.Question }},
		TestRun:   {TestRunSettings{Script: "make {old}", Machine: 1, Minutes: 1}, func(n Node) string { s, _ := SettingsOf(n, DefaultTestRun()); return s.Script }},
		Condition: {ConditionSettings{Match: "all", Tag: "old", TagHas: "old"}, func(n Node) string { s, _ := SettingsOf(n, DefaultCondition()); return s.Tag + "/" + s.TagHas }},
	} {
		n := Node{ID: "x", Type: typ, Settings: mustJSON(c.settings)}
		r, changed := Retag(n, PR, "old", "new")
		got := c.look(r)
		if !changed || strings.Contains(strings.ReplaceAll(got, "old/old", ""), "old") && typ != Condition || !strings.Contains(got, "new") {
			t.Errorf("%s: changed %v: %q", typ, changed, got)
		}
		if typ == Condition && got != "new/old" {
			t.Errorf("a Condition's tag field: %q (the text it looks for is its own)", got)
		}
		if _, again := Retag(r, PR, "old", "new"); again {
			t.Errorf("%s: rewritten twice", typ)
		}
	}
	if _, changed := Retag(Node{ID: "x", Type: Agent}, PR, "old", "new"); changed {
		t.Error("a block never saved changed")
	}
	if _, changed := Retag(Node{ID: "x", Type: CaseEntry, Settings: mustJSON(CaseEntrySettings{Entry: "{input.report}"})}, PR, "old", "new"); changed {
		t.Error("a block that does not use it changed")
	}
}

// A block saved with its own Output to is renamed so that its tag stays what it was (value_summary:
// Value summary); where another block has that name it keeps its own, and the references move; blocks
// with one name are numbered; nothing is left to do the second time.
func TestMigrateNames(t *testing.T) {
	use := func(text string) json.RawMessage { return mustJSON(CaseEntrySettings{Entry: text}) }
	g := Graph{Nodes: []Node{
		agentNode("a", "Gatherer", "value_summary"),
		agentNode("b", "Checker", "verdict"),
		{ID: "c", Type: CaseEntry, Name: "Verdict", Settings: use("[[verdict]]")}, // has the name b's tag would need
		{ID: "u", Type: CaseEntry, Name: "User", Settings: use("{value_summary} {verdict.report} {checker}")},
		agentNode("d", "Plain", ""),
		{ID: "s1", Type: CaseEntry, Name: "Same"}, {ID: "s2", Type: CaseEntry, Name: "same"},
	}}
	m, renames, changed := g.MigrateNames(PR)
	if !changed {
		t.Fatal("nothing changed")
	}
	byID := map[string]Node{}
	for _, n := range m.Nodes {
		byID[n.ID] = n
	}
	if byID["a"].Name != "Value summary" || StoredOutputTo(byID["a"]) != "value_summary" {
		t.Errorf("renamed to its tag: %q with %q", byID["a"].Name, StoredOutputTo(byID["a"]))
	}
	// b's name stays (Verdict is taken), so its tag becomes checker and references move to it.
	if byID["b"].Name != "Checker" || StoredOutputTo(byID["b"]) != "checker" || len(renames) != 1 || renames[0] != (TagRename{Old: "verdict", New: "checker"}) {
		t.Errorf("collision: %q with %q, renames %v", byID["b"].Name, StoredOutputTo(byID["b"]), renames)
	}
	if e, _ := CaseEntrySettingsOf(byID["u"]); e.Entry != "{value_summary} {checker.report} {checker}" {
		t.Errorf("references: %q", e.Entry)
	}
	if e, _ := CaseEntrySettingsOf(byID["c"]); e.Entry != "[[checker]]" {
		t.Errorf("a block that is the other one's: %q", e.Entry)
	}
	if byID["s1"].Name != "Same" || byID["s2"].Name != "same 2" {
		t.Errorf("the same name twice: %q, %q", byID["s1"].Name, byID["s2"].Name)
	}
	if _, more, again := m.MigrateNames(PR); again || len(more) != 0 {
		t.Errorf("a second time: %v %v", again, more)
	}
	// Every tag is its name's now.
	for id, tag := range m.OutputNames(PR) {
		if tag != Slug(DisplayName(byID[id])) {
			t.Errorf("%s: %q", id, tag)
		}
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
