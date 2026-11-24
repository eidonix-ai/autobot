package workflow

import (
	"strings"
	"testing"
)

// A block on a preset that turns the citation marks off does not mark files cited but not opened;
// every other block does, Custom included.
func TestEffectiveCitationMarks(t *testing.T) {
	on := &Persona{Key: "a", Instruction: "i", Goal: "g", IssueInstruction: "i", Issue: "g"}
	off := &Persona{Key: "b", Instruction: "i", Goal: "g", IssueInstruction: "i", Issue: "g", NoCitationMarks: true}
	base := DefaultAgent(PR)
	if !base.EffectiveCitationMarks() {
		t.Error("a block whose preset is not resolved has marks")
	}
	if !base.WithPreset(on).EffectiveCitationMarks() || base.WithPreset(off).EffectiveCitationMarks() {
		t.Error("a block follows its preset's setting")
	}
	custom := base
	custom.Persona = "custom"
	if !custom.EffectiveCitationMarks() {
		t.Error("a Custom block has marks")
	}
}

// The Editor preset is one text for pull requests and issues, has no closing step or summary format,
// turns the marks off, and is among the seeds; its texts pass the template check for both kinds.
func TestEditorPreset(t *testing.T) {
	e := EditorPreset
	if e.Key != "editor_prose" || e.Name != "Editor - Prose Cleanup" || !e.NoCitationMarks || e.Closing != "" || e.Summary != "" || e.IssueSummary != "" ||
		e.Instruction != e.IssueInstruction || e.Goal != e.Issue {
		t.Errorf("the editor preset: %+v", e)
	}
	if !strings.Contains(e.Instruction, "## The draft\n\n{input.summary}\n") {
		t.Error("the editor reads the draft from {input.summary}")
	}
	seeded := false
	for _, p := range SeedPresets {
		seeded = seeded || p.Key == e.Key
	}
	if !seeded {
		t.Error("the editor preset is not among the seeds")
	}
	for _, kind := range []string{PR, Issue} {
		if err := CheckTemplates(kind, nil, e.InstructionFor(kind), e.GoalFor(kind)); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}
}
