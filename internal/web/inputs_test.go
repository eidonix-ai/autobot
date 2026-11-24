package web

import (
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/workflow"
)

func (c *contEnv) saveLinks(nodes []workflow.Node, links []workflow.Link) {
	c.t.Helper()
	w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
	if _, err := c.st.SaveWorkflow(c.ws.ID, workflow.PR, w.Version, string(rawJSON(workflow.Graph{Nodes: nodes, Links: links})), c.admin); err != nil {
		c.t.Fatal(err)
	}
}

func link(from, out, to string) workflow.Link {
	return workflow.Link{From: from, Out: out, To: to, In: "in"}
}

// A block that joins steps reads, after what the ones that fired produced, the ones that did not,
// each under its name with why: failed (and the start of its error) or skipped; through a Node block
// as well, and in each part of {input}. A block with nothing missing reads what it did. [M]
func TestInputNamesMissingSteps(t *testing.T) {
	c := newContEnv(t, nil)
	cond := workflow.Node{ID: "cnd", Type: workflow.Condition, Name: "Docs", Settings: rawJSON(workflow.ConditionSettings{Match: "all", Files: "*.zzz"})}
	c.saveLinks([]workflow.Node{events[0],
		caseEntryNode("a", "Style", "Style is fine."),
		caseEntryNode("bad", "Correctness", ""), // an empty entry fails when it runs
		cond,
		caseEntryNode("sk", "Docs review", "Docs are fine."), // hangs on the condition's "yes", which this item does not take
		caseEntryNode("join", "Join", "ALL={input}|SUM={input.summary}|REP={input.report}|NOTES={input.scratchpad}"),
		{ID: "nd", Type: workflow.Junction},
		caseEntryNode("via", "Via a node", "VIA={input}"),
		caseEntryNode("solo", "Solo", "SOLO={input}"),
		caseEntryNode("two", "Two fired", "TWO={input}"),
		caseEntryNode("b", "Other", "Other is fine."),
	}, []workflow.Link{link("ev1", "out", "a"), link("ev1", "out", "bad"), link("ev1", "out", "cnd"), link("ev1", "out", "b"),
		link("cnd", "yes", "sk"),
		link("a", "out", "join"), link("bad", "out", "join"), link("sk", "out", "join"),
		link("a", "out", "nd"), link("bad", "out", "nd"), link("nd", "out", "via"),
		link("a", "out", "solo"),
		link("a", "out", "two"), link("b", "out", "two")})
	steps := c.run("ev1")
	if steps["bad"].Status != "failed" || steps["sk"].Status != "skipped" || steps["a"].Status != "done" {
		t.Fatalf("the sources: %+v", steps)
	}
	why := "## Correctness (failed: " + steps["bad"].Error + ")"
	if steps["bad"].Error == "" {
		t.Fatal("the failing step has no error")
	}
	missing := why // a skipped branch ("Docs review") is not named: only a step that broke is
	whole := "Style is fine.\n\n" + missing
	// What fired is as it was, bare when it is one source; the missing follow, named, in each part (and
	// alone where the fired one had nothing to give, as in the notes).
	if got, want := steps["join"].Output, "ALL="+whole+"|SUM="+whole+"|REP="+whole+"|NOTES="+missing; got != want {
		t.Errorf("the join's input:\n%q\nwant\n%q", got, want)
	}
	// Through a Node block: the Node's own sources are the block's.
	if got := steps["via"].Output; !strings.Contains(got, "VIA=Style is fine.") || !strings.Contains(got, why) {
		t.Errorf("through a Node: %q", got)
	}
	// Nothing missing: as it was. One source: its output bare. Two that fired: each under its name, nothing else.
	if got := steps["solo"].Output; got != "SOLO=Style is fine." {
		t.Errorf("one source: %q", got)
	}
	if got := steps["two"].Output; got != "TWO=## Style\n\nStyle is fine.\n\n## Other\n\nOther is fine." {
		t.Errorf("two sources that fired: %q", got)
	}
}
