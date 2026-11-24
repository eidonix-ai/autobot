package workflow

import (
	"strings"
	"testing"
)

// The Lens Team dispatches every lens to a sub-agent with include_context: true (no copy-paste of
// the orientation packet, no conflicting single-lens rules), and its texts pass the save check.
func TestLensTeamPreset(t *testing.T) {
	p := LensTeamPreset
	if p.Key != "review_lens_team" || p.Name != "Code Review - Lens Team" || p.Closing != ClosingReport || p.NoCitationMarks || p.NoTools {
		t.Fatalf("the preset: %+v", p)
	}
	for name, lens := range map[string]string{
		"correctness": correctnessLens, "architecture": architectureLens, "conventions": conventionsLens,
		"subtraction": subtractionLens, "prose": proseLens, "consequences": consequencesLens,
	} {
		if !strings.Contains(p.Instruction, lens) {
			t.Errorf("the instruction lacks the %s lens", name)
		}
	}
	if !strings.HasPrefix(p.Instruction, gatherPRHead+"You lead a team review") ||
		!strings.Contains(p.Instruction, "spawn_agent") ||
		!strings.Contains(p.Instruction, "include_context: true") {
		t.Errorf("the instruction:\n%s", p.Instruction)
	}
	if strings.Contains(p.Instruction, "Stay inside your lens") {
		t.Error("Lens Team should not contain the single-lens restriction")
	}
	if err := CheckTemplates(PR, nil, p.Instruction, p.Goal, p.Summary); err != nil {
		t.Error(err)
	}
	if err := CheckTemplates(Issue, nil, p.IssueInstruction, p.Issue, p.IssueSummary); err != nil {
		t.Error(err)
	}
	if p.IssueInstruction != reviewIssueInstruction || p.Issue != reviewIssueGoal {
		t.Error("the issue side is not the review presets'")
	}
	if got, want := p.RunDefaults.JSON(), `{"max_turns":60,"max_minutes":60,"time_warn_percent":20,"sub_agents":6,"sub_agents_per_run":0}`; got != want {
		t.Errorf("run defaults %s, want %s", got, want)
	}
}

// Single-agent code review presets start blocks with 60 turns, 30 minutes, 20% warning and 0 sub-agents;
// no shared preset hardcodes repository-specific CUDA/Flare jargon.
func TestReviewPresetsRunDefaultsAndGenerality(t *testing.T) {
	keys := map[string]bool{}
	wantSingle := `{"max_turns":60,"max_minutes":30,"time_warn_percent":20,"sub_agents":0,"sub_agents_per_run":0}`
	for _, p := range SeedPresets {
		if strings.HasPrefix(p.Key, "review_") && p.Key != "review_lens_team" || p.Key == "code_quality" || p.Key == "security" || p.Key == "test_engineer" {
			keys[p.Key] = true
			if got := p.RunDefaults.JSON(); got != wantSingle {
				t.Errorf("%s: %s, want %s", p.Key, got, wantSingle)
			}
		}
		if err := p.RunDefaults.Check(); err != nil {
			t.Errorf("%s: %v", p.Key, err)
		}
		for _, bad := range []string{"CUDA", "exported cores", "per-model special cases", "other Code Review presets"} {
			if strings.Contains(p.Instruction, bad) {
				t.Errorf("%s contains repo-specific or broken text %q", p.Key, bad)
			}
		}
	}
	if len(keys) != 11 {
		t.Errorf("%d code review presets: %v", len(keys), keys)
	}
}

func TestRunDefaultsJSONAndCheck(t *testing.T) {
	if (RunDefaults{}).JSON() != "" || ParseRunDefaults("") != (RunDefaults{}) || ParseRunDefaults("not json") != (RunDefaults{}) {
		t.Error("none is not empty")
	}
	r := NewRunDefaults(1, 240, 0, 16, 0)
	if back := ParseRunDefaults(r.JSON()); back.JSON() != r.JSON() || *back.SubAgentsPerRun != 0 || *back.TimeWarnPercent != 0 {
		t.Errorf("round trip: %s", r.JSON())
	}
	if err := r.Check(); err != nil {
		t.Error(err)
	}
	for what, bad := range map[string]RunDefaults{
		"turns 0": NewRunDefaults(0, 1, 0, 0, 0), "turns 501": NewRunDefaults(501, 1, 0, 0, 0),
		"minutes 241": NewRunDefaults(1, 241, 0, 0, 0), "warn 51": NewRunDefaults(1, 1, 51, 0, 0), "warn -1": NewRunDefaults(1, 1, -1, 0, 0),
		"subs 17": NewRunDefaults(1, 1, 0, 17, 0), "per run 501": NewRunDefaults(1, 1, 0, 0, 501),
	} {
		if bad.Check() == nil {
			t.Errorf("%s was accepted", what)
		}
	}
}
