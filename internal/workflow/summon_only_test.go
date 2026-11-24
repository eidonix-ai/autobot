package workflow

import (
	"testing"
	"time"
)

// "Only if summoned" with no activity ticked is allowed, and runs on a summons and nothing after it;
// "always" still needs an activity.
func TestSummonedWithNoActivity(t *testing.T) {
	s := DefaultEvent("pr_activity")
	s.Mode, s.SummonPhrase, s.Activity, s.TrustedOnly, s.QuietMinutes = "summoned", "#autobot:review", nil, false, 0
	if err := s.Check("pr_activity"); err != nil {
		t.Fatalf("summoned with no activity refused: %v", err)
	}
	always := s
	always.Mode = "always"
	if always.Check("pr_activity") == nil {
		t.Fatal("always with no activity accepted")
	}
	now := time.Now()
	summons := []Event{{Kind: EvComments, Who: "a", Text: "please #autobot:review", At: now.Add(-time.Minute)}}
	if r := Trigger(s, "pr_activity", summons, now, true, false); r.Outcome != Fire {
		t.Fatalf("a summons: %+v", r)
	}
	later := []Event{{Kind: EvComments, Who: "a", Text: "thanks", At: now.Add(-time.Minute)}, {Kind: "pushes", Who: "a", At: now.Add(-time.Minute)}}
	if r := Trigger(s, "pr_activity", later, now, true, true); r.Outcome == Fire {
		t.Fatalf("activity after a summons ran it: %+v", r)
	}
}
