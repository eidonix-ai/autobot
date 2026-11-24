package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// presetEnv is a workspace "o/r" with derek (an admin), erin and frank (users) in it, a provider lent to
// it, and one Agent block "ag" in its pull request workflow.
type presetEnv struct {
	*env
	ws                 *store.Workspace
	derek, erin, frank *store.User
	prov               *store.Provider
	bd, be, bf         *browser
}

func newPresetEnv(t *testing.T) *presetEnv {
	e := newEnv(t)
	p := &presetEnv{env: e}
	p.derek, p.erin, p.frank = e.user("derek", true), e.user("erin", false), e.user("frank", false)
	p.ws, _ = e.st.CreateWorkspace("o/r", p.derek, "", "")
	e.st.SetMembers(p.ws.ID, []int64{p.erin.ID, p.frank.ID}, p.derek)
	p.prov, _ = e.st.CreateProvider(store.ProviderEdit{Name: "LLM", Kind: store.KindOpenAI, Endpoint: "https://llm.example/v1", Key: "k"}, p.derek)
	e.st.SetProviderModels(p.prov.ID, []string{"m"})
	e.st.LendProvider(p.prov, []int64{p.ws.ID}, p.derek)
	p.saveGraph(p.ws, workflow.PR, workflow.Node{ID: "ag", Type: workflow.Agent})
	p.bd, p.be, p.bf = e.signIn("derek"), e.signIn("erin"), e.signIn("frank")
	return p
}

// saveGraph stores a workspace's workflow of a kind from nodes.
func (p *presetEnv) saveGraph(ws *store.Workspace, kind string, nodes ...workflow.Node) {
	p.t.Helper()
	g := workflow.Graph{Nodes: nodes, Links: []workflow.Link{}}
	b, _ := json.Marshal(g)
	w, _ := p.st.Workflow(ws.ID, kind)
	if _, err := p.st.SaveWorkflow(ws.ID, kind, w.Version, string(b), p.derek); err != nil {
		p.t.Fatal(err)
	}
}

// onPreset is an Agent block that follows a preset.
func (p *presetEnv) onPreset(id, key string) workflow.Node {
	a := workflow.DefaultAgent(workflow.PR)
	a.Model, a.Persona, a.MaxTurns, a.MaxMinutes = workflow.ModelRef{Provider: p.prov.ID, Model: "m"}, key, 5, 1
	b, _ := json.Marshal(a)
	return workflow.Node{ID: id, Type: workflow.Agent, Settings: b}
}

func (p *presetEnv) form(name string, extra url.Values) url.Values {
	f := url.Values{"name": {name}, "description": {"does a thing"}, "pr_instruction": {"Review {pr:title}."}, "pr_goal": {"Reviewed."},
		"issue_instruction": {"Look at {issue:title}."}, "issue_goal": {"Looked at."}}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

// create posts a new preset as the browser's person and returns its stored row.
func (p *presetEnv) create(b *browser, name string, extra url.Values) *store.Preset {
	p.t.Helper()
	f := p.form(name, extra)
	f.Set("csrf", b.csrf())
	code, loc, body := b.post("/presets", f)
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/presets/") {
		p.t.Fatalf("create %s: %d %q\n%s", name, code, loc, body)
	}
	id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(strings.SplitN(loc, "?", 2)[0], "/presets/"), "/"), 10, 64)
	made, err := p.st.Preset(id)
	if err != nil {
		p.t.Fatal(err)
	}
	return made
}

func presetURL(p *store.Preset, rest string) string {
	return "/presets/" + strconv.FormatInt(p.ID, 10) + rest
}

func (p *presetEnv) post(b *browser, to string, f url.Values) (int, string, string) {
	p.t.Helper()
	if f == nil {
		f = url.Values{}
	}
	f.Set("csrf", b.csrf())
	return b.post(to, f)
}

// The built-in presets are in the database from the first start, with their keys, and a later start of
// the server does not duplicate them or bring back one an admin removed.
func TestPresetsAreSeededOnce(t *testing.T) {
	p := newPresetEnv(t)
	all, _ := p.st.Presets()
	keys := map[string]bool{}
	for _, x := range all {
		if !x.Shared() {
			t.Errorf("%s is not shared", x.Key)
		}
		keys[x.Key] = true
	}
	for _, k := range []string{"gather_information", "code_quality", "value_analyst", "security", "test_engineer"} {
		if !keys[k] {
			t.Errorf("no preset %s: %v", k, keys)
		}
	}
	if _, err := New(p.st, fakeGitHub{}); err != nil {
		t.Fatal(err)
	}
	if again, _ := p.st.Presets(); len(again) != len(all) {
		t.Fatalf("a second start made %d presets from %d", len(again), len(all))
	}
	g, _ := p.st.PresetByKey("security")
	if code, _, _ := p.post(p.bd, presetURL(g, "/remove"), nil); code != http.StatusSeeOther {
		t.Fatalf("remove: %d", code)
	}
	New(p.st, fakeGitHub{})
	if _, err := p.st.PresetByKey("security"); err == nil {
		t.Fatal("a removed built-in preset came back at the next start")
	}
}

// Every built-in preset passes the same check a saved one does.
func TestSeedPresetsPassTheSaveCheck(t *testing.T) {
	for _, s := range seedRows() {
		s := s
		if err := checkPresetTexts(&s); err != nil {
			t.Errorf("%s: %v", s.Key, err)
		}
		if err := store.CheckPreset(&s); err != nil {
			t.Errorf("%s: %v", s.Key, err)
		}
	}
}

// Gather Information's seed carries the packet in its head, its Done when, and both summary formats, exactly.
func TestGatherInformationSeed(t *testing.T) {
	p := newPresetEnv(t)
	g, err := p.st.PresetByKey("gather_information")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(g.PRInstruction, "Pull request: {pr}\nTitle: {pr:title}\n\n{pr:body}\n\n{pr:packet}\n\nYou are preparing the case file for this pull request") ||
		!strings.HasPrefix(g.IssueInstruction, "Issue: {issue}\nTitle: {issue:title}\n\n{issue:body}\n\n{issue:packet}\n\n") {
		t.Errorf("the heads: %.150q / %.150q", g.PRInstruction, g.IssueInstruction)
	}
	const done = "Every angle has been looked into and has entries in the scratchpad, and every lead in the orientation packet has been followed or noted as not relevant."
	if g.PRGoal != done || g.IssueGoal != done || g.Closing != "report" || g.Owner != "" {
		t.Errorf("goal %q / %q, closing %q", g.PRGoal, g.IssueGoal, g.Closing)
	}
	const prFormat = `**Author:** <login (role in the project); Discord handle if the PR gives one>
**Motivation:** <two sentences: why this is proposed, explained simply, as to a newcomer>
**Scale:** +N / −N lines in N files
**Proposal:**
- <up to 5 bullets: the changes it makes>
**Touches:**
- <up to 3 major modules it changes, most risky first>
**Related items:**
- #N <title> (<open/closed/merged>): <how it relates, a few words>   (up to 5)
**Link:** <the pull request on GitHub>
**Autobot comments:** <anything else highly relevant, explained simply: what the description doesn't mention, work it undoes or overlaps, claims without evidence. "None." if nothing.>`
	const issueFormat = `**Reporter:** <login (role in the project); Discord handle if the issue gives one>
**Problem:** <two sentences: what is wrong or asked for, explained simply, as to a newcomer>
**Affects:**
- <up to 3 areas of the project it concerns, most affected first>
**Related items:**
- #N <title> (<open/closed/merged>): <how it relates, a few words>   (up to 5)
**Link:** <the issue on GitHub>
**Autobot comments:** <anything else highly relevant, explained simply. "None." if nothing.>`
	if g.PRSummary != prFormat || g.IssueSummary != issueFormat {
		t.Errorf("the summary formats:\n%s\n---\n%s", g.PRSummary, g.IssueSummary)
	}
	for _, k := range []string{"code_quality", "value_analyst", "security", "test_engineer"} {
		if o, _ := p.st.PresetByKey(k); o.PRSummary != "" || o.IssueSummary != "" || strings.Contains(o.PRInstruction+o.IssueInstruction, ":packet}") {
			t.Errorf("%s has a summary format or the packet", k)
		}
	}
}

// Who may do what, enforced on the server: admins make and change shared presets, users their own;
// an admin cannot change another user's; anyone can look at and copy any preset.
func TestPresetPermissions(t *testing.T) {
	p := newPresetEnv(t)
	shared := p.create(p.bd, "Team review", url.Values{"shared": {"1"}})
	erins := p.create(p.be, "Erin's review", nil)
	if !shared.Shared() || erins.OwnerID != p.erin.ID || erins.Shared() {
		t.Fatalf("shared %+v, erin's %+v", shared, erins)
	}
	count := func() int { all, _ := p.st.Presets(); return len(all) }
	before := count()

	forbidden := func(who string, b *browser, to string, f url.Values) {
		t.Helper()
		if f == nil {
			f = url.Values{}
		}
		f.Set("csrf", b.csrf())
		if code, _, _ := b.post(to, f); code != http.StatusForbidden {
			t.Errorf("%s: POST %s gave %d, want 403", who, to, code)
		}
	}
	changed := p.form("Hijacked", nil)
	// A user cannot make, change or remove a shared preset.
	forbidden("erin makes a shared one", p.be, "/presets", p.form("Sneaky", url.Values{"shared": {"1"}}))
	forbidden("erin edits the shared one", p.be, presetURL(shared, ""), changed)
	forbidden("erin removes the shared one", p.be, presetURL(shared, "/remove"), nil)
	// A user cannot change or remove another's.
	forbidden("frank edits erin's", p.bf, presetURL(erins, ""), changed)
	forbidden("frank removes erin's", p.bf, presetURL(erins, "/remove"), nil)
	// An admin cannot change a user's either.
	forbidden("derek edits erin's", p.bd, presetURL(erins, ""), changed)
	forbidden("derek removes erin's", p.bd, presetURL(erins, "/remove"), nil)
	if code, _ := p.bf.get(presetURL(erins, "/remove")); code != http.StatusForbidden {
		t.Errorf("frank opened the removal page of erin's preset: %d", code)
	}
	if code, _ := p.be.get(presetURL(shared, "/remove")); code != http.StatusForbidden {
		t.Errorf("erin opened the removal page of a shared preset: %d", code)
	}
	if n := count(); n != before {
		t.Fatalf("a refused request changed the presets: %d, was %d", n, before)
	}
	if now, _ := p.st.Preset(erins.ID); now.Name != "Erin's review" || now.PRInstruction != erins.PRInstruction {
		t.Fatalf("erin's preset was changed: %+v", now)
	}
	if now, _ := p.st.Preset(shared.ID); now.Name != "Team review" {
		t.Fatalf("the shared preset was changed: %+v", now)
	}

	// What they may do.
	if code, _, _ := p.post(p.be, presetURL(erins, ""), p.form("Erin's second", nil)); code != http.StatusSeeOther {
		t.Errorf("erin edits her own: %d", code)
	}
	if code, _, _ := p.post(p.bd, presetURL(shared, ""), p.form("Team review v2", nil)); code != http.StatusSeeOther {
		t.Errorf("derek edits a shared one: %d", code)
	}
	if now, _ := p.st.Preset(erins.ID); now.Name != "Erin's second" || now.Key != erins.Key || now.OwnerID != p.erin.ID {
		t.Errorf("erin's edit: %+v", now)
	}
	if now, _ := p.st.Preset(shared.ID); now.Name != "Team review v2" || !now.Shared() {
		t.Errorf("derek's edit: %+v", now)
	}

	// Everyone signed in can look at any preset, read-only unless they may change it, and copy it.
	for who, b := range map[string]*browser{"frank": p.bf, "derek": p.bd} {
		code, page := b.get(presetURL(erins, ""))
		if code != 200 || !strings.Contains(page, "only erin can change it") || !strings.Contains(page, "readonly") || strings.Contains(page, ">Save<") ||
			!strings.Contains(page, "Copy to my presets") {
			t.Errorf("%s looking at erin's preset: %d\n%s", who, code, page)
		}
	}
	if code, page := p.be.get(presetURL(shared, "")); code != 200 || !strings.Contains(page, "only an admin can change it") || strings.Contains(page, ">Save<") {
		t.Errorf("erin looking at a shared preset: %d\n%s", code, page)
	}
	if code, page := p.bd.get(presetURL(shared, "")); code != 200 || !strings.Contains(page, ">Save<") || !strings.Contains(page, `<textarea name="pr_instruction"`) {
		t.Errorf("derek looking at a shared preset: %d", code)
	}
	if code, _ := p.bf.get("/presets/99999"); code != http.StatusNotFound {
		t.Errorf("a preset that does not exist: %d", code)
	}
	if code, _, _ := p.be.post("/presets", p.form("No token", nil)); code != http.StatusForbidden && code != http.StatusBadRequest {
		t.Errorf("a post without the form token: %d", code)
	}
}

// Copy makes an owned copy of any preset, shared or another user's, with every text, summary format and
// the closing step; it is a separate preset with its own key, which its owner can change.
func TestCopyAPreset(t *testing.T) {
	p := newPresetEnv(t)
	src := p.create(p.be, "Erin's thorough", url.Values{"pr_summary": {"**Verdict:** <one line>"}, "issue_summary": {"**Verdict:** <one word>"}, "closing": {"report"}})
	g, _ := p.st.PresetByKey("gather_information")
	for _, c := range []struct {
		by   *browser
		who  *store.User
		from *store.Preset
	}{{p.bf, p.frank, src}, {p.bf, p.frank, g}, {p.bd, p.derek, src}} {
		code, loc, _ := p.post(c.by, presetURL(c.from, "/copy"), nil)
		if code != http.StatusSeeOther || !strings.HasSuffix(loc, "?copied=1") {
			t.Fatalf("copy %s: %d %q", c.from.Key, code, loc)
		}
		id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(loc, "/presets/"), "?copied=1"), 10, 64)
		got, _ := p.st.Preset(id)
		if got.OwnerID != c.who.ID || got.Shared() || got.Key == c.from.Key || got.Name != "Copy of "+c.from.Name ||
			got.PRInstruction != c.from.PRInstruction || got.PRGoal != c.from.PRGoal || got.PRSummary != c.from.PRSummary ||
			got.IssueInstruction != c.from.IssueInstruction || got.IssueGoal != c.from.IssueGoal || got.IssueSummary != c.from.IssueSummary ||
			got.Closing != c.from.Closing || got.Description != c.from.Description {
			t.Errorf("the copy of %s by %s: %+v", c.from.Key, c.who.Username, got)
		}
	}
	if now, _ := p.st.Preset(src.ID); now.OwnerID != p.erin.ID {
		t.Error("copying changed the original")
	}
}

// What is saved is checked: only built-in tags, as the kind's own, a known closing step, a name.
func TestPresetSaveIsChecked(t *testing.T) {
	p := newPresetEnv(t)
	before := func() int { all, _ := p.st.Presets(); return len(all) }
	n := before()
	for what, c := range map[string]struct {
		extra url.Values
		want  string
	}{
		"an unknown tag in the pull request instruction": {url.Values{"pr_instruction": {"Review {nosuch}."}}, "For pull requests"},
		"an output tag, which no step sets here":         {url.Values{"pr_goal": {"Use {gather.report}."}}, "For pull requests"},
		"the issue's tag in a pull request text":         {url.Values{"pr_instruction": {"See {issue:title}."}}, "For pull requests"},
		"the packet in an issue text":                    {url.Values{"issue_instruction": {"Read {pr:packet}."}}, "For issues"},
		"an unknown tag in a summary format":             {url.Values{"pr_summary": {"**A:** {nosuch}"}}, "For pull requests"},
		"an unknown tag in an issue summary format":      {url.Values{"issue_summary": {"**A:** {nosuch}"}}, "For issues"},
		"a made-up closing step":                         {url.Values{"closing": {"poem"}}, "closing step"},
		"no name":                                        {url.Values{"name": {" "}}, "name"},
		"no pull request goal":                           {url.Values{"pr_goal": {""}}, "when the work is done"},
	} {
		f := p.form("Checked", c.extra)
		code, _, body := p.post(p.be, "/presets", f)
		if code != http.StatusBadRequest || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d, wanted a refusal mentioning %q\n%s", what, code, c.want, body)
		}
	}
	if before() != n {
		t.Fatal("a refused preset was saved")
	}
	// The same checks on an edit, which also keeps what the person typed in the form.
	mine := p.create(p.be, "Mine", nil)
	code, _, body := p.post(p.be, presetURL(mine, ""), p.form("Mine", url.Values{"pr_instruction": {"Typed {nosuch} text"}}))
	if code != http.StatusBadRequest || !strings.Contains(body, "For pull requests") || !strings.Contains(body, "Typed {nosuch} text") {
		t.Errorf("an edit with an unknown tag: %d\n%s", code, body)
	}
	if now, _ := p.st.Preset(mine.ID); now.PRInstruction != mine.PRInstruction {
		t.Error("a refused edit was saved")
	}
	// Built-in tags are fine in all of them, including {input} and the charter.
	ok := p.form("Fine", url.Values{"pr_instruction": {"{pr} {pr:title} {pr:body} {pr:diff} {pr:files} {pr:packet} {charter} {repo} {input}"},
		"pr_summary": {"**Link:** {pr}"}})
	if code, _, body := p.post(p.be, "/presets", ok); code != http.StatusSeeOther {
		t.Errorf("built-in tags: %d\n%s", code, body)
	}
}

// A block follows its preset as it is now: an edit reaches it, in the form and in what it runs.
func TestABlockFollowsAnEditedPreset(t *testing.T) {
	p := newPresetEnv(t)
	shared := p.create(p.bd, "Team review", url.Values{"shared": {"1"}, "pr_summary": {"**Verdict:** <one line>"}, "closing": {"report"}})
	p.saveGraph(p.ws, workflow.PR, p.onPreset("ag", shared.Key))
	node := func() workflow.AgentSettings {
		w, _ := p.st.Workflow(p.ws.ID, workflow.PR)
		var g workflow.Graph
		json.Unmarshal([]byte(w.Graph), &g)
		a, err := p.ui.agentSettings(g.Nodes[0], workflow.PR)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	if a := node(); a.EffectiveInstruction(workflow.PR) != "Review {pr:title}." || a.EffectiveSummaryFormat(workflow.PR) != "**Verdict:** <one line>" || a.EffectiveClosing(workflow.PR) != "report" || !a.OnPreset() {
		t.Fatalf("before: %+v", a)
	}
	if _, page := p.be.get("/w/o/r/workflows/pr/nodes/ag"); !strings.Contains(page, "Review {pr:title}.</textarea>") {
		t.Fatalf("the block's form:\n%s", page)
	}
	p.post(p.bd, presetURL(shared, ""), p.form("Team review", url.Values{"pr_instruction": {"A better look at {pr:title}."}, "pr_summary": {"**Verdict:** <one word>"}, "closing": {""}}))
	if a := node(); a.EffectiveInstruction(workflow.PR) != "A better look at {pr:title}." || a.EffectiveSummaryFormat(workflow.PR) != "**Verdict:** <one word>" || a.EffectiveClosing(workflow.PR) != "" {
		t.Fatalf("after the edit: %+v", a)
	}
	if _, page := p.be.get("/w/o/r/workflows/pr/nodes/ag"); !strings.Contains(page, "A better look at {pr:title}.</textarea>") || strings.Contains(page, "Review {pr:title}.</textarea>") {
		t.Fatalf("the block's form after the edit:\n%s", page)
	}
	// The editing page tells an admin who follows it.
	p.saveGraph(p.ws, workflow.PR, p.onPreset("ag", shared.Key), p.onPreset("ag2", shared.Key))
	if _, page := p.bd.get(presetURL(shared, "")); !strings.Contains(page, "Used by 2 blocks in 1 workspace: they will follow the new text.") {
		t.Fatalf("the usage notice:\n%s", page)
	}
}

// Who may give a block which preset: Custom, a shared one, their own, or the one it already follows.
func TestChoosingAPresetForABlock(t *testing.T) {
	p := newPresetEnv(t)
	franks := p.create(p.bf, "Frank's own", nil)
	erins := p.create(p.be, "Erin's own", nil)
	save := func(b *browser, persona string) (int, string) {
		t.Helper()
		w, _ := p.st.Workflow(p.ws.ID, workflow.PR)
		code, _, body := b.post("/w/o/r/workflows/pr/nodes/ag", url.Values{"csrf": {b.csrf()}, "version": {strconv.Itoa(w.Version)},
			"name": {"Facts"}, "model": {strconv.FormatInt(p.prov.ID, 10) + ":m"}, "persona": {persona}, "instruction": {"my own"}, "goal": {"my goal"},
			"max_turns": {"40"}, "max_minutes": {"20"}, "rerun": {"resume"}})
		return code, body
	}
	stored := func() string {
		w, _ := p.st.Workflow(p.ws.ID, workflow.PR)
		var g workflow.Graph
		json.Unmarshal([]byte(w.Graph), &g)
		a, _ := workflow.AgentSettingsOf(g.Nodes[0], workflow.PR)
		return a.Persona
	}
	if code, body := save(p.be, franks.Key); code != http.StatusBadRequest || !strings.Contains(body, "belongs to frank") || stored() == franks.Key {
		t.Fatalf("erin gave a block frank's preset: %d (%s)\n%s", code, stored(), body)
	}
	if code, body := save(p.bd, franks.Key); code != http.StatusBadRequest || !strings.Contains(body, "belongs to frank") {
		t.Fatalf("an admin gave a block frank's preset: %d\n%s", code, body)
	}
	if code, body := save(p.be, "no_such_preset"); code != http.StatusBadRequest || !strings.Contains(body, "does not exist") {
		t.Fatalf("a preset that does not exist: %d\n%s", code, body)
	}
	for _, key := range []string{erins.Key, "code_quality", "custom"} {
		if code, body := save(p.be, key); code != 200 || stored() != key {
			t.Fatalf("erin chose %s: %d (%s)\n%s", key, code, stored(), body)
		}
	}
	// A block that already follows someone else's preset keeps it, whoever saves it; choosing it again
	// after changing away is a new choice, and refused.
	p.saveGraph(p.ws, workflow.PR, p.onPreset("ag", franks.Key))
	if code, body := save(p.be, franks.Key); code != 200 || stored() != franks.Key {
		t.Fatalf("saving a block that already follows it: %d (%s)\n%s", code, stored(), body)
	}
	save(p.be, "custom")
	if code, _ := save(p.be, franks.Key); code != http.StatusBadRequest || stored() != "custom" {
		t.Fatalf("choosing it again after leaving it: %d (%s)", code, stored())
	}
	// A preset kept on the block is not stored as text on it.
	save(p.be, erins.Key)
	w, _ := p.st.Workflow(p.ws.ID, workflow.PR)
	if strings.Contains(w.Graph, "my own") || strings.Contains(w.Graph, "my goal") {
		t.Errorf("a preset's block stores text of its own: %s", w.Graph)
	}
}

// The block's dropdown groups Shared and Mine, shows the preset it follows if it is someone else's marked
// with its owner (read-only, with a way to copy it), and never offers other people's.
func TestPresetDropdownGroups(t *testing.T) {
	p := newPresetEnv(t)
	erins := p.create(p.be, "Erin's own", nil)
	franks := p.create(p.bf, "Frank's own", url.Values{"pr_summary": {"**V:** <x>"}})
	p.saveGraph(p.ws, workflow.PR, p.onPreset("ag", "code_quality"))
	_, page := p.be.get("/w/o/r/workflows/pr/nodes/ag")
	shared := between(page, `<optgroup label="Shared">`, `</optgroup>`)
	mine := between(page, `<optgroup label="Mine">`, `</optgroup>`)
	if !strings.Contains(shared, `value="gather_information"`) || !strings.Contains(shared, `value="code_quality" data-instruction=`) || !strings.Contains(shared, " selected>Code Quality") ||
		strings.Contains(shared, erins.Key) || !strings.Contains(mine, `value="`+erins.Key+`"`) || strings.Contains(mine, "gather_information") ||
		strings.Contains(page, franks.Key) || strings.Contains(page, `label="In use"`) || strings.Contains(page, "Copy to my presets") {
		t.Fatalf("erin's dropdown:\n%s", page)
	}
	if !strings.Contains(page, `<option value="custom">Custom</option>`) {
		t.Errorf("no Custom")
	}
	// Frank's has Frank's own, not Erin's.
	_, page = p.bf.get("/w/o/r/workflows/pr/nodes/ag")
	if mine := between(page, `<optgroup label="Mine">`, `</optgroup>`); !strings.Contains(mine, franks.Key) || strings.Contains(page, erins.Key) {
		t.Fatalf("frank's dropdown:\n%s", page)
	}
	// A block on frank's preset, seen by erin: the preset is offered, marked, selected, read-only, copyable.
	p.saveGraph(p.ws, workflow.PR, p.onPreset("ag", franks.Key))
	_, page = p.be.get("/w/o/r/workflows/pr/nodes/ag")
	inUse := between(page, `<optgroup label="In use">`, `</optgroup>`)
	if !strings.Contains(inUse, `value="`+franks.Key+`"`) || !strings.Contains(inUse, " selected>Frank&#39;s own (by frank)") || !strings.Contains(inUse, `data-summary="**V:** &lt;x&gt;"`) ||
		!strings.Contains(page, `<a href="/presets/`+strconv.FormatInt(franks.ID, 10)+`" target="_blank" rel="noopener">Copy to my presets</a>`) ||
		!strings.Contains(page, `name="instruction" rows="9" class="code" spellcheck="false" required readonly>`) {
		t.Fatalf("erin's dropdown on frank's block:\n%s", page)
	}
	// A block whose preset is gone says so.
	p.saveGraph(p.ws, workflow.PR, p.onPreset("ag", "long_gone"))
	if _, page = p.be.get("/w/o/r/workflows/pr/nodes/ag"); !strings.Contains(page, "no longer exists") {
		t.Errorf("a missing preset is not said:\n%s", page)
	}
}

func between(s, from, to string) string {
	_, rest, ok := strings.Cut(s, from)
	if !ok {
		return ""
	}
	rest, _, _ = strings.Cut(rest, to)
	return rest
}

// The Presets page lists Shared and Mine with how many blocks use each, an admin also sees others' (read-only),
// and a user does not.
func TestPresetListPage(t *testing.T) {
	p := newPresetEnv(t)
	erins := p.create(p.be, "Erin's own", nil)
	franks := p.create(p.bf, "Frank's own", nil)
	p.saveGraph(p.ws, workflow.PR, p.onPreset("a", erins.Key), p.onPreset("b", erins.Key), p.onPreset("c", "code_quality"))
	code, page := p.be.get("/presets")
	shared, mine := between(page, "<h2>Shared</h2>", "</section>"), between(page, "<h2>Mine</h2>", "</section>")
	if code != 200 || !strings.Contains(shared, "Gather Information") || !strings.Contains(shared, "1 block in 1 workspace") || !strings.Contains(mine, "Erin&#39;s own") ||
		!strings.Contains(mine, "2 blocks in 1 workspace") || strings.Contains(page, "Frank&#39;s own") || strings.Contains(page, "Other users") ||
		!strings.Contains(page, `href="/presets/new"`) || strings.Contains(page, "shared=1") {
		t.Fatalf("erin's list: %d\n%s", code, page)
	}
	if !strings.Contains(shared, `href="/presets/`) || strings.Count(mine, ">Open<") != 1 {
		t.Errorf("one action per row:\n%s", mine)
	}
	_, page = p.bd.get("/presets")
	if others := between(page, "<h2>Other users' presets</h2>", "</section>"); !strings.Contains(others, "Erin&#39;s own") || !strings.Contains(others, "Frank&#39;s own") ||
		!strings.Contains(others, ">erin<") || !strings.Contains(page, "shared=1") || !strings.Contains(between(page, "<h2>Mine</h2>", "</section>"), "You have none yet") {
		t.Fatalf("derek's list:\n%s", page)
	}
	if code, _ := p.bd.get(presetURL(franks, "")); code != 200 {
		t.Errorf("an admin looking at another user's preset: %d", code)
	}
	if _, nav := p.be.get("/"); !strings.Contains(nav, `<a href="/presets">Presets</a>`) {
		t.Error("no Presets link in the top bar")
	}
	// Only an admin gets the shared choice in the new-preset form.
	if _, form := p.be.get("/presets/new?shared=1"); strings.Contains(form, `name="shared"`) {
		t.Error("a user is offered to share")
	}
	if _, form := p.bd.get("/presets/new?shared=1"); !strings.Contains(form, `name="shared"`) || !strings.Contains(form, `<option value="1" selected>`) {
		t.Errorf("an admin is not offered to share:\n%s", form)
	}
}

// Removing a preset is two steps. The first page says how many blocks will switch; the second switches
// every block that followed it, in every workspace and both kinds of workflow, to Custom with the preset's
// own text (the one for its kind), summary format and closing step; others are left alone.
func TestRemovingAPresetSwitchesItsBlocks(t *testing.T) {
	p := newPresetEnv(t)
	other, _ := p.st.CreateWorkspace("o/two", p.derek, "", "")
	p.st.SetMembers(other.ID, []int64{p.erin.ID}, p.derek)
	erins := p.create(p.be, "Erin's own", url.Values{"pr_instruction": {"PR text {pr:title}"}, "pr_goal": {"PR done"}, "pr_summary": {"**PR:** <x>"},
		"issue_instruction": {"Issue text {issue:title}"}, "issue_goal": {"Issue done"}, "issue_summary": {"**Issue:** <y>"}, "closing": {"report"}})
	p.saveGraph(p.ws, workflow.PR, p.onPreset("a", erins.Key), p.onPreset("b", "code_quality"))
	p.saveGraph(other, workflow.PR, p.onPreset("c", erins.Key))
	issueBlock := p.onPreset("d", erins.Key)
	p.saveGraph(other, workflow.Issue, issueBlock)
	versions := map[string]int{}
	for _, w := range []*store.Workspace{p.ws, other} {
		for _, k := range []string{workflow.PR, workflow.Issue} {
			s, _ := p.st.Workflow(w.ID, k)
			versions[w.Name+k] = s.Version
		}
	}

	// Step one changes nothing and says what step two will do.
	code, page := p.be.get(presetURL(erins, "/remove"))
	if code != 200 || !strings.Contains(page, "3 blocks in 2 workspaces follow it. Each will switch to Custom and keep this preset's text as its own") {
		t.Fatalf("the removal page: %d\n%s", code, page)
	}
	if _, err := p.st.PresetByKey(erins.Key); err != nil {
		t.Fatal("the first step removed the preset")
	}
	// Step two.
	if code, loc, _ := p.post(p.be, presetURL(erins, "/remove"), nil); code != http.StatusSeeOther || loc != "/presets?removed=1" {
		t.Fatalf("remove: %d %q", code, loc)
	}
	if _, err := p.st.PresetByKey(erins.Key); err == nil {
		t.Fatal("the preset is still there")
	}
	block := func(ws *store.Workspace, kind, id string) workflow.AgentSettings {
		t.Helper()
		w, _ := p.st.Workflow(ws.ID, kind)
		var g workflow.Graph
		json.Unmarshal([]byte(w.Graph), &g)
		for _, n := range g.Nodes {
			if n.ID == id {
				a, err := p.ui.agentSettings(n, kind)
				if err != nil {
					t.Fatal(err)
				}
				return a
			}
		}
		t.Fatalf("no block %s", id)
		return workflow.AgentSettings{}
	}
	a := block(p.ws, workflow.PR, "a")
	if a.Persona != "custom" || a.OnPreset() || a.Instruction != "PR text {pr:title}" || a.Goal != "PR done" || a.SummaryFormat != "**PR:** <x>" || a.Closing != "report" ||
		a.EffectiveInstruction(workflow.PR) != "PR text {pr:title}" || a.Model.Model != "m" || a.MaxTurns != 5 {
		t.Errorf("a pull request block: %+v", a)
	}
	if c := block(other, workflow.PR, "c"); c.Persona != "custom" || c.Instruction != "PR text {pr:title}" {
		t.Errorf("another workspace's block: %+v", c)
	}
	d := block(other, workflow.Issue, "d")
	if d.Persona != "custom" || d.Instruction != "Issue text {issue:title}" || d.Goal != "Issue done" || d.SummaryFormat != "**Issue:** <y>" || d.Closing != "report" {
		t.Errorf("an issue block takes the issue's texts: %+v", d)
	}
	if b := block(p.ws, workflow.PR, "b"); b.Persona != "code_quality" || !b.OnPreset() {
		t.Errorf("a block on another preset changed: %+v", b)
	}
	for _, c := range []struct {
		ws   *store.Workspace
		kind string
		bump bool
	}{{p.ws, workflow.PR, true}, {other, workflow.PR, true}, {other, workflow.Issue, true}, {p.ws, workflow.Issue, false}} {
		s, _ := p.st.Workflow(c.ws.ID, c.kind)
		if got := s.Version - versions[c.ws.Name+c.kind]; (got == 1) != c.bump || got > 1 {
			t.Errorf("%s %s: version moved by %d", c.ws.Name, c.kind, got)
		}
	}
	// The blocks still run: what a block follows is checked as before.
	if err := a.Check(workflow.PR); err != nil {
		t.Errorf("the switched block does not pass its check: %v", err)
	}
	// A preset nobody follows says so, and goes at once.
	lone := p.create(p.be, "Lone", nil)
	if _, page := p.be.get(presetURL(lone, "/remove")); !strings.Contains(page, "No block follows it.") {
		t.Errorf("a lone preset:\n%s", page)
	}
}

// A Custom block keeps its own summary format; a block on a preset stores none.
func TestCustomKeepsItsOwnSummaryFormat(t *testing.T) {
	p := newPresetEnv(t)
	save := func(persona, format string) workflow.AgentSettings {
		t.Helper()
		w, _ := p.st.Workflow(p.ws.ID, workflow.PR)
		code, _, body := p.be.post("/w/o/r/workflows/pr/nodes/ag", url.Values{"csrf": {p.be.csrf()}, "version": {strconv.Itoa(w.Version)},
			"name": {"Facts"}, "model": {strconv.FormatInt(p.prov.ID, 10) + ":m"}, "persona": {persona}, "instruction": {"my own"}, "goal": {"my goal"},
			"summary_format": {format}, "closing": {"report"}, "max_turns": {"40"}, "max_minutes": {"20"}, "rerun": {"resume"}})
		if code != 200 {
			t.Fatalf("save %s: %d\n%s", persona, code, body)
		}
		w, _ = p.st.Workflow(p.ws.ID, workflow.PR)
		var g workflow.Graph
		json.Unmarshal([]byte(w.Graph), &g)
		a, _ := p.ui.agentSettings(g.Nodes[0], workflow.PR)
		return a
	}
	if a := save("custom", "**Mine:** <x>"); a.SummaryFormat != "**Mine:** <x>" || a.EffectiveSummaryFormat(workflow.PR) != "**Mine:** <x>" {
		t.Errorf("custom: %+v", a)
	}
	if _, page := p.be.get("/w/o/r/workflows/pr/nodes/ag"); !strings.Contains(page, `<textarea name="summary_format" rows="6" class="code" spellcheck="false">**Mine:** &lt;x&gt;</textarea>`) {
		t.Errorf("the form shows it:\n%s", page)
	}
	if a := save("code_quality", "**Ignored:** <x>"); a.SummaryFormat != "" || a.EffectiveSummaryFormat(workflow.PR) != "" {
		t.Errorf("a preset's block keeps its own format: %+v", a)
	}
	if code, _, body := p.be.post("/w/o/r/workflows/pr/nodes/ag", url.Values{"csrf": {p.be.csrf()}, "version": {"3"}, "name": {"Facts"},
		"model": {strconv.FormatInt(p.prov.ID, 10) + ":m"}, "persona": {"custom"}, "instruction": {"x"}, "goal": {"y"}, "summary_format": {"**A:** {nosuch}"},
		"max_turns": {"40"}, "max_minutes": {"20"}, "rerun": {"resume"}}); code != http.StatusBadRequest || !strings.Contains(body, "nosuch") {
		t.Errorf("an unknown tag in a Custom summary format: %d\n%s", code, body)
	}
}
