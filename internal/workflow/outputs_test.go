package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

// The parts of an Agent block's output are named with a dot: {gather.report}. A colon is for the
// fields Autobot fills from a built-in parent. Misspellings are refused when a block is saved.
func TestCheckTemplatesOutputParts(t *testing.T) {
	outs := OutputSet{"gather": true, "verdict": false} // an Agent's output, and a Decision's
	for _, ok := range []string{"{gather} {gather.summary} {gather.report} {gather.scratchpad}", "{verdict}", "{{gather.report}}"} {
		if err := CheckTemplates(PR, outs, ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, tc := range []struct{ tpl, want string }{
		{"{verdict.report}", "this one is one value"},
		{"{gather.notes}", "see the Tags list"},
		{"{gather:report}", "use a dot, such as {gather.report}"},
		{"{verdict:report}", "this one is one value"},
		{"{nope.report}", "not another step's output tag"},
		{"{pr.title}", "use a colon"},
		{"{pr:titel}", "fields of {pr}"},
		{"{input.notes}", "see the Tags list for its parts"},
		{"{input:report}", "use a dot, such as {input.report}"},
	} {
		if err := CheckTemplates(PR, outs, tc.tpl); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want it to say %q", tc.tpl, err, tc.want)
		}
	}
	// Without the output the parts are not tags either.
	if CheckTemplates(PR, nil, "{gather.report}") == nil {
		t.Error("a part of an output nothing sets passed")
	}
	// Built-in fields still use their colon.
	if err := CheckTemplates(PR, nil, "{pr:title} {pr:diff}"); err != nil {
		t.Error(err)
	}
}

// {input} is what the step before produced, in every kind of workflow, with its parts; it and the
// old name {previous_output} are still accepted and still mean the same, but neither is listed
// any more: every step's output is kept under its own tag.
func TestInputTag(t *testing.T) {
	for _, kind := range []string{PR, Issue} {
		if err := CheckTemplates(kind, nil, "{input} {input.summary} {input.report} {input.scratchpad} {previous_output}"); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
		listed := map[string]Tag{}
		for _, tag := range Tags(kind) {
			listed[tag.Name] = tag
		}
		if _, ok := listed["previous_output"]; ok {
			t.Errorf("%s: the old name is listed", kind)
		}
		for _, name := range []string{"input", "input.summary", "input.report", "input.scratchpad"} {
			if _, ok := listed[name]; ok {
				t.Errorf("%s: {%s} is listed", kind, name)
			}
		}
		all := map[string]Tag{}
		for _, tag := range AllTags(kind) {
			all[tag.Name] = tag
		}
		if all["input"].Name == "" || all["input"].Field() || all["input"].Auto {
			t.Errorf("%s: {input} %+v", kind, all["input"])
		}
	}
	if CanonicalTag("previous_output", PR) != "input.report" {
		t.Error("the old name does not mean the report")
	}
	if got := Render("[{previous_output}] [{input.report}] [{input.summary}] [{input}] [{input.scratchpad}]", map[string]string{
		"input": "WHOLE", "input.summary": "SUM", "input.report": "REP", "input.scratchpad": "NOTES"}); got != "[REP] [REP] [SUM] [WHOLE] [NOTES]" {
		t.Errorf("rendered %q", got)
	}
	// No output tag may be named like it.
	n := DefaultAgent(PR).WithPreset(seed("gather_information"))
	n.OutputTo, n.Model = "input", ModelRef{Provider: 1, Model: "m"}
	if err := n.Check(PR); err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Errorf("an output named input: %v", err)
	}
}

// The template of a new Case Entry block uses {input.report}; a new Post block has no text yet.
func TestNewBlocksDefaultToTheReport(t *testing.T) {
	if DefaultCaseEntry().Entry != "{input.report}" || DefaultPost().Message != "" {
		t.Fatalf("%q %q", DefaultCaseEntry().Entry, DefaultPost().Message)
	}
	// A block saved with its own text keeps it.
	n := Node{ID: "c", Type: CaseEntry, Settings: json.RawMessage(`{"entry":"old: {previous_output}","finalize":true}`)}
	if s, err := CaseEntrySettingsOf(n); err != nil || s.Entry != "old: {previous_output}" {
		t.Fatalf("%+v %v", s, err)
	}
}

// What the Tags list shows of the outputs: an Agent's tag with its three parts under it, a
// Decision's alone; tags and parts are not marked as filled in by Autobot.
func TestOutputTagList(t *testing.T) {
	a, d := DefaultAgent(PR), DefaultDecision(PR)
	a.OutputTo, d.OutputTo = "gather", "verdict"
	ab, _ := json.Marshal(a)
	db, _ := json.Marshal(d)
	// The steps before z: e → a, e → d, a → z, d → z.
	g := Graph{Nodes: []Node{{ID: "e", Type: "pr_ready"}, {ID: "a", Type: Agent, Settings: ab}, {ID: "d", Type: Decision, Settings: db}, {ID: "z", Type: Agent}},
		Links: []Link{{"e", "out", "a", "in"}, {"e", "out", "d", "in"}, {"a", "out", "z", "in"}, {"d", "yes", "z", "in"}}}
	outs := g.OutputTags(PR, "z")
	if !outs["gather"] || outs["verdict"] || len(outs) != 2 {
		t.Fatalf("%v", outs)
	}
	var got []string
	for _, tag := range OutputTagList(outs) {
		if tag.Auto {
			t.Errorf("%s is marked auto", tag.Name)
		}
		got = append(got, tag.Name+"|"+tag.About+"|"+map[bool]string{true: "field", false: ""}[tag.Field()])
	}
	want := "gather|its summary, report and notes|;gather.summary|the summary|field;gather.report|the full report|field;gather.scratchpad|its notes in full|field;verdict|another step's output|"
	if strings.Join(got, ";") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(got, ";"), want)
	}
	// A node is offered the output of the steps before it only: not its own, and not a step after it.
	if got := g.OutputTags(PR, "a"); len(got) != 0 {
		t.Errorf("a is offered %v", got)
	}
	if got := g.OutputTags(PR, "d"); len(got) != 0 {
		t.Errorf("d is offered %v", got)
	}
	// z has no tag saved: it is offered as its name's once something runs after it.
	g.Nodes = append(g.Nodes, Node{ID: "y", Type: Agent, Name: "Last"})
	g.Links = append(g.Links, Link{"z", "out", "y", "in"})
	if got, isDecision := g.OutputTags(PR, "y"), func(m OutputSet) bool { _, ok := m["verdict"]; return ok }; len(got) != 3 || !got["agent"] || !got["gather"] || !isDecision(got) {
		t.Errorf("y is offered %v", got)
	}
}
