package workflow

import (
	"strings"
	"testing"
)

// A block that continues another's conversation needs no model of its own; the tags of what is new
// are for pull requests, and may be used by any template there.
func TestContinuationSettings(t *testing.T) {
	a := DefaultAgent(PR)
	if err := a.Check(PR); err == nil || !strings.Contains(err.Error(), "choose a model") {
		t.Fatalf("a block with no model: %v", err)
	}
	a.ContinueOf = "other"
	if err := a.Check(PR); err != nil {
		t.Fatalf("a continuation with no model: %v", err)
	}
	a.NoConversation = "sometimes"
	if err := a.Check(PR); err == nil {
		t.Fatal("an unknown choice for an item with no conversation")
	}
	a.NoConversation = "fresh"
	if err := a.Check(PR); err != nil || !a.FreshWithoutConversation() || !a.Continues() {
		t.Fatalf("%v", err)
	}
	for _, tag := range []string{"pr_new_commits", "pr_new_comments"} {
		if !builtIn(tag, PR) || builtIn(tag, Issue) {
			t.Errorf("{%s} is a pull request tag", tag)
		}
	}
	if err := CheckTemplates(PR, nil, "{pr_new_commits} {pr_new_comments}"); err != nil {
		t.Fatal(err)
	}
	// The block's line on the canvas does not ask for a model it does not have.
	if got := stepSummary(Node{Type: Agent, Settings: []byte(`{"continue_of":"x","persona":"custom","output_to":"again"}`)}, PR, func(ModelRef) string { return "m" }); got != "→ {again}" {
		t.Fatalf("summary %q", got)
	}
}
