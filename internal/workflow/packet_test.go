package workflow

import (
	"strings"
	"testing"
)

// {pr:packet} and {issue:packet} are built-in tags Autobot fills in, listed with the others, and
// accepted in a template.
func TestPacketTags(t *testing.T) {
	for kind, tag := range map[string]string{PR: "pr:packet", Issue: "issue:packet"} {
		found := false
		for _, tg := range Tags(kind) {
			if tg.Name == tag {
				found = tg.Auto && tg.Field() && strings.Contains(tg.About, "orientation packet")
			}
		}
		if !found {
			t.Errorf("%s: {%s} is not an auto field tag in the list", kind, tag)
		}
		if err := CheckTemplates(kind, nil, "{"+tag+"}"); err != nil {
			t.Errorf("%s: {%s}: %v", kind, tag, err)
		}
		if got := Render("[{"+tag+"}]", map[string]string{tag: "PACKET"}); got != "[PACKET]" {
			t.Errorf("%s rendered %q", tag, got)
		}
	}
	// The packet belongs to the pull request in a pull request workflow, and to the issue in an issue one.
	if err := CheckTemplates(PR, nil, "{issue:packet}"); err == nil {
		t.Error("{issue:packet} was accepted in a pull request workflow")
	}
	if err := CheckTemplates(Issue, nil, "{pr:packet}"); err == nil {
		t.Error("{pr:packet} was accepted in an issue workflow")
	}
}

// Gather Information carries the orientation packet in its head, names the angles, and ends with its
// exact Done when; no other preset carries the packet.
func TestGatherInformationPreset(t *testing.T) {
	g := seed("gather_information")
	if g == nil {
		t.Fatal("no Gather Information preset")
	}
	const done = "Every angle has been looked into and has entries in the scratchpad, and every lead in the orientation packet has been followed or noted as not relevant."
	if g.Goal != done || g.Issue != done {
		t.Errorf("Done when: %q / %q", g.Goal, g.Issue)
	}
	if !strings.HasPrefix(g.Instruction, "Pull request: {pr}\nTitle: {pr:title}\n\n{pr:body}\n\n{pr:packet}\n\nYou are preparing the case file for this pull request:") {
		t.Errorf("the pull request head: %.200q", g.Instruction)
	}
	if !strings.HasPrefix(g.IssueInstruction, "Issue: {issue}\nTitle: {issue:title}\n\n{issue:body}\n\n{issue:packet}\n\nYou are preparing the case file for this issue:") {
		t.Errorf("the issue head: %.200q", g.IssueInstruction)
	}
	for _, want := range []string{
		"Start from the orientation packet above: it is a map of where to look, not the answer. Cover each of these angles, as wide as it needs to be:",
		"- The change: what it does, in plain terms, and how big it is;", "- The why: ", "- Related work: ", "- Recent activity: ", "- People: ", "- Community: ",
		"- Project fit: ", "- Claims and evidence: ", "- Reach: ", "- Process: draft or ready, CI results, review state, age.",
		"Record as you go. Every detail that could matter to someone deciding about this change goes into your scratchpad as its own entry, the moment you find it, with its link or citation. Start each title with its angle (\"Related work: #477 overlaps the same files\", \"People: author's earlier compression series\").",
		"Importance is how soon a reader should see it: high for what a reader could not learn from the description (undeclared changes, reversed or overlapping work, claims without evidence, tensions with the charter); medium for relevant context; low for small details that are still relevant.",
		"this is not a code review: keep implementation detail to what explains the change. Keep what you verified apart from what is claimed, and give every fact its link or citation.",
	} {
		if !strings.Contains(g.Instruction, want) {
			t.Errorf("the pull request instruction lacks %q", want)
		}
	}
	for _, want := range []string{
		"- The report: what is reported or asked for, plainly, and how to reproduce it if it says.", "- The why and impact: ", "- Related work: ", "- Recent activity: ",
		"- People: the reporter", "- Community: ", "- Project fit: ", "- Claims and evidence: logs, versions, measurements", "- Reach: ", "- Process: labels, age, assignment.",
		"Record as you go. Every detail that could matter to someone deciding about this issue goes into your scratchpad", "Importance is how soon a reader should see it:",
		"this is not a code review: keep implementation detail to what explains the issue.",
	} {
		if !strings.Contains(g.IssueInstruction, want) {
			t.Errorf("the issue instruction lacks %q", want)
		}
	}
	if g.Closing != ClosingReport {
		t.Errorf("closing %q", g.Closing)
	}
	for _, p := range SeedPresets {
		if p.Key == "gather_information" {
			continue
		}
		texts := []string{p.Instruction, p.IssueInstruction, p.Goal, p.Issue}
		if strings.HasPrefix(p.Key, "review_") { // the code review presets carry it in their pull request head, and nowhere else
			if !strings.Contains(p.Instruction, "{pr:packet}") {
				t.Errorf("review preset %s has no packet", p.Key)
			}
			texts = []string{p.IssueInstruction, p.Goal, p.Issue}
		}
		for _, text := range texts {
			if strings.Contains(text, ":packet}") {
				t.Errorf("preset %s carries the packet", p.Key)
			}
		}
	}
	// Both versions pass the template check for their kind.
	if err := CheckTemplates(PR, nil, g.Instruction, g.Goal); err != nil {
		t.Errorf("pull request preset: %v", err)
	}
	if err := CheckTemplates(Issue, nil, g.IssueInstruction, g.Issue); err != nil {
		t.Errorf("issue preset: %v", err)
	}
}
