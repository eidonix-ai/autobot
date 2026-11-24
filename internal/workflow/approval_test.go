package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

// A Human Approval needs its question; the details are optional; a block saved before the question
// existed (details only) can still run.
func TestApprovalSettingsCheck(t *testing.T) {
	for _, kind := range []string{PR, Issue} {
		d := DefaultApproval(kind)
		if d.Question == "" || d.Request != "" || d.Check() != nil {
			t.Errorf("%s default: %+v %v", kind, d, d.Check())
		}
	}
	for _, c := range []struct {
		s    ApprovalSettings
		want string
	}{
		{ApprovalSettings{Request: "Details.", Hours: 1}, "write the question asked above the buttons"},
		{ApprovalSettings{Question: "  ", Hours: 1}, "write the question asked above the buttons"},
		{ApprovalSettings{Question: strings.Repeat("q", 1001), Hours: 1}, "the question is limited to 1,000 characters"},
		{ApprovalSettings{Question: "Go?", Request: strings.Repeat("d", 4001), Hours: 1}, "the details are limited to 4,000 characters"},
		{ApprovalSettings{Question: "Go?", Hours: 0}, "the time limit is 1 to 336 hours (two weeks)"},
		{ApprovalSettings{Question: "Go?", Hours: 24}, ""},
	} {
		err := c.s.Check()
		if (c.want == "") != (err == nil) || (err != nil && err.Error() != c.want) {
			t.Errorf("%+v: %v, want %q", c.s, err, c.want)
		}
	}
	if err := (ApprovalSettings{Request: "Run it?", Hours: 1}).CheckRun(); err != nil {
		t.Errorf("a block from before the question does not run: %v", err)
	}
	if err := (ApprovalSettings{Hours: 1}).CheckRun(); err == nil {
		t.Error("a block with neither question nor details runs")
	}
}

// The question is a template: an unknown tag in it is flagged, and a rename of a block rewrites it.
func TestApprovalQuestionIsATemplate(t *testing.T) {
	g := problemGraph()
	if err := g.CheckNodeTemplates(PR, "us", "Run {nosuch}?"); err == nil || err.Error() != "{nosuch}: no block is called Nosuch" {
		t.Errorf("an unknown tag in a question: %v", err)
	}
	raw, _ := json.Marshal(ApprovalSettings{Question: "Skip {old}?", Request: "See {old}.", Hours: 1})
	got, changed := Retag(Node{ID: "ap", Type: Approval, Settings: raw}, PR, "old", "new")
	s, _ := SettingsOf(got, DefaultApproval(PR))
	if !changed || s.Question != "Skip {new}?" || s.Request != "See {new}." {
		t.Errorf("%v %+v", changed, s)
	}
}
