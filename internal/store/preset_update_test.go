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
		if err := st.RunPresetSteps([]PresetSeedStep{{Marker: "m_" + key, ReplacePR: &PresetTextUpdate{Key: key, From: "old", To: "new"}}}); err != nil {
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
	st.RunPresetSteps([]PresetSeedStep{{Marker: "m_untouched", ReplacePR: &PresetTextUpdate{Key: "untouched", From: "old", To: "newer"}}})
	if p, _ := st.PresetByKey("untouched"); p.PRInstruction != "old" {
		t.Fatalf("ran twice: %q", p.PRInstruction)
	}
}

// PresetUpgrade updates each field independently when its SHA-256 matches an older seeded value,
// leaving admin-edited fields and user-owned presets untouched, and running only once per marker.
func TestRunPresetStepsUpgrades(t *testing.T) {
	st := open(t)
	u, err := st.CreateUser("alice", "", "password-1234", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO presets (key, name, description, owner_id, pr_instruction, pr_goal, issue_instruction, issue_goal, run_defaults, created_at, updated_at)
		VALUES ('shared_p', 'Shared', 'old desc', NULL, 'admin edit', 'old goal', '', '', '', 0, 0),
		       ('owned_p', 'Owned', 'old desc', ?, 'old inst', 'old goal', '', '', '', 0, 0)`, u.ID); err != nil {
		t.Fatal(err)
	}
	step := PresetSeedStep{
		Marker: "upgrade_v1",
		Upgrades: []PresetUpgrade{
			{
				Key: "shared_p",
				Match: map[string][]string{
					"description":    {sha256Hex("old desc")},
					"pr_instruction": {sha256Hex("old inst")},
					"pr_goal":        {sha256Hex("old goal")},
					"run_defaults":   {sha256Hex("")},
				},
				To: Preset{Description: "new desc", PRInstruction: "new inst", PRGoal: "new goal", RunDefaults: `{"max_turns":30}`},
			},
			{
				Key:   "owned_p",
				Match: map[string][]string{"pr_instruction": {sha256Hex("old inst")}},
				To:    Preset{PRInstruction: "new inst"},
			},
		},
	}
	if err := st.RunPresetSteps([]PresetSeedStep{step}); err != nil {
		t.Fatal(err)
	}
	sp, _ := st.PresetByKey("shared_p")
	if sp.Description != "new desc" || sp.PRGoal != "new goal" || sp.RunDefaults != `{"max_turns":30}` {
		t.Fatalf("untouched shared fields not upgraded: %+v", sp)
	}
	if sp.PRInstruction != "admin edit" {
		t.Fatalf("admin-edited field overwritten: %q", sp.PRInstruction)
	}
	op, _ := st.PresetByKey("owned_p")
	if op.PRInstruction != "old inst" {
		t.Fatalf("user-owned preset modified: %q", op.PRInstruction)
	}
	// Idempotent: resetting a field does not re-apply the already-recorded step.
	st.db.Exec(`UPDATE presets SET description = 'old desc' WHERE key = 'shared_p'`)
	if err := st.RunPresetSteps([]PresetSeedStep{step}); err != nil {
		t.Fatal(err)
	}
	if sp, _ = st.PresetByKey("shared_p"); sp.Description != "old desc" {
		t.Fatalf("upgrade step ran twice: %q", sp.Description)
	}
}
