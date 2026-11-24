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
	found := false
	for _, s := range SeedPresets {
		found = found || s.Key == "review_pretest"
	}
	if !found {
		t.Fatal("not seeded")
	}
}
