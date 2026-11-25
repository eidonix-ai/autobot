package workflow

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

// settings is a node's defaults with the filters the defaults switch on turned off, so that a case
// tests the one rule it names, and then the case's changes.
func settings(typ string, change func(*EventSettings)) EventSettings {
	s := DefaultEvent(typ).WithOptOut("Autobot: ignore") // the workspace's opt-out tag
	s.Authors, s.OverLimit, s.WaitCI, s.ReviewRequest = "all", "skip", "no", false
	if change != nil {
		change(&s)
	}
	return s
}

func ev(kind, who, text string, trusted bool) Event {
	return Event{Kind: kind, Who: who, Text: text, At: t0, Trusted: trusted}
}

// facts is an open, ready pull request with the given events, targeting main.
func facts(events ...Event) Facts {
	return Facts{Now: t0.Add(time.Hour), Open: true, Author: "alice", Base: "main", DefaultBranch: "main", Events: events}
}

func TestDecide(t *testing.T) {
	ready := ev(EvReady, "alice", "", false)
	summon := func(trusted bool) Event { return ev(EvComments, "carol", "@autobot review please", trusted) }
	trustedSummons := []Comment{{Author: "carol", Body: "@autobot review please", Trusted: true}}
	cases := []struct {
		name  string
		typ   string
		set   func(*EventSettings)
		facts func(*Facts)
		evs   []Event
		want  Outcome
		why   string // part of the reason
	}{
		// Each event.
		{"ready fires", "pr_ready", nil, nil, []Event{ready}, Fire, "ready for review"},
		{"ready needs the event", "pr_ready", nil, nil, nil, Idle, ""},
		{"ready ignores a draft", "pr_ready", nil, func(f *Facts) { f.Draft = true }, []Event{ready}, Idle, ""},
		{"draft fires", "pr_draft", nil, func(f *Facts) { f.Draft = true }, []Event{ev(EvDraft, "alice", "", false)}, Fire, "draft"},
		{"draft ignores a ready pull request", "pr_draft", nil, nil, []Event{ev(EvDraft, "alice", "", false)}, Idle, ""},
		{"issue opened fires", "issue_opened", nil, nil, []Event{ev(EvOpened, "bob", "", false)}, Fire, "opened"},
		{"closed item does nothing", "pr_ready", nil, func(f *Facts) { f.Open = false }, []Event{ready}, Idle, ""},

		// Summoned and always.
		{"always ignores summons text", "pr_ready", nil, nil, []Event{ready, summon(false)}, Fire, ""},
		{"summoned needs a summons", "pr_ready", func(s *EventSettings) { s.Mode = "summoned" }, nil, []Event{ready}, Idle, ""},
		{"summoned by a trusted comment", "pr_ready", func(s *EventSettings) { s.Mode = "summoned" }, nil, []Event{summon(true)}, Fire, "summoned by carol"},
		{"untrusted summoner refused", "pr_ready", func(s *EventSettings) { s.Mode = "summoned" }, nil, []Event{summon(false)}, Skip, "carol summoned it"},
		{"anyone may summon when trust is off", "pr_ready", func(s *EventSettings) { s.Mode, s.TrustedOnly = "summoned", false }, nil, []Event{summon(false)}, Fire, ""},
		{"a summons with the stop tag is no summons", "pr_ready", func(s *EventSettings) { s.Mode = "summoned" }, nil,
			[]Event{ev(EvComments, "carol", "@autobot review. Autobot: ignore", true)}, Idle, ""},
		{"review request summons", "pr_ready", func(s *EventSettings) { s.Mode, s.ReviewRequest = "summoned", true }, nil, []Event{ev(EvReviewRequested, "", "", false)}, Fire, "review was requested"},
		{"review request ignored when off", "pr_ready", func(s *EventSettings) { s.Mode = "summoned" }, nil, []Event{ev(EvReviewRequested, "", "", false)}, Idle, ""},
		{"summoned issue", "issue_opened", func(s *EventSettings) { s.Mode, s.SummonPhrase = "summoned", "@autobot" }, nil, []Event{ev(EvComments, "carol", "hey @autobot look", true)}, Fire, ""},

		// The stop tag.
		{"stop tag in the description", "pr_ready", nil, func(f *Facts) { f.Body = "Autobot: ignore" }, []Event{ready}, Skip, "its description"},
		{"stop tag from a trusted commenter", "pr_ready", nil, func(f *Facts) { f.Comments = []Comment{{"carol", "Autobot: ignore", true}} }, []Event{ready}, Skip, "a comment by carol"},
		{"stop tag from an untrusted commenter does not stop", "pr_ready", nil, func(f *Facts) { f.Comments = []Comment{{"mallory", "Autobot: ignore", false}} }, []Event{ready}, Fire, ""},
		{"stop tag from a stranger does not stop even when trust is off", "pr_ready", func(s *EventSettings) { s.TrustedOnly = false }, func(f *Facts) { f.Comments = []Comment{{"mallory", "Autobot: ignore", false}} }, []Event{ready}, Fire, ""},
		{"stop tag from the author stops", "pr_ready", nil, func(f *Facts) { f.Comments = []Comment{{"alice", "Autobot: ignore", false}} }, []Event{ready}, Skip, "a comment by alice"},
		{"stop tag from the author stops whatever the block trusts", "pr_ready", func(s *EventSettings) { s.TrustedOnly = false }, func(f *Facts) { f.Comments = []Comment{{"Alice", "Autobot: ignore", false}} }, []Event{ready}, Skip, "a comment by Alice"},
		{"no opt-out tag, no opt-out", "pr_ready", func(s *EventSettings) { s.StopTag = "" }, func(f *Facts) { f.Body = "Autobot: ignore" }, []Event{ready}, Fire, ""},
		{"an old tag of the block is ignored", "pr_ready", func(s *EventSettings) { s.StopTag = "Autobot: ignore" }, func(f *Facts) { f.Body = "Old: stop" }, []Event{ready}, Fire, ""},
		{"explicit summons beats an earlier stop tag", "pr_ready", func(s *EventSettings) { s.Mode = "summoned" }, func(f *Facts) { f.Comments = []Comment{{"carol", "Autobot: ignore", true}} }, []Event{summon(true)}, Fire, "summoned by carol"},
		{"explicit summons beats stop tag in description", "pr_ready", nil, func(f *Facts) { f.Body = "Autobot: ignore"; f.Comments = []Comment{{"carol", "@autobot review", true}} }, []Event{ready}, Fire, "ready"},
		{"stop tag after an earlier summons stops", "pr_ready", nil, func(f *Facts) { f.Comments = []Comment{{"carol", "@autobot review", true}, {"alice", "Autobot: ignore", false}} }, []Event{ready}, Skip, "a comment by alice"},
		{"untrusted summons does not beat stop tag", "pr_ready", nil, func(f *Facts) { f.Body = "Autobot: ignore"; f.Comments = []Comment{{"mallory", "@autobot review", false}} }, []Event{ready}, Skip, "its description"},
		{"always mode fires on explicit summons when unreviewed", "pr_ready", nil, func(f *Facts) { f.Body = "Autobot: ignore" }, []Event{summon(true)}, Fire, "summoned by carol"},
		{"always mode ignores later summons once case exists", "pr_ready", nil, func(f *Facts) { f.HasCase = true }, []Event{summon(true)}, Idle, ""},
		{"always mode fires on edit removing stop tag when previously opted out", "pr_ready", nil, func(f *Facts) { f.Body = "cleaned description"; f.WasOptedOut = true }, []Event{ev("edits", "", "", false)}, Fire, "description was edited"},
		{"always mode ignores edits on old items never opted out", "pr_ready", nil, func(f *Facts) { f.Body = "edited old PR" }, []Event{ev("edits", "", "", false)}, Idle, ""},
		{"always mode ignores edits once case exists", "pr_ready", nil, func(f *Facts) { f.HasCase = true; f.WasOptedOut = true }, []Event{ev("edits", "", "", false)}, Idle, ""},

		// Filters.
		{"other branch", "pr_ready", nil, func(f *Facts) { f.Base = "dev" }, []Event{ready}, Skip, "targets dev, not main"},
		{"named branches", "pr_ready", func(s *EventSettings) { s.Branches = "dev, release" }, func(f *Facts) { f.Base = "dev" }, []Event{ready}, Fire, ""},
		{"skip bots", "pr_ready", func(s *EventSettings) { s.Authors = "skip_bots" }, func(f *Facts) { f.Author = "dependabot[bot]" }, []Event{ready}, Skip, "is a bot"},
		{"only bots", "pr_ready", func(s *EventSettings) { s.Authors = "only_bots" }, nil, []Event{ready}, Skip, "is not a bot"},
		{"skip maintainers", "pr_ready", func(s *EventSettings) { s.SkipMaintain = true }, func(f *Facts) { f.MaintainerKnown, f.AuthorMaintains = true, true }, []Event{ready}, Skip, "maintains"},
		{"maintainer allowed when not skipped", "pr_ready", nil, func(f *Facts) { f.MaintainerKnown, f.AuthorMaintains = true, true }, []Event{ready}, Fire, ""},
		{"too big", "pr_ready", func(s *EventSettings) { s.MaxLines = 100 }, func(f *Facts) { f.Lines = 101 }, []Event{ready}, Skip, "over the limit of 100"},
		{"within the size limit", "pr_ready", func(s *EventSettings) { s.MaxLines = 100 }, func(f *Facts) { f.Lines = 100 }, []Event{ready}, Fire, ""},
		{"too big but summoned", "pr_ready", func(s *EventSettings) { s.MaxLines, s.OverLimit = 100, "summoned" }, func(f *Facts) { f.Lines, f.Comments = 500, trustedSummons }, []Event{ready}, Fire, ""},
		{"too big and not summoned", "pr_ready", func(s *EventSettings) { s.MaxLines, s.OverLimit = 100, "summoned" }, func(f *Facts) { f.Lines = 500 }, []Event{ready}, Skip, "not summoned"},
		{"too big, summoned by an untrusted comment", "pr_ready", func(s *EventSettings) { s.MaxLines, s.OverLimit = 100, "summoned" },
			func(f *Facts) { f.Lines, f.Comments = 500, []Comment{{"mallory", "@autobot review", false}} }, []Event{ready}, Skip, "not summoned"},
		{"filters do not apply to issues", "issue_opened", func(s *EventSettings) { s.Authors = "only_bots" }, nil, []Event{ev(EvOpened, "bob", "", false)}, Fire, ""},

		// PR Activity.
		{"activity after the quiet period", "pr_activity", func(s *EventSettings) { s.Activity, s.QuietMinutes = []string{"pushes"}, 15 },
			func(f *Facts) { f.Now = t0.Add(15 * time.Minute) }, []Event{ev("pushes", "", "", false)}, Fire, "pushes"},
		{"activity inside the quiet period waits", "pr_activity", func(s *EventSettings) { s.Activity, s.QuietMinutes = []string{"pushes"}, 15 },
			func(f *Facts) { f.Now = t0.Add(14 * time.Minute) }, []Event{ev("pushes", "", "", false)}, Wait, "quiet"},
		{"more activity restarts the quiet period", "pr_activity", func(s *EventSettings) { s.Activity, s.QuietMinutes = []string{"pushes", "labels"}, 15 },
			func(f *Facts) { f.Now = t0.Add(20 * time.Minute) },
			[]Event{ev("pushes", "", "", false), {Kind: "labels", At: t0.Add(10 * time.Minute)}}, Wait, "quiet"},
		{"activity of other kinds is ignored", "pr_activity", func(s *EventSettings) { s.Activity = []string{"pushes"} }, nil, []Event{ev("labels", "", "", false)}, Idle, ""},
		{"activity needs a case file", "pr_activity", func(s *EventSettings) { s.Activity, s.CaseExists = []string{"pushes"}, true }, nil, []Event{ev("pushes", "", "", false)}, Skip, "no case file"},
		{"activity with a case file", "pr_activity", func(s *EventSettings) { s.Activity, s.CaseExists = []string{"pushes"}, true }, func(f *Facts) { f.HasCase = true }, []Event{ev("pushes", "", "", false)}, Fire, ""},
		{"summoned activity before any summons", "pr_activity", func(s *EventSettings) { s.Mode, s.Activity = "summoned", []string{"pushes"} }, nil, []Event{ev("pushes", "", "", false)}, Idle, ""},
		{"summoned activity after a summons", "pr_activity", func(s *EventSettings) { s.Mode, s.Activity = "summoned", []string{"pushes"} },
			func(f *Facts) { f.Comments = trustedSummons }, []Event{ev("pushes", "", "", false)}, Fire, ""},
		{"a new summons is activity", "pr_activity", func(s *EventSettings) { s.Mode, s.Activity = "summoned", []string{"pushes"} }, nil, []Event{summon(true)}, Fire, "summoned by carol"},
		{"summoned review request with no activity ticked", "pr_activity", func(s *EventSettings) { s.Mode, s.Activity, s.ReviewRequest = "summoned", nil, true }, nil, []Event{ev(EvReviewRequested, "", "", false)}, Fire, "review was requested"},
		{"always activity: trusted summons skips quiet period", "pr_activity", func(s *EventSettings) { s.Activity, s.QuietMinutes = []string{"pushes"}, 90 },
			func(f *Facts) { f.Now = t0.Add(10 * time.Minute) },
			[]Event{ev("pushes", "", "", false), {Kind: EvComments, Who: "carol", Text: "@autobot review", At: t0.Add(5 * time.Minute), Trusted: true}}, Fire, "summoned by carol"},
		{"always activity: review request skips quiet period", "pr_activity", func(s *EventSettings) { s.Activity, s.QuietMinutes, s.ReviewRequest = []string{"pushes"}, 90, true },
			func(f *Facts) { f.Now = t0.Add(10 * time.Minute) },
			[]Event{ev("pushes", "", "", false), {Kind: EvReviewRequested, At: t0.Add(5 * time.Minute)}}, Fire, "review was requested"},
		{"always activity: summons without pending activity fires code_not_changed", "pr_activity", func(s *EventSettings) { s.Activity, s.QuietMinutes = []string{"pushes"}, 90 },
			func(f *Facts) { f.Now = t0.Add(10 * time.Minute) },
			[]Event{summon(true)}, Fire, "code not changed"},
		{"always activity: untrusted summons without pending activity does nothing", "pr_activity", func(s *EventSettings) { s.Activity, s.QuietMinutes = []string{"pushes"}, 90 },
			func(f *Facts) { f.Now = t0.Add(10 * time.Minute) },
			[]Event{summon(false)}, Idle, ""},
		{"always activity: untrusted summons neither skips quiet nor cancels run", "pr_activity", func(s *EventSettings) { s.Activity, s.QuietMinutes = []string{"pushes"}, 90 },
			func(f *Facts) { f.Now = t0.Add(10 * time.Minute) },
			[]Event{ev("pushes", "", "", false), {Kind: EvComments, Who: "mallory", Text: "@autobot review", At: t0.Add(5 * time.Minute), Trusted: false}}, Wait, "quiet"},
		{"always activity: summons before later push still respects quiet period", "pr_activity", func(s *EventSettings) { s.Activity, s.QuietMinutes = []string{"pushes"}, 90 },
			func(f *Facts) { f.Now = t0.Add(10 * time.Minute) },
			[]Event{summon(true), {Kind: "pushes", At: t0.Add(5 * time.Minute)}}, Wait, "quiet"},
		{"activity filters apply", "pr_activity", func(s *EventSettings) { s.Activity, s.Authors = []string{"pushes"}, "skip_bots" },
			func(f *Facts) { f.Author = "x[bot]" }, []Event{ev("pushes", "", "", false)}, Skip, "bot"},

		// Waiting for CI.
		{"CI still running", "pr_ready", func(s *EventSettings) { s.WaitCI = "finished" }, func(f *Facts) { f.CI = "pending" }, []Event{ready}, Wait, "CI to finish"},
		{"CI finished, failed", "pr_ready", func(s *EventSettings) { s.WaitCI = "finished" }, func(f *Facts) { f.CI = "failed" }, []Event{ready}, Fire, ""},
		{"CI must pass, failed", "pr_ready", func(s *EventSettings) { s.WaitCI = "passed" }, func(f *Facts) { f.CI = "failed" }, []Event{ready}, Wait, "CI failed"},
		{"CI must pass, pending", "pr_ready", func(s *EventSettings) { s.WaitCI = "passed" }, func(f *Facts) { f.CI = "pending" }, []Event{ready}, Wait, ""},
		{"CI must pass, passed", "pr_ready", func(s *EventSettings) { s.WaitCI = "passed" }, func(f *Facts) { f.CI = "passed" }, []Event{ready}, Fire, ""},
		{"no CI to wait for", "pr_ready", func(s *EventSettings) { s.WaitCI = "passed" }, func(f *Facts) { f.CI = "none" }, []Event{ready}, Fire, ""},
		{"CI ignored when not waiting", "pr_ready", nil, func(f *Facts) { f.CI = "pending" }, []Event{ready}, Fire, ""},
		{"a skip beats waiting for CI", "pr_ready", func(s *EventSettings) { s.WaitCI, s.Authors = "finished", "skip_bots" },
			func(f *Facts) { f.CI, f.Author = "pending", "x[bot]" }, []Event{ready}, Skip, "bot"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := facts(c.evs...)
			if c.facts != nil {
				c.facts(&f)
			}
			got := Decide(settings(c.typ, c.set), c.typ, f)
			if got.Outcome != c.want || !strings.Contains(got.Why, c.why) {
				t.Fatalf("got %v %q, want %v containing %q", got.Outcome, got.Why, c.want, c.why)
			}
		})
	}
}

// The cheap first look the poller takes ignores who wrote a summons, and never refuses.
func TestTriggerWithoutTrust(t *testing.T) {
	s := settings("pr_ready", func(s *EventSettings) { s.Mode = "summoned" })
	e := []Event{ev(EvComments, "mallory", "@autobot review", false)}
	if got := Trigger(s, "pr_ready", e, t0, false, true); got.Outcome != Fire {
		t.Fatalf("%v", got)
	}
	if got := Trigger(s, "pr_ready", e, t0, true, true); got.Outcome != Skip {
		t.Fatalf("%v", got)
	}
}
