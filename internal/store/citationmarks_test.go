package store

import "testing"

// The citation-marks setting is stored as its inverse, and a preset's starting additional instructions
// as they are: a preset made without them (every row from before the columns) has the marks on and no
// starting text; both survive create, update and both seed steps.
func TestPresetNoCitationMarksRoundTrips(t *testing.T) {
	st := open(t)
	derek := mustUser(t, st, "derek", true)
	if _, err := st.db.Exec(`INSERT INTO presets (key, name, owner_id, pr_instruction, pr_goal, issue_instruction, issue_goal, created_at, updated_at) VALUES ('old', 'Old', NULL, 'p', 'g', 'i', 'g', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if old, err := st.PresetByKey("old"); err != nil || old.NoCitationMarks || old.Additional != "" {
		t.Fatalf("an old row: %+v %v", old, err)
	}
	p := newPreset("Editor", 0)
	p.NoCitationMarks, p.Additional = true, "{input.summary}"
	made, err := st.CreatePreset(p, derek)
	if err != nil || !made.NoCitationMarks || made.Additional != "{input.summary}" {
		t.Fatalf("created: %+v %v", made, err)
	}
	p.NoCitationMarks, p.Additional = false, "{input.report}"
	if err := st.UpdatePreset(made.ID, p, derek); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Preset(made.ID); got.NoCitationMarks || got.Additional != "{input.report}" {
		t.Fatalf("update did not turn the flag off and change the text: %+v", got)
	}
	p.NoCitationMarks = true
	if err := st.UpdatePreset(made.ID, p, derek); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Preset(made.ID); !got.NoCitationMarks {
		t.Fatal("update did not turn the flag on")
	}
	one := Preset{Key: "ed", Name: "Ed", PRInstruction: "p", PRGoal: "g", IssueInstruction: "i", IssueGoal: "g", NoCitationMarks: true, NoTools: true, Additional: "x"}
	two := one
	two.Key, two.Name = "ed2", "Ed 2"
	if err := st.RunPresetSteps([]PresetSeedStep{{Marker: "presets_v6_editor", Adds: []Preset{two}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SeedPresets([]Preset{one}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ed", "ed2"} {
		if got, err := st.PresetByKey(k); err != nil || !got.NoCitationMarks || !got.NoTools || got.Additional != "x" {
			t.Fatalf("seeded %s: %+v %v", k, got, err)
		}
	}
}
