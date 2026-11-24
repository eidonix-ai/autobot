package web

import (
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

var reviewKeys = []string{"review_all", "review_correctness", "review_architecture", "review_conventions", "review_simplicity", "review_prose"}

// olderDB is a database as production has it: seeded with the original five presets under their
// original names, before the code review presets existed. It is not yet served: New is what runs the
// second seed step.
func olderDB(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UseKey(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var old []store.Preset
	for _, p := range seedRows() {
		if strings.HasPrefix(p.Key, "review_") {
			continue
		}
		for _, r := range workflow.PresetRenames {
			if r.Key == p.Key {
				p.Name = r.From
			}
		}
		old = append(old, p)
	}
	if len(old) != 5 {
		t.Fatalf("%d original presets", len(old))
	}
	if err := st.SeedPresets(old); err != nil {
		t.Fatal(err)
	}
	return st
}

// snapshot is every preset's key, name and texts, in order, with its id.
func snapshot(t *testing.T, st *store.Store) []string {
	t.Helper()
	all, err := st.Presets()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range all {
		out = append(out, fmt.Sprintf("%d|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%d", p.ID, p.Key, p.Name, p.Description, p.PRInstruction, p.PRGoal, p.PRSummary,
			p.IssueInstruction, p.IssueGoal, p.IssueSummary, p.Closing, p.OwnerID))
	}
	return out
}

func names(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	all, _ := st.Presets()
	out := map[string]string{}
	for _, p := range all {
		out[p.Key] = p.Name
	}
	return out
}

// The second seed step renames only the presets that still have their original names: an admin's own
// name stays. The keys do not change, so a block on code_quality still resolves.
func TestReviewSeedRenamesOnlyUntouchedNames(t *testing.T) {
	st := olderDB(t)
	admin, err := st.CreateUser("derek", "", "password-1234", true)
	if err != nil {
		t.Fatal(err)
	}
	te, _ := st.PresetByKey("test_engineer")
	edit := *te
	edit.Name = "Our testers"
	if err := st.UpdatePreset(te.ID, edit, admin); err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	all, _ := st.Presets()
	for _, p := range all {
		ids[p.Key] = p.ID
	}
	// A block on code_quality, as production has them.
	a := workflow.DefaultAgent(workflow.PR)
	a.Persona = "code_quality"
	b, _ := json.Marshal(a)
	n := workflow.Node{ID: "ag", Type: workflow.Agent, Settings: b}

	ui, err := New(st, fakeGitHub{})
	if err != nil {
		t.Fatal(err)
	}
	got := names(t, st)
	for key, want := range map[string]string{"code_quality": "Code Review - Quality", "security": "Code Review - Security", "test_engineer": "Our testers",
		"gather_information": "Gather Information", "value_analyst": "Value Analyst"} {
		if got[key] != want {
			t.Errorf("%s is named %q, want %q", key, got[key], want)
		}
	}
	after, _ := st.Presets()
	for _, p := range after {
		if id, was := ids[p.Key]; was && id != p.ID {
			t.Errorf("%s changed its row", p.Key)
		}
	}
	for key := range ids {
		if p, err := st.PresetByKey(key); err != nil || p.ID != ids[key] {
			t.Errorf("the key %s lost its preset: %v", key, err)
		}
	}
	// The block still resolves to the preset, now with its new name.
	s, err := ui.agentSettings(n, workflow.PR)
	if err != nil || !s.OnPreset() || !strings.Contains(s.EffectiveInstruction(workflow.PR), "correctness first") {
		t.Errorf("a block on code_quality: %+v %v", s, err)
	}
	if p, _ := st.PresetByKey("code_quality"); p.PRGoal == "" || asPersona(p).Name != "Code Review - Quality" {
		t.Errorf("the preset behind the block: %+v", p)
	}
}

// The six code review presets go in once; the second step, run again at the next start or called
// directly, changes nothing, and a preset an admin removes stays removed.
func TestReviewSeedInsertsOnceWithoutDuplicates(t *testing.T) {
	st := olderDB(t)
	if all, _ := st.Presets(); len(all) != 5 {
		t.Fatalf("%d presets before", len(all))
	}
	if _, err := New(st, fakeGitHub{}); err != nil {
		t.Fatal(err)
	}
	all, _ := st.Presets()
	if len(all) != 11 {
		t.Fatalf("%d presets after", len(all))
	}
	for _, k := range reviewKeys {
		p, err := st.PresetByKey(k)
		if err != nil || !p.Shared() {
			t.Errorf("%s: %+v %v", k, p, err)
		}
	}
	first := snapshot(t, st)
	if _, err := New(st, fakeGitHub{}); err != nil {
		t.Fatal(err)
	}
	if err := st.SeedPresetsV2(presetRenames(), reviewRows()); err != nil {
		t.Fatal(err)
	}
	if again := snapshot(t, st); !slices.Equal(first, again) {
		t.Errorf("running it again changed the presets:\n%v\n%v", first, again)
	}
	// Removed afterwards, a preset does not come back.
	p, _ := st.PresetByKey("review_prose")
	if err := st.RemovePreset(p.ID, func(store.StoredGraph) (string, bool, error) { return "", false, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := New(st, fakeGitHub{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PresetByKey("review_prose"); err == nil {
		t.Error("a removed review preset came back")
	}
}

// A key that is already taken (a person's own preset with it) is not overwritten or duplicated.
func TestReviewSeedKeepsAPresetThatHasTheKey(t *testing.T) {
	st := olderDB(t)
	admin, _ := st.CreateUser("derek", "", "password-1234", true)
	mine, err := st.CreatePreset(store.Preset{Name: "review_all", Description: "mine", OwnerID: admin.ID, PRInstruction: "pr", PRGoal: "g", IssueInstruction: "i", IssueGoal: "g"}, admin)
	if err != nil || mine.Key != "review_all" {
		t.Fatalf("%+v %v", mine, err)
	}
	if _, err := New(st, fakeGitHub{}); err != nil {
		t.Fatal(err)
	}
	p, _ := st.PresetByKey("review_all")
	if p.ID != mine.ID || p.Description != "mine" || p.Shared() {
		t.Errorf("the existing preset was replaced: %+v", p)
	}
	if all, _ := st.Presets(); len(all) != 11 {
		t.Errorf("%d presets", len(all))
	}
}

// A new database has every preset from the seeds, with the new names, and the second step has nothing
// left to do.
func TestReviewSeedOnAFreshDatabase(t *testing.T) {
	e := newEnv(t)
	got := names(t, e.st)
	want := map[string]string{"gather_information": "Gather Information", "value_analyst": "Value Analyst",
		"code_quality": "Code Review - Quality", "security": "Code Review - Security", "test_engineer": "Code Review - Testing",
		"review_all": "Code Review - All Lenses", "review_correctness": "Code Review - Correctness", "review_architecture": "Code Review - Architecture",
		"review_conventions": "Code Review - Conventions", "review_simplicity": "Code Review - Simplicity", "review_prose": "Code Review - Prose"}
	if len(got) != len(want) {
		t.Errorf("presets: %v", got)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s is named %q, want %q", k, got[k], n)
		}
	}
	// The seeds themselves carry it all, so the first seed is complete without the second step.
	seeds := map[string]string{}
	for _, p := range seedRows() {
		seeds[p.Key] = p.Name
	}
	if !maps.Equal(seeds, want) {
		t.Errorf("the seeds: %v", seeds)
	}
	before := snapshot(t, e.st)
	if err := e.st.SeedPresetsV2(presetRenames(), reviewRows()); err != nil {
		t.Fatal(err)
	}
	if _, err := New(e.st, fakeGitHub{}); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, e.st); !slices.Equal(before, after) {
		t.Error("the second step changed a new database")
	}
}

const (
	wantPacketHead = "Pull request: {pr}\nTitle: {pr:title}\n\n{pr:body}\n\n{pr:packet}\n\n"
	wantDone       = "Every change has been read in context through this review's lens, and the report lists each finding with its severity, file and line, why it matters and the fix, or says plainly that there are none."
	wantFormat     = "**Verdict:** <blocking issues / should-fix issues only / clean>\n**Blocking:**\n- <file:line: the issue, in one line>   (up to 5; \"None.\")\n**Should fix:**\n- <file:line: the issue, in one line>   (up to 5; \"None.\")\n**Nits:** <how many, or \"None.\">\n**Link:** {pr}"
	wantRules      = `How to review:
- Read the project's own rules first: its charter (read_charter), and the contributor, design-review and writing guidance in the repository (AGENTS.md, CONTRIBUTING, docs on design and review). Judge against those, and cite them.
- Stay inside your lens: other reviewers cover the other lenses.
- Read each changed function in its surrounding code, not only the diff, and follow its callers when behaviour changes.
- For every finding give: its severity (blocking, should fix, or nit), the file and line, what is wrong, why it matters, and the fix.
- Before you report a finding, check it in the code. Drop anything you cannot show from the code; say what would settle it if only a test can.
- For GPU kernel code (CUDA and other low-level GPU paths), report defects only; do not propose designs for it.
- Record each finding in your scratchpad as you go, with its severity at the start of the title.
- If you find nothing in your lens, say so plainly.`
)

// Each code review preset has the packet in its head, its lens text and then the shared rules, the
// exact Done when and summary format, the report as its closing step, and issue texts that say it is
// for pull requests; it passes the checks a saved preset does.
func TestReviewPresetsTexts(t *testing.T) {
	e := newEnv(t)
	lens := map[string]string{
		"review_all":          "You are the first reviewer of this pull request, covering all five review lenses: A. Correctness and consequences; B. Architecture and scope; C. Conventions and duplication; D. Simplicity; E. Prose. (Each lens is described in the project's other Code Review presets: correctness everywhere the change reaches; whether it belongs and the approach is right; consistency with precedent and no duplicated helpers; what can be deleted; and the quality of the description, commits, docs and comments.) Give the findings grouped by lens.",
		"review_correctness":  "Your lens is A, correctness and consequences: does it work everywhere it reaches? Check every caller, and every configuration the project supports: each model, cache scheme, parallel layout, multi-host setup and hardware target its documentation names. Check resource lifetimes, inactive and edge-case paths, state and graph transitions, and whether existing data, artifacts and exported cores still load.",
		"review_architecture": "Your lens is B, architecture and scope: does this belong in the repository, and is the approach right? Judge it against the project's rejection criteria and design-review principles as its documents state them: for example keeping the core simple, keeping each layer in its role, no per-model special cases or flags, compatibility with what exists, and measured evidence for performance claims.",
		"review_conventions":  "Your lens is C, conventions and duplication: compare each new construct with its closest precedent in the repository and flag where it departs from the established way, citing the precedent. Flag any existing helper or mechanism it duplicates, citing it.",
		"review_simplicity":   "Your lens is D, simplicity: what can be deleted? Look for unneeded code, options, fallbacks and parallel paths, tests that do not prove a behaviour, and bloated comments. For each, show the smaller version.",
		"review_prose":        "Your lens is E, prose: check the pull request's description, its commit messages, and the docs and comments it changes against the repository's writing rules. Check that the description has the four parts: why, what changed, the claimed result, and the evidence.",
	}
	for _, key := range reviewKeys {
		p, err := e.st.PresetByKey(key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if want := wantPacketHead + lens[key] + "\n\n" + wantRules; p.PRInstruction != want {
			t.Errorf("%s: the instruction is\n%q\nwant\n%q", key, p.PRInstruction, want)
		}
		if p.PRGoal != wantDone || p.PRSummary != wantFormat || p.Closing != "report" {
			t.Errorf("%s: done %q, format %q, closing %q", key, p.PRGoal, p.PRSummary, p.Closing)
		}
		if p.IssueInstruction != "This preset reviews pull requests; use it on a pull-request workflow." || p.IssueGoal != "Not applicable to issues." || p.IssueSummary != "" {
			t.Errorf("%s: issue side %q / %q / %q", key, p.IssueInstruction, p.IssueGoal, p.IssueSummary)
		}
		if p.Description == "" || strings.Contains(p.Description, "\n") || !p.Shared() {
			t.Errorf("%s: description %q, shared %v", key, p.Description, p.Shared())
		}
		if err := checkPresetTexts(p); err != nil {
			t.Errorf("%s: %v", key, err)
		}
		if err := store.CheckPreset(p); err != nil {
			t.Errorf("%s: %v", key, err)
		}
		if err := workflow.CheckTemplates(workflow.PR, nil, p.PRInstruction, p.PRGoal, p.PRSummary); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
}
