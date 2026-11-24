package web

import (
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// The Subtraction reviewer is put into an older database once, as a shared preset that writes its
// report from the scratchpad, with the orientation packet and citation marks; a removed one stays removed.
func TestSubtractionPresetIsSeededOnce(t *testing.T) {
	st := olderDB(t)
	if _, err := New(st, fakeGitHub{}); err != nil {
		t.Fatal(err)
	}
	p, err := st.PresetByKey("review_subtraction")
	if err != nil || !p.Shared() || p.Name != "Code Review - Subtraction" || p.Closing != workflow.ClosingReport || p.NoCitationMarks ||
		!strings.Contains(p.PRInstruction, "{pr:packet}") || !strings.Contains(p.PRInstruction, "Your lens is Subtraction") || !strings.Contains(p.PRSummary, "**Could remove:**") {
		t.Fatalf("the subtraction preset: %+v %v", p, err)
	}
	if err := workflow.CheckTemplates(workflow.PR, nil, p.PRInstruction, p.PRGoal, p.PRSummary); err != nil {
		t.Error(err)
	}
	if err := st.RemovePreset(p.ID, func(store.StoredGraph) (string, bool, error) { return "", false, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := New(st, fakeGitHub{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PresetByKey("review_subtraction"); err == nil {
		t.Error("a removed Subtraction preset came back")
	}
}
