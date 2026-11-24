package store

import (
	"errors"
	"strings"
	"testing"
)

func seeds() []Preset {
	return []Preset{
		{Key: "gather_information", Name: "Gather Information", Description: "Everything relevant", PRInstruction: "pr text", PRGoal: "pr goal",
			IssueInstruction: "issue text", IssueGoal: "issue goal", Closing: "report"},
		{Key: "code_quality", Name: "Code Quality", PRInstruction: "q", PRGoal: "g", IssueInstruction: "qi", IssueGoal: "gi"},
	}
}

// The built-in presets go in once, with their keys; what an admin removes afterwards stays removed,
// and a second start does not put anything back or duplicate it.
func TestSeedPresetsOnce(t *testing.T) {
	st := open(t)
	if err := st.SeedPresets(seeds()); err != nil {
		t.Fatal(err)
	}
	all, _ := st.Presets()
	if len(all) != 2 || !all[0].Shared() || all[0].Key != "code_quality" && all[0].Key != "gather_information" {
		t.Fatalf("seeded: %+v", all)
	}
	g, err := st.PresetByKey("gather_information")
	if err != nil || g.Name != "Gather Information" || g.Closing != "report" || g.PRInstruction != "pr text" || g.IssueGoal != "issue goal" || !g.Shared() || g.Owner != "" {
		t.Fatalf("by key: %+v %v", g, err)
	}
	if err := st.SeedPresets(seeds()); err != nil {
		t.Fatalf("seeding again: %v", err)
	}
	if all, _ := st.Presets(); len(all) != 2 {
		t.Fatalf("seeding again changed the presets: %d", len(all))
	}
	if err := st.RemovePreset(g.ID, func(StoredGraph) (string, bool, error) { return "", false, nil }); err != nil {
		t.Fatal(err)
	}
	st.SeedPresets(seeds())
	if _, err := st.PresetByKey("gather_information"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a removed built-in preset came back: %v", err)
	}
}

func newPreset(name string, owner int64) Preset {
	return Preset{Name: name, Description: "d", OwnerID: owner, PRInstruction: "pr {pr:title}", PRGoal: "pr done", IssueInstruction: "issue {issue:title}", IssueGoal: "issue done"}
}

// A new preset gets a key from its name that nothing else has and that is never "custom"; the key,
// owner and sharing stay when it is edited; the checks refuse what cannot work.
func TestPresetCreateUpdateAndKeys(t *testing.T) {
	st := open(t)
	derek := mustUser(t, st, "derek", true)
	erin := mustUser(t, st, "erin", false)
	a, err := st.CreatePreset(newPreset("My Review!", erin.ID), erin)
	if err != nil || a.Key != "my_review" || a.OwnerID != erin.ID || a.Owner != "erin" || a.Shared() {
		t.Fatalf("created: %+v %v", a, err)
	}
	b, _ := st.CreatePreset(newPreset("My review", derek.ID), derek)
	c, _ := st.CreatePreset(newPreset("my  review", 0), derek)
	if b.Key != "my_review_2" || c.Key != "my_review_3" || !c.Shared() {
		t.Fatalf("keys: %q %q (shared %v)", b.Key, c.Key, c.Shared())
	}
	if k, _ := st.CreatePreset(newPreset("Custom", 0), derek); k.Key != "custom_2" {
		t.Fatalf("the key custom is taken by the block's own choice: %q", k.Key)
	}
	if k, _ := st.CreatePreset(newPreset("日本語", 0), derek); k.Key != "preset" {
		t.Fatalf("a name with no letters: %q", k.Key)
	}
	edit := newPreset("Renamed", derek.ID) // trying to change the owner through an edit
	edit.Closing = "report"
	if err := st.UpdatePreset(a.ID, edit, erin); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Preset(a.ID)
	if got.Key != "my_review" || got.OwnerID != erin.ID || got.Name != "Renamed" || got.Closing != "report" {
		t.Fatalf("updated: %+v", got)
	}
	if err := st.UpdatePreset(9999, edit, erin); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing preset: %v", err)
	}
	for what, mod := range map[string]func(*Preset){
		"no name":              func(p *Preset) { p.Name = "  " },
		"a long name":          func(p *Preset) { p.Name = strings.Repeat("n", 61) },
		"a two-line name":      func(p *Preset) { p.Name = "a\nb" },
		"a two-line summary":   func(p *Preset) { p.Description = "a\nb" },
		"a long summary":       func(p *Preset) { p.Description = strings.Repeat("d", 201) },
		"no pr instruction":    func(p *Preset) { p.PRInstruction = " " },
		"no issue instruction": func(p *Preset) { p.IssueInstruction = "" },
		"no pr goal":           func(p *Preset) { p.PRGoal = "" },
		"no issue goal":        func(p *Preset) { p.IssueGoal = "" },
		"too long":             func(p *Preset) { p.PRGoal = strings.Repeat("g", MaxPresetText+1) },
		"a made-up closing":    func(p *Preset) { p.Closing = "poem" },
	} {
		p := newPreset("ok", 0)
		mod(&p)
		var ie *InputError
		if _, err := st.CreatePreset(p, derek); !errors.As(err, &ie) {
			t.Errorf("%s was accepted (%v)", what, err)
		}
		if err := st.UpdatePreset(a.ID, p, erin); !errors.As(err, &ie) {
			t.Errorf("an edit with %s was accepted (%v)", what, err)
		}
	}
}

// Removing a preset hands every saved workflow, and every run still going, to the rewrite, saves the
// ones it changed with a new version, and does all of it or nothing.
func TestRemovePresetRewritesWorkflowsAndRunningRuns(t *testing.T) {
	st, a, b := twoWorkspaces(t)
	derek := mustUser(t, st, "derek", true)
	p, _ := st.CreatePreset(newPreset("Mine", derek.ID), derek)
	for ws, graph := range map[int64]string{a.ID: `{"uses":"` + p.Key + `"}`, b.ID: `{"uses":"other"}`} {
		if _, err := st.SaveWorkflow(ws, "pr", 0, graph, derek); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []struct{ status, graph string }{{"running", `{"uses":"` + p.Key + `"}`}, {"done", `{"uses":"` + p.Key + `"}`}} {
		if _, err := st.db.Exec(`INSERT INTO workflow_runs (workspace_id, kind, event_node, item, graph, status, started_at) VALUES (?, 'pr', 'ev', 1, ?, ?, 1)`, a.ID, r.graph, r.status); err != nil {
			t.Fatal(err)
		}
	}
	rewrite := func(g StoredGraph) (string, bool, error) {
		if strings.Contains(g.Graph, `"`+p.Key+`"`) {
			return strings.ReplaceAll(g.Graph, p.Key, "custom"), true, nil
		}
		return g.Graph, false, nil
	}
	// A failure in the middle changes nothing.
	boom := errors.New("boom")
	if err := st.RemovePreset(p.ID, func(g StoredGraph) (string, bool, error) {
		if g.Workspace == b.ID {
			return "", false, boom
		}
		return rewrite(g)
	}); !errors.Is(err, boom) {
		t.Fatalf("the error was lost: %v", err)
	}
	if _, err := st.PresetByKey(p.Key); err != nil {
		t.Fatal("the preset went although the rewrite failed")
	}
	if w, _ := st.Workflow(a.ID, "pr"); !strings.Contains(w.Graph, p.Key) || w.Version != 1 {
		t.Fatalf("a failed removal changed a workflow: %+v", w)
	}
	if err := st.RemovePreset(p.ID, rewrite); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PresetByKey(p.Key); !errors.Is(err, ErrNotFound) {
		t.Fatal("the preset is still there")
	}
	wa, _ := st.Workflow(a.ID, "pr")
	wb, _ := st.Workflow(b.ID, "pr")
	if wa.Graph != `{"uses":"custom"}` || wa.Version != 2 || wb.Graph != `{"uses":"other"}` || wb.Version != 1 {
		t.Fatalf("workflows: %+v %+v", wa, wb)
	}
	var running, done string
	st.db.QueryRow(`SELECT graph FROM workflow_runs WHERE status = 'running'`).Scan(&running)
	st.db.QueryRow(`SELECT graph FROM workflow_runs WHERE status = 'done'`).Scan(&done)
	if running != `{"uses":"custom"}` || done != `{"uses":"`+p.Key+`"}` {
		t.Fatalf("runs: running %s, done %s", running, done)
	}
	if err := st.RemovePreset(p.ID, rewrite); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing twice: %v", err)
	}
	if gs, _ := st.Graphs(); len(gs) != 2 {
		t.Fatalf("graphs: %+v", gs)
	}
}
