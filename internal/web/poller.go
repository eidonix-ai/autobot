package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// The poller starts workflows by itself. There are no webhooks: every minute it asks GitHub what
// changed in each workspace that has a live workflow, writes what happened down as events on the
// pull request or issue (opened, ready, a comment, a push...), and lets each event node of the
// workflow decide, with workflow.Decide, whether its events call for a run. Each node keeps its own
// place among an item's events, so everything starts a run at most once, and a restart loses nothing.
//
// When a workflow is switched live (or an event node is added to a live one) everything open is
// marked as already seen, so only what happens afterwards is acted on.

const (
	pollEvery        = time.Minute
	maxStarts        = 3 // runs one workspace may start in one round
	maxScanPages     = 5
	maxBasePages     = 20 // 1,000 open items
	maxCIChecks      = 30 // pull requests whose checks are watched in one round
	maxEventText     = 4000
	maxStartTries    = 3
	trustCacheFor    = 10 * time.Minute
	keepEventsFor    = 14 * 24 * time.Hour
	firstBackoff     = 2 * time.Minute
	longestBackoff   = 30 * time.Minute
	noChecksAfter    = 10 * time.Minute // a pull request that still has no checks then is not waiting for any
	scanPullsKey     = "pulls_updated"
	scanIssuesKey    = "issues_updated"
	scanCommentID    = "comment_id"
	scanCommentSince = "comment_since"
)

// poller is what the poller keeps in memory: nothing that matters across a restart.
type poller struct {
	now     func() time.Time
	eastern *time.Location

	mu      sync.Mutex
	roles   map[string]roleSeen // a user's role on a repository, for a while
	trouble map[int64]trouble   // workspaces GitHub is failing for
	notes   map[string]string   // why switching a workflow live did not work, by workspace and kind
	tries   map[string]int      // failed attempts to start a run, by workspace, node and item
}

type roleSeen struct {
	role string
	at   time.Time
}

type trouble struct {
	msg   string
	until time.Time // no new attempt before this
	wait  time.Duration
}

func newPoller() *poller {
	et, err := time.LoadLocation("America/New_York")
	if err != nil {
		et = time.UTC
	}
	return &poller{now: time.Now, eastern: et, roles: map[string]roleSeen{}, trouble: map[int64]trouble{}, notes: map[string]string{}, tries: map[string]int{}}
}

// fail notes that GitHub failed for a workspace and backs off: 2 minutes, doubling to 30.
func (p *poller) fail(ws int64, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.trouble[ws]
	t.wait = min(max(t.wait*2, firstBackoff), longestBackoff)
	t.until, t.msg = p.now().Add(t.wait), err.Error()
	p.trouble[ws] = t
}

func (p *poller) ok(ws int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.trouble, ws)
}

// backedOff reports whether a workspace is waiting out an earlier failure.
func (p *poller) backedOff(ws int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.trouble[ws]
	return ok && p.now().Before(t.until)
}

func (p *poller) note(ws int64, kind, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notes[fmt.Sprint(ws, kind)] = msg
}

// problem is what the canvas says is wrong with a workspace's polling, or "".
func (p *poller) problem(ws int64, kind string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := p.notes[fmt.Sprint(ws, kind)]; m != "" {
		return m
	}
	if t, ok := p.trouble[ws]; ok {
		return fmt.Sprintf("GitHub did not answer: %s. Trying again after %s.", t.msg, t.until.In(p.eastern).Format("3:04 PM")+" ET")
	}
	return ""
}

// Poll runs a round every minute until ctx ends.
func (s *Server) Poll(ctx context.Context) {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pollRound(ctx)
		}
	}
}

// pollRound polls every workspace that has a live workflow.
func (s *Server) pollRound(ctx context.Context) {
	live, err := s.st.LiveWorkflows()
	if err != nil {
		log.Printf("poller: %v", err)
		return
	}
	byWS := map[int64][]store.LiveWorkflow{}
	var order []int64
	for _, w := range live {
		if _, ok := byWS[w.WorkspaceID]; !ok {
			order = append(order, w.WorkspaceID)
		}
		byWS[w.WorkspaceID] = append(byWS[w.WorkspaceID], w)
	}
	for _, id := range order {
		if ctx.Err() != nil || s.poll.backedOff(id) {
			continue
		}
		ws, err := s.st.WorkspaceByID(id)
		if err != nil {
			log.Printf("poller: workspace %d: %v", id, err)
			continue
		}
		func() {
			defer survive("poller for "+ws.Name, nil)
			cx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			if err := s.pollWorkspace(cx, ws, byWS[id]); err != nil {
				log.Printf("poller: %s: %v", ws.Name, err)
				s.poll.fail(id, err)
			} else {
				s.poll.ok(id)
			}
		}()
	}
	s.st.PruneEvents(s.poll.now().Add(-keepEventsFor))
}

// pollWS is one workspace's round.
type pollWS struct {
	s      *Server
	ws     *store.Workspace
	token  string
	login  string // Autobot's GitHub user here; what it says itself is never news
	now    time.Time
	starts int
	branch string // the repository's default branch, once asked
	wants  struct{ reviews, checks bool }
	nodes  map[string][]workflow.Node // each live kind's event nodes
}

func (s *Server) pollWorkspace(ctx context.Context, ws *store.Workspace, wfs []store.LiveWorkflow) error {
	token, err := s.token(ws)
	if err != nil {
		return err
	}
	pw := &pollWS{s: s, ws: ws, token: token, now: s.poll.now(), nodes: map[string][]workflow.Node{}}
	if id, _ := s.identity(ws); id != nil {
		pw.login = id.Login
	}
	for _, w := range wfs {
		var g workflow.Graph
		if err := json.Unmarshal([]byte(w.Graph), &g); err != nil {
			return err
		}
		for _, n := range g.Nodes {
			t, _ := workflow.TypeOf(n.Type)
			if !t.Event {
				continue
			}
			pw.nodes[w.Kind] = append(pw.nodes[w.Kind], n)
			ev, _ := workflow.EventSettingsOf(n)
			if n.Type == "pr_activity" {
				pw.wants.reviews = pw.wants.reviews || ev.HasActivity("reviews")
				pw.wants.checks = pw.wants.checks || ev.HasActivity("checks")
			}
		}
	}
	// A kind that was never baselined (its position is lost) starts over from now, acting on nothing.
	for kind := range pw.nodes {
		key := scanPullsKey
		if kind == workflow.Issue {
			key = scanIssuesKey
		}
		if v, err := s.st.PollScan(ws.ID, key); err != nil {
			return err
		} else if v == "" {
			if err := s.pollBaseline(ctx, ws, kind, pw.nodes[kind]); err != nil {
				return err
			}
		}
	}
	if pw.nodes[workflow.PR] != nil {
		if err := pw.scanItems(ctx, workflow.PR); err != nil {
			return err
		}
	}
	if pw.nodes[workflow.Issue] != nil {
		if err := pw.scanItems(ctx, workflow.Issue); err != nil {
			return err
		}
	}
	if err := pw.scanComments(ctx); err != nil {
		return err
	}
	if pw.wants.checks {
		if err := pw.watchChecks(ctx); err != nil {
			return err
		}
	}
	// A node added to a live workflow starts from here, so it does not run on what was already there.
	for kind, nodes := range pw.nodes {
		known, err := s.st.PollNodes(ws.ID, kind)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if !known[n.ID] {
				if err := s.st.BaselineNode(ws.ID, kind, n.ID); err != nil {
					return err
				}
			}
		}
	}
	for _, kind := range workflow.Kinds {
		for _, n := range pw.nodes[kind] {
			if err := pw.decideNode(ctx, kind, n); err != nil {
				return err
			}
		}
	}
	return nil
}

// pollBaseline marks everything open as seen, and sets the event nodes (all of a kind's, or those
// given) to act only on what happens from now on.
func (s *Server) pollBaseline(ctx context.Context, ws *store.Workspace, kind string, nodes []workflow.Node) error {
	token, err := s.token(ws)
	if err != nil {
		return err
	}
	pw := &pollWS{s: s, ws: ws, token: token, now: s.poll.now()}
	if id, _ := s.identity(ws); id != nil {
		pw.login = id.Login
	}
	var newest time.Time
	for page := 1; page <= maxBasePages; page++ {
		items, err := pw.list(ctx, kind, "open", time.Time{}, page)
		if err != nil {
			return err
		}
		for _, l := range items {
			if l.Kind != kind || !l.Open {
				continue
			}
			it := pw.itemOf(l)
			it.CIDone = true // checks already running are not news
			if err := s.st.SavePollItem(ws.ID, it); err != nil {
				return err
			}
			if l.Updated.After(newest) {
				newest = l.Updated
			}
		}
		if len(items) < github.PageSize {
			break
		}
	}
	if newest.IsZero() {
		newest = pw.now.Add(-5 * time.Minute)
	}
	key := scanPullsKey
	if kind == workflow.Issue {
		key = scanIssuesKey
	}
	if err := s.st.SetPollScan(ws.ID, key, newest.UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if err := pw.resetComments(ctx); err != nil {
		return err
	}
	for _, n := range nodes {
		if err := s.st.BaselineNode(ws.ID, kind, n.ID); err != nil {
			return err
		}
	}
	return nil
}

// resetComments puts the comment scan at the newest comment, so older ones are not news.
func (pw *pollWS) resetComments(ctx context.Context) error {
	c, err := pw.s.gh.LatestComment(ctx, pw.token, pw.ws.Name)
	if err != nil {
		return err
	}
	id, since := "0", pw.now.Add(-5*time.Minute)
	if c != nil {
		id, since = fmt.Sprint(c.ID), c.Updated
	}
	if err := pw.s.st.SetPollScan(pw.ws.ID, scanCommentID, id); err != nil {
		return err
	}
	return pw.s.st.SetPollScan(pw.ws.ID, scanCommentSince, since.UTC().Format(time.RFC3339))
}

func (pw *pollWS) list(ctx context.Context, kind, state string, since time.Time, page int) ([]github.Listed, error) {
	if kind == workflow.PR {
		return pw.s.gh.ListPulls(ctx, pw.token, pw.ws.Name, state, page)
	}
	return pw.s.gh.ListIssues(ctx, pw.token, pw.ws.Name, state, since, page)
}

func digest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// itemOf is the state of a listed item, as kept between rounds.
func (pw *pollWS) itemOf(l github.Listed) store.PollItem {
	labels := slices.Clone(l.Labels)
	slices.Sort(labels)
	it := store.PollItem{Number: l.Number, Kind: l.Kind, Open: l.Open, Author: l.Author, Draft: l.Draft, HeadSHA: l.HeadSHA,
		EditSig: digest(l.Title, l.Body), LabelSig: digest(labels...), Updated: l.Updated, CIDone: true}
	for _, r := range l.Reviewers {
		it.ReviewRequested = it.ReviewRequested || (pw.login != "" && strings.EqualFold(r, pw.login))
	}
	return it
}

// scanItems reads the pull requests or issues updated since the last round and records what
// happened to each.
func (pw *pollWS) scanItems(ctx context.Context, kind string) error {
	key := scanPullsKey
	if kind == workflow.Issue {
		key = scanIssuesKey
	}
	v, err := pw.s.st.PollScan(pw.ws.ID, key)
	if err != nil {
		return err
	}
	since, _ := time.Parse(time.RFC3339, v)
	newest := since
	for page := 1; page <= maxScanPages; page++ {
		items, err := pw.list(ctx, kind, "all", since, page)
		if err != nil {
			return err
		}
		caughtUp := len(items) < github.PageSize
		for _, l := range items {
			if l.Updated.Before(since) {
				caughtUp = true
				break
			}
			if l.Kind != kind {
				continue
			}
			if err := pw.see(ctx, l); err != nil {
				return err
			}
			if l.Updated.After(newest) {
				newest = l.Updated
			}
		}
		if caughtUp {
			break
		}
	}
	if newest.After(since) {
		return pw.s.st.SetPollScan(pw.ws.ID, key, newest.UTC().Format(time.RFC3339))
	}
	return nil
}

// see compares a listed item with what was seen before and records what is new.
func (pw *pollWS) see(ctx context.Context, l github.Listed) error {
	st := pw.s.st
	it := pw.itemOf(l)
	prev, err := st.PollItem(pw.ws.ID, l.Number)
	if errors.Is(err, store.ErrNotFound) {
		it.CIDone = !pw.wants.checks
		if err := st.SavePollItem(pw.ws.ID, it); err != nil || !l.Open {
			return err
		}
		switch {
		case l.Kind == workflow.Issue:
			return pw.event(l.Number, workflow.EvOpened, l.Author, "")
		case l.Draft:
			return pw.event(l.Number, workflow.EvDraft, l.Author, "")
		}
		return pw.event(l.Number, workflow.EvReady, l.Author, "")
	}
	if err != nil {
		return err
	}
	it.CIDone, it.LastEvent = prev.CIDone, prev.LastEvent
	if l.HeadSHA != prev.HeadSHA {
		it.CIDone = !pw.wants.checks
	}
	if err := st.SavePollItem(pw.ws.ID, it); err != nil || !l.Open || !prev.Open || l.Kind != workflow.PR {
		return err
	}
	var news []string // kinds of event
	if prev.Draft && !l.Draft {
		news = append(news, workflow.EvReady)
	}
	if it.ReviewRequested && !prev.ReviewRequested {
		news = append(news, workflow.EvReviewRequested)
	}
	if it.HeadSHA != prev.HeadSHA {
		news = append(news, "pushes")
	}
	if it.EditSig != prev.EditSig {
		news = append(news, "edits")
	}
	if it.LabelSig != prev.LabelSig {
		news = append(news, "labels")
	}
	for _, kind := range news {
		if err := pw.event(l.Number, kind, "", ""); err != nil {
			return err
		}
	}
	if pw.wants.reviews && l.Updated.After(prev.Updated) {
		who, err := pw.newReview(ctx, l.Number, prev.Updated)
		if err != nil || who == "" {
			return err
		}
		return pw.event(l.Number, "reviews", who, "")
	}
	return nil
}

// newReview names the author of a review submitted after a time, other than Autobot's own, or "".
func (pw *pollWS) newReview(ctx context.Context, number int, after time.Time) (string, error) {
	rs, err := pw.s.gh.Reviews(ctx, pw.token, pw.ws.Name, number)
	if err != nil {
		return "", err
	}
	for _, r := range slices.Backward(rs) {
		if r.Submitted.After(after) && !strings.EqualFold(r.Author, pw.login) {
			return r.Author, nil
		}
	}
	return "", nil
}

func (pw *pollWS) event(number int, kind, who, text string) error {
	if len(text) > maxEventText {
		text = strings.ToValidUTF8(text[:maxEventText], "")
	}
	return pw.s.st.AddPollEvent(pw.ws.ID, number, kind, who, text, pw.now)
}

// scanComments records the new comments, from the whole repository in one call. Autobot's own
// are skipped: what it posts must not start the next run.
func (pw *pollWS) scanComments(ctx context.Context) error {
	st := pw.s.st
	idText, err := st.PollScan(pw.ws.ID, scanCommentID)
	if err != nil {
		return err
	}
	sinceText, err := st.PollScan(pw.ws.ID, scanCommentSince)
	if err != nil {
		return err
	}
	var last int64
	fmt.Sscan(idText, &last)
	since, _ := time.Parse(time.RFC3339, sinceText)
	maxID, newest := last, since
	for page := 1; page <= 3; page++ {
		cs, err := pw.s.gh.RepoComments(ctx, pw.token, pw.ws.Name, since, page)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if c.Updated.After(newest) {
				newest = c.Updated
			}
			if c.ID <= last {
				continue
			}
			maxID = max(maxID, c.ID)
			it, err := st.PollItem(pw.ws.ID, c.Number)
			if errors.Is(err, store.ErrNotFound) || (err == nil && !it.Open) || strings.EqualFold(c.Author, pw.login) {
				continue
			}
			if err != nil {
				return err
			}
			if err := pw.event(c.Number, workflow.EvComments, c.Author, c.Body); err != nil {
				return err
			}
		}
		if len(cs) < 100 {
			break
		}
	}
	if maxID == last && !newest.After(since) {
		return nil
	}
	if err := st.SetPollScan(pw.ws.ID, scanCommentID, fmt.Sprint(maxID)); err != nil {
		return err
	}
	return st.SetPollScan(pw.ws.ID, scanCommentSince, newest.UTC().Format(time.RFC3339))
}

// watchChecks looks at the pull requests whose checks have not finished, and records the finish.
func (pw *pollWS) watchChecks(ctx context.Context) error {
	items, err := pw.s.st.PollCIPending(pw.ws.ID, maxCIChecks)
	if err != nil {
		return err
	}
	for _, it := range items {
		state, err := pw.s.gh.CIState(ctx, pw.token, pw.ws.Name, it.HeadSHA)
		if err != nil {
			return err
		}
		if state == "pending" || (state == "none" && pw.now.Sub(it.Updated) < noChecksAfter) {
			continue
		}
		it.CIDone = true
		if err := pw.s.st.SavePollItem(pw.ws.ID, it); err != nil {
			return err
		}
		if state != "none" {
			if err := pw.event(it.Number, "checks", "", state); err != nil {
				return err
			}
		}
	}
	return nil
}

// decideNode lets one event node look at each item with events it has not dealt with.
func (pw *pollWS) decideNode(ctx context.Context, kind string, n workflow.Node) error {
	numbers, err := pw.s.st.PollPending(pw.ws.ID, kind, n.ID)
	if err != nil {
		return err
	}
	for _, num := range numbers {
		if err := pw.decideItem(ctx, kind, n, num); err != nil {
			return err
		}
	}
	return nil
}

func (pw *pollWS) decideItem(ctx context.Context, kind string, n workflow.Node, number int) error {
	st := pw.s.st
	ev, _ := workflow.EventSettingsOf(n)
	it, err := st.PollItem(pw.ws.ID, number)
	if err != nil {
		return err
	}
	after, _, err := st.PollCursor(pw.ws.ID, kind, number, n.ID)
	if err != nil {
		return err
	}
	stored, err := st.PollEvents(pw.ws.ID, number, after)
	if err != nil {
		return err
	}
	done := func() error { return st.SetPollCursor(pw.ws.ID, number, n.ID, max(it.LastEvent, after)) }
	if len(stored) == 0 || !it.Open {
		return done()
	}
	events := make([]workflow.Event, len(stored))
	for i, e := range stored {
		events[i] = workflow.Event{ID: e.ID, Kind: e.Kind, Who: e.Who, Text: e.Text, At: e.At}
	}
	// The cheap look first: most events call for nothing, and a quiet period only waits.
	switch workflow.Trigger(ev, n.Type, events, pw.now, false, true).Outcome {
	case workflow.Idle:
		return done()
	case workflow.Wait:
		return nil
	}
	f, err := pw.facts(ctx, kind, n, ev, it, events)
	if errors.Is(err, github.ErrNoRepo) {
		pw.log(kind, number, n.ID, "Skipped: GitHub no longer shows it", 0)
		return done()
	}
	if err != nil {
		return err
	}
	res := workflow.Decide(ev, n.Type, f)
	switch res.Outcome {
	case workflow.Idle:
		return done()
	case workflow.Skip:
		pw.log(kind, number, n.ID, "Skipped: "+res.Why, 0)
		return done()
	case workflow.Fire:
		return pw.start(ctx, kind, n, number, res.Why, done)
	}
	return nil
}

// start starts a run, unless this node is still running on the item or the round's limit is used up;
// those wait for the next round with nothing lost.
func (pw *pollWS) start(ctx context.Context, kind string, n workflow.Node, number int, why string, done func() error) error {
	running, err := pw.s.st.WorkflowRunning(pw.ws.ID, kind, n.ID, number)
	if err != nil || running || pw.starts >= maxStarts {
		return err
	}
	id, err := pw.s.startWorkflow(ctx, pw.ws, kind, n.ID, number, false, nil)
	if err != nil {
		key := fmt.Sprint(pw.ws.ID, n.ID, number)
		pw.s.poll.mu.Lock()
		pw.s.poll.tries[key]++
		giveUp := pw.s.poll.tries[key] >= maxStartTries
		if giveUp {
			delete(pw.s.poll.tries, key)
		}
		pw.s.poll.mu.Unlock()
		if giveUp {
			pw.log(kind, number, n.ID, "Could not start: "+err.Error(), 0)
			return done()
		}
		return nil
	}
	pw.starts++
	pw.log(kind, number, n.ID, "Started: "+why, id)
	return done()
}

func (pw *pollWS) log(kind string, number int, node, text string, run int64) {
	e := store.PollLogEntry{Kind: kind, Number: number, Node: node, Text: text, Run: run, At: pw.now}
	if err := pw.s.st.AddPollLog(pw.ws.ID, e); err != nil {
		log.Printf("poller: %v", err)
	}
}

// facts gathers what workflow.Decide needs to know, asking GitHub only for what the node's
// settings make a difference to.
func (pw *pollWS) facts(ctx context.Context, kind string, n workflow.Node, ev workflow.EventSettings, it *store.PollItem, events []workflow.Event) (workflow.Facts, error) {
	g, name := pw.s.gh, pw.ws.Name
	f := workflow.Facts{Now: pw.now, Open: true, Events: events, Author: it.Author, ReviewRequested: it.ReviewRequested}
	if kind == workflow.PR {
		pr, err := g.PullRequest(ctx, pw.token, name, it.Number)
		if err != nil {
			return f, err
		}
		f.Open, f.Draft, f.Body, f.Author, f.Base, f.Lines = pr.State == "open", pr.Draft, pr.Body, pr.Author, pr.Base, pr.Additions+pr.Deletions
		if ev.WaitCI != "no" && ev.WaitCI != "" {
			if f.CI, err = g.CIState(ctx, pw.token, name, pr.HeadSHA); err != nil {
				return f, err
			}
		}
		if strings.TrimSpace(ev.Branches) == "" {
			if f.DefaultBranch, err = pw.defaultBranch(ctx); err != nil {
				return f, err
			}
		}
		if ev.SkipMaintain {
			role, err := pw.role(ctx, pr.Author)
			if err != nil {
				return f, err
			}
			f.MaintainerKnown, f.AuthorMaintains = true, writes(role)
		}
		if n.Type == "pr_activity" && ev.CaseExists {
			_, err := pw.s.st.CaseByNumber(pw.ws.ID, it.Number)
			f.HasCase = err == nil
		}
	} else {
		gi, err := g.Item(ctx, pw.token, name, it.Number)
		if err != nil {
			return f, err
		}
		f.Open, f.Body, f.Author = gi.State == "open", gi.Body, gi.Author
	}
	comments, err := g.Comments(ctx, pw.token, name, it.Number)
	if err != nil {
		return f, err
	}
	// Only a comment that says the summon phrase or the stop tag needs to be known as trusted.
	trusted := func(who, text string) (bool, error) {
		if !ev.TrustedOnly || who == f.Author {
			return true, nil
		}
		if !workflow.Stops(text, ev.StopTag) && !workflow.Summons(text, ev.SummonPhrase, ev.StopTag) {
			return false, nil
		}
		role, err := pw.role(ctx, who)
		return writes(role), err
	}
	for _, c := range comments {
		t, err := trusted(c.Author, c.Body)
		if err != nil {
			return f, err
		}
		f.Comments = append(f.Comments, workflow.Comment{Author: c.Author, Body: c.Body, Trusted: t})
	}
	for i, e := range f.Events {
		if e.Kind == workflow.EvComments {
			if f.Events[i].Trusted, err = trusted(e.Who, e.Text); err != nil {
				return f, err
			}
		}
	}
	return f, nil
}

func writes(role string) bool { return role == "write" || role == "maintain" || role == "admin" }

func (pw *pollWS) defaultBranch(ctx context.Context) (string, error) {
	if pw.branch == "" {
		b, err := pw.s.gh.DefaultBranch(ctx, pw.token, pw.ws.Name)
		if err != nil {
			return "", err
		}
		pw.branch = b
	}
	return pw.branch, nil
}

// role is a user's role on the repository, remembered for ten minutes.
func (pw *pollWS) role(ctx context.Context, user string) (string, error) {
	p, key := pw.s.poll, strings.ToLower(pw.ws.Name+"/"+user)
	p.mu.Lock()
	r, ok := p.roles[key]
	p.mu.Unlock()
	if ok && pw.now.Sub(r.at) < trustCacheFor {
		return r.role, nil
	}
	role, err := pw.s.gh.Role(ctx, pw.token, pw.ws.Name, user)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.roles[key] = roleSeen{role, pw.now}
	p.mu.Unlock()
	return role, nil
}
