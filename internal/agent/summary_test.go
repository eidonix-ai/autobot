package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/store"
)

// finishWith is research, the save turn and the writing session, which finishes with args.
func finishWith(args string) []Message {
	return append(slices.Clone(researchOver),
		call("x", "scratchpad_index", `{}`), call("y", "read_file", `{"path":"b.go"}`),
		call("fin2", "finish", args))
}

// The writing session's finish takes a summary and the report, and the run keeps both; a file cited
// in either that nothing opened is marked in both.
func TestWriterFinishKeepsSummaryAndReport(t *testing.T) {
	cr := closing(t, finishWith(`{"status":"met","summary":"Short: a.go:1 and c.go:3.","report":"## Key findings\n\n- a.go:1 and b.go:2.\n\n## Detail\n\nMore: c.go:3."}`), 15, "a.go")
	tr := cr.tr
	if tr.Outcome != Met || tr.Summary != "Short: a.go:1 and c.go:3 (not opened)." {
		t.Fatalf("%s: summary %q", tr.Outcome, tr.Summary)
	}
	if want := "## Key findings\n\n- a.go:1 and b.go:2.\n\n## Detail\n\nMore: c.go:3 (not opened)."; tr.Report != want {
		t.Fatalf("report %q", tr.Report)
	}
	if w := tr.Writing; w == nil || w.Summary != tr.Summary || w.Report != tr.Report {
		t.Fatalf("the writing session's own transcript: %+v", w)
	}
	// The saved JSON has both.
	b, _ := json.Marshal(tr)
	if !strings.Contains(string(b), `"summary":"Short:`) || !strings.Contains(string(b), `"report":"## Key findings`) {
		t.Fatalf("%s", b)
	}
}

// A finish with no summary is not refused, so the report is never lost: the summary is the report's
// Key findings section if it has one, else its first words, and says it was not written.
func TestWriterFinishWithoutSummaryFallsBack(t *testing.T) {
	cr := closing(t, finishWith(`{"status":"met","report":"## Key findings\n\n- the leak in a.go:1.\n\n## Detail\n\nTHE DETAIL."}`), 15, "a.go")
	tr := cr.tr
	if tr.Outcome != Met || tr.Writing.Outcome != Met || !strings.Contains(tr.Report, "THE DETAIL.") {
		t.Fatalf("the report was refused or lost: %+v", tr.Writing)
	}
	if !strings.Contains(tr.Summary, "the leak in a.go:1.") || strings.Contains(tr.Summary, "THE DETAIL") || !strings.Contains(tr.Summary, "No summary was written") {
		t.Fatalf("from the Key findings: %q", tr.Summary)
	}
	// No such section: the first 300 words.
	long := strings.TrimSpace(strings.Repeat("word ", 400))
	cr = closing(t, finishWith(`{"status":"met","summary":"  ","report":"`+long+`"}`), 15, "a.go")
	if n := len(strings.Fields(strings.SplitN(cr.tr.Summary, "\n\n(No summary", 2)[0])); n != 300 || !strings.Contains(cr.tr.Summary, "No summary was written") {
		t.Fatalf("the first words: %d words, %q", n, cr.tr.Summary[len(cr.tr.Summary)-80:])
	}
	// A report is still needed.
	m := &script{replies: []Message{call("f", "finish", `{"status":"met","summary":"only a summary"}`), call("g", "finish", `{"status":"met","summary":"s","report":"r"}`)}}
	tr = Run(context.Background(), Config{Model: m, MaxTurns: 5, WithSummary: true}, nil)
	if !strings.Contains(last(m.seen[1]), "finish needs a status (met or blocked) and a report") || tr.Report != "r" || tr.Summary != "s" {
		t.Fatalf("%+v", tr)
	}
}

// Every other way a run ends with a report gives it a summary: the research's own finish (its short
// report), a writing session that wrote nothing, an answer in words at the last turn.
func TestEveryEndOfARunHasASummary(t *testing.T) {
	// The research's finish is unchanged: one short report, and a summary argument is not taken.
	m := &script{replies: []Message{call("f", "finish", `{"status":"met","summary":"IGNORED","report":"The research report."}`)}}
	if tr := Run(context.Background(), Config{Model: m, MaxTurns: 5}, nil); tr.Report != "The research report." || tr.Summary != "The research report." {
		t.Fatalf("closing none: %+v", tr)
	}
	// The writing session wrote nothing: the research's own report stays, and its summary.
	cr := closing(t, researchOver, 15, "a.go")
	if cr.tr.Report != "Found it in a.go:1." || cr.tr.Summary != "Found it in a.go:1." {
		t.Fatalf("the writer failed: %q / %q", cr.tr.Report, cr.tr.Summary)
	}
	// A report longer than a summary, from a run that ended in words: cut, and says so.
	long := strings.Repeat("sentence about a.go:1 ", 150)
	m = &script{replies: []Message{call("p", "update_plan", `{"steps":[]}`), {Role: "assistant", Content: long}}}
	tr := Run(context.Background(), Config{Model: m, MaxTurns: 2}, nil) // the second is its last, answered in words
	if tr.Outcome != Budget || strings.TrimSpace(tr.Report) == "" || !strings.Contains(tr.Summary, "No summary was written") {
		t.Fatalf("an answer in words: %s %.40q / %.40q", tr.Outcome, tr.Report, tr.Summary)
	}
	m = &script{replies: []Message{{Content: "a"}, {Content: "b"}, {Content: "c"}, {Content: "d"}}}
	if tr := Run(context.Background(), Config{Model: m, MaxTurns: 10}, nil); tr.Outcome != Unfinished || tr.Summary != "d" {
		t.Fatalf("unfinished: %+v", tr)
	}
	// A run with no report has no summary.
	m = &script{}
	if tr := Run(context.Background(), Config{Model: m, MaxTurns: 2}, nil); tr.Report != "" || tr.Summary != "" {
		t.Fatalf("%+v", tr)
	}
	// The notes report, for a run that said nothing: it gets one too.
	pad := &memPad{}
	pad.Add(store.PadEntry{Author: "main", Title: "Found", Importance: store.High, Body: "body one"})
	tr = &Transcript{Report: NotesReport(pad)}
	tr.EnsureSummary()
	if tr.Summary != "## Found\n\nbody one" {
		t.Fatalf("%q", tr.Summary)
	}
}

// What each session is offered: the writing session's finish takes the summary and the report,
// both required; the research's, and a block's with no closing step, take only the report.
func TestFinishDefinitions(t *testing.T) {
	required := func(c *Conversation) (props, req []string) {
		var spec struct {
			Properties map[string]any
			Required   []string
		}
		if err := json.Unmarshal(c.Tools[0].Parameters, &spec); err != nil || c.Tools[0].Name != "finish" {
			t.Fatalf("%v %s", err, c.Tools[0].Name)
		}
		for k := range spec.Properties {
			props = append(props, k)
		}
		slices.Sort(props)
		return props, spec.Required
	}
	props, req := required(NewWriterConversation("m", 1, "s", nil))
	if !slices.Equal(props, []string{"report", "status", "summary"}) || !slices.Equal(req, []string{"status", "summary", "report"}) {
		t.Errorf("the writer: %v required %v", props, req)
	}
	if d := NewWriterConversation("m", 1, "s", nil).Tools[0].Description; !strings.Contains(d, "300 words") || !strings.Contains(d, "Key findings") {
		t.Errorf("the writer's finish: %s", d)
	}
	for name, c := range map[string]*Conversation{"research": NewResearchConversation("m", 1, "s", nil, true), "plain": NewConversation("m", 1, "s", nil)} {
		if props, req := required(c); !slices.Equal(props, []string{"report", "status"}) || !slices.Equal(req, []string{"status", "report"}) {
			t.Errorf("%s: %v required %v", name, props, req)
		}
	}
	if !strings.Contains(writerPrompt, "300 words") || !strings.Contains(writerPrompt, "Key findings") {
		t.Errorf("the writing prompt does not ask for both: %s", writerPrompt)
	}
}

// Sub-agents are listed to the agent by their summary.
func TestSubAgentsAreListedBySummary(t *testing.T) {
	got := subLines([]SubInfo{{Name: "s1", Title: "callers", Outcome: "met", Summary: "Three callers.\nsecond line"}})
	if got != `s1 "callers": met — Three callers.` {
		t.Fatalf("%q", got)
	}
}

// A writer that dumps the full report into summary (leaving report as "see summary" or shorter)
// or writes an oversized summary is asked to fix it; if it still exhausts retries or turns, the
// full text is preserved in Report and Summary falls back cleanly so a full report is never posted
// as a summary.
func TestWriterFinishRejectsSwappedOrOversizedSummary(t *testing.T) {
	fullReport := strings.TrimSpace("## Key findings\n\n- Concrete bug in a.go:1.\n\n## Detail\n\n" + strings.Repeat("Detailed analysis of a.go:1 and b.go:2. ", 260))

	// 1. Writer puts the full report in summary and "See summary" in report, then fixes it after rejection.
	badArgs, _ := json.Marshal(map[string]string{
		"status":  "met",
		"report":  "See summary field for the report body.",
		"summary": fullReport,
	})
	goodArgs, _ := json.Marshal(map[string]string{
		"status":  "met",
		"report":  fullReport,
		"summary": "### Recommendation: Changes Requested\n- Fix a.go:1.",
	})
	m := &script{replies: []Message{
		call("f1", "finish", string(badArgs)),
		call("f2", "finish", string(goodArgs)),
	}}
	tr := Run(context.Background(), Config{Model: m, MaxTurns: 5, WithSummary: true, NoCitationMarks: true}, nil)
	if tr.Outcome != Met {
		t.Fatalf("outcome = %s, want met", tr.Outcome)
	}
	if !strings.Contains(last(m.seen[1]), "longer than your report") {
		t.Fatalf("expected swapped-summary rejection, got: %s", last(m.seen[1]))
	}
	if tr.Report != fullReport || !strings.HasPrefix(tr.Summary, "### Recommendation:") {
		t.Fatalf("unexpected report/summary after fix: rep=%d sum=%q", len(tr.Report), tr.Summary)
	}

	// 2. Writer puts oversized (>8000 chars) summary even when report is longer -> rejected, then fixed.
	bigSummary := strings.Repeat("Long verbose bullet about a.go:1. ", 260) // ~8840 chars
	biggerReport := fullReport + strings.Repeat("More detail. ", 300)
	overArgs, _ := json.Marshal(map[string]string{
		"status":  "met",
		"report":  biggerReport,
		"summary": bigSummary,
	})
	m2 := &script{replies: []Message{
		call("f1", "finish", string(overArgs)),
		call("f2", "finish", string(goodArgs)),
	}}
	tr2 := Run(context.Background(), Config{Model: m2, MaxTurns: 5, WithSummary: true, NoCitationMarks: true}, nil)
	if tr2.Outcome != Met || !strings.Contains(last(m2.seen[1]), "summary is too long") {
		t.Fatalf("expected oversized-summary rejection: outcome=%s seen=%s", tr2.Outcome, last(m2.seen[1]))
	}

	// 3. Writer exhausts retries while keeping full report in summary -> promoted safely to Report, FallbackSummary in Summary.
	m3 := &script{replies: []Message{
		call("f1", "finish", string(badArgs)),
		call("f2", "finish", string(badArgs)),
		call("f3", "finish", string(badArgs)),
		call("f4", "finish", string(badArgs)),
	}}
	tr3 := Run(context.Background(), Config{Model: m3, MaxTurns: 6, WithSummary: true, NoCitationMarks: true}, nil)
	if tr3.Outcome != Met {
		t.Fatalf("outcome = %s, want met", tr3.Outcome)
	}
	if tr3.Report != fullReport {
		t.Fatalf("full report was not promoted from summary: len(report)=%d", len(tr3.Report))
	}
	if len(tr3.Summary) >= 2000 || !strings.Contains(tr3.Summary, "Concrete bug in a.go:1") {
		t.Fatalf("expected short fallback summary, got len=%d: %q", len(tr3.Summary), tr3.Summary)
	}
}
