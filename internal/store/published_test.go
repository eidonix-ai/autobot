package store

import (
	"testing"
	"time"
)

// Published pages go on the retention schedule of conversations: 30 days after their item closed (or,
// not knowing it, after they were made); a page of an open item stays; a page goes with its case.
func TestRetentionDeletesPages(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	at := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	ago := func(days int) time.Time { return at.Add(-time.Duration(days) * 24 * time.Hour) }
	for _, it := range []Item{
		{Number: 1, Kind: "pr", Title: "open", State: "open", Created: ago(90), Updated: ago(1)},
		{Number: 2, Kind: "pr", Title: "closed long ago", State: "closed", Created: ago(90), Updated: ago(31), Closed: ago(31)},
		{Number: 3, Kind: "pr", Title: "closed lately", State: "closed", Created: ago(90), Updated: ago(29), Closed: ago(29)},
	} {
		if err := st.SaveItem(ws.ID, it, nil); err != nil {
			t.Fatal(err)
		}
	}
	page := func(item, madeDaysAgo int) string {
		c, _, err := st.OpenCase(ws, item, "pr", "x", "opened")
		if err != nil {
			t.Fatal(err)
		}
		id, err := st.Publish(c, "report", "text")
		if err != nil {
			t.Fatal(err)
		}
		st.db.Exec(`UPDATE published SET created_at = ? WHERE uuid = ?`, ago(madeDaysAgo).Unix(), id)
		return id
	}
	open, closedLong, closedLately, unknownOld, unknownNew := page(1, 100), page(2, 40), page(3, 40), page(4, 31), page(5, 29)
	p, err := st.PruneRuns(at, func(int64) bool { return true })
	if err != nil || p.Pages != 2 {
		t.Fatalf("pruned %+v %v", p, err)
	}
	for id, want := range map[string]bool{open: true, closedLong: false, closedLately: true, unknownOld: false, unknownNew: true} {
		if _, err := st.PublishedPage(id); (err == nil) != want {
			t.Errorf("page %s kept %v, want %v", id, err == nil, want)
		}
	}
	// Where the local copy of items is not complete, a page of an item it does not know is kept.
	late := page(6, 50)
	if p, _ := st.PruneRuns(at, func(int64) bool { return false }); p.Pages != 0 {
		t.Errorf("pruned %d pages while the copy was incomplete", p.Pages)
	}
	if _, err := st.PublishedPage(late); err != nil {
		t.Errorf("a page of an unknown item was deleted: %v", err)
	}
	// The same content of the same tag of a case is one page; another tag or content is another.
	c, _, _ := st.OpenCase(ws, 1, "pr", "x", "opened")
	a, _ := st.Publish(c, "report", "same")
	b, _ := st.Publish(c, "report", "same")
	d, _ := st.Publish(c, "report", "other")
	e, _ := st.Publish(c, "notes", "same")
	if a != b || a == d || a == e || d == e {
		t.Errorf("pages %s %s %s %s", a, b, d, e)
	}
}
