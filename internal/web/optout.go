package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// The opt-out tag is one setting of the workspace (Derek, 2026-11-23): what the author of a pull
// request or issue, or a maintainer, writes in the description or a comment to keep Autobot off it.
// Every event block uses it; the stop tag some blocks saved of their own is ignored.

// optOut is the workspace's opt-out tag, "" for none. A workspace that never had one takes, the first
// time it is asked, the stop tag its event blocks used: the most common one among them.
func (s *Server) optOut(ws *store.Workspace) string {
	tag, err := s.st.OptOutTag(ws.ID)
	if errors.Is(err, store.ErrNotFound) {
		tag = s.blocksStopTag(ws)
		if err := s.st.SetOptOutTag(ws, tag, nil); err != nil {
			return tag
		}
	}
	return tag
}

// blocksStopTag is the stop tag most event blocks of the workspace's workflows have saved, or "".
func (s *Server) blocksStopTag(ws *store.Workspace) string {
	counts := map[string]int{}
	for _, kind := range []string{workflow.PR, workflow.Issue} {
		stored, err := s.st.Workflow(ws.ID, kind)
		if err != nil {
			continue
		}
		var g workflow.Graph
		if json.Unmarshal([]byte(stored.Graph), &g) != nil {
			continue
		}
		for _, n := range g.Nodes {
			if t, _ := workflow.TypeOf(n.Type); t.Event {
				if ev, err := workflow.EventSettingsOf(n); err == nil && strings.TrimSpace(ev.StopTag) != "" {
					counts[strings.TrimSpace(ev.StopTag)]++
				}
			}
		}
	}
	best := ""
	for _, tag := range slices.Sorted(maps.Keys(counts)) {
		if counts[tag] > counts[best] {
			best = tag
		}
	}
	return best
}

// eventSettings is an event block's settings with the workspace's opt-out tag.
func (s *Server) eventSettings(ws *store.Workspace, n workflow.Node) (workflow.EventSettings, error) {
	ev, err := workflow.EventSettingsOf(n)
	return ev.WithOptOut(s.optOut(ws)), err
}

// saveOptOut sets the workspace's opt-out tag. It may not match the summon phrase of an event block,
// which would make every summons an opt-out.
func (s *Server) saveOptOut(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	tag := strings.TrimSpace(r.PostForm.Get("opt_out"))
	fail := func(msg string) {
		s.settingsPage(w, r, http.StatusBadRequest, ws, map[string]any{"OptOutError": msg, "OptOutTyped": tag})
	}
	if len(tag) > 100 || strings.ContainsAny(tag, "\n\r") {
		fail("The opt-out tag is one line of up to 100 characters.")
		return
	}
	if tag != "" {
		for _, kind := range workflow.Kinds {
			stored, err := s.st.Workflow(ws.ID, kind)
			if err != nil {
				s.fail(w, err)
				return
			}
			g, err := loadGraph(stored)
			if err != nil {
				s.fail(w, err)
				return
			}
			for _, n := range g.Nodes {
				if t, _ := workflow.TypeOf(n.Type); !t.Event {
					continue
				}
				if ev, err := workflow.EventSettingsOf(n); err == nil && workflow.Stops(ev.SummonPhrase, tag) {
					fail(fmt.Sprintf("The tag matches the summon phrase “%s” of %s, so every summons would opt out.", ev.SummonPhrase, workflow.DisplayName(n)))
					return
				}
			}
		}
	}
	if err := s.st.SetOptOutTag(ws, tag, me(r)); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, ws.Path()+"/settings?saved=1#opt-out", http.StatusSeeOther)
}

// stopRule says whose comments can opt an item out in a manual run (the poller asks the same of its
// own facts): the item's author, and anyone with write access to the repository; never Autobot's own.
func (s *Server) stopRule(ctx context.Context, ws *store.Workspace, token string, it *item) func(author string) bool {
	login := ""
	if id, _ := s.identity(ws); id != nil {
		login = id.Login
	}
	return func(author string) bool {
		switch {
		case login != "" && strings.EqualFold(author, login):
			return false
		case strings.EqualFold(author, it.author):
			return true
		}
		role, err := s.gh.Role(ctx, token, ws.Name, author)
		return err == nil && writes(role)
	}
}

// optedOutBy reports whether the item is currently opted out by the workspace's opt-out tag in its
// description or in a comment from its author or a collaborator with write access, returning a
// short reason ("#autobot:ignore in description" or "#autobot:ignore by alice"), or "" if not.
// Comments are walked chronologically: an explicit summon phrase (from an allowed user) after an
// opt-out clears it ("explicit call always wins"), while a later opt-out re-engages it.
func (s *Server) optedOutBy(ctx context.Context, ws *store.Workspace, g workflow.Graph, eventNode string, it *item) string {
	tag := s.optOut(ws)
	if tag == "" || it == nil {
		return ""
	}
	// Collect the summon phrase(s) for this workflow's event node (falling back to any event node in
	// the graph when the run came from a backlog block, which has no summon phrase of its own).
	type summonRule struct {
		phrase      string
		trustedOnly bool
	}
	var rules []summonRule
	for _, n := range g.Nodes {
		if n.ID == eventNode {
			if ev, err := workflow.EventSettingsOf(n); err == nil && strings.TrimSpace(ev.SummonPhrase) != "" {
				rules = append(rules, summonRule{ev.SummonPhrase, ev.TrustedOnly})
			}
			break
		}
	}
	if len(rules) == 0 {
		for _, n := range g.Nodes {
			if t, _ := workflow.TypeOf(n.Type); t.Event {
				if ev, err := workflow.EventSettingsOf(n); err == nil && strings.TrimSpace(ev.SummonPhrase) != "" {
					rules = append(rules, summonRule{ev.SummonPhrase, ev.TrustedOnly})
				}
			}
		}
	}
	by := ""
	if workflow.Stops(it.body, tag) {
		by = tag + " in description"
	}
	token, _ := s.token(ws)
	counts := s.stopRule(ctx, ws, token, it)
	for _, c := range it.comments {
		if workflow.Stops(c.Body, tag) && counts(c.Author) {
			by = tag + " by " + c.Author
			continue
		}
		for _, r := range rules {
			if workflow.Summons(c.Body, r.phrase, tag) && (!r.trustedOnly || counts(c.Author)) {
				by = ""
				break
			}
		}
	}
	return by
}
