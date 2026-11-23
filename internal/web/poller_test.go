package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// pollFake is a GitHub with pull requests, issues and comments the tests change between rounds.
type pollFake struct {
	fakeGitHub
	mu       sync.Mutex
	prs      []*github.Listed
	issues   []*github.Listed
	comments []github.RepoComment
	lines    map[int]int
	roles    map[string]string
	ci       map[string]string
	err      error // every poller call fails with it
	calls    int   // poller calls made
}

func (g *pollFake) enter() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return g.err
}

func page[T any](all []T, n int) []T {
	from := min((n-1)*github.PageSize, len(all))
	return all[from:min(from+github.PageSize, len(all))]
}

func (g *pollFake) listed(all []*github.Listed, state string, since time.Time) []github.Listed {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []github.Listed
	for _, l := range all {
		if (state == "open" && !l.Open) || l.Updated.Before(since) {
			continue
		}
		out = append(out, *l)
	}
	slices.SortStableFunc(out, func(a, b github.Listed) int { return b.Updated.Compare(a.Updated) })
	return out
}

func (g *pollFake) ListPulls(_ context.Context, _, _, state string, n int) ([]github.Listed, error) {
	if err := g.enter(); err != nil {
		return nil, err
	}
	return page(g.listed(g.prs, state, time.Time{}), n), nil
}

func (g *pollFake) ListIssues(_ context.Context, _, _, state string, since time.Time, n int) ([]github.Listed, error) {
	if err := g.enter(); err != nil {
		return nil, err
	}
	return page(g.listed(g.issues, state, since), n), nil
}

func (g *pollFake) RepoComments(_ context.Context, _, _ string, since time.Time, n int) ([]github.RepoComment, error) {
	if err := g.enter(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []github.RepoComment
	for _, c := range g.comments {
		if !c.Updated.Before(since) {
			out = append(out, c)
		}
	}
	return page(out, n), nil
}

func (g *pollFake) LatestComment(context.Context, string, string) (*github.RepoComment, error) {
	if err := g.enter(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.comments) == 0 {
		return nil, nil
	}
	c := g.comments[len(g.comments)-1]
	return &c, nil
}

func (g *pollFake) CIState(_ context.Context, _, _, sha string) (string, error) {
	if err := g.enter(); err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if s, ok := g.ci[sha]; ok {
		return s, nil
	}
	return "none", nil
}

func (g *pollFake) Role(_ context.Context, _, _, user string) (string, error) {
	if err := g.enter(); err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if r, ok := g.roles[user]; ok {
		return r, nil
	}
	return "none", nil
}

func (g *pollFake) PullRequest(_ context.Context, _, name string, n int) (*github.PullRequest, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, l := range g.prs {
		if l.Number == n {
			state := "open"
			if !l.Open {
				state = "closed"
			}
			return &github.PullRequest{Number: n, URL: fmt.Sprintf("https://github.com/%s/pull/%d", name, n), Title: l.Title, Body: l.Body,
				Author: l.Author, Draft: l.Draft, State: state, HeadSHA: l.HeadSHA, Base: l.Base, Additions: g.lines[n]}, nil
		}
	}
	return nil, github.ErrNoRepo
}

func (g *pollFake) Item(_ context.Context, _, name string, n int) (*github.Item, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, l := range g.issues {
		if l.Number == n {
			state := "open"
			if !l.Open {
				state = "closed"
			}
			return &github.Item{Number: n, Kind: "issue", Title: l.Title, State: state, Body: l.Body, Author: l.Author,
				URL: fmt.Sprintf("https://github.com/%s/issues/%d", name, n)}, nil
		}
	}
	return nil, github.ErrNoRepo
}

func (g *pollFake) Comments(_ context.Context, _, _ string, n int) ([]github.Comment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []github.Comment
	for _, c := range g.comments {
		if c.Number == n {
			out = append(out, github.Comment{Author: c.Author, Body: c.Body, At: c.Updated})
		}
	}
	return out, nil
}

// pollEnv is a workspace o/r with GitHub identity "autobot-bot", a clock, and the fake GitHub.
type pollEnv struct {
	*env
	gh    *pollFake
	ws    *store.Workspace
	admin *store.User
	clk   time.Time
	held  map[int64]bool // runs a test keeps running on purpose
	b     *browser
	csrf  string
}

func newPollEnv(t *testing.T) *pollEnv {
	gh := &pollFake{lines: map[int]int{}, roles: map[string]string{}, ci: map[string]string{}}
	e := newEnvWith(t, gh)
	admin := e.user("derek", true)
	ws, err := e.st.CreateWorkspace("o/r", admin, "flare-autobot", "tok-good")
	if err != nil {
		t.Fatal(err)
	}
	p := &pollEnv{env: e, gh: gh, ws: ws, admin: admin, clk: time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC), held: map[int64]bool{}}
	e.ui.poll.now = func() time.Time { return p.clk }
	p.b = e.signIn("derek")
	p.csrf = p.b.csrf()
	return p
}

// saveGraph stores the pull request workflow: an event node "ev" of the type, with settings.
func (p *pollEnv) saveGraph(typ string, change func(*workflow.EventSettings)) {
	p.t.Helper()
	s := workflow.DefaultEvent(typ)
	s.Authors, s.OverLimit, s.WaitCI, s.ReviewRequest = "all", "skip", "no", false
	if change != nil {
		change(&s)
	}
	raw, _ := json.Marshal(s)
	kind := workflow.PR
	if typ == "issue_opened" {
		kind = workflow.Issue
	}
	stored, _ := p.st.Workflow(p.ws.ID, kind)
	g := workflow.Graph{Nodes: []workflow.Node{{ID: "ev", Type: typ, Settings: raw}}, Links: []workflow.Link{}}
	b, _ := json.Marshal(g)
	if _, err := p.st.SaveWorkflow(p.ws.ID, kind, stored.Version, string(b), p.admin); err != nil {
		p.t.Fatal(err)
	}
}

// goLive switches a kind's workflow live as derek would and returns the response code.
func (p *pollEnv) goLive(kind string) int {
	p.t.Helper()
	code, _, _ := p.b.post("/w/o/r/workflows/"+kind+"/live", url.Values{"csrf": {p.csrf}, "live": {"on"}})
	return code
}

func (p *pollEnv) mutate() time.Time {
	p.clk = p.clk.Add(time.Second)
	return p.clk
}

func (p *pollEnv) addPR(n int, draft bool) *github.Listed {
	p.gh.mu.Lock()
	defer p.gh.mu.Unlock()
	l := &github.Listed{Number: n, Kind: "pr", Open: true, Title: fmt.Sprintf("PR %d", n), Author: "alice", Draft: draft,
		HeadSHA: fmt.Sprintf("sha%d-1", n), Base: "main", Updated: p.mutate()}
	p.gh.prs = append(p.gh.prs, l)
	return l
}

func (p *pollEnv) addIssue(n int) {
	p.gh.mu.Lock()
	defer p.gh.mu.Unlock()
	p.gh.issues = append(p.gh.issues, &github.Listed{Number: n, Kind: "issue", Open: true, Title: fmt.Sprintf("Issue %d", n), Author: "bob", Updated: p.mutate()})
}

// editPR changes a pull request on GitHub, which bumps its update time.
func (p *pollEnv) editPR(n int, f func(*github.Listed)) {
	p.gh.mu.Lock()
	defer p.gh.mu.Unlock()
	for _, l := range p.gh.prs {
		if l.Number == n {
			f(l)
			l.Updated = p.mutate()
		}
	}
}

func (p *pollEnv) comment(n int, who, text string) {
	p.gh.mu.Lock()
	defer p.gh.mu.Unlock()
	p.gh.comments = append(p.gh.comments, github.RepoComment{ID: int64(len(p.gh.comments) + 1), Number: n, Author: who, Body: text, Updated: p.mutate()})
}

// round runs one poll a minute after the last and waits for the runs it started to end.
func (p *pollEnv) round() {
	p.t.Helper()
	p.clk = p.clk.Add(time.Minute)
	p.ui.pollRound(context.Background())
	p.settle()
}

func (p *pollEnv) settle() {
	p.t.Helper()
	for i := 0; i < 200; i++ {
		busy := false
		for id := int64(1); ; id++ {
			r, err := p.st.WorkflowRun(id)
			if err != nil {
				break
			}
			busy = busy || (r.Status == "running" && !p.held[id])
		}
		if !busy {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.t.Fatal("a run did not end")
}

// runs lists the item of each run started, in order.
func (p *pollEnv) runs() []int {
	var out []int
	for id := int64(1); ; id++ {
		r, err := p.st.WorkflowRun(id)
		if err != nil {
			return out
		}
		out = append(out, r.Item)
	}
}

func (p *pollEnv) wantRuns(items ...int) {
	p.t.Helper()
	if got := p.runs(); !slices.Equal(got, items) && !(len(got) == 0 && len(items) == 0) {
		p.t.Fatalf("runs on items %v, want %v\nlog: %v", got, items, p.logText())
	}
}

func (p *pollEnv) logText() []string {
	var out []string
	for _, k := range workflow.Kinds {
		es, _ := p.st.PollLog(p.ws.ID, k, 50)
		for _, e := range es {
			out = append(out, fmt.Sprintf("#%d %s", e.Number, e.Text))
		}
	}
	return out
}

func TestSwitchingLiveStartsNothingForOldItems(t *testing.T) {
	p := newPollEnv(t)
	p.addPR(1, false)
	p.addPR(2, true)
	p.comment(1, "carol", "an old comment")
	p.saveGraph("pr_ready", nil)
	if code := p.goLive(workflow.PR); code != http.StatusSeeOther {
		t.Fatalf("switch: %d", code)
	}
	if st, _ := p.st.Workflow(p.ws.ID, workflow.PR); !st.Live {
		t.Fatal("not live")
	}
	p.round()
	p.round()
	p.wantRuns()

	// Something new after the switch starts one run, and the next rounds do not start it again.
	p.addPR(3, false)
	p.round()
	p.wantRuns(3)
	p.round()
	p.round()
	p.wantRuns(3)
	if log := p.logText(); len(log) != 1 || !strings.Contains(log[0], "Started") {
		t.Fatalf("log %v", log)
	}
	// Old items changing is not new either: PR 2 is still a draft, PR 1 was ready when switched on.
	p.editPR(1, func(l *github.Listed) { l.Title = "Retitled" })
	p.round()
	p.wantRuns(3)
}

func TestSwitchingLiveRecordsHistory(t *testing.T) {
	p := newPollEnv(t)
	p.saveGraph("pr_ready", nil)
	p.goLive(workflow.PR)
	evs, _ := p.st.Events(p.ws.ID, 10)
	if len(evs) == 0 || !strings.Contains(evs[0].Summary, "pr workflow live") || evs[0].Actor != "derek" {
		t.Fatalf("history %+v", evs)
	}
}

func TestOnlyMembersSwitchLive(t *testing.T) {
	p := newPollEnv(t)
	p.saveGraph("pr_ready", nil)
	p.user("eve", false) // belongs to no workspace
	eve := p.signIn("eve")
	code, _, _ := eve.post("/w/o/r/workflows/pr/live", url.Values{"csrf": {eve.csrf()}, "live": {"on"}})
	if code != http.StatusNotFound {
		t.Fatalf("a stranger switching live: %d", code)
	}
	if st, _ := p.st.Workflow(p.ws.ID, workflow.PR); st.Live {
		t.Fatal("a stranger switched it live")
	}
	// Without the form token, not even a member can.
	if code, _, _ := p.b.post("/w/o/r/workflows/pr/live", url.Values{"live": {"on"}}); code != http.StatusForbidden {
		t.Fatalf("without the token: %d", code)
	}
	if st, _ := p.st.Workflow(p.ws.ID, workflow.PR); st.Live {
		t.Fatal("switched live without a token")
	}
	// A member who is not an admin can.
	m := p.user("mia", false)
	p.st.SetMembers(p.ws.ID, []int64{m.ID}, p.admin)
	mia := p.signIn("mia")
	if code, _, _ := mia.post("/w/o/r/workflows/pr/live", url.Values{"csrf": {mia.csrf()}, "live": {"on"}}); code != http.StatusSeeOther {
		t.Fatalf("a member: %d", code)
	}
	if st, _ := p.st.Workflow(p.ws.ID, workflow.PR); !st.Live {
		t.Fatal("a member could not switch it live")
	}
}

func TestPollerRestartDoesNotRepeat(t *testing.T) {
	p := newPollEnv(t)
	p.saveGraph("pr_ready", nil)
	p.goLive(workflow.PR)
	p.addPR(1, false)
	p.round()
	p.wantRuns(1)
	// A new process over the same database remembers.
	ui2, err := New(p.st, p.gh)
	if err != nil {
		t.Fatal(err)
	}
	ui2.poll.now = p.ui.poll.now
	p.ui = ui2
	p.round()
	p.wantRuns(1)
}

func TestPollerDraftThenReady(t *testing.T) {
	p := newPollEnv(t)
	p.saveGraph("pr_ready", nil)
	p.goLive(workflow.PR)
	p.addPR(4, true)
	p.round()
	p.wantRuns() // a draft is not ready
	p.editPR(4, func(l *github.Listed) { l.Draft = false })
	p.round()
	p.round()
	p.wantRuns(4)
}

func TestPollerDraftEvent(t *testing.T) {
	p := newPollEnv(t)
	p.saveGraph("pr_draft", nil)
	p.goLive(workflow.PR)
	p.addPR(4, false)
	p.addPR(5, true)
	p.round()
	p.wantRuns(5)
}

func TestPollerIssueOpened(t *testing.T) {
	p := newPollEnv(t)
	p.addIssue(1)
	p.saveGraph("issue_opened", func(s *workflow.EventSettings) { s.SummonPhrase = "@autobot" })
	p.goLive(workflow.Issue)
	p.round()
	p.wantRuns()
	p.addIssue(2)
	p.round()
	p.round()
	p.wantRuns(2)
}

func TestPollerSummonedNeedsATrustedPerson(t *testing.T) {
	p := newPollEnv(t)
	p.addPR(1, false)
	p.saveGraph("pr_ready", func(s *workflow.EventSettings) { s.Mode = "summoned" })
	p.goLive(workflow.PR)
	p.round()
	p.wantRuns() // being ready is not a summons
	p.comment(1, "mallory", "@autobot review please")
	p.round()
	p.wantRuns()
	if log := p.logText(); len(log) != 1 || !strings.Contains(log[0], "mallory summoned it") {
		t.Fatalf("log %v", log)
	}
	// Someone with write access summons it; the author of the pull request may too.
	p.gh.roles["carol"] = "write"
	p.comment(1, "carol", "@autobot review please")
	p.round()
	p.round()
	p.wantRuns(1)
	p.addPR(2, false)
	p.round()
	p.comment(2, "alice", "@autobot review")
	p.round()
	p.wantRuns(1, 2)
}

func TestPollerStopTagFromTrustedOnly(t *testing.T) {
	p := newPollEnv(t)
	p.saveGraph("pr_ready", nil)
	p.goLive(workflow.PR)
	p.gh.roles["carol"] = "maintain"
	p.addPR(1, false)
	p.comment(1, "mallory", "Autobot: ignore")
	p.addPR(2, false)
	p.comment(2, "carol", "Autobot: ignore")
	p.round()
	p.wantRuns(1) // a stranger cannot stop it; a maintainer can
}

func TestPollerReviewRequestSummons(t *testing.T) {
	p := newPollEnv(t)
	p.addPR(1, false)
	p.saveGraph("pr_ready", func(s *workflow.EventSettings) { s.Mode, s.ReviewRequest = "summoned", true })
	p.goLive(workflow.PR)
	p.round()
	p.editPR(1, func(l *github.Listed) { l.Reviewers = []string{"someone-else"} })
	p.round()
	p.wantRuns()
	p.editPR(1, func(l *github.Listed) { l.Reviewers = []string{"someone-else", "Flare-Autobot"} })
	p.round()
	p.round()
	p.wantRuns(1)
}

func TestPollerIgnoresItsOwnComments(t *testing.T) {
	p := newPollEnv(t)
	p.addPR(1, false)
	p.saveGraph("pr_activity", func(s *workflow.EventSettings) { s.Activity, s.QuietMinutes = []string{"comments"}, 0 })
	p.goLive(workflow.PR)
	p.round()
	p.comment(1, "flare-autobot", "Here is my review.")
	p.round()
	p.wantRuns()
	p.comment(1, "carol", "Thanks")
	p.round()
	p.wantRuns(1)
	p.comment(1, "flare-autobot", "Another post")
	p.round()
	p.wantRuns(1)
}

func TestPollerQuietPeriod(t *testing.T) {
	p := newPollEnv(t)
	p.addPR(1, false)
	p.saveGraph("pr_activity", func(s *workflow.EventSettings) {
		s.Activity, s.QuietMinutes = []string{"pushes", "labels", "edits"}, 15
	})
	p.goLive(workflow.PR)
	p.round()
	p.editPR(1, func(l *github.Listed) { l.HeadSHA = "sha1-2" })
	for i := 0; i < 10; i++ {
		p.round()
	}
	p.wantRuns() // only ten quiet minutes so far
	for i := 0; i < 6; i++ {
		p.round()
	}
	p.wantRuns(1)
	for i := 0; i < 20; i++ {
		p.round()
	}
	p.wantRuns(1) // once for the push

	// More activity restarts the wait: a label, then a new title ten minutes later.
	p.editPR(1, func(l *github.Listed) { l.Labels = []string{"bug"} })
	for i := 0; i < 10; i++ {
		p.round()
	}
	p.editPR(1, func(l *github.Listed) { l.Title = "Better title" })
	for i := 0; i < 14; i++ {
		p.round()
	}
	p.wantRuns(1)
	for i := 0; i < 3; i++ {
		p.round()
	}
	p.wantRuns(1, 1)
}

func TestPollerWaitsForCI(t *testing.T) {
	p := newPollEnv(t)
	p.saveGraph("pr_ready", func(s *workflow.EventSettings) { s.WaitCI = "passed" })
	p.goLive(workflow.PR)
	pr := p.addPR(1, false)
	p.gh.ci[pr.HeadSHA] = "pending"
	p.round()
	p.round()
	p.wantRuns()
	p.gh.ci[pr.HeadSHA] = "failed"
	p.round()
	p.wantRuns()
	p.gh.ci[pr.HeadSHA] = "passed" // nothing about the pull request changed
	p.round()
	p.wantRuns(1)
}

func TestPollerChecksFinishedIsActivity(t *testing.T) {
	p := newPollEnv(t)
	p.addPR(1, false)
	p.saveGraph("pr_activity", func(s *workflow.EventSettings) { s.Activity, s.QuietMinutes = []string{"checks"}, 0 })
	p.goLive(workflow.PR)
	p.editPR(1, func(l *github.Listed) { l.HeadSHA = "sha1-2" }) // a push; not an activity ticked
	p.gh.ci["sha1-2"] = "pending"
	p.round()
	p.round()
	p.wantRuns()
	p.gh.ci["sha1-2"] = "passed"
	p.round()
	p.round()
	p.wantRuns(1)
}

func TestPollerStartsAtMostThreePerRound(t *testing.T) {
	p := newPollEnv(t)
	p.saveGraph("pr_ready", nil)
	p.goLive(workflow.PR)
	for n := 1; n <= 5; n++ {
		p.addPR(n, false)
	}
	p.round()
	if got := len(p.runs()); got != 3 {
		t.Fatalf("%d runs in the first round", got)
	}
	p.round()
	p.wantRuns(1, 2, 3, 4, 5)
	p.round()
	p.wantRuns(1, 2, 3, 4, 5)
}

func TestPollerNeverRunsTheSameNodeTwiceAtOnce(t *testing.T) {
	p := newPollEnv(t)
	p.saveGraph("pr_activity", func(s *workflow.EventSettings) { s.Activity, s.QuietMinutes = []string{"comments"}, 0 })
	p.goLive(workflow.PR)
	p.addPR(1, false)
	p.round()
	// A run from this node on this item is under way.
	id, err := p.st.StartWorkflowRun(store.WorkflowRun{WorkspaceID: p.ws.ID, Kind: workflow.PR, EventNode: "ev", Item: 1, Graph: "{}"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.held[id] = true
	p.comment(1, "carol", "one")
	p.round()
	p.round()
	if got := len(p.runs()); got != 1 {
		t.Fatalf("started while one was running: %d runs", got)
	}
	// When it ends the waiting comment starts the next.
	p.st.FinishWorkflowRun(id, "done")
	delete(p.held, id)
	p.round()
	if got := len(p.runs()); got != 2 {
		t.Fatalf("%d runs after it ended", got)
	}
}

func TestPollerAddedNodeIgnoresWhatWasThere(t *testing.T) {
	p := newPollEnv(t)
	p.addPR(1, true)
	p.saveGraph("pr_ready", nil)
	p.goLive(workflow.PR)
	p.round()
	// A PR Draft block is added to the live workflow: the draft that was already there is not news.
	stored, _ := p.st.Workflow(p.ws.ID, workflow.PR)
	g, _ := loadGraph(stored)
	g.Nodes = append(g.Nodes, workflow.Node{ID: "dr", Type: "pr_draft"})
	b, _ := json.Marshal(g)
	if _, err := p.st.SaveWorkflow(p.ws.ID, workflow.PR, stored.Version, string(b), p.admin); err != nil {
		t.Fatal(err)
	}
	p.round()
	p.round()
	p.wantRuns()
	p.addPR(2, true)
	p.round()
	p.wantRuns(2)
}

func TestPollerBacksOffWhenGitHubFails(t *testing.T) {
	p := newPollEnv(t)
	p.saveGraph("pr_ready", nil)
	p.goLive(workflow.PR)
	p.addPR(1, false)
	p.gh.err = errors.New("GitHub is limiting how often we can ask")
	p.round()
	calls := p.gh.calls
	if calls == 0 {
		t.Fatal("no call made")
	}
	p.round() // inside the wait: nothing asked
	if p.gh.calls != calls {
		t.Fatalf("asked GitHub again inside the backoff: %d calls", p.gh.calls-calls)
	}
	if _, body := p.b.get("/w/o/r/workflows/pr"); !strings.Contains(body, "GitHub did not answer: GitHub is limiting") {
		t.Fatalf("the canvas says nothing:\n%.300s", body)
	}
	p.clk = p.clk.Add(3 * time.Minute)
	p.gh.err = nil
	p.round()
	p.wantRuns(1)
	if _, body := p.b.get("/w/o/r/workflows/pr"); strings.Contains(body, "GitHub did not answer") {
		t.Fatal("the complaint stays after it recovered")
	}
}

func TestCanvasShowsLiveAndRecent(t *testing.T) {
	p := newPollEnv(t)
	p.addPR(1, true)
	p.saveGraph("pr_ready", nil)
	if _, body := p.b.get("/w/o/r/workflows/pr"); !strings.Contains(body, "Live: off") || !strings.Contains(body, "Turn on") {
		t.Fatalf("off state:\n%.200s", body)
	}
	p.goLive(workflow.PR)
	p.editPR(1, func(l *github.Listed) { l.Draft = false })
	p.round()
	p.round()
	_, body := p.b.get("/w/o/r/workflows/pr")
	if !strings.Contains(body, "Live: on") || !strings.Contains(body, "Recent (1)") || !strings.Contains(body, "#1") || !strings.Contains(body, "Started: it is ready for review") {
		t.Fatalf("on state:\n%s", body)
	}
}

func TestSwitchLiveNeedsASavedWorkflowWithAnEventBlock(t *testing.T) {
	p := newPollEnv(t)
	p.goLive(workflow.PR) // nothing saved
	if st, _ := p.st.Workflow(p.ws.ID, workflow.PR); st.Live {
		t.Fatal("an empty workflow went live")
	}
	if _, body := p.b.get("/w/o/r/workflows/pr"); !strings.Contains(body, "Save the workflow") {
		t.Fatal("no explanation")
	}
}

func TestSwitchLiveFailsWhenGitHubCannotBeRead(t *testing.T) {
	p := newPollEnv(t)
	p.saveGraph("pr_ready", nil)
	p.gh.err = errors.New("down")
	p.goLive(workflow.PR)
	if st, _ := p.st.Workflow(p.ws.ID, workflow.PR); st.Live {
		t.Fatal("went live without a baseline")
	}
	if _, body := p.b.get("/w/o/r/workflows/pr"); !strings.Contains(body, "Not switched live") {
		t.Fatal("no explanation")
	}
}

func TestSwitchingLiveIgnoresOldSummons(t *testing.T) {
	p := newPollEnv(t)
	p.addPR(1, false)
	p.gh.roles["carol"] = "write"
	p.comment(1, "carol", "@autobot review please") // before it was live
	p.saveGraph("pr_ready", func(s *workflow.EventSettings) { s.Mode = "summoned" })
	p.goLive(workflow.PR)
	p.round()
	p.round()
	p.wantRuns()
	p.comment(1, "carol", "@autobot review again")
	p.round()
	p.wantRuns(1)
}

// What was waiting when a workflow was switched off is not acted on when it is switched on again.
func TestSwitchingOffAndOnAgainForgetsPending(t *testing.T) {
	p := newPollEnv(t)
	p.saveGraph("pr_ready", func(s *workflow.EventSettings) { s.WaitCI = "finished" })
	p.goLive(workflow.PR)
	p.round()
	pr := p.addPR(1, false)
	p.gh.ci[pr.HeadSHA] = "pending"
	p.round()
	p.wantRuns() // waiting for CI
	p.b.post("/w/o/r/workflows/pr/live", url.Values{"csrf": {p.csrf}, "live": {"off"}})
	p.gh.ci[pr.HeadSHA] = "passed"
	p.goLive(workflow.PR)
	p.round()
	p.round()
	p.wantRuns()
}
