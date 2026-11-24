package workflow

import (
	"strings"
	"testing"
)

// A block follows the preset it is handed (the web layer reads it from the database); one whose preset
// is gone cannot pass its check, and says why; a Custom block uses its own text, summary format included.
func TestBlockFollowsItsPreset(t *testing.T) {
	p := &Persona{Key: "mine", Name: "Mine", Instruction: "pr text", Goal: "pr goal", Summary: "pr format",
		IssueInstruction: "issue text", Issue: "issue goal", IssueSummary: "issue format", Closing: ClosingReport}
	base := DefaultAgent(PR)
	base.Model = ModelRef{Provider: 1, Model: "m"}
	base.Persona = "mine"
	base.Instruction, base.Goal, base.SummaryFormat, base.Closing = "stored", "stored", "stored", ""

	a := base.WithPreset(p)
	for kind, want := range map[string][4]string{PR: {"pr text", "pr goal", "pr format", ClosingReport}, Issue: {"issue text", "issue goal", "issue format", ClosingReport}} {
		got := [4]string{a.EffectiveInstruction(kind), a.EffectiveGoal(kind), a.EffectiveSummaryFormat(kind), a.EffectiveClosing(kind)}
		if got != want || !a.OnPreset() {
			t.Errorf("%s: %v, want %v", kind, got, want)
		}
	}
	if err := a.Check(PR); err != nil {
		t.Errorf("a block on its preset: %v", err)
	}

	// The preset is gone: nothing of the block's own text is used in its place, and the check says so.
	gone := base
	if gone.OnPreset() || gone.EffectiveInstruction(PR) != "stored" && gone.EffectiveInstruction(PR) != "" {
		t.Errorf("unresolved: %+v", gone)
	}
	if err := gone.Check(PR); err == nil || !strings.Contains(err.Error(), "preset no longer exists") {
		t.Errorf("a block whose preset is gone: %v", err)
	}
	none := base
	none.Persona = ""
	if err := none.Check(PR); err == nil || !strings.Contains(err.Error(), "choose a preset") {
		t.Errorf("no preset and not Custom: %v", err)
	}

	// Custom uses its own, whatever a preset it was handed says.
	c := base
	c.Persona = "custom"
	c.Closing = ClosingReport
	if c.EffectiveInstruction(PR) != "stored" || c.EffectiveGoal(PR) != "stored" || c.EffectiveSummaryFormat(PR) != "stored" || c.EffectiveClosing(PR) != ClosingReport || c.OnPreset() {
		t.Errorf("custom: %+v", c)
	}
	if err := c.Check(PR); err != nil {
		t.Errorf("custom: %v", err)
	}
	// The templates of a block include its summary format, so a typo in it is found.
	if got := strings.Join(c.Templates(PR), "|"); !strings.Contains(got, "stored||stored|stored|") {
		t.Errorf("templates: %q", got)
	}
	long := c
	long.SummaryFormat = strings.Repeat("x", maxTemplate+1)
	if long.Check(PR) == nil {
		t.Error("a summary format over the limit was accepted")
	}
	// The form reads it.
	form := map[string]string{"persona": "custom", "summary_format": "  **A:** <x>  ", "closing": "report", "max_turns": "30", "max_minutes": "10", "rerun": "resume"}
	if got := AgentFromForm(func(k string) string { return form[k] }, func(string) []string { return nil }); got.SummaryFormat != "**A:** <x>" {
		t.Errorf("form: %q", got.SummaryFormat)
	}
}

// The built-in presets all follow the rules a saved one must: they are the seeds.
func TestSeedPresetsAreWhole(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range SeedPresets {
		if p.Key == "" || p.Key == "custom" || seen[p.Key] || p.Name == "" || p.Description == "" || strings.Contains(p.Description, "\n") {
			t.Errorf("seed %+v", p.Key)
		}
		seen[p.Key] = true
		for kind, texts := range map[string][]string{PR: {p.Instruction, p.Goal, p.Summary}, Issue: {p.IssueInstruction, p.Issue, p.IssueSummary}} {
			if strings.TrimSpace(texts[0]) == "" || strings.TrimSpace(texts[1]) == "" {
				t.Errorf("%s %s: no instruction or goal", p.Key, kind)
			}
			if err := CheckTemplates(kind, nil, texts...); err != nil {
				t.Errorf("%s %s: %v", p.Key, kind, err)
			}
		}
		if p.Closing != "" && p.Closing != ClosingReport {
			t.Errorf("%s closing %q", p.Key, p.Closing)
		}
	}
	if SeedPresets[0].Key != "gather_information" || DefaultAgent(PR).Persona != "gather_information" {
		t.Error("a new block starts on Gather Information")
	}
}
