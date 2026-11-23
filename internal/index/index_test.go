package index

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/store"
)

// fakeRepo is a GitHub repository the tests change, that counts every call made to it.
type fakeRepo struct {
	mu       sync.Mutex
	items    map[int]*github.ItemRecord
	comments map[int64]github.CommentRecord // conversation comments, by id
	rcomment map[int64]github.CommentRecord
	pulls    map[int]*github.PullRecord
	files    map[int][]github.ChangedFile
	reviews  map[int][]github.CommentRecord
	calls    []string
	lists    []github.ItemQuery // every list of items asked for
	failFrom int                // the call with this number (from 1) and every later one fails; 0 for none
	block    chan struct{}      // when set, ListItems waits for it to close
}

func newRepo() *fakeRepo {
	return &fakeRepo{items: map[int]*github.ItemRecord{}, comments: map[int64]github.CommentRecord{}, rcomment: map[int64]github.CommentRecord{},
		pulls: map[int]*github.PullRecord{}, files: map[int][]github.ChangedFile{}, reviews: map[int][]github.CommentRecord{}}
}

func (g *fakeRepo) enter(call string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, call)
	if g.failFrom > 0 && len(g.calls) >= g.failFrom {
		return errors.New("GitHub is down")
	}
	return nil
}

// count says how many calls began with prefix.
func (g *fakeRepo) count(prefix string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (g *fakeRepo) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = nil
}

func pageOf[T any](all []T, n int) []T {
	from := min((n-1)*github.IndexPage, len(all))
	return all[from:min(from+github.IndexPage, len(all))]
}

func (g *fakeRepo) ListItems(_ context.Context, _, _ string, q github.ItemQuery) ([]github.ItemRecord, error) {
	state := "all"
	if q.State == "open" {
		state = "open"
	}
	if err := g.enter(fmt.Sprintf("items:%s:%s:%d", state, q.Sort, q.Page)); err != nil {
		return nil, err
	}
	if g.block != nil {
		<-g.block
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lists = append(g.lists, q)
	var out []github.ItemRecord
	for _, it := range g.items {
		if !q.Since.IsZero() && it.Updated.Before(q.Since) || state == "open" && it.State != "open" {
			continue
		}
		rec := *it
		for _, c := range g.comments {
			if c.Number == it.Number {
				rec.Comments++
			}
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if q.Sort == "updated" && !out[i].Updated.Equal(out[j].Updated) {
			return out[i].Updated.Before(out[j].Updated)
		}
		return out[i].Number < out[j].Number
	})
	return pageOf(out, q.Page), nil
}

func (g *fakeRepo) PullDetail(_ context.Context, _, _ string, n int) (*github.PullRecord, error) {
	if err := g.enter(fmt.Sprintf("pull:%d", n)); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	p := *g.pulls[n]
	p.Updated = g.items[n].Updated
	for _, c := range g.rcomment {
		if c.Number == n {
			p.ReviewComments++
		}
	}
	return &p, nil
}

func (g *fakeRepo) PullComments(_ context.Context, _, _ string, n, page int) ([]github.CommentRecord, error) {
	if err := g.enter(fmt.Sprintf("pullcomments:%d:%d", n, page)); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []github.CommentRecord
	for _, c := range g.rcomment {
		if c.Number == n {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return pageOf(out, page), nil
}

func (g *fakeRepo) GetItem(_ context.Context, _, _ string, n int) (*github.ItemRecord, error) {
	if err := g.enter(fmt.Sprintf("getitem:%d", n)); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	it, ok := g.items[n]
	if !ok {
		return nil, github.ErrNoRepo
	}
	rec := *it
	for _, c := range g.comments {
		if c.Number == n {
			rec.Comments++
		}
	}
	return &rec, nil
}

func (g *fakeRepo) PullRequestFiles(_ context.Context, _, _ string, n int) ([]github.ChangedFile, error) {
	if err := g.enter(fmt.Sprintf("files:%d", n)); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.files[n], nil
}

func (g *fakeRepo) ReviewRecords(_ context.Context, _, _ string, n int) ([]github.CommentRecord, error) {
	if err := g.enter(fmt.Sprintf("reviews:%d", n)); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reviews[n], nil
}

func (g *fakeRepo) ListComments(_ context.Context, _, _ string, q github.CommentQuery) ([]github.CommentRecord, error) {
	if err := g.enter(fmt.Sprintf("comments:%v:%d", q.Review, q.Page)); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	src := g.comments
	if q.Review {
		src = g.rcomment
	}
	var out []github.CommentRecord
	for _, c := range src {
		if q.Since.IsZero() || !c.Updated.Before(q.Since) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return pageOf(out, q.Page), nil
}

func (g *fakeRepo) ItemComments(_ context.Context, _, _ string, n, page int) ([]github.CommentRecord, error) {
	if err := g.enter(fmt.Sprintf("itemcomments:%d:%d", n, page)); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []github.CommentRecord
	for _, c := range g.comments {
		if c.Number == n {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return pageOf(out, page), nil
}

func (g *fakeRepo) OpenCount(context.Context, string, string) (int, error) {
	if err := g.enter("open"); err != nil {
		return 0, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, it := range g.items {
		if it.State == "open" {
			n++
		}
	}
	return n, nil
}

var epoch = time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)

// addIssue and addPull add items to the repository, updated at the given time.
func (g *fakeRepo) addIssue(n int, title, body string, updated time.Time) {
	g.items[n] = &github.ItemRecord{Number: n, Kind: "issue", Title: title, Body: body, State: "open", Author: "alice", Created: updated, Updated: updated}
}

func (g *fakeRepo) addPull(n int, title, body string, updated time.Time, state string, files ...string) {
	it := &github.ItemRecord{Number: n, Kind: "pr", Title: title, Body: body, State: state, Author: "bob", Created: updated, Updated: updated}
	if state == "closed" {
		it.Closed, it.MergedAt = updated, updated
	}
	g.items[n] = it
	g.pulls[n] = &github.PullRecord{State: state, Merged: state == "closed", Base: "main", HeadRef: fmt.Sprintf("branch-%d", n), HeadSHA: fmt.Sprintf("sha%d-1", n), Additions: 3, Deletions: 1}
	g.files[n] = nil
	for _, f := range files {
		g.files[n] = append(g.files[n], github.ChangedFile{Path: f, Status: "modified", Additions: 3, Deletions: 1})
	}
}

func (g *fakeRepo) comment(id int64, n int, body string, at time.Time) {
	g.comments[id] = github.CommentRecord{Kind: "comment", ID: id, Number: n, Author: "carol", Body: body, Created: at, Updated: at}
}

// touch notes that an item changed at a time (GitHub bumps updated_at on any activity).
func (g *fakeRepo) touch(n int, at time.Time) { g.items[n].Updated = at }

// rig is a store, a fake repository and a syncer with a clock the test moves.
type rig struct {
	t    *testing.T
	st   *store.Store
	ws   store.Workspace
	repo *fakeRepo
	sy   *Syncer
	now  time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	admin, err := st.CreateUser("admin", "", "password-1234", true)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := st.CreateWorkspace("o/r", admin, "", "")
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, st: st, ws: *ws, repo: newRepo(), now: epoch.Add(24 * time.Hour)}
	r.sy = r.syncer()
	return r
}

// syncer makes a fresh Syncer over the same store, as a restart would.
func (r *rig) syncer() *Syncer {
	sy := New(r.st, r.repo)
	sy.Pace = 0
	sy.Now = func() time.Time { return r.now }
	return sy
}

// stepAll steps until nothing more is due now.
func (r *rig) stepAll() {
	r.t.Helper()
	for i := 0; i < 10000; i++ {
		more, err := r.sy.Step(context.Background(), r.ws, "tok")
		if err != nil {
			r.t.Fatal(err)
		}
		if !more {
			return
		}
	}
	r.t.Fatal("steps never ended")
}

func (r *rig) status() Status {
	s, err := StatusOf(r.st, r.ws.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

func (r *rig) read(n int) *store.ItemDetail {
	r.t.Helper()
	d, err := r.st.ItemIndex(r.ws.ID).Read(n)
	if err != nil {
		r.t.Fatalf("item #%d: %v", n, err)
	}
	return d
}

// populate is a small repository: two issues, an open pull request, two merged ones, comments of both kinds and a review.
func (r *rig) populate() {
	g := r.repo
	g.addIssue(1, "Uploads hang", "They never finish. See #3.", epoch)
	g.addIssue(2, "Docs typo", "", epoch.Add(time.Hour))
	g.addPull(3, "Retry uploads", "Fixes #1", epoch.Add(2*time.Hour), "open", "upload.go", "docs/up.md")
	g.addPull(4, "Old merged change", "", epoch.Add(3*time.Hour), "closed", "upload.go")
	g.addPull(5, "Other merged change", "closes #2, see https://github.com/o/r/pull/4 and https://github.com/x/y/pull/9", epoch.Add(4*time.Hour), "closed", "docs/typo.md")
	g.comment(10, 1, "Reproduced on v2, related to #3", epoch.Add(5*time.Hour))
	g.comment(11, 3, "Looks good", epoch.Add(5*time.Hour))
	g.rcomment[20] = github.CommentRecord{Kind: "review_comment", ID: 20, Number: 3, Author: "dana", Body: "bound this loop", Path: "upload.go", Line: 12, Created: epoch.Add(5 * time.Hour), Updated: epoch.Add(5 * time.Hour)}
	g.reviews[3] = []github.CommentRecord{{Kind: "review", ID: 30, Number: 3, Author: "dana", Body: "needs work", State: "CHANGES_REQUESTED", Created: epoch.Add(5 * time.Hour), Updated: epoch.Add(5 * time.Hour)}}
}

func TestBackfillCopiesEverything(t *testing.T) {
	r := newRig(t)
	r.populate()
	if Ready(r.st, r.ws.ID) {
		t.Fatal("ready before it started")
	}
	r.stepAll()
	s := r.status()
	if !s.Ready || s.Items != 5 || s.PRs != 3 || s.Issues != 2 || s.Open != 3 || s.Comments != 4 {
		t.Fatalf("status: %+v", s.IndexStats)
	}
	d := r.read(3)
	if d.Kind != "pr" || d.Base != "main" || d.HeadRef != "branch-3" || d.HeadSHA != "sha3-1" || d.Additions != 3 || len(d.Files) != 2 {
		t.Fatalf("pull request details: %+v", d)
	}
	if len(d.Discussion) != 3 || d.Discussion[0].Kind == "" || d.LinksOut[0].Number != 1 || d.LinksOut[0].How != "fixes" {
		t.Fatalf("discussion or links: %+v %+v", d.Discussion, d.LinksOut)
	}
	if d5 := r.read(5); !d5.Merged || len(d5.LinksOut) != 2 || d5.LinksOut[0].Number != 2 || d5.LinksOut[1].Number != 4 {
		t.Fatalf("a merged pull request and its links (other repositories dropped): %+v %+v", d5.Merged, d5.LinksOut)
	}
	if hits, _ := r.st.ItemIndex(r.ws.ID).Search(store.ItemSearch{Query: "loops"}); len(hits) != 1 || hits[0].Number != 3 {
		t.Fatalf("the inline review comment is not searchable: %+v", hits)
	}
}

// A repository of 250 items takes three pages of the list. When the backfill stops part way, a new
// Syncer (a restart) must carry on from where it was, not read the pages again.
func TestBackfillResumesAfterRestart(t *testing.T) {
	r := newRig(t)
	for n := 1; n <= 250; n++ {
		r.repo.addIssue(n, fmt.Sprintf("Issue %d", n), "", epoch.Add(time.Duration(n)*time.Minute))
	}
	r.repo.failFrom = 3                                                     // the list's first two pages are read, then GitHub goes away
	if _, err := r.sy.Step(context.Background(), r.ws, "tok"); err != nil { // starts
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := r.sy.Step(context.Background(), r.ws, "tok"); err != nil {
			break
		}
	}
	if s := r.status(); s.Ready || s.Items != 200 || s.Error == "" {
		t.Fatalf("after the stop: ready=%v items=%d error=%q", s.Ready, s.Items, s.Error)
	}
	r.repo.failFrom = 0
	r.repo.reset()
	r.sy = r.syncer() // a restart
	r.stepAll()
	if s := r.status(); !s.Ready || s.Items != 250 || s.Error != "" {
		t.Fatalf("after the restart: %+v error=%q", s.IndexStats, s.Error)
	}
	if n := r.repo.count("items:open:created:1"); n != 0 {
		t.Fatalf("the restart read the first page of items again (%d times)", n)
	}
	if n := r.repo.count("items:open:created:3"); n != 1 {
		t.Fatalf("the restart read the third page %d times, want 1", n)
	}
}

// Closed items that did not change are not read again: a sync with nothing new asks only for the
// lists, and one changed pull request costs its own page, its reviews, and its files only if its head moved.
func TestSyncReadsOnlyWhatChanged(t *testing.T) {
	r := newRig(t)
	r.populate()
	r.stepAll()
	r.repo.reset()

	r.now = r.now.Add(3 * time.Minute)
	r.stepAll()
	if got := strings.Join(r.repo.calls, " "); got != "items:all:updated:1 comments:false:1 comments:true:1" {
		t.Fatalf("a sync with nothing new made the calls %q", got)
	}
	for _, p := range []string{"pull:", "files:", "reviews:"} {
		if n := r.repo.count(p); n != 0 {
			t.Fatalf("a sync with nothing new read %d of %s", n, p)
		}
	}

	// A comment on the open pull request, and nothing else: its page and reviews are read, its files are not.
	later := r.now.Add(-time.Minute)
	r.repo.comment(12, 3, "ship it, fixes the retry bug", later)
	r.repo.touch(3, later)
	r.repo.reset()
	r.now = r.now.Add(3 * time.Minute)
	r.stepAll()
	if r.repo.count("pull:3") != 1 || r.repo.count("reviews:3") != 1 || r.repo.count("files:") != 0 || r.repo.count("pull:") != 1 {
		t.Fatalf("calls for a new comment: %v", r.repo.calls)
	}
	if got := len(r.read(3).Discussion); got != 4 {
		t.Fatalf("the new comment is not kept: %d in the discussion", got)
	}

	// The next sync lists that pull request again (a sync reaches back a minute before the latest change)
	// but it has not changed since it was read, so it is not read again.
	r.repo.reset()
	r.now = r.now.Add(3 * time.Minute)
	r.stepAll()
	if got := strings.Join(r.repo.calls, " "); got != "items:all:updated:1 comments:false:1 comments:true:1" {
		t.Fatalf("an unchanged pull request that was listed again made the calls %q", got)
	}

	// A push: the head moves, so its files are read again.
	later = r.now.Add(-time.Minute)
	r.repo.pulls[3].HeadSHA = "sha3-2"
	r.repo.files[3] = append(r.repo.files[3], github.ChangedFile{Path: "new.go", Status: "added"})
	r.repo.touch(3, later)
	r.repo.reset()
	r.now = r.now.Add(3 * time.Minute)
	r.stepAll()
	if r.repo.count("files:3") != 1 || r.repo.count("pull:") != 1 {
		t.Fatalf("calls for a push: %v", r.repo.calls)
	}
	if d := r.read(3); d.HeadSHA != "sha3-2" || len(d.Files) != 3 {
		t.Fatalf("the push is not kept: %+v", d)
	}
}

// A sync is not due again for two minutes.
func TestSyncWaitsItsTurn(t *testing.T) {
	r := newRig(t)
	r.populate()
	r.stepAll()
	r.repo.reset()
	r.now = r.now.Add(time.Minute)
	r.stepAll()
	if len(r.repo.calls) != 0 {
		t.Fatalf("a sync one minute after the last: %v", r.repo.calls)
	}
}

// Something the sync missed (changes GitHub showed late, a comment deleted there) is found by the
// reconcile, which reads it again, says so, and records agreement only when nothing needed fixing.
func TestReconcileReadsAgainWhatIsMissing(t *testing.T) {
	r := newRig(t)
	r.populate()
	r.stepAll()

	// An item and a comment that appeared with times before the sync's cursor, and a deleted comment.
	r.repo.addIssue(6, "Missed issue", "surfaced late", epoch)
	r.repo.comment(13, 2, "a late comment", epoch)
	delete(r.repo.comments, 10)
	r.repo.touch(1, r.repo.items[1].Updated) // comment 10 was on issue 1; GitHub no longer lists it
	r.now = r.now.Add(3 * time.Minute)
	r.stepAll()
	if _, err := r.st.ItemIndex(r.ws.ID).Read(6); err == nil {
		t.Fatal("the sync was not supposed to find the old item")
	}

	r.now = r.now.Add(reconcileEvery)
	r.repo.reset()
	r.stepAll()
	if r.repo.count("items:open:created:") == 0 || r.repo.count("items:all:created:") == 0 || r.repo.count("open") != 1 {
		t.Fatalf("reconcile did not walk the list and check the count: %v", r.repo.calls)
	}
	if d := r.read(6); d.Title != "Missed issue" {
		t.Fatalf("the missing item was not read: %+v", d)
	}
	if d := r.read(2); len(d.Discussion) != 1 || d.Discussion[0].Body != "a late comment" {
		t.Fatalf("the missing comment was not read: %+v", d.Discussion)
	}
	if d := r.read(1); len(d.Discussion) != 0 {
		t.Fatalf("a comment deleted on GitHub is still kept: %+v", d.Discussion)
	}
	s := r.status()
	if !s.AgreedAt.IsZero() || !strings.Contains(s.Note, "read again") || s.ReconciledAt.IsZero() {
		t.Fatalf("a reconcile that fixed things must not claim agreement: %+v", s)
	}

	r.now = r.now.Add(reconcileEvery)
	r.stepAll()
	if s := r.status(); s.AgreedAt.IsZero() || s.Note != "" {
		t.Fatalf("a clean reconcile should record agreement: agreed=%v note=%q", s.AgreedAt, s.Note)
	}
}

// A reconcile does not read items again that already agree.
func TestReconcileLeavesAgreeingItemsAlone(t *testing.T) {
	r := newRig(t)
	r.populate()
	r.stepAll()
	r.now = r.now.Add(reconcileEvery)
	r.repo.reset()
	r.stepAll()
	for _, p := range []string{"pull:", "files:", "reviews:", "itemcomments:"} {
		if n := r.repo.count(p); n != 0 {
			t.Fatalf("a reconcile of agreeing data made %d calls beginning %s: %v", n, p, r.repo.calls)
		}
	}
	if s := r.status(); s.AgreedAt.IsZero() {
		t.Fatalf("they agree: %+v", s)
	}
}

// After a failure the workspace is left alone for a while, the error is kept for the settings page,
// and the next success clears it.
func TestFailureBacksOffAndIsRecorded(t *testing.T) {
	r := newRig(t)
	r.populate()
	r.repo.failFrom = 1
	tg := Target{WS: r.ws, Token: "tok"}
	r.sy.Run(context.Background(), tg)
	if s := r.status(); !strings.Contains(s.Error, "GitHub is down") || s.ErrorAt.IsZero() {
		t.Fatalf("error not recorded: %+v", s)
	}
	n := len(r.repo.calls)
	r.sy.Run(context.Background(), tg)
	if len(r.repo.calls) != n {
		t.Fatal("tried again at once after a failure")
	}
	r.repo.failFrom = 0
	r.now = r.now.Add(firstBackoff + time.Second)
	r.sy.Run(context.Background(), tg)
	if s := r.status(); !s.Ready || s.Error != "" {
		t.Fatalf("did not recover: ready=%v error=%q", s.Ready, s.Error)
	}
}

// Only one run per workspace works at a time.
func TestOneRunPerWorkspace(t *testing.T) {
	r := newRig(t)
	r.populate()
	r.repo.block = make(chan struct{})
	tg := Target{WS: r.ws, Token: "tok"}
	done := make(chan struct{})
	go func() { r.sy.Run(context.Background(), tg); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for r.repo.count("items:") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first run never started")
		}
		time.Sleep(time.Millisecond)
	}
	second := make(chan struct{})
	go func() { r.sy.Run(context.Background(), tg); close(second) }()
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("a second run for the same workspace waited instead of leaving")
	}
	if n := r.repo.count("items:"); n != 1 {
		t.Fatalf("the second run read GitHub too (%d list calls)", n)
	}
	close(r.repo.block)
	<-done
	if !r.status().Ready {
		t.Fatal("the first run did not finish")
	}
}
