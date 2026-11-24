package workflow

import (
	"strings"
	"testing"
)

// The Pre-Test Report preset reads the lens reviews from {input.report}, its texts pass the template
// check, and it is among the seeded presets.
func TestPreTestPreset(t *testing.T) {
	p := PreTestPreset
	if !strings.Contains(p.Instruction, "{input.report}") || !strings.Contains(p.Summary, "**Recommendation:**") || p.Closing != ClosingReport {
		t.Fatalf("%+v", p)
	}
	if err := CheckTemplates(PR, nil, p.Instruction, p.Goal, p.Summary); err != nil {
		t.Fatal(err)
	}
	seeded := map[string]bool{}
	for _, s := range SeedPresets {
		seeded[s.Key] = true
	}
	if !seeded["review_pretest"] || !seeded["review_consequences"] {
		t.Fatal("not seeded")
	}
	c := ConsequencesPreset
	if c.Name != "Code Review - Unintended Consequences" || !strings.Contains(c.Instruction, "Unintended consequences") || c.Closing != ClosingReport {
		t.Fatalf("%+v", c)
	}
	if err := CheckTemplates(PR, nil, c.Instruction, c.Goal, c.Summary); err != nil {
		t.Fatal(err)
	}
}

// The Pre-Test Report sees the machines, and its plan is matched against them.
func TestPreTestSeesTheMachines(t *testing.T) {
	if !strings.Contains(PreTestInstruction, "{machines}") || !strings.Contains(PreTestInstruction, "Match the plan against the available test machines") ||
		PreTestPreset.Instruction != PreTestInstruction {
		t.Fatalf("%s", PreTestInstruction)
	}
	if err := CheckTemplates(PR, nil, PreTestInstruction); err != nil {
		t.Fatal(err)
	}
}
