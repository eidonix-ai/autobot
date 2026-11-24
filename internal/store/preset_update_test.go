package store

import "testing"

// A stored shared preset's text is updated once, and only if nobody edited it.
func TestUpdatePresetTextOnce(t *testing.T) {
	st := open(t)
	for key, text := range map[string]string{"untouched": "old", "edited": "an admin's text"} {
		if _, err := st.db.Exec(`INSERT INTO presets (key, name, pr_instruction, pr_goal, issue_instruction, issue_goal, created_at, updated_at) VALUES (?, ?, ?, '', '', '', 0, 0)`, key, key, text); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"untouched", "edited"} {
		if err := st.UpdatePresetTextOnce("m_"+key, key, "old", "new"); err != nil {
			t.Fatal(err)
		}
	}
	if p, _ := st.PresetByKey("untouched"); p.PRInstruction != "new" {
		t.Fatalf("untouched: %q", p.PRInstruction)
	}
	if p, _ := st.PresetByKey("edited"); p.PRInstruction != "an admin's text" {
		t.Fatalf("edited: %q", p.PRInstruction)
	}
	// Once: a later run with the same marker changes nothing.
	st.db.Exec(`UPDATE presets SET pr_instruction = 'old' WHERE key = 'untouched'`)
	st.UpdatePresetTextOnce("m_untouched", "untouched", "old", "newer")
	if p, _ := st.PresetByKey("untouched"); p.PRInstruction != "old" {
		t.Fatalf("ran twice: %q", p.PRInstruction)
	}
}
