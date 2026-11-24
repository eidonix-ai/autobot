package store

import (
	"fmt"
	"strings"
	"testing"
)

func words(n int, prefix string) string {
	var w []string
	for i := 1; i <= n; i++ {
		w = append(w, fmt.Sprintf("%s%d", prefix, i))
	}
	return strings.Join(w, " ")
}

// A report written without a summary gets one made from it: its Key findings section, else its
// first 300 words, else (when it is short) itself; the first two say they were made.
func TestFallbackSummary(t *testing.T) {
	// The Key findings section, up to the next heading of its level or above, without the detail.
	report := "# Report\n\n## Key findings\n\n- the loader leaks (a.go:12)\n- the cache is stale (b.go:3)\n\n### A sub-heading inside\n\nstill the findings\n\n## Detail\n\nTHE LONG DETAIL"
	got := FallbackSummary(report)
	for _, want := range []string{"the loader leaks (a.go:12)", "the cache is stale (b.go:3)", "still the findings", "No summary was written", "Key findings"} {
		if !strings.Contains(got, want) {
			t.Errorf("the summary lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "THE LONG DETAIL") || strings.Contains(got, "## Key findings") {
		t.Errorf("the summary has what it should not:\n%s", got)
	}
	// Key findings as bold text in a heading, in any case.
	if got := FallbackSummary("## **KEY FINDINGS**\n\nthe one finding\n\n## More\n\nx"); !strings.Contains(got, "the one finding") || strings.Contains(got, "\nx") {
		t.Errorf("a bold heading: %q", got)
	}
	// No such section: the first 300 words, and no more.
	long := "Opening. " + words(400, "w")
	got = FallbackSummary(long)
	if !strings.Contains(got, "w290") || strings.Contains(got, "w300") || !strings.Contains(got, "start of the report") {
		t.Errorf("the first words: %d words, %.80q… … %.60q", len(strings.Fields(got)), got, got[max(0, len(got)-120):])
	}
	if n := len(strings.Fields(strings.SplitN(got, "\n\n(No summary", 2)[0])); n != SummaryWords {
		t.Errorf("%d words, want %d", n, SummaryWords)
	}
	// A Key findings section that is itself long is cut too.
	if got := FallbackSummary("## Key findings\n\n" + words(500, "k")); strings.Contains(got, "k400") || !strings.Contains(got, "k299") {
		t.Errorf("a long section: %d words", len(strings.Fields(got)))
	}
	// A short report is its own summary, unmarked; an empty one has none.
	if got := FallbackSummary("  A short report with no sections.\n"); got != "A short report with no sections." {
		t.Errorf("a short report: %q", got)
	}
	if FallbackSummary(" \n") != "" {
		t.Error("an empty report has a summary")
	}
	// An empty Key findings section does not count.
	if got := FallbackSummary("## Key findings\n\n## Detail\n\nthe detail"); !strings.Contains(got, "the detail") {
		t.Errorf("an empty section: %q", got)
	}
}

// A thread's notes in full: high first, then medium, then low, each group in the order written, each
// as "#N [importance] Title" and the body; a run's final reports are not notes.
func TestScratchpadText(t *testing.T) {
	entries := []PadEntry{
		{N: 1, Title: "Minor", Importance: Low, Body: "low body"},
		{N: 2, Title: "Detail", Importance: Medium, Body: "medium body"},
		{N: 3, Title: "Core", Importance: High, Body: "high body"},
		{N: 4, Title: "Report of today", Importance: High, Body: "the whole report", Report: true},
		{N: 5, Title: "Also core", Importance: High, Body: "second high body"},
		{N: 6, Title: "Another detail", Importance: Medium, Body: "second medium body"},
	}
	want := "#3 [high] Core\n\nhigh body\n\n#5 [high] Also core\n\nsecond high body\n\n#2 [medium] Detail\n\nmedium body\n\n" +
		"#6 [medium] Another detail\n\nsecond medium body\n\n#1 [low] Minor\n\nlow body"
	if got := ScratchpadText(entries); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if ScratchpadText(nil) != "" || ScratchpadText(entries[3:4]) != "" {
		t.Fatal("no notes should give no text")
	}
}

// A report's summary is kept with it in the thread, and in a run's row after its conversation is
// deleted (cut to 2 KB like the report).
func TestSummaryIsKept(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	_, _, th := startConv(t, st, ws, "ag", 1, false)
	if _, err := st.Pad(th).Add(PadEntry{Title: "Report", Importance: High, Description: "first line", Body: "the report", Summary: "the summary\nover two lines", Report: true}); err != nil {
		t.Fatal(err)
	}
	rep, ok, _ := st.Pad(th).LastReport()
	if !ok || rep.Summary != "the summary\nover two lines" || rep.Description != "first line" {
		t.Fatalf("%+v", rep)
	}
	slim := slimSummary(`{"turns":3,"summary":"` + strings.Repeat("s", 5000) + `","report":"short","plan":[1]}`)
	if !strings.Contains(slim, `"report":"short"`) || strings.Contains(slim, "plan") || len(slim) > 2300 || !strings.Contains(slim, "ssss") {
		t.Fatalf("%.200s (%d)", slim, len(slim))
	}
}
