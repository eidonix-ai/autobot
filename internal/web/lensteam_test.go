package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// The Lens Team is put into an older database once, as a shared preset that writes its report from the
// scratchpad and starts blocks with six sub-agents; a removed one stays removed.
func TestLensTeamPresetIsSeededOnce(t *testing.T) {
	st := olderDB(t)
	if _, err := New(st, fakeGitHub{}); err != nil {
		t.Fatal(err)
	}
	p, err := st.PresetByKey("review_lens_team")
	if err != nil || !p.Shared() || p.Name != "Code Review - Lens Team" || p.Closing != workflow.ClosingReport || p.NoCitationMarks ||
		!strings.Contains(p.PRInstruction, "{pr:packet}") || !strings.Contains(p.PRInstruction, "spawn_agent") || !strings.Contains(p.PRSummary, "**By lens:**") ||
		p.RunDefaults != `{"max_turns":60,"max_minutes":60,"time_warn_percent":20,"sub_agents":6,"sub_agents_per_run":0}` {
		t.Fatalf("the lens team preset: %+v %v", p, err)
	}
	if err := checkPresetTexts(p); err != nil {
		t.Error(err)
	}
	if err := st.RemovePreset(p.ID, func(store.StoredGraph) (string, bool, error) { return "", false, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := New(st, fakeGitHub{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PresetByKey("review_lens_team"); err == nil {
		t.Error("a removed Lens Team preset came back")
	}
}

// The code review presets of an older database get their starting run settings once, where they have
// none: a preset that already has some keeps them, and an admin's later change is not undone by a start.
func TestReviewPresetsGetRunDefaultsOnce(t *testing.T) {
	st := olderDB(t)
	admin, err := st.CreateUser("derek", "", "password-1234", true)
	if err != nil {
		t.Fatal(err)
	}
	cq, _ := st.PresetByKey("code_quality")
	if cq.RunDefaults != "" {
		t.Fatalf("an older database has run defaults: %q", cq.RunDefaults)
	}
	sec, _ := st.PresetByKey("security")
	sec.RunDefaults = `{"max_turns":99}`
	if err := st.UpdatePreset(sec.ID, *sec, admin); err != nil {
		t.Fatal(err)
	}
	if _, err := New(st, fakeGitHub{}); err != nil {
		t.Fatal(err)
	}
	const want = `{"max_turns":60,"max_minutes":30,"time_warn_percent":20,"sub_agents":0,"sub_agents_per_run":0}`
	for _, key := range []string{"code_quality", "test_engineer", "review_all", "review_prose", "review_consequences", "review_subtraction", "review_pretest"} {
		if p, err := st.PresetByKey(key); err != nil || p.RunDefaults != want {
			t.Errorf("%s: %q %v", key, p.RunDefaults, err)
		}
	}
	if p, _ := st.PresetByKey("security"); p.RunDefaults != `{"max_turns":99}` {
		t.Errorf("a preset's own starting settings were replaced: %q", p.RunDefaults)
	}
	if p, _ := st.PresetByKey("gather_information"); p.RunDefaults != want {
		t.Errorf("Gather Information starting settings: %q, want %q", p.RunDefaults, want)
	}
	// An admin changes one and clears another; the next start leaves both.
	cq, _ = st.PresetByKey("code_quality")
	cq.RunDefaults = `{"max_turns":10}`
	te, _ := st.PresetByKey("test_engineer")
	te.RunDefaults = ""
	for _, p := range []*store.Preset{cq, te} {
		if err := st.UpdatePreset(p.ID, *p, admin); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := New(st, fakeGitHub{}); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.PresetByKey("code_quality"); p.RunDefaults != `{"max_turns":10}` {
		t.Errorf("an admin's edit was overwritten: %q", p.RunDefaults)
	}
	if p, _ := st.PresetByKey("test_engineer"); p.RunDefaults != "" {
		t.Errorf("a cleared preset got its settings back: %q", p.RunDefaults)
	}
}

// The preset form saves the five starting settings (a blank is none, a 0 is a value), shows them,
// refuses values outside a block's bounds, keeps a copy's, and shows them read-only to others.
func TestPresetFormStartingSettings(t *testing.T) {
	p := newPresetEnv(t)
	_, page := p.be.get("/presets/new")
	for _, name := range []string{"rd_max_turns", "rd_max_minutes", "rd_time_warn_percent", "rd_sub_agents", "rd_sub_agents_per_run"} {
		if !strings.Contains(page, `name="`+name+`"`) {
			t.Errorf("no %s on the new preset form", name)
		}
	}
	if !strings.Contains(page, "Starting settings for blocks") || !strings.Contains(page, "blocks already on the preset are not changed") {
		t.Error("the section's heading or note is missing")
	}
	mine := p.create(p.be, "Starts", url.Values{"rd_max_turns": {"40"}, "rd_max_minutes": {""}, "rd_time_warn_percent": {"0"}, "rd_sub_agents": {"3"}, "rd_sub_agents_per_run": {"0"}})
	if mine.RunDefaults != `{"max_turns":40,"time_warn_percent":0,"sub_agents":3,"sub_agents_per_run":0}` {
		t.Fatalf("stored: %q", mine.RunDefaults)
	}
	_, page = p.be.get(presetURL(mine, ""))
	for _, want := range []string{`name="rd_max_turns" min="1" max="500" value="40"`, `name="rd_max_minutes" min="1" max="240" value=""`, `name="rd_time_warn_percent" min="0" max="50" value="0"`,
		`name="rd_sub_agents" min="0" max="16" value="3"`, `name="rd_sub_agents_per_run" min="0" max="500" value="0"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the form lacks %s", want)
		}
	}
	// Changed, and cleared.
	if code, _, body := p.post(p.be, presetURL(mine, ""), p.form("Starts", url.Values{"rd_max_minutes": {"15"}})); code != http.StatusSeeOther {
		t.Fatalf("edit: %d\n%s", code, body)
	}
	if now, _ := p.st.Preset(mine.ID); now.RunDefaults != `{"max_minutes":15}` {
		t.Errorf("after the edit: %q", now.RunDefaults)
	}
	// Refused, as the block's own settings are, and the person's typing stays.
	for what, extra := range map[string]url.Values{"turns 0": {"rd_max_turns": {"0"}}, "turns 501": {"rd_max_turns": {"501"}}, "minutes 241": {"rd_max_minutes": {"241"}},
		"warning 51": {"rd_time_warn_percent": {"51"}}, "sub-agents 17": {"rd_sub_agents": {"17"}}, "per run 501": {"rd_sub_agents_per_run": {"501"}}, "not a number": {"rd_sub_agents": {"many"}}} {
		code, _, body := p.post(p.be, presetURL(mine, ""), p.form("Starts", extra))
		if code != http.StatusBadRequest || !strings.Contains(body, "Starting settings") {
			t.Errorf("%s: %d\n%s", what, code, body)
		}
		if code, _, _ := p.post(p.be, "/presets", p.form("Another", extra)); code != http.StatusBadRequest {
			t.Errorf("%s on a new preset: %d", what, code)
		}
	}
	if now, _ := p.st.Preset(mine.ID); now.RunDefaults != `{"max_minutes":15}` {
		t.Errorf("a refused edit was saved: %q", now.RunDefaults)
	}
	// A copy keeps them; another person sees them read-only.
	code, loc, _ := p.post(p.be, presetURL(mine, "/copy"), nil)
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/presets/") {
		t.Fatalf("copy: %d %q", code, loc)
	}
	copied := false
	all, _ := p.st.Presets()
	for _, c := range all {
		copied = copied || c.Name == "Copy of Starts" && c.RunDefaults == `{"max_minutes":15}`
	}
	if !copied {
		t.Errorf("a copy lost the starting settings: %v", all)
	}
	_, page = p.bf.get(presetURL(mine, ""))
	if !strings.Contains(page, `name="rd_max_minutes" min="1" max="240" value="15" readonly`) {
		t.Errorf("not read-only for someone else:\n%s", page)
	}
}

// A block's Preset dropdown carries each preset's starting settings in a data attribute, empty for a
// preset with none, and the block's own settings are untouched by choosing nothing.
func TestDropdownOptionCarriesRunDefaults(t *testing.T) {
	p := newPresetEnv(t)
	p.saveGraph(p.ws, workflow.PR, p.onPreset("ag", "code_quality"))
	_, page := p.be.get("/w/o/r/workflows/pr/nodes/ag")
	opt := func(key string) string {
		i := strings.Index(page, `<option value="`+key+`" `)
		if i < 0 {
			t.Fatalf("no option %s", key)
		}
		return page[i : i+strings.Index(page[i:], ">")]
	}
	if o := opt("review_lens_team"); !strings.Contains(o, `data-run-defaults="{&#34;max_turns&#34;:60,&#34;max_minutes&#34;:60,&#34;time_warn_percent&#34;:20,&#34;sub_agents&#34;:6,&#34;sub_agents_per_run&#34;:0}"`) {
		t.Errorf("the Lens Team option: %s", o)
	}
	if o := opt("gather_information"); !strings.Contains(o, `data-run-defaults="{&#34;max_turns&#34;:60,&#34;max_minutes&#34;:30,&#34;time_warn_percent&#34;:20,&#34;sub_agents&#34;:0,&#34;sub_agents_per_run&#34;:0}"`) {
		t.Errorf("Gather Information's option: %s", o)
	}
	// Opening the dialog fills nothing in: the block keeps its own 5 turns and 1 minute.
	if !strings.Contains(page, `name="max_turns" min="1" max="500" value="5"`) || !strings.Contains(page, `name="max_minutes" min="1" max="240" value="1"`) {
		t.Errorf("the block's own settings changed:\n%s", page)
	}
}

// On a database seeded before the v9 preset cleanup, untouched shared presets upgrade to the new
// texts, goals, starting settings and tool mode, while admin-edited fields are preserved untouched.
func TestV9CleanupUpgradesUntouchedPresets(t *testing.T) {
	st := olderDB(t)
	admin, err := st.CreateUser("derek", "", "password-1234", true)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an existing production DB where v2..v8 already ran with the pre-cleanup texts.
	if _, err := New(st, fakeGitHub{}); err != nil {
		t.Fatal(err)
	}
	const (
		oldGatherPR = "Pull request: {pr}\nTitle: {pr:title}\n\n{pr:body}\n\n{pr:packet}\n\nYou are preparing the case file for this pull request: everything relevant to it, gathered, linked and cross-referenced, for the people and agents who will decide about it later. Do not judge whether it is good or should be merged. Find out, and connect.\n\nStart from the orientation packet above: it is a map of where to look, not the answer. Cover each of these angles, as wide as it needs to be:\n- The change: what it does, in plain terms, and how big it is; anything it does that its description does not say, and anything the description says that the change does not do.\n- The why: the problem it addresses, linked issues and reports, and how long and how widely it has been wanted.\n- Related work: other pull requests and issues, open or closed, on the same topic or the same code: competing, overlapping, stacked, superseded, or undone by this one.\n- Recent activity: what has changed lately in the areas it touches, by whom and why, and whether this change builds on that work, conflicts with it or reverses it.\n- People: the author's history and standing in the project and what they have been working on; who usually works in these areas; who has engaged so far.\n- Community: comments, reviews and reactions, and how much interest the change has drawn.\n- Project fit: what the project charter, roadmap and documentation say about this area, as facts, both where it lines up and where it does not.\n- Claims and evidence: what it claims (performance, fixes, test results) and what backs each claim, and what does not.\n- Reach: who and what it affects: users, operators, configuration and defaults, compatibility, dependencies, deployment, documentation.\n- Process: draft or ready, CI results, review state, age.\n\nRecord as you go. Every detail that could matter to someone deciding about this change goes into your scratchpad as its own entry, the moment you find it, with its link or citation. Start each title with its angle (\"Related work: #477 overlaps the same files\", \"People: author's earlier compression series\"). Lean towards recording: a detail you leave out cannot reach the report. When a fact connects to another (a commit to a pull request, an issue to a person, a claim to its evidence), say so in the entry, and record the connection itself as an entry when it matters.\n\nImportance is how soon a reader should see it: high for what a reader could not learn from the description (undeclared changes, reversed or overlapping work, claims without evidence, tensions with the charter); medium for relevant context; low for small details that are still relevant.\n\nRead the code as much as you need to understand what it does and what it touches, but this is not a code review: keep implementation detail to what explains the change. Keep what you verified apart from what is claimed, and give every fact its link or citation."
		oldEditorPR = "You are a strict editor. The draft at the end of this message is text that Autobot will post as a comment on GitHub and in Discord. Rewrite it into its final form, held to George Orwell's rules in \"Politics and the English Language\" and Matthew Butterick's Practical Typography.\n\n## How to edit\n\nWork in two passes.\n\n1. Distill. Strip every item to its essentials. For each sentence, phrase and word, ask whether it can go without losing meaning; if it can, cut it. Cut filler, hedges, throat-clearing, restatements, summaries of what was just said, and sentences about the text itself (\"This report covers…\"). Prefer the short word to the long one, the concrete to the abstract, the active voice to the passive, and the everyday word to jargon when the everyday word is exact. Never use a figure of speech you are used to seeing in print. Break any of these rules sooner than write something barbarous.\n\n2. Polish. A last quality pass on how it reads and renders:\n- It must render cleanly as Markdown on GitHub and in Discord: no HTML, no tables (Discord does not show them), headings only where the text has real sections, lists for parallel items, every code span, code fence, bold and link closed.\n- Short paragraphs that read well on a phone; one blank line between blocks; lists at most one level deep.\n- Typography: one space between sentences; bold and italic sparingly and never together; no capitals or underlining for emphasis; no exclamation marks; no emoji the meaning does not need.\n- Keep it under 2,000 characters if the content allows (Discord's limit for a message; past it the text arrives as an attached file).\n\n## What must not change\n\nKeep every fact, number, name, @mention, link, file and line citation, verdict and recommendation as the draft has them, including any \"(not opened)\" marks. Add no facts, opinions, greetings or sign-offs.\n\nWhen done, call finish with the finished text as the report and nothing else: no preface and no notes on your edits. It is posted as it is.\n\n## The draft"
		oldRevGoal  = "Every change has been read in context through this review's lens, and the report lists each finding with its severity, file and line, why it matters and the fix, or says plainly that there are none."
		oldRevRD    = `{"max_turns":60,"max_minutes":30,"time_warn_percent":20}`
	)
	gi, _ := st.PresetByKey("gather_information")
	gi.PRInstruction, gi.RunDefaults = oldGatherPR, ""
	if err := st.UpdatePreset(gi.ID, *gi, admin); err != nil {
		t.Fatal(err)
	}
	ed, _ := st.PresetByKey("editor_prose")
	ed.PRInstruction, ed.NoTools, ed.RunDefaults = oldEditorPR, false, ""
	if err := st.UpdatePreset(ed.ID, *ed, admin); err != nil {
		t.Fatal(err)
	}
	ra, _ := st.PresetByKey("review_all")
	ra.PRInstruction, ra.PRGoal, ra.RunDefaults = "Our custom All Lenses text.", oldRevGoal, oldRevRD
	if err := st.UpdatePreset(ra.ID, *ra, admin); err != nil {
		t.Fatal(err)
	}
	// Run the v9 cleanup step directly under a fresh marker (as production does on upgrade).
	step := presetSeedSteps()[len(presetSeedSteps())-1]
	step.Marker = "presets_v9_cleanup_test"
	if err := st.RunPresetSteps([]store.PresetSeedStep{step}); err != nil {
		t.Fatal(err)
	}
	gi, _ = st.PresetByKey("gather_information")
	if !strings.Contains(gi.PRInstruction, "what a reader could not learn from the description first") ||
		!strings.Contains(gi.PRInstruction, "one section per angle") {
		t.Errorf("Gather Information was not upgraded with closing report guidance:\n%s", gi.PRInstruction)
	}
	if gi.RunDefaults != workflow.ReviewRunDefaults().JSON() {
		t.Errorf("Gather Information run defaults: %q", gi.RunDefaults)
	}
	ed, _ = st.PresetByKey("editor_prose")
	if !ed.NoTools || ed.PRInstruction != workflow.EditorPreset.Instruction || ed.RunDefaults != workflow.EditorPreset.RunDefaults.JSON() {
		t.Errorf("Editor preset not upgraded: NoTools=%v RD=%q\n%s", ed.NoTools, ed.RunDefaults, ed.PRInstruction)
	}
	ra, _ = st.PresetByKey("review_all")
	if ra.PRInstruction != "Our custom All Lenses text." {
		t.Errorf("admin-edited review_all instruction was overwritten: %q", ra.PRInstruction)
	}
	if ra.PRGoal == oldRevGoal || ra.RunDefaults != workflow.ReviewRunDefaults().JSON() {
		t.Errorf("untouched review_all goal/defaults not upgraded: goal=%q rd=%q", ra.PRGoal, ra.RunDefaults)
	}
}
