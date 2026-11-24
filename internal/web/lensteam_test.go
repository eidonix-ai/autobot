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
	const want = `{"max_turns":60,"max_minutes":30,"time_warn_percent":20}`
	for _, key := range []string{"code_quality", "test_engineer", "review_all", "review_prose", "review_consequences", "review_subtraction", "review_pretest"} {
		if p, err := st.PresetByKey(key); err != nil || p.RunDefaults != want {
			t.Errorf("%s: %q %v", key, p.RunDefaults, err)
		}
	}
	if p, _ := st.PresetByKey("security"); p.RunDefaults != `{"max_turns":99}` {
		t.Errorf("a preset's own starting settings were replaced: %q", p.RunDefaults)
	}
	if p, _ := st.PresetByKey("gather_information"); p.RunDefaults != "" {
		t.Errorf("Gather Information got starting settings: %q", p.RunDefaults)
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
	if o := opt("gather_information"); !strings.Contains(o, `data-run-defaults=""`) {
		t.Errorf("Gather Information's option: %s", o)
	}
	// Opening the dialog fills nothing in: the block keeps its own 5 turns and 1 minute.
	if !strings.Contains(page, `name="max_turns" min="1" max="500" value="5"`) || !strings.Contains(page, `name="max_minutes" min="1" max="240" value="1"`) {
		t.Errorf("the block's own settings changed:\n%s", page)
	}
}
