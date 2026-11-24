package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// Presets (Derek, 2026-11-23): the texts an Agent block can follow. They live in the database; the
// built-in ones are put in once (workflow.SeedPresets). A preset is shared (admins make and change
// it, everyone uses it) or a user's own (its owner changes it and uses it in any workspace they are
// in). Anyone signed in can look at any preset and copy it; an admin cannot change another user's.

// seedRows are the built-in presets as stored rows.
func seedRows() []store.Preset { return presetRows(workflow.SeedPresets) }

// presetRows converts built-in Persona definitions to store.Preset rows.
func presetRows(list []workflow.Persona) []store.Preset {
	var rows []store.Preset
	for _, p := range list {
		rows = append(rows, store.Preset{
			Key: p.Key, Name: p.Name, Description: p.Description,
			PRInstruction: p.Instruction, PRGoal: p.Goal, PRSummary: p.Summary,
			IssueInstruction: p.IssueInstruction, IssueGoal: p.Issue, IssueSummary: p.IssueSummary,
			Closing: p.Closing, NoCitationMarks: p.NoCitationMarks, NoTools: p.NoTools,
			Additional: p.Additional, RunDefaults: p.RunDefaults.JSON(),
		})
	}
	return rows
}

// presetRenames are the seeded presets that took a code review name.
func presetRenames() []store.PresetRename {
	var out []store.PresetRename
	for _, r := range workflow.PresetRenames {
		out = append(out, store.PresetRename{Key: r.Key, From: r.From, To: r.To})
	}
	return out
}

// reviewRunDefaultKeys lists the shared code-review preset keys (other than Lens Team) that receive
// standard starting run settings on older databases where they had none.
func reviewRunDefaultKeys() []string {
	var keys []string
	for _, p := range workflow.SeedPresets {
		if (strings.HasPrefix(p.Key, "review_") && p.Key != "review_lens_team") ||
			p.Key == "code_quality" || p.Key == "security" || p.Key == "test_engineer" {
			keys = append(keys, p.Key)
		}
	}
	// Include the retired review_simplicity key for older databases that still have it.
	return append(keys, "review_simplicity")
}

// presetSeedSteps are the ordered one-time migrations for databases seeded before newer built-in
// presets or settings existed. Each step is tracked by Marker in settings so it runs at most once.
func presetSeedSteps() []store.PresetSeedStep {
	return []store.PresetSeedStep{
		{Marker: "presets_v2", Renames: presetRenames(), Adds: presetRows(workflow.ReviewPresets)},
		{Marker: "presets_v3", Adds: presetRows([]workflow.Persona{workflow.PreTestPreset})},
		{Marker: "presets_v4", Adds: presetRows([]workflow.Persona{workflow.ConsequencesPreset})},
		{Marker: "presets_v6_editor", Adds: presetRows([]workflow.Persona{workflow.EditorPreset})},
		{Marker: "presets_v7_subtraction", Adds: presetRows([]workflow.Persona{workflow.SubtractionPreset})},
		{Marker: "presets_v8_lens_team", Adds: presetRows([]workflow.Persona{workflow.LensTeamPreset})},
		{Marker: "presets_v8_run_defaults", SetDefaults: reviewRunDefaultKeys(), RunDefaults: workflow.ReviewRunDefaults().JSON()},
		{Marker: "presets_v9_cleanup", SetNoTools: []string{workflow.EditorPreset.Key}, Upgrades: v9PresetUpgrades()},
	}
}

// v9PresetUpgrades updates untouched shared presets on existing databases to the cleaned-up texts,
// goals, descriptions and starting run defaults (matching each field by the SHA-256 of its previous
// seeded value so any admin edits are preserved untouched).
func v9PresetUpgrades() []store.PresetUpgrade {
	byKey := map[string]store.Preset{}
	for _, p := range seedRows() {
		byKey[p.Key] = p
	}
	const (
		emptyHash       = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		oldReviewRDHash = "af67e6d69965c338032ede3ccb180f2e8b0770df9571e19b6cb04f8e656e738f" // {"max_turns":60,"max_minutes":30,"time_warn_percent":20}
		oldReviewGoal   = "275609cbd8b98776fedfda3cdbbc80a075e19bec7690fc6e2da003eb22a6c17e"
	)
	rdMatches := []string{emptyHash, oldReviewRDHash}
	u := func(key string, match map[string][]string) store.PresetUpgrade {
		return store.PresetUpgrade{Key: key, Match: match, To: byKey[key]}
	}
	return []store.PresetUpgrade{
		u("gather_information", map[string][]string{
			"pr_instruction":    {"0bbbc7d0d62bbe61cf3ebd74d4030e8b0751a9bc5a8c2f8a51e1f6b4bf204ce1"},
			"issue_instruction": {"5a64a158039dfe5d7e5715d3af941e7db34a61589a0d53000a52fd3868f510fe"},
			"run_defaults":      {emptyHash},
		}),
		u("value_analyst", map[string][]string{
			"pr_instruction":    {"e9ecdf19a9d980bd1bbec49a04b484bb8bf4280a73425378f626d226b057a163"},
			"issue_instruction": {"2365a0fc7d8d2099b114c97871f2f2c81a8e1215a3c026547b1a11553366e479"},
			"run_defaults":      {emptyHash},
		}),
		u("editor_prose", map[string][]string{
			"description":       {"0b4a18315e9ccad8fb4c35a2c02459c1a61abb3d3c7bfdc890291c96f4544671"},
			"pr_instruction":    {"cd7425fe1eb3554e31056fa436acfb8a153f773e5762625d02243a6e600d1f8f"},
			"pr_goal":           {"62bce9ba06a51a3d684a447ad2197e2fc41ba1fa25749b6fa2db6ec683c41395"},
			"issue_instruction": {"cd7425fe1eb3554e31056fa436acfb8a153f773e5762625d02243a6e600d1f8f"},
			"issue_goal":        {"62bce9ba06a51a3d684a447ad2197e2fc41ba1fa25749b6fa2db6ec683c41395"},
			"run_defaults":      {emptyHash},
		}),
		u("code_quality", map[string][]string{
			"description":    {"ccef239cbe7983aa84a866c68f0749872a7114375b37852fe814b0ebc22603cb"},
			"pr_instruction": {"d84d57c4031418994fef5984e40ab654c9360d21ff25aafd1793c9c25f5cc004"},
			"run_defaults":   rdMatches,
		}),
		u("security", map[string][]string{"run_defaults": rdMatches}),
		u("test_engineer", map[string][]string{"run_defaults": rdMatches}),
		u("review_all", map[string][]string{
			"description":    {"17055712b563883dc7618f35ea36e644b0b6edff5d46b0ab0a869ac14258eda9"},
			"pr_instruction": {"6f6490a0b1ec7444a77f63f3a651388bda33dd108b93ea7198569063ddd49d58"},
			"pr_goal":        {oldReviewGoal},
			"run_defaults":   rdMatches,
		}),
		u("review_correctness", map[string][]string{
			"description":    {"cfd4fc6bf585881079881f2f981d1aec7ef4e3de30eb4b47950e018f828d4645"},
			"pr_instruction": {"67b140e4c3b784fe7e5e3fbaec0a0d439d4dfd361b4b0e11be858fec246946a0"},
			"pr_goal":        {oldReviewGoal},
			"run_defaults":   rdMatches,
		}),
		u("review_architecture", map[string][]string{
			"description":    {"0fefc07bf30e631fe551a9e58debe39f48145c4175a631e7a3a6ad8938daec3c"},
			"pr_instruction": {"0aed034e7cdec37f92bf5c5549053258d606be730577fbe61f0f05d2711c6827"},
			"pr_goal":        {oldReviewGoal},
			"run_defaults":   rdMatches,
		}),
		u("review_conventions", map[string][]string{
			"description":    {"9c5d15cca0ea1b6aab7569a087a2d4cce70ebf01181efed13d0fa7d3df08d1c6"},
			"pr_instruction": {"4a1f238eddd734122cfe174261ff83992e74b1d19e7deb7b0724526383444d1f"},
			"pr_goal":        {oldReviewGoal},
			"run_defaults":   rdMatches,
		}),
		u("review_prose", map[string][]string{
			"description":    {"50a31bf39f012f46d4bd207364be4fc55ce59d44733c69524040963fded3babd"},
			"pr_instruction": {"97ae95753812135274fbaf4d4bfb89ce2c35cd6fab88d11b7e99827fd6bf0f2a"},
			"pr_goal":        {oldReviewGoal},
			"run_defaults":   rdMatches,
		}),
		u("review_consequences", map[string][]string{
			"description":    {"c59804d09c3eef72f71365c09b0128de295802543d026ebd02edd83a3c094e12"},
			"pr_instruction": {"34a98caa5676a8f79f9ed11b9ee335f04593bf79f111cb7b19c079474cfbcc96"},
			"pr_goal":        {oldReviewGoal},
			"run_defaults":   rdMatches,
		}),
		u("review_subtraction", map[string][]string{
			"pr_instruction": {"3b69c47cc3afdddd5bceb63747f3723164fa583db92b396c21a2377b8410d27b"},
			"pr_goal":        {"3d48d598e1013726f618b511096f5889882fa2a6d1859f94f8f5ed47fcdef204"},
			"run_defaults":   rdMatches,
		}),
		u("review_pretest", map[string][]string{
			"description":    {"e3f1473a4ed29fce0d29fb8f735ca5f821ec7f84c84805897caf4d3b6b1e5c10"},
			"pr_instruction": {"529712db3cec6782f45781aa2672f949a5a407532f6a74f806dd6a5b2e1b925a", "95cf1fb15e99507eb570aa506d4f972eb78ae79bc0326d2585c69d2025a05f10"},
			"pr_goal":        {"b0da753a6477d27763b37e027af1604de6d121e9a43f363d70f449a0873fa68e"},
			"pr_summary":     {"39ddd142f3949a6919b29afbb75aac542283e2873caf36c926913d7c28cca7e5"},
			"run_defaults":   rdMatches,
		}),
		u("review_lens_team", map[string][]string{
			"description":    {"6ecf24516519382104d8613b6c73ef5a23ec1671f2827932f63663c6a4b5fde4"},
			"pr_instruction": {"7e1f6aebdbf555f1268708cbef46fd27ce4c72b8ca926e0982604c54014fa254"},
			"pr_goal":        {"cb919b44645fa8bb156043a43f732cab9d169a4af6cdcfe2333b7964902eeb55"},
		}),
	}
}

// asPersona is a stored preset as a block uses it.
func asPersona(p *store.Preset) *workflow.Persona {
	return &workflow.Persona{
		Key: p.Key, Name: p.Name, Description: p.Description,
		Instruction: p.PRInstruction, Goal: p.PRGoal, Summary: p.PRSummary,
		IssueInstruction: p.IssueInstruction, Issue: p.IssueGoal, IssueSummary: p.IssueSummary,
		Closing: p.Closing, NoCitationMarks: p.NoCitationMarks, NoTools: p.NoTools,
		Additional: p.Additional, RunDefaults: workflow.ParseRunDefaults(p.RunDefaults),
	}
}

// resolved gives a block's settings the preset their key names, as the database has it now. A key
// that names nothing (a preset removed, or never there) is left as it is: Check says so.
func (s *Server) resolved(a workflow.AgentSettings) (workflow.AgentSettings, error) {
	if a.Persona == "" || a.Persona == "custom" {
		return a, nil
	}
	p, err := s.st.PresetByKey(a.Persona)
	if errors.Is(err, store.ErrNotFound) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	return a.WithPreset(asPersona(p)), nil
}

// agentSettings reads an Agent node's settings with its preset in place.
func (s *Server) agentSettings(n workflow.Node, kind string) (workflow.AgentSettings, error) {
	a, err := workflow.AgentSettingsOf(n, kind)
	if err != nil {
		return a, err
	}
	return s.resolved(a)
}

// canChangePreset says whether a person may edit or remove a preset: an admin a shared one, an owner
// their own. An admin does not change another user's.
func canChangePreset(u *store.User, p *store.Preset) bool {
	if p.Shared() {
		return u.IsAdmin
	}
	return p.OwnerID == u.ID
}

// presetUse is how many blocks follow a preset, and in how many workspaces.
type presetUse struct{ Blocks, Workspaces int }

// agentNodes decodes a stored graph's Agent blocks.
func agentNodes(graph string) (workflow.Graph, error) {
	var g workflow.Graph
	err := json.Unmarshal([]byte(graph), &g)
	return g, err
}

// presetUses counts the blocks of every saved workflow that follow each preset.
func (s *Server) presetUses() (map[string]presetUse, error) {
	graphs, err := s.st.Graphs()
	if err != nil {
		return nil, err
	}
	uses := map[string]presetUse{}
	inWS := map[string]map[int64]bool{}
	for _, sg := range graphs {
		g, err := agentNodes(sg.Graph)
		if err != nil {
			continue
		}
		for _, n := range g.Nodes {
			a, err := workflow.AgentSettingsOf(n, sg.Kind)
			if n.Type != workflow.Agent || err != nil || a.Persona == "custom" || a.Persona == "" {
				continue
			}
			u := uses[a.Persona]
			u.Blocks++
			if inWS[a.Persona] == nil {
				inWS[a.Persona] = map[int64]bool{}
			}
			if !inWS[a.Persona][sg.Workspace] {
				inWS[a.Persona][sg.Workspace] = true
				u.Workspaces++
			}
			uses[a.Persona] = u
		}
	}
	return uses, nil
}

// switchToCustom is a graph with every block that follows the preset made Custom, holding the
// preset's text as its own; whether any was.
func switchToCustom(sg store.StoredGraph, p *store.Preset) (string, bool, error) {
	if !strings.Contains(sg.Graph, p.Key) {
		return sg.Graph, false, nil
	}
	g, err := agentNodes(sg.Graph)
	if err != nil {
		return sg.Graph, false, nil // not a graph we can read: leave it
	}
	changed := false
	for i, n := range g.Nodes {
		a, err := workflow.AgentSettingsOf(n, sg.Kind)
		if n.Type != workflow.Agent || err != nil || a.Persona != p.Key {
			continue
		}
		pers := asPersona(p)
		a.Persona, a.Instruction, a.Goal, a.SummaryFormat, a.Closing = "custom", pers.InstructionFor(sg.Kind), pers.GoalFor(sg.Kind), pers.SummaryFor(sg.Kind), p.Closing
		a.NoCitationMarks, a.NoTools = p.NoCitationMarks, p.NoTools
		raw, err := json.Marshal(a)
		if err != nil {
			return "", false, err
		}
		g.Nodes[i].Settings = raw
		changed = true
	}
	if !changed {
		return sg.Graph, false, nil
	}
	out, err := json.Marshal(g)
	return string(out), true, err
}

// checkPresetTexts refuses a preset whose texts use a tag a block could not fill: only the built-in
// tags are known to a preset, as the pull request's or the issue's own.
func checkPresetTexts(p *store.Preset) error {
	if err := workflow.ParseRunDefaults(p.RunDefaults).Check(); err != nil {
		return &store.InputError{Msg: "Starting settings: " + err.Error() + "."}
	}
	if err := workflow.CheckTemplates(workflow.PR, nil, p.PRInstruction, p.PRGoal, p.PRSummary, p.Additional); err != nil {
		return &store.InputError{Msg: "For pull requests: " + err.Error() + "."}
	}
	if err := workflow.CheckTemplates(workflow.Issue, nil, p.IssueInstruction, p.IssueGoal, p.IssueSummary, p.Additional); err != nil {
		return &store.InputError{Msg: "For issues: " + err.Error() + "."}
	}
	return nil
}

// checkPresetChoice says whether the person saving a block may give it the preset: Custom, a shared
// preset, their own, or the one the block already follows (which may be someone else's).
func (s *Server) checkPresetChoice(u *store.User, chosen workflow.AgentSettings, old workflow.AgentSettings) error {
	if chosen.Persona == "custom" || chosen.Persona == old.Persona {
		return nil
	}
	p, err := s.st.PresetByKey(chosen.Persona)
	if errors.Is(err, store.ErrNotFound) {
		return errors.New("that preset does not exist: choose another, or Custom")
	}
	if err != nil {
		return err
	}
	if !p.Shared() && p.OwnerID != u.ID {
		return fmt.Errorf("that preset belongs to %s: copy it to your own presets first, then choose your copy", p.Owner)
	}
	return nil
}

// ---- pages ----

// presetRow is a preset in a list.
type presetRow struct {
	*store.Preset
	Use presetUse
}

// presetTable is a list of presets as a page shows it; Owners adds a column for who owns each.
type presetTable struct {
	Rows   []presetRow
	Owners bool
}

func presetPath(p *store.Preset) string { return "/presets/" + strconv.FormatInt(p.ID, 10) }

func (s *Server) presetList(w http.ResponseWriter, r *http.Request) {
	all, err := s.st.Presets()
	if err != nil {
		s.fail(w, err)
		return
	}
	uses, err := s.presetUses()
	if err != nil {
		s.fail(w, err)
		return
	}
	var shared, mine, others []presetRow
	for _, p := range all {
		row := presetRow{p, uses[p.Key]}
		switch {
		case p.Shared():
			shared = append(shared, row)
		case p.OwnerID == me(r).ID:
			mine = append(mine, row)
		case me(r).IsAdmin: // an admin sees everyone's, read-only
			others = append(others, row)
		}
	}
	data := map[string]any{"Title": "Presets", "Shared": presetTable{Rows: shared}, "Mine": presetTable{Rows: mine}, "Others": presetTable{Rows: others, Owners: true}}
	switch {
	case r.URL.Query().Has("removed"):
		data["Notice"] = "Preset removed."
	}
	s.render(w, r, http.StatusOK, "presets.html", data)
}

// presetFromForm reads a preset's fields from the form.
func presetFromForm(r *http.Request) store.Preset {
	f := r.PostForm
	return store.Preset{
		Name: f.Get("name"), Description: f.Get("description"),
		PRInstruction: f.Get("pr_instruction"), PRGoal: f.Get("pr_goal"), PRSummary: strings.TrimSpace(f.Get("pr_summary")),
		IssueInstruction: f.Get("issue_instruction"), IssueGoal: f.Get("issue_goal"), IssueSummary: strings.TrimSpace(f.Get("issue_summary")),
		Closing: f.Get("closing"), NoCitationMarks: f.Get("citation_marks") == "", NoTools: f.Get("no_tools") != "",
		Additional: strings.TrimSpace(f.Get("additional")), RunDefaults: runDefaultsFromForm(f).JSON(),
	}
}

// runDefaultsFromForm reads the five starting settings; a blank is none, and a value that is not a
// number is -1, which Check refuses.
func runDefaultsFromForm(f url.Values) workflow.RunDefaults {
	num := func(name string) *int {
		v := strings.TrimSpace(f.Get(name))
		if v == "" {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			n = -1
		}
		return &n
	}
	return workflow.RunDefaults{MaxTurns: num("rd_max_turns"), MaxMinutes: num("rd_max_minutes"), TimeWarnPercent: num("rd_time_warn_percent"),
		SubAgents: num("rd_sub_agents"), SubAgentsPerRun: num("rd_sub_agents_per_run")}
}

// presetForm renders the form: empty for a new preset, or filled with p's fields (the stored ones, or
// what the person typed that was refused).
func (s *Server) presetForm(w http.ResponseWriter, r *http.Request, status int, stored *store.Preset, typed *store.Preset, shared bool, problem string) {
	data := map[string]any{"Error": problem}
	f := typed
	if stored != nil {
		data["P"], data["CanChange"] = stored, canChangePreset(me(r), stored)
		if f == nil {
			f = stored
		}
		uses, err := s.presetUses()
		if err != nil {
			s.fail(w, err)
			return
		}
		data["Use"] = uses[stored.Key]
		data["Title"] = stored.Name
	} else {
		data["CanChange"], data["Title"] = true, "New preset"
	}
	if f == nil {
		f = &store.Preset{}
	}
	data["F"], data["Shared"], data["RD"] = f, shared, workflow.ParseRunDefaults(f.RunDefaults)
	if r.URL.Query().Has("copied") {
		data["Notice"] = "This is your copy."
	}
	if r.URL.Query().Has("saved") {
		data["Notice"] = "Saved."
	}
	s.render(w, r, status, "preset.html", data)
}

func (s *Server) presetNew(w http.ResponseWriter, r *http.Request) {
	s.presetForm(w, r, http.StatusOK, nil, nil, me(r).IsAdmin && r.URL.Query().Get("shared") == "1", "")
}

func (s *Server) presetCreate(w http.ResponseWriter, r *http.Request) {
	p := presetFromForm(r)
	shared := r.PostForm.Get("shared") == "1"
	if shared && !me(r).IsAdmin {
		http.Error(w, "Only an admin can make a shared preset.", http.StatusForbidden)
		return
	}
	if !shared {
		p.OwnerID = me(r).ID
	}
	err := checkPresetTexts(&p)
	var made *store.Preset
	if err == nil {
		made, err = s.st.CreatePreset(p, me(r))
	}
	if msg, ok := inputError(err); ok {
		s.presetForm(w, r, http.StatusBadRequest, nil, &p, shared, msg)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, presetPath(made)+"?saved=1", http.StatusSeeOther)
}

// presetTarget resolves the preset in the URL. Anyone signed in may look at any preset.
func (s *Server) presetTarget(w http.ResponseWriter, r *http.Request) (*store.Preset, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	p, err := s.st.Preset(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return p, true
}

// presetToChange resolves the preset in the URL for a change, which only who may change it can make.
func (s *Server) presetToChange(w http.ResponseWriter, r *http.Request) (*store.Preset, bool) {
	p, ok := s.presetTarget(w, r)
	if ok && !canChangePreset(me(r), p) {
		msg := "Only its owner can change this preset."
		if p.Shared() {
			msg = "Only an admin can change a shared preset."
		}
		http.Error(w, msg, http.StatusForbidden)
		return nil, false
	}
	return p, ok
}

func (s *Server) presetPage(w http.ResponseWriter, r *http.Request) {
	if p, ok := s.presetTarget(w, r); ok {
		s.presetForm(w, r, http.StatusOK, p, nil, p.Shared(), "")
	}
}

func (s *Server) presetSave(w http.ResponseWriter, r *http.Request) {
	p, ok := s.presetToChange(w, r)
	if !ok {
		return
	}
	typed := presetFromForm(r)
	err := checkPresetTexts(&typed)
	if err == nil {
		err = s.st.UpdatePreset(p.ID, typed, me(r))
	}
	if msg, ok := inputError(err); ok {
		s.presetForm(w, r, http.StatusBadRequest, p, &typed, p.Shared(), msg)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, presetPath(p)+"?saved=1", http.StatusSeeOther)
}

// presetCopy makes the person their own copy of any preset.
func (s *Server) presetCopy(w http.ResponseWriter, r *http.Request) {
	p, ok := s.presetTarget(w, r)
	if !ok {
		return
	}
	c := *p
	c.OwnerID, c.Name = me(r).ID, "Copy of "+p.Name
	if r := []rune(c.Name); len(r) > store.MaxPresetName {
		c.Name = string(r[:store.MaxPresetName])
	}
	made, err := s.st.CreatePreset(c, me(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, presetPath(made)+"?copied=1", http.StatusSeeOther)
}

// presetRemovePage is the first step of removing: what it will do to the blocks that follow the preset.
func (s *Server) presetRemovePage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.presetToChange(w, r)
	if !ok {
		return
	}
	uses, err := s.presetUses()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "preset_remove.html", map[string]any{"Title": "Remove " + p.Name, "P": p, "Use": uses[p.Key]})
}

// presetRemove is the second step: every block that follows the preset becomes Custom with its text.
func (s *Server) presetRemove(w http.ResponseWriter, r *http.Request) {
	p, ok := s.presetToChange(w, r)
	if !ok {
		return
	}
	if err := s.st.RemovePreset(p.ID, func(sg store.StoredGraph) (string, bool, error) { return switchToCustom(sg, p) }); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/presets?removed=1", http.StatusSeeOther)
}

// plural is defined in steptest.go; a preset's use line reads "3 blocks in 2 workspaces".
func (u presetUse) String() string {
	if u.Blocks == 0 {
		return "not used"
	}
	return fmt.Sprintf("%d block%s in %d workspace%s", u.Blocks, plural(u.Blocks), u.Workspaces, plural(u.Workspaces))
}

// presetOption is one choice of a block's Preset dropdown, with what choosing it shows in the form.
type presetOption struct {
	Key, Name, Instruction, Goal, Summary, Closing, Additional, RunDefaults string
	Selected                                                                bool
}

// presetGroup is a heading of the dropdown with its choices.
type presetGroup struct {
	Label   string
	Options []presetOption
}

// presetChoices are the presets a person may give a block, as the dropdown groups them: the shared
// ones, their own, and the one the block follows now if it is someone else's, marked with its owner.
func (s *Server) presetChoices(u *store.User, kind string, current workflow.AgentSettings) ([]presetGroup, *store.Preset, error) {
	all, err := s.st.Presets()
	if err != nil {
		return nil, nil, err
	}
	shared, mine, inUse := presetGroup{Label: "Shared"}, presetGroup{Label: "Mine"}, presetGroup{Label: "In use"}
	var foreign *store.Preset
	for _, p := range all {
		pers := asPersona(p)
		o := presetOption{Key: p.Key, Name: p.Name, Instruction: pers.InstructionFor(kind), Goal: pers.GoalFor(kind), Summary: pers.SummaryFor(kind),
			Closing: p.Closing, Additional: p.Additional, RunDefaults: p.RunDefaults, Selected: current.Persona == p.Key}
		switch {
		case p.Shared():
			shared.Options = append(shared.Options, o)
		case p.OwnerID == u.ID:
			mine.Options = append(mine.Options, o)
		case p.Key == current.Persona:
			o.Name += " (by " + p.Owner + ")"
			inUse.Options = append(inUse.Options, o)
			foreign = p
		}
	}
	var groups []presetGroup
	for _, g := range []presetGroup{shared, mine, inUse} {
		if len(g.Options) > 0 {
			groups = append(groups, g)
		}
	}
	return groups, foreign, nil
}
