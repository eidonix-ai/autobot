package workflow

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// This file decides, without asking GitHub anything, whether an event node starts a run on an item.
// The poller writes down what happens on GitHub as Events, gathers the Facts a decision needs, and
// asks Decide. Keeping the decision a plain function of facts is what lets every rule be tested.

// The kinds of thing that happen to an item. The last six are the PR Activity kinds of
// Activities, spelled the same.
const (
	EvOpened          = "opened"           // an issue first seen
	EvReady           = "ready"            // a pull request first seen ready for review, or taken out of draft
	EvDraft           = "draft"            // a pull request first seen as a draft
	EvReviewRequested = "review_requested" // a review requested from Autobot's GitHub user
	EvComments        = "comments"         // a new comment; Text is what it says
)

// Event is one thing that happened to an item.
type Event struct {
	ID      int64
	Kind    string
	Who     string
	Text    string
	At      time.Time
	Trusted bool // Who is the item's author or has write access; set when a decision needs it
}

// Comment is a comment on an item, with whether its author is trusted: the item's author, or someone
// with write access. (Autobot's own comments are never in the list.)
type Comment struct {
	Author  string
	Body    string
	Trusted bool
}

// Facts are what one decision needs to know about one item and one event node.
type Facts struct {
	Now             time.Time
	Open            bool
	Draft           bool    // pull requests
	Events          []Event // what happened since this node last looked at the item
	Body            string  // the item's description; its author is the item's author, so it is trusted
	Comments        []Comment
	Author          string
	Base            string // the branch a pull request merges into
	DefaultBranch   string
	Lines           int  // changed lines of a pull request
	MaintainerKnown bool // AuthorMaintains was looked up
	AuthorMaintains bool
	ReviewRequested bool // a review is requested from Autobot's GitHub user right now
	HasCase         bool
	WasOptedOut     bool // this node previously skipped the item because of the opt-out tag
	CI              string // "pending", "failed", "passed" or "none" (no checks on the head)
}

// Outcome is what a decision comes to.
type Outcome int

const (
	Idle Outcome = iota // nothing to do for this item; not worth a line in the log
	Wait                // looked at again next round: the quiet period or CI is not over
	Skip                // an occurrence that must not start a run; Why says why
	Fire                // start the run
)

// Result is an outcome and, for everything but Idle, the reason.
type Result struct {
	Outcome Outcome
	Why     string
}

// Trigger says whether the events call for a run at all, before any filter is looked at. It is
// also the cheap first test the poller makes before spending GitHub calls on a decision: with
// trust false it ignores who wrote a summons. summonedBefore says the item was summoned earlier.
func Trigger(ev EventSettings, typ string, events []Event, now time.Time, trust, summonedBefore bool) Result {
	var found bool
	var why, refused string
	var summonAt time.Time
	if ev.Mode == "summoned" || (typ == "pr_activity" && ev.QuietMinutes > 0) {
		found, why, refused, summonAt = summons(ev, events, trust)
	}
	if refused != "" && ev.Mode == "summoned" {
		return Result{Skip, refused}
	}
	if typ == "pr_activity" {
		return activityTrigger(ev, events, now, found, why, summonAt, summonedBefore)
	}
	if ev.Mode == "summoned" {
		if found {
			return Result{Fire, why}
		}
		return Result{Idle, ""}
	}
	want := map[string]string{"pr_ready": EvReady, "pr_draft": EvDraft, "issue_opened": EvOpened}[typ]
	for _, e := range events {
		if e.Kind == want {
			return Result{Fire, "it " + map[string]string{EvReady: "is ready for review", EvDraft: "was opened as a draft", EvOpened: "was opened"}[want]}
		}
	}
	// In "always" mode, an item initially skipped (e.g. due to the opt-out tag) can still be
	// triggered later by an explicit summons, or by editing its description to remove the opt-out tag.
	if found, why, _, _ = summons(ev, events, trust); found {
		return Result{Fire, why}
	}
	if ev.StopTag != "" && (typ == "pr_ready" || typ == "issue_opened") {
		for _, e := range events {
			if e.Kind == "edits" {
				return Result{Fire, "its description was edited"}
			}
		}
	}
	return Result{Idle, ""}
}

// summons looks for a summons among the events: a new comment with the summon phrase, or a review
// request. A summons from someone who is not trusted is refused (when trust is checked and the
// event says so) with the reason; it does not count.
func summons(ev EventSettings, events []Event, trust bool) (found bool, why, refused string, at time.Time) {
	for _, e := range events {
		switch {
		case e.Kind == EvReviewRequested && ev.ReviewRequest:
			found, why, at = true, "a review was requested from Autobot", laterOf(at, e.At)
		case e.Kind == EvComments && Summons(e.Text, ev.SummonPhrase, ev.StopTag):
			if trust && ev.TrustedOnly && !e.Trusted {
				refused = fmt.Sprintf("%s summoned it, but is neither the author nor a collaborator with write access", e.Who)
				continue
			}
			found, why, at = true, "summoned by "+e.Who, laterOf(at, e.At)
		}
	}
	if found {
		refused = ""
	}
	return found, why, refused, at
}

// activityTrigger is the PR Activity event: activity of the ticked kinds, once it has been quiet
// long enough. In summoned mode the pull request must have been summoned (now, or earlier), and a
// new summons counts as activity. In always mode, a fresh trusted summons at or after pending
// activity skips the remaining quiet period, while a summons with no pending activity does nothing.
func activityTrigger(ev EventSettings, events []Event, now time.Time, newSummons bool, why string, summonAt time.Time, summonedBefore bool) Result {
	var last time.Time
	var kinds []string
	for _, e := range events {
		isSummonEv := ev.Mode == "summoned" && newSummons && (e.Kind == EvComments || e.Kind == EvReviewRequested)
		if slices.Contains(ev.Activity, e.Kind) || isSummonEv {
			kinds = append(kinds, e.Kind)
			last = laterOf(last, e.At)
		}
	}
	if len(kinds) == 0 {
		return Result{Idle, ""}
	}
	if ev.Mode == "summoned" && !newSummons && !summonedBefore {
		return Result{Idle, ""}
	}
	if ev.Mode == "always" && newSummons && !summonAt.Before(last) {
		return Result{Fire, why}
	}
	if wait := time.Duration(ev.QuietMinutes)*time.Minute - now.Sub(last); wait > 0 {
		return Result{Wait, fmt.Sprintf("waiting for %d quiet minutes after the last activity", ev.QuietMinutes)}
	}
	if newSummons {
		return Result{Fire, why}
	}
	return Result{Fire, "new activity: " + strings.Join(unique(kinds), ", ")}
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func unique(list []string) []string {
	var out []string
	for _, s := range list {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// Decide says what an event node of type typ does about an item.
func Decide(ev EventSettings, typ string, f Facts) Result {
	if !f.Open {
		return Result{Idle, ""}
	}
	if (typ == "pr_ready" && f.Draft) || (typ == "pr_draft" && !f.Draft) {
		return Result{Idle, ""}
	}
	summonedBefore := f.summoned(ev)
	t := Trigger(ev, typ, f.Events, f.Now, true, summonedBefore)
	if t.Outcome != Fire {
		return t
	}
	if typ == "pr_activity" && ev.CaseExists && !f.HasCase {
		return Result{Skip, "it has no case file yet"}
	}
	newSummons, _, _, summonAt := summons(ev, f.Events, true)
	// In "always" mode, pr_ready / pr_draft / issue_opened only re-fire on a later summons when
	// the item has no case file yet, and on a description edit only when this node previously
	// skipped the item for the opt-out tag (so editing an unrelated old PR never triggers a run).
	if typ != "pr_activity" && ev.Mode != "summoned" {
		want := map[string]string{"pr_ready": EvReady, "pr_draft": EvDraft, "issue_opened": EvOpened}[typ]
		hasPrimary := false
		for _, e := range f.Events {
			if e.Kind == want {
				hasPrimary = true
				break
			}
		}
		if !hasPrimary {
			if f.HasCase {
				return Result{Idle, ""}
			}
			if !newSummons && !f.WasOptedOut {
				return Result{Idle, ""}
			}
		}
	}
	explicitCall := newSummons && !stoppedAfter(ev, f.Author, f.Events, summonAt)
	if by := f.stoppedBy(ev); by != "" && !explicitCall {
		return Result{Skip, "the opt-out tag “" + ev.StopTag + "” is in " + by}
	}
	if typ != "issue_opened" {
		for _, c := range PRFilters(ev, f.prFacts(summonedBefore || newSummons)) {
			if !c.Pass {
				return Result{Skip, c.Text}
			}
		}
		if r := f.waitForCI(ev); r.Outcome == Wait {
			return r
		}
	}
	return Result{Fire, t.Why}
}

// summoned reports whether the item has been summoned: a trusted comment with the summon phrase,
// or a review request that is still open.
func (f Facts) summoned(ev EventSettings) bool {
	if f.ReviewRequested && ev.ReviewRequest {
		return true
	}
	for _, c := range f.Comments {
		if Summons(c.Body, ev.SummonPhrase, ev.StopTag) && (c.Trusted || !ev.TrustedOnly) {
			return true
		}
	}
	return false
}

// stoppedBy names where the opt-out tag (ev.StopTag, the workspace's) is, or "". It counts in the
// item's description, which is its author's, and in a comment by the item's author or by someone with
// write access; never in anyone else's, and never in Autobot's own, which are not in the list.
// Comments are evaluated chronologically: a later explicit summons clears an earlier opt-out, and a
// subsequent opt-out re-engages it.
func (f Facts) stoppedBy(ev EventSettings) string {
	by := ""
	if Stops(f.Body, ev.StopTag) {
		by = "its description"
	}
	for _, c := range f.Comments {
		switch {
		case Stops(c.Body, ev.StopTag) && (c.Trusted || strings.EqualFold(c.Author, f.Author)):
			by = "a comment by " + c.Author
		case Summons(c.Body, ev.SummonPhrase, ev.StopTag) && (c.Trusted || !ev.TrustedOnly):
			by = ""
		}
	}
	return by
}

// stoppedAfter reports whether an opt-out comment by the author or a trusted collaborator arrived
// after at in the current poll batch.
func stoppedAfter(ev EventSettings, itemAuthor string, events []Event, at time.Time) bool {
	for _, e := range events {
		if e.Kind == EvComments && e.At.After(at) && Stops(e.Text, ev.StopTag) && (e.Trusted || strings.EqualFold(e.Who, itemAuthor)) {
			return true
		}
	}
	return false
}

func (f Facts) prFacts(summoned bool) PRFacts {
	return PRFacts{Author: f.Author, Base: f.Base, DefaultBranch: f.DefaultBranch, Lines: f.Lines,
		MaintainerKnown: f.MaintainerKnown, AuthorMaintains: f.AuthorMaintains, Summoned: summoned}
}

// waitForCI waits while the head's checks are running, and for "passed" while any has failed.
func (f Facts) waitForCI(ev EventSettings) Result {
	switch {
	case ev.WaitCI == "no" || f.CI == "passed" || f.CI == "none" || f.CI == "":
	case f.CI == "pending":
		return Result{Wait, "waiting for CI to finish"}
	case f.CI == "failed" && ev.WaitCI == "passed":
		return Result{Wait, "CI failed; waiting for it to pass"}
	}
	return Result{Fire, ""}
}

// Check is one condition and how it stood, in words.
type Check struct {
	Pass bool
	Text string
}

// PRFacts are what the pull request filters look at.
type PRFacts struct {
	Author          string
	Base            string
	DefaultBranch   string // "" when not known: then no branch is required unless the node names some
	Lines           int
	MaintainerKnown bool
	AuthorMaintains bool
	Summoned        bool
}

// PRFilters are the conditions on which pull requests an event handles: target branch, author,
// whether the author maintains the repository, and size. The poller and a manual run's verdict
// read them the same way.
func PRFilters(ev EventSettings, f PRFacts) []Check {
	var out []Check
	add := func(pass bool, yes, no string) {
		if pass {
			out = append(out, Check{true, yes})
		} else {
			out = append(out, Check{false, no})
		}
	}
	var want []string
	if b := strings.TrimSpace(ev.Branches); b != "" {
		for _, x := range strings.Split(b, ",") {
			want = append(want, strings.TrimSpace(x))
		}
	} else if f.DefaultBranch != "" {
		want = []string{f.DefaultBranch}
	}
	if len(want) > 0 {
		add(slices.Contains(want, f.Base), "it targets "+f.Base, "it targets "+f.Base+", not "+strings.Join(want, ", "))
	}
	bot := strings.HasSuffix(f.Author, "[bot]")
	switch ev.Authors {
	case "skip_bots":
		add(!bot, "its author is not a bot", "its author "+f.Author+" is a bot")
	case "only_bots":
		add(bot, "its author is a bot", "its author "+f.Author+" is not a bot")
	}
	if ev.SkipMaintain && f.MaintainerKnown {
		add(!f.AuthorMaintains, "its author is not a maintainer", "its author "+f.Author+" maintains the repository")
	}
	if ev.MaxLines > 0 {
		over := f.Lines > ev.MaxLines
		switch {
		case !over:
			add(true, fmt.Sprintf("%d changed lines, within %d", f.Lines, ev.MaxLines), "")
		case ev.OverLimit == "summoned":
			add(f.Summoned, fmt.Sprintf("%d changed lines, over %d, but summoned", f.Lines, ev.MaxLines),
				fmt.Sprintf("%d changed lines, over %d, and not summoned", f.Lines, ev.MaxLines))
		default:
			add(false, "", fmt.Sprintf("%d changed lines, over the limit of %d", f.Lines, ev.MaxLines))
		}
	}
	return out
}
