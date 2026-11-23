package store

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func mustItem(t *testing.T, st *Store, ws int64, n int, kind, title, body string) {
	t.Helper()
	it := Item{Number: n, Kind: kind, Title: title, Body: body, State: "open", Author: "alice", Created: t0, Updated: t0.Add(time.Duration(n) * time.Minute)}
	if err := st.SaveItem(ws, it, nil); err != nil {
		t.Fatal(err)
	}
}

func twoWorkspaces(t *testing.T) (*Store, *Workspace, *Workspace) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	a, err := st.CreateWorkspace("o/a", admin, "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateWorkspace("o/b", admin, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return st, a, b
}

// A workspace's reader must never show another workspace's items, comments, files or links, even
// when the numbers and the words are the same.
func TestItemIndexIsScopedToItsWorkspace(t *testing.T) {
	st, a, b := twoWorkspaces(t)
	mustItem(t, st, a.ID, 1, "pr", "Retry uploads in workspace A", "touches the uploader")
	mustItem(t, st, b.ID, 1, "pr", "Retry uploads in workspace B", "touches the uploader")
	mustItem(t, st, b.ID, 2, "issue", "Only B has this uploader issue", "")
	st.SaveComments(b.ID, []ItemComment{{Kind: "comment", ID: 5, Number: 1, Author: "bob", Body: "secret uploader detail of B", Created: t0, Updated: t0,
		Links: []ItemLink{{To: 2, How: "fixes"}}}})
	st.SaveFiles(b.ID, 1, []ItemFile{{Path: "upload/send.go", Status: "modified"}})
	st.SaveFiles(a.ID, 1, []ItemFile{{Path: "other.go", Status: "modified"}})
	st.SaveFiles(b.ID, 2, []ItemFile{{Path: "other.go", Status: "modified"}}) // the same path in B, on another number
	st.SaveComments(b.ID, []ItemComment{{Kind: "comment", ID: 6, Number: 2, Author: "bob", Body: "x", Created: t0, Updated: t0, Links: []ItemLink{{To: 1, How: "closes"}}}})

	ix := st.ItemIndex(a.ID)
	hits, err := ix.Search(ItemSearch{Query: "uploader"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Number != 1 || !strings.Contains(hits[0].Title, "workspace A") {
		t.Fatalf("search in A found %+v", hits)
	}
	if hits, _ := ix.Search(ItemSearch{Query: "secret"}); len(hits) != 0 {
		t.Fatalf("A found B's comment: %+v", hits)
	}
	if _, err := ix.Read(2); err != ErrNotFound {
		t.Fatalf("A read B's item #2: %v", err)
	}
	d, err := ix.Read(1)
	if err != nil || strings.Contains(d.Title, "B") || len(d.Discussion) != 0 || len(d.Files) != 1 || d.Files[0].Path != "other.go" {
		t.Fatalf("A read #1 as %+v (%v)", d, err)
	}
	if got, _ := ix.Touching("upload", 10); len(got) != 0 {
		t.Fatalf("A saw B's files: %+v", got)
	}
	if len(d.LinksIn) != 0 || len(d.LinksOut) != 0 {
		t.Fatalf("A read B's links: %+v %+v", d.LinksIn, d.LinksOut)
	}
	if r, err := ix.Related(1); err != nil || len(r.Out)+len(r.In)+len(r.Shared) != 0 {
		t.Fatalf("A related #1: %+v %v", r, err)
	}
	// And B still sees its own.
	if hits, _ := st.ItemIndex(b.ID).Search(ItemSearch{Query: "secret"}); len(hits) != 1 {
		t.Fatalf("B lost its comment: %+v", hits)
	}
}

func TestSearchStemsFiltersAndShowsSnippets(t *testing.T) {
	st, a, _ := twoWorkspaces(t)
	mustItem(t, st, a.ID, 1, "issue", "Uploads hang", "The client keeps retrying forever when the server resets the connection.")
	mustItem(t, st, a.ID, 2, "pr", "Bound the retry loop", "Gives up after three attempts.")
	mustItem(t, st, a.ID, 3, "pr", "Unrelated", "Documentation only")
	st.SaveItem(a.ID, Item{Number: 4, Kind: "pr", Title: "Merged retry fix", State: "closed", Merged: true, Author: "carol", Labels: []string{"bug"}, Created: t0, Updated: t0}, nil)
	st.SaveComments(a.ID, []ItemComment{{Kind: "review_comment", ID: 9, Number: 3, Author: "dana", Body: "Please wrap this in a retried call", Path: "a.go", Line: 12, Created: t0, Updated: t0}})
	ix := st.ItemIndex(a.ID)

	hits, err := ix.Search(ItemSearch{Query: "retries"}) // stemmed: finds retrying, retry and retried
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]ItemHit{}
	for _, h := range hits {
		got[h.Number] = h
	}
	if len(got) != 4 || got[1].Snippet == "" || !strings.Contains(got[1].Snippet, "[retrying]") {
		t.Fatalf("stemmed search: %+v", hits)
	}
	if h := got[3]; !strings.Contains(h.Where, "inline review comment by dana") {
		t.Fatalf("a hit in a comment should say where: %+v", h)
	}
	if hits, _ := ix.Search(ItemSearch{Query: "retry", Kind: "issue"}); len(hits) != 1 || hits[0].Number != 1 {
		t.Fatalf("kind filter: %+v", hits)
	}
	if hits, _ := ix.Search(ItemSearch{Query: "retry", State: "merged"}); len(hits) != 1 || hits[0].Number != 4 || hits[0].State != "merged" {
		t.Fatalf("state filter: %+v", hits)
	}
	if hits, _ := ix.Search(ItemSearch{Query: "retry", Label: "BUG"}); len(hits) != 1 || hits[0].Number != 4 {
		t.Fatalf("label filter: %+v", hits)
	}
	if hits, _ := ix.Search(ItemSearch{Query: "retry", Author: "CAROL"}); len(hits) != 1 || hits[0].Number != 4 {
		t.Fatalf("author filter: %+v", hits)
	}
	// What a model types must not be able to break the query.
	for _, q := range []string{`"unbalanced`, `retry AND`, `a:b -c *`, `NEAR(`, `retry"; DROP TABLE items; --`} {
		if _, err := ix.Search(ItemSearch{Query: q}); err != nil {
			t.Errorf("query %q: %v", q, err)
		}
	}
	if _, err := ix.Search(ItemSearch{Query: " ?! "}); !isInput(err) {
		t.Fatalf("a query without words should be an input error, got %v", err)
	}
}

// Every write to an item or comment must reach the index: a word taken out of a body stops finding
// it, and the index agrees with its tables after deletes too.
func TestIndexStaysInStepWithWrites(t *testing.T) {
	st, a, _ := twoWorkspaces(t)
	mustItem(t, st, a.ID, 1, "issue", "Crash", "mentions zebra")
	st.SaveComments(a.ID, []ItemComment{{Kind: "comment", ID: 1, Number: 1, Author: "x", Body: "also giraffe", Created: t0, Updated: t0}})
	ix := st.ItemIndex(a.ID)
	find := func(q string) int { h, _ := ix.Search(ItemSearch{Query: q}); return len(h) }
	if find("zebra") != 1 || find("giraffe") != 1 {
		t.Fatal("new rows are not searchable")
	}
	mustItem(t, st, a.ID, 1, "issue", "Crash", "mentions llama")
	st.SaveComments(a.ID, []ItemComment{{Kind: "comment", ID: 1, Number: 1, Author: "x", Body: "also okapi", Created: t0, Updated: t0}})
	if find("zebra") != 0 || find("llama") != 1 || find("giraffe") != 0 || find("okapi") != 1 {
		t.Fatalf("edits not reflected: zebra=%d llama=%d giraffe=%d okapi=%d", find("zebra"), find("llama"), find("giraffe"), find("okapi"))
	}
	if err := st.PruneComments(a.ID, 1, "comment", nil); err != nil {
		t.Fatal(err)
	}
	if find("okapi") != 0 {
		t.Fatal("a pruned comment is still searchable")
	}
	// Deleting a workspace takes its rows with it; the index must still agree with its tables.
	admin, _ := st.UserByUsername("admin")
	c, _ := st.CreateWorkspace("o/c", admin, "", "")
	mustItem(t, st, c.ID, 1, "issue", "Gone soon", "wombat")
	if err := st.DeleteWorkspace(c.ID); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"item_fts", "comment_fts"} {
		if _, err := st.db.Exec(`INSERT INTO ` + tbl + `(` + tbl + `) VALUES ('integrity-check')`); err != nil {
			t.Fatalf("%s no longer matches its table: %v", tbl, err)
		}
	}
}

func TestLinksAndSharedFiles(t *testing.T) {
	st, a, _ := twoWorkspaces(t)
	mustItem(t, st, a.ID, 10, "pr", "Fix it", "")
	mustItem(t, st, a.ID, 11, "pr", "Other change", "")
	mustItem(t, st, a.ID, 12, "pr", "Closed unmerged", "")
	mustItem(t, st, a.ID, 7, "issue", "The bug", "")
	st.SaveItem(a.ID, Item{Number: 10, Kind: "pr", Title: "Fix it", State: "open", Created: t0, Updated: t0}, []ItemLink{{To: 7, How: "fixes"}})
	st.SaveComments(a.ID, []ItemComment{{Kind: "comment", ID: 1, Number: 10, Author: "x", Body: "see", Created: t0, Updated: t0, Links: []ItemLink{{To: 7, How: "mentions"}, {To: 99, How: "mentions"}}}})
	st.SaveFiles(a.ID, 10, []ItemFile{{Path: "internal/web/server.go"}, {Path: "internal/web/poller.go"}, {Path: "README.md"}})
	st.SaveFiles(a.ID, 11, []ItemFile{{Path: "internal/web/server.go"}, {Path: "internal/webx/other.go"}})
	st.SaveFiles(a.ID, 12, []ItemFile{{Path: "internal/web/server.go"}})
	st.SaveItem(a.ID, Item{Number: 12, Kind: "pr", Title: "Closed unmerged", State: "closed", Created: t0, Updated: t0}, nil)
	ix := st.ItemIndex(a.ID)

	r, err := ix.Related(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Out) != 2 || r.Out[0].Number != 7 || r.Out[0].How != "fixes" || r.Out[0].Title != "The bug" || r.Out[1].Number != 99 || r.Out[1].Title != "" {
		t.Fatalf("links out (the strongest way wins, unknown numbers kept): %+v", r.Out)
	}
	if len(r.Shared) != 2 || r.Shared[0].Count != 1 || r.Shared[0].Files[0] != "internal/web/server.go" {
		t.Fatalf("shared files: %+v", r.Shared)
	}
	if in, _ := ix.Related(7); len(in.In) != 1 || in.In[0].Number != 10 {
		t.Fatalf("links in: %+v", in.In)
	}
	// A folder matches what is below it, not a sibling that shares the prefix; closed unmerged pull requests are left out.
	got, _ := ix.Touching("internal/web/", 10)
	if len(got) != 2 || got[0].Number != 11 && got[0].Number != 10 {
		t.Fatalf("touching folder: %+v", got)
	}
	for _, g := range got {
		if g.Number == 12 {
			t.Fatal("a closed, unmerged pull request counted as touching")
		}
	}
	if got, _ := ix.Touching("internal/we", 10); len(got) != 0 {
		t.Fatalf("a partial folder name matched: %+v", got)
	}
	if got, _ := ix.Touching("internal/web/server.go", 10); len(got) != 2 {
		t.Fatalf("touching a file: %+v", got)
	}
}
