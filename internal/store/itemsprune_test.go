package store

import (
	"testing"
	"time"
)

func countRows(t *testing.T, st *Store, table string, ws int64, number int) int {
	t.Helper()
	col := "number"
	if table == "item_links" {
		col = "from_number"
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE workspace_id = ? AND `+col+` = ?`, ws, number).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An item closed more than 30 days ago keeps a short record and its files; one closed more than 180
// days ago goes with everything of it. Open items and recent ones are left alone.
func TestPruneShortensAndDeletes(t *testing.T) {
	st, a, b := twoWorkspaces(t)
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	ago := func(days int) time.Time { return now.Add(-time.Duration(days) * 24 * time.Hour) }
	add := func(ws int64, n int, state string, closedDays int) {
		it := Item{Number: n, Kind: "pr", Title: "Pull request widget", Body: "the widget body", State: state, Author: "alice", Labels: []string{"bug"},
			Created: ago(300), Updated: ago(max(closedDays, 1)), Merged: state == "closed"}
		if state == "closed" {
			it.Closed = ago(closedDays)
		}
		if err := st.SaveItem(ws, it, []ItemLink{{To: 1, How: "mentions"}}); err != nil {
			t.Fatal(err)
		}
		st.SaveFiles(ws, n, []ItemFile{{Path: "widget/w.go", Status: "modified"}})
		st.SaveComments(ws, []ItemComment{{Kind: "comment", ID: int64(n), Number: n, Author: "bob", Body: "the gadget discussion", Created: ago(closedDays), Updated: ago(closedDays),
			Links: []ItemLink{{To: 2, How: "mentions"}}}})
	}
	add(a.ID, 10, "open", 100)   // open for 100 days: untouched
	add(a.ID, 11, "closed", 10)  // recent: whole
	add(a.ID, 12, "closed", 40)  // shortened
	add(a.ID, 13, "closed", 200) // deleted
	add(b.ID, 12, "closed", 40)  // another workspace's, pruned on its own
	ix := st.ItemIndex(a.ID)

	p, err := st.Prune(a.ID, now)
	if err != nil || p.Slimmed != 1 || p.Deleted != 1 {
		t.Fatalf("pruned %+v %v", p, err)
	}
	if p, _ := st.Prune(a.ID, now); p.Slimmed+p.Deleted != 0 {
		t.Fatalf("a second pruning did more: %+v", p)
	}

	for _, n := range []int{10, 11} {
		d, err := ix.Read(n)
		if err != nil || d.Slim || d.Body == "" || len(d.Discussion) != 1 || len(d.LinksOut) == 0 {
			t.Fatalf("#%d should be whole: %+v %v", n, d, err)
		}
	}
	d, err := ix.Read(12)
	if err != nil || !d.Slim || d.Body != "" || len(d.Discussion) != 0 || len(d.LinksOut) != 0 ||
		d.Title != "Pull request widget" || d.State != "closed" || d.Author != "alice" || len(d.Labels) != 1 || d.Closed.IsZero() || len(d.Files) != 1 {
		t.Fatalf("#12 should be a short record with its labels, dates and files: %+v %v", d, err)
	}
	if _, err := ix.Read(13); err != ErrNotFound {
		t.Fatalf("#13 should be gone: %v", err)
	}
	for _, table := range []string{"items", "item_labels", "item_comments", "item_files", "item_links"} {
		if n := countRows(t, st, table, a.ID, 13); n != 0 {
			t.Errorf("%d rows of #13 are left in %s", n, table)
		}
	}
	if n := countRows(t, st, "item_comments", a.ID, 12); n != 0 {
		t.Errorf("%d comments of the short record #12 are left", n)
	}
	if n := countRows(t, st, "item_comments", b.ID, 12); n != 1 {
		t.Errorf("another workspace's comments were pruned: %d left", n)
	}

	// The short record is out of the search; the whole ones are in.
	hits, _ := ix.Search(ItemSearch{Query: "gadget"})
	got := map[int]bool{}
	for _, h := range hits {
		got[h.Number] = true
	}
	if len(hits) != 2 || !got[10] || !got[11] {
		t.Fatalf("search after pruning: %+v", hits)
	}
	if hits, _ := ix.Search(ItemSearch{Query: "widget", State: "merged"}); len(hits) != 1 || hits[0].Number != 11 {
		t.Fatalf("the title of a short record is still searchable: %+v", hits)
	}
	// But it still answers what touched a file, and what is related to it.
	touching, _ := ix.Touching("widget/", 20)
	found := map[int]bool{}
	for _, s := range touching {
		found[s.Number] = true
	}
	if found[13] || !found[12] || !found[10] {
		t.Fatalf("items touching after pruning: %+v", touching)
	}
	if r, _ := ix.Related(10); len(r.Shared) != 2 {
		t.Fatalf("related after pruning: %+v", r.Shared)
	}
	// Comments on a short record are dropped.
	st.SaveComments(a.ID, []ItemComment{{Kind: "comment", ID: 99, Number: 12, Body: "a late comment", Created: now, Updated: now}})
	if n := countRows(t, st, "item_comments", a.ID, 12); n != 0 {
		t.Fatal("a comment on a short record was kept")
	}
	// A short record that is saved whole again (reopened, say) is searchable again.
	st.SaveItem(a.ID, Item{Number: 12, Kind: "pr", Title: "Pull request widget", Body: "reopened gadget", State: "open", Created: ago(300), Updated: now}, nil)
	if hits, _ := ix.Search(ItemSearch{Query: "reopened"}); len(hits) != 1 || hits[0].Number != 12 {
		t.Fatalf("a reopened item is not searchable: %+v", hits)
	}
	for _, tbl := range []string{"item_fts", "comment_fts"} {
		if _, err := st.db.Exec(`INSERT INTO ` + tbl + `(` + tbl + `) VALUES ('integrity-check')`); err != nil {
			t.Fatalf("%s no longer matches its table: %v", tbl, err)
		}
	}
}
