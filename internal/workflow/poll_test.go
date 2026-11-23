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
	s := DefaultEvent(typ)
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
		{"stop tag from anyone when trust is off", "pr_ready", func(s *EventSettings) { s.TrustedOnly = false }, func(f *Facts) { f.Comments = []Comment{{"mallory", "Autobot: ignore", false}} }, []Event{ready}, Skip, "mallory"},
		{"stop tag beats a summons", "pr_ready", func(s *EventSettings) { s.Mode = "summoned" }, func(f *Facts) { f.Comments = []Comment{{"carol", "Autobot: ignore", true}} }, []Event{summon(true)}, Skip, "stop tag"},

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
