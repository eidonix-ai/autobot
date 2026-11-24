package workflow

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// refNode is a reference block: an Agent block that continues the block of the id, with the name and
// saved tag an older canvas gave it (both may be "").
func refNode(id, of, name, outputTo string) Node {
	b, _ := json.Marshal(map[string]string{"continue_of": of, "output_to": outputTo})
	return Node{ID: id, Type: Agent, Name: name, Settings: b}
}

func entryNode(id, name, text string) Node {
	b, _ := json.Marshal(CaseEntrySettings{Entry: text})
	return Node{ID: id, Type: CaseEntry, Name: name, Settings: b}
}

// ev1 → Explore → Check, and ev2 → a reference to Explore → After.
func refGraph() Graph {
	return Graph{Nodes: []Node{
		{ID: "ev1", Type: "pr_ready"}, {ID: "ev2", Type: "pr_activity"},
		agentNode("ex", "Explore", ""), entryNode("ck", "Check", "{explore.report}"),
		refNode("rf", "ex", "", ""), entryNode("af", "After", "{explore.summary}"),
	}, Links: []Link{{"ev1", "out", "ex", "in"}, {"ex", "out", "ck", "in"}, {"ev2", "out", "rf", "in"}, {"rf", "out", "af", "in"}}}
}

// A reference has no name of its own: it shows its source's with "(continued)", follows a rename of the
// source, and says so when the source is gone.
func TestReferenceShowsItsSourcesName(t *testing.T) {
	g := refGraph()
	rf := g.Nodes[4]
	if got := g.NameOf(rf); got != "Explore (continued)" {
		t.Errorf("the reference is called %q", got)
	}
	if got := g.NameOf(g.Nodes[2]); got != "Explore" {
		t.Errorf("the source is called %q", got)
	}
	g.Nodes[2].Name = "Scout"
	if got := g.NameOf(rf); got != "Scout (continued)" {
		t.Errorf("after the source was renamed: %q", got)
	}
	// A name an older canvas gave it is not what it shows.
	old := refNode("rf", "ex", "Discovery 2", "discovery_2")
	if got := g.NameOf(old); got != "Scout (continued)" {
		t.Errorf("with an old name: %q", got)
	}
	if got := g.Named(old).Name; got != "Scout (continued)" {
		t.Errorf("Named: %q", got)
	}
	// With the source gone it has no tag either.
	gone := Graph{Nodes: []Node{{ID: "ev1", Type: "pr_ready"}, rf}}
	if got := gone.NameOf(rf); got != "Agent (continued)" {
		t.Errorf("without its source: %q", got)
	}
	if _, ok := gone.OutputNames(PR)["rf"]; ok {
		t.Error("a reference with no source has a tag")
	}
}

// A reference takes no name from the workflow's names: another block may be called what an old reference
// was, a new block is not numbered for it, and renaming it is not checked.
func TestReferenceIsOutsideNameUniqueness(t *testing.T) {
	g := refGraph()
	g.Nodes[4] = refNode("rf", "ex", "Agent", "agent") // an older canvas named it Agent
	if got := g.DefaultName(Agent, ""); got != "Agent" {
		t.Errorf("a new block is named %q because of a reference", got)
	}
	other := Node{ID: "x", Type: Agent}
	if err := g.CheckName(PR, other, "Agent"); err != nil {
		t.Errorf("a name an old reference had: %v", err)
	}
	if err := g.CheckName(PR, g.Nodes[4], "Explore"); err != nil {
		t.Errorf("a reference's name was checked: %v", err)
	}
	if err := g.CheckName(PR, other, "Explore"); err == nil {
		t.Error("a name another block has was allowed")
	}
	// The unnamed block gets the tag the reference would have taken.
	g.Nodes = append(g.Nodes, other)
	if tag := g.OutputNames(PR)["x"]; tag != "agent" {
		t.Errorf("the new block's tag is %q", tag)
	}
}

// The reference writes and reads its source's tag, and has no tag in the output names of its own.
func TestReferenceHasItsSourcesTag(t *testing.T) {
	g := refGraph()
	names := g.OutputNames(PR)
	if names["rf"] != "explore" || names["ex"] != "explore" {
		t.Errorf("tags: %v", names)
	}
	for id, tag := range names {
		if strings.HasPrefix(tag, "explore_") || strings.HasPrefix(tag, "agent") {
			t.Errorf("%s has the tag %q", id, tag)
		}
	}
}

// The reference's own chain may use the source's tag after it, the source's chain as before, and a block
// after both is fine; the other events' group never lists the source's tag a second time nor the
// reference apart, and a block's own tag is not among another event's.
func TestReferenceTagRules(t *testing.T) {
	g := refGraph()
	for _, c := range []struct {
		id, text string
	}{{"af", "{explore} {explore.summary} {explore.report} {explore.scratchpad} [[explore.report]]"}, {"ck", "{explore.report}"}, {"rf", "{explore.summary}"}} {
		if err := g.CheckNodeTemplates(PR, c.id, c.text); err != nil {
			t.Errorf("%s may use %s: %v", c.id, c.text, err)
		}
	}
	// After the reference, the tag is upstream: listed once, and not again from other events.
	if up := g.OutputTags(PR, "af"); up["explore"] != Agent || len(up) != 1 {
		t.Errorf("upstream of After: %v", up)
	}
	if other := g.OtherEventTags(PR, "af"); other["explore"] != "" {
		t.Errorf("from other events for After: %v", other)
	}
	// In the source's chain: Check sees it upstream, and the reference elsewhere adds nothing.
	if up := g.OutputTags(PR, "ck"); up["explore"] != Agent || len(up) != 1 {
		t.Errorf("upstream of Check: %v", up)
	}
	if other := g.OtherEventTags(PR, "ck"); other["explore"] != "" {
		t.Errorf("from other events for Check: %v", other)
	}
	// The source does not find its own tag among the other events', where the reference writes it.
	if other := g.OtherEventTags(PR, "ex"); other["explore"] != "" {
		t.Errorf("from other events for the source: %v", other)
	}
	// The reference reads the source's value from the other event until the source runs before it.
	if other := g.OtherEventTags(PR, "rf"); other["explore"] != Agent {
		t.Errorf("from other events for the reference: %v", other)
	}
	// A block downstream of both chains.
	both := refGraph()
	both.Nodes = append(both.Nodes, entryNode("bo", "Both", "{explore.report}"))
	both.Links = append(both.Links, Link{"ck", "out", "bo", "in"}, Link{"af", "out", "bo", "in"})
	if err := both.CheckNodeTemplates(PR, "bo", "{explore.report}"); err != nil {
		t.Errorf("a block after both: %v", err)
	}
	if up, other := both.OutputTags(PR, "bo"), both.OtherEventTags(PR, "bo"); len(up) != 3 || other["explore"] != "" || up["explore"] != Agent {
		t.Errorf("after both: upstream %v, other events %v", up, other)
	}
	// The tag list names it once.
	list := OutputTagList(g.OutputTags(PR, "af"))
	n := 0
	for _, tg := range list {
		if tg.Name == "explore" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the Tags list: %v", list)
	}
}

// An older canvas's reference with a name of its own (Discovery 2): its name and saved tag are cleared,
// every reference to its tag in the workflow is rewritten to the source's, and the values under the old
// tag move to the source's, which keep a value they have. Done once.
func TestMigrateReferences(t *testing.T) {
	cond, _ := json.Marshal(ConditionSettings{Match: "all", Tag: "discovery_2", TagHas: "x"})
	g := Graph{Nodes: []Node{
		{ID: "ev1", Type: "pr_ready"}, agentNode("d", "Discovery", "discovery"), refNode("r", "d", "Discovery 2", "discovery_2"),
		entryNode("u", "User", "{discovery_2.summary} {discovery_2} [[discovery_2.report|The report]] {discovery} {{discovery_2}} {discovery_20}"),
		{ID: "c", Type: Condition, Name: "Has it", Settings: cond},
		refNode("n", "d", "", ""), // already migrated: unchanged
	}, Links: []Link{{"ev1", "out", "d", "in"}}}
	out, renames, changed := g.MigrateReferences(PR)
	if !changed || len(renames) != 1 || renames[0] != (TagRename{Old: "discovery_2", New: "discovery", Keep: true}) {
		t.Fatalf("changed %v, renames %v", changed, renames)
	}
	r := out.Nodes[2]
	if r.Name != "" || StoredOutputTo(r) != "" || ReferenceOf(r) != "d" {
		t.Errorf("the reference: name %q, tag %q, continues %q", r.Name, StoredOutputTo(r), ReferenceOf(r))
	}
	if e, _ := CaseEntrySettingsOf(out.Nodes[3]); e.Entry != "{discovery.summary} {discovery} [[discovery.report|The report]] {discovery} {{discovery_2}} {discovery_20}" {
		t.Errorf("the entry: %q", e.Entry)
	}
	if cs, _ := SettingsOf(out.Nodes[4], DefaultCondition()); cs.Tag != "discovery" || cs.TagHas != "x" {
		t.Errorf("the condition: %+v", cs)
	}
	if got := out.OutputNames(PR)["r"]; got != "discovery" {
		t.Errorf("the reference's tag is %q", got)
	}
	// Once.
	if _, again, changed := out.MigrateReferences(PR); changed || len(again) != 0 {
		t.Errorf("migrated again: %v %v", changed, again)
	}
	// A reference whose source is gone is left as it is.
	lone := Graph{Nodes: []Node{refNode("r", "gone", "Discovery 2", "discovery_2")}}
	if got, renames, changed := lone.MigrateReferences(PR); changed || len(renames) != 0 || got.Nodes[0].Name != "Discovery 2" {
		t.Errorf("a reference with no source: %v %v %+v", changed, renames, got.Nodes[0])
	}
	// Names are migrated apart from references: a reference's saved tag does not rename it.
	if _, _, changed := g.MigrateNames(PR); slices.Contains([]bool{changed}, true) && g.Nodes[2].Name == "" {
		t.Error("MigrateNames named a reference")
	}
	if named, _, _ := g.MigrateNames(PR); named.Nodes[2].Name != "Discovery 2" {
		t.Errorf("MigrateNames touched a reference: %q", named.Nodes[2].Name)
	}
}
