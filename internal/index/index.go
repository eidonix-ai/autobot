// Package index keeps a lean, current local copy of each workspace repository's pull requests and
// issues with their discussion, so that agents can search and cross-reference without asking GitHub.
// It is a working copy, not an archive: GitHub stays the source for anything old. The copy lives in
// the store (see store/items.go); this package fills it and keeps it right with as little GitHub
// traffic as it can.
//
//   - What is kept: open items and items closed or merged within store.KeepInFull, in full. Older
//     closed items shrink to short rows (store.KeepSlim) and then go (see Prune).
//   - Backfill reads that once, a page or a pull request at a time, paced well under GitHub's rate
//     limit: the open items, the items updated within the window, and their discussion. Where it has
//     got to is saved after every step, so a restart carries on. It never walks the whole history.
//   - Sync runs every two minutes and asks only for what changed since the last time: the changed
//     items and the changed comments (both kinds). Asking again with nothing new is a "not modified"
//     answer, which GitHub does not count. A pull request is read again (its page, its files if the
//     head moved, its reviews) only when GitHub says it was updated after it was last read.
//   - Reconcile runs every six hours: it walks GitHub's open items and its recently updated ones,
//     compares them with the copy (what is missing, what is out of date, how many comments each has),
//     reads again whatever differs, and records whether the two agreed.
//   - Prune runs daily, and once when the backfill ends.
//   - Fetch reads one item, of any age, straight from GitHub for an agent that asks for it.
package index

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/store"
)

// Source is what the index asks of GitHub (all reads). *github.Client is one.
type Source interface {
	ListItems(ctx context.Context, token, name string, q github.ItemQuery) ([]github.ItemRecord, error)
	GetItem(ctx context.Context, token, name string, number int) (*github.ItemRecord, error)
	PullDetail(ctx context.Context, token, name string, number int) (*github.PullRecord, error)
	PullRequestFiles(ctx context.Context, token, name string, number int) ([]github.ChangedFile, error)
	ReviewRecords(ctx context.Context, token, name string, number int) ([]github.CommentRecord, error)
	PullComments(ctx context.Context, token, name string, number, page int) ([]github.CommentRecord, error)
	ListComments(ctx context.Context, token, name string, q github.CommentQuery) ([]github.CommentRecord, error)
	ItemComments(ctx context.Context, token, name string, number, page int) ([]github.CommentRecord, error)
	OpenCount(ctx context.Context, token, name string) (int, error)
}

const (
	// A pause before each GitHub call: 2 seconds is at most 1,800 calls an hour, about a third of
	// what a token may make.
	defaultPace    = 2 * time.Second
	syncEvery      = 2 * time.Minute
	reconcileEvery = 6 * time.Hour
	pruneEvery     = 24 * time.Hour
	overlap        = time.Minute // each ask reaches this far back, in case GitHub shows changes late
	firstBackoff   = 2 * time.Minute
	longestBackoff = 30 * time.Minute
	tickEvery      = 30 * time.Second
	maxPages       = 2000 // a list this long is not believed
)

// What the store's index_state holds for a workspace.
const (
	kPhase       = "backfill_phase" // "", open, recent, comments, review_comments, pulls, gaps, done
	kPage        = "backfill_page"  // the next page of the list the phase reads
	kGapAfter    = "backfill_gap_after"
	kWindowFrom  = "backfill_window_from" // the start of the window the backfill reads, in unix seconds
	kItemsSince  = "items_since"          // the latest updated time seen on an item
	kCommentsAt  = "comments_since"
	kReviewsAt   = "review_comments_since"
	kSyncedAt    = "synced_at"
	kReconcileAt = "reconciled_at"
	kAgreedAt    = "agreed_at"
	kNote        = "reconcile_note"
	kPrunedAt    = "pruned_at"
	kPruneNote   = "prune_note"
	kError       = "error"
	kErrorAt     = "error_at"
)

// Syncer fills and maintains the local copy.
type Syncer struct {
	St  *store.Store
	Src Source
	Now func() time.Time
	// Pace is the wait before each GitHub call. Tests set it to zero.
	Pace time.Duration

	mu      sync.Mutex
	running map[int64]bool
	trouble map[int64]trouble
}

type trouble struct {
	until time.Time
	wait  time.Duration
}

// New returns a Syncer with the real pace and clock.
func New(st *store.Store, src Source) *Syncer {
	return &Syncer{St: st, Src: src, Now: time.Now, Pace: defaultPace, running: map[int64]bool{}, trouble: map[int64]trouble{}}
}

// Target is a workspace to keep a copy for, with the token to read its repository.
type Target struct {
	WS    store.Workspace
	Token string
}

// Loop looks at every workspace twice a minute until ctx ends, and runs what is due for each in
// its own goroutine. targets lists the workspaces to index (those with a GitHub identity).
func (sy *Syncer) Loop(ctx context.Context, targets func() ([]Target, error)) {
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		ts, err := targets()
		if err != nil {
			log.Printf("index: %v", err)
		}
		for _, tg := range ts {
			go sy.Run(ctx, tg)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Run does what is due for one workspace: steps until nothing more is due now. Only one Run per
// workspace works at a time; another call returns at once. After a failure the workspace waits
// (2 minutes, doubling to 30) before it is tried again, and the failure is kept for its settings page.
func (sy *Syncer) Run(ctx context.Context, tg Target) {
	id := tg.WS.ID
	sy.mu.Lock()
	if sy.running[id] || sy.Now().Before(sy.trouble[id].until) {
		sy.mu.Unlock()
		return
	}
	sy.running[id] = true
	sy.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("index: %s: %v", tg.WS.Name, r)
		}
		sy.mu.Lock()
		delete(sy.running, id)
		sy.mu.Unlock()
	}()
	for ctx.Err() == nil {
		more, err := sy.Step(ctx, tg.WS, tg.Token)
		if err != nil {
			log.Printf("index: %s: %v", tg.WS.Name, err)
			sy.mu.Lock()
			t := sy.trouble[id]
			t.wait = min(max(t.wait*2, firstBackoff), longestBackoff)
			t.until = sy.Now().Add(t.wait)
			sy.trouble[id] = t
			sy.mu.Unlock()
			return
		}
		sy.mu.Lock()
		delete(sy.trouble, id)
		sy.mu.Unlock()
		if !more {
			return
		}
	}
}

// Step does the next piece of work due for a workspace and says whether more is due right away: a
// backfill is many steps, a sync, a reconcile or a prune one each. A failure is recorded for the
// settings page.
func (sy *Syncer) Step(ctx context.Context, ws store.Workspace, token string) (more bool, err error) {
	defer func() {
		if err != nil {
			sy.St.IndexSet(ws.ID, kError, err.Error())
			sy.setTime(ws.ID, kErrorAt, sy.Now())
		} else {
			sy.St.IndexSet(ws.ID, kError, "")
		}
	}()
	if sy.St.IndexGet(ws.ID, kPhase) != "done" {
		return sy.backfillStep(ctx, ws, token)
	}
	switch sy.due(ws.ID) {
	case "sync":
		err = sy.sync(ctx, ws, token)
	case "reconcile":
		err = sy.reconcile(ctx, ws, token)
	case "prune":
		err = sy.prune(ws)
	}
	return err == nil && sy.due(ws.ID) != "", err
}

// due names the next job a finished workspace is waiting for: the most urgent first.
func (sy *Syncer) due(ws int64) string {
	now := sy.Now()
	switch {
	case now.Sub(sy.at(ws, kSyncedAt)) >= syncEvery:
		return "sync"
	case now.Sub(sy.at(ws, kReconcileAt)) >= reconcileEvery:
		return "reconcile"
	case now.Sub(sy.at(ws, kPrunedAt)) >= pruneEvery:
		return "prune"
	}
	return ""
}

func (sy *Syncer) at(ws int64, key string) time.Time {
	n, _ := strconv.ParseInt(sy.St.IndexGet(ws, key), 10, 64)
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func (sy *Syncer) setTime(ws int64, key string, t time.Time) error {
	return sy.St.IndexSet(ws, key, strconv.FormatInt(t.Unix(), 10))
}

// advance moves a cursor forward to t, never back.
func (sy *Syncer) advance(ws int64, key string, t time.Time) error {
	if t.After(sy.at(ws, key)) {
		return sy.setTime(ws, key, t)
	}
	return nil
}

// pace waits before a GitHub call.
func (sy *Syncer) pace(ctx context.Context) error {
	if sy.Pace > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(sy.Pace):
		}
	}
	return ctx.Err()
}

// ---- Backfill ----

func (sy *Syncer) backfillStep(ctx context.Context, ws store.Workspace, token string) (bool, error) {
	phase := sy.St.IndexGet(ws.ID, kPhase)
	page, _ := strconv.Atoi(sy.St.IndexGet(ws.ID, kPage))
	page = max(page, 1)
	from := sy.at(ws.ID, kWindowFrom)
	goTo := func(next string) (bool, error) {
		for k, v := range map[string]string{kPage: "1", kGapAfter: "0"} {
			if err := sy.St.IndexSet(ws.ID, k, v); err != nil {
				return false, err
			}
		}
		return true, sy.St.IndexSet(ws.ID, kPhase, next)
	}
	nextPage := func(got int) (bool, error) {
		if got < github.IndexPage {
			return false, nil
		}
		return true, sy.St.IndexSet(ws.ID, kPage, strconv.Itoa(page+1))
	}
	items := func(q github.ItemQuery) (bool, error) {
		if err := sy.pace(ctx); err != nil {
			return false, err
		}
		q.Sort, q.Page = "created", page
		recs, err := sy.Src.ListItems(ctx, token, ws.Name, q)
		if err != nil {
			return false, err
		}
		for _, r := range recs {
			if _, err := sy.saveItem(ws, r); err != nil {
				return false, err
			}
		}
		return nextPage(len(recs))
	}
	switch phase {
	case "":
		// Whatever changes from now on, the sync will catch: it starts from a little before now.
		start := sy.Now().Add(-overlap)
		for _, k := range []string{kItemsSince, kCommentsAt, kReviewsAt} {
			if err := sy.setTime(ws.ID, k, start); err != nil {
				return false, err
			}
		}
		if err := sy.setTime(ws.ID, kWindowFrom, sy.Now().Add(-store.KeepInFull)); err != nil {
			return false, err
		}
		return goTo("open")
	case "open": // every open item, however old
		if more, err := items(github.ItemQuery{State: "open"}); err != nil || more {
			return more, err
		}
		return goTo("recent")
	case "recent": // whatever was updated within the window, open or closed
		if more, err := items(github.ItemQuery{Since: from}); err != nil || more {
			return more, err
		}
		return goTo("comments")
	case "comments", "review_comments":
		if err := sy.pace(ctx); err != nil {
			return false, err
		}
		recs, err := sy.Src.ListComments(ctx, token, ws.Name, github.CommentQuery{Review: phase == "review_comments", Since: from, Page: page})
		if err != nil {
			return false, err
		}
		if err := sy.saveComments(ws, recs); err != nil {
			return false, err
		}
		if more, err := nextPage(len(recs)); err != nil || more {
			return more, err
		}
		if phase == "comments" {
			return goTo("review_comments")
		}
		return goTo("pulls")
	case "pulls":
		n, ok, err := sy.St.NextUnreadPull(ws.ID)
		if err != nil {
			return false, err
		}
		if !ok {
			return goTo("gaps")
		}
		return true, sy.refreshPull(ctx, ws, token, n)
	case "gaps":
		// The repository-wide comment lists reached back only as far as the window; open items may have
		// older comments. Read those for the items that are short of what GitHub says they have.
		after, _ := strconv.Atoi(sy.St.IndexGet(ws.ID, kGapAfter))
		n, ok, err := sy.St.NextCommentGap(ws.ID, after)
		if err != nil {
			return false, err
		}
		if ok {
			if err := sy.fillGap(ctx, ws, token, n); err != nil {
				return false, err
			}
			return true, sy.St.IndexSet(ws.ID, kGapAfter, strconv.Itoa(n))
		}
		// Read from end to end: the first reconcile is due in six hours.
		for _, k := range []string{kReconcileAt, kSyncedAt} {
			if err := sy.setTime(ws.ID, k, sy.Now()); err != nil {
				return false, err
			}
		}
		if err := sy.St.IndexSet(ws.ID, kPhase, "done"); err != nil {
			return false, err
		}
		return false, sy.prune(ws)
	}
	return false, fmt.Errorf("unknown backfill phase %q", phase)
}

// ---- Writing what GitHub said ----

// inWindow says whether an item is one the copy keeps in full: open, or closed within store.KeepInFull.
func (sy *Syncer) inWindow(r github.ItemRecord) bool {
	if r.State == "open" {
		return true
	}
	closed := r.Closed
	if closed.IsZero() {
		closed = r.Updated
	}
	return sy.Now().Sub(closed) < store.KeepInFull
}

// saveItem keeps an item if it is within the window, and says whether it did. An older one is
// left alone: if it was kept before, pruning will age it.
func (sy *Syncer) saveItem(ws store.Workspace, r github.ItemRecord) (bool, error) {
	if !sy.inWindow(r) {
		return false, nil
	}
	it := store.Item{Number: r.Number, Kind: r.Kind, Title: r.Title, Body: r.Body, State: r.State, Merged: !r.MergedAt.IsZero(),
		Draft: r.Draft, Author: r.Author, Association: r.Association, Labels: r.Labels, Comments: r.Comments,
		Created: r.Created, Updated: r.Updated, Closed: r.Closed, MergedAt: r.MergedAt}
	return true, sy.St.SaveItem(ws.ID, it, Links(ws.Name, r.Number, r.Title+"\n"+r.Body))
}

func commentRows(ws store.Workspace, recs []github.CommentRecord) []store.ItemComment {
	cs := make([]store.ItemComment, 0, len(recs))
	for _, r := range recs {
		if r.Number == 0 {
			continue
		}
		cs = append(cs, store.ItemComment{Kind: r.Kind, ID: r.ID, Number: r.Number, Author: r.Author, Association: r.Association,
			Body: r.Body, ReviewState: r.State, Path: r.Path, Line: r.Line, Created: r.Created, Updated: r.Updated,
			Links: Links(ws.Name, r.Number, r.Body)})
	}
	return cs
}

// saveComments keeps comments of the items that are kept in full; the store drops the rest.
func (sy *Syncer) saveComments(ws store.Workspace, recs []github.CommentRecord) error {
	return sy.St.SaveComments(ws.ID, commentRows(ws, recs))
}

// refreshPull reads a pull request's own page and its reviews, and its files unless it has not
// moved since they were last read. The page is saved last, so a failure part way means it is read again.
func (sy *Syncer) refreshPull(ctx context.Context, ws store.Workspace, token string, number int) error {
	meta, err := sy.St.ItemMeta(ws.ID, number)
	if err != nil {
		return err
	}
	if err := sy.pace(ctx); err != nil {
		return err
	}
	p, err := sy.Src.PullDetail(ctx, token, ws.Name, number)
	if errors.Is(err, github.ErrNoRepo) {
		// GitHub has no such pull request (it was removed): note it as read, so it is not asked for again.
		return sy.St.SavePull(ws.ID, number, store.PullInfo{State: meta.State, Updated: meta.Updated})
	}
	if err != nil {
		return err
	}
	if meta.DetailUpdated.IsZero() || p.HeadSHA != meta.HeadSHA {
		files, err := sy.pullFiles(ctx, ws, token, number)
		if err != nil {
			return err
		}
		if err := sy.St.SaveFiles(ws.ID, number, files); err != nil {
			return err
		}
	}
	if err := sy.pace(ctx); err != nil {
		return err
	}
	reviews, err := sy.Src.ReviewRecords(ctx, token, ws.Name, number)
	if err != nil {
		return err
	}
	if err := sy.saveComments(ws, reviews); err != nil {
		return err
	}
	return sy.St.SavePull(ws.ID, number, store.PullInfo{State: p.State, Merged: p.Merged, MergedAt: p.MergedAt, Draft: p.Draft,
		Base: p.Base, HeadRef: p.HeadRef, HeadSHA: p.HeadSHA, Additions: p.Additions, Deletions: p.Deletions,
		ReviewComments: p.ReviewComments, Updated: laterOf(p.Updated, meta.Updated)})
}

func (sy *Syncer) pullFiles(ctx context.Context, ws store.Workspace, token string, number int) ([]store.ItemFile, error) {
	if err := sy.pace(ctx); err != nil {
		return nil, err
	}
	files, err := sy.Src.PullRequestFiles(ctx, token, ws.Name, number)
	if err != nil {
		return nil, err
	}
	out := make([]store.ItemFile, len(files))
	for i, f := range files {
		out[i] = store.ItemFile{Path: f.Path, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions}
	}
	return out, nil
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// fillGap reads an item's comments from GitHub, by item, where the copy holds fewer than GitHub says.
func (sy *Syncer) fillGap(ctx context.Context, ws store.Workspace, token string, number int) error {
	issue, review, err := sy.St.CommentGap(ws.ID, number)
	if err != nil {
		return err
	}
	if issue {
		if err := sy.rereadComments(ctx, ws, token, number, "comment"); err != nil {
			return err
		}
	}
	if review {
		return sy.rereadComments(ctx, ws, token, number, "review_comment")
	}
	return nil
}

// rereadComments reads all of an item's comments of a kind ("comment" or "review_comment") and drops
// any the copy has that GitHub no longer shows (deleted there).
func (sy *Syncer) rereadComments(ctx context.Context, ws store.Workspace, token string, number int, kind string) error {
	recs, err := sy.itemComments(ctx, ws, token, number, kind)
	if err != nil {
		return err
	}
	if err := sy.saveComments(ws, recs); err != nil {
		return err
	}
	keep := make([]int64, len(recs))
	for i, r := range recs {
		keep[i] = r.ID
	}
	return sy.St.PruneComments(ws.ID, number, kind, keep)
}

func (sy *Syncer) itemComments(ctx context.Context, ws store.Workspace, token string, number int, kind string) ([]github.CommentRecord, error) {
	var all []github.CommentRecord
	for page := 1; page <= maxPages; page++ {
		if err := sy.pace(ctx); err != nil {
			return nil, err
		}
		var recs []github.CommentRecord
		var err error
		if kind == "review_comment" {
			recs, err = sy.Src.PullComments(ctx, token, ws.Name, number, page)
		} else {
			recs, err = sy.Src.ItemComments(ctx, token, ws.Name, number, page)
		}
		if err != nil {
			return nil, err
		}
		all = append(all, recs...)
		if len(recs) < github.IndexPage {
			break
		}
	}
	return all, nil
}

// ---- Sync ----

// sync reads what changed since the last sync: items, then comments, then inline review comments.
// Each cursor moves forward page by page, so a failure part way loses nothing. At the end, any
// changed item that is short of comments (one that came back into the window, say) is read by item.
func (sy *Syncer) sync(ctx context.Context, ws store.Workspace, token string) error {
	var touched []int
	for page := 1; ; page++ {
		if page > maxPages {
			return errors.New("the list of changed items does not end")
		}
		if err := sy.pace(ctx); err != nil {
			return err
		}
		recs, err := sy.Src.ListItems(ctx, token, ws.Name, github.ItemQuery{Sort: "updated", Since: sy.at(ws.ID, kItemsSince).Add(-overlap), Page: page, Cache: true})
		if err != nil {
			return err
		}
		var latest time.Time
		for _, r := range recs {
			kept, err := sy.saveItem(ws, r)
			if err != nil {
				return err
			}
			if kept {
				touched = append(touched, r.Number)
			}
			if kept && r.Kind == "pr" {
				// Saving the item wrote its new updated time; read the pull request unless it was read at or after that time.
				meta, err := sy.St.ItemMeta(ws.ID, r.Number)
				if err != nil {
					return err
				}
				if r.Updated.After(meta.DetailUpdated) {
					if err := sy.refreshPull(ctx, ws, token, r.Number); err != nil {
						return err
					}
				}
			}
			latest = laterOf(latest, r.Updated)
		}
		if err := sy.advance(ws.ID, kItemsSince, latest); err != nil {
			return err
		}
		if len(recs) < github.IndexPage {
			break
		}
	}
	for _, c := range []struct {
		key    string
		review bool
	}{{kCommentsAt, false}, {kReviewsAt, true}} {
		for page := 1; ; page++ {
			if page > maxPages {
				return errors.New("the list of changed comments does not end")
			}
			if err := sy.pace(ctx); err != nil {
				return err
			}
			recs, err := sy.Src.ListComments(ctx, token, ws.Name, github.CommentQuery{Review: c.review, Since: sy.at(ws.ID, c.key).Add(-overlap), Page: page, Cache: true})
			if err != nil {
				return err
			}
			var latest time.Time
			for _, r := range recs {
				latest = laterOf(latest, r.Updated)
			}
			if err := sy.saveComments(ws, recs); err != nil {
				return err
			}
			if err := sy.advance(ws.ID, c.key, latest); err != nil {
				return err
			}
			if len(recs) < github.IndexPage {
				break
			}
		}
	}
	for _, n := range touched {
		if err := sy.fillGap(ctx, ws, token, n); err != nil {
			return err
		}
	}
	return sy.setTime(ws.ID, kSyncedAt, sy.Now())
}

// ---- Reconcile ----

// reconcile compares the copy with GitHub's open items and its recently updated ones (the window the
// copy keeps in full), reads again what differs, and records whether the two agreed (nothing needed
// fixing, and the open counts match).
func (sy *Syncer) reconcile(ctx context.Context, ws store.Workspace, token string) error {
	local, err := sy.St.ItemSyncStates(ws.ID)
	if err != nil {
		return err
	}
	fixed := 0
	seen := map[int]bool{}
	check := func(r github.ItemRecord) error {
		if seen[r.Number] {
			return nil
		}
		seen[r.Number] = true
		kept, err := sy.saveItemIfOff(ws, r, local)
		if err != nil || !kept {
			return err
		}
		l, have := local[r.Number]
		off := !have || !l.Updated.Equal(r.Updated) || l.State != r.State
		if r.Kind == "pr" {
			if meta, err := sy.St.ItemMeta(ws.ID, r.Number); err != nil {
				return err
			} else if meta.DetailUpdated.IsZero() || r.Updated.After(meta.DetailUpdated) {
				off = true
				if err := sy.refreshPull(ctx, ws, token, r.Number); err != nil {
					return err
				}
			}
		}
		// A different number of comments either way (one missing, or one deleted on GitHub) means read them again.
		recount := (have && r.Comments != l.IssueComments) || (!have && r.Comments > 0)
		if recount {
			if err := sy.rereadComments(ctx, ws, token, r.Number, "comment"); err != nil {
				return err
			}
		}
		if off || recount {
			if err := sy.fillGap(ctx, ws, token, r.Number); err != nil {
				return err
			}
			fixed++
		}
		return nil
	}
	for _, q := range []github.ItemQuery{{State: "open"}, {Since: sy.Now().Add(-store.KeepInFull)}} {
		q.Sort, q.Cache = "created", true
		for page := 1; ; page++ {
			if page > maxPages {
				return errors.New("the list of items does not end")
			}
			if err := sy.pace(ctx); err != nil {
				return err
			}
			q.Page = page
			recs, err := sy.Src.ListItems(ctx, token, ws.Name, q)
			if err != nil {
				return err
			}
			for _, r := range recs {
				if err := check(r); err != nil {
					return err
				}
			}
			if len(recs) < github.IndexPage {
				break
			}
		}
	}
	if err := sy.pace(ctx); err != nil {
		return err
	}
	remote, err := sy.Src.OpenCount(ctx, token, ws.Name)
	if err != nil {
		return err
	}
	stats, err := sy.St.IndexStats(ws.ID)
	if err != nil {
		return err
	}
	note := ""
	switch {
	case fixed > 0:
		note = fmt.Sprintf("read again %d items that were missing or out of date", fixed)
	case stats.Open != remote:
		note = fmt.Sprintf("GitHub counts %d open, the copy %d", remote, stats.Open)
	}
	if err := sy.St.IndexSet(ws.ID, kNote, note); err != nil {
		return err
	}
	if note == "" {
		if err := sy.setTime(ws.ID, kAgreedAt, sy.Now()); err != nil {
			return err
		}
	}
	return sy.setTime(ws.ID, kReconcileAt, sy.Now())
}

// saveItemIfOff saves an item in the window when the copy does not hold it as GitHub shows it.
func (sy *Syncer) saveItemIfOff(ws store.Workspace, r github.ItemRecord, local map[int]store.ItemSync) (bool, error) {
	if !sy.inWindow(r) {
		return false, nil
	}
	if l, have := local[r.Number]; have && l.Updated.Equal(r.Updated) && l.State == r.State {
		return true, nil
	}
	return sy.saveItem(ws, r)
}

// ---- Prune ----

// prune ages the copy (see store.Prune) and notes what it did.
func (sy *Syncer) prune(ws store.Workspace) error {
	p, err := sy.St.Prune(ws.ID, sy.Now())
	if err != nil {
		return err
	}
	if err := sy.St.IndexSet(ws.ID, kPruneNote, fmt.Sprintf("%d shortened, %d deleted", p.Slimmed, p.Deleted)); err != nil {
		return err
	}
	return sy.setTime(ws.ID, kPrunedAt, sy.Now())
}

// ---- Fetch: any one item, live ----

// Fetch reads one issue or pull request, of any age, with its discussion, straight from GitHub. An
// item the copy keeps (open, or closed within the window) is saved and read back from the copy;
// an older one is returned as it is and not stored.
func (sy *Syncer) Fetch(ctx context.Context, ws store.Workspace, token string, number int) (*store.ItemDetail, error) {
	if err := sy.pace(ctx); err != nil {
		return nil, err
	}
	rec, err := sy.Src.GetItem(ctx, token, ws.Name, number)
	if err != nil {
		return nil, err
	}
	if sy.inWindow(*rec) {
		if _, err := sy.saveItem(ws, *rec); err != nil {
			return nil, err
		}
		if rec.Kind == "pr" {
			if err := sy.refreshPull(ctx, ws, token, number); err != nil {
				return nil, err
			}
		}
		if err := sy.fillGap(ctx, ws, token, number); err != nil {
			return nil, err
		}
		return sy.St.ItemIndex(ws.ID).Read(number)
	}

	d := &store.ItemDetail{Remote: true, Item: store.Item{Number: rec.Number, Kind: rec.Kind, Title: rec.Title, Body: rec.Body, State: rec.State,
		Merged: !rec.MergedAt.IsZero(), Draft: rec.Draft, Author: rec.Author, Association: rec.Association, Labels: rec.Labels,
		Created: rec.Created, Updated: rec.Updated, Closed: rec.Closed, MergedAt: rec.MergedAt}}
	var rows []github.CommentRecord
	if rec.Comments > 0 {
		if rows, err = sy.itemComments(ctx, ws, token, number, "comment"); err != nil {
			return nil, err
		}
	}
	if rec.Kind == "pr" {
		if err := sy.pace(ctx); err != nil {
			return nil, err
		}
		p, err := sy.Src.PullDetail(ctx, token, ws.Name, number)
		if err != nil && !errors.Is(err, github.ErrNoRepo) {
			return nil, err
		}
		if p != nil {
			d.Base, d.HeadRef, d.HeadSHA, d.Additions, d.Deletions = p.Base, p.HeadRef, p.HeadSHA, p.Additions, p.Deletions
			files, err := sy.pullFiles(ctx, ws, token, number)
			if err != nil {
				return nil, err
			}
			d.Files = files
			if err := sy.pace(ctx); err != nil {
				return nil, err
			}
			reviews, err := sy.Src.ReviewRecords(ctx, token, ws.Name, number)
			if err != nil {
				return nil, err
			}
			rows = append(rows, reviews...)
			if p.ReviewComments > 0 {
				inline, err := sy.itemComments(ctx, ws, token, number, "review_comment")
				if err != nil {
					return nil, err
				}
				rows = append(rows, inline...)
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Created.Before(rows[j].Created) })
	text := rec.Title + "\n" + rec.Body
	for _, c := range commentRows(ws, rows) {
		d.Discussion = append(d.Discussion, c)
		text += "\n" + c.Body
	}
	for _, l := range Links(ws.Name, number, text) {
		d.LinksOut = append(d.LinksOut, store.ItemRef{Number: l.To, How: l.How})
	}
	sort.Slice(d.LinksOut, func(i, j int) bool { return d.LinksOut[i].Number < d.LinksOut[j].Number })
	return d, nil
}

// ---- What the settings page and the agents ask ----

// Status is how a workspace's copy stands.
type Status struct {
	store.IndexStats
	Ready        bool   // the first full read has finished
	Progress     string // while it has not: what it is reading
	SyncedAt     time.Time
	ReconciledAt time.Time
	AgreedAt     time.Time // the last reconcile that found the copy and GitHub the same
	Note         string    // what the last reconcile did, when it did anything
	PrunedAt     time.Time
	PruneNote    string // what the last pruning did
	Error        string
	ErrorAt      time.Time
}

// StatusOf reads a workspace's status.
func StatusOf(st *store.Store, ws int64) (Status, error) {
	stats, err := st.IndexStats(ws)
	if err != nil {
		return Status{}, err
	}
	s := Status{IndexStats: stats, Ready: st.IndexGet(ws, kPhase) == "done", Note: st.IndexGet(ws, kNote), PruneNote: st.IndexGet(ws, kPruneNote)}
	at := func(k string) time.Time {
		n, _ := strconv.ParseInt(st.IndexGet(ws, k), 10, 64)
		if n == 0 {
			return time.Time{}
		}
		return time.Unix(n, 0)
	}
	s.SyncedAt, s.ReconciledAt, s.AgreedAt, s.PrunedAt = at(kSyncedAt), at(kReconcileAt), at(kAgreedAt), at(kPrunedAt)
	if !s.Ready {
		s.Progress = map[string]string{"": "starting", "open": "reading the open pull requests and issues", "recent": "reading the recently changed ones",
			"comments": "reading comments", "review_comments": "reading review comments", "pulls": "reading each pull request's files and reviews",
			"gaps": "filling in older comments"}[st.IndexGet(ws, kPhase)]
	}
	s.Error, s.ErrorAt = st.IndexGet(ws, kError), at(kErrorAt)
	return s, nil
}

// Ready reports whether a workspace's first full read has finished, so that its copy is complete
// enough to trust for searching.
func Ready(st *store.Store, ws int64) bool { return st.IndexGet(ws, kPhase) == "done" }
