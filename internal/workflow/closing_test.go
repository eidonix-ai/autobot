package workflow

import "testing"

// seed is a built-in preset, which a test hands to a block as the web layer does with the stored one.
func seed(key string) *Persona {
	for i := range SeedPresets {
		if SeedPresets[i].Key == key {
			return &SeedPresets[i]
		}
	}
	return nil
}

// Gather Information writes its report from the scratchpad; the other presets do not, for now; a
// Custom block has its own closing step. Report turns default to 15, also in old saved blocks.
func TestClosingStep(t *testing.T) {
	for i, p := range SeedPresets {
		want := ""
		if p.Key == "gather_information" {
			want = ClosingReport
		}
		if got := (AgentSettings{Persona: p.Key, Closing: ClosingReport}).WithPreset(&SeedPresets[i]).EffectiveClosing(PR); got != want {
			t.Errorf("preset %s: closing %q, want %q (a preset's own, whatever the block stored)", p.Key, got, want)
		}
	}
	if got := (AgentSettings{Persona: "custom", Closing: ClosingReport}).EffectiveClosing(PR); got != ClosingReport {
		t.Errorf("custom: %q", got)
	}
	if got := (AgentSettings{Persona: "custom"}).EffectiveClosing(Issue); got != ClosingNone {
		t.Errorf("custom with none: %q", got)
	}
	if (AgentSettings{}).EffectiveReportTurns() != 15 || (AgentSettings{ReportTurns: 7}).EffectiveReportTurns() != 7 {
		t.Error("report turns")
	}
	base := DefaultAgent(PR).WithPreset(seed("gather_information"))
	base.Model = ModelRef{Provider: 1, Model: "m"}
	if err := base.Check(PR); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*AgentSettings){
		"a made-up closing step": func(s *AgentSettings) { s.Closing = "poem" },
		"negative report turns":  func(s *AgentSettings) { s.ReportTurns = -1 },
		"too many report turns":  func(s *AgentSettings) { s.ReportTurns = 201 },
	} {
		s := base
		mod(&s)
		if s.Check(PR) == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	form := map[string]string{"persona": "custom", "closing": "report", "report_turns": "12", "max_turns": "30", "max_minutes": "10", "rerun": "resume"}
	got := AgentFromForm(func(k string) string { return form[k] }, func(string) []string { return nil })
	if got.Closing != ClosingReport || got.ReportTurns != 12 {
		t.Errorf("form: %+v", got)
	}
}

// {charter} is a top-level tag Autobot fills in; its old names keep working.
func TestCharterTag(t *testing.T) {
	for _, kind := range []string{PR, Issue} {
		found := false
		for _, tag := range Tags(kind) {
			if tag.Name == "charter" {
				found = tag.Auto && !tag.Field() && tag.About == "the project charter (kept in Autobot)"
			}
			if tag.Name == "repo:charter" {
				t.Errorf("%s still lists {repo:charter}", kind)
			}
		}
		if !found {
			t.Errorf("%s: {charter} is not an auto, top-level tag", kind)
		}
	}
	for _, old := range []string{"repo:charter", "project_charter", "charter"} {
		if got := Render("{"+old+"}", map[string]string{"charter": "the charter"}); got != "the charter" {
			t.Errorf("{%s} rendered %q", old, got)
		}
		if err := CheckTemplates(PR, nil, "{"+old+"}"); err != nil {
			t.Errorf("{%s}: %v", old, err)
		}
	}
}
