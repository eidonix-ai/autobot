package workflow

import (
	"strings"
	"testing"
)

func TestConditionHolds(t *testing.T) {
	f := ItemFacts{Files: []string{"internal/web/server.go", "docs/README.md"}, Lines: 120, Author: "alice", Association: "FIRST_TIME_CONTRIBUTOR",
		Labels: []string{"Kernels"}, Title: "Faster attention", Body: "Uses the new CUDA path.", Tags: map[string]string{"value_decision": "deep review"}}
	cases := []struct {
		s    ConditionSettings
		want bool
	}{
		{ConditionSettings{Match: "all", Files: "*.go"}, true},
		{ConditionSettings{Match: "all", Files: "internal/*"}, true},
		{ConditionSettings{Match: "all", Files: "*.rs"}, false},
		{ConditionSettings{Match: "all", MoreLines: 100}, true},
		{ConditionSettings{Match: "all", MoreLines: 500}, false},
		{ConditionSettings{Match: "all", Author: "first_time"}, true},
		{ConditionSettings{Match: "all", Author: "outside"}, true},
		{ConditionSettings{Match: "all", Author: "member"}, false},
		{ConditionSettings{Match: "all", Author: "bot"}, false},
		{ConditionSettings{Match: "all", Label: "kernels"}, true},
		{ConditionSettings{Match: "all", Contains: "cuda"}, true},
		{ConditionSettings{Match: "all", Tag: "value_decision", TagHas: "DEEP"}, true},
		{ConditionSettings{Match: "all", Files: "*.go", MoreLines: 500}, false},
		{ConditionSettings{Match: "any", Files: "*.go", MoreLines: 500}, true},
	}
	for _, c := range cases {
		got, notes := c.s.Holds(f)
		if got != c.want {
			t.Errorf("%+v: %v, want %v (%s)", c.s, got, c.want, strings.Join(notes, "; "))
		}
	}
	if (ConditionSettings{Match: "all"}).Check(PR) == nil {
		t.Error("a condition with no rules passed")
	}
	if (ConditionSettings{Match: "all", Files: "*.go"}).Check(Issue) == nil {
		t.Error("an issue condition on changed files passed")
	}
}
