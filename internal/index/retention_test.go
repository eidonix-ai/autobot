package index

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/store"
)

func daysAgo(now time.Time, d int) time.Time { return now.Add(-time.Duration(d) * 24 * time.Hour) }

// populateOld is a repository with history: the copy must keep the open items and the last 30 days
// of it, and nothing older.
//
//	#1 open issue, 100 days quiet, one comment 190 days old and one 2 days old
//	#2 issue closed 5 days ago, whose comment is 40 days old
//	#3 issue closed 40 days ago
//	#4 pull request merged 200 days ago
//	#5 issue closed 40 days ago that someone commented on yesterday
//	#6 open pull request, 100 days quiet, with a review comment 90 days old
func (r *rig) populateOld() {
	g, now := r.repo, r.now
	g.addIssue(1, "Old open issue", "never fixed", daysAgo(now, 100))
	g.items[1].Created = daysAgo(now, 200)
	g.comment(100, 1, "the very first report", daysAgo(now, 190))
	g.comment(101, 1, "still happens", daysAgo(now, 2))
	g.addIssue(2, "Recently closed", "", daysAgo(now, 5))
	g.items[2].State, g.items[2].Closed = "closed", daysAgo(now, 5)
	g.comment(102, 2, "a comment from before the window", daysAgo(now, 40))
	g.addIssue(3, "Closed last month", "", daysAgo(now, 40))
	g.items[3].State, g.items[3].Closed = "closed", daysAgo(now, 40)
	g.comment(103, 3, "old discussion", daysAgo(now, 41))
	g.addPull(4, "Ancient merge", "", daysAgo(now, 200), "closed", "ancient.go")
	g.addIssue(5, "Closed then commented", "", daysAgo(now, 1))
	g.items[5].State, g.items[5].Closed = "closed", daysAgo(now, 40)
	g.comment(104, 5, "a late comment", daysAgo(now, 1))
	g.addPull(6, "Old open pull request", "", daysAgo(now, 100), "open", "live.go")
	g.rcomment[200] = github.CommentRecord{Kind: "review_comment", ID: 200, Number: 6, Author: "dana", Body: "an old nit", Path: "live.go", Line: 3,
		Created: daysAgo(now, 90), Updated: daysAgo(now, 90)}
}

func (r *rig) has(n int) bool {
	_, err := r.st.ItemIndex(r.ws.ID).Read(n)
	return err == nil
}

func TestBackfillReadsOnlyTheWindow(t *testing.T) {
	r := newRig(t)
	r.populateOld()
	r.stepAll()

	// Every list of items it asked for is either the open ones or reaches back no further than 30 days.
	if len(r.repo.lists) == 0 {
		t.Fatal("no lists asked for")
	}
	for _, q := range r.repo.lists {
		if q.State != "open" && (q.Since.IsZero() || q.Since.Before(r.now.Add(-store.KeepInFull-time.Minute))) {
			t.Fatalf("the backfill asked for items further back than the window: %+v", q)
		}
	}
	for _, n := range []int{1, 2, 6} {
		if !r.has(n) {
			t.Errorf("#%d should be kept", n)
		}
	}
	for _, n := range []int{3, 4, 5} {
		if r.has(n) {
			t.Errorf("#%d is older than the window and should not be kept", n)
		}
	}
	if r.repo.count("itemcomments:3") != 0 || r.repo.count("pull:4") != 0 || r.repo.count("files:4") != 0 || r.repo.count("itemcomments:5") != 0 {
		t.Fatalf("it read items outside the window: %v", r.repo.calls)
	}
	// Comments older than the window, on items inside it, are read by item.
	if d := r.read(1); len(d.Discussion) != 2 || d.Discussion[0].Body != "the very first report" {
		t.Fatalf("#1's old comment: %+v", d.Discussion)
	}
	if d := r.read(2); len(d.Discussion) != 1 {
		t.Fatalf("#2's comment from before the window: %+v", d.Discussion)
	}
	if d := r.read(6); len(d.Discussion) != 1 || d.Discussion[0].Kind != "review_comment" {
		t.Fatalf("#6's old inline comment: %+v", d.Discussion)
	}
	if s := r.status(); s.Items != 3 || s.Comments != 4 || s.PrunedAt.IsZero() {
		t.Fatalf("status %+v, pruned at %v (it prunes once when the backfill ends)", s.IndexStats, s.PrunedAt)
	}

	// A comment on an old closed item, arriving later, is not kept either.
	r.repo.touch(5, r.now)
	r.repo.comment(105, 5, "another late comment", r.now)
	r.now = r.now.Add(3 * time.Minute)
	r.stepAll()
	if r.has(5) {
		t.Fatal("the sync kept an item closed 40 days ago")
	}
	if s := r.status(); s.Comments != 4 {
		t.Fatalf("a comment on an item that is not kept was stored: %d comments", s.Comments)
	}
}

// Closed items shrink after 30 days and go after 180; the job runs daily and says what it did.
func TestPruningRunsDailyAndAgesItems(t *testing.T) {
	r := newRig(t)
	r.populateOld()
	r.stepAll()
	if d := r.read(2); d.Slim || len(d.Discussion) != 1 {
		t.Fatal("#2 should be whole at first")
	}

	r.now = r.now.Add(time.Hour)
	r.stepAll()
	if s := r.status(); s.PruneNote != "0 shortened, 0 deleted" {
		t.Fatalf("pruned again within a day: %q", s.PruneNote)
	}

	r.now = r.now.Add(40 * 24 * time.Hour) // #2 closed 45 days ago
	r.stepAll()
	if d := r.read(2); !d.Slim || d.Body != "" || len(d.Discussion) != 0 || d.Title != "Recently closed" {
		t.Fatalf("#2 should be a short record after 45 days: %+v", d)
	}
	if s := r.status(); s.PruneNote != "1 shortened, 0 deleted" || s.Slim != 1 {
		t.Fatalf("status after the first pruning: %q slim=%d", s.PruneNote, s.Slim)
	}
	if hits, _ := r.st.ItemIndex(r.ws.ID).Search(store.ItemSearch{Query: "comment before window"}); len(hits) != 0 {
		t.Fatalf("a short record is still searchable: %+v", hits)
	}
	if !r.has(1) || !r.has(6) {
		t.Fatal("open items must never be pruned")
	}

	r.now = r.now.Add(200 * 24 * time.Hour) // #2 closed 245 days ago
	r.stepAll()
	if r.has(2) {
		t.Fatal("#2 should be deleted after 180 days")
	}
	if s := r.status(); s.PruneNote != "0 shortened, 1 deleted" {
		t.Fatalf("status after the second pruning: %q", s.PruneNote)
	}

	// Gone from the copy, but an agent asking for it by number still gets it, from GitHub, without it being stored.
	d, err := r.sy.Fetch(context.Background(), r.ws, "tok", 2)
	if err != nil || !d.Remote || d.Title != "Recently closed" || len(d.Discussion) != 1 || d.Discussion[0].Body != "a comment from before the window" {
		t.Fatalf("fetching a pruned item: %+v %v", d, err)
	}
	if r.has(2) {
		t.Fatal("a fetched old item was stored")
	}
}

func TestFetchStoresOnlyWhatTheWindowKeeps(t *testing.T) {
	r := newRig(t)
	r.populateOld()
	r.stepAll()

	// Old and closed: returned, not stored.
	d, err := r.sy.Fetch(context.Background(), r.ws, "tok", 4)
	if err != nil || !d.Remote || d.Kind != "pr" || !d.Merged || len(d.Files) != 1 || d.Files[0].Path != "ancient.go" || d.Base != "main" {
		t.Fatalf("fetch of an old pull request: %+v %v", d, err)
	}
	if d, err = r.sy.Fetch(context.Background(), r.ws, "tok", 3); err != nil || !d.Remote || len(d.Discussion) != 1 || d.Discussion[0].Body != "old discussion" {
		t.Fatalf("fetch of an old issue: %+v %v", d, err)
	}
	if r.has(3) || r.has(4) {
		t.Fatal("old items were stored by a fetch")
	}

	// Open (and missing from the copy): returned and stored.
	r.repo.addIssue(7, "Surfaced late", "an open issue the sync never saw", daysAgo(r.now, 60))
	r.repo.comment(106, 7, "context", daysAgo(r.now, 59))
	d, err = r.sy.Fetch(context.Background(), r.ws, "tok", 7)
	if err != nil || d.Remote || len(d.Discussion) != 1 {
		t.Fatalf("fetch of an open item: %+v %v", d, err)
	}
	if !r.has(7) {
		t.Fatal("an open item fetched by number was not kept")
	}
	if _, err := r.sy.Fetch(context.Background(), r.ws, "tok", 999); err == nil || !strings.Contains(err.Error(), "no such") {
		t.Fatalf("fetch of a number GitHub does not have: %v", err)
	}
}
