package web

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// The workspace's opt-out tag stops every event block, in the poller; the block's own old stop tag is
// ignored once the workspace has one; it counts in the description and in a comment by the item's
// author or a maintainer, and in no one else's, nor in Autobot's own.
func TestOptOutTagStopsEveryEventBlock(t *testing.T) {
	for _, typ := range []string{"pr_ready", "pr_activity"} {
		t.Run(typ, func(t *testing.T) {
			p := newPollEnv(t)
			p.st.SetOptOutTag(p.ws, "Autobot: ignore", p.admin)
			p.saveGraph(typ, func(s *workflow.EventSettings) {
				s.StopTag = "Old: stop" // a block's own, from before: ignored
				s.Activity, s.QuietMinutes, s.Mode = []string{"comments"}, 0, "always"
			})
			p.goLive(workflow.PR)
			p.gh.roles["carol"] = "maintain"
			p.gh.roles["dan"] = "read"
			p.gh.roles["flare-autobot"] = "write" // Autobot's own account can write: its comments still do not count
			p.round()
			for n, c := range []struct{ who, text string }{
				{"alice", "Autobot: ignore"},          // the author: stops
				{"carol", "autobot: IGNORE please"},   // a maintainer: stops
				{"dan", "Autobot: ignore"},            // read access only: does not
				{"mallory", "Autobot: ignore"},        // a stranger: does not
				{"flare-autobot", "Autobot: ignore"},  // Autobot itself: does not
				{"alice", "Old: stop"},                // the block's old tag: does not
				{"alice", "Autobot: ignore-ish text"}, // the whole phrase, as a prefix of more words: stops, as ever
			} {
				p.addPR(n+1, false)
				p.comment(n+1, c.who, c.text)
			}
			p.round()
			p.round()
			// PR 1, 2 and 7 are stopped; 3, 4, 5 and 6 run: but Autobot's own comment is no activity.
			if typ == "pr_activity" {
				p.wantRuns(3, 4, 6)
			} else {
				p.wantRuns(3, 4, 5, 6)
			}
		})
	}
}

// The tag in the description stops it, and the author's description counts like any text of theirs.
func TestOptOutTagInTheDescription(t *testing.T) {
	p := newPollEnv(t)
	p.st.SetOptOutTag(p.ws, "Autobot: ignore", p.admin)
	p.saveGraph("pr_ready", nil)
	p.goLive(workflow.PR)
	p.addPR(1, false).Body = "Please keep the bot off. Autobot: ignore"
	p.addPR(2, false).Body = "Old: stop"
	p.round()
	p.round()
	p.wantRuns(2)
}

// Autobot's own comment, which tells people how to opt out, neither opts the item out nor summons.
func TestOwnCommentsNeitherOptOutNorSummon(t *testing.T) {
	p := newPollEnv(t)
	p.st.SetOptOutTag(p.ws, "Autobot: ignore", p.admin)
	p.saveGraph("pr_activity", func(s *workflow.EventSettings) {
		s.Mode, s.Activity, s.QuietMinutes, s.TrustedOnly = "summoned", []string{"pushes"}, 0, false
	})
	p.gh.roles["flare-autobot"] = "write" // Autobot's own account can write: its comments still do not count
	p.addPR(1, false)
	p.addPR(2, false)
	p.addPR(3, false)
	p.comment(1, "flare-autobot", "Reviewing. To opt out, include Autobot: ignore. @autobot review")
	p.comment(2, "carol", "@autobot review")
	p.comment(3, "carol", "@autobot review")
	p.comment(3, "flare-autobot", "To opt out, include Autobot: ignore")
	p.goLive(workflow.PR)
	p.round()
	for _, n := range []int{1, 2, 3} {
		p.editPR(n, func(l *github.Listed) { l.HeadSHA = l.HeadSHA + "-2" })
	}
	p.round()
	p.round()
	// 1: only its own comment summoned it. 2: carol summoned it. 3: carol did, and Autobot's own comment
	// with the tag must not stop it.
	p.wantRuns(2, 3)
}

// A workspace that never had an opt-out tag takes the stop tag its event blocks used, the most common
// one; with none, none; and a tag set to empty is not set again.
func TestOptOutTagMigratesFromTheBlocks(t *testing.T) {
	c := newContEnv(t, nil)
	ev := func(id, typ, tag string) workflow.Node {
		return node(id, typ, "", map[string]any{"mode": "always", "summon_phrase": "@autobot", "stop_tag": tag, "trusted_only": true})
	}
	c.saveGraph([]workflow.Node{ev("e1", "pr_ready", "Foo: halt"), ev("e2", "pr_draft", "Bar: halt"), ev("e3", "pr_activity", "Foo: halt")})
	if _, err := c.st.OptOutTag(c.ws.ID); err == nil {
		t.Fatal("the workspace had a tag already")
	}
	if got := c.ui.optOut(c.ws); got != "Foo: halt" {
		t.Fatalf("migrated %q", got)
	}
	// The blocks may change now: the setting is what counts.
	c.saveGraph([]workflow.Node{ev("e1", "pr_ready", "Zed: halt")})
	if got := c.ui.optOut(c.ws); got != "Foo: halt" {
		t.Errorf("after the blocks changed: %q", got)
	}
	// None in any block: none.
	d := newContEnv(t, nil)
	d.saveGraph([]workflow.Node{ev("e1", "pr_ready", "")})
	if got := d.ui.optOut(d.ws); got != "" {
		t.Errorf("no tag: %q", got)
	}
	// Deliberately empty is not migrated again.
	d.saveGraph([]workflow.Node{ev("e1", "pr_ready", "Late: tag")})
	if got := d.ui.optOut(d.ws); got != "" {
		t.Errorf("an empty setting was filled again: %q", got)
	}
}

// {repo:opt_out} is the setting, in a run and in a test; the event block's dialog shows it read-only
// and links to the Settings page; any member sets it there; and it may not be a summon phrase.
func TestOptOutTagTagDialogAndSetting(t *testing.T) {
	c := newContEnv(t, nil)
	c.st.SetOptOutTag(c.ws, "Autobot: ignore", c.admin)
	c.saveGraph(append(append([]workflow.Node{}, events...), caseEntryNode("a", "Say", "To opt out, include {repo:opt_out}.")), [2]string{"ev1", "a"})
	steps := c.run("ev1")
	if steps["a"].Output != "To opt out, include Autobot: ignore." {
		t.Errorf("rendered %q", steps["a"].Output)
	}
	_, body := c.b.get("/w/o/r/workflows/pr/nodes/a")
	if !strings.Contains(body, "<code>{repo:opt_out}</code>") {
		t.Errorf("the Tags list lacks {repo:opt_out}")
	}
	code, _, out := c.b.post("/w/o/r/workflows/pr/nodes/a/test", url.Values{"csrf": {c.csrf}})
	if code != 200 || !strings.Contains(out, "To opt out, include Autobot: ignore.") {
		t.Errorf("the Test box %d:\n%s", code, out)
	}
	// The event block's dialog.
	code, form := c.b.get("/w/o/r/workflows/pr/nodes/ev1")
	if code != 200 || !strings.Contains(form, "Stops on <code>Autobot: ignore</code>") || !strings.Contains(form, `href="/w/o/r/settings#opt-out">(workspace setting)</a>`) || strings.Contains(form, `name="stop_tag"`) {
		t.Errorf("the dialog %d:\n%s", code, form)
	}
	// The card.
	w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
	var g workflow.Graph
	json.Unmarshal([]byte(w.Graph), &g)
	if card := c.ui.summaries(c.ws, g, workflow.PR)["ev1"]; !strings.Contains(card, "stops on “Autobot: ignore”") {
		t.Errorf("the card says %q", card)
	}
	// Changing the setting changes all of it.
	if code, _, _ := c.b.post("/w/o/r/settings/opt-out", url.Values{"csrf": {c.csrf}, "opt_out": {"#autobot:off"}}); code != 303 {
		t.Fatalf("saving: %d", code)
	}
	if got := c.ui.optOut(c.ws); got != "#autobot:off" {
		t.Errorf("saved %q", got)
	}
	if card := c.ui.summaries(c.ws, g, workflow.PR)["ev1"]; !strings.Contains(card, "stops on “#autobot:off”") {
		t.Errorf("the card after the change: %q", card)
	}
	if _, page := c.b.get("/w/o/r/settings"); !strings.Contains(page, `value="#autobot:off"`) {
		t.Errorf("the Settings page does not show it")
	}
	// A tag that would opt out every summons is refused, and so is a summon phrase that is the tag.
	code, _, out = c.b.post("/w/o/r/settings/opt-out", url.Values{"csrf": {c.csrf}, "opt_out": {"@autobot*"}})
	if code != 400 || !strings.Contains(out, "so every summons would opt out") || c.ui.optOut(c.ws) != "#autobot:off" {
		t.Errorf("a tag that matches the summon phrase: %d\n%s", code, out)
	}
	w, _ = c.st.Workflow(c.ws.ID, workflow.PR)
	code, _, out = c.b.post("/w/o/r/workflows/pr/nodes/ev1", url.Values{"csrf": {c.csrf}, "version": {itoa(int64(w.Version))}, "mode": {"always"},
		"summon_phrase": {"#autobot:off now"}, "authors": {"all"}, "over_limit": {"skip"}, "wait_ci": {"no"}})
	if code != 400 || !strings.Contains(out, "so every summons would opt out") {
		t.Errorf("a summon phrase that is the tag: %d\n%s", code, out)
	}
	// No tag at all is allowed: no opt-out.
	if code, _, _ := c.b.post("/w/o/r/settings/opt-out", url.Values{"csrf": {c.csrf}, "opt_out": {""}}); code != 303 || c.ui.optOut(c.ws) != "" {
		t.Errorf("clearing: %d %q", code, c.ui.optOut(c.ws))
	}
}

// A manual run's verdict says the same: the tag in the description, or in a comment by the author or a
// maintainer; not in a stranger's or Autobot's.
func TestVerdictOptOut(t *testing.T) {
	p := newPollEnv(t)
	p.gh.roles["carol"] = "maintain"
	p.gh.roles["flare-autobot"] = "write"
	n := workflow.Node{ID: "ev", Type: "pr_ready"}
	for who, stops := range map[string]bool{"alice": true, "carol": true, "mallory": false, "flare-autobot": false} {
		it := &item{kind: workflow.PR, number: 1, state: "open", author: "alice", pr: &github.PullRequest{Base: "main"},
			comments: []github.Comment{{Author: who, Body: "Autobot: ignore"}}}
		_, lines := verdict(n, it, false, "Autobot: ignore", p.ui.stopRule(t.Context(), p.ws, "tok", it))
		got := strings.Contains(strings.Join(lines, "\n"), "✗ the opt-out tag “Autobot: ignore” is in a comment by "+who)
		if got != stops {
			t.Errorf("%s: stops %v, want %v\n%s", who, got, stops, strings.Join(lines, "\n"))
		}
	}
}
