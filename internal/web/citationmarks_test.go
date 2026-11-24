package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// The Editor preset reaches a database seeded before it, once: with the marks off, and a removed one
// stays removed at the next start.
func TestEditorPresetIsSeededOnce(t *testing.T) {
	st := olderDB(t)
	if _, err := New(st, fakeGitHub{}); err != nil {
		t.Fatal(err)
	}
	ed, err := st.PresetByKey("editor_prose")
	if err != nil || !ed.Shared() || ed.Name != "Editor - Prose Cleanup" || !ed.NoCitationMarks || ed.Closing != "" || ed.PRInstruction != ed.IssueInstruction {
		t.Fatalf("the editor preset: %+v %v", ed, err)
	}
	if ed.Additional != "{input.summary}" || !strings.HasSuffix(ed.PRInstruction, "## The draft") {
		t.Fatalf("the editor preset's draft: additional %q, instruction ends %q", ed.Additional, ed.PRInstruction[len(ed.PRInstruction)-30:])
	}
	if all, _ := st.Presets(); len(all) != len(workflow.SeedPresets) {
		t.Fatalf("%d presets, want %d", len(all), len(workflow.SeedPresets))
	}
	if err := st.RemovePreset(ed.ID, func(store.StoredGraph) (string, bool, error) { return "", false, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := New(st, fakeGitHub{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PresetByKey("editor_prose"); err == nil {
		t.Error("a removed Editor preset came back")
	}
}

// A block follows its preset's setting: the Editor turns the marks off, Gather Information leaves them on.
func TestBlockCitationMarksFollowThePreset(t *testing.T) {
	p := newPresetEnv(t)
	for key, want := range map[string]bool{"editor_prose": false, "gather_information": true} {
		a, err := p.ui.agentSettings(p.onPreset("x", key), workflow.PR)
		if err != nil || !a.OnPreset() || a.EffectiveCitationMarks() != want {
			t.Errorf("%s: marks %v, want %v (%v)", key, a.EffectiveCitationMarks(), want, err)
		}
	}
	custom := p.onPreset("y", "custom")
	if a, err := p.ui.agentSettings(custom, workflow.PR); err != nil || !a.EffectiveCitationMarks() {
		t.Errorf("custom: %v", err)
	}
}

// The preset form has the box (ticked: marks on), saves it on create and edit, shows it read-only to
// others, and a copy keeps it.
func TestPresetFormCitationMarks(t *testing.T) {
	p := newPresetEnv(t)
	ticked := func(page string) bool {
		i := strings.Index(page, `name="citation_marks"`)
		if i < 0 {
			t.Fatalf("no citation marks box on the page:\n%s", page)
		}
		return strings.Contains(page[i:i+strings.Index(page[i:], ">")], " checked")
	}
	if code, page := p.be.get("/presets/new"); code != 200 || !ticked(page) || !strings.Contains(page, "Mark files cited but not opened") || !strings.Contains(page, "“(not opened)”") {
		t.Fatalf("the new preset form: %d ticked=%v", code, ticked(page))
	}
	on := p.create(p.be, "Marks on", url.Values{"citation_marks": {"1"}})
	off := p.create(p.be, "Marks off", nil) // an unticked box sends nothing
	if on.NoCitationMarks || !off.NoCitationMarks {
		t.Fatalf("stored: on %+v, off %+v", on.NoCitationMarks, off.NoCitationMarks)
	}
	if _, page := p.be.get(presetURL(on, "")); !ticked(page) {
		t.Error("the box is not ticked for a preset with marks on")
	}
	if _, page := p.be.get(presetURL(off, "")); ticked(page) {
		t.Error("the box is ticked for a preset with marks off")
	}
	// Edit: tick it, then untick it.
	if code, _, _ := p.post(p.be, presetURL(off, ""), p.form("Marks off", url.Values{"citation_marks": {"1"}})); code != http.StatusSeeOther {
		t.Fatalf("edit: %d", code)
	}
	if got, _ := p.st.Preset(off.ID); got.NoCitationMarks {
		t.Error("ticking the box did not turn the marks on")
	}
	if code, _, _ := p.post(p.be, presetURL(off, ""), p.form("Marks off", nil)); code != http.StatusSeeOther {
		t.Fatalf("edit: %d", code)
	}
	if got, _ := p.st.Preset(off.ID); !got.NoCitationMarks {
		t.Error("unticking the box did not turn the marks off")
	}
	// Someone who cannot change it sees the box, disabled.
	if _, page := p.bf.get(presetURL(off, "")); ticked(page) || !strings.Contains(page[strings.Index(page, `name="citation_marks"`):], "disabled") {
		t.Error("another user's preset shows the box ticked or editable")
	}
	// A copy keeps the setting.
	if code, _, _ := p.post(p.bf, presetURL(off, "/copy"), nil); code != http.StatusSeeOther {
		t.Fatalf("copy: %d", code)
	}
	all, _ := p.st.Presets()
	for _, c := range all {
		if c.Name == "Copy of Marks off" && (c.OwnerID != p.frank.ID || !c.NoCitationMarks) {
			t.Errorf("the copy: %+v", c)
		}
	}
	// The starting additional instructions are saved, shown, kept by a copy and checked like the other texts.
	withText := p.create(p.be, "With text", url.Values{"additional": {"  {input.summary}  "}})
	if withText.Additional != "{input.summary}" {
		t.Errorf("stored %q", withText.Additional)
	}
	if _, page := p.be.get(presetURL(withText, "")); !strings.Contains(page, "Additional instructions to start with") || !strings.Contains(page, ">{input.summary}</textarea>") {
		t.Errorf("the form does not show the starting text:\n%s", page)
	}
	if code, _, _ := p.post(p.bf, presetURL(withText, "/copy"), nil); code != http.StatusSeeOther {
		t.Fatalf("copy: %d", code)
	}
	all, _ = p.st.Presets()
	for _, c := range all {
		if c.Name == "Copy of With text" && c.Additional != "{input.summary}" {
			t.Errorf("the copy: %+v", c)
		}
	}
	bad := p.form("Bad text", url.Values{"additional": {"{no_such_tag}"}, "citation_marks": {"1"}})
	if code, _, _ := p.post(p.be, "/presets", bad); code != http.StatusBadRequest {
		t.Errorf("an unknown tag in the starting text: %d", code)
	}
	// The block dialog's option carries it, escaped, for the form's script to fill in.
	p.saveGraph(p.ws, workflow.PR, p.onPreset("ag", "gather_information"))
	_, dialog := p.be.get("/w/o/r/workflows/pr/nodes/ag")
	if !strings.Contains(dialog, `value="editor_prose" data-instruction=`) || !strings.Contains(dialog, `data-additional="{input.summary}"`) ||
		!strings.Contains(dialog, `value="gather_information" data-instruction=`) {
		t.Errorf("the dialog's options:\n%s", dialog)
	}
	// The Editor is a built-in with the box unticked.
	ed, _ := p.st.PresetByKey("editor_prose")
	if _, page := p.bd.get(presetURL(ed, "")); ticked(page) {
		t.Error("the Editor preset shows marks on")
	}
}
