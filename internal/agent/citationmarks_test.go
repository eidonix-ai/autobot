package agent

import (
	"context"
	"testing"
)

// A report that cites a file nothing opened is marked "(not opened)", unless the config turns the
// marks off (a preset that rewrites text): then what the agent wrote is what is kept, summary too.
func TestNoCitationMarksLeavesTheReportAsWritten(t *testing.T) {
	finish := `{"status":"met","summary":"Short: x.go:3.","report":"See x.go:3."}`
	run := func(cfg Config) *Transcript {
		cfg.Model, cfg.MaxTurns, cfg.WithSummary = &script{replies: []Message{call("f", "finish", finish)}}, 5, true
		return Run(context.Background(), cfg, nil)
	}
	if tr := run(Config{}); tr.Report != "See x.go:3 (not opened)." || tr.Summary != "Short: x.go:3 (not opened)." {
		t.Fatalf("with marks: report %q, summary %q", tr.Report, tr.Summary)
	}
	if tr := run(Config{NoCitationMarks: true}); tr.Outcome != Met || tr.Report != "See x.go:3." || tr.Summary != "Short: x.go:3." {
		t.Fatalf("without marks: %s, report %q, summary %q", tr.Outcome, tr.Report, tr.Summary)
	}
}
