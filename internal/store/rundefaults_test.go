package store

import "testing"

// A preset's starting run settings round-trip through create, update, seed and copy; a row made before
// the column reads as none.
func TestPresetRunDefaults(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "derek", true)
	if _, err := st.db.Exec(`INSERT INTO presets (key, name, pr_instruction, pr_goal, issue_instruction, issue_goal, created_at, updated_at) VALUES ('old', 'old', 'i', 'g', 'i', 'g', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if p, err := st.PresetByKey("old"); err != nil || p.RunDefaults != "" {
		t.Fatalf("an old row: %+v %v", p, err)
	}
	const rd = `{"max_turns":60,"sub_agents":6,"sub_agents_per_run":0}`
	n := newPreset("Mine", admin.ID)
	n.RunDefaults = rd
	made, err := st.CreatePreset(n, admin)
	if err != nil || made.RunDefaults != rd {
		t.Fatalf("created: %+v %v", made, err)
	}
	const changed = `{"max_minutes":10}`
	n.RunDefaults = changed
	if err := st.UpdatePreset(made.ID, n, admin); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.Preset(made.ID); p.RunDefaults != changed {
		t.Fatalf("updated: %q", p.RunDefaults)
	}
	s := seeds()
	s[0].RunDefaults = rd
	if err := st.SeedPresets(s); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.PresetByKey("gather_information"); p.RunDefaults != rd {
		t.Fatalf("seeded: %q", p.RunDefaults)
	}
	if p, _ := st.PresetByKey("code_quality"); p.RunDefaults != "" {
		t.Fatalf("a seed without any: %q", p.RunDefaults)
	}
}

// The one-time step sets starting settings on shared presets that have none, once; a preset that has
// some, a user's own and a later start are left alone.
func TestSetRunDefaultsOnce(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "derek", true)
	s := seeds()
	s[1].RunDefaults = `{"max_turns":99}`
	if err := st.SeedPresets(s); err != nil {
		t.Fatal(err)
	}
	mine, _ := st.CreatePreset(newPreset("Mine", admin.ID), admin)
	keys := []string{"gather_information", "code_quality", mine.Key}
	const rd = `{"max_turns":60}`
	if err := st.SetRunDefaultsOnce("m", keys, rd); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"gather_information": rd, "code_quality": `{"max_turns":99}`, mine.Key: ""} {
		if p, _ := st.PresetByKey(key); p.RunDefaults != want {
			t.Errorf("%s: %q, want %q", key, p.RunDefaults, want)
		}
	}
	// An admin clears one; the same marker does not put it back.
	g, _ := st.PresetByKey("gather_information")
	g.RunDefaults = ""
	if err := st.UpdatePreset(g.ID, *g, admin); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunDefaultsOnce("m", keys, rd); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.PresetByKey("gather_information"); p.RunDefaults != "" {
		t.Errorf("ran twice: %q", p.RunDefaults)
	}
}
