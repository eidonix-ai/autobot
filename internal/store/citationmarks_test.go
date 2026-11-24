package store

import "testing"

// The citation-marks setting is stored as its inverse: a preset made without it (every row from before
// the column) has the marks on; the flag survives create, update and both seed steps.
func TestPresetNoCitationMarksRoundTrips(t *testing.T) {
	st := open(t)
	derek := mustUser(t, st, "derek", true)
	if _, err := st.db.Exec(`INSERT INTO presets (key, name, owner_id, pr_instruction, pr_goal, issue_instruction, issue_goal, created_at, updated_at) VALUES ('old', 'Old', NULL, 'p', 'g', 'i', 'g', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if old, err := st.PresetByKey("old"); err != nil || old.NoCitationMarks {
		t.Fatalf("an old row: %+v %v", old, err)
	}
	p := newPreset("Editor", 0)
	p.NoCitationMarks = true
	made, err := st.CreatePreset(p, derek)
	if err != nil || !made.NoCitationMarks {
		t.Fatalf("created: %+v %v", made, err)
	}
	p.NoCitationMarks = false
	if err := st.UpdatePreset(made.ID, p, derek); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Preset(made.ID); got.NoCitationMarks {
		t.Fatal("update did not turn the flag off")
	}
	p.NoCitationMarks = true
	if err := st.UpdatePreset(made.ID, p, derek); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Preset(made.ID); !got.NoCitationMarks {
		t.Fatal("update did not turn the flag on")
	}
	one := Preset{Key: "ed", Name: "Ed", PRInstruction: "p", PRGoal: "g", IssueInstruction: "i", IssueGoal: "g", NoCitationMarks: true}
	two := one
	two.Key, two.Name = "ed2", "Ed 2"
	if err := st.SeedPresetsV6([]Preset{two}); err != nil {
		t.Fatal(err)
	}
	if err := st.SeedPresets([]Preset{one}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ed", "ed2"} {
		if got, err := st.PresetByKey(k); err != nil || !got.NoCitationMarks {
			t.Fatalf("seeded %s: %+v %v", k, got, err)
		}
	}
}
