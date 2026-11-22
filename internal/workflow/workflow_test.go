package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func node(id, typ string) Node { return Node{ID: id, Type: typ, X: 10, Y: 10} }

// The server refuses any workflow the canvas should not be able to make.
func TestCheck(t *testing.T) {
	ok := Graph{Nodes: []Node{node("a", "pr_ready"), node("b", "pr_activity")}}
	if err := ok.Check(PR); err != nil {
		t.Fatalf("a valid workflow: %v", err)
	}
	bad, _ := json.Marshal(EventSettings{Mode: "sometimes", StopTag: "x", SummonPhrase: "y"})
	for name, tc := range map[string]struct {
		g    Graph
		kind string
		want string
	}{
		"type from the other tab":  {Graph{Nodes: []Node{node("a", "issue_opened")}}, PR, "no node type"},
		"unknown type":             {Graph{Nodes: []Node{node("a", "run_anything")}}, PR, "no node type"},
		"duplicate id":             {Graph{Nodes: []Node{node("a", "pr_ready"), node("a", "pr_draft")}}, PR, "share the id"},
		"bad id":                   {Graph{Nodes: []Node{node("A B", "pr_ready")}}, PR, "not valid"},
		"link into an event":       {Graph{Nodes: []Node{node("a", "pr_ready"), node("b", "pr_draft")}, Links: []Link{{"a", "b"}}}, PR, "nothing can link into it"},
		"link to a missing node":   {Graph{Nodes: []Node{node("a", "pr_ready")}, Links: []Link{{"a", "z"}}}, PR, "not on the canvas"},
		"settings that do not fit": {Graph{Nodes: []Node{{ID: "a", Type: "pr_ready", Settings: bad}}}, PR, "choose when it runs"},
		"unknown kind":             {Graph{}, "discussion", "no workflow kind"},
	} {
		if err := tc.g.Check(tc.kind); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	activity := DefaultEvent("pr_activity")
	activity.Activity = nil
	if err := activity.Check("pr_activity"); err == nil {
		t.Error("PR Activity with no activity chosen was accepted")
	}
	activity.Activity = []string{"comments", "teleport"}
	if err := activity.Check("pr_activity"); err == nil {
		t.Error("an unknown kind of activity was accepted")
	}
}
