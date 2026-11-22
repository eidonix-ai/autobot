package store

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

func TestCaseContributions(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	c, created, err := st.OpenCase(ws, 459, "pr", "Fix", "Opened for PR #459")
	if err != nil || !created || c.Lines() != 1 {
		t.Fatalf("open: %+v %v %v", c, created, err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(c.UUID) {
		t.Fatalf("uuid %q is not a random (v4) UUID", c.UUID)
	}
	if again, created, _ := st.OpenCase(ws, 459, "pr", "Fix", "x"); created || again.UUID != c.UUID {
		t.Fatal("a second case opened for the same PR")
	}
	other, _, _ := st.OpenCase(ws, 460, "issue", "Bug", "Opened for issue #460")
	if other.UUID == c.UUID {
		t.Fatal("two cases share a uuid")
	}

	// Two agents write at once; drafts stay out of the file until finalized.
	a, _ := st.StartContribution(c.UUID, "reviewer")
	b, _ := st.StartContribution(c.UUID, "tester")
	st.SetDraft(a, "first draft")
	st.SetDraft(b, "tests: pass\ncargo: pass")
	st.SetDraft(a, "## Review\nneeds a bound on retries")
	if got, _ := st.Case(c.UUID); got.Contents != "Opened for PR #459\n" {
		t.Fatalf("a draft reached the file: %q", got.Contents)
	}
	cb, _ := st.FinalizeContribution(b)
	ca, _ := st.FinalizeContribution(a)
	if cb.StartLine != 2 || cb.EndLine != 3 || ca.StartLine != 4 || ca.EndLine != 5 {
		t.Fatalf("lines: tester %d-%d, reviewer %d-%d", cb.StartLine, cb.EndLine, ca.StartLine, ca.EndLine)
	}
	got, _ := st.Case(c.UUID)
	if got.Contents != "Opened for PR #459\ntests: pass\ncargo: pass\n## Review\nneeds a bound on retries\n" {
		t.Fatalf("file %q", got.Contents)
	}
	// Finalized means immutable.
	if err := st.SetDraft(a, "rewrite"); !errors.Is(err, ErrFinalized) {
		t.Fatalf("revised after finalizing: %v", err)
	}
	if _, err := st.FinalizeContribution(a); !errors.Is(err, ErrFinalized) {
		t.Fatalf("finalized twice: %v", err)
	}

	// A person's edit is recorded with the lines it changed; a stale one is refused.
	edited := strings.Replace(got.Contents, "cargo: pass\n", "", 1)
	edited = strings.Replace(edited, "needs a bound on retries", "needs a bound on retries and a delay", 1)
	if err := st.EditCase(c.UUID, got.Version, edited, admin); err != nil {
		t.Fatal(err)
	}
	h, _ := st.Contributions(c.UUID)
	last := h[len(h)-1]
	if last.Kind != "human" || last.Author != "admin" || last.Note != "line 3 removed; line 5 changed" {
		t.Fatalf("edit record %+v", last)
	}
	var conflict *ConflictingEdit
	if err := st.EditCase(c.UUID, got.Version, "stale", admin); !errors.As(err, &conflict) {
		t.Fatalf("stale edit: %v", err)
	}
}

func TestLineChanges(t *testing.T) {
	for _, tc := range []struct{ before, after, want string }{
		{"a\nb\nc\n", "a\nb\nc\n", "no change"},
		{"a\nb\nc\n", "a\nB\nc\n", "line 2 changed"},
		{"a\nb\nc\nd\n", "a\nd\n", "lines 2–3 removed"},
		{"a\nb\n", "a\nx\ny\nb\n", "2 lines added after line 1"},
	} {
		if got := LineChanges(tc.before, tc.after); got != tc.want {
			t.Errorf("%q -> %q: %q, want %q", tc.before, tc.after, got, tc.want)
		}
	}
}

// A workflow saved from an out-of-date copy is refused.
func TestWorkflowVersions(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	if w, _ := st.Workflow(ws.ID, "pr"); w.Version != 0 {
		t.Fatalf("new workflow version %d", w.Version)
	}
	v, err := st.SaveWorkflow(ws.ID, "pr", 0, `{"nodes":[],"links":[]}`, admin)
	if err != nil || v != 1 {
		t.Fatalf("first save: %d %v", v, err)
	}
	if _, err := st.SaveWorkflow(ws.ID, "pr", 0, `{"nodes":[],"links":[]}`, admin); !errors.Is(err, ErrStaleWorkflow) {
		t.Fatalf("stale save: %v", err)
	}
	if v, err := st.SaveWorkflow(ws.ID, "pr", 1, `{"nodes":[],"links":[]}`, admin); err != nil || v != 2 {
		t.Fatalf("second save: %d %v", v, err)
	}
}
